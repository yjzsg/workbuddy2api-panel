package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestZhipuNativePathEquivalence 锁「智谱-4V 原生路径」与主路径的行为等价。
//
// 背景（2026-10-24 实测）：New API 的「智谱-4V」渠道类型（ChannelTypeZhipu_v4）不走
// OpenAI 路径，而是把上游 URL 拼成 <base>/api/paas/v4/chat/completions；请求体经
// requestOpenAI2Zhipu 后仍是 OpenAI 形状（messages/role/content 原样保留，只把
// max_completion_tokens 归一成 max_tokens、图片 data URL 去掉前缀），响应也交给
// openai.Adaptor.DoResponse 解析 —— 即除 URL 之外全是 OpenAI 语义。
//
// 网关不认这条路径时，客户端在 New API 里选「智谱」类型即 404 page not found，
// 「拉不到模型」也是同一个原因（模型列表固定打 <base>/api/paas/v4/models）。
func TestZhipuNativePathEquivalence(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/api/paas/v4/chat/completions"} {
		path := path
		t.Run(path, func(t *testing.T) {
			up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
			h := NewHandler(Config{
				Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
				Upstream: up,
			})
			req := httptest.NewRequest("POST", path,
				strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`))
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

// TestZhipuNativePathStream 智谱渠道下客户端也会开 stream，同样要等价。
func TestZhipuNativePathStream(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/api/paas/v4/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
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

// TestZhipuNativeModelsPath 「获取模型列表」走 <base>/api/paas/v4/models，
// 必须与 /v1/models 一样可用（否则 New API 侧选智谱类型就拉不到模型），并且受 api_key 保护。
func TestZhipuNativeModelsPath(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		APIKey:   "secret",
		Upstream: up,
	})

	// 无 key → 401
	req := httptest.NewRequest("GET", "/api/paas/v4/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("no key: code=%d body=%s", rec.Code, rec.Body)
	}

	// 带 key → 200 且带 data 数组
	req = httptest.NewRequest("GET", "/api/paas/v4/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if len(resp.Data) == 0 {
		t.Errorf("模型列表为空: %s", rec.Body)
	}
}
