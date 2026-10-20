package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestStripUsageCredit usage.credit 必须被剥掉，其余字段原样保留。
//
// 背景（2026-10-20）：积分消耗是网关的计费口径，只留在网关（reqlog / 成本账本 /
// 面板），不下发客户端。网关侧 stats.Credit() 在写出之前已从同一帧解析完，
// 故剥离不影响记账。
func TestStripUsageCredit(t *testing.T) {
	in := `{"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"credit":0.5}}`
	got := stripUsageCredit(in)
	if strings.Contains(got, `"credit"`) {
		t.Fatalf("credit 未被剥离: %s", got)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(got), &obj); err != nil {
		t.Fatalf("剥离后不是合法 JSON: %v (%s)", err, got)
	}
	u, _ := obj["usage"].(map[string]any)
	if u == nil {
		t.Fatalf("usage 丢失: %s", got)
	}
	for _, k := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
		if _, ok := u[k]; !ok {
			t.Fatalf("usage.%s 被误删: %s", k, got)
		}
	}
	if obj["id"] != "c1" {
		t.Fatalf("其它字段被破坏: %s", got)
	}
}

// TestStripUsageCreditNoopCases 不该改写的三种输入必须逐字节不变。
func TestStripUsageCreditNoopCases(t *testing.T) {
	cases := []struct{ name, in string }{
		{"无 usage", `{"id":"c1","choices":[{"delta":{"content":"hi"}}]}`},
		{"usage 无 credit", `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`},
		{"非法 JSON", `{"usage":{"credit":1}`},
		{"credit 在顶层而非 usage", `{"credit":1,"usage":{"prompt_tokens":1}}`},
	}
	for _, c := range cases {
		if got := stripUsageCredit(c.in); got != c.in {
			t.Errorf("%s: 不应改写，got %q want %q", c.name, got, c.in)
		}
	}
}

// TestStripUsageCreditKeepsZero credit=0（限免模型）同样要剥——它是计费口径而非
// 业务字段，留着会让客户端以为有消耗。
func TestStripUsageCreditKeepsZero(t *testing.T) {
	in := `{"usage":{"prompt_tokens":5,"credit":0}}`
	got := stripUsageCredit(in)
	if strings.Contains(got, "credit") {
		t.Fatalf("credit:0 也应剥离: %s", got)
	}
}
