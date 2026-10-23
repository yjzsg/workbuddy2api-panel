package server

import (
	"fmt"
	"testing"
)

// urlErrTimeout 模拟 net/http 在建连阶段返回的错误：实现了 net.Error，
// Timeout() 为 true，Error() 文本与真实 url.Error 一致。
type urlErrTimeout struct {
	msg string
}

func (e urlErrTimeout) Error() string   { return e.msg }
func (e urlErrTimeout) Timeout() bool   { return true }
func (e urlErrTimeout) Temporary() bool { return true }

func TestIsConnEstablishErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			"TLS 握手超时",
			fmt.Errorf(`Post "https://www.workbuddy.ai/v2/chat/completions": net/http: TLS handshake timeout`),
			true,
		},
		{
			"TCP 拒绝连接",
			fmt.Errorf(`Post "https://www.workbuddy.ai/v2/chat/completions": dial tcp 43.160.158.125:443: connect: connection refused`),
			true,
		},
		{
			"DNS 解析失败",
			fmt.Errorf(`Post "https://www.workbuddy.ai/v2/chat/completions": dial tcp: lookup www.workbuddy.ai: no such host`),
			true,
		},
		{
			"网络不可达",
			fmt.Errorf(`Post "https://www.workbuddy.ai/v2/chat/completions": dial tcp 43.160.158.125:443: connect: network is unreachable`),
			true,
		},
		{
			"服务端响应头超时（不算建连失败）",
			urlErrTimeout{`Post "https://www.workbuddy.ai/v2/chat/completions": net/http: timeout awaiting response headers`},
			false,
		},
		{"EOF（读阶段，走抖动分支但不算建连）", fmt.Errorf("Post \"https://x\": EOF"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isConnEstablishErr(c.err); got != c.want {
				t.Fatalf("isConnEstablishErr(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestUpstreamTimeoutExcludesConnEstablish 建连失败虽然满足 net.Error.Timeout()，
// 但必须被判为「非上游超时」，从而走换号分支而不是停转 + 模型级冷却。
func TestUpstreamTimeoutExcludesConnEstablish(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			"TLS 握手超时 → 换号",
			urlErrTimeout{`Post "https://www.workbuddy.ai/v2/chat/completions": net/http: TLS handshake timeout`},
			false,
		},
		{
			"连接被拒 → 换号",
			urlErrTimeout{`Post "https://www.workbuddy.ai/v2/chat/completions": dial tcp 1.2.3.4:443: connect: connection refused`},
			false,
		},
		{
			"响应头超时 → 仍按上游超时停转",
			urlErrTimeout{`Post "https://www.workbuddy.ai/v2/chat/completions": net/http: timeout awaiting response headers`},
			true,
		},
		{"普通 net.Error 超时（无建连特征）", timeoutErr{true}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isUpstreamTimeout(c.err, false); got != c.want {
				t.Fatalf("isUpstreamTimeout(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
