// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikeys"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// Config handler 依赖。
type Config struct {
	Pool     *pool.Pool
	Upstream *upstream.Client
	APIKey   string // 空 = 不鉴权（静态值；与 Live 同时给出时 Live 优先）。多密钥启用后仅作回落
	// Keys 多密钥表（nil = 退回单 api_key 语义；测试与裸用场景）。
	Keys      *apikeys.Store
	MaxRotate int // 单请求最多换号次数，默认 3
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429/限流文案软冷却基数，默认 600s（连续触发指数退避，封顶 soft_rate_max）
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m

	// Panel 管理面板 handler（可选；nil = 不挂载）。挂载在 /panel/ 前缀下，
	// 面板自带 Bearer 鉴权（同一 api_key）与内嵌静态资源，主路由只做转发。
	Panel http.Handler

	// Live 运行期可变配置（面板在线改 api_key / soft_rate / 脱敏开关时立即生效）。
	// nil 时回退静态字段（测试与裸用场景）。
	Live *livecfg.Holder

	// PromptMode "passthrough"（默认，透传客户端原始 system）/ "custom"（网关替换）。
	PromptMode string
	// PromptText custom 模式下注入的系统提示词文本（来自 config.PromptText）。
	PromptText string

	// GlobalEnabled global realm 路由开关（config global.enabled，缺省 true）。
	// handler 侧第三道闸（与 main 注入 auth 开关、upstream.GlobalEnabled 呼应）：
	// false（显式逃生门）时即便 auth realm=global 也不提供 global: 模型名
	// （modelList 不列 global 名单）。
	GlobalEnabled bool

	// Usage 逐请求用量记录器（可选；nil = 不记录）。
	// 在 recordAttempt 这一唯一汇聚点调用，因此流式/非流式、成功/失败都会计入，
	// 且与 pool 的每账号累计器同源，两条口径不会漂移。
	Usage *usage.Recorder

	// AdminEnabled 运维管理端点开关（config admin.enabled，默认 false）。
	// 关闭时 /admin/* 一律 404（而非 403——不向外暴露"这里存在管理面"）。
	AdminEnabled bool
	// RequestLog 请求指标与脱敏 JSONL 归档（可选；nil = 不记录）。
	RequestLog *reqlog.Recorder

	// RecordClientInfo 是否在请求日志里记录调用来源（客户端 IP / User-Agent）。
	// 来自 logging.request_client_info（缺省 true）；关闭时 reqlog 事件的来源字段
	// 保持为空，归档与面板都不出现来源信息。
	RecordClientInfo bool
}

// loadLive 返回当前运行期快照；Live 为 nil 时用静态字段合成。
func (h *Handler) loadLive() livecfg.Snapshot {
	if h.cfg.Live != nil {
		return h.cfg.Live.Load()
	}
	return livecfg.Snapshot{
		APIKey:           h.cfg.APIKey,
		SoftCooldown:     h.cfg.SoftCooldown,
		RecordClientInfo: h.cfg.RecordClientInfo,
	}
}

// softCooldown 返回当前生效的软冷却基数（热改优先，<=0 回退默认）。
func (h *Handler) softCooldown() time.Duration {
	if d := h.loadLive().SoftCooldown; d > 0 {
		return d
	}
	if h.cfg.SoftCooldown > 0 {
		return h.cfg.SoftCooldown
	}
	return 600 * time.Second
}

// notFoundCooldown 上游 404 的固定短冷却时长。
// 与 SoftCooldown 分流的原因：404 是上游**偶发**路径缺失，不是"本账号在限流"，
// 若共用 soft_rate（600s 起 + 指数升级），一次偶发 404 会把好账号罚 10 分钟并逐次加倍。
// 故固定 60s 防雪崩即可，不随 soft_rate 配置、也不参与软退避指数。
const notFoundCooldown = 60 * time.Second

// wafCooldownBase WAF 403 软冷却基数（任务书 P0-1 建议 60s 起；抖动 ±25% 后落
// [45s,75s]，实际进入 CooldownSoftRate 后再按 softStreak 指数、封顶 soft_rate_max）。
// 与 SoftCooldown 分流的原因：WAF 403 是 IP/指纹维频控（WAF 403 报告 §6），信号比
// 429「账号级限流」轻（账号本身健康、直连 200 实证），但比 404 重（带粘性会连环）；
// 60s 级的快速避让已足够让频控窗口滑过，指数升级由 CooldownSoftRate 既有机制接管。
// 抖动复用 backoff.go jitterDur（单一来源，不重复造轮子）。
const wafCooldownBase = 60 * time.Second

// queueCooldownDefault 排队类响应（6020 queue.waiting / 6021 queue.full）**未给出**
// data.retry_after 时的默认避让时长。
//
// 与 SoftCooldown（600s 起 + 指数升级）分流的原因：排队是上游**容量**信号
// （队列满/排队中），不是"本账号被限流"——账号本身健康，只是此刻上游接不下这个模型。
// 按账号级 600s 罚会连带把所有模型一起停掉；实测 2026-10-15 10:30–10:36 六个 CN 号
// 各吃一次 6021 → 全部账号级冷却 600s → **CN 池整体空转约 10 分钟**。
// 故改为**模型级**短避让：只让该模型在这个号上歇一会儿，其他模型照常服务。
const queueCooldownDefault = 60 * time.Second

// 容量饱和（14003 模型繁忙）时的「同号重试」预算（2026-10-20）。
//
// 上游模型全局饱和时换号毫无收益（新号同样 14003），但前缀缓存按 uid 隔离，换一次
// 就等于整段重算。实测一个 60k→100k 的会话 7 分钟内跳了 8 个号，每次命中率掉到 0–3.5%。
// 改为先在原号等一会儿再试：14003 是逐请求准入，几秒内就可能放行。
// 用 var 而非 const：测试需要把退避压到毫秒级，否则单测要等 3 秒。
var (
	maxCapacityRetries   = 2
	capacityRetryBackoff = 1500 * time.Millisecond
)

// timeoutCooldown 上游超时（响应头迟迟不来）后该 (账号, 模型) 的短时避让时长。
//
// 为什么必须有它：超时路径是"不换号、不罚号"（handler 轮转循环内的
// isUpstreamTimeout 分支），理由是同一份请求换号撞的是同一个慢上游。这个理由对
// **换号**成立，但对**罚号**不成立——零惩罚会让卡住的号状态最干净、在选号打分里
// 永远最优，于是被反复选中：实测 2026-10-15 10:26:30–10:29:20 红花(a72732ea)
// 连续 4 次各烧 120s 头超时（其间同样大小的 prompt 只要 7s），是全池唯一有 >100s
// 记录的号。给一个 60s 的模型级避让，下一次请求就会落到别的号上。
// 用模型级而非账号级：卡的是"这个号 + 这个模型"这一次调用，不该连坐其他模型。
const timeoutCooldown = 60 * time.Second

// ServiceName 网关身份标识。经 /healthz 响应体 service 字段与 X-Service 头同时透出：
// 宿主（如 workbuddy-switch 托管网关子进程）探测同端口的旧服务/其他服务时，对方即使
// 返回 2xx 也不带本标识，宿主据此可识别"假成功"。
const ServiceName = "workbuddy2api"

// dumpReqMinBytes WB2A_DUMP_REQ 调试落盘的"大请求"固定阈值（4MB）。原判断是
// 「超过 max_body_mb 上限一半」，max_body_mb 移除后改为固定值，语义不变：
// 小探针（{"input":"hi"} 之类）不落盘，避免覆盖真正要看的对话请求。
const dumpReqMinBytes = 4 << 20

// Handler 主路由。
type Handler struct {
	cfg     Config
	mux     *http.ServeMux
	degrade degradeGate
	// edgeGate 上游边缘层拒绝的 IP 级拦截状态机（fail-fast，edgegate.go）：
	// 短窗多号命中 WAF 403 / 鉴权层 401 → 激活期轮转再遇同类直接终止
	// （不放大请求量）。进程内状态、重启清零。
	edgeGate edgeGate
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 600 * time.Second // 软限流基数（连续触发按指数退避放大）
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.PromptMode == "" {
		cfg.PromptMode = "passthrough" // 缺省 passthrough：透传客户端原始 system
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	// MiniMax 原生路径兼容（2026-10-21）：New API 的「MiniMax」渠道类型不走 OpenAI 路径，
	// 而是把上游 URL 拼成 <base>/v1/text/chatcompletion_v2（new-api relay/channel/minimax/
	// relay-minimax.go:23）；其请求体是原样透传的 OpenAI 结构（ConvertOpenAIRequest 直接
	// return request），响应也交给 openai.Adaptor.DoResponse 解析（adaptor.go:136）——
	// 即除 URL 之外全是 OpenAI 语义。故这里只需认这个路径，行为与 /v1/chat/completions
	// 完全一致，不需要任何格式转换。
	// 不认这条路径时的现象：客户端在 New API 里选 MiniMax 类型/分组即 404 page not found，
	// 选 OpenAI/DeepSeek 类型则正常。
	h.mux.HandleFunc("POST /v1/text/chatcompletion_v2", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /v1/stats", h.withAuth(h.stats))
	h.mux.HandleFunc("POST /v1/stats/reset", h.withAuth(h.statsReset))
	// 运维管理端点（默认关闭，config admin.enabled 开启后生效）。
	// 路径用 {uid} 通配而非查询参数：uid 是账号身份，放进路径便于审计与直观。
	// 条件注册而非 handler 内 404（设计 supplement §2.3）：未注册的路由对未鉴权
	// 探测回 mux 默认纯文本 404、对 GET 探测无 405+Allow 头，与真 404 完全不可
	// 区分——路由一旦注册，"带 key 得 401 / GET 得 405 / JSON 信封 404" 三者都会
	// 暴露管理面存在。
	if cfg.AdminEnabled {
		h.mux.HandleFunc("POST /admin/accounts/{uid}/disable", h.withAuth(h.adminAccountDisable))
		h.mux.HandleFunc("POST /admin/accounts/{uid}/enable", h.withAuth(h.adminAccountEnable))
		h.mux.HandleFunc("POST /admin/accounts/{uid}/revive", h.withAuth(h.adminAccountRevive))
	}
	h.mux.HandleFunc("GET /healthz", h.healthz)
	if cfg.Panel != nil {
		h.mux.Handle("/panel/", cfg.Panel) // /panel → /panel/ 由 ServeMux 自动重定向
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.cfg.RequestLog != nil && r.Method == http.MethodPost && isChatCompletionPath(r.URL.Path) {
		trace := &requestTrace{id: reqlog.NewRequestID(), start: time.Now()}
		if h.loadLive().RecordClientInfo {
			trace.captureClientInfo(r)
		}
		r = r.WithContext(context.WithValue(r.Context(), requestTraceKey{}, trace))
		obs := &responseObserver{ResponseWriter: w}
		w.Header().Set("X-Request-Id", trace.id)
		h.cfg.RequestLog.Begin()
		defer func() {
			status := obs.status
			if status == 0 {
				status = http.StatusOK
			}
			h.cfg.RequestLog.Record(trace.event(status))
		}()
		h.mux.ServeHTTP(obs, r)
		return
	}
	h.mux.ServeHTTP(w, r)
}

// isChatCompletionPath 判断是否为「对话补全」入口路径。
//
// 两条路径等价（见 NewHandler 里 MiniMax 原生路径的注释）。请求日志钩子必须一视同仁，
// 否则从 New API 的 MiniMax 渠道进来的流量在 request-logs 里完全不可见，排障时会被
// 误判成「上游根本没打过来」。
func isChatCompletionPath(p string) bool {
	return p == "/v1/chat/completions" || p == "/v1/text/chatcompletion_v2"
}

// keyCtxKey context 中承载"本次请求命中的密钥"的键。
type keyCtxKey struct{}

// withKeyCtx 把命中的密钥挂到请求 context，供下游（模型白名单、用量按 key 统计）读取。
func withKeyCtx(ctx context.Context, k *apikeys.Key) context.Context {
	return context.WithValue(ctx, keyCtxKey{}, k)
}

// keyFromReq 取本次请求命中的密钥；未启用多密钥（单 api_key 裸用）时为 nil。
func keyFromReq(r *http.Request) *apikeys.Key {
	if v := r.Context().Value(keyCtxKey{}); v != nil {
		if k, ok := v.(*apikeys.Key); ok {
			return k
		}
	}
	return nil
}

// keyLogLabel 日志里的密钥标识：名称(完整 ID)，缺一项就用另一项，都缺为 "?"。
//
// 刻意不带 secret：日志会落到容器 stdout、也可能被面板/归档读走，
// 密钥本身在 keys.json 里是明文，绝不能再复制一份到日志。
func keyLogLabel(k *apikeys.Key) string {
	if k == nil {
		return "?"
	}
	name := strings.TrimSpace(k.Name)
	switch {
	case name != "" && k.ID != "":
		return name + "(" + k.ID + ")"
	case name != "":
		return name
	case k.ID != "":
		return k.ID
	}
	return "?"
}

// keyIDOf 取本次请求命中的密钥 ID；无密钥上下文时返回空串。
//
// 空串在 usage 侧是合法的"未标记"维度：引入多密钥之前的旧记录也是空串，
// 两者在面板上合并成一行，不伪造归属。
func keyIDOf(r *http.Request) string {
	if k := keyFromReq(r); k != nil {
		return k.ID
	}
	return ""
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 多密钥表优先（能拿到命中身份，供用量按 key 统计与模型白名单）；
		// Keys 为 nil 时退回旧单密钥语义：httpauth（SHA-256 + 常量时间）+ 运行期
		// 快照（面板在线改 api_key 立即生效）。
		key, ok := h.matchKey(r)
		if !ok {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, r.WithContext(withKeyCtx(r.Context(), key)))
	}
}

// matchKey 校验调用方密钥，返回命中的密钥（单密钥模式恒为 nil）与是否放行。
//
// 多密钥表为空 ⇒ MatchAuthz 返回 (nil, true)，与"api_key 为空 = 不鉴权"同语义。
func (h *Handler) matchKey(r *http.Request) (*apikeys.Key, bool) {
	if h.cfg.Keys != nil {
		return h.cfg.Keys.MatchAuthz(r.Header.Get("Authorization"))
	}
	if httpauth.VerifyBearer(r, h.loadLive().APIKey) {
		return nil, true
	}
	return nil, false
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	// 用 ServableNow 判定：healthy>0 但全占满在途时 chat 会 503，探活必须同口径，
	// 否则负载均衡器会把流量持续打进无法受理的实例。
	status := http.StatusOK
	if !h.cfg.Pool.ServableNow() {
		status = http.StatusServiceUnavailable
	}
	// realm_servable 域可服务维度：不改判活语义（存在性探活保持不变），
	// 只新增 CN/global 各自可达性供双域部署运维观察（任一域不可用单独告警）。
	realmServable := map[string]bool{
		"cn":     h.cfg.Pool.ServableForRealm("cn"),
		"global": h.cfg.Pool.ServableForRealm("global"),
	}
	// 恒无鉴权（负载均衡/编排探活只需 2xx/503 语义），身份靠 service 字段 + X-Service 头双保险。
	w.Header().Set("X-Service", ServiceName)
	writeJSON(w, status, map[string]any{
		"healthy":        healthy,
		"total":          total,
		"service":        ServiceName,
		"realm_servable": realmServable,
	})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	// cost_explore 探索台账（issue #136 §5 可观测性）：累计探索事件数 + 各
	// (域, 模型) 的最近探索时刻（键 "realm|model"）。与 accounts[].model_costs
	// 行对照即可读出「探索→毕业」全链路（单一事实来源，不做双表示）。零回归只增键。
	exploreEvents, exploreLast := h.cfg.Pool.CostExploreStatus()
	// realm_totals 按域分组的计数汇总（双 realm 并存时运维一眼看到各域可用性）：
	// 只新增字段，既有 total/healthy/cooling/disabled/in_flight_full 汇总键不变（零回归）。
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":       h.cfg.Pool.List(),
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
		"realm_totals": map[string]map[string]int{
			"cn":     countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("cn")),
			"global": countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("global")),
		},
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
		// model_locks 当前有未过期模型级限流的 (域, 模型) 全清单：账号池视图回答
		// 「哪些号不能用」，本键回答「哪些模型不能用、锁了几个号、还要锁多久」。
		// 与 ModelBlocked（请求失败时的单模型判定）互补；无锁时为 null。零回归只增键。
		"model_locks": h.cfg.Pool.ModelLockView(),
		// credit_floor 生效的积分保底值（0 = 关闭）。与 accounts[].credits +
		// model_costs 对照即可判定「某号为何对某模型不出票」。零值也显式写出
		// （运维口径：缺失会让人误以为没记录）。
		"credit_floor": h.cfg.Pool.CreditFloor(),
		// cost_explore 事件与 per-model 时间戳（时间值由 encoding/json 写 RFC3339）。
		"cost_explore": map[string]any{
			"events_total": exploreEvents,
			"per_model":    exploreLast,
		},
		// realm_weights 按 (模型, 域) 的动态权重（realmweight.go）：某域报限流就降权，
		// 裸名请求下次选号优先用高权重域。只列低于满血（<1）的条目；全满时为 null。
		// 与 realm_totals 对照即可判断「global 是不是正被限流拖累、cn 有没有余量」。
		"realm_weights": h.cfg.Pool.RealmWeightView(),
	})
}

