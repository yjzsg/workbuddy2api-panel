// jitter_test.go 排程随机化（schedule.jitter_minutes / random_delay_minutes /
// account_delay_*）的单测。[移植自根上游 84e367e / 29b5d0d / c5fc618]
package scheduler

import (
	"context"
	"testing"
	"time"
)

// TestJitterOffsetDeterministic 同一 (任务类, 名义时点, 盐) 的偏移必须稳定：
// Run 主循环可能在一小时内被唤醒多次，偏移若每轮重摇，同一个整点会被重复派发。
func TestJitterOffsetDeterministic(t *testing.T) {
	nominal := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	first := jitterOffset(taskCheckin, nominal, 60, "salt-a")
	if first < 0 || first >= time.Hour {
		t.Fatalf("offset=%v 应落在 [0, 60m)", first)
	}
	if again := jitterOffset(taskCheckin, nominal, 60, "salt-a"); again != first {
		t.Errorf("同输入偏移不稳定: %v != %v", again, first)
	}
	// 不同实例盐应给出不同偏移（FNV 冲突概率可忽略；用多组抵消偶然相等）。
	diff := false
	for _, salt := range []string{"salt-b", "salt-c", "salt-d"} {
		if jitterOffset(taskCheckin, nominal, 60, salt) != first {
			diff = true
			break
		}
	}
	if !diff {
		t.Errorf("不同盐应产生不同偏移（got 全部 %v）", first)
	}
	// 不同任务类应错开（同一小时多个任务不同秒触发）。
	if jitterOffset(taskTravel, nominal, 60, "salt-a") == first {
		t.Logf("checkin 与 travel 偏移相同（%v）：概率事件，不视为失败", first)
	}
}

// TestJitterOffsetZeroIsExactHour 关闭抖动时偏移必须为 0：与引入前逐字一致。
func TestJitterOffsetZeroIsExactHour(t *testing.T) {
	nominal := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	if d := jitterOffset(taskCheckin, nominal, 0, "salt"); d != 0 {
		t.Errorf("jitterMinutes=0 时偏移应为 0，got %v", d)
	}
	if d := jitterOffset(taskCheckin, nominal, -5, "salt"); d != 0 {
		t.Errorf("负窗口应视为关闭，got %v", d)
	}
}

// TestNextFireJitterStaysInWindow 抖动后的触发时刻必须仍在同一小时内且晚于 now。
func TestNextFireJitterStaysInWindow(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, loc)
	next := nextFire(now, []int{9, 21}, taskCheckin, 30, "seed")
	if next.Hour() != 21 || next.Day() != 27 {
		t.Fatalf("next=%v want 21:xx same day", next)
	}
	if next.Minute() >= 30 {
		t.Errorf("next=%v 偏移应在 30 分钟窗口内", next)
	}
	if !next.After(now) {
		t.Errorf("next=%v 必须晚于 now=%v", next, now)
	}
}

// TestScheduledSlotWithinHour scheduledSlot 的四种边界：窗口内、确定性、
// 0 关闭（整点）、>60 钳到本小时末。
func TestScheduledSlotWithinHour(t *testing.T) {
	day := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	for _, hour := range []int{0, 9, 23} {
		at := scheduledSlot(day, hour, taskCheckin, 30, "seed")
		start := day.Add(time.Duration(hour) * time.Hour)
		if at.Before(start) || !at.Before(start.Add(time.Hour)) {
			t.Errorf("hour=%d at=%v 未落在 [%v, +1h)", hour, at, start)
		}
		if again := scheduledSlot(day, hour, taskCheckin, 30, "seed"); !again.Equal(at) {
			t.Errorf("hour=%d 派生不稳定: %v != %v", hour, again, at)
		}
	}
	// minutes<=0：保持整点。
	if at := scheduledSlot(day, 9, taskCheckin, 0, "seed"); !at.Equal(day.Add(9 * time.Hour)) {
		t.Errorf("minutes=0 应返回整点，got %v", at)
	}
	// minutes>60：钳住，仍在本小时内（不能跨进下一档）。
	at := scheduledSlot(day, 9, taskCheckin, 600, "seed")
	if at.Hour() != 9 || at.Day() != 21 {
		t.Errorf("超大窗口应钳到本小时，got %v", at)
	}
	// 不同种子应在小时窗口内错开（用多组抵消偶然相等）。
	same := true
	for _, seed := range []string{"other", "third", "fourth"} {
		if !scheduledSlot(day, 9, taskCheckin, 30, seed).Equal(scheduledSlot(day, 9, taskCheckin, 30, "seed")) {
			same = false
			break
		}
	}
	if same {
		t.Errorf("不同种子应派生不同时刻")
	}
}

// TestSchedParamsFireAtDefaultsToNextFire 未配 random_delay 时退回 nextFire 语义
// （含 jitter）：两条路径不能各算一套。
func TestSchedParamsFireAtDefaultsToNextFire(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, loc)
	p := schedParams{jitterMinutes: 10, jitterSalt: "s"}
	got := p.fireAt(now, []int{9, 21}, taskCheckin)
	want := nextFire(now, []int{9, 21}, taskCheckin, 10, "s")
	if !got.Equal(want) {
		t.Errorf("fireAt=%v want %v", got, want)
	}
	// randomDelay 打开后也必须晚于 now，且落在目标整点小时内。
	p = schedParams{randomDelay: 30, randomSeed: "seed"}
	got = p.fireAt(now, []int{21}, taskCheckin)
	if !got.After(now) || got.Hour() != 21 || got.Minute() >= 30 {
		t.Errorf("fireAt(randomDelay)=%v 应在 21:00–21:29 且晚于 now", got)
	}
}

// TestAccountGapOnlyInScheduledCtx 账号间随机抖动只作用于排程遍历：
// 手动入口（无标记 ctx）必须保持原节奏——否则「立即补跑」会莫名多等。
func TestAccountGapOnlyInScheduledCtx(t *testing.T) {
	s := New(Config{AccountDelayMinSeconds: 2, AccountDelayMaxSeconds: 5})
	base := 800 * time.Millisecond
	if got := s.accountGap(context.Background(), base); got != base {
		t.Errorf("手动 ctx 不该抖动: got %v want %v", got, base)
	}
	ctx := context.WithValue(context.Background(), scheduledAccountKey{}, true)
	for i := 0; i < 64; i++ {
		got := s.accountGap(ctx, base)
		if got < base+2*time.Second || got > base+5*time.Second {
			t.Fatalf("抖动越界: got %v want [%v, %v]", got, base+2*time.Second, base+5*time.Second)
		}
	}
	// max=0 关闭：排程 ctx 也回到 base。
	off := New(Config{})
	if got := off.accountGap(ctx, base); got != base {
		t.Errorf("未配置时应关闭: got %v", got)
	}
	// 非法区间（min>max）兜底回 base，不 panic。
	bad := New(Config{AccountDelayMinSeconds: 9, AccountDelayMaxSeconds: 3})
	if got := bad.accountGap(ctx, base); got != base {
		t.Errorf("非法区间应回 base: got %v", got)
	}
}
