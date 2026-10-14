package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestCacheKeyStableForSessionWithoutID 无任何会话标识的客户端（New API 等聚合层）
// 也必须拿到**稳定**的 prompt_cache_key：同会话（首条 user 消息不变、历史递增）的
// 连续请求出站 key 必须相同，不同会话必须不同。
//
// 为什么：上游 prefix cache 按 prompt_cache_key 分桶。此前这类客户端 convID 恒空
// ⇒ buildCacheKey 退化成「每账号一个常量」⇒ 同账号下所有会话挤一个缓存槽、互相
// 驱逐（实测该账号首次大请求命中率仅 7.9~10.4%，换账号后缓存全丢）。
func TestCacheKeyStableForSessionWithoutID(t *testing.T) {
	var keys []string
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(r.Body)
			var obj map[string]any
			_ = json.Unmarshal(b, &obj)
			k, _ := obj["prompt_cache_key"].(string)
			keys = append(keys, k)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(tinySSE)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	store := keysStore(t, "noid", "sk-noid-x", nil)

	post := func(body string) {
		h := NewHandler(Config{
			Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
			Upstream: up,
			Keys:     store,
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk-noid-x")
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	}

	// 会话 A：两次，首条 user 消息相同、历史递增（不带任何会话标识）
	post(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"帮我写个函数"}],"stream":true}`)
	post(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"帮我写个函数"},{"role":"assistant","content":"好"},{"role":"user","content":"改成 Python"}],"stream":true}`)
	// 会话 B：不同首条消息
	post(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"另一个话题"}],"stream":true}`)

	if len(keys) != 3 {
		t.Fatalf("出站次数=%d want 3", len(keys))
	}
	for i, k := range keys {
		if k == "" {
			t.Fatalf("第 %d 次出站未注入 prompt_cache_key", i+1)
		}
	}
	if keys[0] != keys[1] {
		t.Errorf("同会话两次请求 cache key 应相同：%q vs %q", keys[0], keys[1])
	}
	if keys[0] == keys[2] {
		t.Errorf("不同会话 cache key 应不同，实际都是 %q", keys[0])
	}
}

// TestCacheKeyFromClientSessionHeader 客户端把会话 ID 放 **header**、body 里没有
// 任何会话标识时（Claude Code / Codex 经 New API 等中转层转发，body 标识被剥掉），
// 网关必须仍然拿到**会话级稳定**的 prompt_cache_key。
//
// 同时钉住两条纪律（handler 侧端到端，非仅 session 包单测）：
//   - 轮级头 X-Conversation-Request-ID **绝不能被当作会话标识**（每轮必换 → 每轮
//     一个新 cache key → 前缀缓存 100% miss）；
//   - 会话级头优先于「首条 user 消息哈希」启发式兜底。
func TestCacheKeyFromClientSessionHeader(t *testing.T) {
	var keys []string
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(r.Body)
			var obj map[string]any
			_ = json.Unmarshal(b, &obj)
			k, _ := obj["prompt_cache_key"].(string)
			keys = append(keys, k)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(tinySSE)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	store := keysStore(t, "hdr", "sk-hdr-x", nil)

	post := func(body string, hdr map[string]string) {
		t.Helper()
		h := NewHandler(Config{
			Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
			Upstream: up,
			Keys:     store,
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk-hdr-x")
		req.Header.Set("Content-Type", "application/json")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	}

	const turn1 = `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"重构这个函数"}],"stream":true}`
	const turn2 = `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"重构这个函数"},{"role":"assistant","content":"好"},{"role":"user","content":"再加单测"}],"stream":true}`

	// Claude Code：X-Claude-Code-Session-Id（body 无标识）
	post(turn1, map[string]string{"X-Claude-Code-Session-Id": "cc-sess-1"})
	post(turn2, map[string]string{"X-Claude-Code-Session-Id": "cc-sess-1"})
	// 另一会话 → 必须换 key
	post(turn1, map[string]string{"X-Claude-Code-Session-Id": "cc-sess-2"})
	// Codex：session_id（Go 规范化为 Session_id）
	post(turn1, map[string]string{"session_id": "codex-sess-1"})

	if len(keys) != 4 {
		t.Fatalf("出站次数=%d want 4", len(keys))
	}
	for i, k := range keys {
		if k == "" {
			t.Fatalf("第 %d 次出站未注入 prompt_cache_key", i+1)
		}
	}
	if keys[0] != keys[1] {
		t.Errorf("同 header 会话跨轮 cache key 应相同：%q vs %q", keys[0], keys[1])
	}
	if keys[0] == keys[2] {
		t.Errorf("不同 header 会话 cache key 应不同，实际都是 %q", keys[0])
	}
	if keys[3] == keys[0] {
		t.Errorf("Codex session_id 会话应与 Claude Code 会话不同 key，实际都是 %q", keys[3])
	}

	// 轮级头绝不能被采纳：只给 X-Conversation-Request-ID 时，应回落到
	// StickyFallbackKey（首条 user 消息哈希）——即与「完全不带任何标识」的
	// 请求得到同一个出站 key，而不是那个每轮都变的轮级值。
	keys = keys[:0]
	post(turn1, map[string]string{"X-Conversation-Request-ID": "turn-a"})
	post(turn2, map[string]string{"X-Conversation-Request-ID": "turn-b"})
	post(turn1, nil) // 基准：无任何会话标识
	post(turn2, nil)
	if len(keys) != 4 {
		t.Fatalf("出站次数=%d want 4", len(keys))
	}
	if keys[0] != keys[2] || keys[1] != keys[3] {
		t.Errorf("轮级头改变了 cache key —— 说明它被当成会话标识（每轮新 key → 前缀缓存全 miss）: %q %q / %q %q",
			keys[0], keys[2], keys[1], keys[3])
	}
	if keys[0] != keys[1] {
		t.Errorf("同会话跨轮（首条 user 消息不变）启发式键应稳定：%q vs %q", keys[0], keys[1])
	}
}
