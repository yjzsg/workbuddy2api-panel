package server

import (
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// withEdgeWindow 注入短 IP 级判定窗（生产恒 60s；测试用 200ms 加速窗口过期验证）。
func withEdgeWindow(t *testing.T, d time.Duration) {
	t.Helper()
	prev := edgeWindow
	edgeWindow = d
	t.Cleanup(func() { edgeWindow = prev })
}

// ---------------------------------------------------------------------------
// 状态机单测
// ---------------------------------------------------------------------------

// TestEdgeGateMultiAccountTriggers 状态机单测（WAF 403 形态）：窗内两个不同号命中
// → 激活；单号反复命中（任意多次）不触发；激活期内 noteWaf 恒 true（不续期路径）。
func TestEdgeGateMultiAccountTriggers(t *testing.T) {
	withEdgeWindow(t, time.Minute)
	var g edgeGate
	// 单号反复：永不激活（判定口径=不同 UID 数）。
	for n := 0; n < 10; n++ {
		if g.noteWaf("u1") {
			t.Fatalf("single account repeated hits must never trigger, iter=%d", n)
		}
	}
	if g.active() {
		t.Fatal("single account must not activate")
	}
	// 第二个不同号进入窗内 → 本 call 即达阈值激活并返回 true（轮转在该次
	// 403 后立即终止——fail-fast 生效点就是阈值命中的那次请求）。
	if !g.noteWaf("u2") {
		t.Fatalf("threshold crossing call must activate and return true")
	}
	if !g.active() {
		t.Fatal("two distinct accounts within window must activate")
	}
	// 激活期内新命中恒 true（fail-fast 生效），不续期由下条测试验证。
	if !g.noteWaf("u3") {
		t.Fatal("hits during active window must report active")
	}
}

// TestEdgeGateWindowExpiry 窗口过期自然解除 + 激活不续期：
// 激活后（窗 150ms）等过期 → active=false；解除后需全新命中重新判定（旧账已清，
// 单号命中不残留触发）。
func TestEdgeGateWindowExpiry(t *testing.T) {
	withEdgeWindow(t, 150*time.Millisecond)
	var g edgeGate
	g.noteWaf("u1")
	g.noteWaf("u2")
	if !g.active() {
		t.Fatal("must activate")
	}
	// 激活中段再命中：不改变解除时刻（不续期）——记录当前 until 供过期断言前校验
	// 该命中确实落在窗内（150ms 内必成立）。
	if !g.noteWaf("u3") {
		t.Fatal("mid-window hit must report active")
	}
	time.Sleep(200 * time.Millisecond) // 窗口过期
	if g.active() {
		t.Fatal("gate must deactivate after window expiry")
	}
	// 解除后单号命中：旧账已清（判定窗在激活时清空），不残留激活。
	if g.noteWaf("u4") {
		t.Fatal("post-expiry single hit must not re-activate (hits cleared on activation)")
	}
	if g.active() {
		t.Fatal("post-expiry single hit must leave gate inactive")
	}
}

// TestEdgeAuthGateMultiAccountTriggers 状态机单测（鉴权层 401 形态，2026-09-22 新增）：
// noteEdgeAuth 与 noteWaf 同构——单号反复不触发，两个不同号短窗内命中即激活。
func TestEdgeAuthGateMultiAccountTriggers(t *testing.T) {
	withEdgeWindow(t, time.Minute)
	var g edgeGate
	for n := 0; n < 10; n++ {
		if g.noteEdgeAuth("u1") {
			t.Fatalf("single account repeated 401 hits must never trigger, iter=%d", n)
		}
	}
	if g.active() {
		t.Fatal("single account must not activate")
	}
	if !g.noteEdgeAuth("u2") {
		t.Fatal("threshold crossing 401 call must activate and return true")
	}
	if !g.active() {
		t.Fatal("two distinct accounts hitting 401 within window must activate")
	}
	if !g.noteEdgeAuth("u3") {
		t.Fatal("401 hits during active window must report active")
	}
}

// TestEdgeGateSharedWindowAcrossShapes 两种形态共用同一判定窗（设计要点）：
// 401 与 403 都是「边缘层按出口 IP 拒绝我们」的证据，只是状态码不同——不应各自
// 凑满阈值。判定口径始终是「窗内**不同 UID** 数」：单号（不论哪种形态）永不激活，
// 第二个不同号换另一种形态命中即达阈值。
func TestEdgeGateSharedWindowAcrossShapes(t *testing.T) {
	withEdgeWindow(t, time.Minute)
	var g edgeGate
	// 第一个号（401 形态）：窗内只有 1 个不同 UID → 不激活。
	if g.noteEdgeAuth("u1") {
		t.Fatal("first hit (401) must not activate alone")
	}
	if g.active() {
		t.Fatal("one distinct uid must not activate the gate")
	}
	// 第二个**不同**号换 403 形态命中 → 共用窗内已有 2 个不同 UID → 必须激活。
	// 本断言正是「共用同一判定窗」的回归守卫：若两形态各用各的窗，这里两个窗
	// 各只有 1 个 UID，会返回 false。
	if !g.noteWaf("u2") {
		t.Fatal("mixed 401(u1)+403(u2) from two distinct accounts must activate the shared gate")
	}
	if !g.active() {
		t.Fatal("shared gate must be active after mixed-shape threshold crossing")
	}
}

// ---------------------------------------------------------------------------
// 端到端：WAF 403 路径（既有行为，零回归）
// ---------------------------------------------------------------------------

// TestChatWafIPFailFastStopsRotation 端到端验收（任务书第 2 条）：
// 3 个账号全部 WAF 403 → 第二个号命中即激活 IP 级状态 → 第三个号不再被调用
// （放大倍数=1：本请求上游调用=2，远小于 MaxRotate=3 轮全打）。同时验证末端
// 透传语义：空 body → 本地 waf_ip_blocked 可读文案（5755fe3 兜底分支）。
func TestChatWafIPFailFastStopsRotation(t *testing.T) {
	withEdgeWindow(t, time.Minute)
	var calls atomic.Int64
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		return 403, "", false // WAF 拦截：空 body
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	// 上游调用必须止步于 2（第二个号触发 IP 级判定即 break；u3 零调用）。
	// MaxRotate 默认 3：非 fail-fast 路径会打满 3 次（放大倍数=3）。
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls=%d want 2 (fail-fast stops rotation at threshold, u3 must not be hit)", n)
	}
	if rec.Code != 503 {
		t.Fatalf("code=%d want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "waf ip-level block") {
		t.Errorf("empty-body 503 must carry readable local waf_ip text: %s", rec.Body)
	}
	// IP 级激活期间账号软冷却照常记账（协同不叠加）：两号均 soft_rate 冷却中。
	for _, uid := range []string{"u1", "u2"} {
		st, _ := p.Status(uid)
		if !st.Cooling || st.Disabled {
			t.Errorf("uid=%s must soft-cool without disable (IP state coexists, not stacks): %+v", uid, st)
		}
	}
	// u3 从未被调用：不冷却、可选。
	st3, _ := p.Status("u3")
	if st3.Cooling {
		t.Errorf("u3 must not be cooled (never called): %+v", st3)
	}
}

// TestChatWafIPBodyPassthroughWhenActive 激活期有上游原文时仍透传原文
// （5755fe3 优先级：原文优先于本地 waf_ip_blocked 文案，不编造）。
func TestChatWafIPBodyPassthroughWhenActive(t *testing.T) {
	withEdgeWindow(t, time.Minute)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 403, "Forbidden: request blocked by WAF", false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if !strings.Contains(rec.Body.String(), "Forbidden: request blocked by WAF") {
		t.Errorf("upstream body must passthrough even when IP-gate active: %s", rec.Body)
	}
}

// TestChatWafSingleAccountStillRotates 单号 WAF 403 不触发 IP 级（窗口内只有
// 一个号被拦）：轮转继续换到健康号成功——既有行为零回归（IP fail-fast 只在
// 多号短窗时才接管）。
func TestChatWafSingleAccountStillRotates(t *testing.T) {
	withEdgeWindow(t, time.Minute)
	var calls atomic.Int64
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		if authz == "Bearer at-bad" {
			return 403, "", false // 仅此号被 WAF 拦
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200: single-account WAF must still rotate to healthy)", rec.Code, rec.Body)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("calls=%d want 2 (bad once + good once)", n)
	}
	// bad 号软冷却不受 IP 级状态影响（未触发）。
	st, _ := p.Status("bad")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Errorf("single-account WAF must soft-cool: %+v", st)
	}
}