// countsMapFrom 把 CountsDetailed 五元组编码为 /status realm_totals 的字段对象。
func countsMapFrom(total, healthy, cooling, disabled, inFlightFull int) map[string]int {
	return map[string]int{
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
	}
}

// ---- 面板层贴回（P4-0 层 4）----

// 静态 CN 模型表（api-reference §5，动态接口失败时的回退）。
var staticModels = []map[string]any{
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "glm-5.1", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "glm-5v-turbo", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "kimi-k2.7", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "minimax-m3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3-preview", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3-preview-agent", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "deepseek-v4-pro", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "deepseek-v4-flash", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
}

// dynamicModelsCache 动态模型缓存。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

// models 返回模型列表：纯动态（缓存 1h），失败/无号返回空列表（无静态兜底）。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	// 裸名（realm-agnostic）条目先追加，再过密钥白名单：
	// 无白名单的密钥（New API 中转在用的默认密钥）因此能选到裸名模型，
	// 配合 resolveModel 的 realm="" 走全池粘合；只列带域前缀白名单的密钥
	// 不会被裸名条目放行（AllowsModel 精确匹配 full），域锁定不受影响。
	list := appendAnyRealmAliases(h.modelList())
	// 密钥白名单：只列出该密钥可用的模型，避免客户端拿到必然 403 的名字。
	// 白名单为空 = 不限制，此时直接透传（零额外开销）。
	apiKey := keyFromReq(r)
	if apiKey != nil && len(apiKey.AllowedModels) > 0 {
		filtered := make([]map[string]any, 0, len(list))
		for _, m := range list {
			id, _ := m["id"].(string)
			realm, bare := resolveModel(id)
			if apiKey.AllowsModel(realm, bare) {
				filtered = append(filtered, m)
			}
		}
		list = filtered
	}
	// 密钥别名（多密钥特性）：客户端就是用别名调用的，不列出来它就不知道能用什么名字。
	// 只列「别名目标本身在该密钥白名单内」的 —— 否则会广告一个必然 403 的名字。
	if apiKey != nil && len(apiKey.ModelMap) > 0 {
		seen := make(map[string]bool, len(list))
		for _, m := range list {
			if id, _ := m["id"].(string); id != "" {
				seen[id] = true
			}
		}
		aliases := make([]string, 0, len(apiKey.ModelMap))
		for a := range apiKey.ModelMap {
			aliases = append(aliases, a)
		}
		sort.Strings(aliases) // map 迭代序随机，排序保 /v1/models 输出稳定
		for _, a := range aliases {
			if seen[a] {
				continue
			}
			rr, bb := resolveModel(apiKey.ModelMap[a])
			if len(apiKey.AllowedModels) > 0 && !apiKey.AllowsModel(rr, bb) {
				continue
			}
			list = append(list, map[string]any{
				"id":       a,
				"object":   "model",
				"owned_by": "wb2api-alias",
				"alias_of": apiKey.ModelMap[a],
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   list,
	})
}

// globalModels 国际版（global realm）模型名名单（PLAN §7.2 附录 21 名）——已删。
// 纯动态化后 handler 不再持有任何静态名单：无 global 账号 / 探测失败 → 空列表。

// fmtCreditsPrefix 从上游 credits 原文提取倍率并格式化为 "[x0.05 credit]"。
// 上游格式不统一："x0.05 credits" / "x0.29" / "x0.00 credits" 等，
// 统一提取 x数字 部分，去 "credits" 后缀。
func fmtCreditsPrefix(raw string) string {
	s := strings.TrimSpace(raw)
	// 去掉 "credits" 后缀
	s = strings.TrimSuffix(s, "credits")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return "[" + s + " credit]"
}

// applyModelInfoFields 把上游模型对象全字段（ModelInfo）按「空值省略」写出规则
// 合入 /v1/models 条目：name/description/credits/tags/vendor/能力旗标/
// max_allowed_size/reasoning_effort/reasoning_summary。CN 动态分支与 global
// 探测命中分支共用（两域模型对象同构），保证输出字段集一致。
// 不覆盖 id/object/created/owned_by 及调用方先前写好的基础字段；上游未下发的
// 字段（零值）整体省略——不编造。
func applyModelInfoFields(entry map[string]any, mi upstream.ModelInfo) map[string]any {
	if mi.Name != "" {
		entry["name"] = mi.Name
	}
	if mi.Description != "" {
		// 积分倍率前缀：从 "x0.05 credits" / "x0.29" 等格式提取纯数字，
		// 统一为 "[x0.05 credit]" 前缀拼入 description，方便下游面板直接展示。
		if mi.Credits != "" {
			entry["description"] = fmtCreditsPrefix(mi.Credits) + " " + mi.Description
		} else {
			entry["description"] = mi.Description // descriptionZh 中文描述
		}
	}
	if mi.Credits != "" {
		entry["credits"] = mi.Credits // 积分倍率原文（如 "x0.05"），仅展示
	}
	if len(mi.Tags) > 0 {
		entry["tags"] = mi.Tags
	}
	if mi.Vendor != "" {
		entry["vendor"] = mi.Vendor
	}
	if mi.IsDefault {
		entry["is_default"] = true
	}
	if mi.SupportsImages {
		entry["supports_images"] = true // 多模态能力透出
	}
	if mi.SupportsReasoning {
		entry["supports_reasoning"] = true
	}
	if mi.SupportsToolCall {
		entry["supports_tool_call"] = true
	}
	if mi.OnlyReasoning {
		entry["only_reasoning"] = true
	}
	if mi.MaxAllowedSize > 0 {
		entry["max_allowed_size"] = mi.MaxAllowedSize
	}
	if mi.ReasoningEffort != "" {
		entry["reasoning_effort"] = mi.ReasoningEffort
	}
	if mi.ReasoningSummary != "" {
		entry["reasoning_summary"] = mi.ReasoningSummary
	}
	return entry
}

// addListingCompat 补上第三方模型目录客户端要读的能力字段（上游 issue #156：Magpie）。
// [移植自根上游 b262660 / 上游 68c08d13]
//
// 它们不看我们自己的 supports_images / reasoning_supported_efforts，只看
// `modalities.input` 里有没有 "image"、以及 `supported_reasoning_levels` 里每项的
// `effort`。我们的字段是既有约定（面板与别的客户端在用），所以这里**只增不改**：
// 两边同时输出，谁都能读。模型 ID 上的 cn:/global: 前缀保持不变——它区分版本，
// 不能为了目录兜底而抹掉。
func addListingCompat(entry map[string]any) map[string]any {
	if entry["supports_images"] == true {
		entry["modalities"] = map[string]any{"input": []string{"text", "image"}}
	}
	var efforts []string
	switch raw := entry["reasoning_supported_efforts"].(type) {
	case []string:
		efforts = raw
	case []any:
		for _, v := range raw {
			if s, ok := v.(string); ok {
				efforts = append(efforts, s)
			}
		}
	}
	if len(efforts) > 0 {
		levels := make([]map[string]any, 0, len(efforts))
		for _, e := range efforts {
			levels = append(levels, map[string]any{"effort": e})
		}
		entry["supported_reasoning_levels"] = levels
	}
	return entry
}

// modelList 动态获取模型列表并包装成 OpenAI 格式（含 context_length）。
// CN 模型输出统一加 "cn:" 前缀（gateway 路由协议，与 resolveModel 对称）。
// 纯动态：动态拉取失败/无号 → 该域空列表，无静态兜底；
// global.enabled=false（显式逃生门）时只列 CN（global 名单不出现）。
func (h *Handler) modelList() []map[string]any {
	out := make([]map[string]any, 0, len(staticModels)+32)
	infos := h.fetchDynamicModels()
	if len(infos) == 0 {
		// 面板层兜底：动态拉取失败（负缓存期/无可用号）→ 静态 CN 表，
		// 客户端不至于拿空模型列表（可用性优先于"纯动态"洁癖）。
		for _, m := range staticModels {
			e := make(map[string]any, len(m)+1)
			for k, v := range m {
				e[k] = v
			}
			if id, ok := m["id"].(string); ok {
				e["id"] = "cn:" + id
			}
			out = append(out, e)
		}
	}
	for _, mi := range infos {
		entry := map[string]any{
			"id":       "cn:" + mi.ID,
			"object":   "model",
			"created":  1753600000,
			"owned_by": "workbuddy",
		}
		// context_length / max_output_tokens 四级查找（upstream.context_catalog +
		// model_catalog）：上游动态值（maxInputTokens/maxOutputTokens）权威 → 静态
		// 种子表 → model.json 本地缓存 → models.dev 按需拉取（异步不阻塞本次响应，
		// 拉到后写 model.json 供下次命中）→ 1M 兜底 / max_output_tokens 省略。
		// 上游零值不再透出假 131072（误导 Codex/ZCode 等按 context_length 提前
		// 截断、白白丢上下文）。
		entry["context_length"] = upstream.ContextWindowListingV4(mi.ID, mi.ContextWindow, h.cfg.Upstream.HTTP)
		if mo, ok := upstream.MaxOutputTokensListingV4(mi.ID, mi.MaxTokens, h.cfg.Upstream.HTTP); ok {
			entry["max_output_tokens"] = mo
		}
		// 上游模型对象全字段透出（name/描述/标签/倍率/能力旗标等，空值省略）。
		entry = applyModelInfoFields(entry, mi)
		// 面板层：思考可关能力透出（reasoning.canDisableThinking，客户端渲染 off 档）。
		if mi.SupportsReasoning {
			entry["can_disable_thinking"] = mi.CanDisableThinking
		}
		// P0：effort 能力透出——远端 supportedEfforts 权威，缺失落到 CN 静态兜底表
		// （issue #84 客户端可发现档位，不再盲传）。无档位→省略字段（非空数组）。
		if efforts, def := upstream.EffortListing("cn", mi.ID, mi.Efforts, mi.DefaultEffort); efforts != nil {
			entry["reasoning_supported_efforts"] = efforts
			if def != "" {
				entry["reasoning_default_effort"] = def
			}
		}
		out = append(out, addListingCompat(entry))
	}
	// global 模型名单：仅 GlobalEnabled=true 时列出（逃生门）。
	// 名单 = 探测结果（fetchGlobalModels 纯动态，失败/无号 → 空）；无 global 账号时
	// 空名单且零上游调用。
	if h.cfg.GlobalEnabled {
		// global 域 effort 能力三级查找：探测下发桶（权威）→ 静态兜底表 → 省略。
		// 先 fetchGlobalModels（内部探测并落 effort 桶），再按 id 取快照。
		globalIDs, globalAccount := h.fetchGlobalModels()
		if len(globalIDs) == 0 {
			// 面板层兜底：无 global 账号/探测失败 → 静态名单（仅 ID，元数据由四级链补齐），
			// 零上游调用；有账号时探测结果权威（本分支不执行）。
			globalIDs = upstream.GlobalModelNames
		}
		// 探测对象形态的全字段条目（与 fetchGlobalModels 共享同一次探测缓存）：
		// 命中 id 才透出富字段；窄表/失败 → nil，按裸 ID 条目输出（不编造字段）。
		// globalAccount 为 nil（无 global 号）时返回 nil，跳过富字段映射。
		globalInfos := map[string]upstream.ModelInfo{}
		for _, mi := range h.cfg.Upstream.FetchGlobalModelInfos(globalAccount) {
			globalInfos[mi.ID] = mi
		}
		globalEfforts, globalDefaults := h.cfg.Upstream.GlobalEffortSnapshot()
		for _, id := range globalIDs {
			entry := map[string]any{
				"id":       "global:" + id,
				"object":   "model",
				"created":  1753600000,
				"owned_by": "workbuddy",
			}
			// context_length / max_output_tokens 四级查找（upstream.model_catalog，
			// 与 CN 动态分支同口径）：探测富条目真实值权威 → 静态种子表 →
			// model.json 缓存 → models.dev 按需拉取（异步）→ 1M 兜底/省略。
			// 裸 ID 条目（窄表探测）也经种子表补齐，不再裸 131072。
			var remoteCtx, remoteOut int64
			if mi, ok := globalInfos[id]; ok {
				entry = applyModelInfoFields(entry, mi)
				if mi.SupportsReasoning {
					entry["can_disable_thinking"] = mi.CanDisableThinking
				}
				remoteCtx, remoteOut = mi.ContextWindow, mi.MaxTokens
			}
			entry["context_length"] = upstream.ContextWindowListingV4(id, remoteCtx, h.cfg.Upstream.HTTP)
			if mo, ok := upstream.MaxOutputTokensListingV4(id, remoteOut, h.cfg.Upstream.HTTP); ok {
				entry["max_output_tokens"] = mo
			}
			if efforts, def := upstream.EffortListing("global", id, globalEfforts[id], globalDefaults[id]); efforts != nil {
				entry["reasoning_supported_efforts"] = efforts
				if def != "" {
					entry["reasoning_default_effort"] = def
				}
			}
			out = append(out, addListingCompat(entry))
		}
	}
	return out
}

// fetchGlobalModels 返回 global 模型名单（纯动态探测结果）及被探测账号。
// 与 fetchDynamicModels（CN 侧）同语义不同归位：缓存/失败回落封在 upstream.FetchGlobalModels
// （内部 1h + 5min 负缓存）。本方法只负责"何时探测"：
//   - 池中无 global 账号 → 空名单 + nil 账号（不发起上游调用）；
//   - 有 global 账号 → 单账号 Pick（global 域谓词），交 upstream 探测。
//
// 返回的 acct 供调用方在同一账号上取富 ModelInfo（FetchGlobalModelInfos 与
// FetchGlobalModels 共享缓存，不会触发第二次上游探测）。
// GlobalEnabled=false 时 modelList 已不进入本分支（逃生门在调用方 gate）。
func (h *Handler) fetchGlobalModels() ([]string, *auth.Auth) {
	acct := h.cfg.Pool.PickExcludingForRealm(nil, "", "global")
	if acct == nil {
		return nil, nil
	}
	return h.cfg.Upstream.FetchGlobalModels(acct), acct
}

// applyKeyThinking 按密钥的思考开关把意图落成请求体的 thinking.type。
//
// mode："" = 不动（调用方已过滤）；"on" = enabled；"off" = disabled。
// 只对 deepseek 系模型生效——其他模型的思考开关语义不同（qwen 用 enable_thinking 等），
// 乱塞 thinking 对象有风险；与 upstream.injectThinking 的闸同口径（IsDeepSeekModel）。
//
// 为什么落成 thinking.type 就够了：upstream.injectThinking 显式 type 优先 —— "disabled"
// 走它既有的"不注入 + 删 effort 档"分支（实测思考帧归零），"enabled" 走"补默认档"分支。
func applyKeyThinking(body []byte, model, mode string) []byte {
	if len(body) == 0 || mode == "" || !upstream.IsDeepSeekModel(model) {
		return body
	}
	typ := "enabled"
	if mode == "off" {
		typ = "disabled"
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body // 不可解析时原样返回（不二次错误化，与 rewriteModel 同纪律）
	}
	obj["thinking"] = map[string]any{"type": typ}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// rewriteModel 把 outbound chat body 的 model 字段替换为 bare（保留其余字段原样）。
// 仅当 bare != 原 model 时由 chatCompletions 调用；body 不可解析时原样返回（不二次错误化）。
func rewriteModel(body []byte, bare string) []byte {
	if len(body) == 0 || bare == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if cur, ok := obj["model"].(string); !ok || cur == bare {
		return body
	}
	obj["model"] = bare
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// costModelName 成本账本记账用的模型名：兄弟模型兜底成功时记**实际服务**的模型，
// 否则记客户端请求的模型。
//
// 为什么必须区分（2026-10-20 实测）：-sg 是收费模型（x0.03），若把它的 credit
// 记到请求模型 flash 名下，pool 会认为「flash 在这个号上不再免费」，把成本档位
// 从 tier0（实测免费，选号最优先）翻成 tier2（实测收费，硬过滤垫底）。
// 当天 3 小时内 24/42 个账号被这样误标，兜底越多池子越窄。
func costModelName(bareModel, servedSibling string) string {
	if servedSibling != "" {
		return servedSibling
	}
	return bareModel
}

// fetchDynamicModels 从池中任一健康 CN 账号拉模型列表（含 contextWindow/maxTokens），缓存 1h。
// 拉取失败记录时间戳进入 5min 负缓存，冷却期内直接返回 nil（纯动态，无静态表兜底），
// 避免反复打上游。
// 只从 CN realm 账号拉取（PickExcludingForRealm(nil,"","cn")）：全局账号的模型列表
// 未必与 CN 一致，动态模型表只服务 CN 前缀（global 走独立探测）。
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	// 失败负缓存：冷却期内不再请求上游。
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	acct := h.cfg.Pool.PickExcludingForRealm(nil, "", "cn")
	if acct == nil {
		return nil
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		// 拉取失败只进负缓存（5min lastFail），不 NoteError（P1-6/发现 6）：
		// NoteError 喂的是 chat 熔断器，models 端点偶发 5xx 跨界惩罚 chat 通道
		// 健康的账号；models 拉取失败 ≠ 账号 chat 不可用。
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{} // 成功则清空负缓存
	dynamicModelsCache.Unlock()
	return infos
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	// 请求体无大小上限（max_body_mb 已移除）：完整读入，超限类问题交由上游自然返回
	// 错误（其响应经既有错误分类链路透出，信息量更大）。#41 的截断防御语义保留在
	// 读错误路径——移除预拦截后，截断只可能来自客户端自己断流，读 body 出错就地 400，
	// 不把半截 JSON 喂上游 unmarshal 报 unexpected EOF 冤枉罚号。
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	// 调试开关：设置 WB2A_DUMP_REQ 即把上游侧收到的原始请求体落盘，供离线二分定位指纹命中行。
	// 仅在排查上游指纹拦截时开启；不设置时零开销、不落盘。
	// 只落"大请求"（≥4MB 固定阈值，原 max_body_mb/2 语义的接替）：小探针
	// （{"input":"hi"} 之类）会覆盖掉真正要看的对话请求。
	if os.Getenv("WB2A_DUMP_REQ") != "" && len(body) >= dumpReqMinBytes {
		if err := os.WriteFile("/app/data/last_request.json", body, 0o600); err != nil {
			log.Printf("ERR: [server] dump req: %v", err)
		}
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)

	// 工具名名单：上游在没有 tools 的请求里会把工具调用吐成原生标记文本
	// （见 upstream/dsml.go），修复层用它做严格判定。名单来自「本请求声明的 tools」
	// 与「会话历史里出现过的工具名」两个来源——实测 tools 声明会在中转环节丢失，
	// 只认前者会让修复层在最需要它的场景下失效。
	// 在提示词/模型名改写之前取（那些改写不动 tools / messages[].tool_calls 子树）。
	declaredTools := upstream.ToolNameAllowlist(body)

	// realm 前缀解析（D6）：model 名可能带 "[realm:]" 前缀。剥出 realm + bareModel，
	// bareModel 用于选号/粘性/账本/出站 body 重写（前缀是网关侧路由协议，上游只认裸名）。
	// 裸名 → ("cn", 原串)，CN 现状零回归。
	// 密钥级模型别名（多密钥特性）：客户端发来的名字 → 真实模型名（可带域）。
	// **必须在 resolveModel 之前**：别名可能带域前缀，改写后才是真正的路由目标。
	// 典型场景：客户端只会发 `deepseek-v4.1-flash`（裸名 ⇒ 默认 CN 域），而该密钥要的
	// 是 `global:deepseek-v4.1-flash`（国际版号池）。用户控制不了客户端发什么名字，
	// 所以在密钥这一层做翻译。
	apiKey := keyFromReq(r)
	if apiKey != nil {
		if real, hit := apiKey.ModelAlias(peek.Model); hit {
			peek.Model = real
		}
	}
	realm, bareModel := resolveModel(peek.Model)

	// 密钥模型白名单（多密钥特性）：命中密钥限定了模型范围时，名单外的请求直接 403，
	// 既不消耗上游调用也不参与选号。单密钥/裸用场景 key 为 nil ⇒ 不限制。
	// 刻意放在 modelRate 查询之前：被白名单拒绝的请求不该产生任何上游/池副作用。
	// 注意白名单比对的是**别名改写后**的名字（面板勾的也是真实模型名）。
	if apiKey != nil && !apiKey.AllowsModel(realm, bareModel) {
		// 这条拒绝发生在 newChatStat 之前（不消耗上游、不进流水行），所以必须在这里
		// 自己打一行——否则日志里**完全看不到**这类请求。
		// 2026-10-20 实案：New API 渠道填错密钥，minimax 一律 403 静默，
		// grep 日志只有别处的 200，误判成「请求根本没到网关」，多绕了一圈。
		// 只打模型/密钥名/IP，绝不打 secret。
		src := "-"
		if tr := requestTraceFrom(r); tr != nil && tr.clientIP != "" {
			src = tr.clientIP
		}
		log.Printf("WARN: [server] request rejected before dispatch: model=%s key=%s reason=model_not_allowed src=%s",
			peek.Model, keyLogLabel(apiKey), src)
		writeOpenAIError(w, http.StatusForbidden, "model_not_allowed",
			"model "+peek.Model+" is not allowed for this API key")
		return
	}
	modelRate := ""
	if h.cfg.Upstream != nil {
		modelRate = h.cfg.Upstream.ModelRate(realm, bareModel)
	}

	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	st := newChatStat(time.Now(), body, peek.Stream)
	// 日志/统计的模型列用**别名改写后**的名字（= 实际路由目标）。newChatStat 取的是
	// 请求体里的原名（客户端发的别名），不改的话日志与选号/账本口径不一致，
	// 排查时分不清这次到底走了哪条池子。
	st.model = peek.Model
	if tr := requestTraceFrom(r); tr != nil {
		tr.stat = st
		// 来源在 ServeHTTP 入口采集（此时才知道开关与请求头），此处转交给统计对象，
		// 让 stdout 流水行与归档事件共用同一份来源值，两处不会漂移。
		st.clientIP, st.userAgent = tr.clientIP, tr.userAgent
	}
	defer st.done()

	tried := map[string]bool{}
	var lastErr error
	// modelBlock 记录本次是否因「模型级冷却」而选不到号（下方 acct==nil 分支填充）。
	// 默认零值 Blocked=false = 按既有的"没有可用账号"口径报错。
	var modelBlock pool.ModelBlockStatus

	// 会话粘性：从请求体提取会话键并解析绑定号（找不到/无效则 stickyUID 为空，走普通轮换）。
	// 按模型解析：同一个会话可能换模型，绑定号若在当前模型上被 6004 限额（对其他模型
	// 仍可用），必须重分配——否则会被钉在这个号上反复失败。
	// 提取与下方会话头族的聚合键共用同一结果，故**不受粘性开关影响**：粘性未启用
	// （Session==nil）时聚合键仍应是会话级，而不是退化成轮级。
	sessKey := session.ExtractKey(body)
	if sessKey == "" {
		// 一次性探测：客户端到底发了什么键（只记键名）。用于判断是否需要扩展
		// session.ExtractKey 的识别列表（见 ProbeMissingKey 注释）。
		session.ProbeMissingKey(body)
	}
	// stickyKey 是**粘性专用**键，与 sessKey（会话头族聚合用）分开：sessKey 为空时
	// （OpenAI 兼容客户端——dsh / Codex 等既无 conversationId 也无 metadata）用首条
	// user 消息派生会话级 fallback 键，使粘性仍能生效。
	// 不能直接改 sessKey：那会连带改变上游头族 RequestIDForKey 的聚合语义
	// （会话级 vs 轮级兜底），属于另一条链路的契约。
	// 【2026-09-19 同步上游 8058019+10eefa8】替换面板层 deriveKey 兜底——原实现塞在
	// ExtractKey 内部返回，会连带污染头族聚合语义；上游 stickyKey/sessKey 分离更正确。
	stickyKey := sessKey
	if stickyKey == "" {
		// header 兜底（2026-10-14）：Claude Code / Codex 把会话 ID 放在 header，
		// 经中转层转发时 body 标识可能已被剥掉。优先于启发式兜底。
		stickyKey = session.SessionIDFromHeader(r.Header)
	}
	if stickyKey == "" {
		stickyKey = session.StickyFallbackKey(body)
	}
	// 调试开关 WB2A_DBG_STICKY：记录粘性键的**来源与指纹**，用于诊断中转层
	// （New API 等）洗掉会话标识导致逐请求换号。不设置时零开销。
	// 隐私边界见 sticky_debug.go 文件头注释。
	dbgSticky := os.Getenv("WB2A_DBG_STICKY") != ""
	dbgSrc := ""
	if dbgSticky {
		switch {
		case sessKey != "":
			dbgSrc = "sessKey"
		case session.SessionIDFromHeader(r.Header) != "":
			dbgSrc = "header"
		case stickyKey != "":
			dbgSrc = "fallback"
		default:
			dbgSrc = "空"
		}
	}
	stickyUID := ""
	if h.cfg.Session != nil && stickyKey != "" {
		// 传给 ResolveForModel 的是**完整**模型名（peek.Model，含 realm 前缀）。
		// 粘性命中校验走 injected AvailableForModel 闭包 → 闭包内部 resolveModel 剥前缀
		// 得 realm+bare，再按 realm 过滤可用集合。若传已剥前缀的 bareModel，闭包对裸名
		// 恒剥出 realm=cn，跨 realm 粘性会话会被错误钉回 CN 集合；完整前缀才能让
		// 闭包正确过滤到 global 集合（见 cmd/server/wiring.go realmAwareAvailableForModel）。
		// 模型名也参与成本账本与选号过滤，不能用 "-" 占位污染模型键。
		if uid, ok := h.cfg.Session.ResolveForModel(stickyKey, peek.Model); ok {
			stickyUID = uid
		}
	}
	// 粘性键诊断（仅 WB2A_DBG_STICKY）：hit 判据最关键——
	// hit=false 说明 ResolveForModel 认为绑定号在本模型/本 realm 不可用而重新
	// 分配，这就是"同一会话被拆散到多个账号"的直接证据。
	if dbgSticky {
		log.Printf("DBG: [sticky] src=%s fp=%s model=%s hit=%v uid=%s msgs=%d tools=%d first=%s",
			dbgSrc, stickyFingerprint(stickyKey), peek.Model, stickyUID != "",
			logfmt.Label(stickyUID, ""), countMessages(body), countTools(body),
			firstUserSnippet(body))
	}

	// 轮级聚合键：按 body 里最后一条 user 消息派生（同轮内所有上游调用同键，
	// 换 user 消息换键）。#170 起带会话键的客户端也统一走轮级（与官方桌面 CLI 的
	// X-Conversation-Request-ID 轮级语义对齐），故不再限 sessKey=="" 才计算；
	// sessKey 由调用侧以复合键方式入键（防不同会话同轮文本互撞）。
	// 必须在下方 prompt.Rewrite / rewriteModel 之前取——改写会动 messages 内容。
	turnKey := session.TurnKey(body)

	// gateway_hint 判定所需的请求形态（image_url part）：在改写前取（与 turnKey
	// 同理）。11133「模型不支持图片」指向的前提。
	reqHasImage := hasImagePart(body)

	// 在途租约：成功选中即占名额；函数出口（含成功 return 与 panic）统一释放。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	// unbindSticky 解绑当前会话粘性号（stickyUID 非空时）。供「粘性号不可用/被抢」与 fail 共用。
	// 幂等：stickyUID 已空则空操作；不会误解绑其他轮的绑定。仅当 Session != nil 时 stickyUID 才会非空。
	unbindSticky := func() {
		if stickyUID != "" {
			h.cfg.Session.Unbind(stickyKey)
			stickyUID = ""
		}
	}
	// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			unbindSticky()
		}
	}
	// ttfb 首 token 等待（仅流式有观测；非流式传 0 = 无观测，速率不扣减）。
	// 显式入参而不是读 st.ttfb：后者在流式分支里是**调用之后**才赋值的，
	// 靠顺序传递会让将来重排代码时静默把速率算回旧的错口径。
	recordAttempt := func(uid string, delta pool.TokenUsageDelta, credit float64, hasCredit bool, started time.Time, ttfb time.Duration) {
		st.attempts++
		if delta.HasPromptTokens {
			st.promptTokens = delta.PromptTokens
		}
		if delta.HasCompletionTokens {
			st.completionTokens = delta.CompletionTokens
		}
		if delta.HasTotalTokens {
			st.totalTokens = delta.TotalTokens
		} else if delta.HasPromptTokens || delta.HasCompletionTokens {
			st.totalTokens = st.promptTokens + st.completionTokens
		}
		delta.Model = bareModel
		if delta.Model == "" {
			delta.Model = peek.Model
		}
		latency := time.Since(started)
		latencyMs := latency.Milliseconds()
		if latencyMs < 1 {
			latencyMs = 1
		}
		delta.HasLatencyMs = true
		delta.LatencyMs = latencyMs

		// HasCompletionTokens 是「本次有没有 token 观测」的独立判断，与速率怎么算
		// 无关，所以留在调用点；速率本身的边界（TTFB 缺失/超界）收在 tokensPerSecond。
		if delta.HasCompletionTokens {
			if tps, ok := tokensPerSecond(delta.CompletionTokens,
				time.Duration(latencyMs)*time.Millisecond, ttfb); ok {
				delta.HasTokensPerSecond = true
				delta.TokensPerSecond = tps
			}
		}
		h.cfg.Pool.RecordTokenUsage(uid, delta)

		// 用量时序记录。ok 以「上游是否给了 usage」判定：空 delta 意味着这次尝试
		// 没拿到任何 token 统计（传输错误 / >=400 / 解析失败），计为失败尝试。
		// 失败也计入请求数——否则重试放大在「用量」视图里看不见。
		if h.cfg.Usage != nil {
			realm := "cn"
			if a, ok := h.cfg.Pool.Status(uid); ok && a.Realm != "" {
				realm = a.Realm
			}
			h.cfg.Usage.Add(time.Now(), realm, uid, delta.Model, keyIDOf(r), usage.Delta{
				PromptTokens:     delta.PromptTokens,
				HasPromptTokens:  delta.HasPromptTokens,
				CompletionTokens: delta.CompletionTokens,
				HasCompletion:    delta.HasCompletionTokens,
				TotalTokens:      delta.TotalTokens,
				HasTotal:         delta.HasTotalTokens,
				Credit:           credit,
				HasCredit:        hasCredit,
				HasCacheTokens:   st.hasCache,
				ModelRate:        modelRate,
				LatencyMs:        delta.LatencyMs,
				HasLatency:       delta.HasLatencyMs,
				TokensPerSecond:  delta.TokensPerSecond,
				HasTPS:           delta.HasTokensPerSecond,
				// 缓存三段：与 token 同源（同一帧 usage），在此一并落盘。
				CacheHitTokens:   delta.CacheHitTokens,
				CacheMissTokens:  delta.CacheMissTokens,
				CacheWriteTokens: delta.CacheWriteTokens,
			}, delta.HasTotalTokens || delta.HasCompletionTokens || delta.HasPromptTokens)
		}
	}

	// 系统提示词改写（出站前、轮转前；每个请求一次）。
	//   - custom：用自有提示词替换客户端 system/developer（从源头消灭 system 指纹误报）。
	//   - append：开头连续 system/developer 块后插自有提示词，既有消息逐字不动
	//     （客户端项目规范/工具约定与网关提示词并用，issue #129）。
	//   - passthrough + 降级期：换 Degraded 中性提示词直达，不再先撞 400。
	//   - passthrough / append 非降级期：透传客户端原始 system（append 则再插一条网关 system）。
	// 降级裁决：append 在降级期退化为 replace（Rewrite(Degraded)）——append 带
	// 指纹原文重试是确定性再撞墙，replace 是一次性最小抢救（issue #129 设计 §4）。
	degradedApplied := false
	if h.cfg.PromptMode == "custom" && h.cfg.PromptText != "" {
		body = prompt.Rewrite(body, h.cfg.PromptText)
	} else if h.cfg.PromptMode == "append" && h.cfg.PromptText != "" && !h.degrade.Active() {
		body = prompt.Append(body, h.cfg.PromptText)
	} else if (h.cfg.PromptMode == "passthrough" || h.cfg.PromptMode == "append") && h.degrade.Active() {
		body = prompt.Rewrite(body, prompt.Degraded)
		degradedApplied = true
	}

	// outbound model 名重写为 bareModel（D6）：realm 前缀是网关侧路由协议，
	// 上游不认前缀（global 账号也请求裸模型名）。裸名时 bareModel==peek.Model 恒等。
	if bareModel != peek.Model {
		body = rewriteModel(body, bareModel)
	}
	// 密钥级思考开关（多密钥特性）：把密钥的意图落成请求体的 thinking.type。
	// 注入管线只认显式 thinking.type，所以在这里写进去即可覆盖它的默认注入，
	// 且**只影响这一把密钥**（全局默认不变，官方客户端 issue #43 的依赖不受影响）。
	if apiKey != nil && apiKey.Thinking != "" {
		body = applyKeyThinking(body, bareModel, apiKey.Thinking)
	}

	// 会话头族（issue #35 / #170）：后台按 X-Conversation-Request-ID 聚合请求，官方
	// 客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID。**统一轮级**
	// （对齐官方桌面 CLI：TraceStartHook 每次 USER_PROMPT_SUBMIT 清空重生成
	// conversationRequestId，同轮内复用、跨轮必换；官方云链路 lfConvReqId 的会话级
	// 是服务端指令，走本网关的客户端不属于该形态）。此处**轮转循环外**生成一次，
	// 循环内每次出站原样复用 → 换号/重试/降级全部同 ID，后台不再碎片化（此前网关
	// 一个都不发，上游按 HTTP 请求逐条记账，同一对话几十上百个 RequestID）。
	//   - conversationID：body 提取（透传客户端原值，缺省空串——不伪造，见
	//     ResolveConversationID；官方后台不校验一致，空会话则不建立聚合键）；
	//   - conversationRequestID：入站 X-Conversation-Request-ID 透传优先（客户端已
	//     有自己的对话轮 ID 则以客户端为准）。派生分两态：
	//     * turnKey 非空（有末条 user 消息）→ 带会话键客户端走 TurnRequestID(
	//       sessKey+":"+turnKey) 复合键（会话段入键保证不同会话同轮文本不互撞，
	//       轮级粒度对齐官方 CLI）；无会话键客户端走既有 TurnRequestID(turnKey)
	//       纯轮级键（存量会话键值零漂移）。
	//     * turnKey 为空（残留空态：无 user 消息/无可签名内容）→ sessKey 非空时
	//       回落 RequestIDForKey(sessKey)（会话级兜底，好于请求级随机）；sessKey
	//       也空走 NewMessageID 请求级（TurnRequestID 空键行为）——轮转内捕获
	//       一次即共享。
	//   - messageID 在 ChatHeaders 内每条消息生成（消息级独立，无需外部可见）。
	// ConversationID passes the client dialog id through. Clients that have none
	// (Anthropic / Claude Code traffic) fall back to the session key, otherwise
	// the upstream prompt_cache_key degrades into one constant per account and
	// every client on that account ends up sharing a single, constantly evicted
	// prefix-cache slot.
	// 【2026-10-14】第三级回退补齐：会话键也空时（聚合层客户端如 New API 转发时
	// 不带任何会话标识）改用 StickyFallbackKey 派生键（首条 user 消息哈希），与
	// 上方 stickyKey 同源。此前这种情况 convID 恒空 ⇒ buildCacheKey 退化成
	// 「每账号一个常量」⇒ 同账号下所有会话挤一个前缀缓存槽、互相驱逐；实测该账号
	// 首次大请求命中率仅 7.9~10.4%，换账号后缓存全丢。派生键让同会话稳定命中。
	convID := session.CacheKeySource(body, r.Header)
	// 出站 X-Conversation-ID 用**线形**（UUID）值（2026-10-20 防御加固）：内部标识可能是
	// `fb:<hex>` / `d-<hex>` 这类网关合成形态，原样发送等于自报「非官方客户端」——
	// 上游 issue #197/#231 已实证这条头参与渠道校验。
	// 与缓存键**分离**：ConversationID 保持原值供 prompt_cache_key 用，避免一次改动把
	// 全池会话的前缀缓存槽整体平移。convID 为空（body 带 user_id）时退回粘性键派生。
	wireSrc := convID
	if wireSrc == "" {
		wireSrc = stickyKey
	}
	chatMeta := upstream.ChatMeta{
		ConversationID:     convID,
		WireConversationID: session.WireConversationID(wireSrc),
	}
	if v := r.Header.Get("X-Conversation-Request-ID"); v != "" {
		chatMeta.ConversationRequestID = v
	} else if turnKey != "" && sessKey != "" {
		// 轮级复合键：sessKey 入键防跨会话同轮文本互撞。
		chatMeta.ConversationRequestID = session.TurnRequestID(sessKey + ":" + turnKey)
	} else if turnKey != "" {
		// 无会话键客户端：纯轮级键（既有兜底语义不变，存量会话键值零漂移）。
		chatMeta.ConversationRequestID = session.TurnRequestID(turnKey)
	} else if sessKey != "" {
		// 残留空态兜底：无轮可聚合时维持会话级聚合（同会话恒同值）。
		chatMeta.ConversationRequestID = session.RequestIDForKey(sessKey)
	} else {
		// 无会话键也无轮级键：请求级随机（轮转内捕获一次即共享）。
		chatMeta.ConversationRequestID = session.TurnRequestID("")
	}
	chatMeta.TraceID = r.Header.Get("X-Trace-ID")

	// blockedUIDs 本次请求内被上游内容策略拦过的账号（2026-09-29 行为判别）。
	//
	// 为什么不能「被拦就立即回 400」（旧行为）：上游对**账号级拒绝**也回同一句
	// displayMsg（"content did not pass the safety review"），文案不可信——实测两个
	// 账号对内容 "hi" 也返回该文案，而同一请求换号即 200。旧行为有两个后果：
	//   ① 客户端拿到本可避免的 400（换号就能成功）；
	//   ② 被拦账号零惩罚 ⇒ 状态永远"最干净" ⇒ 在选号打分里永远最优 ⇒ 被越选越多
	//      （实测 446/1011 = 44% 的 global 流量落在那 2 个死号上，客户端重试风暴）。
	// 新行为：先换号，用「换号后是否成功」这一**行为证据**判定：
	//   - 有号成功 ⇒ 前面被拦的是账号级坏号 ⇒ 在成功分支统一喂证据（升级惩罚）；
	//   - 整轮都被拦 ⇒ 才是内容问题 ⇒ 末端回 400 content_blocked，**零惩罚**。
	blockedUIDs := make([]string, 0, h.cfg.MaxRotate)
	// siblingTried 14003 同号兄弟模型兜底只做一次（见下方 ErrSoftRate 分支）。
	siblingTried := false
	// servedSibling 本次请求实际由兄弟模型（如 flash-sg）服务时记录其名字，
	// 供响应体把 model 字段改回客户端请求的名字（-sg 不外泄给 New API 等聚合层）。
	servedSibling := ""
	// capacityRetries 容量饱和（14003）时「留在原号等一会儿再试」的次数。
	//
	// 为什么不直接换号（2026-10-20 用户观测 + 日志实证）：上游模型全局饱和时，
	// 换号没有任何收益——新号同样 14003，但前缀缓存按 uid 隔离，等于整段重算。
	// 实测一个 60k→100k 的会话在 7 分钟内跳了 8 个号（quietotter920 → winternewt327
	// → lunarcoyote675 → jiuzhouye99 → yarrowfalcon539 → winteryak293 → onyxgazelle966
	// → xenonmarten603 → prismeagle198），每次换号命中率都掉到 0.0–3.5%。
	capacityRetries := 0
	// retryCapacitySameAccount 容量饱和时同号重试（不换号、不解绑粘性）。
	// 返回 false 表示重试预算用尽或 ctx 已取消，调用方按原逻辑换号。
	retryCapacitySameAccount := func() bool {
		if capacityRetries >= maxCapacityRetries {
			return false
		}
		capacityRetries++
		log.Printf("INFO: [server] model saturated (14003) -> wait %s and retry same acct, no rotate (%d/%d)",
			capacityRetryBackoff, capacityRetries, maxCapacityRetries)
		return sleepCtx(r.Context(), capacityRetryBackoff)
	}

	for i := 0; i < h.cfg.MaxRotate; i++ {
		// 选号：粘性号优先（PickByUIDForModel 已校验该模型可用性 + 在途未满），否则普通轮换。
		var acct *auth.Auth
		if stickyUID != "" {
			acct = h.cfg.Pool.PickByUIDForModel(stickyUID, bareModel)
			if acct == nil || (realm != "" && acct.Realm() != realm) {
				// 粘性号在当前模型不可用（冷却/占满/该模型被 6004 限额）或 realm 不符 → 解绑。
				unbindSticky()
			}
		}
		if acct == nil {
			// 模型感知 + realm 感知选号：模型非空时启用 6004 模型级冷却豁免
			// （healthyForModel），realm 谓词过滤跨域账号。
			acct = h.cfg.Pool.PickExcludingForRealm(tried, bareModel, realm)
		}
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			// 记下"是不是模型级阻塞"。选号返回 nil 有两种完全不同的成因：
			//   (a) 池子真的没有可用号（账号级冷却/在途占满/积分保底）；
			//   (b) 号都在，但每个号都对这个模型处于模型级冷却（11102/6004）。
			// 两者此前都报 no_healthy_account，导致「模型不可用」被读成「号全挂了」。
			// 在这里取一次快照，供下方错误构造区分（issue #102 附带发现 1）。
			// 按**请求域**判定（2026-10-14）：选号本身按 realm 过滤，跨域账号对本请求
			// 不可达、不构成「还能服务」的证明；不传 realm 会把「CN 该模型全灭」误读成
			// 「还有 global 能服务」→ 退回 503 可重试形状（实测 CN glm-5.3 全体 6004）。
			modelBlock = h.cfg.Pool.ModelBlockedForRealm(bareModel, realm)
			break
		}
		st.uid = acct.UID
		// 同步昵称：请求流水行只写 uid8 时无法直观看是哪一号，昵称随本次选号带入日志行。
		st.nick = acct.Nickname
		tried[acct.UID] = true

		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !h.cfg.Pool.Acquire(acct.UID) {
			// 若被抢的正是粘性号，立即解绑并回落普通轮换，避免下一轮仍撞同一个
			// 满载粘性号再浪费一次粘性命中往返（语义与 fail()/粘性命中-nil 的解绑一致）。
			if stickyUID != "" && acct.UID == stickyUID {
				unbindSticky()
			}
			if !rotateBackoff(i, r.Context()) {
				// 客户端已断连：换号重试无意义，终止轮转走末端错误透传。
				break
			}
			continue // 最后一个名额被并发抢走 → 换号
		}
		heldUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					// 12153 一次失败不杀号（临时触发会误杀）：与 scheduler keepalive/checkin
					// 同口径走连续计数，达到 sessionDeadThreshold 才禁用。
					h.cfg.Pool.NoteSessionDead(acct.UID)
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				if !rotateBackoff(i, r.Context()) {
					break // ctx 取消：终止轮转（refresh 失败换号退避，WAF P0-2）
				}
				continue
			}
			acct.BackfillRealm() // 老 auth 空 realm → 落盘前补标识（幂等：已有不动）
			if err := acct.SaveAtomic(); err != nil {
				// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
				log.Printf("ERR: [server] chat refresh acct=%s: save auth failed: %v", logfmt.Label(acct.UID, acct.Nickname), err)
			}
		}

		// 客户端 IP 透传（仅 PassthroughIP 开启）：按请求取首段作为参数传入 ChatStream，
		// 不再读写共享字段——并发请求各自携带独立 IP，互不串扰（issue：ClientIP 竞态）。
		var clientIP string
		if h.cfg.Upstream.PassthroughIP {
			clientIP = upstream.ExtractClientIP(r)
		}
		// 传 r.Context()：客户端断连/请求取消立即中断在途上游调用并释放租约，
		// 不再让"幽灵请求"占满账号在途名额直到 IdleTimeout。
		//
		// 容量冷却预判（2026-10-20）：该号刚在这个模型上吃过 14003（容量类避让未过期），
		// 且该模型有兄弟模型 → **直接发兄弟模型**，省掉每轮一次必然失败的往返。
		// 此前表现为「请求在 flash / -sg 之间来回跳」：14003 后记 60s 容量避让，但粘性
		// 绑定按设计保留，于是下一轮又先撞一次 flash（429）再回落 -sg，每轮多付一次往返。
		sendBody := body
		attemptSibling := ""
		if sib, ok := upstream.SiblingModel(bareModel); ok && h.cfg.Pool.ModelCapacityCooled(acct.UID, bareModel) {
			sendBody = rewriteModel(body, sib)
			siblingTried = true // 兄弟模型已用掉：若它也 14003，14003 分支不再重试它
			attemptSibling = sib
		}
		attemptStarted := time.Now()
		rc, status, respBody, terr := h.cfg.Upstream.ChatStreamContext(r.Context(), acct, sendBody, clientIP, chatMeta)
		// 预判成功才回写 servedSibling：失败（含兄弟也 14003）时要换号重试，
		// 不能让下一跳的响应体被改写成 -sg。
		if attemptSibling != "" && terr == nil && status < 400 {
			servedSibling = attemptSibling
			st.upstreamModel = attemptSibling
		}
		// 分类信封一次成型：upstream 已在错误路径返回 *upstream.Error（Kind +
		// Retry-After 头解析，见 ChatStreamContext 注释）。传输层错误（非 *Error）走
		// 抖动换号分支；防御分支（terr 为 nil 但 status>=400，如 ErrNone 兜底）回落
		// 本地 Classify，双保险不改变语义。
		var uerr *upstream.Error
		if errors.As(terr, &uerr) {
			status = uerr.Status
		}
		if uerr == nil && terr != nil {
			// 上游超时 / 停滞：**不换号、不罚号**。
			//
			// 超时不是账号的问题：同一份请求换到别的号，撞上的是同一个慢上游，
			// 只会把客户端拖到 MaxRotate × header_timeout（部署值 600s 时最坏
			// 约半小时），期间还给一串健康号喂连败计数。此前全仓没有任何超时
			// 识别，超时和"网络抖动"共用同一条换号路径。
			//
			// 判定三态：net.Error.Timeout()（ResponseHeaderTimeout / Client.Timeout）、
			// 显式 deadline（DeadlineExceeded / os.ErrDeadlineExceeded）、以及
			// **客户端仍在但 ctx 被取消**——那只能是我们自己的空闲看门狗掐的流，
			// 也就是上游停滞。客户端主动断连时 r.Context() 已取消，走下面的抖动分支。
			if isUpstreamTimeout(terr, r.Context().Err() != nil) {
				recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
				// 短时**模型级**避让（2026-10-15）：上面的"不换号"是对的（同一份请求
				// 换号撞的是同一个慢上游），但"不罚号"会让卡住的号状态最干净、在选号
				// 打分里永远最优 → 被反复选中。实测红花(a72732ea) 10:26:30–10:29:20
				// 连续 4 次各烧 120s 头超时（其间同样大小的 prompt 只要 7s），是全池
				// 唯一有 >100s 记录的号。给 60s 模型级避让，下一次请求就落到别的号上。
				// 用模型级而非账号级：卡的是"这个号 + 这个模型"这一次调用，不连坐其他模型。
				// 仍不进熔断/连败计数：超时不是账号故障证据（网络/上游抖动同样会超时）。
				h.cfg.Pool.CooldownSoftForModel(acct.UID, timeoutCooldown,
					time.Now().Add(timeoutCooldown), bareModel, "upstream timeout")
				st.status = http.StatusServiceUnavailable
				lastErr = fmt.Errorf("%w: %v", errUpstreamTimeout, terr)
				log.Printf("WARN: [server] upstream timeout acct=%s: %v (rotation stopped, account not penalized; model-level %s cooldown)",
					logfmt.Label(acct.UID, acct.Nickname), terr, timeoutCooldown)
				break
			}
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 连败兜底（issue #114）：喂连败计数——连不上上游是「不知道原因的失败」，
			// 连败 N 次临时出池，单次/偶发不罚（NoteFailures 内部达阈才动作）。
			// 上游 client 已打 transport error 日志。
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			h.cfg.Pool.NoteFailures(acct.UID)
			fail(acct.UID)
			if !rotateBackoff(i, r.Context()) {
				break // ctx 取消：终止轮转（传输层错误换号退避，WAF P0-2）
			}
			continue
		}
		if status >= 400 {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
			st.status = status
			var kind upstream.ErrKind
			if uerr != nil {
				kind = uerr.Kind
			} else {
				kind = upstream.Classify(status, string(respBody))
				uerr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			}
			// 内容拦截误报（passthrough/append 模式首遇）：判定为 system 指纹误报，
			// 触发降级到次日 00:00 CST，换 Degraded 中性提示词同请求内重试（append
			// 降级重试同样退化为 replace——原文在场只会确定性再撞 400）。
			// 第二次仍被拦 → 落下方 ErrContentBlocked 分支：**换号**，用行为证据
			// 判定是账号问题还是内容问题（2026-09-29；该分支不再立即回 400）。
			if kind == upstream.ErrContentBlocked && (h.cfg.PromptMode == "passthrough" || h.cfg.PromptMode == "append") && !degradedApplied {
				h.degrade.Trigger()
				body = prompt.Rewrite(body, prompt.Degraded)
				degradedApplied = true
				delete(tried, acct.UID) // 单账号池也能拿到重试机会（降级重试占一次名额）
				releaseHeld()
				log.Printf("WARN: [server] content-blocked (likely fingerprint false positive) -> degraded prompt retry")
				continue
			}
			if kind == upstream.ErrContentBlocked {
				// 内容策略拦截：**先换号，不立即回客户端**（2026-09-29 行为判别修复）。
				//
				// 旧行为是「立即回 400、不轮转」，理由是「换任何账号都会撞同一审核」——
				// 该假设已被实测证伪：上游对**账号级拒绝**也回同一句 displayMsg
				// （两个账号对内容 "hi" 也返回「内容未通过安全审核」，而同一请求换号即 200）。
				// 文案不可信，唯一可靠判据是行为：换号后能否成功。
				//
				// 因此这里只记账 + 换号，把判定推迟到「结果已知」之后：
				//   - 后续有号成功 → 成功分支对 blockedUIDs 统一喂 NoteContentBlockEvidence
				//     （前 2 次软冷却让位，第 3 次 Disable——账号侧被上游拒）；
				//   - 整轮都被拦 → 末端 ErrContentBlocked 分支回 400，**零惩罚**（内容问题）。
				// applyErrorPolicy 的 ErrContentBlocked 分支仍为零动作（不冷却/熔断/NoteError），
				// 惩罚只由「证据」路径施加，两条路径职责不重叠。
				//
				// 轮转代价：真内容拦截时多打 1~2 次上游（罕见），换来「账号级误判不再变成
				// 客户端可见 400 + 死号泄漏」。
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				blockedUIDs = append(blockedUIDs, acct.UID)
				st.status = http.StatusBadRequest
				lastErr = uerr
				if !rotateBackoff(i, r.Context()) {
					break // ctx 取消：终止轮转（客户端已走，换号无意义）
				}
				continue
			}
			// 11115「prompt is too long」：立即透传上游原文回客户端，**不罚号不轮转**
			// ——上下文超限是请求的问题（同一 body 换任何号都超限，白扔健康号配额；
			// 与 WAF IP fail-fast 同哲学：确定与账号无关的错误直接终止轮转）。
			// applyErrorPolicy ErrPromptTooLong 分支零动作（不冷却/不熔断/不 NoteError，
			// 不喂连败），fail 只释放租约。error-passthrough：message 装上游 body 原文
			// （code/msg/requestId 原样，含真实 token 数与上限值——上游原文是最有价值
			// 的错误信息，客户端必须看到，禁止固定词覆盖）。
			if kind == upstream.ErrPromptTooLong {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				writeOpenAIErrorHint(w, http.StatusBadRequest, "prompt_too_long", promptTooLongMessage(string(respBody)),
					h.hintOf(upstream.ErrPromptTooLong, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// 图片格式/数据无效：立即透传上游原文回客户端，不罚号不轮转。
			// 同一 body 换账号仍是同样的解析结果，轮转只会放大无效请求。
			if kind == upstream.ErrImageInvalid {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "image request was rejected by upstream"
				}
				writeOpenAIErrorHint(w, http.StatusBadRequest, "image_invalid", msg,
					h.hintOf(upstream.ErrImageInvalid, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// 请求体解析失败（11101）：与 11115 / 图片无效同一哲学——同一 body 换任何
			// 账号都是同样的解析结果，轮转只会放大无效请求（每号一次上游调用 +
			// rotateBackoff 占用在途名额）。更要紧的是：继续轮转后末端会落到
			// 「其余保持 503」，把确定失败的请求伪装成"账号不可用、稍后再试"，
			// 客户端于是对必然失败的请求无限重试。立即透传上游原文回 400。
			if kind == upstream.ErrBadParams {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "chat request body was rejected by upstream"
				}
				writeOpenAIErrorHint(w, http.StatusBadRequest, "bad_params", msg,
					h.hintOf(upstream.ErrBadParams, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// 上游「请求被拦截，请重新发送」（code 11-128）：**请求级**拒绝，与账号授权
			// 状态无关（2026-10-16 实测：同一请求换任何号都被同样拒绝；被拒号的 token
			// 直连上游返回同一错误，且该号同一分钟仍在服务其他请求）。
			// 与 11115 / 图片无效 / 11101 同一哲学——确定与账号无关的错误直接终止轮转、
			// 透传上游原文。此前归 ErrClient → 轮转 3 个号全被同因拒（白耗 3 次调用配额）
			// → 对外 503 no_healthy_account：客户端看不到上游原文，还会对必然失败的
			// 请求无限重试。
			if kind == upstream.ErrUpstreamRejected {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "request was rejected by upstream"
				}
				writeOpenAIErrorHint(w, http.StatusBadRequest, "upstream_rejected", msg,
					h.hintOf(upstream.ErrUpstreamRejected, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// 14003「模型繁忙」同号兄弟模型兜底（2026-10-20）：
			// 14003 是**逐请求的模型容量准入**，不是账号级风控——直连实测同号
			// 5~6 秒内 429→200 反复，且同一时刻该号的其他模型（glm-5.3-flash）稳定 200。
			// 换号会同时丢掉粘性与前缀缓存；同号换兄弟模型（flash → flash-sg）
			// 是代价最小的动作，故在换号之前先试一次。
			// 只兜一次：兄弟模型也忙时按原逻辑换号，不引入新的重试放大。
			siblingServed := false
			if kind == upstream.ErrSoftRate && upstream.IsModelBusy(string(respBody)) && !siblingTried {
				if sib, ok := upstream.SiblingModel(bareModel); ok {
					siblingTried = true
					// 原模型仍按既有语义记 60s 模型级避让，避免其他请求继续撞它。
					h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
					log.Printf("INFO: [server] model busy 14003 -> retry same acct with sibling model %s (was %s) acct=%s",
						sib, bareModel, logfmt.Label(acct.UID, acct.Nickname))
					attemptStarted = time.Now()
					sibRC, sibStatus, sibBody, sibErr := h.cfg.Upstream.ChatStreamContext(r.Context(), acct, rewriteModel(body, sib), clientIP, chatMeta)
					var sibUErr *upstream.Error
					if errors.As(sibErr, &sibUErr) {
						sibStatus = sibUErr.Status
					}
					if sibErr == nil && sibStatus < 400 {
						// 兄弟模型接住了：替换本次尝试结果，落到下方成功路径。
						// st.model 保持客户端请求的原模型名；粘性仍按 bareModel 绑定，
						// 客户端下一轮仍先试 flash，忙时再兜底。
						rc, status, respBody, terr = sibRC, sibStatus, sibBody, sibErr
						siblingServed = true
						servedSibling = sib
						st.upstreamModel = sib
					} else {
						recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
						if sibUErr == nil {
							// 传输层错误：与主路径同语义——只换号 + 连败计数，不罚模型。
							lastErr = sibErr
							h.cfg.Pool.NoteFailures(acct.UID)
						} else {
							// 兄弟模型也失败：同样记模型级避让，再换号。
							lastErr = &upstream.Error{Kind: sibUErr.Kind, Status: sibStatus, Msg: string(sibBody), RetryAfter: sibUErr.RetryAfter}
							h.applyErrorPolicy(acct.UID, sibUErr.Kind, string(sibBody), sib, sibUErr)
						}
						// 两个模型都只是「容量饱和」→ 留在原号等一会儿再试，不换号。
						// 换号在这时没有任何收益（上游全局饱和），只会丢掉整段前缀缓存。
						if sibUErr != nil && upstream.IsModelBusy(string(sibBody)) && retryCapacitySameAccount() {
							i-- // 容量重试不占用换号预算
							continue
						}
						fail(acct.UID)
						if !rotateBackoff(i, r.Context()) {
							break
						}
						continue
					}
				}
			}
			if !siblingServed {
				// lastErr 携带完整 body（uerr.Msg 在 upstream 侧截断 200 字符，透传语义
				// 5755fe3 要求原文全量）+ Kind/RetryAfter（末端映射与冷却时长共用）。
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody), RetryAfter: uerr.RetryAfter}
				// 记避让用的模型名取**实际发出**的那个：预判走兄弟模型时，冷却应记在 -sg 上
				// （flash 的避让在预判那一刻已由上一轮记过）。
				errModel := bareModel
				if attemptSibling != "" {
					errModel = attemptSibling
				}
				h.applyErrorPolicy(acct.UID, kind, string(respBody), errModel, uerr)
				// 容量饱和（14003）：先把避让记上（下一跳预判会自动走兄弟模型），
				// 再留在原号等一会儿重试——不换号、不解绑粘性。
				if kind == upstream.ErrSoftRate && upstream.IsModelBusy(string(respBody)) && retryCapacitySameAccount() {
					i-- // 容量重试不占换号预算
					continue
				}
				fail(acct.UID)
				// 边缘层 IP 级 fail-fast（优先于 rotateBackoff 退避——IP 级拦截时退避无意义，
				// 任务书设计纪律）：该次边缘层拒绝喂入 IP 级状态机，若激活（短窗多号命中，
				// IP 被拦而非账号）则立即终止轮转——继续换号只会把请求放大 MaxRotate 倍
				// 打同一出口 IP，加重风控。
				// 两种形态共用同一状态机：WAF 403（账号级软冷却照常记账）与鉴权层 401
				// （零账号惩罚，见 applyErrorPolicy 的 ErrEdgeAuth 分支）。IP 级状态只改变
				// 「是否继续轮转」——协同不叠加。
				if kind == upstream.ErrWafBlock && h.edgeGate.noteWaf(acct.UID) {
					break
				}
				if kind == upstream.ErrEdgeAuth && h.edgeGate.noteEdgeAuth(acct.UID) {
					break
				}
				if !rotateBackoff(i, r.Context()) {
					break // ctx 取消：终止轮转（分类错误换号退避，WAF P0-2）
				}
				continue
			}
		}
		// 「成功判定 + 粘性绑定」延后到这一跳真正成功之后（markTurnSuccess）。
		// 理由：上游存在「200 已开流 + 一帧 error」的真实形态（6004 限流 / 内容拦截 /
		// 审核）——在读到第一帧之前就 NoteSuccess 会把被限流的号记成健康，粘性还会把
		// 会话钉死在它身上，后续每一轮都打同一个限流号。
		// markTurnSuccess 同样承载本仓的「行为证据」逻辑（blockedUIDs）：
		// 只有真成功才清算被拦号，语义与 NoteSuccess 严格同点。
		markTurnSuccess := func(uid string) {
			h.cfg.Pool.NoteSuccess(uid)
			// ⭐ 行为证据落地（2026-09-29）：本次请求成功 ⇒ 内容本身可过审 ⇒ 之前那些
			// 回 content_blocked 的账号是**账号侧被上游拒**，不是内容问题。此时才喂证据：
			// 未达阈值软冷却让位，达阈值 Disable（账号需重新登录）。
			// 位置刻意放在 NoteSuccess 之后：先承认本次成功号可用，再清算被拦号。
			for _, buid := range blockedUIDs {
				if buid == uid {
					continue // 理论上不会（同号不会既被拦又成功），防御性跳过
				}
				if h.cfg.Pool.NoteContentBlockEvidence(buid) {
					log.Printf("WARN: [server] content-block evidence: disabled acct=%s after %d account-side content rejections while other accounts served the same request",
						logfmt.Label(buid, ""), pool.ContentBlockThreshold())
				}
			}
			// 11102 负缓存清命：该账号该模型实测成功，立即解除避让（不必等 TTL 到期）。
			// BlockModelClear 按 "11102" reason 前缀识别，只清 11102 条目、不碰 6004 独立冷却。
			h.cfg.Pool.BlockModelClear(uid, bareModel)
			// 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
			// 绑定键用 stickyKey（会话头族 → SessionIDFromHeader → StickyFallbackKey
			// 三级回退），而非 sessKey——聚合层客户端（如 DSH）不带任何会话头时
			// sessKey 恒空，此前判据 `sessKey != ""` 会让粘性静默失效、每轮纯轮转。
			if stickyKey != "" && h.cfg.Session != nil {
				h.cfg.Session.Bind(stickyKey, uid)
			}
		}
		if peek.Stream {
			// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
			st.status = http.StatusOK
			stats := newChatStatsReaderSince(rc, st.start)
			// gateway_hint（SSE）：成功状态 200 已开流，中途 error 帧透传时附加
			// hint 字段（hintFn 惰性求值——正常流零开销，只有真撞到 error 帧才
			// 组装请求上下文做判定）。
			// errFrame：上游 error 帧原文（观察者旁路采集），用于流尾的账号处置。
			var errFrame string
			// 标记修复：上游在没有 tools 的请求里会把工具调用吐成原生标记文本，
			// 这里在透传前还原成 delta.tool_calls（见 upstream/dsml.go）。
			// 第二个参数为 true = 名单为空时启用弱判定——tools 声明在链路上丢失是
			// 实测最常见的泄漏成因，只做严格判定等于对主场景不设防。
			repair := upstream.NewMarkupRepair(declaredTools, true)
			sErr := upstream.StreamHint(w, stats, upstream.FrameHintFunc(func() upstream.HintContext {
				return h.hintContext(bareModel, reqHasImage)
			}), upstream.WithErrorFrameObserver(func(payload string) { errFrame = payload }),
				upstream.WithMarkupRepair(repair),
				// 兄弟模型兜底成功时把帧里的 model 改回客户端请求的名字：
				// -sg 是网关内部备胎，不外泄给 New API 等聚合层。
				upstream.WithModelRewrite(servedSibling, bareModel))
			switch {
			case upstream.IsEmptyStreamError(sErr):
				// 上游 200 但空流（0 有效帧）：StreamHint 已写 error 帧 + [DONE]
				// 兜底（HTTP 头已发出只能 200），但这是上游缺陷不是成功——日志/
				// 状态收敛到 502 观测，与非流式 Aggregate 空流→502 upstream_parse
				// 同语义（此前 `_ =` 吞错把失败流记成 200，运维看到假成功）。
				// 只认 IsEmptyStreamError：客户端断连的写失败不误标（人已走，
				// 502 观测没有意义）。
				st.status = http.StatusBadGateway
				st.outcome = reqlog.OutcomeStreamError
				log.Printf("WARN: [server] stream acct=%s model=%s: empty upstream stream (200+0 frames)", logfmt.Label(acct.UID, acct.Nickname), bareModel)
			case errFrame != "":
				// 上游以 error 帧报错（6004 限流 / 内容拦截 / 审核）：按帧内容分类并
				// 处置账号——**不记成功、不清 11102 负缓存、不绑粘性**。此前这些动作
				// 在流开始前就做了，于是一个正在限流的号被当成健康号，粘性还会把
				// 整个会话钉在它身上，后续每轮都失败。
				kind := upstream.FrameKind(errFrame)
				h.applyErrorPolicy(acct.UID, kind, errFrame, bareModel, nil)
				st.status = http.StatusServiceUnavailable
				st.outcome = reqlog.OutcomeStreamError
				log.Printf("WARN: [server] stream acct=%s model=%s: upstream error frame kind=%s payload=%s",
					logfmt.Label(acct.UID, acct.Nickname), bareModel, kind, logfmt.Truncate(errFrame, 200))
			case sErr != nil:
				// 客户端写失败（断连）：上游帧无恙，账号健康——账号侧照常记成功
				// （与 default 同语义），请求日志归为 Interrupted（人已走，未完成）。
				st.outcome = reqlog.OutcomeInterrupted
				markTurnSuccess(acct.UID)
			default:
				// 真成功：这一跳读完且上游没有报错，才记成功并让粘性跟上。
				st.outcome = reqlog.OutcomeSuccess
				markTurnSuccess(acct.UID)
			}
			credit, hasCredit := stats.Credit()
			if hit, miss, _, ok := stats.CacheTokens(); ok {
				st.cacheHit, st.cacheMiss, st.hasCache = hit, miss, true
			}
			recordAttempt(acct.UID, stats.Usage(), credit, hasCredit, attemptStarted, stats.TTFB())
			// WARN 信号放 recordAttempt 之后：st.promptTokens 此时才是本次的观测值。
			if st.hasCache {
				cacheMissWarn.noteCacheTokens(bareModel, st.promptTokens, st.cacheHit, st.cacheMiss)
			}
			if n := repair.Converted(); n > 0 {
				// 修复命中：上游本来会把工具调用吐成正文标记，这里已还原成 tool_calls。
				// 打一行便于运维确认「这次不再空转」以及统计发生率。
				log.Printf("INFO: [server] stream acct=%s model=%s: repaired %d native tool call(s) from assistant text (tool names known=%d)",
					logfmt.Label(acct.UID, acct.Nickname), bareModel, n, len(declaredTools))
			} else if m := repair.Seen(); m > 0 {
				// 识别到标记却没还原：说明判定没过（工具名不合规 / 块内夹带正文 /
				// 参数畸形）。单独记一行，排障时能一眼区分「没识别到」与「识别到但拒绝」。
				log.Printf("WARN: [server] stream acct=%s model=%s: saw %d native tool call block(s) but repaired none (tool names known=%d)",
					logfmt.Label(acct.UID, acct.Nickname), bareModel, m, len(declaredTools))
			}
			st.ttfb = stats.TTFB()
			// usage 缺失时保留 chatStat.toks 的 -1 哨兵（观测缺失 → 显示 "-"），
			// 不写入零值——否则「没观测到 usage」被伪造成「测得 0 token」，
			// 与非流式走 completionTokens 返回 -1 的口径不一致。
			toks, hasUsage := stats.Tokens()
			if hasUsage {
				st.toks = toks
			}
			// metrics 采集：token 三段 + 缓存三段 + 真实扣费（供 /v1/stats）。
			// 与成本账本同源同口径（都读末帧 usage），故此处一并带出，避免二次解析。
			st.hasUsage = hasUsage
			st.prompt = stats.PromptTokens()
			st.cacheHit, st.cacheMiss, st.cacheWr, _ = stats.CacheTokens()
			if credit, ok := stats.Credit(); ok {
				st.credit = credit
				st.hasCredit = true
			}
			// 成本账本：末帧 usage 带 credit 与 token 总数时记录实测单价，
			// 供下次选号把免费/便宜的号排在前面。
			if credit, ok := stats.Credit(); ok {
				h.cfg.Pool.NoteModelCost(acct.UID, costModelName(bareModel, servedSibling), credit, stats.TotalTokens())
			} else if hasUsage {
				// R9(c) 防护观测：usage 存在但 credit 缺失（如 global SSE 末帧未带 credit）。
				// 不算合法成本观测（缺失≠0），仅记一条 WARN 协助排障，绝不写入账本。
				log.Printf("WARN: [server] stream usage without credit acct=%s model=%s (no cost observation)", logfmt.Label(acct.UID, acct.Nickname), bareModel)
			}
			rc.Close()
			return
		}
		resp, err := upstream.Aggregate(rc)
		rc.Close()
		if err != nil {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
			// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			st.status = http.StatusBadGateway
			st.outcome = reqlog.OutcomeHTTPError
			return
		}
		// 标记修复（非流式）：把正文里的原生工具调用标记还原成 message.tool_calls。
		// 只在「没有结构化 tool_calls」且「finish_reason 为 stop」时生效（见 dsml.go）；
		// 名单为空时走弱判定（tools 声明在中转环节丢失是常态）。
		mrepair := upstream.RepairAggregatedResponse(resp, declaredTools, true)
		if n := mrepair.Converted(); n > 0 {
			log.Printf("INFO: [server] non-stream model=%s: repaired %d native tool call(s) from assistant text (tool names known=%d)",
				bareModel, n, len(declaredTools))
		} else if m := mrepair.Seen(); m > 0 {
			log.Printf("WARN: [server] non-stream model=%s: saw %d native tool call block(s) but repaired none (tool names known=%d)",
				bareModel, m, len(declaredTools))
		}
		credit, total, hasCredit := usageCreditTotal(resp)
		if usage, ok := resp["usage"].(map[string]any); ok {
			if hit, okH := upstream.UsageCacheHitTokens(usage); okH {
				miss, _ := upstream.UsageCacheMissTokens(usage)
				st.cacheHit, st.cacheMiss, st.hasCache = int64(hit), int64(miss), true
			}
		}
		recordAttempt(acct.UID, usageDeltaFromResponse(resp), credit, hasCredit, attemptStarted, 0)
		if st.hasCache {
			cacheMissWarn.noteCacheTokens(bareModel, st.promptTokens, st.cacheHit, st.cacheMiss)
		}
		// 兄弟模型兜底成功：对外回显客户端请求的模型名（-sg 不外泄）。
		if servedSibling != "" {
			resp["model"] = bareModel
		}
		// metrics 采集（非流式）：与流式同口径，从同一份 usage 带出。
		// 必须在剥离 credit **之前**调用——它要读 usage.credit。
		fillStatFromUsage(st, resp)
		// 积分字段不外泄（2026-10-20）：credit 是网关计费口径，留在 reqlog/成本账本即可。
		stripUsageCreditField(resp)
		writeJSON(w, http.StatusOK, resp)
		st.status = http.StatusOK
		st.outcome = reqlog.OutcomeSuccess
		st.toks = completionTokens(resp)
		// 非流式同理：聚合成功（无 error 帧、非空流）才算这一跳成功，事后才记成功/绑粘性。
		markTurnSuccess(acct.UID)
		// 成本账本（非流式）：从聚合响应的 usage 取 credit 与 token 总数。
		if hasCredit {
			st.credit = credit
			st.hasCredit = true
			h.cfg.Pool.NoteModelCost(acct.UID, costModelName(bareModel, servedSibling), credit, total)
		}
		return
	}
	// 末端错误透传（error-passthrough）：上游返回的错误原样透传，不再规范化成固定文案。
	//
	// 背景：此前把上游原始错误文本统一改写，防账号 UID / 上游内部错误码（11128 / 12153 /
	// 6004 后台措辞）泄露——但副作用是客户端看不到真实错误，根本没法排查上游问题。
	// 任务书规定上游错误码/账号语义**允许**泄露给客户端（有意为之），排查必须看到原文。
	//
	//   - 上游返回（*upstream.Error）→ error.message 装**上游 body 原文**（code/msg/
	//     requestId 原样保留，如 {"code":6004,"msg":"…","requestId":"…"}）。HTTP 状态码
	//     按 OpenAI 兼容口径映射类别：ErrSoftRate → 429（限流语义、客户端应等待重试），
	//     其余保持 503（网关侧无健康账号可用）。业务 code 取本地分类可读名
	//     （rate_limit_exceeded / no_healthy_account）。
	//   - 本地调度类错误（无可用账号 acct==nil、传输层抖动、非上游返回的 lastErr）
	//     → 保留自有文案 no_healthy_account（本地错误没有上游原文可透传，不编造）。
	status := http.StatusServiceUnavailable
	code := "no_healthy_account"
	msg := "all accounts are temporarily unavailable, please retry later"
	// 上游超时：轮转已在传输层分支止损（见 isUpstreamTimeout），这里给一条**能区分**
	// 的文案，别混进"没有可用账号"——两者的排查方向完全不同。
	if errors.Is(lastErr, errUpstreamTimeout) {
		code = "upstream_timeout"
		msg = "upstream timed out: rotation stopped (another account would hit the same slow upstream), please retry later"
	}
	// gateway_hint（末端透传）：上游错误按 Kind + 原文 + 请求形态判定；本地调度类
	// 错误（无上游原文）固定 no_healthy_account hint。
	hint := upstream.NoHealthyAccountHint()
	// upstreamMsgPassed 记录 error.message 是否已被上游原文占据：模型级阻塞分支
	// 据此决定要不要覆盖 msg（上游原文优先，含 requestId）。
	upstreamMsgPassed := false
	var ue *upstream.Error
	if errors.As(lastErr, &ue) {
		hint = h.hintOf(ue.Kind, ue.Msg, bareModel, reqHasImage, ue)
		switch ue.Kind {
		case upstream.ErrSoftRate:
			status = http.StatusTooManyRequests
			code = "rate_limit_exceeded"
			msg = "rate limited: all accounts are cooling down, please wait a moment and try again"
		case upstream.ErrWafBlock:
			if h.edgeGate.active() {
				// IP 级拦截措辞（fail-fast 终止路径）：空 body 时给出明确可读文案——
				// 网关出口 IP 被 WAF 拦截、轮转已止损、窗口 X 秒后自动解除。客户端
				// 提前重试无意义（换号不换 IP）；有上游原文时原文优先（下方统一）。
				code = "waf_ip_blocked"
				msg = "waf ip-level block: upstream firewall is blocking the gateway IP, rotation stopped; retry after the block window expires"
			}
		case upstream.ErrEdgeAuth:
			// 鉴权层 401 与 WAF 403 同族（都是边缘层按出口 IP 拒绝），只是状态码不同：
			// 机器可读 code 分开（客户端可据此区分是风控还是鉴权），文案口径一致。
			// 空 body 时给明确可读文案；有上游原文（openresty 401 页）时原文优先（下方统一）。
			// [本仓自研 2026-09-22，上游未吸收]
			code = "edge_auth_rejected"
			msg = "upstream gateway rejected the request at its auth edge (401); accounts are healthy, this is an egress-IP-level rejection"
			if h.edgeGate.active() {
				code = "edge_auth_blocked"
				msg = "edge auth ip-level block: upstream gateway is rejecting the gateway IP at its auth edge, rotation stopped; retry after the block window expires"
			}
		case upstream.ErrContentBlocked:
			// 整轮轮转都被内容策略拦（2026-09-29）：这才是**内容问题**——换任何号都拦，
			// 所以零账号惩罚（没有任何账号被喂证据；blockedUIDs 里那些号在循环内已
			// 被 applyErrorPolicy 零动作处理过）。回 400 与旧行为一致，客户端据此调整内容。
			// 空 body 时给可读文案；有上游原文（code 11140 + displayMsg）时原文优先（下方统一）。
			// [本仓自研 2026-09-29，上游未吸收]
			status = http.StatusBadRequest
			code = "content_blocked"
			msg = "content blocked by upstream content firewall"
		case upstream.ErrUpstreamRejected:
			// 兜底（正常走不到：chatCompletions 已 fail-fast 400 透传原文）。
			// 万一从其他路径到达末端，仍按"请求级拒绝"给 400 + 可读 code，别退化成
			// 503 no_healthy_account（那会误导客户端重试必然失败的请求）。
			status = http.StatusBadRequest
			code = "upstream_rejected"
			msg = "request was rejected by upstream request firewall"
		case upstream.ErrModelBlocked:
			// 11102「该后端无此模型」：上游原文（下方统一透传）已经写清了原因，但
			// code 此前停在默认的 no_healthy_account —— 那是"服务端过载"的语义，
			// 客户端据此会不断重试（#81 里就是 503 → 客户端自动重试 5 次），而真正
			// 该做的是换个模型。用 400 + 明确的 code 把不可重试的性质讲清楚。
			code = "model_unavailable"
			status = http.StatusBadRequest
		}
		if s := strings.TrimSpace(ue.Msg); s != "" {
			// 上游原文优先：透传 code/msg/requestId，不拼接本地前缀。
			msg = s
			upstreamMsgPassed = true
		}
	}
	// 模型级阻塞覆盖上面的通用文案（放在最后 = 优先级最高）。
	//
	// 为什么能覆盖 code 而不丢信息：modelBlock.Reason 就是 BlockModelBackoff 存下的
	// 上游原因，已在 hint 里原样带出，所以覆盖 message 不会丢掉上游信息，反而补上
	// 了上游不会告诉客户端的两件事——「有几个号被挡」和「最早什么时候解封」。
	//
	// 用 400 而非 503：这是"你选的这个模型当前不可用"，不是"服务端暂时过载"。
	// 换号重试必然同样失败，客户端不该按可重试错误处理。
	if modelBlock.Blocked {
		// 排队类例外（2026-10-15）：6020/6021 的模型级避让**不是**「模型不可用」，
		// 而是「上游队列此刻满」。6004/11102 报 400 是因为「换号重试必然同样失败，
		// 客户端该换模型」；排队正相反——等一会儿重试才是对的（上游队列几十秒就排空）。
		// 若也报 400 model_unavailable，会把客户端推向"换模型"这个错误动作。
		// 故保持 429 + 上游原文（客户端据此退避重试），本地调度信息进 hint。
		//
		// 模型繁忙（14003，2026-10-19）同属该家族：上游说的是「switch models or retry
		// later」，两种动作都接受，等 60s 即可，不该被推成 model_unavailable。
		if upstream.IsRetryableModelReason(modelBlock.Reason) {
			code = "rate_limit_exceeded"
			if !upstreamMsgPassed {
				msg = "upstream queue is full for this model, please retry later"
			}
			hint = fmt.Sprintf("queue busy on %d account(s)", modelBlock.Count)
			if !modelBlock.Until.IsZero() {
				hint += "; retry after " + modelBlock.Until.Format(time.RFC3339)
			}
			status = http.StatusTooManyRequests
			writeOpenAIErrorHint(w, status, code, msg, hint)
			st.status = status
			st.outcome = reqlog.OutcomeHTTPError
			return
		}
		code = "model_unavailable"
		// 有上游原文时保留它（含 requestId，用户要拿去向上游反馈），只把"有几个号
		// 被挡、最早何时解封"这类本地调度信息放进 gateway_hint。
		if !upstreamMsgPassed {
			msg = "model is unavailable on every account (per-model cooldown), try another model"
		}
		hint = fmt.Sprintf("model_blocked: %d account(s) cooling down this model", modelBlock.Count)
		if !modelBlock.Until.IsZero() {
			hint += "; earliest unblock at " + modelBlock.Until.Format(time.RFC3339)
		}
		if s := strings.TrimSpace(modelBlock.Reason); s != "" {
			hint += "; upstream: " + s
		}
		status = http.StatusBadRequest
	}
	writeOpenAIErrorHint(w, status, code, msg, hint)
	st.status = status
	st.outcome = reqlog.OutcomeHTTPError
}

// tokensPerSecond 计算吐字速率（token/s），返回 (速率, 是否有意义)。
//
// 分母用「生成耗时」= 端到端耗时 - 首 token 等待（TTFB），不是端到端耗时。
//
// 为什么要减：不减的话，首 token 等待越长、报告速率被压得越低。同一模型换个
// 上游或网络，TTFB 从 0.3s 涨到 2s，速率能凭空掉一半——读起来像"模型变慢了"，
// 其实只是排队久了。issue #34 报的正是这个，维护者也确认「速率计算时并没有减去
// 首token到达时间」。
//
// ttfb<=0 表示没有观测：非流式回复天然没有「首个 data 帧」（日志里那一列记的是
// "-"）。此时不猜、不扣——凭空假定一个 TTFB 会把分母推向零、把速率抬成虚高，
// 比不扣更糟。只有真测到才扣。
//
// 同理，ttfb 不小于总耗时时（时钟粒度、或 TTFB 落在计时终点之后）退回端到端耗时，
// 避免零/负分母。token 数为负哨兵值（-1 = 观测缺失）时返回 false。
//
// 扣除后不足 minGenWindow 也退回端到端（issue #127）：ttfb 量的是「首个 SSE 帧
// 到达」，当上游把整个响应攒到最后一次性下发（假流式/中间层攒批刷新——首帧与
// 末帧几乎同时到）时，total−ttfb 只剩几毫秒，拿它当分母会把几百 token 除成
// 上万 tok/s 的幻数。这种形态下「生成时长」根本不可测，诚实的分母只有端到端。
//
// 用量账本（handler）与控制台流水行（logging.go）都走这一个函数：两处各算一遍时
// 口径漂移过一次（流水行漏扣 TTFB、与面板数字对不上），共用是防再次分叉的唯一办法。
func tokensPerSecond(completionTokens int64, total, ttfb time.Duration) (float64, bool) {
	if completionTokens < 0 || total <= 0 {
		return 0, false
	}
	gen := total
	if ttfb > 0 {
		if g := total - ttfb; g >= minGenWindow {
			gen = g
		}
	}
	return float64(completionTokens) / gen.Seconds(), true
}

// minGenWindow 可信生成窗口的下限：扣除 TTFB 后剩余窗口不足该值即视为「生成
// 时长不可测」，退回端到端耗时（见 tokensPerSecond 注释，issue #127）。
// 真流式下首帧到末帧通常铺满剩余窗口，200ms 远低于正常生成时长，不影响真实
// 快速输出（如缓存命中后的爆发）被如实报告。
const minGenWindow = 200 * time.Millisecond

// promptTooLongMessage 11115 透传 message：上游 body 原文（含真实 token 数/
// 上限值/requestId，客户端自行排查）；空 body 兜底为可读分类短文案（不编造原文）。
func promptTooLongMessage(body string) string {
	if strings.TrimSpace(body) == "" {
		return "prompt is too long"
	}
	return body
}

// rotateBackoff 轮转间指数退避 + 抖动（WAF 403 修复 P0-2，报告 §6）：
// 第 i 次轮转失败（continue 换号前）等待 backoffAfter(i)（500ms·2^i 封顶 8s，
// ±25% 抖动），ctx 取消（客户端断连/优雅停机）返回 false——调用方立即终止轮转
// （客户端已走，换号重试无意义）。退避是「换号前歇一下」让上游频控窗口滑过；
// 正常单号请求（首次成功）不经过本函数，零开销。
func rotateBackoff(i int, ctx context.Context) bool {
	d := backoffAfter(i)
	if d <= 0 {
		return ctx.Err() == nil
	}
	if !sleepCtx(ctx, d) {
		log.Printf("WARN: [server] rotate backoff aborted: ctx cancelled")
		return false
	}
	return true
}

// errUpstreamTimeout 上游超时的哨兵：末端出口据此给出与「没号可用」可区分的文案。
var errUpstreamTimeout = errors.New("upstream timeout")

// isUpstreamTimeout 判断这一跳的失败是否属于「上游超时 / 停滞」。超时不是账号的
// 问题，换号注定白换（同一份请求撞同一个慢上游），必须止损：不轮转、不罚号。
// 三态判定见传输层错误分支的注释。
func isUpstreamTimeout(err error, clientGone bool) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	return !clientGone && errors.Is(err, context.Canceled)
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify / ChatStreamContext 的 *Error 信封），
// 此处不再按原始 status 二次判断。仅在 chatCompletions 轮转循环内调用：内容拦截
// 会立即 400 返回，其余种类 continue 换号（continue 前由 rotateBackoff 退避）。
//
// 十一条路径，各司其职：
//   - ErrHardCredit → CooldownUntilTomorrow4AM：即时硬冷却到次日 04:00（等签到恢复）。
//   - ErrSoftRate → **排队类（6020 queue.waiting / 6021 queue.full）先分流**：
//     上游容量信号，走模型级短避让（时长取 body 的 data.retry_after，缺失 60s），
//     不罚账号、不连坐其他模型（2026-10-15：6 个 CN 号各吃一次 6021 曾把整个 CN 域
//     打死 10 分钟）。其余按优先级对齐上游重置墙钟（带「将在 … 重置」时 6004 走模型级
//     豁免、非 6004 走账号级，均不指数堆加）；无重置时间才走有界退避（soft_rate 基数起、
//     softStreak 翻倍、封顶 soft_rate_max，冷却中兜底探测不翻倍）。P1-2 后冷却时长
//     优先采信 Retry-After 头（uerr.RetryAfter，body 文案墙钟之外的头形态来源）。
//   - ErrWafBlock → 账号级软冷却（WAF 403 修复 P0-1）：**不 Disable**——WAF 403 是
//     IP/指纹维频控信号（报告 §6：双账号 403 后账号本身健康），罚过即走、到期自愈。
//     时长优先 Retry-After 头（P1-2）；缺失按 wafCooldownBase(60s) 起 · 2^softStreak
//     封顶 soft_rate_max 的既有 CooldownSoftRate 有界退避（比 429 的 soft_rate 严：
//     基数小但响应快；WAF 信号带 IP 级粘性故指数升级保底存在）。基数经 jitterDur
//     抖动（复用 backoff.go 单一抖动来源，防多账号同相位冷却到期再聚团）。
//   - ErrEdgeAuth → **零账号惩罚**（不冷却/不熔断/不 NoteError/不喂连败）：401 + 无业务
//     信封是**出口 IP 级**事实（APISIX 鉴权层拒绝页），账号完全无辜——2026-09-22 事故
//     正是此处缺失：该形态此前落 ErrClient → 喂 NoteFailures → 一次 5 分钟边缘层抖动把
//     33 个健康号各记满 5 次连败、一起降权 10 分钟 → 池子打空 → 连环 503。铁证：同批号
//     风暴中 401、风暴后同 token 立刻 200（16 个可复现）。「停止继续打上游」的职责交给
//     IP 级状态机（handler 轮转循环的 edgeGate.noteEdgeAuth，短窗多号即 fail-fast），
//     不靠惩罚账号实现。同 ErrContentBlocked/ErrPromptTooLong 一类的「非账号问题」。
//   - ErrNotFound → Cooldown(CoolSoft, notFoundCooldown 固定 60s)：短冷却防雪崩，不随 soft_rate 退避。
//   - ErrSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//   - ErrContentBlocked → **本函数零动作**；惩罚由 chatCompletions 的「行为判别」
//     路径施加（2026-09-29）：先换号，后续有号成功才喂 NoteContentBlockEvidence
//     （账号级拦截），整轮都被拦则回 400 且零惩罚（内容问题）。
//     passthrough/append 模式首遇仍触发降级重试（system 指纹误报专用）。
//   - ErrBadParams → 不罚账号（无冷却/熔断/NoteError，同 ErrContentBlocked 待遇），但仍轮转。
//   - ErrPromptTooLong → 11115「prompt is too long」：请求的问题不是账号的问题
//     （同一 body 换任何号都超限）。零动作（不冷却/不熔断/不 NoteError、不喂连败，
//     同 ErrContentBlocked 待遇），chatCompletions 已直接透传原文返回不轮转——
//     该分支只为文档完备，不指望走到换号路径。
//   - ErrImageInvalid → 图片格式/数据无效：请求的问题不是账号的问题（同一 body
//     换任何号都会得到相同的解析错误）。零动作（不冷却/不熔断/不 NoteError、
//     不喂连败），chatCompletions 已直接透传原文返回不轮转。
//   - ErrServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。
//   - ErrModelBlocked → BlockModelBackoff：(账号, 模型) 11102 负缓存避让（复用 modelCooldowns
//     机制，Until=指数退避 TTL，选号侧 healthyForModel 避开，切模型即可用）。
//   - 其他（default：ErrClient/ErrNone）→ 只换号不罚（防雪崩），不喂熔断；ErrClient
//     额外喂连败计数（NoteFailures，issue #114）：未知 4xx 连败 N 次临时出池——
//     「不知道原因的兜底」，与冷却「知道原因的惩罚」并存取更长者不叠加（health
//     或门；带权威分类的错误不喂连败，防重复计罚）。ErrNone 零防御路径不喂。
//
// body 仅在 ErrSoftRate 分支用于识别上游 6004 模型级限流并解析重置时间；model 为请求
// 携带的模型名（触发 6004 时记录以便后续切模型豁免）。uerr 是 ChatStreamContext 返回的
// 分类信封（可携带 RetryAfter，P1-2）；零值/防御路径下为 nil，冷却时长回落既有计算。
//
// 恢复出口：CoolSoft/CoolHard 各自到期自动恢复；熔断按其指数退避截止到期；
// 成功（NoteSuccess）清 fails/熔断；签到解冻（ReenableIfCredits→reviveCoolingLocked）只清冷却，不动熔断。
func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind, body, model string, uerr *upstream.Error) {
	switch kind {
	case upstream.ErrHardCredit:
		// 402 + 余额关键词即积分耗尽：同步冷却到次日 04:00（签到任务 09/21 点恢复），
		// 不需要异步核查（冗余）。立即换号。
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "余额不足")
	case upstream.ErrSoftRate:
		// 域权重（realmweight.go）：本次限流记到该账号所属域上，裸名请求（realm=""）
		// 下次选号会优先避开这个域。放在分支最前，让 14003 / 6004 / 6020 / 6021 /
		// 普通 429 全部统一计入——它们都是「这个域此刻不好过」的信号。
		// 2026-10-19 实测：cn 域 45 次请求 0 限流、global 域 86 次 21 限流（19.6%），
		// 而裸名请求按账号数（35:6）灌向 global，权重能把这份差异反馈进选号。
		h.cfg.Pool.NoteRealmRateLimited(uid, model)
		// 排队类（6020 queue.waiting / 6021 queue.full）——上游**容量**信号，最先判。
		//
		// 为什么必须与普通 429 分流（2026-10-15 实测）：排队意味着"上游此刻接不下这个
		// 模型"，账号本身健康、号内额度也在。若按账号级 600s 冷却：
		//   - 6 个 CN 号各吃一次 6021（10:43:28–10:43:41 一次请求就转完 6 个）→
		//     全部账号级冷却 600s → **整个 CN 域所有模型一起死**（实测同期
		//     cn:glm-5.3 / cn:fast-model 均报 rate_limit_exceeded）；
		//   - 上游队列往往几十秒就排空，我们却自伤 10 分钟。
		// 故走**模型级**避让：时长优先取上游给的 data.retry_after（6020 实测 40s），
		// 缺失回落 queueCooldownDefault；只让该模型在这个号上歇一会儿，
		// 其他模型照常服务，且账号本身不被罚。
		if upstream.IsQueueBusy(body) {
			d := queueCooldownDefault
			if v, ok := upstream.ParseQueueRetryAfter(body); ok {
				d = v
			}
			h.cfg.Pool.CooldownSoftForModel(uid, d, time.Now().Add(d), model, upstream.QueueBusyReason)
			return
		}
		// 模型繁忙（14003）——**模型级容量**信号，与排队同类处理。
		//
		// 上游 displayMsg 原文就写着「Model busy. Please switch models or retry later」，
		// 语义上与 6004（用量超限，必须切模型）、6020/6021（队列满，等几十秒）并列，
		// 唯独不表示「这个账号在限流」。之前它落末尾账号级兜底 → 每次 600s、softStreak
		// 指数翻倍封顶 2h，把这个号的所有模型一起停掉。
		//
		// 2026-10-19 生产实测：41 个号的池子，1 小时内 73 次 14003 打到 35 个号，
		// 而成功请求也只用到 37 个号——几乎「用过就中」，等于每小时把池子刷一遍。
		// 改走模型级 60s 避让后，只让该模型在这个号上歇一分钟，账号与其余模型照常。
		if upstream.IsModelBusy(body) {
			// 传非零 resetAt 是关键：CooldownSoftForModel 在 resetAt 为零值时走
			// 「账号级有界退避」分支（写 e.until + softStreak++），那就又变回
			// 600s×2^n 的账号冷却了。这里给 now+d，与排队类同一调用形态，
			// 只写 modelCooldowns[model]、不动账号级 until。
			d := queueCooldownDefault
			// Capacity 口径：14003 是逐请求的容量准入（2026-10-20 直连实测同一账号
			// 5~6s 内 429↔200 反复），**不解除已有会话绑定**——否则一次饱和就把整个
			// 会话踢到别的号，上游前缀缓存按 uid 隔离，几百 k 的上下文要重算。
			// 其他会话照常避开这个号（healthyForModel 不看 Capacity）。
			h.cfg.Pool.CooldownCapacityForModel(uid, d, time.Now().Add(d), model, upstream.ModelBusyReason)
			return
		}
		// modelRateLimited 判定该 429 是否明确指向模型级限流（业务 code 6004）：
		// 是则写模型级独立冷却（切模型豁免），否则走账号级软冷却。
		// [上游 dbd7c68..origin/main：无重置时间时挂 AuditOnly 展示项]
		modelRateLimited := upstream.IsModelRateLimit(body)
		// 统一对齐上游重置时间（重构核心）：只要 body 带「将在 … 重置」，无论业务
		// code 是 6004 还是 11140 rate-limiting 等形态，都精确冷却到该墙钟、绝不
		// softStreak 指数堆加。
		//   - 模型级（6004）→ CooldownSoftForModel：写 modelCooldowns[model]，切模型
		//     豁免（既有 issue #31 语义）。
		//   - 账号级（非 6004）→ CooldownSoftRate：写账号级 until，不产生模型豁免
		//     （普通账号级限流不该因切模型绕过）。
		if resetAt, ok := upstream.ParseRateReset(body); ok {
			if modelRateLimited {
				h.cfg.Pool.CooldownSoftForModel(uid, h.softCooldown(), resetAt, model, "6004 model rate limit")
				return
			}
			h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), resetAt, "429 rate limit")
			return
		}
		// P1-2：body 无重置文案但带 Retry-After 头 → 冷却到该时刻（不做指数堆加，
		// 与重置墙钟同一对齐语义）。头优先于「有界退避」，但**低于** body 重置文案
		// （上方已 return）——文案是上游更权威的口径（WAF 403 报告 §2.1：intl CLI
		// 同序，Retry-After 也只在无重置文案时兜底）。
		if uerr != nil && uerr.RetryAfter > 0 {
			h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), time.Now().Add(uerr.RetryAfter), "429 rate limit (retry-after)")
			if modelRateLimited {
				h.cfg.Pool.RecordModelRateLimitAudit(uid, model, "6004 model rate limit (reset unknown)")
			}
			return
		}
		// 无重置时间 → 账号级有界退避（soft_rate 基数起、softStreak 翻倍、封顶
		// soft_rate_max）；已在冷却中的兜底探测不翻倍（见 CooldownSoftRate）。
		h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), time.Time{}, "429 rate limit")
		if modelRateLimited {
			h.cfg.Pool.RecordModelRateLimitAudit(uid, model, "6004 model rate limit (reset unknown)")
		}
	case upstream.ErrWafBlock:
		// P0-1：WAF 403（无业务信封拦截形态）。软冷却复用 CooldownSoftRate 家族
		// （不新建平行冷却系统）：基数 wafCooldownBase（60s，抖动后落 [45s,75s]）、
		// softStreak 指数升级、封顶 soft_rate_max、冷却中兜底探测不翻倍——全部继承
		// 既有语义。Retry-After 头优先（P1-2，WAF 拦截页可能带该头）。不 Disable。
		if uerr != nil && uerr.RetryAfter > 0 {
			h.cfg.Pool.CooldownSoftRate(uid, jitterDur(wafCooldownBase), time.Now().Add(uerr.RetryAfter), "waf 403 block (retry-after)")
			return
		}
		h.cfg.Pool.CooldownSoftRate(uid, jitterDur(wafCooldownBase), time.Time{}, "waf 403 block")
	case upstream.ErrEdgeAuth:
		// 边缘层鉴权拒绝（401 + 无业务信封，APISIX「401 Authorization Required」页）：
		// **零账号惩罚** —— 不冷却、不熔断、不 NoteError、**不喂连败计数**。
		// 理由：这是出口 IP 级事实，账号完全无辜。2026-09-22 事故正是此处缺失造成的
		// ——该形态此前落 ErrClient 兜底 → 喂 NoteFailures → 一次 5 分钟边缘层抖动
		// 让 33 个健康号各记满 5 次连败、一起降权 10 分钟 → 池子被打空 → 连环 503。
		// 铁证：同一批号在风暴中 401、风暴后同 token 立刻 200（16 个号可复现）。
		// 「停止继续打上游」的职责交给 IP 级状态机（h.edgeGate.noteEdgeAuth，
		// 短窗多号命中即 fail-fast 终止轮转），不靠惩罚账号来实现——惩罚账号既治不了
		// IP 级问题，又白白废掉健康号。
		// 本分支与 ErrContentBlocked/ErrPromptTooLong 同属「请求/环境问题非账号问题」
		// 一类：零动作，仅由轮转循环换号（且 IP 级激活时连轮转都会终止）。
	case upstream.ErrSessionDead:
		h.cfg.Pool.Disable(uid, "12153 session dead")
	case upstream.ErrNotFound:
		// 404 短冷却（软冷却），防雪崩。固定 notFoundCooldown，不随 soft_rate 退避：
		// 偶发路径缺失不是限流信号，不该按限流惩罚升级。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, notFoundCooldown, "upstream 404")
	case upstream.ErrAccountFault:
		// 账号级授权/配额故障按 msg 分野（口径与 Classify 的 accountFaultMarkers 一致）：
		//   - "request illegal"（code 11140）→ 账号级**授权封禁**：软冷却到期也不会自动
		//     恢复（需重新 OAuth 登录），到期后重新选号只会再撞 403 浪费一次轮换——
		//     硬禁用（Disable），不再参与选号。/status 以 disabled + disabled_reason 呈现。
		//   - 14017（trial not activated）→ register 未完成，补完 register 后可能自愈，
		//     **保持软冷却**（禁用会让用户补完 register 后仍无法用）。
		// 两条路径对坏号都立刻换号（同一请求轮转出池），只是后续可恢复性不同。
		// 大小写不敏感（与 Classify 的 marker 匹配同口径）。
		if strings.Contains(strings.ToLower(body), "request illegal") {
			h.cfg.Pool.Disable(uid, "account banned by upstream (11140 request illegal), re-login required")
			return
		}
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.softCooldown(), "account fault (14017)")
	case upstream.ErrServer:
		// 5xx 上游故障：Classify 已把 ≥500 判为 ErrServer，在此喂熔断计数（不再手写 status>=500）。
		h.cfg.Pool.NoteError(uid)
	case upstream.ErrContentBlocked:
		// 内容策略拦截：**本分支恒为零动作**（不冷却/不熔断/不 NoteError）。
		// chatCompletions 收到本 kind 后先换号（2026-09-29 行为判别修复）：
		//   - 后续有号成功 → 由成功分支调 NoteContentBlockEvidence 施加惩罚
		//     （证据确凿的账号级拦截：软冷却让位 → 达阈值 Disable）；
		//   - 整轮都被拦 → 末端回 400 content_blocked，零惩罚（内容问题）。
		// 惩罚与判定分离在**两个不同位置**是刻意的：这里无法知道「是账号问题还是
		// 内容问题」（上游对两者回同一句 displayMsg），只有拿到换号结果才能判定。
	case upstream.ErrPromptTooLong:
		// 11115「prompt is too long」：请求的问题不是账号的问题（同一 body 换任何
		// 号都超限）。零动作（不冷却/不熔断/不 NoteError，同 ErrContentBlocked
		// 待遇），chatCompletions 已直接透传原文返回不轮转——该分支只为文档完备，
		// 不指望走到换号路径。
	case upstream.ErrImageInvalid:
		// 图片格式/数据无效：请求的问题不是账号的问题（同一 body 换任何号都会
		// 得到相同解析错误）。零动作，chatCompletions 已 fail-fast 透传。
	case upstream.ErrUpstreamRejected:
		// 上游「请求被拦截」（code 11-128）：**请求级**拒绝，与账号授权状态无关
		// （2026-10-16 实测误禁 12 号的那批错误即此形态）。零动作（不冷却/不熔断/
		// 不 NoteError，同 ErrBadParams 待遇）；chatCompletions 已 fail-fast 400
		// 透传原文、终止轮转——该分支只为文档完备，不指望走到换号路径。
	case upstream.ErrBadParams:
		// 请求体解析失败（400 + Unmarshal chat params failed / 11101）：发给上游的 body
		// 有问题（网关侧不再截断，均为客户端畸形 JSON）。换了账号照样 400，
		// 不罚账号（无冷却/熔断/NoteError，同 ErrContentBlocked 待遇）；chatCompletions
		// 已 fail-fast 400 透传原文、终止轮转——「换号可能有不同模型权限」属 11102
		// （ErrModelBlocked）的分类域，与本类无关。
	case upstream.ErrModelBlocked:
		// 11102「该后端无此模型」：(账号, 模型) 负缓存避让。复用 modelCooldowns 机制
		// （与 6004 同域），写 modelCooldowns[model]，Until 为指数退避 TTL（6h 起、封顶
		// 24h）。选号侧 healthyForModel 对该账号自动避开该模型；切模型/切账号即可用。
		// 立即换号（本轮 continue），该账号该模型冷却，下次选号避开。
		h.cfg.Pool.BlockModelBackoff(uid, model, upstream.ModelBlockReason)
	default:
		// 其余（ErrClient/ErrNone）：只换号不罚（防雪崩），不喂熔断。
		// ErrClient（未知 4xx）喂连败计数（issue #114）：连续 N 次该形态失败 →
		// 账号临时出池（NoteFailures 达阈降权），单次/偶发不罚（不误伤）。ErrNone
		// 到这里属防御路径（status>=400 但分类成功），语义不明不喂。
		if kind == upstream.ErrClient {
			h.cfg.Pool.NoteFailures(uid)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}

