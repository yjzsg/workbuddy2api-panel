# PORTING.md — 本仓库与根上游的同步手册

> 生成：2026-09-17（P4 换基落地后）。**下次同步上游前先读这份。**
> 根上游：`Sliverkiss/workbuddy2api` ｜ 本仓库：`linguo2625469/workbuddy2api-panel`（= 上游主干 + 面板层）

---

## 1. 仓库形态

**本仓库 = 上游主干 + 面板层（贴在上面的补丁）。** 三类别混在一个树里，处置方式不同：

| 类别 | 内容 | 处置 |
|---|---|---|
| **上游主干** | 绝大多数 `internal/`、`cmd/` 文件 | 跟随上游；**本仓库对它们的修改一律登记在 §2**，同步时需重放 |
| **面板层（本仓库独有文件）** | `internal/panel/`（Web UI）、`internal/httpauth`、`internal/livecfg`、`internal/usage`、`internal/upstream/{desktop,global_register,school,tasks,blackcat}.go`、`internal/scheduler/{blackcat,school_api}.go` | 保留；上游没有对应物 |
| **部署补丁（L0，永不被上游覆盖）** | `Dockerfile`、`docker-compose.yml`、`config.example.json`、`data/`、`auths/`、未跟踪脚本（`activate_friend.py`/`bind_invite.sh`） | 上游版本**禁止**覆盖（见 §6） |

上游浅克隆常驻 NAS `/vol4/_upstream_wb2api`；换基工作树 `/vol4/_rebase_try_B`；修复脚本 `/vol4/_fix_iter*.py`。

---

## 2. hook 点清单（对上游主干文件的全部修改）

同步上游后，这些补丁需要按此清单重放（脚本化见 §3）。**行号会漂移，用符号名定位。**

### internal/auth
1. `auth.go` — 追加 `BackfillRealmFor(a, realm)`（包外登录路径写 realm；上游删了该入口）

### internal/pool
2. `entry.go` — 追加 `TokenUsage`/`TokenUsageDelta` 类型；`entry` 加 `tokenUsage` 字段；`stateAccount`/`Status` 加 `TokenUsage` 字段
3. `state.go` — 追加 `RecordTokenUsage(uid, delta)`；`statusOf` 透出 `TokenUsage`
4. `pool.go` — 追加 `Remove(uid)`（管理面板删号用）
5. `persist.go` — load/save 各加一行 `tokenUsage` 落盘/恢复
5b. ⭐ `transition.go` — **`reviveCoolingLocked` 只解「余额型冷却」（CoolHard）**（2026-09-17，`fix_iter13.py`）：余额充足不代表限流解除，而余额后台刷新**每 5 分钟**调它 → 原实现无条件 `clearCoolingLocked` 会把 429/6004 的软冷却一并抹掉，形成「冷却 → 刷新解冻 → 立刻又被选中 → 再撞」死循环（实测 账号C 反复 6004）。配套测试改动见「测试适配点」

### internal/upstream
6. `client.go` — 追加 `webBase(a)` + `WebBaseCN` 可覆盖字段（默认 `https://www.workbuddy.cn`）；追加 `CreditPackage` 类型 + `CreditPackages(a)`（面板"积分构成"）；`ModelInfo` 加 `CanDisableThinking` 字段（含 `dynModelEntry` 解析 + `modelInfo()` 映射）
7. `client_token.go` — 面板层抽取文件（`clientToken()`，school.go 依赖）
8. `payload.go` + `client.go` — **effort 降级日志降噪**（2026-09-17 加）：`PrepareBodyOptWithEffortsAndDefault` 变薄壳、函数体移入 `prepareBodyOptCore(src, sanitize, efforts, defs, realmTag)`；`normalizeReasoningEffort` 接 `realmTag`，日志按 `realm|model|请求档|结果档` 去重（`effortWarned sync.Map`）+ 模型名标域前缀；client 内部改走 `prepareBodyOptCore(..., realmKey(realm))`。**请求体改写照旧每次都做**，只有日志去重。测试：`internal/upstream/effort_warn_dedup_test.go`（参考实现 `fix_iter11.py`）

