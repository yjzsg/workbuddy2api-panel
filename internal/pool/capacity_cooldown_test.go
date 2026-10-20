package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestCapacityCooldownKeepsStickyAvailability 容量类冷却（14003 模型繁忙 / 排队）的语义：
//   - 新会话分配（严格集）避开这个号；
//   - 已有绑定的会话（宽松集 + PickByUIDForModel）仍可用，避免换号导致前缀缓存全废；
//   - 状态面（ModelBlockedForRealm）照旧报告避让，运维视图不变。
//
// 6004 配额类必须继续解除绑定（换模型才有效，钉在原号只会反复失败）。
func TestCapacityCooldownKeepsStickyAvailability(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	const model = "deepseek-v4.1-flash"

	p.CooldownCapacityForModel("u1", time.Minute, time.Now().Add(time.Minute), model, "14003 model busy")

	if got := p.WeightedAvailableUIDsForModelRealm(model, ""); len(got) != 0 {
		t.Fatalf("新会话分配不应选中容量冷却中的号: %v", got)
	}
	if got := p.WeightedAvailableUIDsForModelRealmSticky(model, ""); len(got) != 1 || got[0] != "u1" {
		t.Fatalf("粘性命中应保留容量冷却中的号: %v", got)
	}
	if a := p.PickByUIDForModel("u1", model); a == nil {
		t.Fatal("PickByUIDForModel 应放行容量冷却（让会话留在原号，保住前缀缓存）")
	}
	if blk := p.ModelBlockedForRealm(model, ""); !blk.Blocked {
		t.Fatalf("状态面仍应报告该模型被避让: %+v", blk)
	}

	// 6004 配额类：宽松集与粘性直取都必须拦住。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Minute), "glm-5.3", "6004 quota")
	if got := p.WeightedAvailableUIDsForModelRealmSticky("glm-5.3", ""); len(got) != 0 {
		t.Fatalf("6004 不应被宽松集放行: %v", got)
	}
	if a := p.PickByUIDForModel("u1", "glm-5.3"); a != nil {
		t.Fatal("6004 限额应让 PickByUIDForModel 返回 nil（handler 才能解绑换号）")
	}
}

// TestCapacityCooldownExpiresBackToNormal 冷却到期后两套口径都恢复可用，
// 且 Capacity 标记不会滞留在条目上（过期即 prune，下次选号重新判定）。
func TestCapacityCooldownExpiresBackToNormal(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	const model = "deepseek-v4.1-flash"

	p.CooldownCapacityForModel("u1", time.Minute, time.Now().Add(20*time.Millisecond), model, "14003 model busy")
	time.Sleep(40 * time.Millisecond)

	if got := p.WeightedAvailableUIDsForModelRealm(model, ""); len(got) != 1 {
		t.Fatalf("冷却到期后应恢复可用: %v", got)
	}
	if blk := p.ModelBlockedForRealm(model, ""); blk.Blocked {
		t.Fatalf("冷却到期后不应再报告阻塞: %+v", blk)
	}
}
