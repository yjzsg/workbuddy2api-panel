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

> ⚠️ **2026-09-19 更新**：`session.deriveKey` 已**退役** —— 上游 `8058019`/`10eefa8` 把同一能力**官方化**为 `session.StickyFallbackKey`（且更完善：多认 `prompt_cache_key`、带 `user_id` 抑制、`stickyKey` 与 `sessKey` 分离不污染头族聚合语义）。本仓已迁移到上游实现。上表保留为历史案例。

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
| 17 | ✅ **`session.deriveKey` 已退役（2026-09-19 迁到上游实现）** | 上游 `8058019`+`10eefa8` 把该能力**官方化**为 `session.StickyFallbackKey`（首条 user 文本 sha256 前 16 字节，前缀 `fb:`），且比我们原来更完善：① `ExtractKey` 多认 **`prompt_cache_key`**（pi-ai/dsh 系客户端把会话 ID 放这里，网关 upstream 侧本就认它）；② **`hasUserID` 抑制**（带 `metadata.user_id` / 顶层 `user_id` 的请求不参与 fallback，守 P1-anti-monopoly 契约）；③ ⭐ **`stickyKey` 与 `sessKey` 分离** —— 上游注释明确「不能直接改 `sessKey`：那会连带改变上游头族 `RequestIDForKey` 的聚合语义（会话级 vs 轮级兜底），属于另一条链路的契约」，而我们原来的 `deriveKey` 正是**塞在 `ExtractKey` 内部返回**，属设计偏差，本次一并纠正。另 `a767465` 的 `contentSignature` 让 `firstUserText` 也吃多模态 parts（纯图片轮/首图会话不再碎片化），我们原来只取文本。本仓现仅保留纯诊断的 **`session.ProbeMissingKey`**（只记键名、不改任何出站请求） |

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
- `internal/upstream/streak.go` — 连登兑换 + 抽奖 API（`GrowthStreakFull`/`GrowthRedeemTier`/`LotteryChances`/`LotteryDraw`；2026-09-21 面板同步带入）
- `internal/scheduler/streak.go` — 连登奖励排程（`RunStreakBonusNow`/`streakBonusAccount`；`makeupYesterday` 用 `scheduler.go` 的 bool 版，本文件不重复定义）
- `internal/scheduler/blackcat.go` / `school_api.go` — 上述功能的排程闭环
- `internal/upstream/cninvite.go` + `internal/scheduler/cn_invite.go` + `internal/panel/cninvite.go` — **CN 邀请活动**（绑码 + 每日桌面事件链；面板 `/panel/api/cninvite/{status,run}` + 任务中心卡片）
- `scripts/` — 面板自有脚本（`probe_active.py`/`probe_max_tokens.py`/`task_*.py`）+ 上游带过来的 `global_region.py`
- **用量页的 prompt cache 三段**（2026-09-18 加）— 把上游 usage 帧里的
  `prompt_cache_hit_tokens` / `prompt_cache_miss_tokens` / `prompt_cache_write_tokens`
  **持久化**进 `usage.Recorder`（桶短键 `ch`/`cm`/`cw`），用量页三张表加「缓存命中 / 命中率」两列、
  卡片区加两张卡（6 列改 4 列，8 张卡两行）。口径：
  · **计数器语义**（缺失即 0，不设 Has 标志）—— 与 freebuff `panel-metrics.js` 的
    `cache_read_input_tokens`/`cache_creation_input_tokens` 同口径；
  · 命中率分母 = 命中 + 未命中，**不含 write**（写入是"为后续命中付的费"）；
  · 前端 `usRate()`：分母为 0 时显示 `—` 而非 `0.0%`（没观测 ≠ 命中率 0%）。
  ⚠️ **`usage.Rollup` 折叠小时桶→日桶是逐字段累加（不是整桶复制）**，新增桶字段必须同步加进去，
  否则折叠后静默丢失（编译不报错、既有用例也不报错）—— 已有 `TestCacheSurvivesRollup` 锁住。
  > 历史注记：同日先做过一版「网关统计」页（读 `server` 包内存聚合 `/v1/stats`），
  > 因用户要求"持久化、直接加到用量里"已**撤掉**（面板侧 Config/路由/handler/前端全还原）；
  > 网关侧 `/v1/stats` 保留（属上游 PR #161 第②项，不是面板层功能）。

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

> ✅ **2026-09-19 复核：PR #161 已被上游合并**（`4b6db7c` Merge pull request #161，含 `f044e5c` ① / `733d348` ② / `c2c0201` ③ + `480ade3` dev.sh）。
> 逐文件核对：`client_english_test.go` **逐字节一致**；`watch.go`/`watch_test.go` 仅差 2 行（**模块路径**，预期差异）；
> 英文限流正则最终形态 `(?i)reset at (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})` **与本仓一致**。
> → 当时的"提前采纳"落得准确，**无需返工**；本节从"提前采纳"转为"**已对齐上游**"。