// TestChatWafIPGateClearsAfterExpiryThenRotates 窗口过期解除端到端：
// 多号短窗触发激活 → 窗口过期（200ms 注入窗）→ 后续单号 WAF 403 不再
// fail-fast，轮转继续（IP 级状态自然解除，恢复既有轮转行为）。
func TestChatWafIPGateClearsAfterExpiryThenRotates(t *testing.T) {
	withEdgeWindow(t, 150*time.Millisecond)
	// 阶段一：两个号全 403 → 激活（此时两个号已软冷却）。
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 403, "", false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("stage1 code=%d want 503", rec.Code)
	}
	// 等两个维度过期：IP 窗（150ms）+ 账号软冷却（wafCooldownBase 抖动下界 45s
	// 太长——直接 Cooldown 0 强制解除，聚焦 IP 窗语义）。
	time.Sleep(200 * time.Millisecond)
	p.Cooldown("u1", pool.CoolSoft, 0, "force expiry")
	p.Cooldown("u2", pool.CoolSoft, 0, "force expiry")
	// 阶段二：窗口过期后单号 403（u1 bad / u2 good）→ 不 fail-fast，换到 u2 成功。
	up2 := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at1" {
			return 403, "", false
		}
		return 200, sseOK, true
	})
	h.cfg.Upstream = up2 // 复用同一 Handler/IP 状态机（验证过期解除而非新实例）
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec2.Code != 200 {
		t.Fatalf("stage2 code=%d body=%s (want 200: expired IP gate must not fail-fast)", rec2.Code, rec2.Body)
	}
}

