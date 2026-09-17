package server

import (
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestModelListNameFieldDynamicCN 动态分支：上游下发的 name（显示名）透出到 /v1/models；
// 上游省略 name 的模型整体省略该字段（不输出空字符串，不编造）。
func TestModelListNameFieldDynamicCN(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[
			{"id":"dyn-named","name":"Hunyuan T1","maxInputTokens":65536,"maxOutputTokens":8192},
			{"id":"dyn-unnamed","maxInputTokens":65536,"maxOutputTokens":8192}
		],"agents":[{"name":"cli","models":["dyn-named","dyn-unnamed"]}]}}`, false
	})
	resetModelsCache()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: false})

	got := h.modelList()
	byID := map[string]map[string]any{}
	for _, m := range got {
		if id, ok := m["id"].(string); ok {
			byID[id] = m
		}
	}
	if name := byID["cn:dyn-named"]["name"]; name != "Hunyuan T1" {
		t.Errorf("dyn-named name=%v want Hunyuan T1", name)
	}
	if _, ok := byID["cn:dyn-unnamed"]["name"]; ok {
		t.Error("dyn-unnamed should omit name field (upstream omitted)")
	}
}

// TestModelListNameFieldNoCNStaticFallback 无 CN 健康号 → CN 面静态兜底表（面板层）。
func TestModelListNameFieldNoCNStaticFallback(t *testing.T) {
	resetModelsCache()
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: upstream.New(), GlobalEnabled: false})
	if got := h.modelList(); len(got) != len(staticModels) {
		t.Fatalf("no CN account: modelList len=%d want static fallback %d", len(got), len(staticModels))
	}
}

// TestModelListNameFieldGlobalNarrow 窄表探测（只有 ID 名单）→ 条目无 name（数据源没给，不编造）。
func TestModelListNameFieldGlobalNarrow(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()
	cf := newGlobalModelsHandlerFake(t, 200, `{"code":0,"data":["gpt-5.4","narrow-only"]}`) // 窄表：只有 ID
	p := testPoolWith(&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})
	got := h.modelList()
	if len(got) == 0 {
		t.Fatal("narrow probe should yield global entries")
	}
	for _, m := range got {
		if id, _ := m["id"].(string); strings.HasPrefix(id, "global:") {
			if _, ok := m["name"]; ok {
				t.Errorf("global narrow entry should not carry name (no data source): %v", id)
			}
		}
	}
}
