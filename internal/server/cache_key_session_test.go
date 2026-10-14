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
