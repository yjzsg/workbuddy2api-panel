# workbuddy2api-panel ← 根上游同步移植清单

生成时间：2026-09-17 00:35 (GMT+8)
对比对象：`Sliverkiss/workbuddy2api`（根上游） → `linguo2625469/workbuddy2api-panel`（我们跑的）
部署位置：NAS `fnos` (`<面板IP>`) `<家目录>/docker/workbuddy2api-panel`，容器 `workbuddy2api`，端口 `7863`

---

## 0. 三条必须先接受的事实

| 事实 | 证据 |
|---|---|
| **落后 59 个提交** | 面板 HEAD `3f55d504`(=v1.9.2-panel)，其根提交 `88545c1 chore: 版本号 1.9.1-panel` = 上游 2026-09-16 00:33:38 的代码快照。上游该时点后共 **59 提交**（北京 09-16 00:52 → 09-17 00:26），且**仍在每小时推** |
| **不能用 git 同步** | 面板仓库 `git rev-list --count HEAD` = **9**，与上游**无共同祖先**（作者每次同步是重置历史再叠自己的改动）→ merge/rebase/cherry-pick 全部不可用 |
| **模块路径不同** | 上游 `module workbuddy2api`；面板 `module github.com/linguo2625469/workbuddy2api-panel`。搬任何上游文件都必须改写 import 前缀（sed 可脚本化） |

**所以只有两条路**：① 按主题手工移植（外科手术）；② 以上游为主干重贴面板层（换基）。见 §2。

---

## 1. 分层地图：哪些是面板的、哪些要跟着上游走

| 层 | 内容 | 处置 |
|---|---|---|
| **L0 部署补丁** | `Dockerfile`（删 `# syntax=` 行 + GOPROXY）、`docker-compose.yml`（entrypoint → `/app/data/config.json`、PUID 10001）、`auths/`+`data/` 属主 | ⛔ **永不被上游版本覆盖** |
| **L1 面板 UI 层** | `internal/panel/*`(3.4k行) `internal/httpauth` `internal/livecfg` `internal/usage` | ⛔ 保留（上游**没有**任何 UI） |
| **L2 面板独有功能** | `internal/upstream/{desktop,global_register,school,tasks,blackcat,streak}.go` `internal/scheduler/{blackcat,streak}.go` | ⚠️ 逐个判定，见 §4 |
| **L3 上游核心** | 其余全部（auth/pool/server/session/upstream/scheduler/redisstore/cmd） | ✅ 跟随上游 |

**重要观察**：面板增强层**接入点很薄** —— `handler.go` 里只有 `Live *livecfg.Holder`(L51) 与 `Usage *usage.Recorder`(L67) 两个字段 + 一处 `h.cfg.Usage.Add(...)`(L510)，`client.go` 里几乎没有 hook。**这是换基可行的关键依据**。

---

## 2. 路线对比与建议

| 路线 | 工作量 | 风险 | 收益 | 后续维护 |
|---|---|---|---|---|
| **A 主题移植**（推荐先做） | 分批，每批 1–3 小时 | 低（每批可独立回滚） | 按需拿最重要的能力 | 每次上游更新都要重来一遍 |
| **B 全量换基** | 一次 1–2 天 | 中（scheduler/main/config 接线要重写） | **一次到位**，此后同步只差 UI 层 | 上游更新变成 rsync + 修 hook，长期最省 |
| **C 最小止血** | 30 分钟 | 极低 | 只解决当下 IP 频控 | 治标 |

**建议**：**A 的 P0 批次 + B 的试编译（P4-0）并行** —— 先拿止血能力，同时用一次"假换基"把真实冲突量量出来（不落地，只在 `/vol4` 里试编译），有了数字再决定要不要做 B。

---

## 3. 移植批次清单（按优先级，可直接勾）

### 🔴 P0 · 止血批（独立、低耦合，建议第一个做）

- [ ] **P0-1 WAF IP 级 fail-fast**
  - 新增 `internal/server/wafip.go`（80 行，自足：`wafIPGate` / `noteWaf` / `active`，60s 滑窗 ≥2 个**不同** UID 命中 WAF 403 → 激活 60s 跳过轮转）
  - 前置依赖：`ErrWafBlock` 分类（面板**没有**）→ 改 `internal/upstream/client.go`：ErrKind 增加 `ErrWafBlock`（403 + 非业务信封体）+ `Retry-After` 头族解析
  - 接线：`handler.go` 加字段 `wafIP wafIPGate`(上游 L82)；调用点两处 —— L751 `noteWaf(acct.UID)`、L838 `if h.wafIP.active()`
  - 带测试：`internal/server/waf_test.go`、`internal/upstream/client_test.go` 的 waf 用例
  - 验收：单测过；构造同 IP 两号连续 403 → 日志出现 IP 级拦截且**不再轮转**

- [ ] **P0-2 轮转退避单一来源**
  - 新增 `internal/server/backoff.go`（75 行：`jitterDur` / `backoffAfter` / `sleepCtx`，base 500ms、封顶 8s、±25% 抖动）
  - 接线：`handler.go` 的 `rotateBackoff(i, ctx)` 两处（上游 L623/L644）。**面板当前无 `rotateBackoff`**，需一并引入
  - 效果：轮转不再"零延迟连环打"上游，减轻风控

- [ ] **P0-3 11115「prompt is too long」→ 请求级错误**
  - `client.go`：ErrKind 增加 `ErrPromptTooLong`（11115）
  - `handler.go`：**不罚号、不轮转、末端透传原文**
  - 注意：上游曾上线"出站前 token 预估"又被 Revert（`6a4dd4c`/`4c16e45`）→ **不要移植预估那部分**，只移植分类 + 透传

- [ ] **P0-4 工具调用残缺参数检测**
  - 新增 `internal/upstream/truncation.go`（47 行：`isTruncatedArguments` / `dropTruncatedToolCalls`）
  - 新增 `internal/upstream/tool_pairing.go`（226 行：tool 配对对称裁剪 / `repackToolResultBlocks`，防 11148 顶死会话）
  - 接线：`client.go` / `sse.go` 流式解析出口
  - **这一条直接对应我们踩过的"客户端卡死"**：SSE 被截断时 arguments 只剩半截 JSON，原样下发 → 客户端非法 JSON → 会话卡死

### 🟠 P1 · 模型元数据四级链（替换我们的 PR#18 硬编码）

- [ ] **P1-1** `internal/upstream/context_catalog.go`（106 行）—— 字段级知识表；**零值不再透出假 `131072`，未知落 1M**（宁高估不低估）
- [ ] **P1-2** `internal/upstream/model_catalog.go`（320 行）+ `model.json` 本地缓存（第 3 级，种子迁移 + 并发安全 + 损坏降级）
- [ ] **P1-3** `internal/upstream/modelsdev.go`（306 行）—— 第 4 级按需异步拉取；**必须同时带上两个内存泄漏修复**：负缓存 TTL 到期回收（`064e505`/`4092853`）、goroutine 泄漏冒烟
- [ ] **P1-4** `handler.go` `/v1/models` 接四级链 + `model.json` 路径接线（与 `state.json` 同风格）
- [ ] **P1-5** `internal/upstream/effort_catalog.go`（145 行）—— 推理档位（按 realm 分表）
- ⚠️ **冲突点**：面板现有 `applyModelInfoFields` / `modelEntry` / `fetchGlobalModelInfos` 是 PR#18 手写补丁。**先定谁为准**（建议：`global:*` 命名保留面板的，元数据取值走上游四级链）

### 🟡 P2 · 账号池与稳定性（收益大但触及 pool 深处）

- [ ] **P2-1** `internal/pool/degrade.go`（60 行）连败降权 + config 三参数 `degrade_threshold` / `degrade_cooldown` / `degrade_cooldown_max`
  - 配套：`8fbbe21` ErrClient 与传输层失败喂连败计数
- [ ] **P2-2** `Acquire` 未知 uid 空 entry 解引用 **panic**（`94e4e61`）
- [ ] **P2-3** 全冷却兜底选号未推进 `usedSeq` → LRU 误判「最旧」（`49930b2`/`0c10de3`）
- [ ] **P2-4** 本地 `state.json` 缺失时丢弃有效 Redis 快照 + 假日志（`c768f48`/`d2cd004`）
- [ ] **P2-5** `modelCost` 过期条目永不回收 → 补 `pruneExpiredModelCosts`（`dcc4918`/`64064ce`）
  - ⚠️ **先比对**：上游「成本台账单模型粒度持久化 + `/status` 透出」（`c5dc4e3`/`2493532`）与面板 `internal/usage/usage.go`(541 行) 可能功能重叠 → 定谁作为唯一事实来源
- [ ] **P2-6** session 粘性剔 `user_id`（剩四键全 conversation 维度）+ GC goroutine 关停竞态/泄漏（`ebd7921`/`2b8dba0`/`af51945`）
- [ ] **P2-7** `redisstore` goWrite 抢槽丢写 / Close 排空竞态（`5f10a9c`/`2b662b9`）
- [ ] **P2-8** `auth` 出站请求头锁外直读 token/domain 数据竞争（`910b8b2`/`722ee19`）
- [ ] **P2-9** 删成功率 EMA 因子（四因子 → 三因子，`9a7e866`）

