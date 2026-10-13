package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikeys"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const tinySSE = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null,\"index\":0}]}\n\n" +
	"data: [DONE]\n\n"

// keysStore 建一把测试密钥（allowed 为空 = 不限制模型）。
func keysStore(t *testing.T, name, secret string, allowed []string) *apikeys.Store {
	t.Helper()
	s := apikeys.NewStore("")
	if _, err := s.Add(name, secret, "", allowed, true); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestKeyAuthMultiKey 多密钥表存在时：未带/错带密钥 401，带对密钥放行。
func TestKeyAuthMultiKey(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, tinySSE, true })
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: up, Keys: keysStore(t, "a", "good-key", nil)})

	cases := []struct {
		authz string
		want  int
	}{
		{"", http.StatusUnauthorized},
		{"Bearer wrong", http.StatusUnauthorized},
		{"good-key", http.StatusUnauthorized}, // 缺 Bearer 前缀
		{"Bearer good-key", http.StatusOK},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/models", nil)
		if c.authz != "" {
			req.Header.Set("Authorization", c.authz)
		}
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("Authorization=%q code=%d want %d", c.authz, rec.Code, c.want)
		}
	}
}

// TestKeyAuthNoKeysMeansOpen 表为空时保持"不鉴权"旧语义（裸用/未迁移场景）。
func TestKeyAuthNoKeysMeansOpen(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, tinySSE, true })
	// Keys 为 nil（未启用多密钥）⇒ 旧单密钥语义：APIKey 为空即放行。
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("未启用多密钥且 api_key 为空时应放行，实际 %d", rec.Code)
	}
}

// TestModelWhitelistBlocksAndSkipsUpstream 白名单外的模型必须 403，
// 且**不能**产生任何上游调用（否则白名单就成了摆设）。
func TestModelWhitelistBlocksAndSkipsUpstream(t *testing.T) {
	called := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		called++
		return 200, tinySSE, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up,
		Keys:     keysStore(t, "limit", "sk-limit", []string{"glm-5.2"}),
	})

	post := func(model string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer sk-limit")
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := post("kimi-k3-1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("白名单外 code=%d want 403，body=%s", rec.Code, rec.Body)
	}
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("403 响应不是 JSON：%v body=%s", err, rec.Body)
	}
	if e.Error.Code != "model_not_allowed" {
		t.Errorf("code=%q want model_not_allowed", e.Error.Code)
	}
	if called != 0 {
		t.Errorf("白名单拒绝的请求不应触达上游，实际调用 %d 次", called)
	}
}

// TestModelWhitelistBareNameMatchesAnyRealm 白名单里的裸名对任意域都放行；
// 带域条目只匹配该域。
func TestModelWhitelistBareNameMatchesAnyRealm(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, tinySSE, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up,
		Keys:     keysStore(t, "limit", "sk", []string{"glm-5.2", "global:gpt-6-luna"}),
	})
	post := func(model string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer sk")
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	// 裸名条目 ⇒ 任意域放行（cn 与 global 都行）
	if got := post("glm-5.2"); got == http.StatusForbidden {
		t.Errorf("glm-5.2（裸名条目）应放行，实际 403")
	}
	if got := post("cn:glm-5.2"); got == http.StatusForbidden {
		t.Errorf("cn:glm-5.2（裸名条目匹配任意域）应放行，实际 403")
	}
	if got := post("global:glm-5.2"); got == http.StatusForbidden {
		t.Errorf("global:glm-5.2（裸名条目匹配任意域）应放行，实际 403")
	}
	// 带域条目 ⇒ 只匹配该域
	if got := post("global:gpt-6-luna"); got == http.StatusForbidden {
		t.Errorf("global:gpt-6-luna 应放行，实际 403")
	}
	if got := post("cn:gpt-6-luna"); got != http.StatusForbidden {
		t.Errorf("cn:gpt-6-luna（带域条目只匹配 global）应 403，实际 %d", got)
	}
}

// TestModelsListFilteredByWhitelist /v1/models 只列出该密钥允许的模型。
func TestModelsListFilteredByWhitelist(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 404, "{}", false })
	h := NewHandler(Config{
		Pool:     testPoolWith(),
		Upstream: up,
		Keys:     keysStore(t, "limit", "sk", []string{"model-that-does-not-exist"}),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	var body struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, m := range body.Data {
		id, _ := m["id"].(string)
		if id != "model-that-does-not-exist" && id != "cn:model-that-does-not-exist" {
			t.Errorf("白名单外的模型不应出现在 /v1/models：%q", id)
		}
	}

	// 不限制时列表不应为空（对照：同一 handler 换成空白名单）。
	h2 := NewHandler(Config{Pool: testPoolWith(), Upstream: up, Keys: keysStore(t, "all", "sk2", nil)})
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/v1/models", nil)
	req2.Header.Set("Authorization", "Bearer sk2")
	h2.ServeHTTP(rec2, req2)
	var body2 struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.Unmarshal(rec2.Body.Bytes(), &body2)
	if len(body2.Data) == 0 {
		t.Error("不限制模型时 /v1/models 不应为空")
	}
}
