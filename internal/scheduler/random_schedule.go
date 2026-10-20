// random_schedule.go 排程随机化。[移植自根上游 c5fc618，适配本仓结构]
//
// 两个独立开关：
//   - schedule.account_delay_min/max_seconds：排程遍历时给账号间最小间隔追加随机
//     秒数，把「固定 800ms 一个号」的节奏摊开；只作用于排程遍历，手动补跑不受影响。
//   - schedule.random_delay_minutes：把每个每日档位在其小时窗口内摊开（与
//     schedule.jitter_minutes 的「整点后偏移」是两种语义，可各自开关）。
//
// 两者都是**确定性**或**无状态**派生，不新增上游请求：抖动只是把既有的触发时刻平移。
package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/rand"
	"time"
)

// scheduledAccountKey 排程遍历标记：只有 Run 主循环派发的批次（runBatch）带它，
// 手动入口（RunXxxNow，走 context.Background()）不带 ⇒ 账号间抖动只作用于排程遍历。
type scheduledAccountKey struct{}

// accountGap 在原有的账号间最小间隔上追加随机秒数（schedule.account_delay_*）。
//
// 只在排程遍历且区间合法（0 ≤ min ≤ max）时生效；区间未配置（max<=0）或非法时
// 原样返回 base —— 配置侧的 normalize 已拦截非法值，这里只是不 panic 的兜底。
// 读 s.cfg 的 AccountDelay* 字段不加锁：它们在 New 之后不再被 Reconfigure 改写。
func (s *Scheduler) accountGap(ctx context.Context, base time.Duration) time.Duration {
	if ctx.Value(scheduledAccountKey{}) != true || s.cfg.AccountDelayMaxSeconds <= 0 {
		return base
	}
	lo, hi := s.cfg.AccountDelayMinSeconds, s.cfg.AccountDelayMaxSeconds
	if lo < 0 || hi < lo {
		return base
	}
	return base + time.Duration(lo+rand.Intn(hi-lo+1))*time.Second
}

// scheduledSlot 由部署私有种子确定性派生「某天某小时某任务」在小时窗口内的触发时刻
// （schedule.random_delay_minutes）。
//
// 为什么用 sha256 而不是 math/rand：同一 slot 在进程重启后必须仍是同一时刻（否则重启
// 一次节奏就变），且不同日期/小时/任务类/部署各自取值——没有可变随机状态可丢，因此
// 也不会像「每轮现摇」那样把同一个小时重复派发。
func scheduledSlot(day time.Time, hour int, kind taskKind, minutes int, seed string) time.Time {
	at := time.Date(day.Year(), day.Month(), day.Day(), hour, 0, 0, 0, day.Location())
	if minutes <= 0 {
		return at
	}
	// 每个取值都留在原小时内与原自然日内：60 分钟时最大偏移 59:59，不与下一档重叠。
	if minutes > 60 {
		minutes = 60
	}
	seconds := minutes * 60
	if seconds >= 3600 {
		seconds = 3599
	}
	identity := fmt.Sprintf("%s\x00%s\x00%d\x00%d", seed, at.Format("2006-01-02"), hour, kind)
	digest := sha256.Sum256([]byte(identity))
	offset := binary.BigEndian.Uint64(digest[:8]) % uint64(seconds+1)
	return at.Add(time.Duration(offset) * time.Second)
}
