// queue_timeout_test.go 两条「不是账号的锅」的失败形态必须**只做模型级短避让**，
// 绝不落成账号级 600s 软冷却（那会把该号所有模型一起停掉）。
//
// 背景（2026-10-15 生产实测）：
//   - 6021 queue.full：一次请求轮转 6 个 CN 号各吃一次 → 若按账号级 600s 冷却，
//     六个号全灭 ⇒ 整个 CN 域（含 glm-5.3 / fast-model）一起报 rate_limit_exceeded，
//     而上游队列往往几十秒就排空——纯自伤。
//   - 上游头超时：卡住的号零惩罚 ⇒ 状态最干净 ⇒ 选号打分最优 ⇒ 被反复选中
//     （红花 10:26:30–10:29:20 连吃 4 次 120s）。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const queueFullBody = `{"code":6021,"msg":"queue.full.title","requestId":"b870ff984ed4a6b78916b88893a6b7a2"}`
const queueWaitingBody = `{"code":6020,"msg":"queue.waiting.title","requestId":"15ce3266","data":{"queue_position":526,"queue_size":526,"retry_after":40,"estimated_user_type":"free"}}`

// TestQueueFullUsesModelLevelCooldown 6021 queue.full：账号**不进**账号级冷却
// （其他模型照常可用），只对触发模型写一段 ≈ retry_after 的短避让。
func TestQueueFullUsesModelLevelCooldown(t *testing.T) {
	const model = "deepseek-v4.1-flash"
	p := testPoolWith(&auth.Auth{UID: "cn1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{
		Pool:     p,
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 429, queueFullBody, false }),
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)))

	// 客户端仍会拿到 429（上游队列确实满了，这一条请求注定失败）——但池子不该被罚 10 分钟。
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429（队列满是上游事实，客户端该看到 429），body=%s", rec.Code, rec.Body)
	}

	// 关键断言 1：账号**没有**账号级冷却 ⇒ 其他模型仍可选。
	if got := p.AvailableUIDsForRealm("cn"); len(got) != 1 || got[0] != "cn1" {
		t.Fatalf("6021 不该把账号整体冷却（其他模型会连坐）：AvailableUIDsForRealm=%v", got)
	}
	// 关键断言 2：触发模型被模型级避让 ⇒ 该模型暂时避开这个号。
	if got := p.AvailableUIDsForModelRealm(model, "cn"); len(got) != 0 {
		t.Fatalf("该模型应被模型级避让，AvailableUIDsForModelRealm=%v", got)
	}
	// 关键断言 3：时长是默认 60s 级，不是 soft_rate 的 600s。
	st := p.ModelBlockedForRealm(model, "cn")
	if !st.Blocked {
		t.Fatalf("应报该模型被挡：%+v", st)
	}
	if d := time.Until(st.Until); d > 120*time.Second {
		t.Fatalf("避让时长 %v 过久（want ≤120s，soft_rate 600s 是修复前的错误行为）", d)
	}
}

// TestQueueWaitingHonoursRetryAfter 6020 queue.waiting 带 data.retry_after:40 →
// 避让时长取上游给的值（不是写死的默认值）。
func TestQueueWaitingHonoursRetryAfter(t *testing.T) {
	const model = "deepseek-v4.1-flash"
	p := testPoolWith(&auth.Auth{UID: "cn1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{
		Pool:     p,
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 429, queueWaitingBody, false }),
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429", rec.Code)
	}

	st := p.ModelBlockedForRealm(model, "cn")
	if !st.Blocked {
		t.Fatalf("应报该模型被挡：%+v", st)
	}
	d := time.Until(st.Until)
	if d < 30*time.Second || d > 55*time.Second {
		t.Fatalf("避让时长 %v，want ≈40s（取 body 的 data.retry_after）", d)
	}
	if got := p.AvailableUIDsForRealm("cn"); len(got) != 1 {
		t.Fatalf("账号不该被账号级冷却：AvailableUIDsForRealm=%v", got)
	}
}

// TestPlainSoftRateStillAccountLevel 对照：**不带排队码**的普通 429 仍走账号级
// 软冷却（既有语义零变化）——否则本改动就把账号级限流也一起放过了。
func TestPlainSoftRateStillAccountLevel(t *testing.T) {
	const model = "deepseek-v4.1-flash"
	p := testPoolWith(&auth.Auth{UID: "cn1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{
		Pool:     p,
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 429, `{"code":14003,"msg":"too many requests"}`, false }),
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429", rec.Code)
	}

	if got := p.AvailableUIDsForRealm("cn"); len(got) != 0 {
		t.Fatalf("普通 429 仍应是账号级冷却：AvailableUIDsForRealm=%v", got)
	}
}

// TestUpstreamTimeoutGivesModelLevelCooldown 上游超时：仍不换号（同慢上游换号无益），
// 但要给 (账号, 模型) 一段短避让，否则卡住的号会被反复选中（实测连吃 4 次 120s）。
// 超时错误复用 upstream_timeout_test.go 的 timeoutErr（net.Error + Timeout()=true）。
func TestUpstreamTimeoutGivesModelLevelCooldown(t *testing.T) {
	const model = "deepseek-v4.1-flash"
	p := testPoolWith(&auth.Auth{UID: "cn1", AccessToken: "at", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, timeoutErr{true}
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503（超时），body=%s", rec.Code, rec.Body)
	}

	// 账号级不冷却：超时不是账号故障证据，不连坐其他模型。
	if got := p.AvailableUIDsForRealm("cn"); len(got) != 1 {
		t.Fatalf("超时不该做账号级冷却：AvailableUIDsForRealm=%v", got)
	}
	// 模型级避让存在，且是 60s 级。
	st := p.ModelBlockedForRealm(model, "cn")
	if !st.Blocked {
		t.Fatalf("超时后该 (账号,模型) 应有短避让（否则会被反复选中）：%+v", st)
	}
	if d := time.Until(st.Until); d < 30*time.Second || d > 90*time.Second {
		t.Fatalf("避让时长 %v，want ≈60s", d)
	}
}
