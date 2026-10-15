package server

// 裸名（无 realm 前缀）模型条目：/v1/models 追加「两域同名模型的地域无关别名」。
//
// 为什么需要：resolveModel 自 3d32370 起把裸名解析为 realm=""（地域无关、全池参与粘合），
// 请求侧裸名已完全可用；但 /v1/models 只列 cn: 与 global: 前缀条目 → 客户端
// （DSH / New API / Codex 等按列表选模型）根本选不到裸名，只能在两个域之间二选一，
// 中转层再按权重轮换 → 每请求跨 realm → 必然换号 → 前缀缓存被打散。
//
// 保守口径（跨域可用 = 两域能力交集，绝不广告任一域做不到的属性）：
//   - context_length / max_allowed_size / max_output_tokens：取两域较小值；
//   - reasoning_supported_efforts：取交集，default 取交集中的默认档；
//     例：cn:deepseek-v4.1-flash = [low high max]、global = [high] → 裸名只广告 [high]，
//     避免客户端给 global 侧传 max 被上游 400（issue #84 同源约束）。
//   - supports_images / supports_reasoning / supports_tool_call / only_reasoning /
//     can_disable_thinking：取交集（AND），true 表示两域都支持才透出。
//   - credits / vendor / description 等域相关字段：省略（两域不同，编造无意义）；
//     name 沿用 CN 侧展示名并标注「任意地域」，realm_agnostic=true 供客户端识别。
//
// 不产生任何路由副作用：这些条目只是"广告"，真正决定可用域的是 resolveModel
// 返回的 realm（裸名 → "" → PickExcludingForRealm 不做域过滤 → 全池）。

import "sort"

// appendAnyRealmAliases 在既有 cn:/global: 条目之后，为两域同名模型追加裸名条目。
// 单域模型（只在 cn 或只在 global 出现）不加裸名——那会让客户端广告一个
// 实际只能落单域的名字，跨域粘合名不副实。
func appendAnyRealmAliases(list []map[string]any) []map[string]any {
	if len(list) < 2 {
		return list
	}
	// 按裸名聚合两域条目（保持 list 的出现序，最后统一排序输出）。
	byBare := make(map[string]map[string]map[string]any, len(list))
	order := make([]string, 0, len(list))
	for _, e := range list {
		id, _ := e["id"].(string)
		realm, bare := resolveModel(id)
		if realm == "" || bare == "" {
			continue // 已是裸名条目或非法前缀：不参与聚合，避免自引用
		}
		if _, ok := byBare[bare]; !ok {
			order = append(order, bare)
		}
		if byBare[bare] == nil {
			byBare[bare] = make(map[string]map[string]any, 2)
		}
		byBare[bare][realm] = e
	}
	// 已有裸名 id 集合：list 可能已含裸名条目（如模型_map 别名或上游直出），
	// 此时不得再合成同名条目（否则 /v1/models 出现重复 id，客户端行为不确定）。
	existing := make(map[string]bool, len(list))
	for _, e := range list {
		if id, _ := e["id"].(string); id != "" {
			if realm, _ := resolveModel(id); realm == "" {
				existing[id] = true
			}
		}
	}
	out := make([]map[string]any, 0, len(order))
	for _, bare := range order {
		if existing[bare] {
			continue
		}
		pair := byBare[bare]
		cn, okCN := pair["cn"]
		gl, okGL := pair["global"]
		if !okCN || !okGL {
			continue
		}
		if entry := anyRealmEntry(bare, cn, gl); entry != nil {
			out = append(out, entry)
		}
	}
	if len(out) == 0 {
		return list
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["id"].(string) < out[j]["id"].(string)
	})
	res := make([]map[string]any, 0, len(list)+len(out))
	res = append(res, list...)
	res = append(res, out...)
	return res
}