### internal/scheduler
8. `scheduler.go` — 结构体加热配置字段（`schedMu`/`rearmSchedule`/`rearmBalance`/`balanceInterval`）；`New` 初始化 rearm channel；`nextWake` 在 schedMu 下快照；`Run` 响应 rearmSchedule；`dispatch` 的 school/cat 改接 Go 路线（`RunSchoolAllNow`/`RunBlackcatNow`）；追加 `Reconfigure`/`poke`/`StartBalanceRefresh`/`SetBalanceInterval`/`RunBalanceRefreshNow`
9. `school_api.go` — 面板层新文件（Go 版开学季闭环：`RunSchoolAllNow`/`RunSchoolAccountNow` + 助手）

### internal/server
10. `handler.go` — Config 加 `Panel`/`Live`/`Usage` 字段；追加 `loadLive()`/`softCooldown()`；挂载 `/panel/`；`withAuth` 走 `httpauth.VerifyBearer`；`modelList` 加静态兜底（`staticModels` + `upstream.GlobalModelNames`）与 `can_disable_thinking`；`chatCompletions` 加 `recordAttempt`（5 处调用点）+ `attemptStarted`；`applyErrorPolicy` 软冷却基数改 `h.softCooldown()`
    - ⚠️ **2026-09-17 起 `SetMaxBodyBytes` / `maxBodyBytes` / 413 预拦截已随上游退役**（`server.max_body_mb` 删除），别再贴回；`chatCompletions` 读 body 是上游的 `io.ReadAll(r.Body)` 无上限直读
11. `logging.go` — 加 `chatLogOut`/`SetChatLogOutput`（默认 `os.Stdout`，面板镜像用）；`chatStatsReader` 扩展为并集（分字段 token + `Usage()`，保留上游 `Credit()`/`Tokens()`/`TotalTokens()`）；`parseSSELine` 并集解析；加 `usageDeltaFromResponse`
    - `logChatRow` 签名跟随上游加 `nick`（账号列显示 `昵称(uid8)`），输出目标保留 `chatLogOut`；`uidPrefix` 已委托 `logfmt.UID8`

### cmd/server（本仓库主导的装配层）
12. `config.go` — 内联 `Schedule` 结构（面板词汇 `blackcat_hours` 等）+ `school_*`/`activity_report_count`/`max_in_flight_global`/`degrade_*` 键；`WriteDefault`/`ParseConfigInto`/`ParseConfig`；`validateScheduleHours`
    - **不再有 `server.max_body_mb`**（上游退役；旧 `config.json` 里的该键因 JSON 未知字段被忽略，`Server struct{}` 空段占位）
    - 上游的 `Schedule config.Schedule`（`internal/config` 包）与我们内联结构的差异**保留**：内联是面板层既定形态，取 theirs 会丢 `blackcat_*`/`cn_invite_*`/`balance_refresh_*`
13. `main.go` — `appVersion`/`usagePathFor`/`stateSibling`/`modelJSONPath`；livecfg/usage/panel 装配；`saveConfig`（含 `SetDegrade`/`SetMaxInFlightGlobal`/新签名 `Reconfigure`/`SetCNInvite`）；`StartBalanceRefresh`；`scheduler.Config` 字段映射（`CatHours←blackcat_hours`、`SchoolHours`、`ActivityReportCount`）；恢复上游的 `SetModelCatalogPath`/`store.Close`

### internal/session
14. `session.go` — 追加 `ProbeMissingKey(body)`（2026-09-17，`fix_iter14.py`）：`ExtractKey` 返回空时记一条**去重**日志（只记请求体顶层键名 + `metadata` 子键名，**不含值**），用于判断客户端是否带了未识别的会话标识别名。调用点在 `handler.go` 的 `sessKey := session.ExtractKey(body)` 之后

### 面板层新增功能（独立文件 + 少量接线，非"hook"）
15. **CN 邀请活动**（2026-09-17 加）— `internal/upstream/cninvite.go` + `internal/scheduler/cn_invite.go` + `internal/panel/cninvite.go`；
    接线点：`scheduler.go`（`Config` 加 `CNInviteCode/Hours/Until/Disabled` + `taskCNInvite` 进 `taskKind`/`nextWake`/`dispatch`）、
    `cmd/server/config.go`（`schedule.cn_invite_*` + `defaultCNInviteCode`）、`cmd/server/main.go`（注入 + `saveConfig` 热改）、
    `internal/panel/panel.go`（两条路由）、`index.html`+`app.js`（任务中心卡片）。参考实现：`fix_iter9.py`

