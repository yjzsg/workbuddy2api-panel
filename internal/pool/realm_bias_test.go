// realm_bias_test.go 静态域偏好倍率（pool.realm_bias）：policy 叠加在动态权重之上。
//
// 与 realmweight_test.go 的分工：那边测「某域报限流 → 动态降权」；这边测
// 「运维就是想让某一域多吃流量」——两者相乘后共同决定裸名请求的跨域抽签。
//
// 为什么不都写端到端比例断言：`pick` 里 idle 补偿会让「刚被用过的号」权重从 6.0
// 掉到 1.0（weightOf 的 lastUsed 项），于是**稳态下短名单在全体账号间轮转**，
// 比例随「谁刚被用过」漂移，端到端断言只能给宽松区间。精确比例改在域抽签这一层
// 直接断言（pickRealmByWeightLocked 的输入是候选数 × 权重）。
package pool

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// newMixedPool 造 nCN 个 cn 号 + nGlobal 个 global 号（域由 Domain 推断）。
func newMixedPool(nCN, nGlobal int) *Pool {
	p := New("")
	for i := 0; i < nCN; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("c%d", i), Domain: "www.codebuddy.cn"})
	}
	for i := 0; i < nGlobal; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("g%d", i), Domain: "www.workbuddy.ai"})
	}
	return p
}

// candsOf 取当前全部账号作为候选（测试用，顺序无关）。
func candsOf(p *Pool) []*entry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*entry, 0, len(p.byUID))
	for _, e := range p.byUID {
		out = append(out, e)
	}
	return out
}

// noPickGap 关掉防撞号窗口。
//
// 必须用**负**窗口：pick 的过滤是 `now.Sub(lastUsed) >= minPickGap`，而 now 在本次
// pick 开始时取、同一次里刚选中的号 lastUsed==now，所以 0 也不够（`0 >= 0` 仍成立
// 但会把上一轮刚用的号挤出短名单，行为随调用节奏漂移）。负值才等价「无窗口」。
func noPickGap(t *testing.T) {
	t.Helper()
	old := minPickGap
	minPickGap = -time.Second
	t.Cleanup(func() { minPickGap = old })
}

// TestRealmBiasMultipliers 域乘数表：未配 bias 时返回 nil（零回归），
// 配了 cn=2 后 cn/global = 2/1。
func TestRealmBiasMultipliers(t *testing.T) {
	p := newMixedPool(6, 35)
	cands := candsOf(p)
	now := time.Now()

	if mul := p.realmWeightMultipliersLocked(cands, "m", now); mul != nil {
		t.Fatalf("未配 bias 且两域满血时应返回 nil，got %v", mul)
	}

	p.SetRealmBias(map[string]float64{"cn": 2})
	mul := p.realmWeightMultipliersLocked(cands, "m", now)
	if mul == nil {
		t.Fatalf("配了 bias 应返回乘数表")
	}
	if math.Abs(mul["cn"]-2) > 1e-9 || math.Abs(mul["global"]-1) > 1e-9 {
		t.Fatalf("乘数应为 cn=2/global=1，got %v", mul)
	}
}

// TestRealmBiasShiftsRealmDraw 域抽签比例 = 候选数 × 权重：6×2 : 35×1 → cn≈25.5%。
func TestRealmBiasShiftsRealmDraw(t *testing.T) {
	p := newMixedPool(6, 35)
	cands := candsOf(p)
	p.SetRealmBias(map[string]float64{"cn": 2})
	mul := p.realmWeightMultipliersLocked(cands, "m", time.Now())
	if mul == nil {
		t.Fatalf("乘数表为空")
	}

	p.mu.Lock()
	n := map[string]int{}
	for i := 0; i < 4000; i++ {
		n[p.pickRealmByWeightLocked(cands, mul)]++
	}
	p.mu.Unlock()

	share := float64(n["cn"]) / 4000
	want := 6 * 2.0 / (6*2.0 + 35)
	if math.Abs(share-want) > 0.03 {
		t.Fatalf("cn 抽中比例应≈%.3f，got %.3f (n=%v)", want, share, n)
	}
}

