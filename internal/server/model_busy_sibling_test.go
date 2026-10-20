package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
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

func TestCostModelName(t *testing.T) {
	if got := costModelName("deepseek-v4.1-flash", ""); got != "deepseek-v4.1-flash" {
		t.Fatalf("无兜底时应记请求模型, got %q", got)
	}
	if got := costModelName("deepseek-v4.1-flash", "deepseek-v4.1-flash-sg"); got != "deepseek-v4.1-flash-sg" {
		t.Fatalf("兜底时应记实际服务模型, got %q", got)
	}
}

// TestSiblingFallbackCostAttributedToServedModel：兜底成功时成本必须记在 -sg 名下，
// 不能记到请求的 flash 上——否则 pool 会认为 flash「限免结束」，把账号从 tier0 降级。
func TestSiblingFallbackCostAttributedToServedModel(t *testing.T) {
	const model = "deepseek-v4.1-flash"
	p := testPoolWith(&auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	cl, _ := recordingUpstream(t, func(m string) (int, string, bool) {
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
	if _, ok := p.ModelCost("g1", model); ok {
		t.Fatalf("不应把 -sg 的收费观测记到 %s 名下（会让 costTier 误判限免结束）", model)
	}
	per1k, ok := p.ModelCost("g1", "deepseek-v4.1-flash-sg")
	if !ok || per1k <= 0 {
		t.Fatalf("-sg 应留下收费观测, got (%v, %v)", per1k, ok)
	}
}

// TestCapacityCooledUsesSiblingDirectly 该号对 flash 正处于容量冷却（刚吃过 14003）时，
// 请求应**直接**用兄弟模型发出，不再白撞一次 flash。
//
// 回归背景（2026-10-20 用户观测）：面板上请求在 flash / -sg 之间来回跳——14003 记 60s
// 容量避让但粘性绑定保留，于是每轮都先撞一次必然失败的 flash，再回落 -sg。
func TestCapacityCooledUsesSiblingDirectly(t *testing.T) {
	const model = "deepseek-v4.1-flash"
	p := testPoolWith(&auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	// 模拟上一轮刚吃过 14003：该号该模型进入 60s 容量冷却。
	p.CooldownCapacityForModel("g1", time.Minute, time.Now().Add(time.Minute), model, "14003 model busy")

	// 粘性命中路径才会选中「容量冷却中」的号（严格选号会避开它），故显式装配会话路由。
	sess := session.New(session.Config{
		TTL:               time.Minute,
		Store:             newBindStore(),
		Available:         func() []string { return []string{"g1"} },
		AvailableForModel: func(string) []string { return []string{"g1"} },
	})
	sess.Bind("conv-1", "g1")

	cl, seen := recordingUpstream(t, func(m string) (int, string, bool) {
		return 200, sseSiblingModel, true
	})
	h := NewHandler(Config{Pool: p, MaxRotate: 3, Upstream: cl, Session: sess})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:`+model+`","stream":true,"metadata":{"conversation_id":"conv-1"},"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	got := seen()
	if len(got) != 1 || got[0] != "deepseek-v4.1-flash-sg" {
		t.Fatalf("冷却期应直接走兄弟模型（只 1 次调用），实际序列=%v", got)
	}
	if strings.Contains(rec.Body.String(), "-sg") {
		t.Fatalf("响应仍泄漏 -sg: %s", rec.Body.String())
	}
}

// TestCapacitySaturationRetriesSameAccount 两个模型都 14003（上游全局饱和）时，
// 应留在原号等待重试，而不是换号——换号在上游全局饱和时没有收益，却必定丢掉前缀缓存。
//
// 实测反例（2026-10-20）：一个 60k→100k 的会话 7 分钟跳了 8 个号，每次命中率 0–3.5%。
func TestCapacitySaturationRetriesSameAccount(t *testing.T) {
	oldBackoff, oldRetries := capacityRetryBackoff, maxCapacityRetries
	capacityRetryBackoff, maxCapacityRetries = time.Millisecond, 2
	t.Cleanup(func() { capacityRetryBackoff, maxCapacityRetries = oldBackoff, oldRetries })

	const model = "deepseek-v4.1-flash"
	p := testPoolWith(
		&auth.Auth{UID: "g1", AccessToken: "at1", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"},
		&auth.Auth{UID: "g2", AccessToken: "at2", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"},
	)
	// 粘性会话绑在 g1：同号重试必须留在 g1（非粘性请求本来就会换号，不是本用例的范围）。
	sess := session.New(session.Config{
		TTL:               time.Minute,
		Store:             newBindStore(),
		Available:         func() []string { return []string{"g1", "g2"} },
		AvailableForModel: func(string) []string { return []string{"g1", "g2"} },
	})
	sess.Bind("conv-1", "g1")

	// 上游恒定 14003（两个模型都饱和）。
	cl, seen := recordingUpstream(t, func(m string) (int, string, bool) {
		return 429, modelBusy429Body, false
	})
	h := NewHandler(Config{Pool: p, MaxRotate: 3, Upstream: cl, Session: sess})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:`+model+`","stream":true,"metadata":{"conversation_id":"conv-1"},"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code == http.StatusOK {
		t.Fatalf("全饱和时应失败，body=%s", rec.Body.String())
	}
	got := seen()
	// 前 4 次必须发生在同一个号上：flash → 兄弟 -sg → 2 次同号重试（预判直接走 -sg）。
	// 之后预算用尽才允许换号（第 5 次是另一个号上的 flash），这不在本用例断言范围内。
	if len(got) < 4 {
		t.Fatalf("应至少 4 次调用（flash + 兄弟 + 2 次同号重试），实际 %d 次: %v", len(got), got)
	}
	if got[0] != model {
		t.Fatalf("首次应发原模型，实际 %v", got)
	}
	for i := 1; i < 4; i++ {
		if got[i] != "deepseek-v4.1-flash-sg" {
			t.Fatalf("第 %d 次应走兄弟模型（同号重试），实际序列=%v", i+1, got)
		}
	}
}
