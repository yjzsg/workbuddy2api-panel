package usage

import (
	"path/filepath"
	"testing"
	"time"
)

// TestCacheTokensRecordedAndAggregated 缓存三段按命中/未命中/写入分开累计，
// 命中率 = 命中 / (命中 + 未命中) —— **分母不含 write**（写入是"为后续命中付的费"，
// 计入会压低首次请求的命中率）。
func TestCacheTokensRecordedAndAggregated(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "global", "u1", "deepseek-v4.1-flash", Delta{
		HasCacheTokens: true,
		PromptTokens:   1000, HasPromptTokens: true,
		CacheHitTokens: 800, CacheMissTokens: 200, CacheWriteTokens: 50,
	}, true)
	r.Add(now, "global", "u1", "deepseek-v4.1-flash", Delta{
		HasCacheTokens: true,
		PromptTokens:   1000, HasPromptTokens: true,
		CacheHitTokens: 1000, CacheMissTokens: 0, CacheWriteTokens: 0,
	}, true)

	s := r.Snapshot(24, nil)
	if s.Totals.CacheHitTokens != 1800 || s.Totals.CacheMissTokens != 200 || s.Totals.CacheWriteTokens != 50 {
		t.Fatalf("cache = %d/%d/%d, want 1800/200/50",
			s.Totals.CacheHitTokens, s.Totals.CacheMissTokens, s.Totals.CacheWriteTokens)
	}
	// 量纲为百分比（0~100，与上游 usage.go 同口径；前端 app.js usRate 已同步）。
	want := 1800.0 / 2000.0 * 100
	if d := s.Totals.CacheHitRate - want; d > 1e-9 || d < -1e-9 {
		t.Fatalf("hit rate = %v, want %v（分母不含 write）", s.Totals.CacheHitRate, want)
	}
	// 按模型/按域行同样带上缓存数据（前端三张表共用一套行渲染）。
	if len(s.ByModel) != 1 || s.ByModel[0].CacheHitTokens != 1800 {
		t.Fatalf("by_model 未带缓存: %+v", s.ByModel)
	}
}

// TestCacheHitRateZeroWhenNoObservation 没有缓存观测（命中+未命中 = 0）时命中率留 0，
// 由前端按 hit+miss==0 显示 "—"。后端不编造：把"没观测"写成"命中率 0%"是两回事。
func TestCacheHitRateZeroWhenNoObservation(t *testing.T) {
	r := New("")
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 5, HasPromptTokens: true}, true)
	s := r.Snapshot(24, nil)
	if s.Totals.CacheHitTokens != 0 || s.Totals.CacheMissTokens != 0 {
		t.Fatalf("cache 应为 0，得到 %d/%d", s.Totals.CacheHitTokens, s.Totals.CacheMissTokens)
	}
	if s.Totals.CacheHitRate != 0 {
		t.Fatalf("无观测时命中率应为 0（展示侧判 —），得到 %v", s.Totals.CacheHitRate)
	}
}

// TestCacheSurvivesRollup ⚠️ 锁住本仓最容易漏的一处：Rollup 折叠小时桶→日桶时是
// **逐字段累加**（不是整桶复制），新加的缓存三段若漏加，折叠后缓存数据会**静默丢失**
// —— 编译不报错，既有用例也不报错。
func TestCacheSurvivesRollup(t *testing.T) {
	r := New("")
	old := time.Now().AddDate(0, 0, -100) // 超出小时保留窗口 → 会被折叠成日桶
	r.Add(old, "global", "u", "m", Delta{
		HasCacheTokens: true, PromptTokens: 10, HasPromptTokens: true,
		CacheHitTokens: 900, CacheMissTokens: 100, CacheWriteTokens: 30}, true)
	r.Add(old, "global", "u", "m", Delta{
		HasCacheTokens: true, PromptTokens: 10, HasPromptTokens: true,
		CacheHitTokens: 500, CacheMissTokens: 500, CacheWriteTokens: 20}, true)

	r.Rollup(time.Now())
	// ⚠️ 用 hours=0（全部历史）：面板上游 c206468「时间窗口全口径生效」后，**折叠出的日桶
	// 也受时间窗过滤**，100 天前的日桶不再进 24 小时窗口。本用例的靶子是「折叠有没有漏
	// 字段」，不是窗口口径，所以取全部历史来观察折叠结果（窗口口径另有用例覆盖）。
	s := r.Snapshot(0, nil)
	if s.Totals.CacheHitTokens != 1400 || s.Totals.CacheMissTokens != 600 || s.Totals.CacheWriteTokens != 50 {
		t.Fatalf("折叠后 cache = %d/%d/%d, want 1400/600/50（漏字段会让折叠丢数据）",
			s.Totals.CacheHitTokens, s.Totals.CacheMissTokens, s.Totals.CacheWriteTokens)
	}
	if s.Totals.CacheHitRate <= 0 {
		t.Fatalf("折叠后命中率丢了: %v", s.Totals.CacheHitRate)
	}
}

// TestCachePersistsAcrossReload 缓存三段必须真的落盘：flush 后新 Recorder 载入仍在
// （New 会自行 load）。这是"记录要持久化"这条要求的直接验收。
func TestCachePersistsAcrossReload(t *testing.T) {
	p := filepath.Join(t.TempDir(), "usage.json")
	r := New(p)
	r.Add(time.Now(), "global", "u", "m", Delta{
		HasCacheTokens: true, PromptTokens: 100, HasPromptTokens: true,
		CacheHitTokens: 77, CacheMissTokens: 23, CacheWriteTokens: 11}, true)
	r.Save()

	s := New(p).Snapshot(24, nil)
	if s.Totals.CacheHitTokens != 77 || s.Totals.CacheMissTokens != 23 || s.Totals.CacheWriteTokens != 11 {
		t.Fatalf("重载后 cache = %d/%d/%d, want 77/23/11",
			s.Totals.CacheHitTokens, s.Totals.CacheMissTokens, s.Totals.CacheWriteTokens)
	}
}
