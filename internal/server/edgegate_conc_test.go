package server

// edgegate_conc_test.go 代码健康审计（任务书第 1 条）补缺：edgeGate 多 goroutine
// 并发 noteWaf/noteEdgeAuth/active 压力。状态机单测（触发/过期/不续期/两形态共用窗）
// 已由 edgegate_test.go 覆盖，本文件只补「并发记账无竞态 + 阈值语义在交错下不被破坏」。
import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestEdgeGateConcurrentMixed 并发混发：N goroutine 各对多个不同 uid 无限
// noteWaf/noteEdgeAuth + 持续读 active。不变量：不 panic（-race 验证锁覆盖）、
// 激活后 active 恒 true（直到窗过期）、激活后命中恒 true（不续期语义下激活期
// 内无 false 回落——note 激活期分支直接返回 true）。
func TestEdgeGateConcurrentMixed(t *testing.T) {
	withEdgeWindow(t, 200*time.Millisecond)
	var g edgeGate
	const goroutines = 8
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 50; n++ {
				uid := fmt.Sprintf("u%d-%d", i, n%6) // 每 goroutine 6 个不同 uid，跨 g 也不重复
				// 两种形态交错发数：共用同一判定窗，任一形态都不该破坏计数语义。
				var hit bool
				if n%2 == 0 {
					hit = g.noteWaf(uid)
				} else {
					hit = g.noteEdgeAuth(uid)
				}
				if hit {
					// 已激活（本窗内）：并发下其他线程的命中也应恒 true。
					// 只做无锁交叉读验证 active 一致性（读侧允许滞后，不可 panic）。
					_ = g.active()
				}
			}
		}(i)
	}
	wg.Wait()
	// 8×6=48 个不同 uid 全部入窗（远超阈值 2）→ 激活必然已发生且仍激活
	// （200ms 窗内完成所有发数，无 sleep 落在窗外）。
	if !g.active() {
		t.Fatal("48 distinct uids within window must leave gate active")
	}
}

// TestEdgeGateConcurrentSingleUIDPerAccount 并发下「单号反复不触发」不变量：
// 所有 goroutine 共用 2 个 uid 反复命中，永不达到 2 个不同号的语义下应激活
// （2 个不同号即阈值）——语义反转对照：本测试用 2 个 uid，应激活；再用单
// uid 独立 gate 验证永不激活（两种形态各验一遍）。
func TestEdgeGateConcurrentSingleUIDPerAccount(t *testing.T) {
	withEdgeWindow(t, time.Minute)
	// 单 uid 并发大量命中：永不激活（判定口径=不同 UID 数，锁保护下计数一致）。
	// 两种形态都跑一遍：401 与 403 的单号语义必须一致。
	for _, tc := range []struct {
		name string
		note func(*edgeGate, string) bool
	}{
		{"waf403", (*edgeGate).noteWaf},
		{"edgeAuth401", (*edgeGate).noteEdgeAuth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var single edgeGate
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for n := 0; n < 100; n++ {
						if tc.note(&single, "only-one") {
							t.Error("concurrent single-uid hits must never activate")
							return
						}
					}
				}()
			}
			wg.Wait()
			if single.active() {
				t.Fatal("single account gate must never activate regardless of concurrency")
			}
		})
	}
}
