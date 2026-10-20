package scheduler

import (
	"testing"
	"time"
)

// TestNextWakeAppliesJitter 走生产路径（nextWake）确认抖动真正生效：
// 生产配置 jitter_minutes=20 + jitter_salt，08:00 的 daily_chat 必须落在 08:00–08:20 内，
// 且与 jitterOffset 单独算出的偏移逐秒一致；关掉 jitter 时仍是整点。
func TestNextWakeAppliesJitter(t *testing.T) {
	loc := time.Local
	base := time.Date(2026, 10, 21, 8, 0, 0, 0, loc)
	now := time.Date(2026, 10, 21, 3, 0, 0, 0, loc)

	s := New(Config{
		CheckinDisabled:    true,
		TravelDisabled:     true,
		ActivityDisabled:   true,
		KeepaliveDisabled:  true,
		GrowthDisabled:     true,
		CatDisabled:        true,
		CNInviteCode:       "",
		DailyChatHours:     []int{8},
		JitterMinutes:      20,
		JitterSalt:         "wb2a-yeying-2026",
	})
	at, kinds := s.nextWake(now)
	if len(kinds) != 1 || kinds[0] != taskDailyChat {
		t.Fatalf("kinds=%v want [daily_chat]", kinds)
	}
	want := base.Add(jitterOffset(taskDailyChat, base, 20, "wb2a-yeying-2026"))
	if !at.Equal(want) {
		t.Fatalf("next=%v want %v (jitter offset)", at, want)
	}
	if at.Before(base) || !at.Before(base.Add(20*time.Minute)) {
		t.Fatalf("next=%v 不在 08:00–08:20 窗口内", at)
	}
	if again, _ := s.nextWake(now); !again.Equal(at) {
		t.Fatalf("抖动必须确定性：%v != %v", again, at)
	}

	// 关闭抖动 ⇒ 逐字回到整点（与引入前一致）
	s0 := New(Config{
		CheckinDisabled: true, TravelDisabled: true, ActivityDisabled: true,
		KeepaliveDisabled: true, GrowthDisabled: true, CatDisabled: true,
		DailyChatHours: []int{8},
	})
	if off, _ := s0.nextWake(now); !off.Equal(base) {
		t.Fatalf("jitter=0 时 next=%v want %v", off, base)
	}
}