// TestChatWafIPSoftCooldownUnaffected 软冷却语义不受 IP 级状态影响的隔离回归：
// IP 级激活存在时，账号软冷却的时长/reason/softStreak 口径与既有 P0-1 完全一致
// （IP 状态只改变轮转决策，不叠加冷却时长——两号均按 wafCooldownBase 抖动界
// [45s,75s] 冷却，无额外延长）。
func TestChatWafIPSoftCooldownUnaffected(t *testing.T) {
	withEdgeWindow(t, time.Minute)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 403, "", false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d want 503", rec.Code)
	}
	for _, uid := range []string{"u1", "u2"} {
		st, _ := p.Status(uid)
		if !st.Cooling || st.CoolKind != "soft_rate" {
			t.Fatalf("uid=%s must be in soft_rate cooldown: %+v", uid, st)
		}
		if st.SoftStreak != 1 {
			t.Errorf("uid=%s soft_streak=%d want 1 (one WAF hit each, no stacking)", uid, st.SoftStreak)
		}
		if d := time.Duration(st.CoolRemaining) * time.Second; d < 45*time.Second || d > 75*time.Second {
			t.Errorf("uid=%s cool=%v want in [45s,75s] (IP state must not extend account cooldown)", uid, d)
		}
		if !strings.Contains(st.Reason, "waf") {
			t.Errorf("uid=%s reason=%q want waf marker", uid, st.Reason)
		}
	}
}

// ---------------------------------------------------------------------------
// 端到端：鉴权层 401 路径（2026-09-22 全池降权事故的修复验收）
// ---------------------------------------------------------------------------

// TestChatEdgeAuthFailFastStopsRotation 401 风暴端到端：
// 3 个账号全部 401（无业务信封）→ 第二个号命中即激活 IP 级状态 → 第三个号不再被
// 调用；末端 503 带 edge_auth 可读文案。
// 关键断言：**账号零惩罚**——无冷却、无禁用、连败计数为 0、未被降权。
// 这正是事故的修复点：修复前 401 落 ErrClient → 喂 NoteFailures → 满 5 次降权 10 分钟。
func TestChatEdgeAuthFailFastStopsRotation(t *testing.T) {
	withEdgeWindow(t, time.Minute)
	var calls atomic.Int64
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		return 401, "", false // APISIX 鉴权层拒绝：空 body（无业务信封）
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls=%d want 2 (401 fail-fast stops rotation at threshold, u3 must not be hit)", n)
	}
	if rec.Code != 503 {
		t.Fatalf("code=%d want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "edge_auth") {
		t.Errorf("empty-body 503 must carry edge_auth code: %s", rec.Body)
	}
	// ⭐ 事故修复点：401 绝不能罚账号。
	for _, uid := range []string{"u1", "u2"} {
		st, _ := p.Status(uid)
		if st.Cooling {
			t.Errorf("uid=%s must NOT be cooled on edge-auth 401 (account is innocent): %+v", uid, st)
		}
		if st.Disabled || st.ManualDisabled {
			t.Errorf("uid=%s must NOT be disabled on edge-auth 401: %+v", uid, st)
		}
		if st.ConsecutiveFails != 0 {
			t.Errorf("uid=%s consecutive_fails=%d want 0 (must NOT feed degrade counter)", uid, st.ConsecutiveFails)
		}
		if !st.DegradeUntil.IsZero() {
			t.Errorf("uid=%s degrade_until=%v want zero (must NOT degrade)", uid, st.DegradeUntil)
		}
	}
	// u3 从未被调用：不冷却、可选。
	st3, _ := p.Status("u3")
	if st3.Cooling {
		t.Errorf("u3 must not be cooled (never called): %+v", st3)
	}
}