### 测试适配点（上游更新后需重新适配）
- `cmd/server/config_test.go` — 词汇 `Cat*` → `Blackcat*`（默认 `[23]`）
- `internal/server/logging_test.go` — `captureStdout` 需同步调 `SetChatLogOutput`
- `internal/server/{handler,handler_effort_models,handler_global_models,handler_models_name,handler_global}_test.go` — 8 个 modelList 测试按"静态兜底"断言
- `internal/scheduler/school_test.go` — dispatch 测试按 Go 路线断言
- `internal/pool/pool_test.go` / `transition_test.go` — **Reenable 解冻语义**（2026-09-17）：只清 CoolHard；
  `TestCooldownSoftStreakResetByReenable` 改断言（不再归零 streak），`TestTransitionReviveClearsCoolingKeepsBreaker`
  拆为 `TestTransitionReenableClearsHardOnlyKeepsBreaker` / `…ReenableKeepsSoftAndModelCooldowns` / `…ReviveClearsEverything`

**⚠️ 面板自有测试用例会被"文件级覆盖"整批丢掉（2026-09-17 审计发现，已补回 19 条）**：
面板测试**文件**都在我们树里，但**文件内的面板专属用例**在换基/增量同步时被上游版覆盖 →
编译全绿也发现不了（测试丢了不报错）。补回方式见 `restore_panel_tests.py` + `fix_iter16~20.py`。
补回后仍红的用例要逐条判定「上游有意反转旧语义」（改前先看上游自带用例的注释）还是「真回归」。

---

## 3. 同步流程（上游更新后照做）

```bash
# ① 拉上游
ssh fnos "cd /vol4/_upstream_wb2api && git fetch origin && git reset --hard origin/master"   # 浅克隆需先 --unshallow

# ② 拼候选树（上游主干 + 面板层 + sed 前缀），然后按序重放修复脚本
#    起点 = 上游树；贴回：internal/{panel,httpauth,livecfg,usage}、
#    internal/upstream/{desktop,global_register,school,tasks,blackcat}.go、
#    internal/scheduler/{blackcat,streak}.go；再 sed 模块前缀
python3 /vol4/_fix_iter1.py    # 层1：删面板 streak + 补 webBase/ReportChatActivityModel
python3 /vol4/_fix_iter2.py    # 层2：抽 client_token.go
python3 /vol4/_fix_iter3.py    # 层3：贴回 7 组面板符号 + 改调用点
python3 /vol4/_fix_iter4.py    # 层4A：internal 双向合并
python3 /vol4/_fix_iter4b.py   # 层4A 补：粘行修复
python3 /vol4/_fix_iter5.py    # 层4B：cmd/server config+main
python3 /vol4/_fix_iter5b.py   # 层4B 补
python3 /vol4/_fix_iter6.py    # 测试网适配
python3 /vol4/_fix_iter7.py    # WebBaseCN 恢复（面板测试依赖）
python3 /vol4/_fix_iter8.py    # 增量：合根上游 d2cd004..64064ce + 面板 v1.10.0 面板层（见 §3.1）
# ⚠️ 上游再更新后部分锚点会漂移 → 按编译报错迭代修（每层脚本都以 count 断言 fail-fast）

# ③ 验证（TZ 不再需要：上游 #130 已把日期敏感测试改成 CST 自然日口径，2026-09-17 实测无 TZ 全绿）
docker run --rm -v <tree>:/src -v /vol4/_gocache:/go/pkg/mod -w /src \
  -e GOPROXY=https://goproxy.cn,direct -e GOSUMDB=sum.golang.google.cn -e GOFLAGS=-mod=mod \
  golang:1.23-alpine \
  sh -c 'go build ./... && go vet ./... && go test -count=1 -timeout 480s ./...'

# ④ 落地（备份 → 替换 → 重建 → 验收）
cd <家目录>/docker/workbuddy2api-panel
tar -czf /vol4/_panel_backup_$(date +%F_%H%M).tgz internal cmd scripts go.mod go.sum login.sh signin.sh credit.sh Dockerfile docker-compose.yml config.example.json
rsync -a --delete <tree>/internal/ internal/
rsync -a --delete <tree>/cmd/ cmd/
cp <tree>/go.mod <tree>/go.sum . && cp <tree>/login.sh .
cp <tree>/scripts/global_region.py scripts/            # login.sh 的 global 注册流程依赖
docker compose build && docker compose up -d
# 验收：healthz / status（账号数+realm）/ v1/models（cn:+global:，ctx 真值）/ 面板 UI / 1 次流式+非流式请求
```

