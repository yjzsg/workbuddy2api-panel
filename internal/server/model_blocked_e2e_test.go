// model_blocked_e2e_test.go 端到端：本域全部账号对请求模型处于**模型级冷却**时，
// 网关必须在选号层就返回 model_unavailable（400，「换模型」），而不是
//
//	(a) 兜底挑一个已知冷却的号去撞上游（实测 CN glm-5.3 全体 6004 时连撞 12 次、每次 ~2.3s），
//	(b) 或退回 503 no_healthy_account（"服务端过载"形状，客户端会不断重试）。
//
// 与 internal/pool 的 TestFallbackSkipsModelCooledAccount /
// TestModelBlockedForRealmRealmScoped 构成三层：选号层（pool）、判定层（ModelBlockedForRealm）、
// 出口层（本文件，HTTP 状态码 + code + hint）。
package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// countingUpstream 统计出站次数，用于证明「选号层就拦住了，没打上游」。
func countingUpstream(called *int) *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			*called++
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(tinySSE)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// TestModelCooledAllRealmYieldsModelUnavailable CN 唯一号被 glm-5.3 的 6004 冷却挡住：
// 请求必须在选号层被拒（0 次出站），返回 400 + model_unavailable + model_blocked hint。
func TestModelCooledAllRealmYieldsModelUnavailable(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "cn1", AccessToken: "at", ExpiresAt: 9999999999})
	// 6004 形态：只写模型级冷却，不写账号级 until（这正是本次要修的形态）。
	p.CooldownSoftForModel("cn1", time.Hour, time.Now().Add(2*time.Hour), "glm-5.3", "6004 model rate limit")

	called := 0
	h := NewHandler(Config{Pool: p, Upstream: countingUpstream(&called)})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`))
	h.ServeHTTP(rec, req)

	if called != 0 {
		t.Errorf("本域全被模型级冷却时不该打上游，实际打了 %d 次（旧行为：兜底挑冷却号去撞）", called)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400（不可重试：换模型，而非重试），body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "model_unavailable") {
		t.Errorf("body=%s want code=model_unavailable", body)
	}
	if !strings.Contains(body, "model_blocked") {
		t.Errorf("hint 应带出 model_blocked 与最早解封时刻：%s", body)
	}
	if strings.Contains(body, "no_healthy_account") {
		t.Errorf("不得退回 no_healthy_account（会让客户端按可重试处理）：%s", body)
	}
}

// TestModelCooledRealmScopedOnlyBlocksItsRealm 域隔离：CN 号被 glm-5.3 冷却，但 global 号
// 还能服务同一模型名时，**cn 域**请求仍应判为「模型不可用」——跨域账号对带前缀的请求不可达，
// 不构成「还能服务」的证明。这条同时钉住了 ModelBlocked → ModelBlockedForRealm 的域过滤。
func TestModelCooledRealmScopedOnlyBlocksItsRealm(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := testPoolWith(
		&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn", AccessToken: "at", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at", ExpiresAt: 9999999999},
	)
	p.CooldownSoftForModel("cn1", time.Hour, time.Now().Add(2*time.Hour), "glm-5.3", "6004 model rate limit")

	called := 0
	h := NewHandler(Config{Pool: p, Upstream: countingUpstream(&called)})

	// cn 域：只有 cn1 可路由，且它被挡 → 400 model_unavailable。
	//
	// 【2026-10-15】必须用显式 `cn:` 前缀：裸名现在是 realm=""（地域无关，全池参与
	// 粘合），此时 global 的 g1 仍能服务该模型 → 放行 200 才是正确行为。
	// 本用例测的是**带前缀请求的域隔离**，故显式写 cn:。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:glm-5.3","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "model_unavailable") {
		t.Fatalf("cn 域应报 model_unavailable：code=%d body=%s", rec.Code, rec.Body)
	}
	if called != 0 {
		t.Errorf("cn 域全被挡时不该打上游，实际 %d 次", called)
	}

	// global 域：g1 能服务该模型 → 正常放行（不能被 cn 的冷却连坐）。
	called = 0
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:glm-5.3","messages":[{"role":"user","content":"hi"}]}`)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("global 域不该受 cn 的模型级冷却影响：code=%d body=%s", rec2.Code, rec2.Body)
	}
	if called != 1 {
		t.Errorf("global 域应正常出站 1 次，实际 %d 次", called)
	}
}

// TestModelCooledOtherModelStillServed 模型级冷却只锁触发模型：同一账号打别的模型照常放行。
func TestModelCooledOtherModelStillServed(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "cn1", AccessToken: "at", ExpiresAt: 9999999999})
	p.CooldownSoftForModel("cn1", time.Hour, time.Now().Add(2*time.Hour), "glm-5.3", "6004 model rate limit")

	called := 0
	h := NewHandler(Config{Pool: p, Upstream: countingUpstream(&called)})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("其他模型不该被 glm-5.3 的冷却连坐：code=%d body=%s", rec.Code, rec.Body)
	}
	if called != 1 {
		t.Errorf("应正常出站 1 次，实际 %d 次", called)
	}
}