### 🟢 P3 · 连接层与功能（可选）

- [ ] **P3-1** `internal/upstream/transport.go`（110 行）连接加固：真禁 h2 / Dial+TLS 握手超时 / 短 keepalive / 失败清池
- [ ] **P3-2** `internal/upstream/hint.go`（147 行）+ `StreamHint` —— 错误附加 `error.gateway_hint` 并列字段（非流式 + SSE 两变体，message 仍原样透传不包装）
- [ ] **P3-3** global 域 `max_in_flight` 分档（风控紧域压低单号并发）
- [ ] **P3-4** `Classify 429` 前移到 hardRule 之前（带 quota 措辞不再误判硬冷却）
- [ ] **P3-5** `cmd/activity/main.go`（124 行）一次性触发器 + `internal/config/schedule.go`（133 行）—— **对我们"活跃上报/邀请激活"有直接价值**（可 `docker cp` 进容器执行）
- [ ] **P3-6** `pool/persist_unix.go` / `persist_other.go` 平台拆分（顺带，非必须）
- [ ] **P3-7** 错误响应透传：移除末端规范化固定文案（`5755fe3`）；`max_completion_tokens → max_tokens` 翻译（`edb9e97`）

### 🔵 P4 · 换基（✅ 已试编译量化：可行且推荐，见 §8）

- [x] **P4-0 试编译量化 —— ✅ 2026-09-17 00:45 完成，结论见 §8**（层 1/2 已修，层 3 报 16 处，均在 `internal/panel`）
  ```bash
  U=/vol4/_upstream_wb2api ; P=<家目录>/docker/workbuddy2api-panel
  rm -rf /vol4/_rebase_try && mkdir -p /vol4/_rebase_try
  rsync -a --exclude=.git --exclude=auths --exclude=data --exclude='config.json' \
        --exclude='Dockerfile*' --exclude='docker-compose*' $U/ /vol4/_rebase_try/
  # 贴回面板独有层
  for d in internal/panel internal/httpauth internal/livecfg internal/usage; do
      cp -a $P/$d /vol4/_rebase_try/$d ; done
  cp -a $P/internal/upstream/{desktop,global_register,school,tasks,blackcat,streak}.go /vol4/_rebase_try/internal/upstream/
  cp -a $P/internal/scheduler/{blackcat,streak}.go /vol4/_rebase_try/internal/scheduler/
  # 模块前缀改写（上游 → 面板）
  grep -rl '"workbuddy2api/internal/' /vol4/_rebase_try | xargs sed -i \
      's#"workbuddy2api/internal/#"github.com/linguo2625469/workbuddy2api-panel/internal/#g'
  sed -i 's#^module workbuddy2api#module github.com/linguo2625469/workbuddy2api-panel#' /vol4/_rebase_try/go.mod
  # 试编译（NAS 无 go，用容器；镜像拉不动就退回用项目 Dockerfile 构建）
  docker run --rm -v /vol4/_rebase_try:/src -w /src \
    -e GOPROXY=https://goproxy.cn,direct golang:1.23-alpine \
    sh -c 'go build ./... 2>&1 | head -80; echo "--- 错误数 ---"; go build ./... 2>&1 | grep -c "" '
  ```
  **产出**：编译错误条数 + 涉及文件清单 → 这就是换基的真实工作量
- [x] **P4-1 换基修复完成（2026-09-17 01:30）**：`_rebase_try_B` 上 `go build` + `go vet` + `go test`（19 包）**全绿**，层 3/4 共 37 个编译错全部修复 + 18 处测试适配，详见 §8 执行记录
- [x] **P4-2** 已写 `PORTING.md`（本机工作区 + NAS `/vol4/_porting.md` + 面板仓库根）：hook 点清单（13 组）+ 同步流程（9 个脚本按序重放）+ 8 条行为差异 + 禁止事项
- [x] **P4-3 落地完成（2026-09-17 01:40）**：备份 → rsync 替换 → 重建容器 → 验收全过，见 §9「落地记录」

---

## 4. L2（面板独有功能）退役判定

上游这一天在**主动吸收面板的功能**，`growth_bonus.go` 注释原文：

> 逆向来源：下游 panel（`/tmp/wb2a-panel internal/upstream/blackcat.go + scheduler/streak.go`）的连登管家闭环

| 面板文件 | 行数 | 上游对应 | 建议 |
|---|---|---|---|
| `upstream/streak.go` + `scheduler/streak.go` | 106 + 115 | `growth_bonus.go`(139) + `growth_reward.go`(218)，commit `243c7f2` 明写"吸收 panel 连登管家三动作" | **退役**，改用上游实现 |
| `upstream/blackcat.go`（夜猫 3 次对话 + 事件链上报） | 126 | 上游走 `scripts/` + `scheduler` 的 `cat_hours` 脚本路线 | **保留**（上游未吸收对话链） |
| `upstream/school.go`（开学季纯 API：share_invite 等） | 274 | 上游 `scheduler/school.go` 走 shell 脚本 `school_open_day_cron.sh` | 二选一：保留 API 路线，或迁上游脚本路线 |
| `upstream/tasks.go`（成长任务） | 185 | `scripts/task_runner.py` + `growth_reward.go` | 先比对判据实现再定 |
| `upstream/desktop.go`（桌面六事件链） | 582 | 上游**无** | ⛔ **必须保留** —— 我们激活好友就靠它 |
| `upstream/global_register.go`（国际版注册） | 232 | 上游**无** | ⛔ 保留 |
| `internal/panel/*` `usage` `livecfg` `httpauth` | ~3.5k | 上游**无 UI** | ⛔ 保留 |

---

## 5. 每批的通用执行流程（模板，逐批照做）

1. **打快照**（必做，非 git 方式也要）
   ```bash
   ssh fnos "tar -czf /vol4/_panel_backup_$(date +%F_%H%M).tgz -C <家目录>/docker/workbuddy2api-panel internal cmd Dockerfile docker-compose.yml"
   ```
2. **取上游文件 → 改前缀**（见 P4-0 的两条 sed）
3. **接线**：只改 hook 点，不重写上游逻辑；每次改前记录改了哪个文件哪一行
4. **本地验证**：容器内 `go build ./... && go vet ./... && go test ./...`（**把对应的 `_test.go` 一起搬**，上游有 153 个测试文件，是现成的回归网）
5. **单号灰度**：先只放 1 个账号 + 1 次请求，看日志
6. **重建容器**（`docker compose up -d --build`，参考技能 `fnos-docker-compose-deploy`）
7. **回滚**：`tar -xzf /vol4/_panel_backup_*.tgz`；⚠️ **不要用 `git clean -fd`**，会连带删掉 `activate_friend.py` / `bind_invite.sh` 等未跟踪脚本

---

## 6. 风险与反模式

- ⛔ 别用上游 `Dockerfile` 覆盖（`# syntax=docker/dockerfile:1` 在 NAS 镜像站返 **401**；且缺 `GOPROXY=goproxy.cn`）
- ⛔ 别用上游 `docker-compose.yml` 覆盖（entrypoint 与单文件挂载补丁会丢，面板保存配置立刻失败）
- ⛔ 别用上游 `config.example.json` 覆盖 `data/config.json`
- ⛔ 别直接照抄上游 `module workbuddy2api`（必须 sed，否则整个 tree 编译不了）
- ⚠️ **`diff` 行数 ≠ 工作量**：上游把 `client.go` 做了大重构（拆出 6 个新文件），diff 会显得四处都是差异，实际是"搬家"
- ⚠️ 上游带 Revert 历史的改动（token 预估那对提交）**不要跟着搬**
- ⚠️ 移植 `wafip.go` 前必须先有 `ErrWafBlock` 分类，否则编译过但永不触发（静默失效）
- ⚠️ `internal/usage`（面板）与上游成本台账功能重叠 → 定唯一事实来源，别双写

---

## 7. 验收清单（每批上线后跑）

- [ ] `docker ps` → `workbuddy2api` healthy；`curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:7863/healthz`
- [ ] `auths/` 10 个账号仍在；`/v1/models` 里 `global:*` 列表正常
- [ ] 面板 UI 回归：券码卡片 / 任务中心 / 自动任务 / 用量页
- [ ] 目标场景验证（对应批次）：IP 频控日志 / `context_length` 不再假 131072 / 截断请求不再卡死
- [ ] `settings.yaml` 与向导值未被动过（DSH 侧：`127.0.0.1:40808` / `yeying.space`）

---

## 8. ⭐ P4-0 试编译**已实跑**：换基成本已量化（2026-09-17 00:45）

**结论：换基可行，成本比预想小得多、而且高度集中。** 方法：上游树 + `sed` 模块前缀 + 贴回面板层 → 容器里 `go build`，一层层修到露底。

### 迭代过程（Go 的报错是分层的：上游包一错，下游包根本不检查）

