package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestExpiringCreditBoostsWeight 快过期积分占比高的号权重大于占比低/无的号（同总量下）。
func TestExpiringCreditBoostsWeight(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "expiring-heavy"}) // 总量相同,快过期占比高
	p.Add(&auth.Auth{UID: "stable-heavy"})   // 总量相同,快过期占比低
	// 上游 dbd7c68..origin/main：快过期偏好从「第四因子 ×8 加成」改成
	// 「快过期虚拟实例数 ×expiringVirtualSlots」，由 pool.prefer_expiring 控制。
	p.SetPreferExpiring(true)
	soon := time.Now().Add(48 * time.Hour)
	p.SetCreditsDetailed("expiring-heavy", 1000, 1000, 900, soon, 900)   // 有有效快过期批次
	p.SetCreditsDetailed("stable-heavy", 1000, 1000, 50, time.Time{}, 0) // 无有效批次

	p.mu.RLock()
	eExp := p.byUID["expiring-heavy"]
	eSta := p.byUID["stable-heavy"]
	maxC := int64(1000)
	now := time.Now()
	wExp := p.routingWeightOf(eExp, maxC, now)
	wSta := p.routingWeightOf(eSta, maxC, now)
	p.mu.RUnlock()

	if wExp <= wSta {
		t.Errorf("expiring-heavy weight %.3f <= stable-heavy %.3f; 快过期积分应加权", wExp, wSta)
	}
	// 快过期号权重 = 普通权重 × expiringVirtualSlots（虚拟实例数）。
	if ratio := wExp / wSta; ratio < float64(expiringVirtualSlots)-0.01 || ratio > float64(expiringVirtualSlots)+0.01 {
		t.Errorf("routing weight ratio %.3f want %d (expiringVirtualSlots)", ratio, expiringVirtualSlots)
	}
}

// TestExpiringClampedToCredits expiring 超过总量/负值时被钳制,不污染权重。
func TestExpiringClampedToCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 100, 100, 9999, time.Time{}, 0) // expiring > credits
	p.mu.RLock()
	if p.byUID["u1"].creditsExpiring != 100 {
		t.Errorf("creditsExpiring=%d want 100 (clamped to credits)", p.byUID["u1"].creditsExpiring)
	}
	p.mu.RUnlock()

	p.SetCreditsDetailed("u1", 100, 100, -5, time.Time{}, 0) // 负值
	p.mu.RLock()
	if p.byUID["u1"].creditsExpiring != 0 {
		t.Errorf("creditsExpiring=%d want 0 (negative clamped)", p.byUID["u1"].creditsExpiring)
	}
	p.mu.RUnlock()
}

// TestSetCreditsLeavesExpiringUnchanged 旧 SetCredits 只更新总量,不清 expiring(向后兼容)。
func TestSetCreditsLeavesExpiringUnchanged(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 500, 500, 200, time.Time{}, 0)
	p.SetCredits("u1", 600, 0) // 旧入口只更新总量
	p.mu.RLock()
	e := p.byUID["u1"]
	if e.credits != 600 {
		t.Errorf("credits=%d want 600", e.credits)
	}
	if e.creditsExpiring != 200 {
		t.Errorf("creditsExpiring=%d want 200 (SetCredits 不应清)", e.creditsExpiring)
	}
	p.mu.RUnlock()
}