**踩过的坑**：`docker compose build` 若在 `alpine:3.20` 报 401（NAS 镜像站 docker.fnnas.com 间歇性）→ 先 `docker pull alpine:3.20` 把它落进本地镜像库再 build。

### 3.1 增量同步（日常小步更新走这条，别重走全量换基）

> ⚠️ **每次同步必须同时看两个上游**：根上游 `Sliverkiss/workbuddy2api`（服务端主干）
> **和** 面板仓库 `linguo2625469/workbuddy2api-panel`（`internal/panel/*` 等面板层文件）。
> 2026-09-17 就漏了后者一次（app.js 用量图表真实时间轴），容器重建完才发现。

```bash
git -C /vol4/_upstream_wb2api fetch origin
LAST=<上次已合的上游 SHA>            # 当前为 a9ccace（2026-09-17）

# A) 先分诊：逐文件 --check，把变更分成「干净」与「冲突」两组
for f in $(git -C /vol4/_upstream_wb2api diff --name-only $LAST..origin/master); do
  git -C /vol4/_upstream_wb2api diff $LAST..origin/master -- "$f" > /tmp/one.patch
  (cd <tree> && git apply --check -p1 /tmp/one.patch 2>/dev/null) && echo "OK $f" || echo "CONF $f"
done

# B) 干净组一次打完（含新增文件）；注意新文件里的 import 前缀要改成我们的 module
git -C /vol4/_upstream_wb2api diff $LAST..origin/master -- <clean files...> > /tmp/clean.patch
(cd <tree> && git apply -p1 /tmp/clean.patch)
cd <tree> && grep -rl '"workbuddy2api/' --include='*.go' . \
  | xargs sed -i 's#"workbuddy2api/#"github.com/linguo2625469/workbuddy2api-panel/#g'

# C) 冲突组逐个三方合并：ours=树 / base=$LAST 版 / theirs=origin/master 版
git -C /vol4/_upstream_wb2api show $LAST:<f>    > /tmp/base
git -C /vol4/_upstream_wb2api show origin/master:<f> > /tmp/theirs
git merge-file -p <tree>/<f> /tmp/base /tmp/theirs > /tmp/merged   # rc = 冲突块数
```

**冲突块的处置原则**：上游改的取 theirs、面板层改动保留 ours、两边都新增的取 **both**
（例：handler 的"空流 502 观测" vs 面板的"用量记录"）。参考实现：`fix_iter10.py`（含一个
按块选 ours/theirs/both 的通用解决器）。

**判据（关键）**：上游派生文件（`pool/`、`server/`、`upstream/`、`session/`、`cmd/*`）**永远以我们的树为准**——
我们的树取自更新的上游；只有 `internal/panel/*`、`config.example.json`、`cmd/server/main.go` 的 `appVersion`
这类**面板层文件**需要反向合（base = 面板仓库上次已合的 SHA）。参考实现：`fix_iter8.py`。

```bash
# D) 增量落地只需 internal/ + cmd/（+ 有变动时的 config.example.json）
rsync -a --delete <tree>/internal/ internal/ && rsync -a --delete <tree>/cmd/ cmd/
# ⚠️ Dockerfile 是 L0 本地补丁版（NAS 镜像站绕行 + 国内 proxy + 只构建面板需要的二进制），永不同步
docker compose build && docker compose up -d

# E) 验证（无需 TZ：上游 #130 已把日期敏感测试改成 CST 自然日口径）
docker run --rm -v <tree>:/src -v /vol4/_gocache:/go/pkg/mod -w /src \
  -e GOPROXY=https://goproxy.cn,direct -e GOSUMDB=sum.golang.google.cn -e GOFLAGS=-mod=mod \
  golang:1.23-alpine sh -c 'go build ./... && go vet ./... && go test -count=1 -timeout 480s ./...'
```

