// dailychat.go 每日对话保底：国际版积分说明的硬条件——「当天必须通过 WorkBuddy /
// CodeBuddy 客户端发起至少 1 次有效对话或任务」，否则当天那 30 分拿不到。
//
// 用户自己用客户端的日子天然满足；本任务兜住「那天没打开客户端」的情况。
// 与 CN 邀请的「桌面六事件链」不同：那边判据是事件埋点，这边判据是**真实对话**，
// 故走真实 chat 请求（复用 DesktopDailyChat 的桌面指纹形态）。
package scheduler

import (
	"log"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// dailyChatModel 按 realm 选**免费档**（credits x0.00）模型：本任务只求「当天发生过
// 一次有效对话」，不该为此消耗积分。
//
//	global → deepseek-v4.1-flash（x0.00；同价的 hy3 / hy4-preview-f 里上下文最大）
//	cn     → hy3（x0.00；cn 侧 deepseek-v4.1-flash 是 x0.03，不用）
//
// 上游只认裸模型名（realm 前缀是网关侧路由协议），故不带前缀。
func dailyChatModel(a *auth.Auth) string {
	if a != nil && a.Realm() == "global" {
		return "deepseek-v4.1-flash"
	}
	return "hy3"
}

// RunDailyChatNow 立即执行每日对话保底：对每个非禁用账号发一次最小有效对话。
//
// 幂等性：**不做**「当天已对话则跳过」的判断——多打一次免费请求无副作用，而漏打
// 就丢 30 分；上游的按天判据我们无法从网关侧可靠读到，故取「宁可多打」。
// 并发：与 RunBalanceRefreshNow 同口径，逐账号一个 goroutine，全部收尾后返回。
func (s *Scheduler) RunDailyChatNow() {
	var wg sync.WaitGroup
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(a *auth.Auth, uid, nickname string) {
			defer wg.Done()
			model := dailyChatModel(a)
			// 一次重试（间隔 2s）：并发下偶发流形态异常——实测 29 号两轮各失败 1 号，
			// 且失败账号每轮不同（单号复测恒 200），重试一次即可覆盖。
			var err error
			for attempt := 1; attempt <= 2; attempt++ {
				if attempt > 1 {
					time.Sleep(2 * time.Second)
				}
				if err = s.cfg.Upstream.DesktopDailyChat(a, model); err == nil {
					suffix := ""
					if attempt > 1 {
						suffix = " [retry]"
					}
					log.Printf("daily chat %s: ok (%s)%s", logfmt.Label(uid, nickname), model, suffix)
					return
				}
			}
			log.Printf("daily chat %s (%s): %v", logfmt.Label(uid, nickname), model, err)
		}(a, st.UID, st.Nickname)
	}
	wg.Wait()
}
