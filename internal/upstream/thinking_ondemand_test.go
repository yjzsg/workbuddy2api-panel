package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

func injectAndParse(t *testing.T, src string) map[string]any {
	t.Helper()
	out := PrepareBodyOpt([]byte(src), false)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return obj
}

func thinkingTypeOf(obj map[string]any) (string, bool) {
	th, ok := obj["thinking"].(map[string]any)
	if !ok {
		return "", false
	}
	t, _ := th["type"].(string)
	return t, true
}

// TestInjectThinkingOnDemand 锁住 v2 契约（2026-09-18）：
//
//  1. 网关**不再注入** `thinking:{type:"enabled"}` —— 实测该字段单独存在时上游不认
//     （rlen=0），而与 `reasoning_effort` 同时存在会**放大** reasoning，多轮把 max_tokens
//     吃满、content 被挤空（用户侧「卡循环」）。
//  2. 真正让模型思考的是 `reasoning_effort`，所以只**按需补默认档**。
//  3. 客户端没表达思考意图 → 一律不动（保持上游默认不思考）。
func TestInjectThinkingOnDemand(t *testing.T) {
	const M = `"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"max_tokens":100`

	// 1) 什么都没发 → 不补 effort、不注入 thinking
	obj := injectAndParse(t, `{`+M+`}`)
	if _, ok := obj["reasoning_effort"]; ok {
		t.Errorf("无思考意图却补了 reasoning_effort: %v", obj["reasoning_effort"])
	}
	if typ, present := thinkingTypeOf(obj); present && typ == "enabled" {
		t.Errorf("无思考意图却注入了 thinking.type=enabled: %v", obj["thinking"])
	}

	// 2) 带 reasoning_effort → 原样保留，不注入 thinking
	obj = injectAndParse(t, `{`+M+`,"reasoning_effort":"max"}`)
	if obj["reasoning_effort"] != "max" {
		t.Errorf("显式 effort 不应被覆盖，实际 %v", obj["reasoning_effort"])
	}
	if typ, present := thinkingTypeOf(obj); present && typ == "enabled" {
		t.Errorf("v2 不应注入 thinking.type=enabled: %v", obj["thinking"])
	}

	// 3) 带 reasoning_summary → 补默认档 effort，不注入 thinking
	obj = injectAndParse(t, `{`+M+`,"reasoning_summary":"auto"}`)
	if obj["reasoning_effort"] != "high" {
		t.Errorf("带 summary 应补默认档 high，实际 %v", obj["reasoning_effort"])
	}
	if typ, present := thinkingTypeOf(obj); present && typ == "enabled" {
		t.Errorf("v2 不应注入 thinking.type=enabled: %v", obj["thinking"])
	}

	// 4) 显式 thinking=enabled（无 effort）→ 补默认档（该字段单独无效，需配 effort）
	obj = injectAndParse(t, `{`+M+`,"thinking":{"type":"enabled"}}`)
	if obj["reasoning_effort"] != "high" {
		t.Errorf("显式 enabled 缺 effort 应补默认档 high，实际 %v", obj["reasoning_effort"])
	}

	// 5) 显式 disabled → 尊重，并删掉 effort
	obj = injectAndParse(t, `{`+M+`,"thinking":{"type":"disabled"},"reasoning_effort":"max"}`)
	if typ, present := thinkingTypeOf(obj); !present || typ != "disabled" {
		t.Errorf("显式 disabled 应被尊重，实际 %v", obj["thinking"])
	}
	if v, ok := obj["reasoning_effort"]; ok {
		t.Errorf("disabled 时应删掉 reasoning_effort，实际 %v", v)
	}

	// 6) 非 deepseek 模型 → 零改动
	obj = injectAndParse(t, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`)
	if _, ok := obj["thinking"]; ok {
		t.Errorf("非 deepseek 模型不应动 thinking，实际 %v", obj["thinking"])
	}
}

// TestNoThinkingFieldInjected 回归：任何输入都不得凭空出现 thinking.type=enabled。
func TestNoThinkingFieldInjected(t *testing.T) {
	cases := []string{
		`{"model":"deepseek-v4.1-flash","messages":[]}`,
		`{"model":"deepseek-v4.1-flash","messages":[],"reasoning_effort":"max"}`,
		`{"model":"deepseek-v4.1-flash","messages":[],"reasoning_effort":"high"}`,
		`{"model":"deepseek-v4.1-flash","messages":[],"reasoning_summary":"auto"}`,
	}
	for _, c := range cases {
		out := PrepareBodyOpt([]byte(c), false)
		if strings.Contains(string(out), `"type":"enabled"`) {
			t.Errorf("不得注入 thinking.type=enabled，输入=%s 输出=%s", c, out)
		}
	}
}
