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
//   - **布尔开关 enable_thinking**（harness 等客户端用它表达"要不要思考"）：在显式
//     thinking.type 缺省时按它归一化 —— false ⇒ 走 disabled 分支（不注入 thinking、
//     删 effort 档；实测思考帧归零），true ⇒ 走 enabled 分支。
//     [2026-10-13 补] 此前只看 thinking.type，客户端说 false 也照开思考（实测
//     enable_thinking=false + reasoning_effort=high 仍出 524 帧思维链），客户端会把
//     意外的 reasoning_content 当正文显示。
//   - 非 deepseek 模型（glm/kimi/qwen 等）→ 零改动（enable_thinking 是 qwen 系的原生
//     字段，绝不能在这里动它）。
package upstream

import (
	"fmt"
	"log"
	"strings"
	"sync"
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

// IsDeepSeekModel 是 isDeepSeekModel 的导出面：handler 的**密钥级思考开关**需要同口径
// 判定（只对 deepseek 系落 thinking 字段——其他模型的思考开关语义不同，见 thinking.go 头注释）。
// 与 server.ResolveModel 同一模式：包内用小写，跨包走导出面，避免两处口径漂移。
func IsDeepSeekModel(model string) bool { return isDeepSeekModel(model) }

// backfillReasoningContent DeepSeek 多轮一致性：保证每条 assistant 消息带
// reasoning_content 字段且值为 string——即 requiresReasoningContentOnAssistantMessages
// （官方客户端 matches 规则，issue #165 对齐官方 apply 门控）。
//
// 门控（对齐官方 ReasoningContentBackfillRule：thinkingEnabled || hasTrace），
// thinkingEnabled 取自注入后请求体的 thinking.type == "enabled"（injectThinking 先行，
// payload.go 管线顺序已保证；本网关对 deepseek 无条件注入 enabled，等价于非 disabled 一律补）：
//   - 非 deepseek 模型 → 零改动（isDeepSeekModel 闸不动）。
//   - deepseek + enabled（含 L1 注入后）→ 每条 assistant 保证 reasoning_content 是
//     string：已有 string 原样保留（不覆盖）；reasoning 是非空 string 且 rc 非 string →
//     复制 reasoning 值；两者皆无 → 补空串 ""。第三方客户端丢推理回传（零痕迹）形态
//     下官方本就补，网关此前只移植了 hasTrace 半边（issue #165 修复点）。
//   - deepseek + disabled + 无痕迹 → 零改动（官方 thinkingEnabled=false 且 ec=false → 不补）。
//   - deepseek + disabled + 有痕迹 → 照补（官方 hasTrace 半边，双方一致）。
//
// 归一化对齐官方 "string"!=typeof 语义：reasoning_content 为 null/数字等非 string
// 值时不算「已有」，落补 ""/复制分支（旧代码键存在即跳过，null 会被当「已有」漏补）。
// hasTrace = 会话内任一消息带非空 reasoning（string）或已有 reasoning_content 字段
// （比官方仅扫 assistant 的口径宽，只影响 disabled 分支，装饰性差异）。
func backfillReasoningContent(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !isDeepSeekModel(model) {
		return
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}
	// thinkingEnabled 半边：读注入后的 thinking.type（与官方 el.thinkingEnabled 对应）。
	thinkingEnabled := false
	if th, ok := obj["thinking"].(map[string]any); ok {
		if typ, _ := th["type"].(string); strings.EqualFold(strings.TrimSpace(typ), "enabled") {
			thinkingEnabled = true
		}
	}
	// hasTrace 半边：检测是否有任何 reasoning 痕迹（非空 reasoning 或已有 reasoning_content）。
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
	if !thinkingEnabled && !hasTrace {
		return
	}
	// 第二遍：所有 assistant 消息补/复制 reasoning_content 字段，并镜像保证
	// reasoning 字段存在且非空（issue #165 追评——部分账号/租户对 thinking 形态
	// 校验 len(reasoning)>0：缺失/null/空串 400，空白串 200；官方 CLI 本就给
	// assistant 挂上一轮 reasoning 文本，见 itemsToMessages 的 applyPendingReasoning）。
	// 跳过条件只认 string（官方 "string"!=typeof 才动手）：null/数字归一化。
	//   - reasoning 已是非空 string → 不动；
	//   - rc 是非空 string → 镜像写入 rc 值（两字段最终都存在且非空）；
	//   - 两者皆无/皆空 → 补单个空格 " "（上游 len>0 不 trim：空白串过闸、空串
	//     不过——空白串占位有官方 Moonshot 规则 "-" 同款先例，且对模型上下文
	//     无语义影响：该字段是透传校验位非内容消费位）。
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}
		rc, hasRC := msg["reasoning_content"].(string)
		if hasRC {
			// rc 已有 string → 不覆盖（原有语义保留）。
		} else if r, ok := msg["reasoning"].(string); ok {
			rc = r
			msg["reasoning_content"] = rc
		} else {
			rc = ""
			msg["reasoning_content"] = rc
		}
		// 镜像：reasoning 缺失/null/空串 → 归一化（非空 rc 优先，皆无补 " "）。
		if r, ok := msg["reasoning"].(string); ok && r != "" {
			continue // 已非空 → 不覆盖
		}
		if rc != "" {
			msg["reasoning"] = rc
		} else {
			msg["reasoning"] = " "
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
	// 布尔开关归一化：显式 thinking.type 最具体，优先；缺省时看 enable_thinking。
	// 为什么必须归一化：harness 用 enable_thinking 表达"要不要思考"，而网关此前只看
	// thinking.type ⇒ 客户端说 false 也照开（实测 524 帧思维链），客户端把意外的
	// reasoning_content 当正文显示。翻译成上游认的 thinking.type 才能真的关得上。
	if typ == "" {
		if on, has := boolFlag(obj["enable_thinking"]); has {
			if !ok {
				th = map[string]any{}
				obj["thinking"] = th
				ok = true
			}
			if on {
				typ = "enabled"
			} else {
				typ = "disabled"
			}
			th["type"] = typ
		}
	}
	// reasoning_effort 的"关思考"取值（off）：与 thinking.type=disabled /
	// enable_thinking=false 同路径 —— 删档 + 不注入 thinking。
	// 为什么必须在这里截：客户端用它表达"别思考"，而网关此前既不认、又把非法档透传给
	// 上游，上游对非法 reasoning_effort 不走 reasoning_content 通道（思维链混进 content，
	// 客户端当正文显示）。2026-10-13 实测。显式 thinking.type 更具体（上面已归一化），
	// 故只在 typ 仍空时生效。
	if typ == "" && isOffReasoningEffort(obj) {
		delete(obj, "reasoning_effort")
		delete(obj, "reasoningEffort")
		return
	}
	// 显式控制分支：type 非空（enabled/disabled 均为明确意图）→ 不改 type。
	if typ != "" {
		if strings.EqualFold(typ, "disabled") {
			delete(obj, "reasoning_effort")
			delete(obj, "reasoningEffort")
			return // disabled：关思考且不带任何 effort（照抄客户端 case 行为）
		}
		ensureDeepSeekEffort(obj, defaultEffort) // 显式 enabled 缺 effort → 补默认档
		return
	}
	// 无 thinking（或 thinking 非法非对象值）或 thinking 对象 type 缺失/为空：
	// 注入 enabled（客户端 case "deepseek" 行为）。有 reasoning_effort 也走此分支
	// （effort 保留给既有降级逻辑，开关照开）。
	if !ok {
		obj["thinking"] = map[string]any{"type": "enabled"}
	} else {
		th["type"] = "enabled"
	}
	ensureDeepSeekEffort(obj, defaultEffort)
}

// boolFlag 解析"要不要"类开关：兼容 bool 与字符串（"false"/"0"/"no"/"off" 为假）。
// 第二个返回值表示"字段存在且可识别"——不存在/不可识别时调用方保持原行为。
func boolFlag(v any) (bool, bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "1", "yes", "on":
			return true, true
		case "false", "0", "no", "off":
			return false, true
		}
	}
	return false, false
}