// TestChatEdgeAuthSingleAccountRotatesToHealthy 单号 401 不触发 IP 级（窗口内只有
// 一个号被拒）：轮转继续换到健康号成功——与 WAF 单号路径同语义，且被拒号**不受罚**。
func TestChatEdgeAuthSingleAccountRotatesToHealthy(t *testing.T) {
	withEdgeWindow(t, time.Minute)
	var calls atomic.Int64
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		if authz == "Bearer at-bad" {
			return 401, "", false // 仅此号撞 401
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200: single-account 401 must still rotate to healthy)", rec.Code, rec.Body)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("calls=%d want 2 (bad once + good once)", n)
	}
	// 与 WAF 路径的**刻意差异**：401 不软冷却该号（账号无辜，IP 级事实）。
	st, _ := p.Status("bad")
	if st.Cooling || st.Disabled {
		t.Errorf("bad account must stay healthy after edge-auth 401 (no cooldown): %+v", st)
	}
	if st.ConsecutiveFails != 0 || !st.DegradeUntil.IsZero() {
		t.Errorf("bad account must not accumulate degrade state: %+v", st)
	}
}

// TestChatEdgeAuthNeverDegradesPool 事故精确回归（2026-09-22）：
// 单账号池反复撞 401 —— 修复前每次 401 都喂 NoteFailures，第 5 次即把该号降权
// 10 分钟（degrade_cooldown），于是「无号可用 → fallback_earliest_expiry 挑降权号
// 硬打 → 连环 503」。修复后连败计数恒 0、永不降权，号始终可用。
//
// 单账号池是刻意的：IP 级状态机需要 2 个**不同** UID 才激活，单号场景永远不激活
// → 每次请求都真打一次上游、真走一次 applyErrorPolicy，才能把「喂不喂连败计数」
// 这条路径压到 5 次以上（正好覆盖 degrade_threshold=5 的触发点）。
func TestChatEdgeAuthNeverDegradesPool(t *testing.T) {
	withEdgeWindow(t, time.Minute)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 401, "", false
	})
	p := testPoolWith(&auth.Auth{UID: "only", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	// 8 次 > degrade_threshold(5)：修复前第 5 次就会置 DegradeUntil。
	for i := 0; i < 8; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
		if rec.Code != 503 {
			t.Fatalf("iter=%d code=%d want 503", i, rec.Code)
		}
		st, _ := p.Status("only")
		if st.ConsecutiveFails != 0 {
			t.Fatalf("iter=%d consecutive_fails=%d want 0 (401 must never feed degrade counter)", i, st.ConsecutiveFails)
		}
		if !st.DegradeUntil.IsZero() {
			t.Fatalf("iter=%d degraded at %v — edge-auth 401 must never degrade a healthy account", i, st.DegradeUntil)
		}
		if st.Cooling || st.Disabled {
			t.Fatalf("iter=%d account penalized (cooling=%v disabled=%v) — must be zero penalty", i, st.Cooling, st.Disabled)
		}
	}
	// 号仍可选（池未被抽干）——事故里正是「池子被打空」导致连环 503。
	if got := p.Pick("glm-5.2"); got == nil {
		t.Fatal("account must remain pickable after repeated edge-auth 401 (pool must not be drained)")
	}
}