// anyRealmEntry 由两域条目合成一条裸名条目（交集口径）。任一域缺失关键字段时
// 返回 nil（不广告），避免给客户端一个字段残缺的模型名。
func anyRealmEntry(bare string, cn, gl map[string]any) map[string]any {
	// context_length 必须两域都有且为正，否则裸名的上下文承诺无依据。
	cnCtx, ok1 := asInt64(cn["context_length"])
	glCtx, ok2 := asInt64(gl["context_length"])
	if !ok1 || !ok2 || cnCtx <= 0 || glCtx <= 0 {
		return nil
	}
	entry := map[string]any{
		"id":       bare,
		"object":   "model",
		"created":  1753600000,
		"owned_by": "workbuddy-any-realm",
		// realm_agnostic=true：客户端可据此知道该名字不锁地域。
		// realms 列出可落地域，便于排障时对照请求实际落到哪域。
		"realm_agnostic": true,
		"realms":         []string{"cn", "global"},
		// name 沿用 CN 侧展示名（两域同名模型展示名一致），标注「任意地域」。
		"context_length": minInt64(cnCtx, glCtx),
	}
	if n := anyRealmDisplayName(cn, gl, bare); n != "" {
		entry["name"] = n
	}
	// max_allowed_size / max_output_tokens：取小值（保守，短边承诺）。
	if v, ok := minBothInt64(cn, gl, "max_allowed_size"); ok {
		entry["max_allowed_size"] = v
	}
	if v, ok := minBothInt64(cn, gl, "max_output_tokens"); ok {
		entry["max_output_tokens"] = v
	}
	// 布尔能力旗标：AND（两域都支持才透出）。
	for _, f := range []string{
		"supports_images", "supports_reasoning", "supports_tool_call",
		"only_reasoning", "can_disable_thinking",
	} {
		if boolField(cn, f) && boolField(gl, f) {
			entry[f] = true
		}
	}
	// effort 档位：交集。default 取交集里 cn 的默认档；cn 默认档不在交集则省略。
	if cnEff, ok1 := asStrings(cn["reasoning_supported_efforts"]); ok1 {
		if glEff, ok2 := asStrings(gl["reasoning_supported_efforts"]); ok2 {
			if inter := intersectStrings(cnEff, glEff); len(inter) > 0 {
				entry["reasoning_supported_efforts"] = inter
				if def, _ := cn["reasoning_default_effort"].(string); containsString(inter, def) {
					entry["reasoning_default_effort"] = def
				} else if def, _ := gl["reasoning_default_effort"].(string); containsString(inter, def) {
					entry["reasoning_default_effort"] = def
				}
			}
		}
	}
	// tags：交集（两域都有才广告）。
	if ct, ok1 := asStrings(cn["tags"]); ok1 {
		if gt, ok2 := asStrings(gl["tags"]); ok2 {
			if inter := intersectStrings(ct, gt); len(inter) > 0 {
				entry["tags"] = inter
			}
		}
	}
	return entry
}

func anyRealmDisplayName(cn, gl map[string]any, bare string) string {
	if n, ok := cn["name"].(string); ok && n != "" {
		return n + "（任意地域）"
	}
	if n, ok := gl["name"].(string); ok && n != "" {
		return n + "（任意地域）"
	}
	return bare
}

func asInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case int64:
		return t, true
	case int:
		return int64(t), true
	case float64:
		return int64(t), true
	}
	return 0, false
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func minBothInt64(a, b map[string]any, field string) (int64, bool) {
	av, ok1 := asInt64(a[field])
	bv, ok2 := asInt64(b[field])
	if !ok1 || !ok2 || av <= 0 || bv <= 0 {
		return 0, false
	}
	return minInt64(av, bv), true
}

func boolField(m map[string]any, field string) bool {
	v, ok := m[field].(bool)
	return ok && v
}

func asStrings(v any) ([]string, bool) {
	raw, ok := v.([]string)
	if !ok {
		return nil, false
	}
	return raw, len(raw) > 0
}

// intersectStrings 按 a 的顺序保留同时出现在 a、b 中的元素（交集，顺序稳定）。
func intersectStrings(a, b []string) []string {
	set := make(map[string]bool, len(b))
	for _, s := range b {
		set[s] = true
	}
	out := make([]string, 0, len(a))
	for _, s := range a {
		if set[s] {
			out = append(out, s)
		}
	}
	return out
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
