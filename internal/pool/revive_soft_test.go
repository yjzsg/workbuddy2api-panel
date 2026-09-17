package pool

import (
	"testing"
	"time"
)

// TestReviveCoolingLockedOnlyClearsHard 锁定解冻语义：余额恢复只解「余额型冷却」
// （CoolHard），限流软冷却（CoolSoft，429/6004 配额窗口）不被清。
//
// 为什么必须锁死：reviveCoolingLocked 的两个调用方（签到 CheckinAll、余额后台刷新
// RunBalanceRefreshNow）都以「余额恢复」为依据，而后者**每 5 分钟**跑一次。原实现
// 无条件清整个冷却域 → 撞 6004 的号 5 分钟内就被解冻 → 立刻又被选中再撞，形成
// 「冷却 → 刷新解冻 → 再撞」死循环（实测 账号C 反复 6004）。
func TestReviveCoolingLockedOnlyClearsHard(t *testing.T) {
	p := &Pool{}
	softUntil := time.Now().Add(10 * time.Minute)
	e := &entry{
		coolKind:   CoolSoft,
		until:      softUntil,
		reason:     "429 rate limit",
		softStreak: 3,
	}
	p.reviveCoolingLocked(e, 480)
	if e.credits != 480 {
		t.Errorf("credits 应更新为 480，得到 %d", e.credits)
	}
	if e.coolKind != CoolSoft || !e.until.Equal(softUntil) || e.softStreak != 3 {
		t.Errorf("限流软冷却不该被余额解冻清掉: kind=%v until=%v streak=%d（want CoolSoft/%v/3）",
			e.coolKind, e.until, e.softStreak, softUntil)
	}

	hardUntil := time.Now().Add(6 * time.Hour)
	e2 := &entry{coolKind: CoolHard, until: hardUntil, reason: "credits exhausted", softStreak: 1}
	p.reviveCoolingLocked(e2, 500)
	if e2.coolKind != 0 || !e2.until.IsZero() || e2.reason != "" || e2.softStreak != 0 {
		t.Errorf("余额型冷却（CoolHard）应被清掉: kind=%v until=%v reason=%q streak=%d",
			e2.coolKind, e2.until, e2.reason, e2.softStreak)
	}
	if e2.credits != 500 {
		t.Errorf("credits 应更新为 500，得到 %d", e2.credits)
	}
}

// TestReviveCoolingLockedKeepsModelCooldowns 模型级 6004 冷却（modelCooldowns）同属
// 限流域，不该被余额刷新清 —— 否则模型豁免消失，该模型立刻又被选中再撞。
func TestReviveCoolingLockedKeepsModelCooldowns(t *testing.T) {
	p := &Pool{}
	until := time.Now().Add(5 * time.Minute)
	e := &entry{
		coolKind:       CoolSoft,
		until:          until,
		modelCooldowns: map[string]modelCooldown{"deepseek-v4.1-flash": {Until: until, Reason: "6004 model rate limit"}},
	}
	p.reviveCoolingLocked(e, 100)
	if len(e.modelCooldowns) == 0 {
		t.Error("模型级限流冷却不该被余额解冻清掉")
	}
	if e.coolKind != CoolSoft {
		t.Errorf("限流软冷却不该被清: kind=%v", e.coolKind)
	}
}
