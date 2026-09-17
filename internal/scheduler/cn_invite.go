// cn_invite.go CN 侧邀请活动闭环（面板层新增）：每号绑码（幂等）+ 每天一次"桌面六事件链"。
//
// 为什么需要：CN 邀请奖励 = 基础奖 50（好友首次使用）+ 活跃奖 100（好友 7 日内累计使用 3 天）。
// 实测「网关对话不算使用」——判据是官方客户端的 /v2/report 事件链（upstream.ReportDesktopEvent +
// upstream.DesktopChatSequence）。本任务把它放进面板排程：活动窗口内每天对每个 CN 账号发一次，
// 自然累计"使用天数"；绑码是幂等的（新号入库后自动补绑）。
//
// 面板可见：/panel/api/cninvite/status（最近一次结果）+ /panel/api/cninvite/run（立即执行）。
package scheduler

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// CNInviteItem 单账号一轮结果（面板只读展示）。
type CNInviteItem struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Bind     string `json:"bind"`     // ok | already | own-code | not-new | code=N | error:...
	Activate string `json:"activate"` // ok | error:...
}

// RunCNInviteAllNow 对全部 CN 账号执行一轮：绑码（幂等）+ 桌面事件链。
// 面板任务中心「立即执行」与每日排程共用（幂等，可重复跑；窗口外静默跳过）。
func (s *Scheduler) RunCNInviteAllNow() {
	code, _, until, off := s.CNInviteConfig()
	if off || strings.TrimSpace(code) == "" {
		return
	}
	if !cnInviteInWindow(time.Now(), until) {
		log.Printf("CN邀请: 活动窗口已结束（until=%s），跳过", until)
		return
	}
	s.cnInvMu.Lock()
	if s.cnInvRunning {
		s.cnInvMu.Unlock()
		log.Printf("CN邀请: 上一轮仍在跑，跳过本次")
		return
	}
	s.cnInvRunning = true
	s.cnInvMu.Unlock()

	items := make([]CNInviteItem, 0, 8)
	defer func() {
		s.cnInvMu.Lock()
		s.cnInvLast = items
		s.cnInvAt = time.Now()
		s.cnInvRunning = false
		s.cnInvMu.Unlock()
	}()

	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessToken == "" {
			continue
		}
		if a.IsGlobal() {
			continue // D4 门控：global 无 CN 活动体系，不发起上游调用
		}
		it := s.cnInviteOne(a, code)
		items = append(items, it)
		log.Printf("CN邀请 %s: bind=%s activate=%s", it.Nickname, it.Bind, it.Activate)
		time.Sleep(activityAccountDelay)
	}
}

// RunCNInviteAccountNow 单账号一轮（面板逐账号执行用）。
func (s *Scheduler) RunCNInviteAccountNow(a *auth.Auth) CNInviteItem {
	code, _, _, off := s.CNInviteConfig()
	if off {
		return CNInviteItem{UID: a.UID, Nickname: a.Nickname, Bind: "disabled"}
	}
	return s.cnInviteOne(a, code)
}

// CNInviteStatus 最近一次运行结果（面板只读）。
func (s *Scheduler) CNInviteStatus() (time.Time, bool, []CNInviteItem) {
	s.cnInvMu.Lock()
	defer s.cnInvMu.Unlock()
	out := make([]CNInviteItem, len(s.cnInvLast))
	copy(out, s.cnInvLast)
	return s.cnInvAt, s.cnInvRunning, out
}

// CNInviteConfig 快照 CN 邀请配置（面板展示 / 排程共用）。
func (s *Scheduler) CNInviteConfig() (code string, hours []int, until string, disabled bool) {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	hours = append([]int(nil), s.cfg.CNInviteHours...)
	return s.cfg.CNInviteCode, hours, s.cfg.CNInviteUntil, s.cfg.CNInviteDisabled
}

// SetCNInvite 热改 CN 邀请配置（面板保存配置时调用；hours 为空则保持原值）。
func (s *Scheduler) SetCNInvite(code string, hours []int, until string, disabled bool) {
	s.schedMu.Lock()
	s.cfg.CNInviteCode = code
	if len(hours) > 0 {
		s.cfg.CNInviteHours = hours
	}
	s.cfg.CNInviteUntil = until
	s.cfg.CNInviteDisabled = disabled
	s.schedMu.Unlock()
	poke(s.rearmSchedule)
}

// cnInviteOne 单账号：绑码 + 发一次桌面六事件链。
func (s *Scheduler) cnInviteOne(a *auth.Auth, code string) CNInviteItem {
	it := CNInviteItem{UID: a.UID, Nickname: a.Nickname}
	if strings.TrimSpace(code) != "" {
		res, err := s.cfg.Upstream.InviteBind(a, code)
		switch {
		case err != nil:
			it.Bind = "error:" + shortErr(err)
		case res.Code == 0:
			it.Bind = "ok"
		case res.Code == 12310:
			it.Bind = "already"
		case res.Code == 12313:
			it.Bind = "own-code"
		case res.Code == 12311:
			it.Bind = "not-new"
		default:
			it.Bind = fmt.Sprintf("code=%d", res.Code)
		}
	}
	// 当天一次"使用"：桌面端六事件链（网关对话不算，见文件头注释）。
	conv := fmt.Sprintf("wb2api-cninv-%d-%s", time.Now().Unix(), shortUID(a.UID))
	if err := s.cfg.Upstream.ReportDesktopEvent(a,
		upstream.DesktopChatSequence(conv, conv+"-req", conv+"-msg", "fast-model", "fast-model")...); err != nil {
		it.Activate = "error:" + shortErr(err)
	} else {
		it.Activate = "ok"
	}
	return it
}

// cnInviteInWindow 是否在活动窗口内（until 为空 = 不过期；日期按本地时区，含当天）。
func cnInviteInWindow(now time.Time, until string) bool {
	until = strings.TrimSpace(until)
	if until == "" {
		return true
	}
	day, err := time.ParseInLocation("2006-01-02", until, now.Location())
	if err != nil {
		return true // 配置写坏时不静默停任务
	}
	return !now.After(day.Add(24*time.Hour - time.Second))
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

func shortUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}
