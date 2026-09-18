package upstream

import (
	"encoding/json"
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

// TestInjectThinkingOnDemand 锁住「按需开思考」契约（2026-09-18）：
// 客户端没表达思考意图时**不得**注入 thinking.enabled，否则 only_reasoning 模型会
// 被强制拉进思考模式 → reasoning 膨胀吃满 max_tokens → content 被挤空（用户侧"卡循环"）。
func TestInjectThinkingOnDemand(t *testing.T) {
	const M = `"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"max_tokens":100`

	// 1) 什么都没发 → 不注入 thinking、不补 effort（保持上游默认不思考）
	obj := injectAndParse(t, `{`+M+`}`)
	if v, ok := obj["thinking"]; ok {
		t.Errorf("无思考意图却注入了 thinking: %v", v)
	}
	if v, ok := obj["reasoning_effort"]; ok {
		t.Errorf("无思考意图却补了 reasoning_effort: %v", v)
	}

	// 2) 带 reasoning_effort → 注入 enabled，且显式 effort 不被覆盖（#43 场景不回退）
	obj = injectAndParse(t, `{`+M+`,"reasoning_effort":"max"}`)
	if th, ok := obj["thinking"].(map[string]any); !ok || th["type"] != "enabled" {
		t.Errorf("带 effort 应注入 thinking:enabled，实际 %v", obj["thinking"])
	}
	if obj["reasoning_effort"] != "max" {
		t.Errorf("显式 effort 不应被覆盖，实际 %v", obj["reasoning_effort"])
	}

	// 3) 带 reasoning_summary → 注入（对齐官方 isThinkingEnabled）
	obj = injectAndParse(t, `{`+M+`,"reasoning_summary":"auto"}`)
	if th, ok := obj["thinking"].(map[string]any); !ok || th["type"] != "enabled" {
		t.Errorf("带 summary 应注入 thinking:enabled，实际 %v", obj["thinking"])
	}

	// 4) reasoning:{effort} 形态 → 注入
	obj = injectAndParse(t, `{`+M+`,"reasoning":{"effort":"high"}}`)
	if th, ok := obj["thinking"].(map[string]any); !ok || th["type"] != "enabled" {
		t.Errorf("reasoning.effort 应注入 thinking:enabled，实际 %v", obj["thinking"])
	}

	// 5) 显式 disabled → 尊重，并删掉 effort
	obj = injectAndParse(t, `{`+M+`,"thinking":{"type":"disabled"},"reasoning_effort":"max"}`)
	if th, ok := obj["thinking"].(map[string]any); !ok || th["type"] != "disabled" {
		t.Errorf("显式 disabled 应被尊重，实际 %v", obj["thinking"])
	}
	if v, ok := obj["reasoning_effort"]; ok {
		t.Errorf("disabled 时应删掉 reasoning_effort，实际 %v", v)
	}

	// 6) 非 deepseek 模型 → 零改动
	obj = injectAndParse(t, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`)
	if v, ok := obj["thinking"]; ok {
		t.Errorf("非 deepseek 模型不应注入 thinking，实际 %v", v)
	}
}
