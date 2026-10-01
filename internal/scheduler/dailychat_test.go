package scheduler

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestDailyChatModelByRealm 免费档模型映射：global → deepseek-v4.1-flash、
// cn / 未知 → hy3。两者都是 credits x0.00 档——本任务不该产生积分消耗。
//
// 构造 realm 走导出的 Domain（realm 字段未导出，包外无法直接写）：
// isGlobalDomain(.workbuddy.ai 后缀) → global，其余回落 cn。
func TestDailyChatModelByRealm(t *testing.T) {
	cases := []struct {
		name string
		a    *auth.Auth
		want string
	}{
		{"global 域", &auth.Auth{Domain: "www.workbuddy.ai"}, "deepseek-v4.1-flash"},
		{"cn 域", &auth.Auth{Domain: "workbuddy.cn"}, "hy3"},
		{"空 domain（回落 cn）", &auth.Auth{}, "hy3"},
		{"nil auth（保守取 cn 档）", nil, "hy3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := dailyChatModel(c.a); got != c.want {
				t.Errorf("dailyChatModel=%q want %q", got, c.want)
			}
		})
	}
}

// TestNextWakeIncludesDailyChat 每日对话保底进排程：默认 [8] 点；显式禁用则无排程。
func TestNextWakeIncludesDailyChat(t *testing.T) {
	// 只留每日对话一类任务（其余全禁用），验证默认时点。
	s := New(Config{
		CheckinDisabled:   true,
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		CatDisabled:       true,
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 20, 7, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 20, 8, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（默认 daily_chat_hours=[8]）", at, want)
	}
	if !hasKind(kinds, taskDailyChat) {
		t.Errorf("kinds=%v want 含 taskDailyChat", kinds)
	}

	// 显式禁用 → 无排程（含每日对话在内全部禁用）。
	s2 := New(Config{
		CheckinDisabled:   true,
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		CatDisabled:       true,
		DailyChatDisabled: true, GrowthDisabled: true,
	})
	if at, kinds := s2.nextWake(time.Now()); !at.IsZero() || len(kinds) != 0 {
		t.Errorf("at=%v kinds=%v want zero/nil（含每日对话全部禁用）", at, kinds)
	}
}

// TestReconfigureDailyChat 热配置（面板保存后调用）：改时点/开关后 nextWake 立即跟随。
func TestReconfigureDailyChat(t *testing.T) {
	s := New(Config{
		CheckinDisabled:   true,
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		CatDisabled:       true,
	})
	// 时点改 3 点，其余任务保持禁用、每日对话保持启用。
	s.Reconfigure(nil, nil, nil, nil, nil, nil, []int{3},
		true, true, true, true, true, true, false)

	at, kinds := s.nextWake(time.Date(2026, 9, 20, 2, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 20, 3, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（热配置改 3 点）", at, want)
	}
	if !hasKind(kinds, taskDailyChat) {
		t.Errorf("kinds=%v want 含 taskDailyChat", kinds)
	}
}
