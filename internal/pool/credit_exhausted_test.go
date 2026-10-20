// credit_exhausted_test.go cooldown.credit_exhausted（积分耗尽短冷却，issue #78）单测。
// [移植自根上游 c5fc618 / 上游 ef292ddd]
package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// cooled 取账号当前的冷却类型与到期时刻（白盒读，同 revivesoft_test 口径）。
func cooled(t *testing.T, p *Pool, uid string) (CoolKind, time.Time) {
	t.Helper()
	p.mu.RLock()
	defer p.mu.RUnlock()
	e := p.byUID[uid]
	if e == nil {
		t.Fatalf("账号 %s 不在池中", uid)
	}
	return e.coolKind, e.until
}

// TestCreditExhaustedDefaultHard 未配置时保持旧行为：硬冷却到次日 04:00
// （等签到回血），行为与引入前逐字一致。
func TestCreditExhaustedDefaultHard(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// Cooldown 内部再取一次 time.Now() 算 until，故到期时刻带亚秒漂移；
	// 只比到秒，并同时给出调用前后两个下界，跨 04:00 边界也不会误报。
	before := time.Now()
	p.CooldownUntilTomorrow4AM("u1", "credit exhausted")
	after := time.Now()
	kind, until := cooled(t, p, "u1")
	if kind != CoolHard {
		t.Errorf("kind=%v want CoolHard（未配置 credit_exhausted 时行为不变）", kind)
	}
	got := until.Truncate(time.Second)
	wantBefore := nextDay4AM(before).Truncate(time.Second)
	wantAfter := nextDay4AM(after).Truncate(time.Second)
	if !got.Equal(wantBefore) && !got.Equal(wantAfter) {
		t.Errorf("until=%v want %v（±1s 容差）", got, wantBefore)
	}
}

// TestCreditExhaustedShortCooldown 配了时长后只退出这一段：软冷却、到期回池，
// 不占用一整天（用户可能充值/上游可能刷新额度）。
func TestCreditExhaustedShortCooldown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditExhausted(10 * time.Minute)
	p.CooldownUntilTomorrow4AM("u1", "credit exhausted")
	kind, until := cooled(t, p, "u1")
	if kind != CoolSoft {
		t.Fatalf("kind=%v want CoolSoft（短冷却走软冷却，不写硬冷却）", kind)
	}
	rem := time.Until(until)
	if rem < 9*time.Minute || rem > 10*time.Minute+30*time.Second {
		t.Errorf("剩余 %v，want ≈10m（不是到次日 04:00）", rem)
	}
}

// TestSetCreditExhaustedZeroRestoresHard 0 = 关停（回到旧行为）：即使先配过正值，
// 再用 0 注入也必须完全恢复到「硬冷却到次日 04:00」。
func TestSetCreditExhaustedZeroRestoresHard(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditExhausted(10 * time.Minute)
	p.SetCreditExhausted(0)
	p.CooldownUntilTomorrow4AM("u1", "credit exhausted")
	if kind, _ := cooled(t, p, "u1"); kind != CoolHard {
		t.Errorf("kind=%v want CoolHard（0 = 关停）", kind)
	}
	// 负值同样按关停处理，不产生「立即到期」的空冷却。
	p.SetCreditExhausted(-time.Minute)
	p.CooldownUntilTomorrow4AM("u1", "credit exhausted")
	if kind, _ := cooled(t, p, "u1"); kind != CoolHard {
		t.Errorf("负值注入后 kind=%v want CoolHard", kind)
	}
}
