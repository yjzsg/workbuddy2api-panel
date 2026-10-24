package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// growthFake 假 growth 域上游：任务列表 / accept / claim 三个端点，
// silentAccept=true 时模拟「accept 返回 200+OK 但未真正登记」的形态。
type growthFake struct {
	mu           sync.Mutex
	status       string
	silentAccept bool
	calls        []string
}

func (g *growthFake) setStatus(s string) {
	g.mu.Lock()
	g.status = s
	g.mu.Unlock()
}

func (g *growthFake) callNames() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, len(g.calls))
	copy(out, g.calls)
	return out
}

func (g *growthFake) handler() http.Handler {
	mux := http.NewServeMux()
	// 任务列表（GET）。注意 growthJSON 走 chatBase，claim 走 webBase —— 测试里
	// 两者指向同一个 httptest server，故路径都在这里。
	mux.HandleFunc("/v2/activity/growth/tasks", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		st := g.status
		g.mu.Unlock()
		body := map[string]any{
			"code": 0,
			"data": map[string]any{
				"tasks": []map[string]any{{
					"task_code":     "chat_5",
					"target":        5,
					"current":       5,
					"accept_status": st,
					"has_reward":    true,
					"reward_credit": 300,
					"reward_energy": 5,
				}},
			},
		}
		writeTestJSON(w, body)
	})
	mux.HandleFunc("/v2/activity/growth/tasks/accept", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.calls = append(g.calls, "accept")
		if !g.silentAccept {
			g.status = "accepted"
		}
		g.mu.Unlock()
		writeTestJSON(w, map[string]any{"code": 0, "data": map[string]any{}})
	})
	mux.HandleFunc("/activity/growth/tasks/chat_5/claim", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.calls = append(g.calls, "claim")
		g.mu.Unlock()
		writeTestJSON(w, map[string]any{
			"code": 0,
			"data": map[string]any{"already_claimed": false, "credit": 300, "energy": 5},
		})
	})
	return mux
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// newGrowthPanel 造一个只连假上游的面板（池里一个 CN 账号）。
func newGrowthPanel(t *testing.T, g *growthFake) (*Panel, *auth.Auth, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(g.handler())
	t.Cleanup(srv.Close)

	uc := upstream.New()
	uc.ChatBaseCN = srv.URL
	uc.WebBaseCN = srv.URL
	uc.GlobalEnabled = false

	a := &auth.Auth{UID: "uid-cn-1", Nickname: "测试号", AccessToken: "tok", Domain: "www.codebuddy.cn"}
	pl := pool.New("")
	pl.Add(a)

	p := &Panel{cfg: Config{Pool: pl, Upstream: uc, APIKey: "test-key"}}
	return p, a, srv
}

// TestAcceptWithVerifyRegisters accept 登记生效时返回 true，且只调一次。
func TestAcceptWithVerifyRegisters(t *testing.T) {
	g := &growthFake{status: "not_accepted"}
	p, a, _ := newGrowthPanel(t, g)

	if !p.acceptWithVerify(a, "chat_5") {
		t.Fatal("accept 已登记生效，应返回 true")
	}
	calls := g.callNames()
	if len(calls) != 1 || calls[0] != "accept" {
		t.Fatalf("应只调一次 accept，实际 %v", calls)
	}
}

// TestAcceptWithVerifyRetriesWhenSilentlyDropped 上游 200+OK 但回读仍 not_accepted 时
// 必须重试一次并最终返回 false（调用方据此 502，而不是继续跑动作白跑一遍）。
func TestAcceptWithVerifyRetriesWhenSilentlyDropped(t *testing.T) {
	g := &growthFake{status: "not_accepted", silentAccept: true}
	p, a, _ := newGrowthPanel(t, g)

	if p.acceptWithVerify(a, "chat_5") {
		t.Fatal("accept 未登记生效，应返回 false")
	}
	calls := g.callNames()
	if len(calls) != 2 {
		t.Fatalf("未生效应重试一次（共 2 次 accept），实际 %v", calls)
	}
}

// TestAccountTaskAutoAcceptsFirst 单任务「一键完成」必须先 accept 再跑动作：
// 未接受时任务永远点不亮（2026-10-24 实测），所以 accept 必须发生在 claim 之前。
func TestAccountTaskAutoAcceptsFirst(t *testing.T) {
	g := &growthFake{status: "not_accepted"}
	p, a, _ := newGrowthPanel(t, g)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto", p.withAuth(p.accountTaskAuto))

	req := httptest.NewRequest(http.MethodPost,
		"/panel/api/accounts/"+a.UID+"/tasks/auto",
		strings.NewReader(`{"task_code":"chat_5"}`))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON: %v body=%s", err, rec.Body.String())
	}
	if resp["accepted"] != true {
		t.Fatalf("响应应标记 accepted=true，实际 %v", resp["accepted"])
	}
	if resp["claimed"] != true {
		t.Fatalf("达标后应自动领奖，实际 %v（body=%s）", resp["claimed"], rec.Body.String())
	}
	if resp["credit"] != float64(300) {
		t.Fatalf("到账积分应为 300，实际 %v", resp["credit"])
	}

	calls := g.callNames()
	acceptAt, claimAt := -1, -1
	for i, c := range calls {
		switch c {
		case "accept":
			if acceptAt < 0 {
				acceptAt = i
			}
		case "claim":
			claimAt = i
		}
	}
	if acceptAt < 0 {
		t.Fatalf("必须调用 accept，实际 %v", calls)
	}
	if claimAt >= 0 && acceptAt > claimAt {
		t.Fatalf("accept 必须早于 claim，实际 %v", calls)
	}
}

// TestAccountTaskAutoFailsWhenAcceptNotRegistered accept 未落账时必须 502 且不领奖，
// 避免「动作白跑 + 谎报成功」。
func TestAccountTaskAutoFailsWhenAcceptNotRegistered(t *testing.T) {
	g := &growthFake{status: "not_accepted", silentAccept: true}
	p, a, _ := newGrowthPanel(t, g)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto", p.withAuth(p.accountTaskAuto))

	req := httptest.NewRequest(http.MethodPost,
		"/panel/api/accounts/"+a.UID+"/tasks/auto",
		strings.NewReader(`{"task_code":"chat_5"}`))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("期望 502，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	for _, c := range g.callNames() {
		if c == "claim" {
			t.Fatalf("accept 未生效时不应领奖，实际调用 %v", g.callNames())
		}
	}
}
