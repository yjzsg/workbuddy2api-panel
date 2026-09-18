// thinking.go DeepSeek 思维链开启：出站请求体注入 thinking:{type:"enabled"} + 默认档位。
//
// 根因（issue #43，Hermes 逆向官方客户端 codebuddy.js 已确认）：
// 官方客户端对 deepseek 系模型标记 thinkingFormat:"deepseek" + requiresReasoningContentOnAssistantMessages，
// 发请求时「开思考」必须显式带 thinking:{type:"enabled"}，否则上游默认按不思考应答
// （思维链不返回）。网关 payload 层此前完全不感知该字段，透传请求没有这个开关
// → 上游不给思维链；glm/kimi 走其他 thinkingFormat（qwen 系 enable_thinking 或默认开）所以正常。
//
// 打回修复（Hermes #43 验收实测）：
//
//	thinking.type=enabled 单一字段不足——真实上游 deepseek-v4-flash 对「不带 reasoning_effort」的裸请求
//	仍然按不思考应答（reasoning_content 长度 0），带 reasoning_effort:high 才有思维链。
//	逆向 codebuddy.js 证实：isThinkingEnabled = !!(reasoning_summary || reasoning_effort || reasoning?.effort)，
//	case "deepseek" 的 enabled 分支在实际出站里同时保留 reasoning_effort，官方「开思考」= thinking.type:enabled
//	+ 某档 effort；默认档来自 reasoning.defaultEffort ?? 兜底 "high"（configure thinking 无来源时 warn fallback to 'high'）。
//
// 行为对齐官方客户端（两路组合）：
//   - thinking.type 已显式 enabled / disabled → 客户端显式控制，绝不覆盖；disabled 时照抄 case 行为
//     删 reasoning_effort（snake/camel 双字段）。enabled 但缺 effort → 补默认档（官方 configure 行为）。
//   - 无 thinking / thinking.type 空 / 已有 reasoning_effort → 注入 {type:"enabled"} + 补默认档。
//   - 显式 reasoning_effort 一律不覆盖、不降级（降级交给 payload.go normalizeReasoningEffort）。
//   - 非 deepseek 模型（glm/kimi/qwen 等）→ 零改动。
package upstream

import (
	"strings"
)

// defaultDeepSeekEffort 官方客户端默认档兜底（configure thinking 无来源时 warn fallback to 'high'，
// REASONING_SUPPLEMENTS.defaultEffort 亦为 "high"）。补入后走 normalizeReasoningEffort 降级管线，
// 模型不支持 high 时自动落到 ≤high 的最高支持档。
const defaultDeepSeekEffort = "high"

// lookupDefaultEffort 从 FetchModels 缓存的 defaultEfforts 表按模型名查默认档。
// nil map 或模型未缓存 → 空串（thinking.go 回退硬编码 high）。
// 键为模型 ID 原样（与 efforts 缓存对齐：normalizeReasoningEffort 精确匹配 model）。
func lookupDefaultEffort(defaultEfforts map[string]string, model string) string {
	if len(defaultEfforts) == 0 || model == "" {
		return ""
	}
	return defaultEfforts[model]
}

// isDeepSeekModel 模型名以 deepseek 为前缀（不区分大小写）。
// 覆盖 deepseek-v4.1-flash / deepseek-v4-pro / deepseek-r1 等变体；
// 前缀匹配对齐官方 thinkingFormat:"deepseek" 的判定口径，避免漏注。
func isDeepSeekModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek")
}

