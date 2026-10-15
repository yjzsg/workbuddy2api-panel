package server

import "strings"

// resolveModel 解析模型名协议（PLAN D6）：
//
//	分布式前缀： "[realm:]model"
//
// 取第一个 ":"，前段恰为 "cn"/"global" 才剥离；否则视为裸名。
// 大小写敏感（前缀必须是精确的小写枚举）。bare 即出站/选号/账本使用的裸模型名。
//
// 【2026-10-15】裸名的 realm 由 "cn" 改为 **空串（realm-agnostic）**：
//
// 为什么：中转层（New API 等）会把**无前缀**的模型名按权重分发到多个地域渠道
// （截图实测：DEEP #737 权重 3= global / DEEP_cn #786 权重 1 = cn，同分组）。
// 于是同一个客户端会话的请求在网关侧看到的是 `deepseek-v4.1-flash`（裸名）与
// `cn:deepseek-v4.1-flash` 交替——而选号按 realm 过滤，绑定号在另一域必然
// `available=false` → `delete(entries,key)` 重新分配 → **每个号只吃到零星几条，
// 上游前缀缓存全是冷前缀**（实测命中率 0.2%~5%，粘住时 99.6%，成本差约 40 倍）。
//
// 裸名 = "我没指定地域" 的语义本就应该是**全池参与**：粘性键可以落在任意域的账号上，
// 只要那个号对本模型可用就一直用下去，跨域不再触发重新分配。显式写 `cn:` / `global:`
// 的请求仍严格限定该域（想要域内专属资源/费率/容灾语义时用前缀）。
//
// 出站不受影响：`upstream.Client` 用 `a.Realm()`（**账号自己的域**）决定打哪个
// 上游端点（client.go prepareBody/Chat），与请求解析出的 realm 无关。
// realm 为空时按"无限制"处理的下游：
//   - `PickExcludingForRealm(..., realm="")` → 不做域过滤（realm.go 的 `realm != ""` 守卫）
//   - `WeightedAvailableUIDsForModelRealm(..., realm="")` → 同上，粘性可用集含两域
//   - `ModelBlockedForRealm(..., realm="")` → 全池判定
//   - `AllowsModel("", model)` → 只按裸名匹配白名单（apikeys.go 已有该分支）
//   - `ModelRate("", model)` → realmKey("") 退 "cn" 费率表；仅影响面板档位展示，
//     实际扣费取上游返回的 credit，不受此影响。
func resolveModel(model string) (realm, bare string) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "", model // 裸名 = 地域无关，全池参与粘合
	}
	prefix := model[:idx]
	if prefix != "cn" && prefix != "global" {
		return "", model // 冒号前段不在枚举内：按裸名处理，不剥离
	}
	return prefix, model[idx+1:]
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }
