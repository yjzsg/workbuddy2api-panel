// realmweight.go 按 (模型, 域) 记账的动态权重。
//
// 背景（2026-10-19 生产实测）：同一模型（deepseek-v4.1-flash）在 cn 与 global
// 两个域都有账号，但两域的拥挤程度差得很远——4 小时内 cn 域 45 次请求 0 次限流，
// global 域 86 次请求 21 次限流（19.6%）。裸名请求（realm=""）是**全池**选号，
// 账号级权重只看 credits/闲置/成功率，完全不知道「这个号所在的域现在很挤」，
// 于是 85% 的流量继续往 global 灌。
//
// 机制：给每个 (模型, 域) 记一个 0~1 的权重。
//   - 限流（ErrSoftRate 全家：14003 / 6004 / 6020 / 6021 / 普通 429）→ 乘性衰减；
//   - 时间线性恢复（读时按 elapsed 补，无需定时器）；
//   - 只在 realm=="" 的请求上参与选号（显式 cn:/global: 请求没有选择权）。
//
// 这是**优先级**而非配额：高权重域先被挑，挑不到（在途占满/冷却/不健康）自然落到
// 低权重域。cn 域只有 6 个号，靠这个语义才不会把 35 个 global 号的流量硬塞过去。
package pool

import (
	"math/rand/v2"
	"time"
)

const (
	// defaultRealmWeightDecay 一次限流后的乘数（半衰式：1.0 → 0.5 → 0.25 …）。
	defaultRealmWeightDecay = 0.5
	// defaultRealmWeightFloor 权重下限：留一点，好让该域恢复后能被重新探到。
	defaultRealmWeightFloor = 0.05
	// defaultRealmWeightRecoverPerMin 每分钟恢复量（0.2 → 5 分钟从 floor 回满）。
	defaultRealmWeightRecoverPerMin = 0.2
	// defaultRealmWeightMaxRatio 域间「每账号负载」倍率上限。
	//
	// 为什么需要它（2026-10-19）：权重是按**账号**乘的，域的流量质量 = n_r × w_r。
	// 池子里 cn 6 个号、global 35 个号，若 global 限流把 w 打到 0.05、cn 保持 1.0，
	// 6 个 cn 号会吃掉 6/(6+1.75)≈77% 的流量——账号数这个容量维度被完全忽略了。
	// 钳制后最高域 / 最低域的**每账号负载**最多差 5 倍：6 个号吃 46%，35 个号吃 54%，
	// 既用上了 cn 的健康余量，又不会把它压垮。
	//
	// 注意账号数 n_r 不需要显式进公式：pick 是按账号累积权重的，n_r 份同时相加
	// 就是该域的总质量。钳制的只是「每账号」的相对强度。
	defaultRealmWeightMaxRatio = 5.0
)

// realmWeightKey 域权重键。同一模型名可跨域，必须分别记账。
type realmWeightKey struct {
	model string
	realm string
}

// realmWeight 单个 (模型, 域) 的运行态。不持久化（同 lastUsed/usedSeq 口径）：
// 重启后归 1.0，最多多打几次 429 重新学一遍。
type realmWeight struct {
	w      float64   // 当前权重，1.0 = 满血
	lastAt time.Time // 上次结算时刻（时间恢复的起点）
}

// SetRealmWeight 注入域权重参数（main 从 config 解析后调用）。
// 任一值 <=0 时保持该字段的默认值，便于只调其中一项。
func (p *Pool) SetRealmWeight(decay, floor, recoverPerMin, maxRatio float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if decay > 0 && decay < 1 {
		p.realmWeightDecay = decay
	}
	if floor > 0 {
		p.realmWeightFloor = floor
	}
	if recoverPerMin > 0 {
		p.realmWeightRecoverPerMin = recoverPerMin
	}
	if maxRatio >= 1 {
		p.realmWeightMaxRatio = maxRatio
	}
}

// maxRealmBias 静态域偏好倍率上限：再大也只是把一个域压死，
// 而「压死」应该用 global.enabled=false 那种显式开关表达，不该藏在倍率里。
const maxRealmBias = 20.0

// SetRealmBias 注入静态域偏好倍率（key = "cn"/"global"，缺省 1.0）。
//
// 与 realm_weight_* 的关系：后者是**动态**的（哪一域报限流就降权、随时间恢复），
// 本项是**策略**（运维就是想让某一域多吃/少吃）。两者相乘后再参与跨域抽签。
// 非法值（<=0、NaN）被丢弃（等价 1.0），过大值钳到 maxRealmBias。
func (p *Pool) SetRealmBias(bias map[string]float64) {
	clean := make(map[string]float64, len(bias))
	for k, v := range bias {
		if k == "" || v <= 0 || v != v { // v != v 判 NaN，省掉 math 依赖
			continue
		}
		if v > maxRealmBias {
			v = maxRealmBias
		}
		clean[k] = v
	}
	p.mu.Lock()
	p.realmBias = clean
	p.mu.Unlock()
}

// RealmBias 返回当前静态域偏好倍率副本（供 /status 与测试观察）。
func (p *Pool) RealmBias() map[string]float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.realmBias) == 0 {
		return nil
	}
	out := make(map[string]float64, len(p.realmBias))
	for k, v := range p.realmBias {
		out[k] = v
	}
	return out
}

// realmWeightLocked 取 (模型, 域) 的当前权重，顺带按 elapsed 做时间恢复。
// 调用方必须持 p.mu（pick 路径已在写锁内）。
func (p *Pool) realmWeightLocked(model, realm string, now time.Time) float64 {
	if model == "" || realm == "" {
		return 1
	}
	st := p.realmWeights[realmWeightKey{model: model, realm: realm}]
	if st == nil {
		return 1
	}
	w := st.w
	if elapsed := now.Sub(st.lastAt); elapsed > 0 {
		w += elapsed.Minutes() * p.realmWeightRecoverPerMin
		if w > 1 {
			w = 1
		}
	}
	st.w = w
	st.lastAt = now
	return w
}

// realmWeightMultipliersLocked 算出本次 pick 要乘到各账号上的域乘数表。
//
// 两步：
//  1. 各域健康因子（限流衰减 + 时间恢复，目标 1.0 —— 也就是「时间长了回到
//     只按账号数分配」的稳态）；
//  2. 每账号负载比钳制：health_r ≤ min(health) × maxRatio。
//     账号数不同（实测 cn 6 / global 35）时，光看健康因子会让 6 个号的域吃掉
//     77% 流量；钳制把「每账号」的相对强度差压在 maxRatio 倍以内。
//     账号数本身不进公式——pick 按账号累加权重，n_r 份同时相加就是域总质量。
//
// 单域（或 cands 只落在一个域）时不钳制：没有比较对象，钳制只会平白压低。
func (p *Pool) realmWeightMultipliersLocked(cands []*entry, model string, now time.Time) map[string]float64 {
	if model == "" || len(cands) == 0 {
		return nil
	}
	health := map[string]float64{}
	for _, e := range cands {
		r := e.a.Realm()
		if r == "" {
			continue
		}
		if _, ok := health[r]; ok {
			continue
		}
		health[r] = p.realmWeightLocked(model, r, now)
	}
	if len(health) < 2 {
		return health
	}
	minW := 1.0
	for _, w := range health {
		if w < minW {
			minW = w
		}
	}
	capW := minW * p.realmWeightMaxRatio
	if capW > 1 {
		capW = 1
	}
	for r, w := range health {
		if w > capW {
			health[r] = capW
		}
	}
	// 静态偏好（pool.realm_bias）：在动态健康因子与容量钳制**之后**叠加。
	// 顺序不能反：钳制管的是「每账号负载别把某域压垮」，bias 管的是「运维就想让
	// 这一域多吃」。反过来先乘 bias 会被 capW(=1) 吃掉，等于没配。
	if len(p.realmBias) > 0 {
		for r, w := range health {
			if b, ok := p.realmBias[r]; ok && b > 0 {
				health[r] = w * b
			}
		}
		// 偏好同样受「每账号负载」上限约束：否则 bias 配大了会把另一域彻底饿死，
		// 相当于绕过 global.enabled 那种显式开关做了硬隔离。只在比较对象不止一个时钳。
		minW = 1.0
		for _, w := range health {
			if w < minW {
				minW = w
			}
		}
		capW = minW * p.realmWeightMaxRatio
		for r, w := range health {
			if w > capW {
				health[r] = capW
			}
		}
	}
	// 权重全等时返回 nil：跨域抽签是多余的（比例本来就等于账号数），
	// 且省掉一次随机数消耗——保证「两域都满血」时选号行为与改动前逐位一致。
	first := true
	var lo, hi float64
	for _, w := range health {
		if first {
			lo, hi, first = w, w, false
			continue
		}
		if w < lo {
			lo = w
		}
		if w > hi {
			hi = w
		}
	}
	if hi-lo <= 1e-9 {
		return nil
	}
	return health
}

// pickRealmByWeightLocked 按「域质量 = 该域候选数 × 域权重」抽一个域。
//
// 质量累加天然含账号数：某域 35 个候选各贡献 w，就是 35w。返回空串表示不筛选
// （只有一个域，或权重全非正），调用方保持原候选集。
//
// 用定点 int64 抽签并与 pickWeighted 共用同一个随机源（p.randInt64N，测试可注入
// 确定性），保证「权重→抽签」的口径在本包内一致。
func (p *Pool) pickRealmByWeightLocked(cands []*entry, mul map[string]float64) string {
	const scale = 1_000_000
	mass := map[string]int64{}
	order := make([]string, 0, len(mul))
	for _, e := range cands {
		r := e.a.Realm()
		w := mul[r]
		if r == "" || w <= 0 {
			continue
		}
		wi := int64(w*scale + 0.5)
		if wi < 1 {
			wi = 1
		}
		if _, ok := mass[r]; !ok {
			order = append(order, r)
		}
		mass[r] += wi
	}
	if len(order) < 2 {
		return ""
	}
	var total int64
	for _, r := range order {
		total += mass[r]
	}
	rnd := rand.Int64N
	if p.randInt64N != nil {
		rnd = p.randInt64N
	}
	x := rnd(total)
	var acc int64
	for _, r := range order {
		acc += mass[r]
		if x < acc {
			return r
		}
	}
	return order[len(order)-1]
}

// RealmWeight 读当前 (模型, 域) 权重（对外只读，供 /status 与测试观察）。
func (p *Pool) RealmWeight(model, realm string) float64 {
	if model == "" || realm == "" {
		return 1
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.realmWeightLocked(model, realm, time.Now())
}

// NoteRealmRateLimited 记一次该账号所属域在此模型上的限流，按乘性衰减降权。
//
// 传 uid 而不是 realm：调用方（applyErrorPolicy）手里只有 uid，域由 pool 内部
// 用 auth.Realm() 解析——与 pick 里 realmOK 的取值口径完全一致。
func (p *Pool) NoteRealmRateLimited(uid, model string) {
	if uid == "" || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	// Realm() 内部取 a.mu；pick 的 realmOK 同样在 p.mu 内调用，无死锁风险。
	realm := e.a.Realm()
	if realm == "" {
		return
	}
	now := time.Now()
	w := p.realmWeightLocked(model, realm, now) // 先恢复再衰减
	w *= p.realmWeightDecay
	if w < p.realmWeightFloor {
		w = p.realmWeightFloor
	}
	p.realmWeights[realmWeightKey{model: model, realm: realm}] = &realmWeight{w: w, lastAt: now}
}

// RealmWeightView 返回当前 (模型 → 域 → 权重) 快照，供 /status 观察。
//
// 只列出低于满血（<1）的条目——满血等于「没被限流影响过」，全列出来只是噪音。
// 只读：不推进 st.lastAt（时间恢复的写路径留给 pick/NoteRealmRateLimited，
// 免得运维刷一次 /status 就把恢复节奏打乱）。
func (p *Pool) RealmWeightView() map[string]map[string]float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.realmWeights) == 0 {
		return nil
	}
	now := time.Now()
	out := map[string]map[string]float64{}
	for k, st := range p.realmWeights {
		w := st.w
		if elapsed := now.Sub(st.lastAt); elapsed > 0 {
			w += elapsed.Minutes() * p.realmWeightRecoverPerMin
			if w > 1 {
				w = 1
			}
		}
		if w >= 1 {
			continue
		}
		if out[k.model] == nil {
			out[k.model] = map[string]float64{}
		}
		out[k.model][k.realm] = w
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// pruneRealmWeightsLocked 丢掉已回满且久未更新的条目，防 map 随模型名无限增长。
// 满血条目与不存在等价（读取时返回 1），删掉不改变行为。
func (p *Pool) pruneRealmWeightsLocked(now time.Time) {
	for k, st := range p.realmWeights {
		if now.Sub(st.lastAt) > 30*time.Minute && st.w >= 1 {
			delete(p.realmWeights, k)
		}
	}
}