```bash
# F) ⭐ 符号级遗漏审计（**编译通过 ≠ 没丢东西**，每次同步后必跑）
python3 audit_panel_loss2.py <家目录>/docker/workbuddy2api-panel 3f55d50 <tree>
python3 restore_panel_tests.py <panel_repo> 3f55d50 <tree> <file:TestName> [...]
```

**为什么必须做**：换基/增量都是**靠编译报错驱动**的，而**没有外部引用的面板增强不会报错**。
2026-09-17 就因此丢了两类东西：

| 类型 | 实例 | 为什么编译查不出来 |
|---|---|---|
| 面板独有生产逻辑 | `session.deriveKey`（无会话标识客户端的内容派生粘性兜底） | 只在 `ExtractKey` 末尾内部调用，外部无引用 → 静默丢失，客户端从"能粘"变"纯轮转" |
| 面板自有测试用例 | 30 条（pool 软退避/TokenUsage 持久化、server 静态兜底、cmd/server 配置…） | 测试丢了不报错，回归网静默变薄 |

**判据**：审计列出的「基线有、树里没有」的符号要逐条定性 ——
① **上游等价替代**（改名/合并/拆分，如 `softRateMarkers`→`softRateRule`、`PickExcluding*`→`PickExcludingForRealm`）；
② **有意退役**（`streak.*`、`SetMaxBodyBytes`）；
③ **真丢失**（补回）。
测试用例同理：补回后仍红的，多半是**上游有意反转旧语义** —— 去读上游自带用例的注释（上游写得很直白）。

---

## 4. 与上游的刻意行为差异（改前先想清楚）