| 轮次 | 报错数 | 位置 | 根因 | 处置 |
|---|---|---|---|---|
| 层 1 | **8** | `internal/upstream/{streak,blackcat,desktop,tasks}.go` | ① 面板 `streak.go` 与上游 `growth_bonus.go`/`growth_reward.go` 重名（上游已吸收面板功能）② 面板层依赖两个上游没有的 Client 方法 | 删面板 streak（退役）+ 删 blackcat 里 3 个重名方法 + 补回 `webBase`、`ReportChatActivityModel` ✅ |
| 层 2 | **3** | `internal/upstream/school.go` | `clientToken()` 原本住在面板 `streak.go` 里，退役时被一起删掉，而 `school.go` 也在用 | 抽成独立文件 `client_token.go` ✅ |
| 层 3 | **16** | **全部在 `internal/panel`** | 面板 UI 层用到的上游 API 被上游改了签名/改了名/面板自行加过方法 | 见下表，未修 |
| 层 4 | 被遮蔽 | `cmd/server`、`internal/server` | 依赖 `internal/panel`，须等层 3 修完才会被检查 | 用符号探测替代，见下 |

### 层 3 全文（16 处，全在面板 UI 层）

```
login.go:229  undefined: auth.BackfillRealmFor
login.go:242  p.cfg.Pool.Revive undefined
login.go:271  UserResource returns 2 values (面板按 3 值用)
login.go:273  Pool.ReenableIfCredits have (string,?,?) want (string,int64)
panel.go:246  Pool.Pick 需要 1 个参数（面板传空）
panel.go:266  mi.CanDisableThinking undefined
panel.go:326  p.cfg.Pool.Revive undefined
panel.go:360  UserResource returns 2 values
panel.go:366  Pool.ReenableIfCredits 参数不符
panel.go:381  UserResource returns 2 values
panel.go:386  Pool.SetCredits 参数不符
panel.go:393  p.cfg.Pool.Remove undefined
panel.go:469  Scheduler.RunBalanceRefreshNow undefined
panel.go:526  undefined: upstream.CreditPackage
panel.go:547  p.cfg.Upstream.CreditPackages undefined
taskcenter.go:428 Scheduler.RunSchoolAccountNow undefined
```

### 必须"贴回上游主干"的面板 API 扩展（共 11 个符号）

| 面板期望的 API | 面板里的定义位置 | 上游现状 | 修法 |
|---|---|---|---|
| `Pool.Revive` | `pool/state.go`（另有 `ReviveDisabled`） | 无 | 贴回 |
| `Pool.Remove(uid)` | `pool/pool.go:220` | 无 | 贴回 |
| `Pool.SetCredits(uid, credits, total)` | `pool/cooldown.go:9` | 签名只剩 `(uid, int64)` | 改调用点 |
| `Pool.ReenableIfCredits(uid, remain, total)` | `pool/state.go:101` | 只剩 `(uid, int64)` | 改调用点 |
| `Pool.Pick()` | — | 上游 `Pick(model string)` | 改调用点 |
| `Scheduler.Reconfigure` | `scheduler/scheduler.go` | 无 | 贴回 |
| `Scheduler.SetBalanceInterval` | `scheduler/scheduler.go` | 无 | 贴回 |
| `Scheduler.RunBalanceRefreshNow` | `scheduler/scheduler.go:386` | 无 | 贴回 |
| `Scheduler.RunSchoolAccountNow(a)` | `scheduler/school.go:45` | 无 | 贴回 |
| `server.Handler.SetMaxBodyBytes` | `server/handler.go` | 无（面板为 issue #17 加的） | 贴回 |
| `auth.BackfillRealmFor(a, realm)` | `auth/auth.go:117` | 无（上游删了 auth 死代码） | 贴回 |
| `upstream.CreditPackage` / `Client.CreditPackages` | `upstream/client.go:1075/1099` | 改名/重构为 `CreditBuckets` + `UserResourceDetailed` | 改调用点 |
| `Client.UserResource` 返回 3 值 | `upstream/client.go:1187` | 上游返回 2 值 | 改调用点 |
| `ModelInfo.CanDisableThinking` | 面板 `server/handler.go:335` 使用 | 无该字段（面板加的） | 贴回 |

→ **合计约 14 个适配点**（贴回 8 个 + 改调用 5 个 + 改字段 1 个），落点在 5 个上游文件；面板侧调用点改写约 20 处（集中在 `internal/panel`）。

### 复现资产（都在 NAS，下次直接接上）

| 路径 | 内容 |
|---|---|
| `/vol4/_rebase_try` | 变体 A：上游 + 面板层（未修） |
| `/vol4/_rebase_try_B` | 变体 B：A + 面板接线层（已修到层 3） |
| `/vol4/_fix_iter1.py` / `_fix_iter2.py` | 层 1、层 2 的自动修复脚本（可复用） |
| `/vol4/_gocache` | 容器用的 GOMODCACHE 卷（避免每次重下依赖，一轮编译约 10 秒） |
| `panel_upstream_sync/P4-0_build_layer3.log` | 层 3 报错存档（本机） |

编译命令（NAS 无 go，走容器）：
```bash
docker run --rm -v /vol4/_rebase_try_B:/src -v /vol4/_gocache:/go/pkg/mod -w /src \
  -e GOPROXY=https://goproxy.cn,direct -e GOSUMDB=sum.golang.google.cn -e GOFLAGS=-mod=mod \
  golang:1.23-alpine sh -c 'go build -gcflags=all=-e ./... 2>&1'
```
⚠️ 必须加 `-gcflags=all=-e`，否则每个包只报 10 条就 `too many errors` 截断。

### 对路线的修正

- **P4 从"待评估"升级为"可行且推荐"**：总量 ≈ 14 个 API 适配点 + ~20 处调用点，且**全部集中在面板自己的代码里**，上游主干几乎不需要动。这与"面板增强层 hook 极薄"的判断一致。
- **层 1 已顺带验证了 §4 的退役判定**：删掉面板 streak 后只多出 1 个 `clientToken` 被 `school.go` 共用的问题 → 说明退役路线成立。
- **注意**：删面板 `streak.go` 时，`clientToken()` 要单独抽出（`school.go` 依赖它）——这个坑已写进上面的迭代记录。

---

## 9. ⭐ P4 执行完成（2026-09-17 01:30）：全树 build + vet + test 全绿

**结论：换基树 `/vol4/_rebase_try_B` 上 `go build ./...` + `go vet ./...` + `go test ./...`（19 个包）全部通过。** 层 3/4 修复完成，另有 2 处设计决策与一批测试适配，全部记录如下。

### 迭代 3–6 记录（脚本可按序重放）

| 轮次 | 脚本 | 内容 | 结果 |
|---|---|---|---|
| 层 3 | `fix_iter3.py` | 贴回 7 组面板符号（`BackfillRealmFor` / `Pool.Revive` / `Pool.Remove` / `ModelInfo.CanDisableThinking`（字段+解析+映射）/ `CreditPackage`+`CreditPackages`（补 sort import）/ `RunBalanceRefreshNow`（适配分桶签名）/ 新建 `scheduler/school_api.go`）+ 改 6 处调用点（`Pick("")`、去 total 的 `UserResource`/`ReenableIfCredits`/`SetCredits`、`RunSchoolNow`→`RunSchoolAllNow`） | 16 错 → 0 |
| 层 4A | `fix_iter4.py` + `fix_iter4b.py` | internal 包双向合并 50 处：**pool**（`TokenUsage`/`TokenUsageDelta`/`RecordTokenUsage` + Status 透出 + state.json 持久化）、**scheduler**（热配置 `schedMu`/rearm + `Reconfigure`/`SetBalanceInterval`/`StartBalanceRefresh` + `dispatch` 接 Go 路线）、**logging**（`chatLogOut` + `chatStatsReader` 并集扩展 + `usageDeltaFromResponse`）、**handler**（`Panel`/`Live`/`Usage` 字段 + `httpauth` + `recordAttempt` 全接线 + `staticModels` 兜底 + `can_disable_thinking`） | `./internal/...` → 0 |
| 层 4B | `fix_iter5.py` + `fix_iter5b.py` | cmd/server：config.go 补 `SchoolHours`/`SchoolEnabled`/`ActivityReportCount`/`MaxInFlightGlobal`/`Degrade*`；main.go 补 `SetModelCatalogPath`（四级链第 3 级）/`SetDegrade`/`SetMaxInFlightGlobal`/`store.Close` + `scheduler.Config` 字段映射（`CatHours←blackcat_hours`）+ `saveConfig` 热应用扩展 | 全树 → 0 |
| 测试适配 | `fix_iter5c.py` + `fix_iter6.py` | 18 处：`config_test.go` 词汇（Cat→Blackcat）；`captureStdout` 同步 `chatLogOut`；8 个 modelList 测试改静态兜底断言；dispatch 测试改接 Go 路线 | 23 失败 → 0 |

### 最终验证命令（⚠️ 必须带 TZ）