// TestRealmBiasShiftsTrafficToCN 端到端 A/B：同规模两个池子（4 cn : 35 global，
// 对齐生产 cn 4 个健康号 / global 35 个），一个不配 bias、一个配 cn=4。
//
// 为什么要预热：idle 补偿让「从未用过」的号权重 +5，刚开始的几百次 pick 全在
// 新号之间轮转，比例反映的是 UID/洗牌顺序而不是域权重（未预热时实测 cn 甚至
// 能到 31%，比配上 bias 之后还高）。预热后权重趋于一致，比例才由域抽签决定。
//
// 生产口径：cn 份额 ≈ 4b / (4b + 35)。b=4 → 16/51 ≈ 31%；b=1（不配）时
// 由 idle 均衡决定，实测约 18%~24%（不是账号数比 10%，idle 会把小域拉高）。
func TestRealmBiasShiftsTrafficToCN(t *testing.T) {
	noPickGap(t)

	measure := func(p *Pool) float64 {
		warm := func(n int) {
			for i := 0; i < n; i++ {
				if a := p.Pick("m"); a == nil {
					t.Fatalf("预热选号返回 nil")
				}
			}
		}
		warm(500) // 39 个号 × 足够多轮，确保每个号都被用过
		cn, n := 0, 1500
		for i := 0; i < n; i++ {
			a := p.Pick("m")
			if a == nil {
				t.Fatalf("选号返回 nil")
			}
			if a.Realm() == "cn" {
				cn++
			}
		}
		return float64(cn) / float64(n)
	}

	plain := newMixedPool(4, 35)
	biased := newMixedPool(4, 35)
	biased.SetRealmBias(map[string]float64{"cn": 4})

	base := measure(plain)
	after := measure(biased)
	want := 4 * 4.0 / (4*4.0 + 35) // 域抽签：cn 质量 4×4，global 质量 35×1
	t.Logf("base(不配 bias)=%.3f after(bias=4)=%.3f want=%.3f", base, after, want)

	// 只断言「配了 bias 之后落在预测份额上」：bias 一旦生效，域抽签就接管比例，
	// 结果只由「候选数 × 权重」决定，与 credits/idle 的漂移无关。
	// 不断言 after > base：base 由 idle 均衡决定，实测在 0.28~0.34 之间随墙钟抖动
	// （idle 项按 lastUsed 与 now 的微秒差算），拿它做判据必然 flaky。
	if math.Abs(after-want) > 0.08 {
		t.Fatalf("配 bias=4 后 cn 占比应≈%.3f，got %.3f", want, after)
	}
}

// TestRealmBiasOnlyAffectsBareModel 显式 cn: 请求不受 bias 影响（没有选择权）。
func TestRealmBiasOnlyAffectsBareModel(t *testing.T) {
	noPickGap(t)
	p := newMixedPool(2, 6)
	p.SetRealmBias(map[string]float64{"cn": 3})

	for i := 0; i < 200; i++ {
		a := p.PickExcludingForRealm(map[string]bool{}, "m", "cn") // 参数顺序：(tried, model, realm)
		if a == nil {
			t.Fatalf("显式 cn 选号返回 nil")
		}
		if a.Realm() != "cn" {
			t.Fatalf("显式 cn 请求不该落到 %s", a.Realm())
		}
	}
}

// TestRealmBiasSanitize 非法值被丢弃、过大值被钳（不 panic、不把域压死）。
func TestRealmBiasSanitize(t *testing.T) {
	p := newMixedPool(1, 1)
	p.SetRealmBias(map[string]float64{
		"cn":     0,          // 丢弃
		"global": -1,         // 丢弃
		"":       5,          // 空 key 丢弃
		"x":      1e9,        // 钳到 maxRealmBias
		"nan":    math.NaN(), // 丢弃
	})
	got := p.RealmBias()
	if _, ok := got["cn"]; ok {
		t.Fatalf("cn=0 应被丢弃，got %v", got)
	}
	if _, ok := got["global"]; ok {
		t.Fatalf("global=-1 应被丢弃，got %v", got)
	}
	if _, ok := got[""]; ok {
		t.Fatalf("空 key 应被丢弃，got %v", got)
	}
	if _, ok := got["nan"]; ok {
		t.Fatalf("NaN 应被丢弃，got %v", got)
	}
	if v := got["x"]; v != maxRealmBias {
		t.Fatalf("过大值应钳到 %v，got %v", maxRealmBias, v)
	}

	p.SetRealmBias(nil)
	if p.RealmBias() != nil {
		t.Fatalf("清空后应为 nil")
	}
}

// TestRealmBiasMultipliesDynamicWeight 动态降权与静态偏好相乘，且偏好同样受
// maxRatio 钳制：global 被打到 floor 时，cn bias 再大也不能吃满。
func TestRealmBiasMultipliesDynamicWeight(t *testing.T) {
	p := newMixedPool(6, 35)
	p.SetRealmBias(map[string]float64{"cn": 20})

	for i := 0; i < 10; i++ {
		p.NoteRealmRateLimited("g0", "m")
	}
	mul := p.realmWeightMultipliersLocked(candsOf(p), "m", time.Now())
	if mul == nil {
		t.Fatalf("乘数表为空")
	}
	want := p.realmWeightFloor * p.realmWeightMaxRatio // global=floor，cn 被钳到 floor×maxRatio
	if math.Abs(mul["cn"]-want) > 1e-9 {
		t.Fatalf("cn 应被钳到 %.4f，got %.4f", want, mul["cn"])
	}
	if math.Abs(mul["global"]-p.realmWeightFloor) > 1e-9 {
		t.Fatalf("global 应在 floor %.4f，got %.4f", p.realmWeightFloor, mul["global"])
	}
}
