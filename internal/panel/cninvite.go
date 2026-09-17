// cninvite.go 面板「CN 邀请」视图（面板层新增）：最近一次运行结果 + 立即执行一轮。
//
// 排程本体在 internal/scheduler/cn_invite.go（每日窗口内自动跑）；本文件只做面板读写：
//
//	GET  /panel/api/cninvite/status   最近一次结果 + 配置（只读，不触发上游调用）
//	POST /panel/api/cninvite/run      立即执行一轮（异步，进度看运行日志）
package panel

import (
	"log"
	"net/http"
)

// cnInviteStatus 最近一次 CN 邀请运行结果（只读）。
func (p *Panel) cnInviteStatus(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	code, hours, until, off := p.cfg.Scheduler.CNInviteConfig()
	at, running, items := p.cfg.Scheduler.CNInviteStatus()
	out := map[string]any{
		"ok":       true,
		"enabled":  !off,
		"code":     code,
		"hours":    hours,
		"until":    until,
		"running":  running,
		"accounts": items,
	}
	if !at.IsZero() {
		out["last_run"] = at.Format("2006-01-02 15:04:05")
	}
	writeJSON(w, http.StatusOK, out)
}

// cnInviteRun 立即执行一轮（绑码 + 桌面事件链），异步。
func (p *Panel) cnInviteRun(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunCNInviteAllNow()
	log.Printf("panel: CN 邀请一轮已触发（绑码 + 桌面事件链）")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}