| # | 差异 | 说明 |
|---|---|---|
| 1 | **school/cat 走 Go API 路线** | `dispatch` 接 `RunSchoolAllNow`/`RunBlackcatNow`；上游脚本版（`RunSchoolNow`/`RunCatNow`）保留定义但未接线——面板镜像只 COPY `probe_active.py`/`global_region.py`，不带 `scripts/*.py` 任务脚本 |
| 2 | **school 独立排程 12:00** | 新增 `school_hours`/`school_enabled`（默认 `[12]`/true）；上游同款机制，面板旧行为是搭签到便车（9/21 点） |
| 3 | **/v1/models 静态兜底** | CN 动态失败 → `staticModels`（10 个）；global 无号/探测失败 → `upstream.GlobalModelNames`（21 名）；上游是纯动态空列表。元数据仍走上游四级链 |
| 4 | **无 `credits_total`** | 上游删了"总积分额度"概念；login/checkin/balance 响应不再带该字段（面板 `app.js` 按"相对池内最高"降级显示） |
| 5 | **配置词汇 `blackcat_*`** | 生产 `data/config.json` 用 `blackcat_hours`/`blackcat_enabled`（映射 scheduler 内部 Cat 域）；新增 `school_*`/`activity_report_count`/`max_in_flight_global`/`degrade_*` 键（老配置缺省=默认值） |
| 6 | **软冷却基数热改** | `applyErrorPolicy` 一律取 `h.softCooldown()`（面板 Live 快照优先） |
| 7 | **`WebBaseCN` 可覆盖** | 面板任务领奖域可注入（测试用；生产默认 `https://www.workbuddy.cn`） |
| 8 | **cmd/credit\|signin\|login\|trial 保持上游版** | 上游实现更新（realm 路由、`LoadAuthFiles` 宽匹配、状态分类细化），面板版是旧快照 |
| 9 | **CN 邀请任务（面板独有，2026-09-17 加）** | `schedule.cn_invite_*`：每天对每个 CN 号**幂等绑码**（默认码 `evaxd5iz16ln`）+ 发一次**桌面六事件链**（网关对话不算"使用"，活跃奖靠"7 日内累计使用 3 天"）。上游无此任务；接口 `internal/upstream/cninvite.go`，排程 `internal/scheduler/cn_invite.go`，面板 `/panel/api/cninvite/{status,run}` + 任务中心卡片 |
| 10 | **`server.max_body_mb` 已退役（2026-09-17 跟随上游）** | 请求体不再有预拦截 413：`io.ReadAll(r.Body)` 无上限直读，超限类问题交上游自然响应。面板设置页的「请求体上限」输入框已删；旧 `config.json` 里的 `server.max_body_mb`（生产曾是 16）被当作未知字段忽略。理由：网关提前 413 会挡住上游真实错误、更难排障。自然约束是 `ReadTimeout=60s`（超大 body 传不完 → 连接错误而非 413） |
| 11 | **流水日志账号列 `昵称(uid8)`（2026-09-17 跟随上游）** | 原 `uid=xxxxxxxx` → `昵称(uid8)`，按**显示列宽**对齐（CJK/emoji 记 2 列）；模型列 26 宽只补不截（旧的 11 字节截断把 `cn:deepseek-v4-flash` 切成 `cn:deepseek`）；输出目标仍是 `chatLogOut`（面板 `/panel/api/logs` 镜像） |
| 12 | **effort 降级日志去重 + 域标注（2026-09-17 加）** | 上游对**每条**被降级的请求打 WARN（实测 12 分钟 67 条：客户端固定发 `reasoning_effort: max`，而 `global:deepseek-v4.1-flash` 只支持 `high`）→ 本地按 `realm\|model\|请求档\|结果档` 去重**只打一次**，并把模型名标成 `global:xxx`（cn/global 同名模型档位表不同，裸名分不清是哪个域）。**降级行为本身完全不变**（请求体改写每次照做） |
| 13 | ⭐ **余额刷新只解「余额型冷却」（2026-09-17 加）** | `ReenableIfCredits`（签到 / **每 5 分钟的**余额后台刷新 / 面板「刷新」按钮）只清 `CoolHard`（余额不足 → 次日 04:00），**不再清限流软冷却（CoolSoft）与模型级 6004**。原因：余额刷新每 5 分钟一次，清限流冷却会造成「冷却 → 刷新解冻 → 立刻又被选中 → 再撞」死循环。强制解冻仍走面板「解冻」按钮（`Pool.Revive`，全清冷却+熔断） |
| 14 | **面板渲染模型级 6004 限流（2026-09-17 加）** | `renderAccounts` 补渲染 `rate_limited_models`：账号级 healthy 但对某模型不可用时，原实现显示「可用」（面板从不渲染该字段，上游面板也没有）→ 现显示「xxx 限流 · 剩余时间」标签（带 tooltip 列全部受限模型与到期时刻）。**账号级冷却/熔断/降权的既有渲染不变** |
| 15 | **内容拦截回的是上游原文 + 英文 hint（跟随上游，2026-09-17 审计确认）** | 面板版把内容拦截改写成中文分类文案（`触发网站风控违禁词…内容命中网关内容防火墙规则[色情]`，按 色情/暴力/政治/赌博/毒品 归类）；上游改为 **error-passthrough**：`message` 装上游 body 原文，`gateway_hint` = `request content was rejected by content policy; adjust the prompt and retry`（**不含上游字样**，有泄漏守卫测试）。要恢复中文分类需在 `hintOf` 里按 `upstream` 的关键词表拼一句 —— 属面板层增强，可做但会分叉 |
| 16 | **`model_cooldowns` 现在落盘（跟随上游）** | 面板把模型级 6004 冷却当**运行时态**（不落盘、重启清零，两个用例锁定）；上游 `stateAccount.ModelCooldowns` 带 `json:"model_cooldowns"` → **持久化**，重启不失忆。面板那两条断言已删 |
| 17 | ⭐ **`session.deriveKey` 内容派生粘性兜底（面板独有，2026-09-17 恢复 `fix_iter15.py`）** | 上游 `ExtractKey` 只认显式 `conversation_id`/`conversationId`/`metadata.*`，无标识客户端（dsh/Codex/Cherry Studio）恒空 → 不粘、同对话换号、上游前缀缓存 miss。面板版末尾有兜底：`SHA-256(system 文本 + 首条 user 文本)` 前 16 字节 → 键前缀 `d-`（与显式 id 命名空间隔离）。**换基时漏贴了它**（无外部引用 → 编译不报错），导致 dsh 从"能粘"变"纯轮转"。恢复后实测：同 body 连发 3 次固定落同一账号 |

---

## 5. 面板层功能地图（上游没有，别删）

