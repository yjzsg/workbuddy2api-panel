// 账号状态机迁移的唯一权威实现。
//
// entry 的「可选择性」由四个正交维度决定：禁用(disabled)、账号级冷却(until/coolKind)、
// 模型级冷却(modelCooldowns)、熔断(breakerUntil)。维度之间以「迁移原语」收拢，
// 禁止在其他文件散写这些字段——所有入口（applyErrorPolicy / refresh / keepalive /
// 签到 / 选号）对状态的改动都必须经本文件的原语或经 Cooldown/NoteError/NoteSuccess 等
// 封装（它们在持锁下调用本文件原语）。
//
// 迁移矩阵（事件 → 动作 → 字段）：
//
//	disabled           ← disableLocked（Disable / NoteSessionDead 达阈）
//	until/coolKind     ← Cooldown(CoolSoft/Hard，固定时长) / CooldownSoftRate / CooldownSoftForModel 无解析分支
//	modelCooldowns     ← CooldownSoftForModel 有解析分支；被 disableLocked/Cooldown/clearCoolingLocked 清
//	breakerUntil       ← recordBreakerFailureLocked（NoteError 喂入）；NoteSuccess 清
//	softStreak         ← CooldownSoftRate / CooldownSoftForModel 无解析分支；NoteSuccess/reviveCoolingLocked 清
//	sessionDeadFails   ← NoteSessionDead；ClearSessionDead/NoteSuccess/ReviveDisabled 清
//
// 关键正交性（疑点 4 修正）：
//   - 冷却域（until/coolKind/softStreak/modelCooldowns）与熔断器（fails/retryCount/
//     breakerUntil）正交：冷却管「近期被限流/余额耗尽」，熔断管「反复 5xx 失败」。
//     disableLocked 只清冷却域、不动熔断——禁用是授权/session 终态，不应覆盖熔断观测。
//   - clearCoolingLocked 是「冷却域归零」的单一来源，被 disableLocked 与
//     reviveCoolingLocked（签到解冻）共用，二者对冷却域的处置因此永远一致。
package pool

import "time"

// clearCoolingLocked 清冷却域：until/coolKind/softStreak/modelCooldowns 全归零，
// reason 一并清空。熔断器（fails/retryCount/breakerUntil）不属冷却域，不动。
// 调用方必须已持有 p.mu。
func (e *entry) clearCoolingLocked() {
	e.until = time.Time{}
	e.coolKind = 0
	e.reason = ""
	e.softStreak = 0
	e.modelCooldowns = nil // 冷却域清零时一并清模型级独立冷却（模型豁免随之消失）
}

// disableLocked 禁用迁移：置 disabled 并清冷却域（禁用是比冷却更强的不可用终态）。
//
// 疑点 4 修正：旧 Disable 只置 disabled+reason，不碰 until/modelCooldowns/softStreak，
// 会出现「disabled=true 但 cooling=true / 残留 modelCooldowns」的一致性问题——一个
// 先被硬冷却（到次日 04:00）再被禁用的账号会同时呈现两种状态。禁用后冷却无意义
// （账号已退出选号，冷却截止不再被读取），故一并清空。
//
// 熔断器保留：熔断是「连续 5xx 失败」信号（与授权/会话无关），禁用后再复活时
// 熔断观测仍有效，不应被禁用覆盖。
func (p *Pool) disableLocked(e *entry, reason string) {
	e.clearCoolingLocked()
	e.disabled = true
	e.reason = reason
	p.dirty.Store(true)
}

// reviveCoolingLocked 解冻「余额型冷却」并更新 credits，不动熔断器
// （fails/retryCount/breakerUntil）。签到/余额刷新走这里：签到成功只证明余额恢复与
// billing 通道健康，不证明 chat 通道健康，熔断（连续 5xx 信号）不应被签到覆盖。
//
// **只清 CoolHard**（余额不足 → 冷却到次日 04:00）：调用方都以「余额恢复」为依据，
// 而 CoolSoft（429/6004 配额窗口）的解除条件是上游配额重置，与 credits 无关 ——
// 无条件清会让每 5 分钟的余额刷新把限流冷却抹掉，形成「冷却 → 刷新解冻 → 再撞」死循环。
// 软冷却到期后由 healthy 判定自然放行；需强制解冻走 Pool.Revive。
// 调用方必须已持有 p.mu。
func (p *Pool) reviveCoolingLocked(e *entry, credits int64) {
	e.credits = credits
	// 只解冻「余额型冷却」（CoolHard）：本函数两个调用方（签到 CheckinAll、余额后台
	// 刷新 RunBalanceRefreshNow —— 后者每 5 分钟一次）都以「余额恢复」为解冻依据，而
	// 余额充足**不代表限流解除**：CoolSoft（429/6004 的配额窗口）的解除条件是上游
	// 配额重置，与 credits 无关。
	//
	// 原实现无条件 clearCoolingLocked，会把限流冷却一并抹掉：账号撞 6004 → 冷却 8
	// 分钟 → 5 分钟内的余额刷新把它解冻 → 立刻又被选中 → 再撞 6004，形成
	// 「冷却 → 刷新解冻 → 再撞」死循环（实测 账号C 反复 6004）。
	// 需要强制解冻（含限流/熔断）时走面板「解冻」按钮 → Pool.Revive（显式全清）。
	if e.coolKind == CoolHard {
		e.clearCoolingLocked()
	}
}