// backfillReasoningContent DeepSeek 多轮一致性：历史 assistant 消息带 reasoning 痕迹时，
// 上游要求后续请求所有 assistant 消息都带 reasoning_content 字段（string，可为空串）
// ——即 requiresReasoningContentOnAssistantMessages（官方客户端 matches 规则）。
//
// 规则（对齐官方客户端逻辑）：
//   - 会话内任一 assistant 消息带非空 reasoning（string）或已有 reasoning_content 字段
//     → 所有 assistant 消息确保有 reasoning_content（string）：
//   - reasoning 非空且无 reasoning_content → 复制 reasoning 值
//   - 已有 reasoning_content → 原样保留（不覆盖）
//   - 两者皆无 → 补空串 ""
//   - 任何 assistant 均无 reasoning 痕迹 → 零改动（不白白加字段）。
//
// 仅 deepseek 模型生效（thinkingFormat:deepseek + requiresReasoningContent）。
func backfillReasoningContent(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !isDeepSeekModel(model) {
		return
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}
	// 第一遍：检测是否有任何 reasoning 痕迹（非空 reasoning 或已有 reasoning_content）。
	hasTrace := false
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		if r, ok := msg["reasoning"].(string); ok && r != "" {
			hasTrace = true
			break
		}
		if _, ok := msg["reasoning_content"]; ok {
			hasTrace = true
			break
		}
	}
	if !hasTrace {
		return
	}
	// 第二遍：所有 assistant 消息补 reasoning_content 字段 —— **一律补空串**。
	//
	// 【2026-09-18 修复 v3】原实现把历史 `reasoning` 的值**复制**进 `reasoning_content`
	// （对齐官方客户端的 ReasoningContentBackfillRule）。实测这会造成**历史推理重放**：
	//
	//   · 客户端（WorkBuddy）发的是 `reasoning` 字段（实测 158 条 / 0.39MB，
	//     `reasoning_content` 字段数为 0）；上游**不认 `reasoning`**，直连时会被忽略。
	//   · 但本函数把它复制成上游认的 `reasoning_content` → 16 万 token 的历史推理
	//     全部进入上下文（实测 prompt 448,875 → 清空后 287,559）。
	//   · 模型看到自己此前**全部**推理后倾向继续/复述 → reasoning 膨胀吃满 max_tokens、
	//     content 被挤空 → 下一轮历史更长 → 滚雪球（用户侧「卡循环」）。
	//
	// 实测对照（同一真实请求体）：原样 14.3s / rlen=1733；清空历史 reasoning 后
	// 6.8s / rlen=1 / content 正常。
	//
	// 上游的要求（requiresReasoningContentOnAssistantMessages）是**字段必须存在**，
	// 并不要求内容非空 —— 所以补空串即可满足格式，同时不把历史推理带进上下文。
	// 客户端自带的 `reasoning_content`（若真有）保持原样不覆盖。
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}
		if _, ok := msg["reasoning_content"]; ok {
			continue // 已有 → 不覆盖（客户端显式给的保留）
		}
		msg["reasoning_content"] = ""
	}
	// 第三遍：**清空历史 assistant 的 `reasoning` 值**（字段保留，值置空）。
	//
	// 【2026-09-18 修复 v3 补】上一版只补了 `reasoning_content=""`，但客户端发的
	// `reasoning` 值仍原样出站 —— 实测它同样被上游算进 prompt：
	// 真实请求体 158 条历史 reasoning（0.39MB）→ prompt=448,875；
	// 清空后 → 287,559（省 16 万 token）。
	// 历史推理对后续轮次没有价值，留着只会让模型复述/放大自己的旧思路。
	// 保留字段本身（不是删除）以兼容上游对字段存在的宽松预期。
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "assistant" {
			continue
		}
		if _, ok := msg["reasoning"]; ok {
			msg["reasoning"] = ""
		}
	}
}