- `internal/panel/` — Web 管理面板（账号/券码/任务中心/自动任务/用量/设置），挂 `/panel/`
- `internal/livecfg/` — 运行期可变配置（api_key/soft_rate/脱敏开关，保存配置即生效）
- `internal/usage/` — 逐请求用量记录器（`data/usage.json`）
- `internal/httpauth/` — 网关与面板共用的 Bearer 鉴权（常量时间）
- `internal/upstream/desktop.go` — 桌面六事件链（激活好友靠它）
- `internal/upstream/global_register.go` — 国际版注册/激活
- `internal/upstream/school.go` — 开学季纯 API 客户端
- `internal/upstream/tasks.go` — 成长任务
- `internal/upstream/blackcat.go` — 夜猫子对话链
- `internal/scheduler/blackcat.go` / `school_api.go` — 上述功能的排程闭环
- `internal/upstream/cninvite.go` + `internal/scheduler/cn_invite.go` + `internal/panel/cninvite.go` — **CN 邀请活动**（绑码 + 每日桌面事件链；面板 `/panel/api/cninvite/{status,run}` + 任务中心卡片）
- `scripts/` — 面板自有脚本（`probe_active.py`/`probe_max_tokens.py`/`task_*.py`）+ 上游带过来的 `global_region.py`

---

## 5.1 已采纳的上游未合并 PR（本地补丁，需随上游变化重审）

上游 PR 里**未合并**但有价值的改动可以单独落 —— **只挑相关项，别整包搬**，落完在下方登记
（来源、落了哪几项、为什么没落其余项、怎么验收的）。

### PR #161 `cold-summer`（2026-09-18 落**全部三项**，提交 `255711d`（①③）+ `1b02815`（②））

来源 `https://github.com/Sliverkiss/workbuddy2api/pull/161`（4 提交 / 953 行）。
拉取与取补丁：

```bash
git -C /vol4/_upstream_wb2api fetch origin pull/161/head:pr-161
git -C /vol4/_upstream_wb2api diff origin/master...pr-161 -- <文件> > /vol4/_pr161_x.patch
git -C /vol4/_rebase_try_B apply -v /vol4/_pr161_x.patch      # _rebase_try_B 非 git 仓库，git apply 照样能用
```

| 项 | 内容 | 落否 | 说明 |
|---|---|---|---|
| ① | `fix(upstream)`：`ParseRateReset` 补英文文案形态 | ✅ **落** | **修线上正在踩的 bug**。global 域 429 body 是英文 `… will reset at 2026-09-18 13:26:07 UTC+8 …`，而原正则只认中文「将在 … 重置」→ 解析失败 → 落「无 resetAt」有界退避分支 → `softStreak` 指数翻倍（10→20→40→80→120min 封顶），**且走账号级冷却清空 `modelCooldowns`**（一个模型限流 → 该号全模型不可用，而上游原话正是"可以切其他模型继续用"）。落法：正则拆 `softRateResetPatternCN`/`EN`，EN 锚定 `reset at (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})`（不捕获自然语言），`ParseRateReset` 先 CN 后 EN |
| ② | `feat(server)`：`/v1/stats` 请求统计端点 | ✅ **落** | 新增 `GET /v1/stats` + `POST /v1/stats/reset`（按模型聚合 token/缓存/延迟/扣费，纯内存、重启清零）。`metrics.go`(313)/`metrics_test.go`(148) **直接抄 PR 原文件**（自包含，只用标准库 + `chatStat`）。**必须手工适配**（本仓 `chatStatsReader` 已被面板层改写为 pointer 语义，PR 的 `s.prompt`/`s.tokens` 不存在）：<br>· `logging.go`：`chatStat` 加 metrics 字段；`done()` 落 `recordChatMetric`（单一埋点，流式/非流式/错误路径全覆盖）；`chatStatsReader` 加缓存三段 + `PromptTokens()`（取 `promptTokens`）/`CacheTokens()`；`parseSSELine` 解析 `prompt_cache_{hit,miss,write}_tokens`<br>· `handler.go`：挂两个路由；流式/非流式各补一处取值（非流式走 `fillStatFromUsage(st, resp)`，`resp` 本就是 `map[string]any`）<br>· `client.go`：顺手修 `reModelRateLimit` 在 var 块里的 gofmt 对齐（加 EN 正则后名字变长） |
| ③ | `feat(pool)`：`auths` 目录热加载 | ✅ **落** | 新文件 `internal/pool/watch.go` + `watch_test.go`，`cmd/server/main.go` 挂 `p.StartAuthDirWatch(cfg.AuthDir)` → **加完账号免手动重启**（此前 `SyncToDir` 只在启动时跑一次，面板显示"已添加"但状态"未加载"）。落地**必改**：PR 的 import 是 `workbuddy2api/internal/auth`，本仓模块名是 `github.com/linguo2625469/workbuddy2api-panel` |
| — | `dev.sh` / `.gitignore` | ❌ 未落 | 上游开发脚本，与本仓部署方式无关 |