---

## 5.2 2026-09-19 增量同步（根上游 `9d1a21b → a767465`，13 提交）

本次采纳 3 组：

| 上游提交 | 内容 | 落地要点 |
|---|---|---|
| `3b048ec` | `backfillReasoningContent` 门控对齐官方（`thinkingEnabled \|\| hasTrace`）+ 非 string 值归一化（#165） | 补丁**干净应用**（`thinking.go` + `backfill_test.go`）。注意：仍是**复制** `reasoning` 值 —— 再次确认"历史 reasoning 回放"是上游设计而非 bug |
| `4ac68b7` | global chat 固定走 `/v2/chat/completions`（绕开 `/console` 的腾讯云 WAF 内容规则，#119） | `client.go` 6 hunk 干净应用；`handler_global_test.go` 因上下文不同**手工改 3 处路径断言**；旧 console→v2 fallback 已移除 |
| `8058019`+`10eefa8`+`a767465` | session 兜底键**官方化**：`StickyFallbackKey` / `hasUserID` / `prompt_cache_key` / `contentSignature` | **覆盖** `internal/session/{session,ids,session_test,ids_test}.go` + 新增 `ids_signature_test.go`；`handler.go` **手工移植 4 处**（stickyKey 派生 + 粘性判断/解析/解绑/绑定切到 stickyKey，**会话头族保持 sessKey**）；`handler_test.go` 的 `TestHandlerAppendTurnKeyStable` 恢复上游版（原断言依赖已删的 `deriveKey`）；`handler_ids_test.go` 同步（含新增 `TestChatImageTurnAggregation`） |

**本仓保留的唯一 session 包增强**：`session.ProbeMissingKey`（纯诊断，只记键名；已补 `import "sort"`）。
**必改 import**：session 包与 `handler_ids_test.go` 里的 `workbuddy2api/internal/...` → `github.com/linguo2625469/workbuddy2api-panel/internal/...`。

**未采纳**（有意）：
- `a20d06f` admin 账号临时停用/恢复/复活（+1367 行，含 `config admin.enabled` 开关与 `/admin/accounts/{uid}/...` 路由）—— 上游新功能，本仓暂不需要；
- `4f574ce` Windows 原生服务脚本、`480ade3` dev.sh —— 与本仓 docker 部署方式无关。

**验收**：`go build` / `go vet` / `go test`（**19 包**）全绿。`gofmt` 报的 `session.go`/`client.go`/`handler.go` 差异均为**上游/既有**（注释缩进风格、`handler.go` map 对齐老问题），非本次引入。

> ⚠️ 上游做过 **rebase**（先前记录的 `d2cd004` 等 SHA 已不在 `origin/master` 历史里）→ 引用上游提交 SHA 时**要重新核对**。

## 5.3 2026-09-21 增量同步（根上游 `a9ccace → origin/master` 30 提交 + 面板 `6d0bcab → origin/main` 14 条）

### A. 根上游（`Sliverkiss/workbuddy2api`，30 提交）
| 阶段 | 结果 |
|---|---|
| 分诊 | 干净 **38** / 冲突 **27** |
| 干净组 | `git apply -p1`（6479 行 / 38 文件）+ sed import 前缀（残留 0） |
| 三方合并 | 13 个 rc=0 自动落地 |
| 冲突块 | 14 文件 / **29 块**全部手工判定（`/tmp/resolve.py <file> <out> <n:ours\|theirs\|both>`） |
| 编译迭代 | **6 轮**（分层遮蔽逐层暴露）→ build/vet/test 全绿 |

关键决策：`handler.go`(7 块：1 both / 2 theirs / **3 ours** / 4-7 theirs)、`session.go`→ours（`"sort"` import 是 `ProbeMissingKey` 所需）、`pool/watch.go`→ours（module 前缀）、`thinking.go`→**theirs**（PORTING 禁止事项）、`payload_test.go`→both、`config.example.json`→**ours**（§12：内联是面板既定形态）、`logging.go`(1 ours / 2 theirs / 3 both)、`cmd/server/config.go`→both、`metrics.go`→theirs（`Credits` 字段）、`README.md`(6 块)→both。

**采纳要点**：`/v1/stats` 倍率列（`metrics.Credits` + `enrichCredits`）、admin 路由（`AdminEnabled`）、`ErrImageInvalid` 族、global 模型目录 catalog 体系（`model_catalog.go`/`modelsdev.go`）、tool_pairing / transport / truncation 新文件、默认提示词换小码酱。

