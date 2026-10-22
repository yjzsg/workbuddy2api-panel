// 余额「未知 ≠ 为 0」：fresh entry（state.json 丢失 / 进程刚起）不能因为 credits 的
// 零值被积分保底当成耗尽号。2026-10-22 旁挂 A/B 实测事故：新实例 fresh state 下
// `global:minimax-m3.1-flash` 全 503、`credit floor` WARN 刷屏，直到第一次余额刷新。
package pool

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// fresh entry（从未观测余额）+ 收费模型：floor 放行，不能静默失联。
func TestCreditFloorUnknownCreditsAllowsPaid(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "fresh"})

	// 不调 SetCredits：credits 零值但「未知」。模型有实测收费记录（tier 2）。
	p.NoteModelCost("fresh", "hy4-preview", 2.9, 1000)

	if a := p.PickExcludingForRealm(nil, "hy4-preview", ""); a == nil {
		t.Fatal("余额未知不应被积分保底拦截（否则 state 丢失后整池 503）")
	}
}

// 已知余额为 0（上游真读回 0）：照旧拦截 —— 修复不能把真耗尽号也放进来。
func TestCreditFloorKnownZeroStillBlocks(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "broke"})

	p.SetCredits("broke", 0, 0) // 上游真实读数 = 0，creditsKnown 置真
	p.NoteModelCost("broke", "hy4-preview", 2.9, 1000)

	if a := p.PickExcludingForRealm(nil, "hy4-preview", ""); a != nil {
		t.Fatalf("已知 credits=0 的号不应承接收费模型，got %s", a.UID)
	}
}

// 余额已知但低于 floor（30 < 100）：行为与引入前一致（拦截）。
func TestCreditFloorKnownBelowFloorBlocks(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "low"})

	p.SetCredits("low", 30, 0)
	p.NoteModelCost("low", "hy4-preview", 2.9, 1000)

	if a := p.PickExcludingForRealm(nil, "hy4-preview", ""); a != nil {
		t.Fatalf("credits=30 < floor=100 且收费模型，应拦截，got %s", a.UID)
	}
}

// state.json 恢复：旧文件无 credits_known 字段，但 Credits>0 视为已知（非零即证据）；
// Credits=0 且无标记 = 未知，不拦。
func TestCreditFloorStateRestoreKnownSemantics(t *testing.T) {
	withNoPickGap(t)
	for _, tc := range []struct {
		name      string
		credits   int64
		known     bool
		wantBlock bool
	}{
		{"旧文件非零余额视为已知", 500, false, false}, // 500 > floor，本就不拦
		{"旧文件零余额视为未知", 0, false, false},   // 未知 → 放行
		{"新文件零余额显式已知", 0, true, true},    // 真为 0 → 拦
	} {
		p := New("")
		p.SetCostExploreInterval(0)
		p.SetCreditFloor(100)
		p.SetRandomSource(func(n int64) int64 { return 0 })
		p.Add(&auth.Auth{UID: "u"})
		p.mu.Lock()
		if e, ok := p.byUID["u"]; ok {
			e.credits = tc.credits
			e.creditsKnown = tc.known || tc.credits > 0
		}
		p.mu.Unlock()
		p.NoteModelCost("u", "hy4-preview", 2.9, 1000)

		got := p.PickExcludingForRealm(nil, "hy4-preview", "") != nil
		if got == tc.wantBlock {
			t.Errorf("%s: 选中=%v want 拦截=%v", tc.name, got, tc.wantBlock)
		}
	}
}