**验收（都实测过，不是"应该没问题"）**：

- `go build` / `go vet` / `go test`（19 包）三项 EXIT=0；新增用例 `TestParseRateReset_English`(4 子例)、
  `TestReloadAuthDir*`/`TestStartAuthDirWatchNoopOnBadDir`(4 条)、`TestMetrics*`(6 条) 全过；
- **①用线上真实 body 单测**：`ParseRateReset` → `2026-09-18 13:26:07 (UTC+8)`（临时用例，验完即删）；
- **②线上实测**：重启后打 2 个请求 → `GET /v1/stats` 返回按模型聚合（`total`: 3 请求 / 输入 **189,862**
  token vs 输出 **1,839** / `cache_hit_rate` 15.4% / `avg_ttfb_ms` / `tokens_per_sec` / 逐模型 `models[]`），
  字段结构正常（—— 顺手印证 PR 作者那句"成本几乎全在输入侧"）；
- **③端到端**：用同 uid 辅助容器 `touch auths/<f>.json`（宿主机用户无权 touch，文件属主是容器 uid 10001）
  → 9s 内日志出 `[watch] auths 目录变化：账号数保持 11（已热加载凭证更新）`，
  且 `/status` 的 `total/healthy/cooling/sticky` **完全不变**（状态未被重置 —— 正是 PR 用例
  `TestReloadAuthDirPreservesState` 锁的契约："热加载不得重置既有账号的冷却/计数状态"）。

> ⚠️ PR 若后续被上游合并（或内容有变），**要回来重审这一节**：本仓是"提前采纳"，不是"已对齐上游"。

## 6. 禁止事项

- ⛔ 别用上游 `Dockerfile`/`docker-compose.yml`/`config.example.json` 覆盖（L0 补丁：镜像站 401 绕行、entrypoint 指向 `/app/data/config.json`、PUID/PGID）
- ⛔ 别把 `blackcat_hours` 改成 `cat_hours`（生产配置在用）
- ⛔ 别 `git clean -fd`（会删掉未跟踪的 `activate_friend.py`/`bind_invite.sh` 等）
- ⛔ 别用上游 `scripts/` 覆盖本仓库 `scripts/`（面板脚本会丢）
- ⚠️ 上游带 Revert 历史的改动不要跟着搬（先看 `git log`）
- ✅ 日期敏感测试的 TZ 坑已由上游 **#130** 修掉（测试桩改 CST 自然日口径）→ **2026-09-17 起不再需要 `TZ=Asia/Shanghai`**
- ⛔ 别把 `internal/panel/*`（app.js/index.html/panel.go）退回旧版——面板仓库 1.10.0 的 UI 增量（连败降权显示、成本台账 tooltip、模型能力徽标）必须保留
- ⛔ 别把 `internal/session/session.go` 的 `deriveKey` 兜底退回上游版（面板层增强，见 §4 第 17 条）——退回即"无会话标识客户端粘性失效"
- ⛔ 别在同步时整批覆盖**面板自有测试用例**（`internal/pool/*_test.go`、`internal/server/handler_test.go`、`cmd/server/config_test.go`、`internal/upstream/client_test.go` 等）——文件在≠用例在，丢了不报错
- ⛔ 别把 `internal/pool/watch.go` / `watch_test.go` 的 import 退回上游写法 `workbuddy2api/internal/auth`——本仓模块名是 `github.com/linguo2625469/workbuddy2api-panel`，退回即编译不过（见 §5.1）
- ⚠️ **部署仓库的工作树领先 git HEAD 63 个文件**（2026-09-18 发现，含生产文件如 `internal/pool/*.go`、`internal/upstream/sse.go`）→ 改前先 `git status --short`；**别用 `git checkout .` / `git stash` / `git reset --hard` 回退**，那会丢掉未提交的换基成果。容器 `build: .` 构建的是**工作树**，不是 HEAD
