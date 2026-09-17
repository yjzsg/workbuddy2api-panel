# workbuddy2api-panel ← 根上游同步移植清单

生成时间：2026-09-17 00:35 (GMT+8)
对比对象：`Sliverkiss/workbuddy2api`（根上游） → `linguo2625469/workbuddy2api-panel`（我们跑的）
部署位置：NAS `fnos` (`192.168.123.6`) `/home/yeying/docker/workbuddy2api-panel`，容器 `workbuddy2api`，端口 `7863`

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
  U=/vol4/_upstream_wb2api ; P=/home/yeying/docker/workbuddy2api-panel
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
   ssh fnos "tar -czf /vol4/_panel_backup_$(date +%F_%H%M).tgz -C /home/yeying/docker/workbuddy2api-panel internal cmd Dockerfile docker-compose.yml"
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

1. 备份：`tar -czf /vol4/_panel_backup_$(date +%F_%H%M).tgz -C /home/yeying/docker/workbuddy2api-panel internal cmd Dockerfile docker-compose.yml`
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
| 构建 | `docker compose build`（首次在 `alpine:3.20` 报镜像站 401 → `docker pull alpine:3.20` 落本地库后成功） | 新镜像 `sha256:c260585f…` |
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
   cd /home/yeying/docker/workbuddy2api-panel
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
