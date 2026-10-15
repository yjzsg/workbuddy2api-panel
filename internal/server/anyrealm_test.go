package server

// 测试 /v1/models 裸名（realm-agnostic）条目：交集口径 + 单域不广告 + 不覆盖既有条目。

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikeys"
)

func TestAnyRealmEntryIntersection(t *testing.T) {
	cn := map[string]any{
		"id": "cn:deepseek-v4.1-flash", "name": "Deepseek-V4.1-Flash",
		"context_length": int64(1000000), "max_output_tokens": int64(128000),
		"max_allowed_size":            int64(1000000),
		"supports_images":             true,
		"supports_reasoning":          true,
		"supports_tool_call":          true,
		"can_disable_thinking":        true,
		"only_reasoning":              true,
		"credits":                     "x0.11",
		"vendor":                      "f",
		"description":                 "cn 描述",
		"reasoning_supported_efforts": []string{"low", "high", "max"},
		"reasoning_default_effort":    "high",
		"tags":                        []string{"craft"},
	}
	gl := map[string]any{
		"id": "global:deepseek-v4.1-flash", "name": "Deepseek-V4.1-Flash",
		"context_length": int64(1000000), "max_output_tokens": int64(128000),
		"max_allowed_size":            int64(1000000),
		"supports_images":             true,
		"supports_reasoning":          true,
		"supports_tool_call":          true,
		"can_disable_thinking":        true,
		"only_reasoning":              true,
		"credits":                     "x0.00",
		"reasoning_supported_efforts": []string{"high"},
		"reasoning_default_effort":    "high",
	}
	e := anyRealmEntry("deepseek-v4.1-flash", cn, gl)
	if e == nil {
		t.Fatal("anyRealmEntry=nil，两域同名且字段齐全时应产出条目")
	}
	if e["id"] != "deepseek-v4.1-flash" {
		t.Errorf("id=%v want deepseek-v4.1-flash（裸名，无前缀）", e["id"])
	}
	if e["realm_agnostic"] != true {
		t.Error("realm_agnostic 未标记 true")
	}
	// effort 取交集：global 只支持 high → 裸名不得广告 max（否则 global 侧被上游 400）
	effs, _ := e["reasoning_supported_efforts"].([]string)
	if len(effs) != 1 || effs[0] != "high" {
		t.Errorf("efforts=%v want [high]（两域交集）", effs)
	}
	if e["reasoning_default_effort"] != "high" {
		t.Errorf("default_effort=%v want high", e["reasoning_default_effort"])
	}
	// 域相关字段不得出现（两域不同）
	for _, f := range []string{"credits", "vendor", "description"} {
		if _, ok := e[f]; ok {
			t.Errorf("裸名条目不应含域相关字段 %s", f)
		}
	}
}

// global 侧能力弱于 cn 时，布尔旗标必须取 AND（不能广告 global 做不到的能力）。
func TestAnyRealmEntryBooleanIsAnd(t *testing.T) {
	cn := map[string]any{
		"id": "cn:hy4-preview", "context_length": int64(192000),
		"supports_images": true, "supports_tool_call": true, "supports_reasoning": true,
	}
	gl := map[string]any{
		"id": "global:hy4-preview", "context_length": int64(192000),
		"supports_tool_call": true, "supports_reasoning": true,
		// 无 supports_images
	}
	e := anyRealmEntry("hy4-preview", cn, gl)
	if e == nil {
		t.Fatal("条目为 nil")
	}
	if _, ok := e["supports_images"]; ok {
		t.Error("supports_images 只有 cn 支持，裸名不得透出")
	}
	if e["supports_tool_call"] != true {
		t.Error("两域都支持 tool_call，应透出")
	}
}

// context_length 两域不一致时取小值（保守承诺）。
func TestAnyRealmEntryContextTakesMin(t *testing.T) {
	cn := map[string]any{"id": "cn:x", "context_length": int64(1000000)}
	gl := map[string]any{"id": "global:x", "context_length": int64(200000)}
	e := anyRealmEntry("x", cn, gl)
	if e == nil {
		t.Fatal("条目为 nil")
	}
	if v, _ := e["context_length"].(int64); v != 200000 {
		t.Errorf("context_length=%v want 200000（取两域小值）", v)
	}
}

// 任一域缺 context_length → 不广告裸名（不给客户端字段残缺的名字）。
func TestAnyRealmEntryMissingContextReturnsNil(t *testing.T) {
	cn := map[string]any{"id": "cn:x"}
	gl := map[string]any{"id": "global:x", "context_length": int64(1000)}
	if e := anyRealmEntry("x", cn, gl); e != nil {
		t.Errorf("want nil, got %v", e)
	}
}

// 单域模型不加裸名。
func TestAppendAnyRealmAliasesSkipsSingleRealm(t *testing.T) {
	list := []map[string]any{
		{"id": "cn:only-cn", "context_length": int64(1000)},
		{"id": "global:only-global", "context_length": int64(1000)},
		{"id": "cn:both", "context_length": int64(1000)},
		{"id": "global:both", "context_length": int64(2000)},
	}
	out := appendAnyRealmAliases(list)
	ids := map[string]bool{}
	for _, e := range out {
		id, _ := e["id"].(string)
		ids[id] = true
	}
	if ids["only-cn"] || ids["only-global"] {
		t.Error("单域模型不应出现裸名条目")
	}
	if !ids["both"] {
		t.Error("两域同名模型应出现裸名条目 both")
	}
	if len(out) != len(list)+1 {
		t.Errorf("条目数=%d want %d（+1 裸名）", len(out), len(list)+1)
	}
}

// 已有裸名条目时不自引用、不重复追加。
func TestAppendAnyRealmAliasesNoSelfReference(t *testing.T) {
	list := []map[string]any{
		{"id": "cn:x", "context_length": int64(1000)},
		{"id": "global:x", "context_length": int64(1000)},
		{"id": "x", "context_length": int64(1000)}, // 已是裸名
	}
	out := appendAnyRealmAliases(list)
	if len(out) != 3 {
		t.Errorf("条目数=%d want 3（不应重复追加裸名 x）", len(out))
	}
}

// 裸名条目在 models() 组装链路里的位置：白名单过滤前追加，
// 因此无白名单的密钥（New API 中转在用）能直接看到裸名；
// 而只列了带域前缀白名单的密钥过滤阶段仍按 resolveModel 精确匹配，不受影响。
func TestAnyRealmEntryRespectsKeyWhitelistSemantics(t *testing.T) {
	// 白名单只列 global: 前缀 → 裸名条目不应通过 AllowsModel（realm="" 时 full==bare，
	// 不等于 "global:deepseek-v4.1-flash"），即锁域密钥不会误拿到全池名。
	k := &apikeys.Key{AllowedModels: []string{"global:deepseek-v4.1-flash"}}
	if k.AllowsModel("", "deepseek-v4.1-flash") {
		t.Error("锁 global 的密钥不应被允许使用裸名（会绕过域锁定）")
	}
	// 白名单列裸名 → 任意域都放行（全池粘合语义）。
	k2 := &apikeys.Key{AllowedModels: []string{"deepseek-v4.1-flash"}}
	if !k2.AllowsModel("", "deepseek-v4.1-flash") {
		t.Error("裸名白名单条目应匹配任意域")
	}
}
