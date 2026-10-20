package main

// 被封账号自动探测恢复（2026-10-20 防御加固）。
//
// 背景：上游对「账号存在异常行为」会返回 403 + 11140，网关按既有语义硬禁用
// （reason = "account banned by upstream (11140 request illegal), re-login required"）。
// 这类限制是**账号级、有时限**的，实测同一账号：
//   - 账务接口（/v2/billing/meter/get-user-resource）仍 200，token 未失效；
//   - 对话接口稳定 403，且官方客户端同样不可用（用户实测）；
//   - 用完整官方头族 + 合法 X-Conversation-ID 直连仍 403（排除"请求形状"误判）。
// 也就是说：**重登无效，只能等上游解除**。
//
// 这里做的是「等解除」的自动化：每隔一段时间，对处于该禁用原因的账号发一次
// 1-token 探测；一旦返回 200 就自动 revive 回池。探测频率刻意很低（30 分钟/号），
// 避免给已经受限的账号继续制造流量特征。
//
// 边界：
//   - 只处理 reason 前缀为 "account banned by upstream" 的账号；手动禁用、余额冷却、
//     内容审核等其他禁用原因一律不碰（那些要么是人的决定，要么有别的恢复路径）。
//   - 探测失败（403/网络错误）只记日志，不改状态——下次继续等。
//   - 探测成功即 revive，但**不重置**该账号的其它冷却/熔断状态（Revive 的既有语义）。

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// bannedReasonPrefix 必须与 server.applyErrorPolicy 写入 11140 硬禁用时的 reason 前缀一致。
const bannedReasonPrefix = "account banned by upstream"

// reviveProbeInterval 探测周期：单个受限账号每 30 分钟最多被探一次。
const reviveProbeInterval = 30 * time.Minute

// reviveProbeTimeout 单次探测上限。探测只是 1-token 请求，正常应在 1s 内返回。
const reviveProbeTimeout = 30 * time.Second

// startBannedReviveProbe 启动后台探测循环（ctx 取消即退出）。
func startBannedReviveProbe(ctx context.Context, p *pool.Pool, up *upstream.Client) {
	if p == nil || up == nil {
		return
	}
	go func() {
		t := time.NewTicker(reviveProbeInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				probeBannedAccounts(ctx, p, up)
			}
		}
	}()
	log.Printf("[pool] 被封账号自动探测已启用：每 %s 探一次，解除限制后自动回池", reviveProbeInterval)
}

// probeBannedAccounts 对当前所有「上游封禁」账号各探一次。导出仅为测试可调。
func probeBannedAccounts(ctx context.Context, p *pool.Pool, up *upstream.Client) {
	for _, st := range p.List() {
		if !st.Disabled || !strings.HasPrefix(st.DisabledReason, bannedReasonPrefix) {
			continue
		}
		a := p.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		status, err := probeOnce(ctx, up, a)
		switch {
		case err == nil && status == 200:
			if p.Revive(st.UID) {
				log.Printf("INFO: [pool] 上游限制已解除，自动恢复 acct=%s（探测 200）", shortUID(st.UID))
			}
		case status == 403:
			log.Printf("INFO: [pool] acct=%s 仍被上游限制（探测 403），保持禁用", shortUID(st.UID))
		default:
			log.Printf("WARN: [pool] 恢复探测 acct=%s status=%d err=%v（保持禁用）", shortUID(st.UID), status, err)
		}
	}
}

// probeOnce 发一条 1-token 非流式请求，返回上游状态码。
func probeOnce(ctx context.Context, up *upstream.Client, a *auth.Auth) (int, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      "deepseek-v4.1-flash",
		"stream":     false,
		"max_tokens": 1,
		"messages": []any{
			map[string]any{"role": "user", "content": "1"},
		},
	})
	reqCtx, cancel := context.WithTimeout(ctx, reviveProbeTimeout)
	defer cancel()
	rc, status, _, err := up.ChatStreamContext(reqCtx, a, body, "", upstream.ChatMeta{})
	if rc != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(rc, 8192))
		_ = rc.Close()
	}
	return status, err
}

// shortUID 日志用短 uid（前 8 位）。
func shortUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}
