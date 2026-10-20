// cost_tier_test.go 成本分层判据回归（2026-10-21）：
// 分层从「EMA 单价 ≤ 0」改为「最近一次原始观测 ≤ 0」，修复 EMA 残值把已免费的号
// 永久钉在收费层的问题（生产实测 3.66e-9 credits/1k 仍判 tier 2，且反向迁移在
// EMA 数学上不可能发生）。同时锁死旧 state.json 的兼容口径：无 last_per_1k 时回退 EMA。
package pool

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestTierUsesLastObservationNotEMA 复刻生产现场：EMA 衰减到 3.66e-9（正数，永不
// 归零），但最近一次真实观测是 0（免费）——必须判 tier 0，否则被硬过滤永久排除。
func TestTierUsesLastObservationNotEMA(t *testing.T) {
	zero := 0.0
	mc := modelCostEntry{
		CostPer1k: 3.66e-9, // 付费观测后的衰减尾巴（0.7^24 量级）
		LastPer1k: &zero,
		LastSeen:  time.Now(),
		Samples:   1635,
	}
	if ti, _ := mc.tier(); ti != 0 {
		t.Errorf("tier=%d want 0（last_per_1k=0 应判免费；EMA 残值不参与判层）", ti)
	}
}

// TestTierFallsBackToEMAWhenLastPer1kMissing 旧 state.json 无 last_per_1k 字段
// （指针 nil）→ 回退 EMA 判层，升级前后判定不变（零回归）。
func TestTierFallsBackToEMAWhenLastPer1kMissing(t *testing.T) {
	paid := modelCostEntry{CostPer1k: 8.5e-4, LastSeen: time.Now(), Samples: 1626}
	if ti, _ := paid.tier(); ti != 2 {
		t.Errorf("旧收费条目 tier=%d want 2（无 last_per_1k 时应回退 EMA）", ti)
	}
	free := modelCostEntry{CostPer1k: 0, LastSeen: time.Now(), Samples: 412}
	if ti, _ := free.tier(); ti != 0 {
		t.Errorf("旧免费条目 tier=%d want 0", ti)
	}
}

// TestTierFlipsBothWays 免费↔收费可双向翻转（旧 EMA 判据只有单向门）。
func TestTierFlipsBothWays(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u"})
	steps := []struct {
		name  string
		cred  float64
		wantT int
	}{
		{"首次免费", 0, 0},
		{"开始收费", 1.5, 2},
		{"再次免费", 0, 0},
	}
	for _, st := range steps {
		p.NoteModelCost("u", "m", st.cred, 1000)
		mc, ok := modelCostSnapshot(p, "u", "m")
		if !ok {
			t.Fatalf("%s：观测未写入", st.name)
		}
		if ti, _ := mc.tier(); ti != st.wantT {
			t.Errorf("%s：tier=%d want %d（ema=%v last=%v）", st.name, ti, st.wantT, mc.CostPer1k, deref(mc.LastPer1k))
		}
	}
}

// TestNoteModelCostRecoversFreeTier 端到端：历史付费污染的号在观测到一次免费请求后
// 必须回到 tier 0 并重新参与选号（旧实现 EMA 永不归零，它永远出局）。
func TestNoteModelCostRecoversFreeTier(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0) // 关掉探索维度，锚定成本分层语义
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "was-paid"})
	p.Add(&auth.Auth{UID: "always-free"})
	// was-paid 积分远高：若只看权重它必胜，成本分层必须压过积分。
	p.SetCredits("was-paid", 1_000_000, 0)
	p.SetCredits("always-free", 1, 0)

	p.NoteModelCost("was-paid", "m", 2.9, 1000) // 历史污染（旧 _sg 记账串味留下的收费观测）
	p.NoteModelCost("always-free", "m", 0, 1000)
	for i := 0; i < 20; i++ {
		if a := p.PickExcludingForRealm(nil, "m", ""); a == nil || a.UID != "always-free" {
			t.Fatalf("第 %d 次选中 %v，want always-free（付费层被硬过滤）", i, a)
		}
	}

	// 它后来免费了：一次免费观测即回到 tier 0。
	p.NoteModelCost("was-paid", "m", 0, 1000)
	mc, _ := modelCostSnapshot(p, "was-paid", "m")
	if ti, _ := mc.tier(); ti != 0 {
		t.Fatalf("tier=%d want 0（免费观测后应回到免费层，ema=%v last=%v）", ti, mc.CostPer1k, deref(mc.LastPer1k))
	}
	seen := false
	for i := 0; i < 100; i++ {
		a := p.PickExcludingForRealm(nil, "m", "")
		if a == nil {
			t.Fatal("选号返回 nil")
		}
		if a.UID == "was-paid" {
			seen = true
			break
		}
	}
	if !seen {
		t.Error("was-paid 恢复 tier 0 后应重新参与抽签（此前被 EMA 残值永久排除）")
	}
}

// TestModelCostLastPer1kPersists last_per_1k 跨重启往返无损，恢复后分层不变。
func TestModelCostLastPer1kPersists(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteModelCost("u1", "m", 2.9, 1000) // 收费
	p.NoteModelCost("u1", "m", 0, 1000)   // 免费回归
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	mc, ok := modelCostSnapshot(p2, "u1", "m")
	if !ok {
		t.Fatal("观测未恢复")
	}
	if mc.LastPer1k == nil {
		t.Fatal("last_per_1k 未持久化（恢复后为 nil，会回退 EMA 判层）")
	}
	if *mc.LastPer1k != 0 {
		t.Errorf("last_per_1k=%v want 0", *mc.LastPer1k)
	}
	if ti, _ := mc.tier(); ti != 0 {
		t.Errorf("恢复后 tier=%d want 0", ti)
	}
	// EMA 仍保留历史（用于排序/展示），不因判层口径改变而失真。
	if mc.CostPer1k <= 0 {
		t.Errorf("ema=%v 应仍为正（历史付费痕迹保留）", mc.CostPer1k)
	}
}

func deref(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}
