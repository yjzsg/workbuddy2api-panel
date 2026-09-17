package upstream

import (
	"sync"
	"testing"
)

// TestEffortWarnOnceDedup 锁定降级日志去重语义：同 (realm, model, 请求档, 结果档)
// 只打第一条；换域 / 换档位 / 换模型仍会打（可观测性不被去重吃掉）。
//
// 用独立的 realm 名（"dedup_test"）避免与其他用例的写入互相干扰。
func TestEffortWarnOnceDedup(t *testing.T) {
	effortWarned = sync.Map{}
	const r = "dedup_test"
	cases := []struct {
		model, req, got string
		want            bool
	}{
		{"deepseek-v4.1-flash", "max", "high", true},  // 首次
		{"deepseek-v4.1-flash", "max", "high", false}, // 同组合 → 抑制
		{"deepseek-v4.1-flash", "max", "high", false}, // 再来一次仍抑制
		{"deepseek-v4.1-flash", "low", "high", true},  // 换请求档 → 新组合
		{"deepseek-v4.1-flash", "max", "xhigh", true}, // 换结果档 → 新组合
		{"glm-5.3", "max", "high", true},              // 换模型 → 新组合
		{"deepseek-v4.1-flash", "max", "high", false},
	}
	for i, c := range cases {
		if got := effortWarnOnce(r, c.model, c.req, c.got); got != c.want {
			t.Errorf("case %d (%s %s->%s): got %v want %v", i, c.model, c.req, c.got, got, c.want)
		}
	}
	// 换 realm 应各自首打（cn/global 桶不同）。
	if !effortWarnOnce("dedup_test_cn", "deepseek-v4.1-flash", "max", "high") {
		t.Error("不同 realm 应各自首打")
	}
}

// TestEffortModelLabel 锁定域标注：realm 非空且模型名未带 ":" 时补前缀。
func TestEffortModelLabel(t *testing.T) {
	for _, c := range []struct{ realm, model, want string }{
		{"global", "deepseek-v4.1-flash", "global:deepseek-v4.1-flash"},
		{"cn", "glm-5.3", "cn:glm-5.3"},
		{"", "deepseek-v4.1-flash", "deepseek-v4.1-flash"},                     // 无 realm：原样
		{"global", "global:deepseek-v4.1-flash", "global:deepseek-v4.1-flash"}, // 已带前缀：不重复
	} {
		if got := effortModelLabel(c.realm, c.model); got != c.want {
			t.Errorf("effortModelLabel(%q,%q) = %q want %q", c.realm, c.model, got, c.want)
		}
	}
}

// TestNormalizeReasoningEffortDowngradeStillApplied 去重只作用于日志：请求体改写必须
// 每次都发生（否则第二条请求就会带着上游不认的 max 出去，被 400 毁掉）。
func TestNormalizeReasoningEffortDowngradeStillApplied(t *testing.T) {
	efforts := map[string][]string{"deepseek-v4.1-flash": {"high"}}
	for i := 0; i < 3; i++ {
		obj := map[string]any{"model": "deepseek-v4.1-flash", "reasoning_effort": "max"}
		normalizeReasoningEffort(obj, efforts, "dedup_test")
		if got := obj["reasoning_effort"]; got != "high" {
			t.Fatalf("第 %d 次改写后 = %v，want high（去重不得影响改写）", i+1, got)
		}
	}
}
