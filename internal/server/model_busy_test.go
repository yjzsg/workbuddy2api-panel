// model_busy_test.go 上游 14003「模型繁忙」的分类与冷却落点。
//
// 背景（2026-10-19 生产实测）：41 个号的池子里，1 小时内 73 次 14003 打到 35 个号，
// 而成功请求也只用到 37 个号——几乎「用过就中」。此前它落 applyErrorPolicy 末尾的
// 账号级兜底：CooldownSoftRate 600s 起、softStreak 指数翻倍封顶 2h，等于每小时把
// 池子刷一遍，客户端全靠当场换号硬撑。
//
// 修复口径：14003 与 6020/6021 同属「模型级容量」信号——只让该模型在这个号上避让
// queueCooldownDefault（60s），账号本身与其余模型不受影响。
package server

import (
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// modelBusyBody 是 2026-10-19 抓到的上游原文（字段顺序与文案照抄）。
const modelBusyBody = `{"code":14003,"msg":"too many requests","requestId":"10e3da9e3136f5c228ee83955f50819b","displayMsg":{"en":"Model busy. Please switch models or retry later","zh":"模型繁忙，请换模型或稍后重试"}}`

// TestApplyErrorPolicyModelBusyIsModelLevel 14003 必须落在模型级冷却：
// 账号不背 600s 账号级软冷却，只让该模型在这个号上歇 60s，其余模型照常可选。
func TestApplyErrorPolicyModelBusyIsModelLevel(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 10 * time.Minute})

	h.applyErrorPolicy("u1", upstream.ErrSoftRate, modelBusyBody, "deepseek-v4.1-flash",
		&upstream.Error{Kind: upstream.ErrSoftRate, Status: 429, Msg: modelBusyBody})

	st, _ := p.Status("u1")
	// Status.Cooling 是聚合标记（模型级避让也算「有冷却」），不能用它判断账号级。
	// 真正的判据是 RateLimitedModels：有该模型条目 = 模型级；空 = 账号级。
	if len(st.RateLimitedModels) == 0 || st.RateLimitedModels[0].Model != "deepseek-v4.1-flash" {
		t.Fatalf("应写入该模型的避让条目, got %+v", st.RateLimitedModels)
	}

	blk := p.ModelBlockedForRealm("deepseek-v4.1-flash", "")
	if !blk.Blocked || blk.Count != 1 {
		t.Fatalf("应在该模型上写入模型级避让: %+v", blk)
	}
	if !upstream.IsModelBusyReason(blk.Reason) {
		t.Fatalf("reason 应带模型繁忙前缀, got %q", blk.Reason)
	}
	if !upstream.IsRetryableModelReason(blk.Reason) {
		t.Fatalf("模型繁忙属于「稍后重试」家族, got %q", blk.Reason)
	}

	// 账号级没被罚 ⇒ 同号的其他模型仍可用（这是本次修复的核心收益）。
	if other := p.ModelBlockedForRealm("glm-5.3", ""); other.Blocked {
		t.Fatalf("其他模型不应被牵连: %+v", other)
	}
}

// TestApplyErrorPolicyPlain429StaysAccountLevel 回归：不带 14003/6020/6021 的普通
// 429 仍走账号级软冷却——账号确实在限流时，不该因为这次改动被放过。
func TestApplyErrorPolicyPlain429StaysAccountLevel(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: time.Minute})

	body := `{"code":11140,"msg":"rate limiting"}`
	h.applyErrorPolicy("u1", upstream.ErrSoftRate, body, "deepseek-v4.1-flash",
		&upstream.Error{Kind: upstream.ErrSoftRate, Status: 429, Msg: body})

	st, _ := p.Status("u1")
	if len(st.RateLimitedModels) != 0 {
		t.Fatalf("普通 429 不应写模型级避让条目: %+v", st.RateLimitedModels)
	}
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("普通 429 应触发账号级软冷却: cooling=%v kind=%q", st.Cooling, st.CoolKind)
	}
	if blk := p.ModelBlockedForRealm("deepseek-v4.1-flash", ""); blk.Blocked {
		t.Fatalf("普通 429 不该让该模型被判为阻塞: %+v", blk)
	}
}

// TestApplyErrorPolicy6004StillModelLevel 回归：6004 仍走模型级（上游对齐重置时间
// 那条分支优先级更高，本改动不该把它挤到 14003 之前或之后改变行为）。
func TestApplyErrorPolicy6004StillModelLevel(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 10 * time.Minute})

	body := `{"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-10-19 20:00:00 UTC+8 重置"}`
	h.applyErrorPolicy("u1", upstream.ErrSoftRate, body, "glm-5.3",
		&upstream.Error{Kind: upstream.ErrSoftRate, Status: 429, Msg: body})

	st, _ := p.Status("u1")
	if len(st.RateLimitedModels) == 0 || st.RateLimitedModels[0].Model != "glm-5.3" {
		t.Fatalf("6004 应写模型级限流条目: %+v", st.RateLimitedModels)
	}
	blk := p.ModelBlockedForRealm("glm-5.3", "")
	if !blk.Blocked || !strings.HasPrefix(blk.Reason, "6004") {
		t.Fatalf("6004 应写模型级限流条目: %+v", blk)
	}
	if upstream.IsRetryableModelReason(blk.Reason) {
		t.Fatalf("6004 必须保持「换模型」语义，不能被并入可重试家族: %q", blk.Reason)
	}
}