**⚠️ 教训（本轮新增，务必记住）**：**`both` 不能用在含「块级结构边界」的冲突块上** —— 本轮 6 次编译报错里 **5 次**都是这个原因：
| 文件 | 症状 | 修法 |
|---|---|---|
| `handler.go` | ours 块含 struct 闭合 `}` → 上游 `AdminEnabled` 字段落到方法体后 | 抽出字段块插回 struct 内 |
| `logging.go` | 上游方法重复声明 + `s.tokens`/`s.prompt` 不存在 | 删重复方法 + 删错位赋值 |
| `cmd/server/config.go` | `if err := c.validateScheduleHours(); err != nil {` 的 body 被挤掉 | 补回 `return err` + 闭合 |
| `scheduler_test.go` / `payload_test.go` | ours 测试函数尾缺 2 个 `}` | 补闭合 |

→ **正确做法**：这类块**先取 ours，再手工把上游新增内容插到正确位置**（字段进 struct、语句进 if 体）。

**另**：`cmd/server/config.go` 取 both 后引用了上游 `c.Schedule.Normalize()`（上游把排程归一移到 `internal/config` 包）→ **移除该调用**，保留本仓上方的内联补齐逻辑（§12 已预警）。

### B. 面板上游（`linguo2625469/workbuddy2api-panel`，14 条）
**关键策略**：面板仓库对**上游派生文件**的版本**落后根上游** → 对这些文件做三方合并会把刚同步的新代码**倒退**。
→ **只合面板层文件**，上游派生文件一律跳过。

| 提交 | 性质 | 处理 |
|---|---|---|
| `bb1dfe6` 校园日活动与小程序首对话 | 面板层（`internal/panel/*` + `upstream/{school,tasks}.go`） | **合** |
| `08752df` run_queue 建队合并 mp 口径待办 | 面板层（`taskcenter.go`+14） | **合** |
| `c3cc888` 模型目录双域分流 + 前缀输出 | 面板层 `panel.go`+129 | **合**（upstream 部分跳过） |
| `d3488be` usage 真实时间轴 | 面板层 `app.js`+123 | **合** |
| `c192fd1` 券码提示文字 | 面板层 `index.html`(1) | **合** |
| `03ce06d`/`860ec53`/`b59655c` 手工吸收上游 | 上游派生 | **跳过**（已由根上游同步覆盖） |
| `73fe1f8` 移除 max_body_mb | = 根上游 PR #159 | **跳过** |

落地 11 个文件：自动合 `internal/panel/{autotask,taskcenter,tasks}.go`；冲突块 `app.js`(`1:theirs,2:theirs,3:ours,4:ours`)、`index.html`(theirs)、`panel.go`(2 块全 theirs)；面板新增 `internal/scheduler/streak.go`(116)、`internal/upstream/streak.go`(106)。

**去重（面板版与我们既有实现重复的符号）**：
- `upstream/streak.go` 删 `clientToken()`（我们 09-19 已抽成 `client_token.go`）
- `scheduler/streak.go` 删 `makeupYesterday()`（`scheduler.go:686` 有更完善的 **bool 版**，用 `GrowthHeatmap`+`HeatmapDayScore`）
- `upstream/streak.go` 常量去重：`streakRedeemPath`→`redeemPath`、删重复 `lotteryDrawPath`（`growth_reward.go` 已有）
- 清 `time`/`crypto/rand`/`encoding/hex`/`fmt` 未用 import

### C. ⭐ F 步符号级遗漏审计（基线 `3f55d50`）
脚本在 `/vol4/`（**下划线前缀**）：`_audit2.py`、`_restore_panel_tests.py`。
```
python3 /vol4/_audit2.py <panel_repo> 3f55d50 <tree>
→ A) 生产缺失 45 个   B) 仅测试缺失 21 个
```
**逐条定性结果：① 上游等价替代 / ② 有意退役 / ③ 真丢失 = 0**

| 分类 | 实例 |
|---|---|
| ① 等价替代 | `hardMarkers`→`hardRule`、`softRateMarkers`→`softRateRule`（§3.1-F 已举过的例子）、`modelEntry`→`applyModelInfoFields`、`fetchGlobalModelInfos`→`globalInfos`、`PickExcludingForModel`→`PickExcludingForRealm`、`PickByUID`→`PickByUIDForModel`、`deriveKey`→`StickyFallbackKey`、`trunc`→`short()`、`adoptReportGap`→`travelAccountDelay`/`activityReportGap`、`TestTransitionReviveClearsCoolingKeepsBreaker`→**拆 3 个**、`TestRequestIDForKeyStability`→`TestRequestIDForKeyDerivation` |
| ② 有意退役（§4 登记） | `cmd/credit\|signin`（**§4-8 明示"保持上游版，面板版是旧快照"**）、`SetMaxBodyBytes`/`max_body`（§4-10）、内容拦截那批（§4-15）、`derivedKeyPrefix`/`messageText`（§4-17）、`TestMaxBody*`/`TestChatOversized*`/`TestModelCooldownsNotPersisted`（§4-16） |
| ③ 真丢失 | **0 项** |

