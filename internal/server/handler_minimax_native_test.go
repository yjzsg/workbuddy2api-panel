package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestMiniMaxNativePathEquivalence 锁「MiniMax 原生路径」与主路径的行为等价。
//
// 背景（2026-10-21 实测）：New API 的「MiniMax」渠道类型不走 OpenAI 路径，而是把上游
// URL 拼成 <base>/v1/text/chatcompletion_v2；请求体原样透传、响应用 openai.Adaptor 解析，
// 即除 URL 外全是 OpenAI 语义。网关不认这条路径时，客户端在 New API 里选 MiniMax
// 类型/分组会直接拿到 404 page not found，选 OpenAI/DeepSeek 类型却正常。
//
// 本测试同时覆盖主路径，防止将来有人改路由时把两条路径的语义改岔。
func TestMiniMaxNativePathEquivalence(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/text/chatcompletion_v2"} {
		path := path
		t.Run(path, func(t *testing.T) {
			up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
			h := NewHandler(Config{
				Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
				Upstream: up,
			})
			req := httptest.NewRequest("POST", path,
				strings.NewReader(`{"model":"minimax-m3.1-flash","messages":[{"role":"user","content":"hi"}]}`))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}
			var resp map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("resp not json: %v body=%s", err, rec.Body)
			}
			if resp["object"] != "chat.completion" {
				t.Errorf("object=%v", resp["object"])
			}
			msg, _ := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
			if msg["content"] != "你好" {
				t.Errorf("content=%q", msg["content"])
			}
		})
	}
}

// TestMiniMaxNativePathStream 流式也要等价：MiniMax 渠道下客户端常开 stream。
func TestMiniMaxNativePathStream(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/text/chatcompletion_v2",
		strings.NewReader(`{"model":"minimax-m3.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("ct=%q", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "你好") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body=%q", body)
	}
}

// TestMiniMaxNativePathRequiresAuth 新路径必须同样受 api_key 保护——不能因为它是
// 「兼容别名」就绕过鉴权。
func TestMiniMaxNativePathRequiresAuth(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		APIKey:   "secret",
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
	})
	req := httptest.NewRequest("POST", "/v1/text/chatcompletion_v2", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("no key: code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestIsChatCompletionPath 请求日志钩子的路径判定。
// 若漏掉 MiniMax 原生路径，从 New API 该渠道进来的流量在 request-logs 里完全不可见，
// 排障时会被误判成「上游根本没打过来」。
func TestIsChatCompletionPath(t *testing.T) {
	yes := []string{"/v1/chat/completions", "/v1/text/chatcompletion_v2", "/api/paas/v4/chat/completions"}
	for _, p := range yes {
		if !isChatCompletionPath(p) {
			t.Errorf("%q 应被识别为对话补全路径", p)
		}
	}
	no := []string{"/v1/models", "/v1/chat/completions/x", "/v1/text/chatcompletion_v2/", "/status", "",
		"/api/paas/v4/models", "/api/paas/v4/chat/completions/"}
	for _, p := range no {
		if isChatCompletionPath(p) {
			t.Errorf("%q 不应被识别为对话补全路径", p)
		}
	}
}
