package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestStreamResponseStripsUsageCredit 流式响应末帧不得带 usage.credit（积分只留网关）。
func TestStreamResponseStripsUsageCredit(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	cl, _ := recordingUpstream(t, func(m string) (int, string, bool) { return 200, sseWithCredit, true })
	h := NewHandler(Config{Pool: p, MaxRotate: 3, Upstream: cl})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, `"credit"`) {
		t.Fatalf("流式响应泄漏了积分字段:\n%s", body)
	}
	// token 口径必须保留（客户端要靠它算上下文用量）。
	if !strings.Contains(body, `"prompt_tokens"`) {
		t.Fatalf("usage 的 token 字段被误删:\n%s", body)
	}
}

// TestNonStreamResponseStripsUsageCredit 非流式聚合响应同理。
func TestNonStreamResponseStripsUsageCredit(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	cl, _ := recordingUpstream(t, func(m string) (int, string, bool) { return 200, sseWithCredit, true })
	h := NewHandler(Config{Pool: p, MaxRotate: 3, Upstream: cl})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v body=%s", err, rec.Body.String())
	}
	u, _ := resp["usage"].(map[string]any)
	if u == nil {
		t.Fatalf("响应缺 usage: %s", rec.Body.String())
	}
	if _, has := u["credit"]; has {
		t.Fatalf("非流式响应泄漏了 usage.credit: %v", u)
	}
	if _, has := u["prompt_tokens"]; !has {
		t.Fatalf("usage.prompt_tokens 被误删: %v", u)
	}
}

// TestNonStreamCreditStillRecordedForGateway 剥离只影响响应：网关自己的成本账本
// 仍然要拿到 credit（否则计费/成本档位全部失明）。
func TestNonStreamCreditStillRecordedForGateway(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	cl, _ := recordingUpstream(t, func(m string) (int, string, bool) { return 200, sseWithCredit, true })
	h := NewHandler(Config{Pool: p, MaxRotate: 3, Upstream: cl})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	// sseWithCredit 是 2.0 credit / 2000 token → 成本账本应留下该观测。
	per1k, ok := p.ModelCost("g1", "deepseek-v4.1-flash")
	if !ok || per1k <= 0 {
		t.Fatalf("网关侧未记录成本观测（剥离不应影响记账）: per1k=%v ok=%v", per1k, ok)
	}
}
