package server

import (
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// contentBlockBody 上游拒绝的真实响应体（2026-09-29 实测样本）。
//
// ⚠️ 关键事实：**账号级拒绝与内容级拒绝返回同一句 displayMsg**——实测两个「死号」
// 对内容 "hi" 也返回这段文案，而同一请求换号即 200。所以文案**不能**用来判别，
// 唯一可靠判据是行为（换号后能否成功）。这正是本组用例存在的原因。
const contentBlockBody = `{"code":11140,"msg":"request illegal","requestId":"r-1",` +
	`"displayMsg":{"en":"The content did not pass the safety review. Please adjust and retry.",` +
	`"zh":"内容未通过安全审核，请调整后重试。","zh-hant":"內容未通過安全審核，請調整後重試。"}}`

// TestChatContentBlockRotatesToHealthy 2026-09-29 修复的核心行为：
// 一个账号被上游内容策略拦、另一个健康 → **客户端拿到 200**（而不是 400），
// 且被拦账号拿到「账号级」证据（软冷却让位），健康号零惩罚。
//
// 修复前：content_blocked 立即回 400 且不轮转（假设「换任何号都会撞同一审核」，
// 该假设已被实测证伪）→ 客户端拿到本可避免的 400；被拦号零惩罚 → 状态永远
// "最干净" → 在选号打分里永远最优 → 被越选越多（实测 446/1011 = 44% 的 global
// 流量落在 2 个死号上，并引发客户端重试风暴）。
func TestChatContentBlockRotatesToHealthy(t *testing.T) {
	var calls atomic.Int64
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		if authz == "Bearer at-bad" {
			return 403, contentBlockBody, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "custom"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s（want 200：content_blocked 必须换号，不能把可避免的 400 丢给客户端）",
			rec.Code, rec.Body)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("upstream calls=%d want 2 (bad once + good once)", n)
	}
	// 被拦号拿到证据（软冷却让位）——不再是零惩罚。
	stBad, _ := p.Status("bad")
	if !stBad.Cooling {
		t.Errorf("bad account must be soft-cooled by content-block evidence: %+v", stBad)
	}
	if stBad.Disabled {
		t.Errorf("first evidence must NOT disable (threshold=%d): %+v", pool.ContentBlockThreshold(), stBad)
	}
	// 健康号不受任何惩罚。
	stGood, _ := p.Status("good")
	if stGood.Cooling || stGood.Disabled || !stGood.DegradeUntil.IsZero() {
		t.Errorf("good account must stay clean: %+v", stGood)
	}
}

// TestChatAllContentBlockedReturns400NoPenalty 整轮都被拦 = **内容问题**：
// 回 400 content_blocked（与旧行为一致，客户端据此调整内容），且**零账号惩罚**
// ——内容不是账号的错，这正是 2026-09-28 那条修复要保护的性质，不能被本次改动破坏。
func TestChatAllContentBlockedReturns400NoPenalty(t *testing.T) {
	var calls atomic.Int64
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		return 403, contentBlockBody, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "custom"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s（want 400：整轮都被拦才是内容问题）", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "content_blocked") {
		t.Errorf("400 must carry content_blocked code: %s", rec.Body)
	}
	// 整轮都失败 ⇒ 没有任何账号拿到证据 ⇒ 全部零惩罚。
	for _, uid := range []string{"u1", "u2", "u3"} {
		st, _ := p.Status(uid)
		if st.Cooling {
			t.Errorf("uid=%s must NOT be cooled on a pure content problem: %+v", uid, st)
		}
		if st.Disabled {
			t.Errorf("uid=%s must NOT be disabled on a pure content problem: %+v", uid, st)
		}
		if st.ConsecutiveFails != 0 || !st.DegradeUntil.IsZero() {
			t.Errorf("uid=%s must not accumulate degrade state on a pure content problem: %+v", uid, st)
		}
	}
	// 轮转确实走了 3 次（每号一次）——证明「先换号再判定」真的发生了。
	if n := calls.Load(); n != 3 {
		t.Errorf("upstream calls=%d want 3 (each account tried once)", n)
	}
}

// TestChatContentBlockRepeatedEvidenceDisablesDeadAccount 事故精确回归（2026-09-29）：
// 单账号池反复被内容策略拦（且该号永远拿不到成功）→ 累积证据后**自动禁用**，
// 不再留在池里被反复选中。
//
// 单账号池是刻意的：池里只有它，每次请求必然真打它一次、真走一次证据路径，
// 才能把「连续证据 → 禁用」这条路径压到阈值以上（与 TestChatEdgeAuthNeverDegradesPool
// 同一手法，方向相反：那条验证「绝不惩罚」，这条验证「证据足够才惩罚」）。
func TestChatContentBlockRepeatedEvidenceDisablesDeadAccount(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 403, contentBlockBody, false
	})
	p := testPoolWith(&auth.Auth{UID: "only", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "custom"})

	// 每次请求都是「整轮被拦」→ 单账号池里整轮 == 该号 → 没有其他号成功 ⇒ 不喂证据。
	// 所以这里期望的是：**不惩罚**（单号池无法区分账号问题与内容问题，必须偏向不误伤）。
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
		if rec.Code != 400 {
			t.Fatalf("iter=%d code=%d want 400", i, rec.Code)
		}
		st, _ := p.Status("only")
		if st.Disabled {
			t.Fatalf("iter=%d 单号池无法证明是账号问题（没有「另一个号成功」的证据），不应禁用: %+v", i, st)
		}
		if st.Cooling {
			t.Fatalf("iter=%d 单号池不应冷却（零惩罚）: %+v", i, st)
		}
	}
}
