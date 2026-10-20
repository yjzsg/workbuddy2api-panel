// realmweight_test.go 按 (模型, 域) 的动态权重：衰减 / 恢复 / 选号优先级。
//
// 生产依据（2026-10-19，4 小时窗口）：
//
//	cn(www.codebuddy.cn)     45 次请求  0 次 429   0.0%
//	global(www.workbuddy.ai) 86 次请求 21 次 429  19.6%
//
// 而裸名请求按账号数（35:6）灌向 global —— 权重机制把这份差异反馈进选号。
package pool

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func newTwoRealmPool(t *testing.T) *Pool {
	t.Helper()
	p := New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "c1", Domain: "www.codebuddy.cn"})
	return p
}

// TestRealmWeightDecayAndIsolation 一次限流砍半；只影响该域，另一域保持满血。
func TestRealmWeightDecayAndIsolation(t *testing.T) {
	p := newTwoRealmPool(t)

	if w := p.RealmWeight("m", "global"); w != 1 {
		t.Fatalf("初始权重应为 1，got %v", w)
	}
	p.NoteRealmRateLimited("g1", "m")
	// 用容差而非精确相等：权重是「读时按 elapsed 恢复」的，写入与读取之间哪怕
	// 过了几微秒也会得到 0.50000000571 这种值。CI（Linux、时钟精度更高）上
	// 曾因此稳定失败（go-binaries #29–#36 全部红）。
	if w := p.RealmWeight("m", "global"); math.Abs(w-0.5) > 1e-6 {
		t.Fatalf("一次限流应降到 0.5，got %v", w)
	}
	if w := p.RealmWeight("m", "cn"); w != 1 {
		t.Fatalf("cn 不该被 global 的限流牵连，got %v", w)
	}
	// 另一个模型互不影响。
	if w := p.RealmWeight("other", "global"); w != 1 {
		t.Fatalf("其他模型不该受影响，got %v", w)
	}
}

// TestRealmWeightFloor 连续限流不会把权重打到 0（否则该域永远探不回来）。
func TestRealmWeightFloor(t *testing.T) {
	p := newTwoRealmPool(t)
	for i := 0; i < 20; i++ {
		p.NoteRealmRateLimited("g1", "m")
	}
	if w := p.RealmWeight("m", "global"); w < defaultRealmWeightFloor || w > defaultRealmWeightFloor+0.001 {
		t.Fatalf("应停在下限 %v，got %v", defaultRealmWeightFloor, w)
	}
}

// TestRealmWeightRecovers 时间恢复：读时按 elapsed 补，不需要定时器。
func TestRealmWeightRecovers(t *testing.T) {
	p := newTwoRealmPool(t)
	p.NoteRealmRateLimited("g1", "m") // 1.0 → 0.5

	// 假装这条记录已经 2 分钟没动过：0.5 + 2*0.2 = 0.9。
	p.mu.Lock()
	st := p.realmWeights[realmWeightKey{model: "m", realm: "global"}]
	st.lastAt = time.Now().Add(-2 * time.Minute)
	p.mu.Unlock()

	if w := p.RealmWeight("m", "global"); w < 0.85 || w > 0.95 {
		t.Fatalf("2 分钟后应恢复到约 0.9，got %v", w)
	}
}

// TestPickPrefersHigherRealmWeight 端到端：global 被打到 floor 后，裸名请求
// 几乎全部落到 cn；此前是按账号数 35:6 灌向 global。
func TestPickPrefersHigherRealmWeight(t *testing.T) {
	old := minPickGap
	minPickGap = 0
	t.Cleanup(func() { minPickGap = old })

	p := newTwoRealmPool(t)

	count := func(n int) map[string]int {
		got := map[string]int{}
		for i := 0; i < n; i++ {
			if a := p.Pick("m"); a != nil {
				got[a.Realm()]++
			}
		}
		return got
	}

	// 基线：两域权重都是 1，应该都能被选中（账号权重相近，不允许一边倒）。
	base := count(60)
	if base["global"] == 0 || base["cn"] == 0 {
		t.Fatalf("基线应两域都出现，got %v", base)
	}

	// global 连吃 10 次限流 → 健康因子到 floor(0.05)。
	// 注意：1v1 场景下钳制把 cn 压到 0.05×maxRatio(5)=0.25，所以 cn 是「明显更多」
	// 而不是「绝对垄断」——这正是设计意图：小域不能无限吃流量。
	for i := 0; i < 10; i++ {
		p.NoteRealmRateLimited("g1", "m")
	}
	after := count(100)
	if after["cn"] < 70 {
		t.Fatalf("cn 应明显占优（want cn>=70），got %v", after)
	}
	if after["global"] == 0 {
		t.Fatalf("global 仍应保留少量流量（否则域权重变成硬隔离），got %v", after)
	}
}

// TestRealmWeightMaxRatioBalancesCapacity 账号数不同时的容量钳制：
// cn 6 个号 / global 35 个号，global 被限流打到 floor 后，cn 也不能吃满——
// 它的每账号负载最多是 global 的 maxRatio 倍。
//
// 期望配比：6×0.25 : 35×0.05 = 1.5 : 1.75 → cn 约 46%。
func TestRealmWeightMaxRatioBalancesCapacity(t *testing.T) {
	old := minPickGap
	minPickGap = 0
	t.Cleanup(func() { minPickGap = old })

	p := New("")
	for i := 0; i < 6; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("c%d", i), Domain: "www.codebuddy.cn"})
	}
	for i := 0; i < 35; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("g%d", i), Domain: "www.workbuddy.ai"})
	}
	// 把 global 打到 floor。
	for i := 0; i < 10; i++ {
		p.NoteRealmRateLimited("g0", "m")
	}

	got := map[string]int{}
	for i := 0; i < 200; i++ {
		if a := p.Pick("m"); a != nil {
			got[a.Realm()]++
		}
	}
	cnPct := float64(got["cn"]) / float64(got["cn"]+got["global"]) * 100
	t.Logf("cn=%d global=%d → cn 占 %.1f%%", got["cn"], got["global"], cnPct)
	if cnPct < 25 || cnPct > 65 {
		t.Fatalf("钳制后 cn 占比应在 25~65%%（容量约束），got %.1f%%", cnPct)
	}
}

// TestRealmWeightSkippedForExplicitRealm 显式域请求没有选择权，不该被权重影响。
func TestRealmWeightSkippedForExplicitRealm(t *testing.T) {
	old := minPickGap
	minPickGap = 0
	t.Cleanup(func() { minPickGap = old })

	p := newTwoRealmPool(t)
	for i := 0; i < 10; i++ {
		p.NoteRealmRateLimited("g1", "m") // global 权重打到 floor
	}

	// 显式 global 请求仍然只能拿到 global 号（权重再低也只有一个域可选）。
	for i := 0; i < 20; i++ {
		a := p.PickExcludingForRealm(nil, "m", "global")
		if a == nil {
			t.Fatalf("显式 global 请求不该返回 nil（权重只影响跨域优先级）")
		}
		if got := a.Realm(); got != "global" {
			t.Fatalf("显式 global 请求拿到了 %s 号", got)
		}
	}
}