// writeOpenAIErrorHint 同 writeOpenAIError，另在 error 对象上附加
// error.gateway_hint（hint 为空串时不带字段——未覆盖形态不编造）。
// message 仍是上游原文透传（hint 只做并列补充，绝不替换/包装 message）。
func writeOpenAIErrorHint(w http.ResponseWriter, status int, code, msg, hint string) {
	if hint == "" {
		writeOpenAIError(w, status, code, msg)
		return
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message":      msg,
			"type":         "api_error",
			"code":         code,
			"gateway_hint": hint,
		},
	})
}

// hasImagePart 报告聊天请求体是否携带多模态 image_url part（OpenAI 兼容形态
// messages[].content[] {type:"image_url"}）。畸形/其他形态一律 false（hint 侧
// 宁缺勿滥：判不出带图就不给「模型不支持图片」指向）。
func hasImagePart(body []byte) bool {
	var peek struct {
		Messages []struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &peek) != nil {
		return false
	}
	for _, m := range peek.Messages {
		for _, p := range m.Content {
			if p.Type == "image_url" {
				return true
			}
		}
	}
	return false
}

// hintContext 组装 chatCompletions 的 gateway_hint 判定上下文：请求裸模型名 +
// 是否带图 + 模型目录 supports_images 声明（目录未收录 → ModelInCatalog=false，
// 不做「不支持」判定，防查不到误判）。仅错误路径调用（成功请求零开销）。
//
// 目录查询只读既有缓存快照（cachedModelsSnapshot），**不触发上游拉取**：错误路径
// 加一次 FetchModels 网络调用既拖慢错误响应、又污染上游调用语义（错误风暴时放大
// 请求量——与 WAF IP fail-fast 的「不放大请求量」哲学相悖）。缓存冷（最近 1h 未
// 拉过）→ ModelInCatalog=false，11133 退中性 hint（宁缺勿滥，不编造能力事实）。
func (h *Handler) hintContext(bareModel string, hasImage bool) upstream.HintContext {
	ctx := upstream.HintContext{Model: bareModel, HasImage: hasImage}
	if bareModel == "" {
		return ctx
	}
	for _, mi := range cachedModelsSnapshot() {
		if mi.ID == bareModel {
			ctx.ModelInCatalog = true
			ctx.ModelSupportsImages = mi.SupportsImages
			return ctx
		}
	}
	return ctx
}

// cachedModelsSnapshot 只读模型目录缓存（TTL 内快照）；缓存冷/空 → nil。
// 不发起任何上游调用（与 fetchDynamicModels 的差异点，见 hintContext 注释）。
func cachedModelsSnapshot() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	if len(dynamicModelsCache.ids) == 0 || time.Since(dynamicModelsCache.fetched) >= dynamicModelsTTL {
		return nil
	}
	return dynamicModelsCache.ids
}

// hintOf 末端错误透传的统一 hint 入口：kind + 上游原文 + 请求上下文 →
// gateway_hint 文案（upstream.GatewayHint 单一事实来源）。uerr 为 nil 时回落
// body 原文判定（防御路径）。transport 层错误（lastErr 非 *upstream.Error 且
// 上游没回 body）→ 无 hint（不编造）。
func (h *Handler) hintOf(kind upstream.ErrKind, body, bareModel string, hasImage bool, uerr *upstream.Error) string {
	msg := body
	if uerr != nil && uerr.Msg != "" {
		msg = uerr.Msg
	}
	return upstream.GatewayHint(kind, msg, h.hintContext(bareModel, hasImage))
}
