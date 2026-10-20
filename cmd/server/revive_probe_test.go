package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

type probeRT func(*http.Request) (*http.Response, error)

func (f probeRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// probeClient 构造一个把对话请求固定回指定状态码的上游客户端。
func probeClient(status int, body string, hits *int) *upstream.Client {
	rt := probeRT(func(r *http.Request) (*http.Response, error) {
		if hits != nil {
			*hits++
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})
	c := &http.Client{Transport: rt}
	return &upstream.Client{HTTP: c, ChatHTTP: c, ChatBaseCN: "https://fake.example", ChatBaseGlobal: "https://fake.example"}
}

const bannedReason = "account banned by upstream (11140 request illegal), re-login required"

// TestProbeBannedAccountsRevivesOn200 探测到 200 即自动回池——上游限制解除后
// 不需要人工点「解冻」。
func TestProbeBannedAccountsRevivesOn200(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	p.Disable("u1", bannedReason)

	var hits int
	up := probeClient(200, `{"choices":[{"message":{"content":"1"}}]}`, &hits)
	probeBannedAccounts(context.Background(), p, up)

	if hits == 0 {
		t.Fatal("未发出探测请求")
	}
	st, _ := p.Status("u1")
	if st.Disabled {
		t.Fatalf("探测 200 后应自动恢复，实际仍禁用: %+v", st)
	}
}

// TestProbeBannedAccountsKeepsDisabledOn403 仍 403 时保持禁用（不反复进池白撞）。
func TestProbeBannedAccountsKeepsDisabledOn403(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	p.Disable("u1", bannedReason)

	var hits int
	up := probeClient(403, `{"code":11140,"msg":"request illegal"}`, &hits)
	probeBannedAccounts(context.Background(), p, up)

	if hits == 0 {
		t.Fatal("未发出探测请求")
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatal("仍被限制时不应恢复")
	}
}

// TestProbeBannedAccountsSkipsOtherReasons 其它禁用原因（手动/余额/内容）不探测：
// 那些要么是人的决定，要么有别的恢复路径。
func TestProbeBannedAccountsSkipsOtherReasons(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	p.Disable("u1", "余额不足")
	p.Disable("u2", bannedReason)

	var hits int
	up := probeClient(200, `{"choices":[{"message":{"content":"1"}}]}`, &hits)
	probeBannedAccounts(context.Background(), p, up)

	if hits != 1 {
		t.Fatalf("只应探测 1 个被封账号，实际 %d 次", hits)
	}
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatal("非封禁原因不应被探测/恢复")
	}
	if st, _ := p.Status("u2"); st.Disabled {
		t.Fatal("被封账号探测 200 后应恢复")
	}
}
