// handler_listing_compat_test.go /v1/models 的第三方目录兼容字段（Magpie，上游 issue #156）。
// [移植自根上游 b262660 / 上游 68c08d13]
package server

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestAddListingCompatUnit 只增不改：既有的 supports_images /
// reasoning_supported_efforts 原样保留，额外补两份目录客户端要读的等价字段。
func TestAddListingCompatUnit(t *testing.T) {
	entry := map[string]any{
		"id":                          "cn:hy3",
		"supports_images":             true,
		"reasoning_supported_efforts": []string{"low", "high"},
	}
	got := addListingCompat(entry)
	if got["supports_images"] != true {
		t.Errorf("既有字段不该被改: %v", got["supports_images"])
	}
	mod, ok := got["modalities"].(map[string]any)
	if !ok {
		t.Fatalf("modalities 缺失: %v", got["modalities"])
	}
	in, _ := mod["input"].([]string)
	if len(in) != 2 || in[0] != "text" || in[1] != "image" {
		t.Errorf("modalities.input=%v want [text image]", mod["input"])
	}
	levels, ok := got["supported_reasoning_levels"].([]map[string]any)
	if !ok || len(levels) != 2 {
		t.Fatalf("supported_reasoning_levels=%v want 2 项", got["supported_reasoning_levels"])
	}
	if levels[0]["effort"] != "low" || levels[1]["effort"] != "high" {
		t.Errorf("levels=%v want low/high 原序", levels)
	}
	// ID 前缀保持不变（区分版本，不能为了目录兜底抹掉）。
	if got["id"] != "cn:hy3" {
		t.Errorf("id 被改写成 %v", got["id"])
	}
}

// TestAddListingCompatNoCapabilities 没有能力字段的条目不被塞空壳字段。
func TestAddListingCompatNoCapabilities(t *testing.T) {
	entry := map[string]any{"id": "cn:plain"}
	got := addListingCompat(entry)
	if _, ok := got["modalities"]; ok {
		t.Errorf("无 supports_images 时不该出现 modalities")
	}
	if _, ok := got["supported_reasoning_levels"]; ok {
		t.Errorf("无档位时不该出现 supported_reasoning_levels")
	}
}

// TestAddListingCompatAnySlice JSON 反序列化出来的 []any 形态也要认得
// （/v1/models 的实现路径上两种形态都可能出现）。
func TestAddListingCompatAnySlice(t *testing.T) {
	entry := map[string]any{
		"reasoning_supported_efforts": []any{"low", 42, "max"},
	}
	got := addListingCompat(entry)
	levels, ok := got["supported_reasoning_levels"].([]map[string]any)
	if !ok || len(levels) != 2 {
		t.Fatalf("supported_reasoning_levels=%v want 2 项（非字符串项跳过）", got["supported_reasoning_levels"])
	}
	if levels[1]["effort"] != "max" {
		t.Errorf("levels=%v want 末项 max", levels)
	}
}

// TestModelListCarriesMagpieCompatFields 全表成立：凡有能力旗标的条目都带上对应的
// 目录兼容字段（以后加模型、换目录不会漏）。
func TestModelListCarriesMagpieCompatFields(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, fullFieldsModelsBody, false
	})
	resetModelsCache()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: false})

	list := h.modelList()
	if len(list) == 0 {
		t.Fatal("modelList 为空")
	}
	var hy3 map[string]any
	for _, m := range list {
		if m["id"] == "cn:hy3" {
			hy3 = m
		}
		if m["supports_images"] == true {
			if _, ok := m["modalities"]; !ok {
				t.Errorf("%v: supports_images=true 但缺 modalities", m["id"])
			}
		}
		if raw, ok := m["reasoning_supported_efforts"].([]string); ok && len(raw) > 0 {
			levels, ok := m["supported_reasoning_levels"].([]map[string]any)
			if !ok || len(levels) != len(raw) {
				t.Errorf("%v: supported_reasoning_levels=%v 与 reasoning_supported_efforts=%v 不一一对应",
					m["id"], m["supported_reasoning_levels"], raw)
			}
		}
	}
	if hy3 == nil {
		t.Fatalf("cn:hy3 不在表里: %v", list)
	}
	if _, ok := hy3["modalities"]; !ok {
		t.Errorf("cn:hy3 缺 modalities: %v", hy3)
	}
}
