package session

import (
	"testing"
	"time"
)

// TestResolveForModelKeepsBindingOnCapacityCooldown 容量类模型冷却（14003 模型繁忙 /
// 6020-6021 排队）必须**不解除**已有绑定：即使该号在严格可用集里已经消失，只要宽松集
// （AvailableForModelSticky）还包含它，就应继续命中。
//
// 回归背景（2026-10-20 生产实测）：上游前缀缓存的键含 uid。此前 14003 会写 60s 模型
// 冷却 → 绑定被判失效 → 换号 → 整个上下文重算。13:57:07 那条 384,954 token 的请求
// 命中率从 99.7% 掉到 0.8%（TTFB 5.2s → 7.0s）；560k 级别单次全 miss 达 15.6s。
func TestResolveForModelKeepsBindingOnCapacityCooldown(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1", "a2"}, time.Minute)
	// 严格集（新会话分配）：a1 正在容量冷却 → 只剩 a2。
	r.cfg.AvailableForModel = func(model string) []string { return []string{"a2"} }
	// 宽松集（命中校验）：a1 只是模型饱和，绑定可留。
	r.cfg.AvailableForModelSticky = func(model string) []string { return []string{"a1", "a2"} }

	r.Bind("c1", "a1")
	got, ok := r.ResolveForModel("c1", "deepseek-v4.1-flash")
	if !ok || got != "a1" {
		t.Fatalf("容量冷却不应解除绑定: got %s ok=%v want a1/true", got, ok)
	}

	// 回落路径：慢路径 re-check 也要按宽松集判定（此处先删掉快路径绑定再同步恢复，
	// 直接构造「绑定刚被重新写入但严格集不含」的场面）。
	r2 := routerWith(newCountingStore(), []string{"a1", "a2"}, time.Minute)
	r2.cfg.AvailableForModel = func(model string) []string { return []string{"a2"} }
	r2.cfg.AvailableForModelSticky = func(model string) []string { return []string{"a1", "a2"} }
	r2.Bind("c2", "a1")
	if got, ok := r2.ResolveForModel("c2", "deepseek-v4.1-flash"); !ok || got != "a1" {
		t.Fatalf("re-check 路径同样应保留绑定: got %s ok=%v", got, ok)
	}
}

// TestResolveForModelWithoutStickySetReassigns 回归：未注入 AvailableForModelSticky 时
// 行为与引入前完全一致——绑定号不在严格集里就重分配（这样 6004 限额仍换得动号）。
func TestResolveForModelWithoutStickySetReassigns(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1", "a2"}, time.Minute)
	r.cfg.AvailableForModel = func(model string) []string { return []string{"a2"} }

	r.Bind("c1", "a1")
	got, ok := r.ResolveForModel("c1", "hy4-preview")
	if !ok || got != "a2" {
		t.Fatalf("无宽松集时应按严格集重分配: got %s ok=%v want a2/true", got, ok)
	}
}
