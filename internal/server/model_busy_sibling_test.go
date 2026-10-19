package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const modelBusy429Body = `{"code":14003,"msg":"too many requests","requestId":"r1","displayMsg":{"en":"Model busy. Please switch models or retry later","zh":"模型繁忙，请换模型或稍后重试"}}`

// recordingUpstream 记录每次出站请求体的 model 字段，并按模型返回预设结果。
func recordingUpstream(t *testing.T, respond func(model string) (status int, body string, stream bool)) (*upstream.Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	cl := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			var req struct {
				Model string `json:"model"`
			}
			_ = json.Unmarshal(raw, &req)
			mu.Lock()
			seen = append(seen, req.Model)
			mu.Unlock()
			status, body, stream := respond(req.Model)
			ct := "application/json"
			if stream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseGlobal: "https://fake.example",
	}
	return cl, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// TestModelBusyRetriesSiblingModelOnSameAccount：14003 时先在**同一账号**上换 -sg
// 兄弟模型重试，成功即返回 200，不再换号。
func TestModelBusyRetriesSiblingModelOnSameAccount(t *testing.T) {
	const model = "deepseek-v4.1-flash"
	p := testPoolWith(&auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	cl, seen := recordingUpstream(t, func(m string) (int, string, bool) {
		if m == "deepseek-v4.1-flash-sg" {
			return 200, sseWithCredit, true
		}
		return 429, modelBusy429Body, false
	})

	h := NewHandler(Config{Pool: p, MaxRotate: 3, Upstream: cl})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:`+model+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	got := seen()
	if len(got) != 2 || got[0] != model || got[1] != "deepseek-v4.1-flash-sg" {
		t.Fatalf("上游模型调用序列=%v want [%s deepseek-v4.1-flash-sg]", got, model)
	}
}

// TestModelBusySiblingAlsoBusyReturns429：兄弟模型也 14003 时，两个模型都记模型级
// 避让；单号池随后选不出号，末端按「模型繁忙」回 429（可重试），而不是 503。
func TestModelBusySiblingAlsoBusyReturns429(t *testing.T) {
	const model = "deepseek-v4.1-flash"
	p := testPoolWith(&auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	cl, seen := recordingUpstream(t, func(m string) (int, string, bool) {
		return 429, modelBusy429Body, false
	})

	h := NewHandler(Config{Pool: p, MaxRotate: 3, Upstream: cl})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:`+model+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429 body=%s", rec.Code, rec.Body.String())
	}
	got := seen()
	if len(got) != 2 || got[0] != model || got[1] != "deepseek-v4.1-flash-sg" {
		t.Fatalf("上游模型调用序列=%v want [%s deepseek-v4.1-flash-sg]", got, model)
	}
	st, _ := p.Status("g1")
	if len(st.RateLimitedModels) != 2 {
		t.Fatalf("两个模型都应留下模型级避让，实际 %+v", st.RateLimitedModels)
	}
}

const sseSiblingModel = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"deepseek-v4.1-flash-sg\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"deepseek-v4.1-flash-sg\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12,\"credit\":0.001}}\n\n" +
	"data: [DONE]\n\n"

// TestSiblingModelNameNotLeakedToStream：兄弟模型兜底成功时，SSE 帧里的 model
// 必须改回客户端请求的 flash，-sg 不得出现在响应里（New API 会原样回显）。
func TestSiblingModelNameNotLeakedToStream(t *testing.T) {
	const model = "deepseek-v4.1-flash"
	p := testPoolWith(&auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	cl, _ := recordingUpstream(t, func(m string) (int, string, bool) {
		if m == "deepseek-v4.1-flash-sg" {
			return 200, sseSiblingModel, true
		}
		return 429, modelBusy429Body, false
	})

	h := NewHandler(Config{Pool: p, MaxRotate: 3, Upstream: cl})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:`+model+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "-sg") {
		t.Fatalf("响应里泄漏了兄弟模型名: %s", body)
	}
	if !strings.Contains(body, `"model":"`+model+`"`) {
		t.Fatalf("响应 model 应为 %s: %s", model, body)
	}
}

// TestSiblingModelNameNotLeakedNonStream：非流式同理。
func TestSiblingModelNameNotLeakedNonStream(t *testing.T) {
	const model = "deepseek-v4.1-flash"
	p := testPoolWith(&auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	cl, _ := recordingUpstream(t, func(m string) (int, string, bool) {
		if m == "deepseek-v4.1-flash-sg" {
			// 非流式客户端也走「上游 SSE + 网关聚合」路径，故这里仍返回 SSE。
			return 200, sseSiblingModel, true
		}
		return 429, modelBusy429Body, false
	})

	h := NewHandler(Config{Pool: p, MaxRotate: 3, Upstream: cl})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:`+model+`","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "-sg") {
		t.Fatalf("响应里泄漏了兄弟模型名: %s", body)
	}
}