// thinkingProbeMu / thinkingProbeSeen 同形态只记一次，避免刷屏（上限 32 种，防无界增长）。
var (
	thinkingProbeMu   sync.Mutex
	thinkingProbeSeen = map[string]bool{}
)

// probeThinkingFlags 一次性诊断：把客户端「思考开关」的形态记进日志。
//
// 为什么需要：2026-10-13 实测 deepseek-harness 带 enable_thinking=false 时思考仍被打开
// （网关此前只认 thinking.type），客户端把意外的 reasoning_content 当正文显示。此探针把
// 「客户端到底发了哪些开关、取值是什么」钉在日志里，不必靠猜；同形态只记一次。
//
// 只记开关字段（thinking.type / enable_thinking / reasoning_effort / reasoning_summary）
// 的**名字与取值** —— 它们是布尔/枚举控制位，不含任何对话内容。
func probeThinkingFlags(obj map[string]any) {
	if obj == nil {
		return
	}
	var b strings.Builder
	if th, isMap := obj["thinking"].(map[string]any); isMap {
		typ, _ := th["type"].(string)
		b.WriteString("thinking.type=" + strings.TrimSpace(typ) + " ")
	} else if _, has := obj["thinking"]; has {
		b.WriteString("thinking=<非对象> ")
	}
	for _, k := range []string{"enable_thinking", "reasoning_effort", "reasoningEffort", "reasoning_summary"} {
		if v, has := obj[k]; has {
			b.WriteString(k + "=" + fmt.Sprintf("%v", v) + " ")
		}
	}
	shape := strings.TrimSpace(b.String())
	if shape == "" {
		shape = "(无思考开关)"
	}
	thinkingProbeMu.Lock()
	seen := thinkingProbeSeen[shape]
	if !seen && len(thinkingProbeSeen) < 32 {
		thinkingProbeSeen[shape] = true
	}
	thinkingProbeMu.Unlock()
	if seen {
		return
	}
	log.Printf("[thinking] 客户端思考开关形态（同形态只记一次）：%s", shape)
}

