// fallback_modelcooled_test.go 全冷却兜底必须尊重**模型级**冷却（2026-10-14）。
//
// 背景（生产实测）：CN 的 glm-5.3 被上游 6004 全体限流期间，
// pickEarliestExpiryLocked 的排序判据只有 expiry()（账号级 until / 熔断 / 降权三者最早），
// 而模型级冷却**不写账号级 until**（CooldownSoftForModel 的既定契约）⇒ 账号级 until
// 最早的那个号（红花）被连续兜底选中 12 次，15:11:12–15:13:03 每 ~2.3s 白打一次上游、
// 客户端 12 连败；而它的上游重置时刻在 4.5 小时后（19:48:10）——每次重试注定失败。
//
// 本文件钉住三条契约：
//  1. 模型级冷却中的号不参与兜底；
//  2. 本域全部模型级冷却时兜底返回 nil（交给 handler 报 model_unavailable）；
//  3. model 为空（无模型上下文）与「只被账号级冷却」两条既有语义零变化。
package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// softCool 写账号级软冷却（兜底路径的参与前提）。
func softCool(p *Pool, uid string, d time.Duration) {
	p.mu.Lock()
	e := p.byUID[uid]
	e.coolKind = CoolSoft
	e.until = time.Now().Add(d)
	p.mu.Unlock()
}

// modelCool 写模型级冷却（6004 形态：只进 modelCooldowns，不动账号级 until）。
func modelCool(p *Pool, uid, model string, d time.Duration) {
	p.mu.Lock()
	e := p.byUID[uid]
	if e.modelCooldowns == nil {
		e.modelCooldowns = map[string]modelCooldown{}
	}
	e.modelCooldowns[model] = modelCooldown{
		Until:  time.Now().Add(d),
		Reason: "6004 model rate limit",
	}
	p.mu.Unlock()
}

// TestFallbackSkipsModelCooledAccount 账号级 until 最早、但对该模型仍处模型级冷却的号
// 必须被跳过，改选次早的——否则「最早到期」这个判据会让同一个号被反复选中。
func TestFallbackSkipsModelCooledAccount(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	softCool(p, "u1", 5*time.Minute)  // 账号级 until 最早 → 旧逻辑必选它
	softCool(p, "u2", 30*time.Minute) // 次早
	modelCool(p, "u1", "glm-5.3", 2*time.Hour)

	a := p.PickExcludingForRealm(nil, "glm-5.3", "")
	if a == nil {
		t.Fatal("应兜底到 u2，得到 nil")
	}
	if a.UID != "u2" {
		t.Fatalf("u1 对该模型仍有模型级冷却，不该被兜底选中；得到 %s", a.UID)
	}

	// 反向对照：u1 的模型级冷却只锁 glm-5.3，换模型它仍是「最早到期」→ 必须还能被选中。
	// 若这条不成立，说明实现把模型级冷却误当成了账号级排除。
	if a := p.PickExcludingForRealm(nil, "deepseek-v4.1-flash", ""); a == nil || a.UID != "u1" {
		t.Fatalf("其他模型不该受 glm-5.3 模型级冷却影响：got %v", a)
	}
}

// TestFallbackNilWhenAllModelCooled 本域全部账号对该模型模型级冷却 → 兜底返回 nil，
// 让 handler 走 model_unavailable（「换模型」）而不是白打上游再换一个号接着打。
func TestFallbackNilWhenAllModelCooled(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	softCool(p, "u1", 5*time.Minute)
	softCool(p, "u2", 5*time.Minute)
	modelCool(p, "u1", "glm-5.3", 2*time.Hour)
	modelCool(p, "u2", "glm-5.3", 2*time.Hour)

	if a := p.PickExcludingForRealm(nil, "glm-5.3", ""); a != nil {
		t.Fatalf("全部模型级冷却时应返回 nil，得到 %s", a.UID)
	}

	// 契约 3a：model 为空（面板探活等无模型上下文路径）不受影响，仍按账号级 until 兜底。
	if a := p.PickExcludingForRealm(nil, "", ""); a == nil {
		t.Fatal("model 为空时兜底语义必须保持不变，不应返回 nil")
	}
	// 契约 3b：换模型仍可选。
	if a := p.PickExcludingForRealm(nil, "deepseek-v4.1-flash", ""); a == nil {
		t.Fatal("其他模型不该受 glm-5.3 的模型级冷却影响")
	}
}

// TestFallbackStillUsesAccountLevelCooledAccount 「只被账号级软冷却」的号照旧参与兜底
// （这是兜底的本意：软冷却可能已恢复，失败成本仅一轮换）。防止本次改动把账号级一起误杀。
func TestFallbackStillUsesAccountLevelCooledAccount(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	softCool(p, "u1", 5*time.Minute)

	a := p.PickExcludingForRealm(nil, "glm-5.3", "")
	if a == nil || a.UID != "u1" {
		t.Fatalf("账号级软冷却的号必须仍可兜底，got %v", a)
	}
}

// TestModelBlockedForRealmRealmScoped 域过滤：CN 号全被该模型挡住、global 号还能服务时，
// **按请求域**判定应报「阻塞」（否则客户端拿到 503 no_healthy_account 这种可重试形状，
// 而它其实是「换个模型」）。不限域（ModelBlocked）保持原语义。
func TestModelBlockedForRealmRealmScoped(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	modelCool(p, "cn1", "glm-5.3", time.Hour)

	st := p.ModelBlockedForRealm("glm-5.3", "cn")
	if !st.Blocked || st.Count != 1 {
		t.Fatalf("cn 域：唯一 CN 号被挡 → 应报阻塞 Count=1，得到 %+v", st)
	}
	if st.Reason == "" || st.Until.IsZero() {
		t.Errorf("应带出上游原因与最早解封时刻：%+v", st)
	}

	if st := p.ModelBlockedForRealm("glm-5.3", "global"); st.Blocked {
		t.Fatalf("global 域：g1 可服务 → 不应报阻塞：%+v", st)
	}
	if st := p.ModelBlocked("glm-5.3"); st.Blocked {
		t.Fatalf("不限域：g1 可服务 → 不应报阻塞：%+v", st)
	}
}