**唯一补回**：`internal/auth/auth.go` 的 `func GlobalEnabled() bool`（面板新增观测函数，3 行）。

**⚠️ 方法论（本轮新增）**：
1. 判断"缺失符号是否真丢"，**要对照面板 `origin/main`**（我们的直接来源），不是根上游；再叠加 §4 差异表定性
2. 审计脚本按 `audit_panel*` 搜不到（实际名 `_audit2.py`）

### D. 验收（2026-09-21 14:34 部署）
```
docker compose build → 镜像 b0d54af05dcf（旧镜像已 tag wb2api-rollback-20260921）
docker compose up -d → workbuddy2api  Up (healthy)
① /healthz            → 200
② /status             → total=32 healthy=32 cooling=0 | realm {global:27, cn:5}
③ /v1/models          → 66 个（cn: 42 + global: 24），ctx 真值（1000000/300000/176000…）
④ /panel/             → 200；/panel/api/config → 200
⑤ 非流式 cn:fast-model → "收到。"（usage 带缓存三段）
   流式 global:fast-model → heartbeat + delta 逐帧 + 末帧 usage（prompt_cache_hit_tokens=128, credit=0.11）+ [DONE]
```
备份：`/vol4/_panel_backup_2026-09-21_1433.tgz`（849KB）

---

## 6. 禁止事项

- ⛔ 别用上游 `Dockerfile`/`docker-compose.yml`/`config.example.json` 覆盖（L0 补丁：镜像站 401 绕行、entrypoint 指向 `/app/data/config.json`、PUID/PGID）
- ⛔ 别把 `blackcat_hours` 改成 `cat_hours`（生产配置在用）
- ⛔ 别 `git clean -fd`（会删掉未跟踪的 `activate_friend.py`/`bind_invite.sh` 等）
- ⛔ 别用上游 `scripts/` 覆盖本仓库 `scripts/`（面板脚本会丢）
- ⚠️ 上游带 Revert 历史的改动不要跟着搬（先看 `git log`）
- ✅ 日期敏感测试的 TZ 坑已由上游 **#130** 修掉（测试桩改 CST 自然日口径）→ **2026-09-17 起不再需要 `TZ=Asia/Shanghai`**
- ⛔ 别把 `internal/panel/*`（app.js/index.html/panel.go）退回旧版——面板仓库 1.10.0 的 UI 增量（连败降权显示、成本台账 tooltip、模型能力徽标）必须保留
- ✅ **`deriveKey` 已于 2026-09-19 退役**（上游 `8058019`/`10eefa8` 官方化为 `StickyFallbackKey`，比我们原实现更完善）→ **别再补回 `deriveKey`**；同步 session 包时只需保留纯诊断的 `session.ProbeMissingKey`（见 §4 第 17 条）
- ⛔ 别在同步时整批覆盖**面板自有测试用例**（`internal/pool/*_test.go`、`internal/server/handler_test.go`、`cmd/server/config_test.go`、`internal/upstream/client_test.go` 等）——文件在≠用例在，丢了不报错
- ⛔ 别把 `internal/pool/watch.go` / `watch_test.go` 的 import 退回上游写法 `workbuddy2api/internal/auth`——本仓模块名是 `github.com/linguo2625469/workbuddy2api-panel`，退回即编译不过（见 §5.1）
- ⚠️ **别用 `git checkout .` / `git stash` / `git reset --hard` 回退**。2026-09-18 曾发现工作树领先 HEAD 63 个文件（含生产文件），已提交让 **HEAD == 工作树**（`f204e68`）；但容器是 `build: .`，**部署真相始终是工作树** → 改前先 `git status --short`，回退用 `git revert`/逐文件恢复
- ⛔ **别为了本机某个客户端的现象去改 `internal/upstream/thinking.go`**（`injectThinking` / `backfillReasoningContent`）—— 那是上游面向**全部客户端**的契约：**issue #43** 的验收项就是「无 effort 裸请求也开思考」（非它则只发 `thinking` 的客户端拿不到思维链）；**issue #157** 维护者结论是「**客户端配置问题，非网关缺陷**」；**issue #91** 明确 `reasoning_content` 是**要被传递出去**的字段。
  2026-09-18 曾偏离两处（① 不注入 thinking ② 不回放历史 reasoning），**A/B/C 同参数多组对照证明收益不成立**（不注入 vs 注入都退化），**已于 `94b2aee` 全部回滚**，5 个文件与两个上游逐字节一致。
  → 若再遇到「卡循环 / 反复 `finish_reason=length` 空正文」，先走**客户端侧**（`maxInputTokens` 压缩点、`reasoning_effort` 档位、`max_tokens` 预算），别动网关。详见技能 `workbuddy-compact-threshold` §九。
