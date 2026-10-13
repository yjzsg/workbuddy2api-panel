package usage

import (
	"testing"
	"time"
)

// TestSnapshotByKey 按密钥维度聚合：汇总、按模型下钻、未标记行、排序。
func TestSnapshotByKey(t *testing.T) {
	r := New("")
	now := time.Now()

	// k_a：两次请求（不同模型各一次 + 同模型累加）
	r.Add(now, "cn", "u1", "glm-5.2", "k_a", Delta{
		TotalTokens: 100, HasTotal: true, Credit: 1, HasCredit: true}, true)
	r.Add(now, "cn", "u1", "glm-5.2", "k_a", Delta{
		TotalTokens: 50, HasTotal: true}, true)
	// k_b：global 模型一次
	r.Add(now, "global", "u1", "gpt-6-luna", "k_b", Delta{
		TotalTokens: 200, HasTotal: true}, true)
	// 未标记（引入多密钥之前的旧记录口径）+ 失败一次
	r.Add(now, "cn", "u2", "glm-5.2", "", Delta{TotalTokens: 10, HasTotal: true}, false)

	s := r.Snapshot(0, nil)
	if len(s.ByKey) != 3 {
		t.Fatalf("ByKey 应有 3 行，实际 %d：%+v", len(s.ByKey), s.ByKey)
	}
	byID := make(map[string]KeyUsage, len(s.ByKey))
	for _, ku := range s.ByKey {
		byID[ku.ID] = ku
	}

	if got := byID["k_a"].Agg; got.Requests != 2 || got.TotalTokens != 150 || got.Errors != 0 {
		t.Errorf("k_a 汇总 = %+v，want req=2 tok=150 err=0", got)
	}
	if got := byID["k_a"].Agg.Credits; got != 1 {
		t.Errorf("k_a 积分 = %v want 1", got)
	}
	if got := byID["k_b"].Agg; got.Requests != 1 || got.TotalTokens != 200 {
		t.Errorf("k_b 汇总 = %+v", got)
	}
	if got := byID[""].Agg; got.Requests != 1 || got.Errors != 1 {
		t.Errorf("未标记行 = %+v，want req=1 err=1", got)
	}

	// 按模型下钻：k_a 只有 glm-5.2 一行，且 tokens 累加正确。
	m := byID["k_a"].ByModel
	if len(m) != 1 || m[0].Key != "glm-5.2" || m[0].Agg.TotalTokens != 150 {
		t.Errorf("k_a.ByModel = %+v", m)
	}
	// k_b 的模型与 k_a 不串。
	if len(byID["k_b"].ByModel) != 1 || byID["k_b"].ByModel[0].Key != "gpt-6-luna" {
		t.Errorf("k_b.ByModel = %+v", byID["k_b"].ByModel)
	}

	// 排序：请求数降序 ⇒ k_a(2) 在最前。
	if s.ByKey[0].ID != "k_a" {
		t.Errorf("首个应为 k_a（请求数最多），实际 %q", s.ByKey[0].ID)
	}

	// 各密钥汇总之和 = 全池汇总（不重不漏）。
	var sumReq, sumTok int64
	for _, ku := range s.ByKey {
		sumReq += ku.Agg.Requests
		sumTok += ku.Agg.TotalTokens
	}
	if sumReq != s.Totals.Requests || sumTok != s.Totals.TotalTokens {
		t.Errorf("按密钥求和(%d,%d) != 汇总(%d,%d)",
			sumReq, sumTok, s.Totals.Requests, s.Totals.TotalTokens)
	}
}

// TestSnapshotByKeyModelDrilldownSorted 同一密钥内按模型下钻按 tokens 降序。
func TestSnapshotByKeyModelDrilldownSorted(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u", "small", "k1", Delta{TotalTokens: 10, HasTotal: true}, true)
	r.Add(now, "cn", "u", "big", "k1", Delta{TotalTokens: 9000, HasTotal: true}, true)
	r.Add(now, "cn", "u", "mid", "k1", Delta{TotalTokens: 500, HasTotal: true}, true)

	s := r.Snapshot(0, nil)
	if len(s.ByKey) != 1 {
		t.Fatalf("ByKey=%d want 1", len(s.ByKey))
	}
	got := s.ByKey[0].ByModel
	want := []string{"big", "mid", "small"}
	if len(got) != 3 {
		t.Fatalf("ByModel=%d want 3", len(got))
	}
	for i := range want {
		if got[i].Key != want[i] {
			t.Errorf("ByModel[%d]=%q want %q（应按 tokens 降序）", i, got[i].Key, want[i])
		}
	}
}

// TestSnapshotByKeyWindowRespected 窗口口径同样作用于密钥维度。
func TestSnapshotByKeyWindowRespected(t *testing.T) {
	r := New("")
	old := time.Now().Truncate(time.Hour).Add(-48 * time.Hour)
	r.Add(old, "cn", "u", "m", "k_old", Delta{TotalTokens: 111, HasTotal: true}, true)
	r.Add(time.Now(), "cn", "u", "m", "k_new", Delta{TotalTokens: 222, HasTotal: true}, true)

	// 近 1 小时：只应有 k_new
	s := r.Snapshot(1, nil)
	for _, ku := range s.ByKey {
		if ku.ID == "k_old" {
			t.Errorf("近 1 小时窗口不应包含 48 小时前的 k_old：%+v", ku)
		}
	}
	if len(s.ByKey) != 1 || s.ByKey[0].ID != "k_new" {
		t.Errorf("窗口内 ByKey = %+v，want 仅 k_new", s.ByKey)
	}
	// 全部历史：两条都在
	all := r.Snapshot(0, nil)
	if len(all.ByKey) != 2 {
		t.Errorf("全部历史 ByKey=%d want 2", len(all.ByKey))
	}
}