```bash
docker run --rm -v /vol4/_rebase_try_B:/src -v /vol4/_gocache:/go/pkg/mod -w /src \
  -e GOPROXY=https://goproxy.cn,direct -e GOSUMDB=sum.golang.google.cn -e GOFLAGS=-mod=mod \
  -e TZ=Asia/Shanghai \
  golang:1.23-alpine sh -c 'go build ./... && go vet ./... && go test -count=1 -timeout 480s ./...'
```

- ⚠️ **`-e TZ=Asia/Shanghai` 必须加**：4 个日期敏感测试（`TestGrowthHeatmapYesterdayMissed` + 3 个 `TestMakeupYesterday*`）依赖 CST 自然日；容器默认 UTC 时失败，且**上游原树同样失败**（已做基线对照）——不是合并问题。
- 验证日志存档：`panel_upstream_sync/P4-0_final_verify.log`（build/vet/test 三项 EXIT=0）。

### 合并中的行为变更（落地后需知晓）

1. **school/cat 走面板 Go 路线**：`dispatch` 改接 `RunSchoolAllNow` / `RunBlackcatNow`；上游脚本版 `RunSchoolNow`/`RunCatNow` 保留定义但未接线（面板镜像只 COPY 了 `probe_active.py`，脚本路线不可行）。
2. **school 独立排程**：新增 `school_hours`/`school_enabled`（默认 [12]/true）——面板旧行为是搭签到便车（9/21 点），现走上游的独立时点机制。
3. **/v1/models 静态兜底**（面板行为；上游为纯动态空列表）：CN 动态失败 → 10 个静态模型；global 无号/探测失败 → 21 个静态名单（元数据仍走四级链补齐）。
4. **credits_total 降级**：上游删了 total 概念；login/checkin/balance 响应不再带 `credits_total`（面板前端已按"相对池内最高"优雅降级，`app.js:166`）。
5. **配置词汇**：生产 config.json 的 `blackcat_hours`/`blackcat_enabled` 保留（映射 scheduler 内部 Cat 域）；新增键 `school_hours`/`school_enabled`/`activity_report_count`/`max_in_flight_global`/`degrade_*`（老配置缺省 → 默认值，向后兼容）。
6. **cmd/credit|signin|login|trial 保持上游版**：对照 diff 确认上游实现更新（已吸收 realm 路由、`LoadAuthFiles` 宽匹配、状态分类细化），面板版是旧快照。
7. **gofmt 未强制**：上游本身 43 个文件非 gofmt-clean，未做全树格式化（避免与上游风格打架）；插入代码有少量对齐差异，纯观感。

### 落地待办（§5 流程，待用户确认后执行）

1. 备份：`tar -czf /vol4/_panel_backup_$(date +%F_%H%M).tgz -C <家目录>/docker/workbuddy2api-panel internal cmd Dockerfile docker-compose.yml`
2. 替换：以 `_rebase_try_B` 覆盖面板仓库的 `internal/` `cmd/`（⚠️ **保留 L0**：Dockerfile / docker-compose.yml / config.example.json / data/config.json / auths/，rsync 时排除）
3. 重建容器：`docker compose up -d --build` → 单号灰度 → 面板 UI 回归（§7 验收清单）
4. 写 `PORTING.md`（P4-2）

### 下次上游同步的"重放清单"（P4-2 素材）

- **重放脚本**：`fix_iter1 → 2 → 3 → 4 → 4b → 5 → 5b → 6`（脚本针对"上游树 + 面板层"的固定起点；上游再更新后部分锚点会漂移，按编译报错迭代修）
- **测试适配点**（上游更新后需重新适配）：`cmd/server/config_test.go`（词汇）、`internal/server/{logging,handler,handler_effort_models,handler_global_models,handler_models_name,handler_global}_test.go`（静态兜底/日志镜像）、`internal/scheduler/school_test.go`（dispatch Go 路线）
- **每次 sync 后验证**：build + vet + test（带 `TZ=Asia/Shanghai`）

### 复现资产（NAS `/vol4/`，新增）

| 路径 | 内容 |
|---|---|
| `_rebase_try_B` | ⭐ 换基完整树（build+vet+test 全绿） |
| `_fix_iter1.py` ~ `_fix_iter6.py`（含 4b/5b/5c） | 全部迭代脚本（按序可重放） |
| `_gocache` | 容器 GOMODCACHE 卷 |
| 本地 `panel_upstream_sync/P4-0_final_verify.log` | 最终验证日志（build/vet/test） |

---

## 10. ⭐ P4-3 落地记录（2026-09-17 01:36–01:40）：已上线

### 步骤与产物

| 步骤 | 动作 | 产物/结果 |
|---|---|---|
| 备份 | `tar` 面板目录（internal cmd scripts go.mod go.sum *.sh Dockerfile compose config.example） | `/vol4/_panel_backup_2026-09-17_0136.tgz`（145 项，377KB） |
| 替换 | `rsync -a --delete` internal/ + cmd/；`cp` go.mod/go.sum/login.sh；`cp scripts/global_region.py` | 代码与换基树完全一致 |
| L0 补丁 | Dockerfile 加一行 `COPY scripts/global_region.py /app/scripts/global_region.py`（新 login.sh 的 global 注册流程依赖它） | 仅此一处改动，其余 L0 未动 |
| 测试保留 | rsync 本会删掉 4 个面板测试（`auth/permhint_test.go`、`upstream/{tasks,school_mp,desktop}_test.go`）→ 提前拷回换基树；`tasks_test` 依赖的 `WebBaseCN` 字段被 fix_iter1 简化掉 → **`fix_iter7.py` 恢复**（字段+默认值+覆盖分支） | 树与仓库一致且全绿（19 包） |
| 构建 | `docker compose build`（首次在 `alpine:3.20` 报镜像站 401 → `docker pull alpine:3.20` 落本地库后成功） | 新镜像 `sha256:<uid8-2>…` |
| 上线 | `docker compose up -d` | 容器 `workbuddy2api` Up (healthy) |

### 验收结果（§7 清单逐项）

| 项 | 结果 |
|---|---|
| 容器健康 | `Up (healthy)`，启动日志出现新特性行：**开学季任务已启用：[12] 点（Go API 闭环）**、活跃上报每号 5 条、用量恢复 174 桶 |
| `/healthz` | `{"healthy":9,"total":9,"realm_servable":{"cn":true,"global":true}}` |
| `/status` | 9 账号（cn 3 / global 6）、0 cooling / 0 disabled、**每号 `token_usage` 全透出**（合并的用量链生效） |
| `/v1/models` | 65 个（cn 42 / global 23）；**context_length 为真实值**（256000/300000/176000/272000…，不再是假 131072） |
| 面板 UI | `/panel/` 200、`/panel/api/overview` 200（version `1.9.2-panel`） |
| 灰度请求 | 非流式 + 流式各 1 次 → 均 200；聊天表格日志行正常输出；容器内已有**真实流量**（`global:deep` 流式请求）跑通 |

### 两个需要知道的事实