// injectThinking 按 DeepSeek 思维链开关规则改写请求体。非 deepseek 零改动。
//
// 核心逻辑（对齐官方客户端）：
//   - 「开思考」必须 thinking.type=enabled + 有 effort 档位（Hermes #43 打回证据）。
//   - 显式 thinking.type 非空 → 客户端显式控制：enabled 缺 effort 时补默认档；
//     disabled 尊重并删 reasoning_effort（snake/camel 双字段）。
//   - 无 thinking / type 空 / 已有 effort → 注入 enabled 并补默认档（已有 effort 不覆盖）。
//
// defaultEffort 为该模型声明的默认档（来自 FetchModels 缓存 reasoning.defaultEffort）；
// 空串时回退硬编码 defaultDeepSeekEffort（向后兼容）。
func injectThinking(obj map[string]any, defaultEffort string) {
	model, _ := obj["model"].(string)
	if !isDeepSeekModel(model) {
		return
	}
	th, ok := obj["thinking"].(map[string]any)
	typ := ""
	if ok {
		typ, _ = th["type"].(string)
		typ = strings.TrimSpace(typ)
	}
	// 显式控制分支：type 非空（enabled/disabled 均为明确意图）→ 不改 type。
	if strings.EqualFold(typ, "disabled") {
		delete(obj, "reasoning_effort")
		delete(obj, "reasoningEffort")
		return // disabled：关思考且不带任何 effort（照抄客户端 case 行为）
	}
	// 【2026-09-18 修复 v2】**不再注入 thinking:{type:"enabled"}**，只按需补默认档 effort。
	//
	// 实测（global:deepseek-v4.1-flash，注入开关 A/B 对照）：
	//   · thinking 单独存在（无 effort）→ rlen=0，**对上游根本无效**；
	//   · 真正让模型思考的是 `reasoning_effort`（只发 effort → rlen=152）；
	//   · 但 thinking 与 effort **同时存在**会「放大」思考 —— 多轮实测
	//     注入时 rlen 829→2348→2155→**3979**（吃满 max_tokens，轮 4 finish=length），
	//     去掉注入后同条件 rlen 503→804→1421→200→779→1065、**全部 finish=stop**。
	//   → 注入 thinking 有害无益：它既不生效（单独），又会让 reasoning 膨胀挤空 content，
	//     用户侧表现为「卡循环」。
	//
	// 而 issue #43（「客户端要思维链却拿不到」）的真正成因是**缺 effort 档位**，不是缺
	// thinking 开关 —— 所以本函数保留 ensureDeepSeekEffort，按需补默认档即可。
	// 判定口径对齐官方 `isThinkingEnabled = !!(reasoning_summary || reasoning_effort ||
	// reasoning?.effort)`：客户端没表达思考意图 → 不补、不思考（保持上游默认）。
	if !hasThinkingIntent(obj) {
		return
	}
	ensureDeepSeekEffort(obj, defaultEffort)
}

// hasThinkingIntent 报告请求体是否表达了「要思考」的意图，口径对齐官方客户端
// `isThinkingEnabled`：reasoning_summary / reasoning_effort / reasoning.effort 任一存在即真。
// 用于避免无条件注入 thinking.enabled 把「客户端没要思考」的请求强制拉进思考模式。
func hasThinkingIntent(obj map[string]any) bool {
	for _, k := range []string{"reasoning_effort", "reasoningEffort", "reasoning_summary", "reasoningSummary"} {
		if v, ok := obj[k]; ok && v != nil {
			return true
		}
	}
	// reasoning:{effort:...} 形态（官方 reasoning?.effort）
	if r, ok := obj["reasoning"].(map[string]any); ok {
		if v, ok := r["effort"]; ok && v != nil {
			return true
		}
	}
	// 显式 thinking.type=enabled：客户端明确要求思考。实测该字段**单独存在时上游不认**
	// （rlen=0），必须配 effort 档位才生效 —— 所以这里返回真、由调用方补默认档。
	if th, ok := obj["thinking"].(map[string]any); ok {
		if t, _ := th["type"].(string); strings.EqualFold(strings.TrimSpace(t), "enabled") {
			return true
		}
	}
	return false
}

// ensureDeepSeekEffort 缺 effort 档位时补默认档（snake 优先，camel 兜底）。
// 已有任一 effort → 不覆盖（显式档位不做任何改写，降级交给 normalizeReasoningEffort）。
// defaultEffort 空串 → 回退 defaultDeepSeekEffort（硬编码 "high"）。
func ensureDeepSeekEffort(obj map[string]any, defaultEffort string) {
	_, hasSnake := obj["reasoning_effort"]
	if hasSnake {
		return
	}
	_, hasCamel := obj["reasoningEffort"]
	if hasCamel {
		return
	}
	if defaultEffort == "" {
		defaultEffort = defaultDeepSeekEffort
	}
	obj["reasoning_effort"] = defaultEffort
}
