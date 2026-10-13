package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikeys"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const tinySSE = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null,\"index\":0}]}\n\n" +
	"data: [DONE]\n\n"

// keysStore 建一把测试密钥（allowed 为空 = 不限制模型）。
func keysStore(t *testing.T, name, secret string, allowed []string) *apikeys.Store {
	t.Helper()
	s := apikeys.NewStore("")
	if _, err := s.Add(name, secret, "", allowed, nil, "", true); err != nil {
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

// TestKeyModelAliasRewritesRequest 密钥别名把客户端发的短名字翻译成真实模型名。
//
// 背景：**裸名在网关上默认解析成 CN 域**（见 resolveModel），而客户端只会发它习惯的
// 名字 —— 用户控制不了。所以要让某个密钥走国际版，只能在密钥这一层做翻译。
// 本用例钉住三件事（都是"看不见就白做"的）：
//  1. 白名单是**带域精确**的：没配别名时，客户端发裸名会被 403（裸名 → cn 域，
//     与 `global:` 条目匹配不上）——这正是别名要解决的痛点；
//  2. 配了别名后放行，**出站 model 用真实名剥域后的裸名**，且**账号用的是 global 池**那个
//     （路由真的变了，不只是名字变了）；
//  3. 别名只作用于配了它的那把密钥。
func TestKeyModelAliasRewritesRequest(t *testing.T) {
	type hit struct{ model, authz string }
	var mu sync.Mutex
	var hits []hit
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(r.Body)
			var obj map[string]any
			_ = json.Unmarshal(b, &obj)
			m, _ := obj["model"].(string)
			mu.Lock()
			hits = append(hits, hit{model: m, authz: r.Header.Get("Authorization")})
			mu.Unlock()
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(tinySSE)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	cn := &auth.Auth{UID: "u-cn", AccessToken: "tok-cn", ExpiresAt: 9999999999}
	gl := &auth.Auth{UID: "u-gl", AccessToken: "tok-global", ExpiresAt: 9999999999, Domain: "api.workbuddy.ai"}
	if gl.Realm() != "global" {
		t.Fatalf("测试前置：期望 global 账号，实际 realm=%s", gl.Realm())
	}

	withAlias := apikeys.NewStore("")
	if _, err := withAlias.Add("别名", "sk-alias", "", []string{"global:deepseek-v4.1-flash"},
		map[string]string{"deepseek-v4.1-flash": "global:deepseek-v4.1-flash"}, "", true); err != nil {
		t.Fatal(err)
	}
	noAlias := apikeys.NewStore("")
	if _, err := noAlias.Add("无别名", "sk-plain", "", []string{"global:deepseek-v4.1-flash"}, nil, "", true); err != nil {
		t.Fatal(err)
	}

	post := func(h *Handler, key, model string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
			`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}],"stream":true}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rec, req)
		return rec
	}

	// 1) 配了别名：裸名 → global:... ⇒ 放行，出站裸名，走 global 账号
	h := NewHandler(Config{Pool: testPoolWith(cn, gl), Upstream: up, Keys: withAlias})
	rec := post(h, "sk-alias", "deepseek-v4.1-flash")
	if rec.Code != http.StatusOK {
		t.Fatalf("配了别名应放行，code=%d body=%s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	if len(hits) != 1 {
		mu.Unlock()
		t.Fatalf("上游调用次数=%d want 1", len(hits))
	}
	got := hits[0]
	mu.Unlock()
	if got.model != "deepseek-v4.1-flash" {
		t.Errorf("出站 model=%q want deepseek-v4.1-flash（真实名剥域后的裸名；上游只认裸名）", got.model)
	}
	if !strings.Contains(got.authz, "tok-global") {
		t.Errorf("别名指向 global 域，应走 global 账号，实际 Authorization=%q", got.authz)
	}

	// 2) 没配别名：同一个裸名 → cn 域，与 `global:` 白名单条目匹配不上 ⇒ 403 且零上游调用
	h2 := NewHandler(Config{Pool: testPoolWith(cn, gl), Upstream: up, Keys: noAlias})
	rec2 := post(h2, "sk-plain", "deepseek-v4.1-flash")
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("带域精确白名单 + 裸名请求应 403（这就是别名要解决的痛点），实际 %d body=%s",
			rec2.Code, rec2.Body.String())
	}
	mu.Lock()
	n := len(hits)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("被白名单拒的请求不应打上游：调用次数=%d want 1", n)
	}

	// 3) 无别名的密钥发**带前缀**的真实名 ⇒ 应放行（白名单命中的是它）
	rec3 := post(h2, "sk-plain", "global:deepseek-v4.1-flash")
	if rec3.Code != http.StatusOK {
		t.Fatalf("带前缀的真实名应放行，code=%d body=%s", rec3.Code, rec3.Body.String())
	}
	mu.Lock()
	got3 := hits[len(hits)-1]
	mu.Unlock()
	if got3.model != "deepseek-v4.1-flash" || !strings.Contains(got3.authz, "tok-global") {
		t.Errorf("带前缀请求应同样出站裸名并走 global：model=%q authz=%q", got3.model, got3.authz)
	}
}

// TestModelsListsKeyAliases /v1/models 要把该密钥的别名列出来（客户端才知道能用什么名字），
// 但**只列别名目标本身在白名单内**的 —— 否则会广告一个必然 403 的名字。
func TestModelsListsKeyAliases(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, tinySSE, true })
	s := apikeys.NewStore("")
	if _, err := s.Add("别名", "sk-alias", "",
		[]string{"global:gpt-6-luna"},
		map[string]string{
			"luna":       "global:gpt-6-luna",  // 目标在白名单内 ⇒ 该列
			"notallowed": "global:gpt-6-astra", // 目标不在白名单 ⇒ 不该列
		}, "", true); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: up, Keys: s})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-alias")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	ids := map[string]map[string]any{}
	for _, m := range out.Data {
		if id, _ := m["id"].(string); id != "" {
			ids[id] = m
		}
	}
	if _, ok := ids["luna"]; !ok {
		t.Errorf("/v1/models 应列出别名 luna，实际 id=%v", keysOf(ids))
	}
	if got, ok := ids["luna"]; ok {
		if got["alias_of"] != "global:gpt-6-luna" {
			t.Errorf("别名条目应带 alias_of，实际 %v", got["alias_of"])
		}
	}
	if _, ok := ids["notallowed"]; ok {
		t.Error("别名目标不在白名单内时不该列出（否则广告一个必然 403 的名字）")
	}
}

func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestKeyThinkingOverride 密钥级思考开关：客户端请求了思考，密钥可以强制关掉。
//
// 场景（2026-10-13 用户报）：harness 那一路请求里只有 reasoning_effort、没有任何
// "关思考"的字段，网关按官方客户端口径默认注入 enabled ⇒ 它拿到 reasoning_content
// 后当正文渲染。网关不能替所有客户端决定，但可以按密钥覆盖 —— 不动全局默认。
//
// 断言：出站请求体的 thinking.type 被改写；未设开关的密钥不受影响；非 deepseek 模型不动。
func TestKeyThinkingOverride(t *testing.T) {
	captured := make(chan map[string]any, 8)
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(r.Body)
			var obj map[string]any
			_ = json.Unmarshal(b, &obj)
			captured <- obj
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(tinySSE)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	mk := func(thinking string) *apikeys.Store {
		s := apikeys.NewStore("")
		if _, err := s.Add("k", "sk-"+thinking+"-x", "", nil, nil, thinking, true); err != nil {
			t.Fatal(err)
		}
		return s
	}

	post := func(store *apikeys.Store, key, model string) {
		h := NewHandler(Config{
			Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
			Upstream: up,
			Keys:     store,
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
			`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}],"stream":true,`+
				`"reasoning_effort":"high"}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	}

	thinkingType := func(obj map[string]any) string {
		th, _ := obj["thinking"].(map[string]any)
		s, _ := th["type"].(string)
		return s
	}

	// 1) 强制关：客户端带 reasoning_effort 也照样关（thinking.type=disabled 且删掉 effort 档）
	post(mk("off"), "sk-off-x", "deepseek-v4.1-flash")
	obj := <-captured
	if got := thinkingType(obj); got != "disabled" {
		t.Errorf("强制关：thinking.type=%q want disabled（out=%v）", got, obj["thinking"])
	}
	if _, has := obj["reasoning_effort"]; has {
		t.Errorf("强制关：reasoning_effort 应被删掉（disabled 分支），实际 %v", obj["reasoning_effort"])
	}

	// 2) 强制开：客户端什么都没带也开
	post(mk("on"), "sk-on-x", "deepseek-v4.1-flash")
	obj = <-captured
	if got := thinkingType(obj); got != "enabled" {
		t.Errorf("强制开：thinking.type=%q want enabled", got)
	}
	if _, has := obj["reasoning_effort"]; !has {
		t.Error("强制开：应补上默认 effort 档")
	}

	// 3) 跟随客户端（空）：保持既有默认注入（deepseek 无 thinking → 注入 enabled）
	post(mk(""), "sk--x", "deepseek-v4.1-flash")
	obj = <-captured
	if got := thinkingType(obj); got != "enabled" {
		t.Errorf("跟随客户端：应保持既有默认注入，thinking.type=%q", got)
	}

	// 4) 非 deepseek 模型：密钥开关不生效（thinking 字段语义不同，不能乱塞）
	post(mk("off"), "sk-off-x", "glm-5.2")
	obj = <-captured
	if _, has := obj["thinking"]; has {
		t.Errorf("非 deepseek 模型不应被塞 thinking 字段，实际 %v", obj["thinking"])
	}
}