1. **auths/ 现为 9 个账号**（清单早先写的 10 已过期）——本次替换**未触碰 auths/**，是替换前的既有状态。
2. **回滚命令**（5 分钟级）：
   ```bash
   cd <家目录>/docker/workbuddy2api-panel
   tar -xzf /vol4/_panel_backup_2026-09-17_0136.tgz
   docker compose build && docker compose up -d
   ```

### 同步手册

`PORTING.md` 已落盘三处：本机 `panel_upstream_sync/PORTING.md`、NAS `/vol4/_porting.md`、面板仓库根 `PORTING.md`。
内容 = hook 点清单（13 组）+ 同步流程（9 脚本按序重放 + TZ 验证）+ 8 条行为差异 + 禁止事项。

---

## 11. ⭐ 增量同步：两侧更新合并（2026-09-17 01:45–01:55，`fix_iter8.py`）

### 11.1 侦察结论

| 仓库 | 相对我们的差距 | 处置 |
|---|---|---|
| 根上游 `Sliverkiss/workbuddy2api` | **+4 提交**（`d2cd004..64064ce`，2 项实质变更） | ✅ 已合 |
| 上游面板 `linguo2625469/workbuddy2api-panel` | 无新提交（`c192fd1` = v1.10.0 最新），但**我们落后它 3 个提交**（`3f55d50..c192fd1`） | ✅ 面板层已合 |

### 11.2 根上游增量（2 项，净 diff 5 文件 / +77−4，`git apply` 干净落地）

- `64064ce`/`dcc4918` **#131 `pruneExpiredModelCosts`**：pool `modelCost` 过期条目永不回收 → 补 prune（`entry.go` +26 / `pick.go` +4−1 / `cost_test.go` +31）
- `9514f1e`/`4bf1ee4` **#130 growth 热力图测试桩改 CST 自然日**：**我们踩的 TZ 坑被上游自己修掉了** → 容器不再需要 `-e TZ=Asia/Shanghai`（实测无 TZ 下 `TestGrowth|TestMakeup` 全绿）

### 11.3 面板 1.10.0 面板层（我们落后 3 个提交）

要合的只有**面板层独有**部分；其余 40 个文件是他们"手工吸收 `b5077d5..76bb543`"的产物，我们的树取自更新的上游 `d2cd004`，已覆盖且更全（pool/server/upstream/session 等一律不反向合）。

| 文件 | 内容 | 处理 |
|---|---|---|
| `internal/panel/app.js`、`index.html` | 123 行 UI：连败降权显示、成本台账 tooltip、模型能力徽标（默认/工具/视觉/思考常开）、券码提示文字 | ours == base → 直接取 theirs |
| `internal/panel/{panel.go,login.go,taskcenter.go}` | 同批 UI 接线 | `git merge-file` 三方合并（base=`3f55d50`）**零冲突** |
| `config.example.json` | 补 `max_in_flight_global`/`degrade_*`（他们的）+ `school_hours`/`school_enabled`/`activity_report_count`（我们的） | theirs 为底 + 我们的键 |
| `cmd/server/main.go` | `appVersion` 1.9.2-panel → 1.10.1-panel | 手工 |

> ⚠️ **生产 UI 回退（已定位，已于 01:57 重建上线修复 → §11.7）**：P4-3 部署时 NAS 树停在 `3f55d50`，而面板仓库当天 00:12 已发 1.10.0 → 生产面板少了上述 **123 行 UI**。后端数据都在（实测 `/panel/api/overview` 的 `model_costs`/`degrade_until`/`token_usage` 均有值），纯前端不渲染。`fix_iter8` 已在树里补齐。

### 11.4 词汇/取舍差异（两条路线，不是缺陷）

| 维度 | 上游面板 1.10.0 | 我们（换基树） |
|---|---|---|
| 夜猫子键 | `BlackcatHours`/`blackcat_hours` 一路保留到 scheduler | config 层读 `blackcat_hours` → 内部映射上游 `CatHours` |
| school 排程 | 沿用旧语义：签到末尾 `RunSchoolNow()`，无独立时点、无 `school_hours` 键 | **独立 `school_hours=[12]` + Go API 闭环**（我们主动的行为变更） |
| 新增配置键 | 无 | `school_hours`/`school_enabled`/`activity_report_count` |

### 11.5 验证

`go build ./...` + `go vet ./...` + `go test ./...`（19 包）**三项 EXIT=0**；日期敏感测试无 TZ 也全绿。

### 11.6 PR 目标（用户澄清）

- **上游（PR 目标）= `linguo2625469/workbuddy2api-panel`**（317★，我们跑的面板项目本体；GitHub 上非 fork）
- **根上游 = `Sliverkiss/workbuddy2api`**（842★，无面板层，不该收面板代码）
- 我们的 fork = `yjzsg/workbuddy2api-panel`（PR #18 出处；token 已从本地会话日志找回并验证 `login=yjzsg`）

### 11.7 重建上线（2026-09-17 01:56–01:58，已生效）

| 步骤 | 结果 |
|---|---|
| 同步 | `rsync -a --delete internal/ cmd/`（11 文件）+ `config.example.json` |
| 构建 | `docker compose build` 一次通过（`alpine:3.20` 已在本地镜像库） |
| 重建 | 容器 `workbuddy2api` → `Up (healthy)`；启动日志新行：开学季 [12] 点 Go 闭环、活跃上报每号 5 条、用量恢复 175 桶 |
| 验收 | `/healthz` 9/9、双 realm 可服务；`/panel/api/overview` **version=`1.10.1-panel`**、9 账号 9 healthy；`/panel/app.js` 已含 123 行 UI（`model_costs`/`degrade_until`/`can_disable_thinking` 命中 5 处）；`/v1/models` 65 个（cn 42 / global 23，ctx 真值 256000/300000）；非流式 + 流式各 200、无 error/panic；容器内已有真实流量（`global:deep` 流式） |
| 回滚 | `/vol4/_panel_backup_2026-09-17_0136.tgz` → `tar -xzf` + `docker compose build && up -d` |

**PR：用户决定不提（2026-09-17 01:56）**。面板仓库侧仍为 v1.10.0；我们的部署继续走"**换基树 = 权威源**"路线（面板仓库只作面板层来源，需要时用 `fix_iter8` 的模式增量吸收）。

---

## 12. ⭐ 增量同步第二轮（2026-09-17 20:00–20:10，`fix_iter10.py`）：根上游 48 提交 + 面板 app.js

用户："两个上游 有更新吗？" → 选择**全合** + `max_body_mb` **跟随上游退役**。

### 两侧增量

| 仓库 | 增量 | 规模 |
|---|---|---|
| 根上游 `Sliverkiss/workbuddy2api` | `64064ce..a9ccace` = **48 提交** | 46 文件 / +3373 −421 |
| 上游面板 `linguo2625469/workbuddy2api-panel` | `c192fd1..4f18f7f` = **2 提交** | 仅 `internal/panel/app.js`（+123 −38） |

### 根上游主要内容

- **破坏性**：`server.max_body_mb` 退役（413 预拦截删除，大请求直读交上游）→ PORTING.md §4 第 10 条
- **新功能**：`prompt.mode=append`（#129）、pool `costTier` 条件探索（#136，默认 30m）、scheduler 迟到唤醒补跑 5s 网络宽限（#152）、`school_season` 校园日（脚本路线，未接线）
- **日志可读性**：`logfmt.Label(uid,nick)` → 流水行 `昵称(uid8)` + `DisplayWidth/Pad` 中文对齐 + 模型列 26 宽只补不截 → §4 第 11 条
- **关键修复**：⭐ Expiring 分桶读错字段（`PackageEndTime` → **`CycleEndTime`**，上游从不下发前者）；SSE 非 delta 回退未 latch 致正文重复追加；空 content 帧不 latch；tool_calls Aggregate 三修（残缺参数/缺 index/非 delta 兜底）；usage 缺 `total_tokens` 补齐 + 流式缺失保留 -1 哨兵；models.dev 负缓存 TTL 永不淘汰；GrowthYesterdayDate 夏令时错位；RefreshToken 数据竞争（新增 `auth.RefreshTokenValue()`）；Truncate `n<=0` 守卫

### 执行（`fix_iter10.py`）

1. **分诊**：逐文件 `git apply --check` → **38 干净 / 8 冲突**（净 diff 存 NAS `/vol4/_up_48.patch`，5298 行）
2. 干净组一次打完（含 8 个新测试文件）；新文件里 `"workbuddy2api/` import 前缀 sed 归一（4 个文件）
3. 冲突组三方合并（base=`64064ce`）：`config.example.json` / `scheduler.go` / `expiring_test.go` **零冲突**；
   `config.go`(5 块) / `main.go`(1) / `handler.go`(3) / `logging.go`(2) 共 **11 块**按 ours/theirs/both 逐块解
4. `max_body_mb` 退役清理：handler 的 `maxBodyBytes`/`SetMaxBodyBytes`/413 分支 + main 的注入/热改/文档行
   + `config.go` 默认值 + **面板 UI 输入框**（`app.js` 字段映射 + `index.html` 表单）
5. 面板 `app.js` 三方合并（base=`c192fd1`）：ours(+CNInvite 卡片) × theirs(用量真实时间轴) → **零冲突**，1618 行
6. 备份：`/vol4/_rebase_backup_2026-09-17_1955.tgz`（换基树）+ `/vol4/_panel_backup_2026-09-17_2004.tgz`（面板目录）

### 验证与上线

- `go build` + `go vet` + `go test`（19 包）**三项 EXIT=0**（无需 TZ）
- 同步 `internal/` + `cmd/` + `config.example.json`；**Dockerfile 未同步**（L0 本地补丁版）
- 容器重建 healthy；验收：**11 账号**（10 healthy，cn 5 / global 6）、65 模型（ctx 真值）、面板 200（`1.10.1-panel`）、灰度非流式+流式 200、无 error/panic
- **新日志格式已生效**：
  `| #001 | 20:06:26 | global:deepseek-v4.1-flash | stream | 200 | yejzsg@gmail.com(<uid8-3>) | TTFB=6425ms | tok=602 | 60.5tok/s | total=9.9s |`

### 追加（20:25）：effort 降级日志降噪（`fix_iter11.py`）

上线后发现日志被 `WARN: reasoning_effort downgraded` 刷屏（12 分钟 67 条）—— 上游对**每条**被降级的请求都打 WARN，而客户端固定发 `reasoning_effort: max`、`global:deepseek-v4.1-flash` 只支持 `high`，于是每条请求都命中。降级本身是**正确且必要**的（issue #84：国际版传 max 会被上游 400 拒掉），但逐条 WARN 会淹没真错误。

改法：`PrepareBodyOptWithEffortsAndDefault` 拆出 `prepareBodyOptCore(..., realmTag)`，`normalizeReasoningEffort` 按 `realm|model|请求档|结果档` 去重（`effortWarned sync.Map`）并把模型名标成 `global:xxx`；**请求体改写照旧每次执行**。新增 `internal/upstream/effort_warn_dedup_test.go`（3 个用例：去重语义 / 域标注 / 改写不受去重影响）。

验证：3 次 `reasoning_effort: max` 的 `global:deepseek-v4.1-flash` 请求 → 降级日志 **1 条**（`model=global:deepseek-v4.1-flash max -> high (同类降级只记一次)`），原来会是 3 条。全量 build+vet+test 仍全绿。

### 两处"未采纳 / 待办"

- **Dockerfile 的 Go builder 安全更新未采纳**：上游升到 `golang:1.26-alpine`，我们本地补丁版是 `1.23-alpine`（NAS 镜像站绕行 + 国内 proxy）。升 Go 版本是独立风险项，未与本次合并混做；要升请单独验证镜像可拉取 + 构建通过。
- **生产 `config.json` 仍带 `"server":{"max_body_mb":16}`**：被当作未知字段忽略（无害）；想清理就删掉该段。

### 教训（已写进 PORTING.md §3.1）

**每次同步必须同时检查两个上游**——本次第一轮漏了面板仓库的 `app.js`，容器重建完才发现（第二轮补合）。

---

## 13. ⭐ 冷却相关三处修复（2026-09-17 20:40–21:05，`fix_iter12/13/13b.py`）

用户报告："会话粘性是不是有问题了。还有冷却的账户，为啥在账户面板不显示" → "面板刷新一下，冷却的账号就不冷却了，然后后续还会达到它"。

### 诊断结论

| 用户观察 | 结论 |
|---|---|
| 会话粘性"有问题" | **机制正常**（实测：同 `conversation_id` 两次请求固定落同一账号，`sticky_sessions` 0→1）。真实流量落不同账号是因为**客户端没发会话标识**（`ExtractKey` 只认 `conversation_id`/`conversationId`/`metadata.*` 四种形态）→ 设计上退化为纯轮转 |
| 冷却账号在面板"不显示" | **账号级冷却显示正常**（实测 overview 返回 `cool_remaining_sec`/`cool_kind`，面板渲染出「限流冷却 · X分」）。真正缺口是**模型级 6004 限流**：`CooldownSoftForModel` 带 resetAt 时**不写账号级 until**（代码注释："6004 从不写账号级 until"）→ `Cooling=false` → `cool_remaining_sec` 不输出 → 面板显示「可用」；而 `rate_limited_models` 字段后端一直透出、**面板从未渲染**（上游面板也没有） |
| ⭐ "刷新一下就不冷却了，后续还会达到它" | **真 bug**：`ReenableIfCredits` → `reviveCoolingLocked` → **无条件 `clearCoolingLocked`**（清整个冷却域，含 429/6004）。而 **每 5 分钟的余额后台刷新** + 面板「刷新」按钮（`balance_all` → `RunBalanceRefreshNow`）都走这条 → 撞 6004 的号 5 分钟内被解冻 → 立刻又被选中 → 再撞，死循环 |

### 修复

| 脚本 | 内容 |
|---|---|
| `fix_iter12.py` | 面板 `renderAccounts` 补渲染 `rate_limited_models` → 模型级限流显示「xxx 限流 · 剩余时间」（tooltip 列全部受限模型 + 到期时刻）；**不并入 `frozen`**（账号对其他模型仍可用，不该出现「解冻」按钮） |
| `fix_iter13.py` | `reviveCoolingLocked` **只清 `CoolHard`**；同步三处文档注释（`transition.go` / `state.go` / `scheduler.go`） |
| `fix_iter13b.py` | 更新两个锁定旧语义的测试：`TestCooldownSoftStreakResetByReenable` 改断言（不再归零 streak）；`TestTransitionReviveClearsCoolingKeepsBreaker` 拆为 3 例（Reenable 只清 Hard / Reenable 保留软+模型级 / Revive 全清） |
| 新增测试 | `internal/pool/revive_soft_test.go`（2 例）；`internal/upstream/effort_warn_dedup_test.go`（3 例，属 §12 追加） |

### 验证

- `go build` + `go vet` + `go test`（19 包）**三项 EXIT=0**
- **行为验证**：制造软冷却（`账号C` remain=254）→ 触发 `POST /panel/api/balance_all` →
  **修复后 `cooling=True remain=251`**（时间正常流逝、未被解冻）；修复前同样操作会变成 `cooling=False`
- 面板 overview 同刻：`cooling 计数 = 1`、该账号 `remain=251 kind=soft_rate` ✔
- 备份 `/vol4/_panel_backup_2026-09-17_2056.tgz`

### 副作用与逃生门

- 签到（每天 09:00/21:00）也**不再解冻限流冷却** —— 限流窗口 8 分钟 vs 签到间隔 12 小时，影响可忽略，且语义正确（余额与配额无关）。
- 需要强制解冻（含限流/熔断）→ 面板「解冻」按钮 → `Pool.Revive`（显式全清）✔

### 已确认（2026-09-17 22:06，`fix_iter14.py`）：客户端确实不带会话标识

加了一次性探测日志（`session.ProbeMissingKey` —— 只记**键名**不记值、同键集合去重）后，抓到客户端真实请求体：

```
[session] 未识别会话标识（粘性不生效）顶层键=[max_tokens messages model reasoning_effort stream stream_options tools] metadata 键=[]
```

→ **客户端（dsh）不带任何会话标识**：既无 `conversation_id`/`conversationId`，也无 `metadata`，
也没有 `session_id`/`chat_id`/`thread_id` 等别名。与上游注释一致（"dsh / Codex / Cherry Studio 等
请求体里既无 conversationId 也无 metadata"）。

**结论**：粘性对这类客户端**设计上无法生效**（除非客户端支持注入会话字段）。
上游已提供 `session.TurnKey(body)`（按最后一条 user 消息派生**轮**级键）做用量归因兜底，
但那是"轮"不是"会话"，多轮对话无法用于固定账号。

**代价**：`prompt_cache_key` 失去会话段（仅剩账号段）→ 同对话换号时上游前缀缓存 miss。

### 上游对此的立场（2026-09-17 查证 `a9ccace` 源码）

| 层面 | 机制 | 无标识客户端 |
|---|---|---|
| **粘性**（同对话固定账号） | `ExtractKey(body)` 只认 body 的 `conversation_id` / `conversationId` / `metadata.*` | **不粘**（设计边界，测试 `TestNoSessionKeyPassthrough` 锁定） |
| **轮级聚合**（上游后台记账不碎片） | `TurnKey(body)` = **最后一条** user 消息的「序号 + 文本」→ 出站 `X-Conversation-Request-ID` | **能工作**，不依赖客户端配合 |
| **会话级 ID** | `ResolveConversationID(body)` → 出站 `X-Conversation-ID` | **不发**（注释："透传优先，不伪造…避免误导后台建错会话"） |
| **prompt_cache_key** | `wb2a-<uid8>-<convHex>`；conv 为空时 convHex = sha256(uid) 的定值 | 只剩**账号隔离段** → 同账号连续请求仍命中，换号即 miss |

上游**明确否决**了"用首条消息推断会话"这条路 —— `TurnKey` 注释原文：

> 为什么不取第一条 user 消息：首条在整个会话内不变，会把一次会话的所有轮并进同一个聚合键（跨对话轮混并）。取最后一条才对齐官方 `X-Conversation-Request-ID` 的「对话轮」语义。

官方 CodeBuddy CLI 的正路是**在 body 里发 `conversationId`**（camelCase；上游 issue #35 就是修"此前只认 snake_case，
导致粘性路由不命中、同对话轮转不同账号、上游上下文缓存 miss"）。

→ **上游**没有可靠替代方案（`TurnKey` 解决的是**另一个问题**：无标识客户端在上游用量明细里"一条请求一条记录"的
碎片化；它是轮级键，"用户发下一条消息自动换键"，拿来做粘性等于每轮换号）。

### ⭐ 但**面板层本来就有解**（2026-09-17 22:2x 找到并恢复，`fix_iter15.py`）

上面那段"结论：网关侧没有可靠替代方案"是**只看上游**得出的 —— 实际**面板基线 `3f55d50` 的
`ExtractKey` 末尾有 `deriveKey` 兜底**，生产一直在用：

```
面板版：... return deriveKey(obj)      // SHA-256(system 文本 + 首条 user 文本) 前 16 字节，前缀 "d-"
上游版：... return strOrEmpty(obj["conversationId"])   // 无标识 → 直接空
```

**换基时漏贴了它**（`deriveKey` 只在 `ExtractKey` 末尾内部调用、外部零引用 → 编译不报错）→ dsh 从"能粘"退化为"纯轮转"。
恢复后实测：同一 body（仅一条 user）连发 3 次 → 全部落 `当时只道是寻常(<uid8-1>)`，`sticky_sessions` 2→3；
真实客户端流量也明显集中（6 次同账号）。恢复脚本 `fix_iter15.py`（+`15b/15c` 适配
`TestHandlerAppendTurnKeyStable`：该用例的反证要用**无开头 system 块**的 body 才成立，
因为 `prompt.Append` 插在开头连续 system/developer 块**之后**）。

> 教训：**"上游没有"≠"我们没有"**。审计面板层遗漏时必须回到**面板基线**找，不能只读上游源码下结论。

---

## 14. ⭐ 面板层遗漏审计（2026-09-17 22:2x–22:5x，`fix_iter15~20.py`）

用户："继续，完成后再看看有没有其他面板改进了，我们遗漏了的。"
—— 起因是 `deriveKey` 那次教训：**换基靠编译报错驱动，而"无外部引用"的面板增强不会报错**。

### 14.1 审计方法（已固化成脚本，可重跑）

| 脚本 | 作用 |
|---|---|
| `audit_panel_loss.py` | 按文件比对（v1）：能看出"符号搬家"（如 `school.go`→`school_api.go`），但假阳性多 |
| **`audit_panel_loss2.py`** | **全树并集比对（推荐）**：只列「面板基线有、树里任何文件都没有」的符号，分「生产/测试」两组 |
| `restore_panel_tests.py` | 按 `(文件, 函数名)` 从面板基线抽取用例（花括号配平 + 上邻注释）追加回树 |

```bash
python3 audit_panel_loss2.py <家目录>/docker/workbuddy2api-panel 3f55d50 /vol4/_rebase_try_B
python3 restore_panel_tests.py <panel_repo> 3f55d50 <tree> internal/pool/pool_test.go:TestXxx ...
```

### 14.2 审计结果：生产代码 **49 个符号缺失，全部定性为 ①②**（无真丢失）

| 定性 | 例子 |
|---|---|
| ① **上游等价替代**（改名/合并/拆分） | 错误分类表 `softRateMarkers`/`hardMarkers`/`contentBlockedMarkers`/`badParamsMarkerCode`… → `errorRule` 体系（`softRateRule`/`hardRule`/`contentBlockedRule`/`badParamsRule`/`alreadyCheckinRule`/`sessionDeadRule`/`accountFaultRule`）；`ParseSoftRateReset` → `ParseRateReset` + `IsModelRateLimit` 拆分；`PickExcluding`/`PickExcludingForModel` → `PickExcludingForRealm(tried, model, realm)` 三合一；`PickByUID` → `PickByUIDForModel`；`defaultWorkBuddyUA` → `defaultWorkBuddyUAFor`；`mergeGlobalModelInfos`/`parseGlobalModelInfos`/`staticGlobalModelInfos` → `mergeGlobalCatalog`/`parseGlobalModelNames`/`globalModelsProbePaths`；`modelEntry`（131072 兜底）→ `ContextWindowListingV4` 四级链；`fetchGlobalModelInfos`/`globalModels` → `FetchGlobalModelInfos`/`upstream.GlobalModelNames`；`turnSalt`/`requestIDs` → `deriveSalt`；`trunc` → `logfmt.Truncate`（还带 CJK 列宽） |
| ② **有意退役** | `streak.*`（8 个：`RunStreakBonusNow`/`streakBonusAccount`/`GrowthRedeemTier`/`StreakFull`/`Lottery*`/`compactJSON`，层 1 决定）、`Handler.SetMaxBodyBytes`（max_body 退役）、`GlobalEnabled`（getter，无调用者）、`adoptReportGap`（领养前置上报被上游重构进活跃上报任务）、`cmd/credit` 的 `resourcePackage`/`packageRemainUsed`/`fetchUserResource`（上游改为 `ResourceSummary` 聚合口径，CLI-only） |

### 14.3 测试网：**补回 19 条面板自有用例**（文件在 ≠ 用例在）

面板测试**文件**都在我们树里，但**文件内的面板专属用例**在同步时被上游版覆盖掉了（丢测试不报错）。

**补回并通过（19 条）**：`TestRecordTokenUsage`、`TestTokenUsagePersistsAcrossReload`、
`TestSoftRateModelClearedByPlainCooldown`（pool）；`TestRunBalanceRefreshNowUpdatesCreditsAndRevives`（scheduler）；
`TestRequestIDForKeyStability`（session）；`TestModelsDynamicFallsBackToStatic`（server）；
`TestPrepareBodyDeterministic`、`TestBillingUA_WhenClientNameEmpty`（upstream）；
`TestWriteDefault`、`TestLoadConfigPathIsDirectory`、`TestBalanceRefreshDefaults`、`TestPromptDefaultPassthrough`（cmd/server）。

**判定为「上游有意反转旧语义」而删除（10 条，删除处都留了依据注释）**：

| 用例 | 上游反转依据 |
|---|---|
| `TestModelCooldownsNotPersisted`、`TestSoftRateModelNotPersistedToState` | 上游 `stateAccount.ModelCooldowns` 带 `json:"model_cooldowns"`（**持久化**），上游自带用例反过来断言"必须落盘" |
| `TestCooldownSoftExponentialBackoff`、`TestApplyErrorPolicySoftRateExponentialBackoff` | 上游只在**进入新冷却**时推进退避；自带 `TestCooldownSoftBoundedBackoffIfNotCooling` + `TestCooldownSoftRateNoDoubleWhenAlreadyCooling`（注释原文："旧实现每次都 softStreak++ 指数翻倍，把全池推到 2h 封顶"） |
| `TestModelsFetchFailurePenalizesAccount` | 上游 `fetchDynamicModels` 明文"只进负缓存，**不 NoteError**（P1-6/发现 6）：models 端点偶发 5xx 会跨界惩罚 chat 通道健康的账号" |
| `TestParseSoftRateReset`（我们恢复的那份） | 与上游自带 `TestParseRateReset` 重名；上游注释写明"旧语义（非 6004 带时间 → false）是**有意推翻**的：11140 rate-limiting 变体带重置时间时同样应对齐" |
| `TestFetchModelsDefaultEffortDualKeyAndSizes` | 上游只把 `reasoning.defaultEffort` 映射到 `DefaultEffort`；`reasoning.effort` 走 `ReasoningEffort`（**单档**语义），自带 `TestParseGlobalModelNamesSingleEffort` 断言 defaults 为空 |
| `TestMergeModelCapabilitiesKeepsCLIWhenOverlayEmpty`、`TestFetchModelsOverlaysV3ConfigCapabilities` | `mergeModelCapabilities`/`codeBuddyIDEUA` 已被上游 v3 合并机制取代；上游自带 8 个 v3 合并用例（双域/降级/去重/稳定输出）覆盖更全 |

**适配点（上游签名变化）**：`SetCredits(uid,credits,total)`→`(uid,credits)`；
`PickExcludingForModel(t,m)`→`PickExcludingForRealm(t,m,"")`；`applyErrorPolicy` 补第 5 参 `*upstream.Error`；
`UserResource` 3 返回值→2；`cmd/server/config_test.go` 补 `time` import。

### 14.4 顺带修正的一条文档结论

§13 里"网关侧没有可靠替代方案"是**只看上游**得出的 —— 面板基线本就有 `deriveKey` 兜底，
换基时漏贴（无外部引用 → 编译不报错）。已恢复并实测生效。**教训：审计必须回到面板基线找，不能只读上游下结论。**

### 14.5 验证

`go build` + `go vet` + `go test`（19 包）**三项 EXIT=0**（无需 TZ）。
本轮**只改测试文件**（rsync dry-run 确认 9 个 `_test.go`，无生产文件变更）→ **生产二进制不变，无需重建容器**；
已 rsync 到面板目录保持仓库一致。

---

## 15. ⭐ 采纳上游未合并 PR #161 **全部三项**（2026-09-18 01:05–01:20，提交 `255711d` + `1b02815`）：已上线

用户线索："上游库好像有授权相关的更新"。核查后：**两个上游都没有新提交**
（`linguo2625469/workbuddy2api-panel` @ `4f18f7f`、`Sliverkiss/workbuddy2api` @ `9d1a21b`，
`git ls-remote` 双向确认）→ 真正的新东西是**未合并的 PR**：
`https://github.com/Sliverkiss/workbuddy2api/pull/161`（`cold-summer`，4 提交 / 953 行）。

### 为什么必须落 ①（这是**线上正在踩的 bug**，不是优化）

```
本仓正则:   softRateResetPattern = `将在 (.+?) 重置`                       ← 只认中文
线上真实:   {"code":6004,"msg":"usage exceeds frequency limit, but don't worry,
             your usage will reset at 2026-09-18 13:26:07 UTC+8, ..."}      ← 英文
```
→ `ParseRateReset` 解析失败 → 落「无 resetAt」有界退避分支 →
**① `softStreak` 指数翻倍**（10→20→40→80→120min 封顶，因为冷却刚到期就被新 429 判定为"新限流"）；
**② 走账号级冷却并清空 `modelCooldowns`** → **一个模型被限流，该号其余模型也全部不可用**
（而上游文案原话就是"可以切其他模型继续用"）。

日志实证（近 12h）：`账号B` / `账号A` / `账号C` 反复撞该英文 body。

### 落了什么

| 项 | 落否 | 落地要点 |
|---|---|---|
| ① `ParseRateReset` 补英文形态 | ✅ | 正则拆 CN/EN；EN = `(?i)reset at (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})`（**锚定时间格式**，不捕获 `reset at the end of the day` 这类自然语言）；先 CN 后 EN。3 个 hunk 均 `git apply` 干净（offset 1） |
| ③ `auths` 目录热加载 | ✅ | 新文件 `internal/pool/watch.go`(130) + `watch_test.go`(196)；`cmd/server/main.go` 挂 `p.StartAuthDirWatch(cfg.AuthDir)`（1 hunk，offset 25）。**必改 import**：PR 用 `workbuddy2api/internal/auth`，本仓模块名 `github.com/linguo2625469/workbuddy2api-panel` |
| ② `/v1/stats` 统计端点 | ✅ | 新增 `GET /v1/stats` + `POST /v1/stats/reset`（按模型聚合 token/缓存/延迟/扣费，纯内存、重启清零）。`metrics.go`(313)+`metrics_test.go`(148) **直接抄 PR 原文件**；**必须手工适配**：`logging.go`（`chatStat` 加 metrics 字段 / `done()` 落 `recordChatMetric` 单一埋点 / `chatStatsReader` 加缓存三段 + `PromptTokens()` 取 `promptTokens` + `CacheTokens()` / `parseSSELine` 解析 `prompt_cache_{hit,miss,write}_tokens`）与 `handler.go`（2 个路由 + 流式/非流式各一处取值）—— 本仓 `chatStatsReader` 是面板层 pointer 语义，PR 的 `s.prompt`/`s.tokens` 不存在 |
| `dev.sh` / `.gitignore` | ❌ | 上游开发脚本，与本仓部署方式无关 |

### 验收（实测，非推断）

- `go build` / `go vet` / `go test`（19 包）**三项 EXIT=0**；
- 新增用例全过：`TestParseRateReset_English`(4 子例) + `TestStartAuthDirWatchNoopOnBadDir` /
  `TestReloadAuthDirAddsAccount` / `TestReloadAuthDirPreservesState` / `TestReloadAuthDirRemovesDeleted`
  + `TestMetrics*`(6 条：按模型聚合 / 缺 usage 不计成 0 / 总量=各模型之和 / reset 清空 / 空模型名兜底 / 容量上限)；
- **②线上实测**：重启后打 2 个请求 → `GET /v1/stats` 返回 `total`：3 请求 / 输入 **189,862** token
  vs 输出 **1,839**（≈103 倍，印证 PR 作者"成本几乎全在输入侧"）/ `cache_hit_rate` 15.4% /
  `avg_ttfb_ms` / `tokens_per_sec` / 逐模型 `models[]`，字段结构正常 ✅；
- **①用线上真实 body 单测**（临时用例，验完即删）：`ParseRateReset` → `2026-09-18 13:26:07 (UTC+8)` ✅；
- **③端到端**：宿主机用户无权 `touch auths/*.json`（属主是容器 uid 10001）→ 改用同 uid 辅助容器
  `docker run --rm -u 10001:10001 -v <auths>:/a golang:1.23-alpine touch /a/<f>` →
  9s 内日志出 `[watch] auths 目录变化：账号数保持 11（已热加载凭证更新）`，
  且 `/status` 的 `total=12 healthy=11 cooling=1 sticky=3` **与改前完全一致**（状态未被重置）✅；
- 容器 `Up (healthy)`，重启后立刻有 200 流式请求落库。

### ⚠️ 顺手发现的一个仓库卫生问题（未处理，已写进 PORTING.md 禁止事项）

部署仓库 `<家目录>/docker/workbuddy2api-panel` 的**工作树领先 git HEAD `657856e` 共 63 个文件**
（+3333/−527，含 `internal/pool/*.go`、`internal/upstream/sse.go` 等**生产文件**）。
容器是 `build: .`（构建**工作树**而非 HEAD）→ 部署行为正确，但 **HEAD 不是部署真相**。
**别用 `git checkout .` / `git stash` / `git reset --hard`**，会丢掉未提交的换基成果。

### 补：面板侧 UI（同日 01:25–01:35，提交 `7518d78`）

用户："面板上还没有这些数据"。`/v1/stats` 只在网关侧，面板没有对应 UI。

**新开一页而不是塞进「用量」页** —— 两页是**两套口径**：用量页读 `usage.Recorder` 的持久化分桶
（按账号/模型/域、可按时间窗回看、重启不丢，**没有缓存字段**）；`/v1/stats` 是 `server` 包的
纯内存聚合（重启清零）。硬合并要么改持久化格式，要么在前端编造换算 → 并列展示 + 把口径差异写在界面上。

接线（避免 import 环）：`panel.Config` 加 `Stats func() any` / `StatsReset func()`，由 `cmd/server/main.go`
注入 `func() any { return server.MetricsSnapshotOf() }` / `server.ResetMetrics`。
`server` 已 import `panel`，面板反向 import 会成环。
⚠️ 注意 Go **不做** `func() T` → `func() any` 的隐式转换 —— 第一次就是直接写
`Stats: server.MetricsSnapshotOf` 而编译失败（`cannot use ... as func() any value`），必须包一层闭包。

前端：导航「网关统计」+ `view-stats` 段落；`renderStats`/`loadStats` + 清零按钮（带 confirm）；
6 张卡片（请求/缓存命中率/输入/输出/TTFB/吐字速率）+ 按模型 13 列表。
显示纪律：命中率分母（hit+miss）为 0 时显示 `—` 而非 `0.0%`；扣费 0 也显示 `—`（未观测≠免费）。

**验收（实测）**：

- `go build` / `go vet` / `go test`（19 包）全绿；
- ⚠️ **`node --check app.js` 必须在本机单独跑** —— 容器内无 node，`TestAppJSSyntax` 会 **skip**（不是通过）。
  本机 node v22 校验通过；`index.html` 只有 `<script src="app.js">` 一个外链（CSP `script-src 'self'` 安全）；
- `GET /panel/api/stats` → 完整快照（含逐模型命中率）；`POST /panel/api/stats/reset` → `{"ok":true}`
  且计数归零；**未授权 → 401**；
- **浏览器端到端**（Camoufox，截图 `D:\工作区\chrome_profiles\panel_stats.png`）：过密钥闸门 → 点导航 →
  `view-stats` 可见、标题「网关统计」、**6 张卡片**、按模型表渲染、口径注记正确、控制台无（本页）错误；
- ⚠️ 截图时控制台有 1 条 CSP 报错 —— 经**对照实验**确认来自 **Camoufox 注入的 sandbox 脚本**
  （`{file: "sandbox..."}`；空白页 0 条、面板首页**不点任何东西就有**）→ 与本次改动无关，不修。

---

## 附录 A · 上游 59 提交主题分布（供对照）

| 主题 | 关键提交 |
|---|---|
| 账号池/选号 | `64eb4aa` 退避基建 · `34405ca` WAF 软冷却 · `2680f4c` max_in_flight · `8825c4c` IP 级 fail-fast · `cf1e7e5`+`8fbbe21`+`d80a97f` 连败降权 · `94e4e61` panic · `9a7e866` 删 EMA · `c5dc4e3`+`2493532` 成本台账 · `49930b2`/`0c10de3` usedSeq · `c768f48`/`d2cd004` Redis 快照 · `dcc4918`/`64064ce` prune |
| 错误分类/请求级 | `76fafa6` ErrWafBlock · `145220d` 429 前移 · `5f26ce3`+`f41c496` 11115 · `edb9e97` max_completion_tokens · `5755fe3` 透传 · (`6a4dd4c`/`4c16e45`/`d32dc15` 预估被 Revert) |
| 模型元数据 | `32a3c13` 知识表 · `7218307` model.json · `10db174` models.dev · `41d4714`+`bb5a7dd` 接线 · `064e505`/`4092853` 负缓存泄漏 |
| 会话/连接 | `155af65` tool 配对 · `3d9a4cc` 连接加固 · `ebd7921` 粘性键 · `2b8dba0`/`af51945` GC 竞态 · `910b8b2`/`722ee19` 头竞争 · `5f10a9c`/`2b662b9` redisstore |
| gateway_hint | `a749016` · `fa7b5d9` · `76bb543` StreamHint |
| 活动/成长（吸收了面板） | `91418c5` 连登奖励 · `243c7f2` 吸收面板连登管家 · `b27c634` first_buddy · `f98197f` 开学季 4 任务 · `8622910` 成长任务 |
| 测试/清理 | `c2021cc` `683bd5d` `4bf1ee4` `9514f1e` `8b2cd71` `84d7d50` `d202aa8` |

## 附录 B · 规模对照

| 项 | 上游 | 面板 |
|---|---|---|
| `internal` 非测试 `.go` 数 | 153（含测试） | 96 |
| `internal` 总行数 | 40,341（含测试） | 25,594 |
| 独有**非测试**文件 | 17 个（约 2.3k 行） | 19 个（约 4.3k 行） |
| 共同文件里"我独有"的行 | — | 约 3.2k 行 |
| 共同文件里"上游新增"的行 | 约 4.7k 行 | — |

工具位置：上游浅克隆在 NAS `/vol4/_upstream_wb2api`（depth 50，需要更新就 `git fetch --unshallow`）
