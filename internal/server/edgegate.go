// edgegate.go 上游**边缘层拒绝**的 IP 级 fail-fast 状态机（任务书 waf-ip-failfast，
// 2026-09-22 由 wafip.go 泛化——原版只认 WAF 403，现同时覆盖鉴权层 401）。
//
// 背景（fork-scan-absorb T-1 / BulidH 实测 + 2026-09-22 全池降权事故）：
// 上游 APISIX 边缘层会按**出口 IP** 拒绝我们，与账号无关。已观测到两种形态：
//
//   - 403 WAF 拦截（无业务信封，HTML 页/空体）：3 个账号 1 秒内全 403；
//   - 401 鉴权拒绝（无业务信封，openresty「401 Authorization Required」页）：
//     35 个账号在同一秒内全 401，其中包含 10 秒前刚成功过的号——同一批号在
//     风暴结束后同 token 立刻 200（16 个可复现），证明是 IP 级事实而非账号故障。
//
// 两种形态**共用同一个判定窗**：它们都是「边缘层按出口 IP 拒绝我们」的证据，
// 只是状态码不同；合并计数让任一形态的短窗多号命中都能触发 IP 级 fail-fast，
// 也让「401 与 403 混排」的抖动更快被识别（不必各自凑满阈值）。
//
// 判定：短窗多号计数——edgeWindow（60s 滑动窗）内 ≥ edgeThreshold 个**不同** UID
// 接连命中边缘层拒绝 → 判定 IP 级拦截，激活至 now+edgeWindow。单号反复命中
// （账号级偶发）永不触发：只数不同号。激活期内新命中不续期（保守：不做主动探测，
// 窗口自然解除）。
//
// 归属层评估：放 server（Handler 局部）而非 pool——IP 级状态唯一消费者是
// chatCompletions 轮转循环（是否继续轮转），pool 是账号级记账层，跨 UID 语义
// 不属于任何账号；server 已有进程级状态先例 degradeGate（mu+until 同风格）。
// 账号级记账由 applyErrorPolicy 各自负责（ErrWafBlock 软冷却照常、ErrEdgeAuth
// 零惩罚），IP 级状态只改变「是否继续轮转」——协同不叠加。
// 进程内状态、重启清零（窗口 60s，重建成本极低）。
package server

import (
	"log"
	"sync"
	"time"
)

// edgeWindow IP 级判定滑动窗 + 激活时长：窗内不同账号命中边缘层拒绝达阈值即
// 判 IP 级拦截，激活同样长（到期自然解除）。var 仅供测试注入短窗（生产恒 60s，
// 任务书「如 60s 滑动窗」口径）。
var edgeWindow = 60 * time.Second

// edgeThreshold 判定阈值：窗内不同 UID 数达到该值激活。取 2——「多号」的最小
// 定义：单号反复命中永不触发（账号级偶发归各自的账号级记账管），两个不同号在
// 60s 内接连被边缘层拒（同一出口 IP）已是 IP 级证据（BulidH 实测 3 号 1s 全拦，
// 阈值 2 更早止损，少放大一次轮转）。
const edgeThreshold = 2

// edgeGate 边缘层拒绝的 IP 级 fail-fast 状态机（Handler 内嵌，零值可用）。
type edgeGate struct {
	mu    sync.Mutex
	hits  map[string]time.Time // uid → 最近一次边缘层拒绝时刻（判定窗内，惰性剪枝）
	until time.Time            // IP 级拦截激活截止；零值 = 未激活
	// shape 本次激活由哪种形态触发（观测用，WARN 里打出）。激活期内不更新
	// （与「不续期」同口径）；下次激活由新触发形态覆盖。
	shape string
}

// noteWaf 记一次某账号的 WAF 403（无业务信封）。
func (g *edgeGate) noteWaf(uid string) bool { return g.note(uid, "waf 403") }

// noteEdgeAuth 记一次某账号的边缘层鉴权拒绝（401，无业务信封）。
func (g *edgeGate) noteEdgeAuth(uid string) bool { return g.note(uid, "edge auth 401") }

// note 记一次某账号的边缘层拒绝，返回记账后 IP 级拦截是否激活（调用方据此
// fail-fast 终止轮转，优先于 rotateBackoff 退避）。
//   - 已激活（now < until）：不续期、不记账（窗口期内不重置——保守自然解除）→ true；
//   - 未激活：记 hits[uid]=now（同号重复命中覆盖不累计，判定口径是「不同号数」），
//     剪掉窗外的过期命中；不同 UID 数达 edgeThreshold → 激活到 now+edgeWindow
//     （打一条 WARN 供观测），清空判定窗（解除后需全新命中重新判定，不叠旧账）。
func (g *edgeGate) note(uid, shape string) bool {
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if now.Before(g.until) {
		return true // 激活期内新命中：不续期（自然解除语义，任务书第 3 条）
	}
	if g.hits == nil {
		g.hits = map[string]time.Time{}
	}
	g.hits[uid] = now
	for u, t := range g.hits {
		if now.Sub(t) > edgeWindow {
			delete(g.hits, u)
		}
	}
	if len(g.hits) >= edgeThreshold {
		g.until = now.Add(edgeWindow)
		g.shape = shape
		log.Printf("WARN: [server] upstream edge-level block (%s): %d accounts hit within %s, rotate fail-fast until %s", shape, len(g.hits), edgeWindow, g.until.Format(time.RFC3339))
		g.hits = map[string]time.Time{}
		return true
	}
	return false
}

// active 报告 IP 级拦截是否激活（末端错误文案区分 IP 级/账号级措辞用）。
func (g *edgeGate) active() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Now().Before(g.until)
}
