package auth

import (
	"encoding/base64"
	"testing"
)

// makeJWT 造一个只含 payload 的 JWT（签名段留空）：TokenSource 只解 payload。
func makeJWT(payload string) string {
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." +
		enc.EncodeToString([]byte(payload)) + ".sig"
}

func TestTokenSourceFromJWT(t *testing.T) {
	a := &Auth{AccessToken: makeJWT(`{"token_source":"sso","sub":"u1"}`)}
	if got := a.TokenSource(); got != "sso" {
		t.Fatalf("TokenSource=%q want sso", got)
	}
}

func TestTokenSourceTrimmed(t *testing.T) {
	a := &Auth{AccessToken: makeJWT(`{"token_source":"  enterprise  "}`)}
	if got := a.TokenSource(); got != "enterprise" {
		t.Fatalf("TokenSource=%q want enterprise", got)
	}
}

func TestTokenSourceAbsent(t *testing.T) {
	for name, token := range map[string]string{
		"无声明":  makeJWT(`{"sub":"u1"}`),
		"非JWT": "plain-access-token",
		"两段":   "a.b",
		"空串":   "",
	} {
		if got := (&Auth{AccessToken: token}).TokenSource(); got != "" {
			t.Errorf("%s: TokenSource=%q want empty", name, got)
		}
	}
}

func TestTokenSourceNonString(t *testing.T) {
	// token_source 是数字时按「无该声明」处理，不做隐式转换。
	a := &Auth{AccessToken: makeJWT(`{"token_source":123}`)}
	if got := a.TokenSource(); got != "" {
		t.Fatalf("TokenSource=%q want empty", got)
	}
}

func TestTokenSourcePaddedBase64(t *testing.T) {
	// 标准 base64（带 +/ 与 = 填充）也接受：官方 token 用 RawURL，但测试若用
	// StdEncoding 造数据不应被判定为坏 token。
	payload := base64.URLEncoding.EncodeToString([]byte(`{"token_source":"pad"}`))
	a := &Auth{AccessToken: "h." + payload + ".s"}
	if got := a.TokenSource(); got != "pad" {
		t.Fatalf("TokenSource=%q want pad", got)
	}
}

func TestTokenSourceNilAuth(t *testing.T) {
	var a *Auth
	if got := a.TokenSource(); got != "" {
		t.Fatalf("nil TokenSource=%q want empty", got)
	}
}