// isOffReasoningEffort 客户端是否用 reasoning_effort 表达"关思考"（唯一取值 "off"，
// 大小写/空白不敏感；snake/camel 两键都看）。
//
// 为什么只有 "off"（用户口径 2026-10-13）：默认档 high；none / "" / null / 未知词
// 一律回落到默认档 —— 因为官方上游对非法档**不走 reasoning_content 通道**（见
// ensureDeepSeekEffort 的说明）。
func isOffReasoningEffort(obj map[string]any) bool {
	for _, k := range []string{"reasoning_effort", "reasoningEffort"} {
		if v, ok := obj[k]; ok {
			if sv, isStr := v.(string); isStr && strings.EqualFold(strings.TrimSpace(sv), "off") {
				return true
			}
		}
	}
	return false
}

// isEffortTier 是否为真实档位：effortRank 内且不是控制值 "off"（"off" 由
// injectThinking 在开思考判定前截走，不是档位）。
func isEffortTier(v any) bool {
	sv, ok := v.(string)
	if !ok {
		return false
	}
	sv = strings.TrimSpace(strings.ToLower(sv))
	if sv == "off" {
		return false
	}
	_, known := effortRank[sv]
	return known
}

// ensureDeepSeekEffort 保证「开思考」的请求带一个**真实档位**的 reasoning_effort。
//
// 为什么必须真实档位（2026-10-13 直连官方实测）：官方上游只认已知档位；非法/无值
// （none / "" / null / 非字符串 / 未知词 / 字段缺失）时它**不走 reasoning_content
// 通道**，而是把思维链混进 content —— 客户端当正文显示（用户报的
// "let me 这些思考显示成正文" 即此）。故此处把一切非档位值归一到默认档。
//
// 规则：
//   - 任一键已是真实档位 → 原样保留（降级交给 normalizeReasoningEffort）。
//   - 其余（缺失 / null / 空串 / 非字符串 / 未知词）→ 用默认档（defaultEffort，
//     空则硬编码 high）；已有键就地覆盖（snake 优先），无则新增 snake。
func ensureDeepSeekEffort(obj map[string]any, defaultEffort string) {
	if defaultEffort == "" {
		defaultEffort = defaultDeepSeekEffort
	}
	if v, ok := obj["reasoning_effort"]; ok && isEffortTier(v) {
		return
	}
	if v, ok := obj["reasoningEffort"]; ok && isEffortTier(v) {
		return
	}
	for _, k := range []string{"reasoning_effort", "reasoningEffort"} {
		if _, ok := obj[k]; ok {
			obj[k] = defaultEffort
			return
		}
	}
	obj["reasoning_effort"] = defaultEffort
}
