package upstream

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// jwtWith 造 payload 段为 payload 的 JWT（签名段随意）。
func jwtWith(payload string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
}

func decodeUserinfo(t *testing.T, h http.Header) map[string]any {
	t.Helper()
	raw := h.Get("X-Userinfo")
	if raw == "" {
		t.Fatal("X-Userinfo 未注入")
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("X-Userinfo 不是标准 base64: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("X-Userinfo 不是 JSON: %v (%s)", err, string(b))
	}
	return m
}

// 带 token_source 的账号：三头族齐全，X-Userinfo 只含实际存在的段。
func TestIdentityHeadersWithTokenSource(t *testing.T) {
	c := &Client{}
	req, _ := http.NewRequest("POST", "https://example.invalid/x", nil)
	a := &auth.Auth{UID: "u1", AccessToken: jwtWith(`{"token_source":"sso"}`)}
	c.injectIdentityHeaders(req, a)

	if got := req.Header.Get("X-Auth-Method"); got != "sso" {
		t.Errorf("X-Auth-Method=%q want sso", got)
	}
	m := decodeUserinfo(t, req.Header)
	if m["uin"] != "u1" || m["token_source"] != "sso" {
		t.Errorf("X-Userinfo=%v want uin=u1 token_source=sso", m)
	}
	if _, has := m["owner_uin"]; has {
		t.Errorf("个人号不应有 owner_uin: %v", m)
	}
}

// 企业号：额外带 owner_uin（= enterpriseId）。
func TestIdentityHeadersEnterpriseOwner(t *testing.T) {
	c := &Client{}
	req, _ := http.NewRequest("POST", "https://example.invalid/x", nil)
	a := &auth.Auth{UID: "u2", EnterpriseID: "ent-9", AccessToken: jwtWith(`{"token_source":"enterprise"}`)}
	c.injectIdentityHeaders(req, a)

	m := decodeUserinfo(t, req.Header)
	if m["owner_uin"] != "ent-9" {
		t.Errorf("owner_uin=%v want ent-9", m["owner_uin"])
	}
	if m["uin"] != "u2" || m["token_source"] != "enterprise" {
		t.Errorf("X-Userinfo=%v", m)
	}
}

// 个人号（JWT 无 token_source）：整族不发——补空值比不发更像伪造。
func TestIdentityHeadersPersonalNoTokenSource(t *testing.T) {
	c := &Client{}
	req, _ := http.NewRequest("POST", "https://example.invalid/x", nil)
	c.injectIdentityHeaders(req, &auth.Auth{UID: "u3", AccessToken: "opaque-token"})

	for _, h := range []string{"X-Auth-Method", "X-Userinfo"} {
		if got := req.Header.Get(h); got != "" {
			t.Errorf("%s=%q want 空（个人号不注入）", h, got)
		}
	}
}

func TestIdentityHeadersBoundaries(t *testing.T) {
	c := &Client{}
	// nil auth / 空 UID：不 panic 也不注入。
	for _, a := range []*auth.Auth{nil, {AccessToken: jwtWith(`{"token_source":"sso"}`)}} {
		req, _ := http.NewRequest("POST", "https://example.invalid/x", nil)
		c.injectIdentityHeaders(req, a)
		if req.Header.Get("X-Userinfo") != "" || req.Header.Get("X-Auth-Method") != "" {
			t.Errorf("a=%v 不应注入身份头", a)
		}
	}
}

// 计费 UA 按 realm 分档：global 走 axios（真机 5.8.4 国际版），CN 走 WorkBuddy/<ver>。
func TestBillingUAPerRealm(t *testing.T) {
	auth.SetGlobalEnabled(true)
	c := &Client{}
	if got := c.billingUA(&auth.Auth{UID: "g", Domain: "www.workbuddy.ai"}); got != "axios/1.16.1" {
		t.Errorf("global billingUA=%q want axios/1.16.1", got)
	}
	if got := c.billingUA(&auth.Auth{UID: "c", Domain: "www.codebuddy.cn"}); got != "WorkBuddy/"+defaultClientVersionCN {
		t.Errorf("cn billingUA=%q", got)
	}
	// 显式 SaaS 归因仍保持旧行为（空 UA）。
	if got := (&Client{ClientName: "SaaS"}).billingUA(nil); got != "" {
		t.Errorf("SaaS billingUA=%q want 空", got)
	}
}

// ChatHeaders 恒带 X-Private-Data: true（官方 chat 出站无条件声明）。
func TestChatHeadersPrivateData(t *testing.T) {
	auth.SetGlobalEnabled(true)
	c := &Client{}
	req, _ := http.NewRequest("POST", "https://example.invalid/x", nil)
	c.ChatHeaders(req, &auth.Auth{UID: "u1", AccessToken: jwtWith(`{"token_source":"sso"}`)}, "", ChatMeta{})
	if got := req.Header.Get("X-Private-Data"); got != "true" {
		t.Errorf("X-Private-Data=%q want true", got)
	}
	if req.Header.Get("X-Auth-Method") != "sso" {
		t.Errorf("ChatHeaders 应带上身份头族")
	}
}
