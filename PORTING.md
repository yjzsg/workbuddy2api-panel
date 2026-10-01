# PORTING.md — 本仓库与根上游的同步手册

> 生成：2026-09-17（P4 换基落地后）。**下次同步上游前先读这份。**
> 上游：`linguo2625469/workbuddy2api-panel`（= 面板主干；本仓库是它的 fork）
>
> ⚠️⚠️ **2026-09-25 起：只跟面板上游，不再跟根上游。**
> 原根上游 `Sliverkiss/workbuddy2api` **已删库**（404 / API Not Found）。
> 延续仓库为 `HanawaBanana/workbuddy2api`（描述明写「原 Sliverkiss/workbuddy2api 已删库」），
> 但**已按用户决定不再单独跟踪**——面板上游会自己手工吸收根上游的改动。
> NAS 上 `/vol4/_upstream_wb2api` 这个浅克隆自此**仅作历史留档，不再 fetch**。
> 详细核查过程见 §5.6。
>
> 🚀 **2026-09-28 起：构建与部署已改走 GitHub。** 源码推到 `yjzsg/workbuddy2api-panel`
> （remote 名 `fork`），由 GitHub Actions 构建镜像推到 GHCR，NAS 只负责 `pull`。
> **NAS 上不再 `docker compose build`**（会卡在 `apk add`，见 §7 A）。
> 部署 = `docker compose pull && docker compose up -d --force-recreate`；回滚 = 切 `sha-<短sha>` tag。

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

# ④ 落地（备份 → 替换 → 提交推送 → CI 构建 → NAS 拉取 → 验收）
cd <家目录>/docker/workbuddy2api-panel
tar -czf /vol4/_panel_backup_$(date +%F_%H%M).tgz internal cmd scripts go.mod go.sum login.sh signin.sh credit.sh Dockerfile docker-compose.yml config.example.json
rsync -a --delete <tree>/internal/ internal/
rsync -a --delete <tree>/cmd/ cmd/
cp <tree>/go.mod <tree>/go.sum . && cp <tree>/login.sh .
cp <tree>/scripts/global_region.py scripts/            # login.sh 的 global 注册流程依赖
git add -A internal cmd && git commit -m "..." && git push fork HEAD:main
#   ↑ push 触发 .github/workflows/build-image.yml：runner 上构建 + 推 GHCR（约 1–2 分钟）
#   查进度：api.github.com/repos/yjzsg/workbuddy2api-panel/actions/runs
docker compose pull && docker compose up -d --force-recreate
# 验收：healthz / status（账号数+realm）/ v1/models（cn:+global:，ctx 真值）/ 面板 UI / 1 次流式+非流式请求
```

**踩过的坑**：~~`docker compose build` 若在 `alpine:3.20` 报 401（NAS 镜像站 docker.fnnas.com 间歇性）→ 先 `docker pull alpine:3.20`~~
——**2026-09-28 起 NAS 不再 build**，改走 CI（§7）。这条留作历史：当时能过只是因为 `apk add` 那层还在构建缓存里。

### 3.1 增量同步（日常小步更新走这条，别重走全量换基）

> ⚠️ **2026-09-25 起只有一个上游**：面板仓库 `linguo2625469/workbuddy2api-panel`。
> 原根上游 `Sliverkiss/workbuddy2api` 已删库（见文首与 §5.6），不再跟踪。
> 下面 `git -C /vol4/_upstream_wb2api ...` 的命令**只适用于 2026-09-25 之前**，留作历史；
> 现在同步直接对本仓库的 `origin`（= 面板上游）操作。
>
> ⚠️ **但「一个上游」不等于「可以整文件覆盖」**——本仓与面板上游在 `internal/upstream/client.go`
> 等文件上**结构性分叉**（本仓走根上游的 `errorRule/matchMode` 重构，面板仍是 `xxxMarkers`）。
> 逐文件三方合并只在**结构相同**的文件上有效；结构不同的必须**手工移植**（见 §5.6 D 节）。
> 判据：`comm -23 <(ours 顶层符号) <(theirs 顶层符号)` —— 两边互不包含就是结构分叉。

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
git add -A internal cmd && git commit -m "..." && git push fork HEAD:main   # 触发 CI 构镜像
docker compose pull && docker compose up -d --force-recreate

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

## 5.4 2026-09-22 增量同步（面板 `b69d06e → origin/main` 6 条；根上游**无实质更新**）

### A. 根上游 `Sliverkiss/workbuddy2api`：12 提交，**无实质更新**
`2e2f08e`（09-21 14:01，上次同步点）→ `origin/master`：12 条里 11 条是 `.github` 治理/CI
（PR 评审流水线、OIDC 换票、workflow_dispatch…），**唯一的代码改动是 `README.md +6`**
→ **本次不动根上游**。

### B. 面板上游 `linguo2625469/workbuddy2api-panel`：6 提交 / 7 文件 +371-68
| 提交 | 内容 | 文件 |
|---|---|---|
| `9371f7d`+`c294926`（**PR #35**） | **Add Account 对话框内支持 cockpit tools JSON 导入** | 新增 `internal/panel/import.go`(+181)、`app.js`(+52)、`index.html`(+46) |
| `5e1422c`+`1fba6b4`+`ab9a162`（**PR #36**） | **Docker bind mount 下保存配置失败**修复（保留 tmp 新内容） | `cmd/server/main.go`(+27) |
| `c206468` | **用量页时间窗口全口径生效**（折叠出的日桶也受窗口过滤） | `internal/usage/usage.go`(+77)、`usage_test.go` |

**处理方式（按 §3.1 判据，全部是面板层 → 逐个三方合并，不整包覆盖）**：
`git merge-file -p <file> <b69d06e> <origin/main>`：
- 干净自动合 5 个：`cmd/server/main.go`、`internal/panel/index.html`、`internal/panel/panel.go`、
  `internal/usage/usage.go`、`internal/usage/usage_test.go`
- `internal/panel/app.js` **1 处冲突 → 取 both**：
  - theirs（上游新增）：`数据自 <since>` + `文件 ` 前缀
  - ours（本仓面板层增强）：`命中率分母 = 命中 + 未命中（不含 write）`
  - → 两段都保留（实测该文案**只在本仓存在**，base 与上游都没有）
- `internal/panel/import.go`：上游新增文件，直接取

### C. ⭐ 适配：`import.go` 调的是**旧版上游接口**（不覆盖、改调用方）
面板上游对上游派生文件的版本**落后根上游**（§3.1），`import.go` 按旧签名写：

| 接口 | 面板上游（`import.go` 期望） | 本仓（根上游最新） |
|---|---|---|
| `Upstream.UserResource(a)` | `(rm, tt, err)` 3 值 | **`(remain, err)`** 2 值（total 由 `UserResourceDetailed` 拆分） |
| `Pool.ReenableIfCredits` | `(uid, remain, total)` | **`(uid, remain)`** |

→ 按 §3.1「上游派生文件永远以我们的树为准」**改调用方**：
```go
if rm, err := p.cfg.Upstream.UserResource(a); err == nil {
    p.cfg.Pool.ReenableIfCredits(uid, rm)
}
```

### D. ⭐ 一处**用例假设过期**（不是代码回归）
`internal/usage/usage_cache_test.go::TestCacheSurvivesRollup` 用 `Snapshot(24, nil)` 却期望
100 天前折叠出的日桶被计入 → `c206468` **有意**改成"日桶也受时间窗过滤"（`hours<=0` 才是全部历史）
→ 该用例必然 0/0/0。
**核验**：折叠逻辑里 `dst.CH += src.CH` / `dst.CM += src.CM` / `dst.CW += src.CW` **三行都在**
（本仓自己的注释「缓存三段必须一起搬」也保留）→ **不是折叠漏字段**。
**修**：用例改用 `Snapshot(0, nil)`（靶子是折叠，不是窗口口径），并加注释说明。

### E. 验证
```
go build ./...  → 通过
go vet   ./...  → 通过
go test -count=1 -timeout 480s ./...  → 全绿（修 D 之后）
```
备份：`/vol4/_panel_bak_2026-09-22_2056.tgz`

## 5.5 2026-09-23 **本仓自研修复**：边缘层 401 → 全池降权（上游两仓库均未修）

### A. 事故现象（2026-09-22 22:41:10–22:45:50）
- 2h 窗口 442 请求 / **68 个 503**；用量页 `2026-09-22T22` = 467 请求 / **204 errors（43.7%）**，历史最大。
- 崩溃前 22:21–22:38 稳定 10–22 req/min **全 200**；风暴期 22:41–22:45 是 12/6/22/17/11，**水位没变**
  → **不是并发压垮**（网关自身已限 `max_in_flight_global=2` / `max_in_flight=3`）。

### B. 根因（证据链）
1. 401 的 body 全文是 openresty/APISIX 的 `401 Authorization Required` 错误页 → **上游 API 网关鉴权层拒绝**，请求没到业务 app。
2. 22:41:10 **所有号同一秒一起 401**（35 个），含 22:40:43 刚成功过的 `tangerinewren259`。
3. **16 个号「风暴中 401 → 风暴后同 token 立刻 200」**（可复现）→ 账号无辜。
4. 代码链：
   ```
   Classify: 401 且无业务信封 → IsWafBlocked 只认 403，漏过 → 兜底 return ErrClient
   applyErrorPolicy default: if kind == ErrClient { Pool.NoteFailures(uid) }
   NoteFailures: consecutiveFails++ → 满 degrade_threshold=5 → degradeUntil = now + 10m
   ```
   → 5 分钟风暴里每个号被打 5+ 次 → **33/40 号一起降权 10 分钟**（`degrade_cooldown: 10m`）。
5. 池子被打空（可用只剩 cn 5 / **global 2**），请求全是 `global:deepseek-v4.1-flash`
   → 只剩 `fallback_earliest_expiry` 挑**降权号**硬打 → 还是 401 → **503**。

**代码里的不对称（bug 本体）**：`IsWafBlocked` 只认 `status == 403`。同一个「边缘层 HTML 拒绝页」，
403 归 `ErrWafBlock`（软冷却 60s、**不喂连败**），401 却落 `ErrClient`（**喂连败** → 降权 10m）。

### C. 上游核查结论（**两仓库都没有此修复**）
| 上游 | 位置 | 结论 |
|---|---|---|
| 根上游 `Sliverkiss/workbuddy2api` | `origin/master` `9a26ae7`（`a9ccace` 之后 **59 提交**） | `IsWafBlocked` **一字未改**（仍只认 403）；`internal/pool/{degrade,entry,pool}.go` **零改动**；全树无 `StatusUnauthorized` 分类分支、无 `ErrEdgeAuth`/风暴概念 |
| 面板上游 `linguo2625469/workbuddy2api-panel` | `origin/main` `5a6b167`（`ab9a162` 之后 8 条） | `internal/pool/` **未改**；`internal/upstream/client.go` +141/−15 全是 14018/image_url/11135 三连修，**未碰分类链** |

→ 上游注释里已明写 `IsWafBlocked —— 403 且无业务信封（HTML 拦截页）：APISIX WAF`，
说明他们知道是 APISIX，但**只覆盖了 403 形态**。故本修复为**本仓自研**，非上游移植。

### D. 改动（5 改 + 3 新增 + 3 删）

| 文件 | 变更 |
|---|---|
| `internal/upstream/client.go` | 新增 `ErrKind` 枚举 `ErrEdgeAuth`（插在 `ErrWafBlock` 后）+ `String()` 分支 + `IsEdgeAuth(status, body)`（与 `IsWafBlocked` 逐字同构，只差状态码）+ `Classify` 第 11 层（判在通用 4xx 兜底**之前**）+ 判定顺序文档 |
| `internal/upstream/hint.go` | `case ErrEdgeAuth` → gateway_hint（与 WAF 同语义：换号不换 IP，等窗口） |
| `internal/server/edgegate.go` | **由 `wafip.go` 泛化**：`wafIPGate`→`edgeGate`，`wafIPWindow`→`edgeWindow`，`noteWaf`/`noteEdgeAuth` 共用 `note(uid, shape)`。**401 与 403 共用同一判定窗**（都是「出口 IP 被边缘层拒」的证据，只是状态码不同；混排抖动更快识别）。阈值/窗/不续期语义**逐字保留**（`edgeThreshold=2` 不同 UID / `edgeWindow=60s`） |
| `internal/server/handler.go` | ① 字段 `wafIP`→`edgeGate`；② 轮转循环加 `if kind == ErrEdgeAuth && h.edgeGate.noteEdgeAuth(uid) { break }`；③ `applyErrorPolicy` 新增 `ErrEdgeAuth` → **零账号惩罚**（不冷却/不熔断/不 NoteError/**不喂连败**）；④ 末端错误映射 `edge_auth_rejected` / `edge_auth_blocked`；⑤ 策略表文档补第 11 条 |
| `internal/upstream/{client,hint}_test.go` | 新增 401 形态用例（含「带信封 401 仍是 SessionDead/Client」的不劫持守卫） |
| `internal/server/edgegate{,_conc}_test.go` | 由 `wafip{,_conc}_test.go` 改名 + 新增 401 用例 |
| ~~`internal/server/wafip{,_conc}_test.go`~~ | **已删**（改名进 `edgegate*`） |

**设计要点**：账号惩罚与「停止打上游」**解耦**——账号零惩罚（账号无辜），
止打职责交给 IP 级状态机（短窗 2 个不同 UID 即 fail-fast 终止轮转）。
效果：风暴中每次客户端请求只打 1–2 次上游（原先 5–10 次），且**不再有任何号被降权**。

### E. 验证
```
gofmt -l（仅本仓既有的 client.go/hint_test.go 为脏，非本次引入；本次碰过的文件全 clean）
go build ./...  → 通过
go vet   ./...  → 通过
go test -count=1 -timeout 480s ./...  → 全绿（21 包）
```
新增用例（均 PASS）：
- `TestIsEdgeAuth` / `TestClassify`（含 401 无信封→EdgeAuth、401 带信封→SessionDead/Client 守卫）
- `TestEdgeGateMultiAccountTriggers` / `TestEdgeAuthGateMultiAccountTriggers` / `TestEdgeGateWindowExpiry`
- `TestEdgeGateSharedWindowAcrossShapes`（401+403 混排共用窗）
- `TestEdgeGateConcurrentMixed` / `TestEdgeGateConcurrentSingleUIDPerAccount`（并发）
- ⭐ `TestChatEdgeAuthFailFastStopsRotation`（2 次上游调用即 break + **账号零惩罚**断言）
- ⭐ `TestChatEdgeAuthNeverDegradesPool`（**事故精确回归**：单号池连撞 8 次 401，`consecutive_fails` 恒 0、`degrade_until` 恒零、号仍可选）
- `TestChatEdgeAuthSingleAccountRotatesToHealthy`（单号 401 仍轮转到健康号）

⚠️ **`-race` 本机跑不了**：`-race` 需要 cgo，而 NAS 无 gcc、`golang:1.23`（Debian 版）拉不动
（registry-1.docker.io TLS 超时）、alpine 容器内 `apk add gcc` 也超时（网络受限）。
项目历史验证标准本就是 `build+vet+test`（见 §3.1 E 步），**本仓从未跑过 `-race`**，非本次降级。
并发用例仍会执行（只是不带竞态检测）；`edgeGate` 的锁结构与既有 `wafIPGate` 一致（单 mutex 全覆盖）。

## 5.6 2026-09-25 上游变动 + 面板同步（`5a6b167 → dbd7c68`，8 提交）

### A. ⚠️ 根上游 `Sliverkiss/workbuddy2api` 已删库

| 核查 | 结果 |
|---|---|
| `https://github.com/Sliverkiss/workbuddy2api` | **HTTP 404** |
| `api.github.com/repos/Sliverkiss/workbuddy2api` | `{"message":"Not Found"}` |
| 用户 `Sliverkiss` 的 77 个公开仓库 | **无 workbuddy2api**（只有个不相干的 `CodeBuddy2api`） |
| 症状 | NAS 上 `git fetch` 报 `could not read Username for 'https://github.com'` |

**判据**：public 仓库的 `git fetch` 突然要凭据 = **仓库没了**（GitHub 对不存在/私有仓库回 404，
git 转去问账号密码），**不是 token 过期**，别往那个方向查。

**延续仓库 = `HanawaBanana/workbuddy2api`**（描述：「WorkBuddy2API 延续仓库 —— 账号池转 OpenAI 兼容 API
（原 Sliverkiss/workbuddy2api 已删库）」，提交 `a65565d0 docs: 标注延续来源`）。
我们的基线 `9a26ae7` 是它的祖先（`compare` → `ahead_by=3`）。

**怎么找到延续仓库（可复用）**：
```bash
api.github.com/search/repositories?q=workbuddy2api        # 72 个同名仓库
api.github.com/repos/<候选>/commits/<我们的基线sha>        # 200=同一棵树，422=不是
api.github.com/repos/<候选>/compare/<基线sha>...HEAD      # status=ahead 才算接得上
```
⚠️ **父仓库被删后，GitHub 会把 fork 的 `fork` 字段置 false、`parent` 置 null**
→ 不能用 `fork` 字段判断，**只认「含基线提交」这一条判据**。
同名的那堆（`Zhengyuuuui`/`hawklithm`/`xiaofan6ya`/`ckldy`）都不含我们的基线提交，**不是同一棵树**。

**用户决定（2026-09-25）**：**不再单独跟踪根上游**，只跟面板上游
（面板会自己手工吸收根上游改动，如 `d47219b 吸收上游三连修`）。
→ `/vol4/_upstream_wb2api` 自此仅作历史留档，不再 fetch。

> 备注（未执行）：新根上游 `9a26ae7..5e2c2b4d` 有 3 提交，实质只有 `internal/server/handler.go +7`
> （成功响应透出 `X-Wb-Account` 头）；`Dockerfile`/`docker-compose.yml` 是 L0 本地补丁版不可同步，
> `README.md` 是文档。面板上游**不含**这 3 提交（`X-Wb-Account` 在面板树里不存在）。

### B. 面板上游 8 提交（`5a6b167..dbd7c68`）

| 提交 | 内容 | 文件 |
|---|---|---|
| `09fd96e` | 待办扫描过滤上游锁定任务（Sequential 每日解锁环不再误入队列） | `taskcenter.go +7` |
| `1d7c97b` | 扫描待办结果不再被上一轮队列残留轮询冲掉 | `app.js ±18` |
| `c564e90` | 修复 v1.11.3 任务中心 JS 崩溃（`pollQueueOnce` 残留调用点） | `app.js +13/-2` |
| `410309c` | 修复 `reattachQueueView` 顶层 TDZ 崩溃 + **JS 冒烟测试入册** | `app.js +5/-1`、**`frontend_test.go +67`（新文件）** |
| `2b0eedd` | **模型倍率列显示优惠生效价**（牌价+折扣+标签+时段说明） | `app.js +20`、`panel.go +12`、`client.go +161` |
| ×3 | 版本号 1.11.3 / 1.11.5 / 1.11.6 | `main.go` |

### C. ⭐ 关键判据：两条线在 `client.go` 已**结构性分叉**，不能用三方合并

| | 本仓（= 根上游架构） | 面板上游 |
|---|---|---|
| 分类规则实现 | `errorRule` + `matchMode`/`matchPattern`（17 处） | 纯 `xxxMarkers` 字符串切片（0 处 `errorRule`） |
| 顶层符号 | 87 个，含 `hardRule`/`sessionDeadRule`/`IsEdgeAuth` | 87 个，含 `hardMarkers`/`sessionDeadMarkers`/`applyModelPromotions` |
| 互包含性 | `comm -23` 双向都非空 → **互不包含** | 同左 |
| `billingMeterJSON` 定义处 | `client.go`（根上游搬来的） | `report.go` |

**行级三方合并对该文件是错的工具**：实测产出 5 处冲突，且冲突边界把函数**从中间切开**
（`billingMeterJSON` 的头在 ours 侧、尾在公共区，`storeEfforts` 的头在 theirs 侧）。
强行按「两个都要」拼会得到语义错误的交错代码。

**正确做法 = 手工移植**（同 §5.1「手工吸收」）：
1. 先判断该提交的改动**是否自包含**。`2b0eedd` 的 `client.go` 部分恰好自包含
   （4 个 `ModelInfo` 字段 + `v3ModelPromotion` 类型 + `promoZone`/`promoClock`/`promoActive`/`applyModelPromotions`
   + 一个调用点），只依赖 `time`/`strings`/`strconv`，与分类链结构无关。
2. 整块搬进本仓结构，**适配调用点**：上游 `out` 是 `map[string]ModelInfo`，本仓 `fetchV3Models`
   产出**切片** → 经 map 中转再回填。
3. 上游把 `modelPromotions` 放在 `fetchV3ConfigModelMap` 的 env 结构里；本仓的
   `parseGlobalModelNames(raw)` **不透出该字段且签名已被多处调用** → 另写
   `parseV3ModelPromotions(raw)` 单独解析（失败返回 nil，优惠是展示性增强，绝不拖垮模型目录）。

### D. 分诊与落点

| 文件 | 方式 | 结果 |
|---|---|---|
| `internal/panel/taskcenter.go` | 三方合并 | 干净 |
| `internal/panel/panel.go` | 三方合并 | 干净（`panelModelEntry` 的 `credits` + `promo_*` 并存） |
| `internal/panel/frontend_test.go` | 新增文件 | 直接取上游 |
| `internal/panel/app.js` | 三方合并 **1 冲突 → 取 both** | 我们的 `loadCNInvite(true)` ‖ 上游的 `reattachQueueView()` |
| `cmd/server/main.go` | 三方合并 **1 冲突 → 取 theirs** | 版本号 `1.10.1-panel` → **`1.11.6-panel`**（跟上游） |
| `internal/upstream/client.go` | ⚠️ **手工移植**（见 C） | +187（比上游 +161 多出适配代码） |

**`app.js` 冲突的教训**：上游把 `pollQueueOnce` 换成了 `reattachQueueView`（那 4 个连环修复就是围绕它），
而我们那一行有自己的 `loadCNInvite(true)` → **必须取 both**，只取一边会丢功能或留悬空调用点。
合并后 `pollQueueOnce` 只剩上游自己的注释引用，**调用点已清零**。

### E. 验证（全绿）

```
gofmt  → 本次碰过的文件均 clean（client.go 的脏是仓库既有，已确认我的新增行合规）
go build ./...  → 通过
go vet   ./...  → 通过
go test -count=1 -timeout 480s ./...  → 全绿（21 包，含新增的 panel/frontend_test.go）
```

**符号级遗漏审计（§3.1 F 步）**：11 项逐条 grep 确认存活 ——
`ErrEdgeAuth` / `IsEdgeAuth` / `Classify` 第 11 层 / `edgeGate.noteEdgeAuth` / `edge_auth_blocked`
（§5.5 的 401 修复）、`loadCNInvite` / `rate_limited_models` / 命中率分母文案（面板本地特性）、
`reattachQueueView` / `promo_factor` / `applyModelPromotions`（本次合入）。
全树无冲突标记残留。

## 5.7 2026-09-28 **本仓自研修复**：内容审核被误判成账号封禁（上游未修）

> ⚠️⚠️ **2026-09-29：本节 B 节的判据（`displayMsg` 文案能分野内容/账号）已被实测证伪。**
> 隔离实例 A/B：`onyxibis257` / `yejzsg@gmail.com` 对内容 `"hi"` **也**返回同一句
> 「内容未通过安全审核」，而同一请求换健康号即 200 ⇒ **那 2 个号是账号级被拒，不是内容问题**。
> `displayMsg` 是面向终端用户的装饰性文案，**不能当分类判据**。
> 本节保留作历史记录；**当前实现见 §5.8**（把判定从「文案」改为「行为」）。

### A. 事故现象
生产 40 个号里 **2 个被 `Pool.Disable` 永久禁用**（需人工 revive）：

```
onyxibis257        disabled=True  reason=account banned by upstream (11140 request illegal), re-login required
yejzsg@gmail.com   disabled=True  reason=account banned by upstream (11140 request illegal), re-login required
```

### B. 根因（证据链）
完整 body（日志在 200 字符处截断，但 `en` 字段完整）：
```json
{"code":11140,"msg":"request illegal","requestId":"...",
 "displayMsg":{"en":"The content did not pass the safety review. Please adjust and retry.",
               "zh":"内容未通过安全审核…"}}
```
**`displayMsg` 明说是内容审核拒绝——用户请求内容触发的，与账号无关。**

代码链（`internal/upstream/client.go` + `internal/server/handler.go`）：
```
Classify: accountFaultRule（matchFold，pattern "request illegal"）先命中 → ErrAccountFault
   ↑ contentBlockedRule 只认 "blocked by security policy" / "unapproved channel" /
     "illegal api invocation"，接不住这个形态
applyErrorPolicy(ErrAccountFault):
   if strings.Contains(lower(body), "request illegal") { Pool.Disable(uid, "...") }
```
→ **把「用户内容触发审核」记成「账号授权封禁」→ 永久禁用健康号。**

**影响**：40 号损失 2 个（5%）；**只要再发一次触发审核的内容就再损失一个**（持续性失血），
且惩罚的是无辜健康号。

**与 §5.5 的 401 事故同类**：把「非账号问题」记到账号头上
（401 = 出口 IP 级 → 全池降权；本例 = 内容级 → 单号永久禁用）。

### C. 上游核查：**未修**
面板上游 `dbd7c68..1e23c2b`（26 提交）无一条碰 `11140` / `displayMsg` / `安全审核`；
根上游留档克隆同样无。→ 本仓自研。

### D. 改动
| 文件 | 变更 |
|---|---|
| `internal/upstream/client.go` | 新增 `contentSafetyRule`（`ErrContentBlocked`，pattern `"safety review"` + `"内容未通过"`）；`Classify` 在 `accountFaultRule` **之前**加一层分流；判定顺序文档补第 3 层 |
| `internal/upstream/client_test.go` | `TestClassify` 加 5 例（zh 形态 / en 形态 / 仅 en / 仅 zh / **真封禁仍 ErrAccountFault** ×2） |
| `internal/server/handler_test.go` | 新增 `TestChatContentSafety11140DoesNotDisable`（事故回归，双子用例） |

**判据设计**：
- 只用 displayMsg 的**文案**，不用「displayMsg 存在性」→ 真·账号封禁若也带 displayMsg 不受影响
- 两个 pattern 互为冗余 → 上游只发 zh 或只发 en 都能命中
- `"内容未通过"` 取**已观测前缀**（日志截断在 `内容未通过安`，不臆造完整文案）

**验证过分类用的是完整 body**：`Classify(resp.StatusCode, string(raw))` —— 未截断；
`truncate(...,200)` 只用于日志与 `Error.Msg`。

### E. ⭐ 测试时发现的行为差异（值得记住）
`NewHandler` 缺省 `PromptMode = "passthrough"` → 首遇 `ErrContentBlocked` 会先做**一次降级重试**
（`prompt.Rewrite(body, prompt.Degraded)` 后 `continue`），再撞才回 400。
而**生产配置是 `prompt.mode = "custom"`** → **不降级重试**，直接 400。
→ 测试用两个子用例分别覆盖（custom=1 次上游调用 / passthrough=2 次）。

### F. 验证（全绿）
```
gofmt（本次碰过的文件 clean；client.go 的脏是仓库既有，已确认新增行合规）
go build ./... / go vet ./...  → 通过
go test -count=1 -timeout 480s ./...  → 全绿（21 包）
```
关键回归：`TestChatAccountFault11140Disables`（**真封禁仍禁用，语义未被破坏**）与
`TestChatAccountFault14017Rotates` 均通过。

### G. 人工补救
那 2 个被误禁的号已 revive：
```bash
curl -X POST -H "Authorization: Bearer <key>" \
  http://127.0.0.1:7863/panel/api/accounts/<uid>/revive      # 路由不需要 body
```
→ 40/40 healthy。

## 5.8 2026-09-29 **本仓自研修复**：内容拦截改为「行为判别」（推翻 §5.7 的文案判据）

### A. 现象

`global:` 模型「经常被拦截」，`cn:` 从不被拦：

```
近 12h：  global 1011 请求 → 200×565 / 400×445（44.0%）
          cn      190 请求 → 200×180 /  400×0
```

400 全部是 `403 content_blocked`（上游 code 11140 + displayMsg「内容未通过安全审核」）。

### B. 根因（两个独立伤害，同一个假设错误）

**错误假设**：`handler.go` 旧分支注释写着「内容命中网关内容防火墙：立即回客户端，
**不轮转**——换任何账号都会撞同一审核」。**该假设被实测证伪。**

隔离实例单账号 A/B（内容 = `"hi"`，同镜像 / 同配置 / 同出口 IP）：

| 账号 | 结果 |
|---|---|
| `gentlenewt309`（对照） | **200** |
| `onyxibis257` | **400 content_blocked** |
| `yejzsg@gmail.com` | **400 content_blocked** |

⇒ 上游对**账号级拒绝**也回同一句 displayMsg。文案不可信，唯一可靠判据是**行为**。

**伤害 ①（客户端可见 400）**：本来换个健康号就能成功，却直接回 400。
**伤害 ②（死号泄漏）**：`ErrContentBlocked` 零惩罚 ⇒ 被拦号 `fails=0 / cooling=False /
disabled=False`，状态永远"最干净" ⇒ 在选号打分里永远最优 ⇒ 被越选越多：

```
onyxibis257        pick=224   ok=0    0%     ← 446/1011 = 44% 的 global 流量落在 2 个死号上
yejzsg@gmail.com   pick=222   ok=0    0%
其余 38 个号        pick=2~156 ok≈pick   ~100%
```

客户端重试 → 又落回这 2 个号 → 再 400 → **重试风暴**（02:30 一分钟 247 个 400）。

### C. 修法：把判定从「文案」改成「行为」

`ErrContentBlocked` 不再立即返回，改为**先轮转**，用「换号后能否成功」判定：

| 结果 | 判定 | 动作 |
|---|---|---|
| 后续某个号**成功** | 内容可过审 ⇒ 被拦的是**账号侧** | 对 `blockedUIDs` 喂 `NoteContentBlockEvidence` |
| 整轮**都被拦** | 换任何号都一样 ⇒ **内容问题** | 末端回 400 `content_blocked`，**零惩罚** |

`NoteContentBlockEvidence`（`internal/pool/state.go`，与 `NoteSessionDead` 同构）：
- 未达 `contentBlockThreshold=3`：软冷却 `contentBlockCooldown=1m` 让位，返回 false；
- 达阈值：`Disable(uid, contentBlockReason)`，返回 true。
- 清零点：`NoteSuccess`（成功是「该号没被上游拒」的直接证据）、`ReviveDisabled`。

⇒ 死号约 2 分钟内被自动摘出池（每次证据后软冷却 1 分钟，冷却结束再被选中才累积下一次），
且**客户端始终拿到 200**（轮转到健康号）。

### D. 代码位置

```
internal/pool/entry.go     + contentBlockFails 字段 / contentBlockThreshold / contentBlockCooldown
                           + contentBlockReason / ContentBlockThreshold()
internal/pool/state.go     + NoteContentBlockEvidence / ClearContentBlock
                           ~ NoteSuccess、ReviveDisabled 各加一行清零
internal/server/handler.go ~ 轮转循环：blockedUIDs 记账 + ErrContentBlocked 改为换号
                           ~ 成功分支：对 blockedUIDs 喂证据
                           ~ 末端 switch：新增 case ErrContentBlocked → 400（零惩罚）
```

### E. 代价（有意接受）

真内容拦截时多打 1~2 次上游（罕见）——换来「账号级误判不再变成客户端可见 400 + 死号泄漏」。
单账号池无法证明是账号问题（没有「另一个号成功」的证据）⇒ **不惩罚**，偏向不误伤。

### F. 验证

新增 8 个用例，核心三条：
- `TestChatContentBlockRotatesToHealthy` —— 被拦号 + 健康号 → 客户端 **200**，被拦号软冷却，健康号零惩罚；
- `TestChatAllContentBlockedReturns400NoPenalty` —— 整轮都被拦 → 400 `content_blocked`，**零惩罚**（保住 §5.7 要保护的性质）；
- `TestChatContentBlockRepeatedEvidenceDisablesDeadAccount` —— 单号池不惩罚（无法证明是账号问题）。
- pool 侧 5 条覆盖阈值/清零/复活/未知 uid。

同时**改写了 3 个编码旧行为的既有用例**（`TestContentBlockedSecondHitReturns400` /
`TestContentBlockedReturnsFirewallMessage` / `TestChatContentSafety11140DoesNotDisable`）——
它们的调用次数断言从「拦截即停」改为「轮转两个账号」，语义变更是有意的。

## 5.9 2026-09-29 查明：网关会「替换客户端 system prompt」（上游设计，本仓改了提示词内容）

### A. 机制（上游本就有，非本仓补丁）

`internal/prompt/defaultprompt.md` 被 `//go:embed` 嵌进二进制；`prompt.mode="custom"` + `file=""`
→ `Load()` 返回内嵌提示词 → `handler.go` 调 `prompt.Rewrite()`：**删除 messages 中所有
role∈{system,developer} 的消息，头部插入一条 `{"role":"system","content":<网关提示词>}`**。

**与域无关**（`global:` / `cn:` 都一样，实测两者都回人格口吻）。

设计动机（`internal/prompt/prompt.go` 包注释）：客户端 CLI 在 system prompt 注入固定模板句，
上游内容审核按**逐字精确匹配**误杀合法流量（issue #36 / PR39 的 11-128），所以网关替换掉它。

### B. ✅ 本仓的偏离已修正（2026-09-29）

```
上游  internal/prompt/defaultprompt.md  = 38 行「你是一名工程助手…」（干净）
本仓  （2026-09-29 之前）= 300 行「Little Code Sauce / YG」人格提示词（b4d4997 一次同步时替换）
本仓  （2026-09-29 起）= **已恢复为上游那份 38 行版，逐字节一致**（`cmp` 验过）
```

**修正前的后果**（2026-09-29 在 DSH 会话里实测）：每个经过网关的请求，模型读到的 system 都是
那份人格提示词 → 模型用第三人称谈「YG」、只输出散文、不认客户端 schema
（探针发 `"hi"` 回 `"hey. what's going on"` —— 该提示词 Casual examples 的原句；
换 `passthrough` 则回干净的 `"Hi! How can I help you today?"`）。

**顺带消掉的风险**：那份人格提示词含大量会被内容审核盯上的词汇
（CSAM / incest / non-con / RAT / stealer / phishing / 露骨词表），**每次请求都发给上游**。
38 行版是干净的工程助手提示词。**"它是否导致账号被上游内容信誉标记"仍是未证实的假设**
（反证：健康号带旧提示词也照样 200），但把这段词汇从出站流量里去掉本身没有坏处。

### C. 三个模式（`handler.go`）

| mode | 行为 |
|---|---|
| `passthrough`（**代码缺省**，`config.example.json` 也写这个） | 透传客户端原始 system |
| `append` | **保留**客户端 system，在开头连续 system/developer 块之后**再插**一条网关 system |
| `custom`（**生产配置是手工设的**） | **删除**客户端所有 system/developer，换成网关提示词 |

### D. 仍未决（`prompt.mode`）

- **`prompt.mode` 仍是 `custom`**（用户 2026-09-29 只选了「恢复 38 行提示词」，未选改 mode）。
  因此**「自带 schema / 工具定义」的客户端（DSH 等）仍然拿不到自己的 system prompt**——
  换掉提示词内容不解决这一点，**只有把 mode 改成 `append` 才解决**。
  代价：客户端 system prompt 会重新出现在发往上游的请求里，11-128 指纹误杀风险回归
  （**注**：2026-09-29 全窗口日志里 `11-128` 出现 **0 次**，该风险当前未观测到）。
  → 要动的话建议先在隔离实例上跑 `append` 观察几天 400 率。

## 5.10 2026-10-01 同步：面板上游 `dbd7c68..8584e45`（53 提交）

### A. ⛔ 基线修正（重要）
上一轮（2026-09-28）把基线记成 `1e23c2b`（27 提交）——**错**。26 提交批次
（`dbd7c68..1e23c2b`）**从未合入**（树里没有 `prefer_expiring`、`normalizeUsageCacheAliases`，
`internal/scheduler/school.go` 还在，版本号仍 `1.11.6-panel`）。
**正确基线 = `dbd7c68`**，本次实际范围 `dbd7c68..8584e45` = **53 提交 / 62 文件**。

**基线验证姿势（每次同步前必做）**：
```bash
git rev-list --count <base>..origin/main          # 数量对不对
git grep -l prefer_expiring HEAD -- '*.go'        # 抽查上一批的标志物是否真在树里
grep -rn 'appVersion' cmd/server/main.go          # 版本号是否跟上了
```
别沿用上一轮的记录——上一轮可能就是错的。

### B. ⛔ 分叉判据必须带签名
上一轮用**只取符号名**的正则做互包含性检查 → `Pick(model)` 与 `Pick()`、
`SetCredits(uid,credits)` 与 `SetCredits(uid,credits,total)` 被当成同名符号，
**签名差异被掩盖**，误判 `internal/pool` 为「无分叉」。实际是**结构性分叉**：

| 本仓（根上游系） | 面板上游 |
|---|---|
| `Pick(model string)` | `Pick()` / `PickExcluding(tried)` / `PickExcludingForModel` / `PickByUID` |
| `SetCredits(uid, credits)` | `SetCredits(uid, credits, total)` |
| `SetCreditsDetailed(uid, credits, expiring)` | `…(uid, credits, total, expiring, earliestAt, earliestRemaining)` |
| `ReenableIfCredits(uid, remain)` | `ReenableIfCredits(uid, remain, total)` |
| `ReviveDisabled(uid) bool` | `ReviveDisabled(uid)` |
| `UserResource(a) (remain, err)` | `UserResource(a) (remain, total, err)` |
| `UserResourceDetailed(a, soon) (remain, CreditBuckets, err)` | `…(remain, total, expiring, err)` |

⇒ 判据：`grep "^func (p \*Pool) [A-Z]"` **取整行**（带签名）做 `comm`。

### C. 本次合并方式（三档）
1. **无冲突 / 上游新增** → 直接取上游（`internal/reqlog`、`logfmt/shortua`、CI workflow、
   各 `*_test.go` 新增用例）。
2. **两侧都在同一处加东西** → 取并集（`internal/usage/usage.go` 的
   credit 分区 ∪ prompt-cache 三段；`chatStat`/`chatStatsReader` 字段；
   `Status` 的 `CreditsTotal` ∪ `ManualDisabled`）。
3. **结构性分叉** → **以本仓为基底，手工移植上游特性**（`internal/pool/*`、`internal/upstream/client.go`、
   `internal/server/{handler,logging}.go`、`internal/scheduler/scheduler.go`、`cmd/server/*`、`internal/panel/*`）。
   ⛔ 不要对这些文件用 `git merge-file` 的自动结果直接落盘。

### D. 本次吸收的上游特性（全部已落地并测过）
| 特性 | 落点 |
|---|---|
| `pool.credit_floor` 积分保底（触底号不接实测收费模型） | `pool/{pick,state,cooldown,pool}.go` + `cmd/server/config.go` |
| `pool.prefer_expiring` 最早到期优先（**虚拟实例 ×3**，替代本仓原「第四因子 ×8」） | `pool/pick.go` `routingWeightOf` / `expiringNow` |
| `NoteCheckinDone` + `Status.CheckinDone`（面板「签到/已签」） | `pool/{cooldown,entry,state}.go` + `scheduler.CheckinAll` |
| `RecordModelRateLimitAudit` + `modelCooldown.AuditOnly` + `RateLimitedModel.Kind` | `pool/{cooldown,entry,state}.go` + `handler` |
| `creditsTotal` / `creditsEarliestExpiry` / `creditsEarliestRemaining` / `lastCheckinDay` | `pool/{entry,persist}.go` |
| `UserResourceDetailedWithExpiry` + `CycleEndTime` + `CreditPackage.ExpiresAt` | `internal/upstream/client.go` |
| 计费类调用瞬时错误有界重试（签到 / 余额查询） | `upstream/{report,client}.go` |
| `parseRetryNumber` 溢出保护、`SanitizeFingerprints` → `atomic.Bool` | `internal/upstream/client.go` |
| 模型倍率快照（`storeModelRates`/`ModelRate`/`normalizeModelRate`） | `internal/upstream/client.go` |
| `internal/reqlog` 请求指标 + JSONL 归档 + 客户端 IP/UA（`logging.request_client_info`） | 新包 + `server/logging.go` + `panel` 两个端点 |
| usage 积分维度（credit/rate 分区 + `fileVersion=3`） | `internal/usage/usage.go` |
| scheduler 墙钟分段睡眠（`waitSlot`）+ `growth` 排程 | `internal/scheduler/scheduler.go` |
| **开学季（school）整体下线** | 删 `scheduler/{school,school_api,school_test}.go`；`taskSchool`→`taskGrowth`；panel/config 同步 |

### E. 本仓自研（上游仍未吸收，同步时务必保留）
`ErrEdgeAuth`/`IsEdgeAuth`（401）、`contentSafetyRule`（内容审核）、`edgeGate`（IP 级熔断）、
`NoteContentBlockEvidence`/`ClearContentBlock`、`SetManualDisabled`/`ManualDisabledState`、
`ModelCost`/`NoteModelCost` 成本账本、`Pick(model)`/`PickByUIDForModel` 选号 API、
面板 CN 邀请 / 每日对话保底 / 缓存命中率列。

### F. 验证
```
gofmt -l（全树 56；**当时误判为「与合并前一致」—— 见 §5.11 C-3：真实基线是 46**，本次已补齐）
go build ./...   通过
go vet   ./...   通过
go test -count=1 -timeout 900s ./...   22 包全绿
符号级遗漏审计：12 项自研 + 14 项上游特性逐条 grep 确认 ✅
school 残留：仅 2 处注释 ✅
```

### G. 一个后续隐患
`internal/pool` 与 `internal/upstream/client.go` 的分叉**只增不减**：本仓带根上游特性、
面板带自己的演进。每次同步都要手工移植。若要根治，需要决定「以哪条线为主干」并做一次换基。

## 5.11 2026-10-01 收尾：CI 红叉查因 —— 一个被 root 掩盖两周的真 bug + 三处合并遗漏

### A. 触发：GitHub Actions 邮件
`go-binaries` run #1（commit `1e71098`）失败。**push 本身没失败**
（`git rev-list --count fork/main..HEAD` = 0）。但失败**是真的**：

```
--- FAIL: TestSaveAtomicPermissionHint (0.00s)
    permhint_test.go:27: 权限错误应包含 Docker 指引，实际:
      open /tmp/.../workbuddy-u1.json.tmp: permission denied
FAIL  internal/auth  0.010s        ← 其余 21 包全绿
```

### B. 根因链（**不是本次合并造成的**，但本次合并才让它暴露）

| 时间 | 事件 |
|---|---|
| 2026-09-12 `08ca79a` | 上游加 `SaveAtomic` 权限指引 + `permhint_test.go` |
| 2026-09-17 `657856e` | 本仓「以上游 `64064ce` 为主干重贴面板层」**整文件替换** `internal/auth/auth.go`，**删掉了指引，却留下了测试** |
| 至今 | 本仓验证一律在 `golang:1.23-alpine` 里**以 root** 跑；该测试首行 `if os.Geteuid()==0 { t.Skip }` → **每次都静默跳过**，从未暴露 |
| 2026-10-01 | 本次合并带进上游的 `go-binaries` CI（runner 用户 `runner`，**非 root**）→ 第一次真跑 → 红 |

⇒ 教训：**root 会把 POSIX 权限类用例整批 `t.Skip` 掉**。本地「全绿」与 CI 绿不是同一件事。

**修复**：`internal/auth/auth.go` 按上游 `origin/main` 版本复原指引（3 条解法，含 `PUID/PGID`），
并加注记说明丢失经过。

### C. 顺带查出并修的三处合并遗漏

| # | 问题 | 处理 |
|---|---|---|
| 1 | `app.js` 的 `usRateTone()` 成**死代码** —— 我改写缓存 KPI 时丢了它的调用，命中率配色信号没了 | 接回：`usRateTone(...)==='warn' ? 'c-warn' : 'c-soft'` |
| 2 | `app.js` `usDimBody` 失败占位 `colspan="10"` **写死**，而 `US_DIMS` 被我改成 12/9 → 切维度错位 | 改为 `US_DIMS[usDim].span` 动态取值 |
| 3 | **gofmt 基线漂移**：合并前 `0c91110` 是 **46** 个脏文件，合并后 **56** —— 我留下 10 个结构体字面量对齐被改宽的脏文件 | `gofmt -w` 那 10 个，回到 46 |

⛔ **第 3 条差点漏掉**：我上一轮拿 `78dfa26`（**已经含我自己改动的 WIP 提交**）当参照比 gofmt，
等于自证清白。**比 gofmt 基线必须拿真正的合并前提交（`0c91110`）**。

### D. ⛔ 撤回上一轮的一个错误决定：`go-binaries.yml` 不该删

上一轮我删了它，理由是「无 release/tag 上下文必失败（每 push 一个红叉）」——
**理由不成立**：它的 `publish` job 有 `if: startsWith(github.ref, 'refs/tags/v')` 门，
`build` job `needs: test` 在 push main 时正常跑五平台编译并存 artifact。
而且**它是本仓唯一跑 `go test ./...` 的 workflow** —— 上面那个真 bug 就是它抓到的。
**已恢复**。

（`docker-ghcr.yml` 删除仍正确：它 `IMAGE_NAME: ${{ github.repository }}` 与本仓
`build-image.yml` 是**同一个 GHCR 包名**，纯重复且会互相覆盖。）

### E. 新增的两条同步纪律

1. **本地验证必须以非 root 身份跑一遍**（`docker run -u 1001:1001`），否则权限类用例静默跳过：
   ```bash
   git archive HEAD | tar x -C /tmp/ci_tree        # 干净树（避开 NAS 上 data/ 属主问题）
   docker run --rm -u 1001:1001 -v /tmp/ci_tree:/src -v /tmp/nrcache:/gocache -w /src \
     -e GOMODCACHE=/gocache/mod -e GOCACHE=/gocache/build \
     golang:1.23-alpine sh -c 'cd /src && go test -count=1 ./...'
   ```
2. **静默丢失审计**（合并后必跑）：判据 = 某行在 `ours` 有、`base` 没有（fork 独有），
   却在 `merged` 里找不到 ⇒ 丢失候选。本次靠它捞出了 `usRateTone`/`colspan` 两处。
   脚本口径：`git show {ours,base,merged}:<file>` 做行集合差，逐文件报告。

### F. 验证（本次收尾）
```
node --check internal/panel/app.js           语法 OK
go build ./...   通过
go vet   ./...   通过
gofmt -l 全树 46 == 合并前 0c91110 的 46（零新增脏文件）
go test -count=1 -timeout 900s ./...（-u 1001:1001，干净树）  22 包全绿，TEST_EXIT=0
```

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
- ⚠️ **别用 `git checkout .` / `git stash` / `git reset --hard` 回退**。2026-09-18 曾发现工作树领先 HEAD 63 个文件（含生产文件），已提交让 **HEAD == 工作树**（`f204e68`）。改前先 `git status --short`，回退用 `git revert`/逐文件恢复。
  ℹ️ **2026-09-28 起「部署真相」已不是工作树**（容器改跑 GHCR 镜像，见 §7）；但**源码真相仍是本仓 HEAD**，
  而 HEAD 只有 `push` 出去才有异地备份 —— **提交后务必 `git push fork HEAD:main`**（2026-09-28 之前曾有 16 条提交只在 NAS 单盘上）。
- ⛔ **别把 `ErrEdgeAuth`（401 + 无业务信封）退回 `ErrClient`**，也别给它加任何账号级惩罚
  （冷却/熔断/NoteFailures）——2026-09-22 全池降权事故的根因就是它落 `ErrClient` 后喂了连败计数
  （5 分钟边缘层抖动 → 33 个健康号一起降权 10 分钟 → 池子打空 → 连环 503）。
  止打职责归 `edgeGate`（IP 级状态机），不归账号惩罚。详见 §5.5。
- ⛔ **别把 `internal/server/edgegate.go` 退回 `wafip.go` 的 403 专用形态**（也别把 `noteEdgeAuth` 拆成独立状态机）
  —— 401 与 403 共用同一判定窗是刻意的：两者都是「出口 IP 被边缘层拒绝」，混排要能更快触发。
- ⚠️ **`contentSafetyRule` 的判据（`safety review` / `内容未通过`）不是可靠分野**（2026-09-29 实测证伪，
  见 §5.8 B）：上游对**账号级拒绝**也回同一句文案。规则本身保留（用于给出 `content_blocked` 这个
  机器可读 code），但**它只决定"叫什么名字"，不决定"罚不罚"**——罚与不罚由行为证据决定。
  ⛔ 别再基于该文案做「不轮转 / 不罚号」的判断。
- ⛔ **别把 `ErrContentBlocked` 改回「立即 400、不轮转」**（2026-09-29 之前的行为）——
  那会让「账号级被拒」变成客户端可见的 400，并让被拦号零惩罚地留在池里被越选越多
  （实测 44% 的 global 流量落在 2 个死号上 + 客户端重试风暴）。判定必须靠**换号结果**，见 §5.8 C。
- ⛔ **别在「整轮都被拦」时罚账号**——那才是内容问题（换任何号都一样）。
  `NoteContentBlockEvidence` **只能在「后续有号成功」之后调用**（`handler.go` 成功分支），
  否则就退回 §5.7 的误禁老路。
- ⛔ **别删 `NoteSuccess` / `ReviveDisabled` 里对 `contentBlockFails` 的清零**——
  不清会让历史证据跨成功累积，最终误禁健康号。
- ⚠️ **`prompt.mode` 决定客户端 system prompt 的命运**（`handler.go`）：`custom` = **删除**客户端
  system 换成网关提示词；`append` = 保留客户端 system 再插一条；`passthrough` = 原样透传。
  **生产用 `custom`**（防客户端指纹被上游逐字误杀，见 `internal/prompt/prompt.go` 包注释），
  代价是**任何依赖自己 system prompt 的客户端（schema / 工具定义）都会失效**——
  2026-09-29 在 DSH 会话里实测到「模型改说散文、第三人称谈 YG、不认输出格式」。
  另注：`internal/prompt/defaultprompt.md` 曾在本仓被换成**本工作区那份 300 行人格提示词**，
  2026-09-29 已恢复为上游的 38 行版（见 §5.9 B）。
- ⚠️ **`NewHandler` 缺省 `PromptMode="passthrough"`，生产配置是 `prompt.mode="custom"`** ——
  两者对 `ErrContentBlocked` 的行为不同（前者先降级重试一次，后者直接 400）。
  写相关测试时**显式指定 PromptMode**，别依赖缺省值（见 §5.7 E）。
- ⛔ **别在同步上游 `internal/upstream/client.go` 时整文件覆盖**——本仓在该文件有 `ErrEdgeAuth` /
  `IsEdgeAuth` / `Classify` 第 11 层三处自研改动（上游没有，见 §5.5 C 节核查结论）。
  ⚠️ 且该文件与面板上游**结构性分叉**（本仓 `errorRule/matchMode` vs 面板 `xxxMarkers`）
  → **逐文件三方合并也不适用**，必须**手工移植**（见 §5.6 C 节）。先判断改动是否自包含，再整块搬。
- ⛔ **别在结构分叉的文件上迷信「三方合并成功」**——`git merge-file` 返回 0（无冲突）
  只说明文本能拼上，不说明语义正确。分诊前先跑一次顶层符号互包含性检查：
  `comm -23 <(ours 符号) <(theirs 符号)` 双向非空 ⇒ 结构分叉 ⇒ 走手工移植。
- ⛔ **别再 fetch `/vol4/_upstream_wb2api`**（2026-09-25 用户决定：只跟面板上游）。
  原根上游 `Sliverkiss/workbuddy2api` 已删库；延续仓库是 `HanawaBanana/workbuddy2api`，
  **仅作历史留档**，不纳入日常同步。见 §5.6 A。
- ⛔ **别把 `internal/panel/app.js` 的 `reattachQueueView` 换回 `pollQueueOnce`**——
  上游已废弃后者（`c564e90`/`410309c` 就是在修它的残留调用点导致的 JS 崩溃）。
  本仓那一行是 `loadSchoolStatus(true); loadCNInvite(true); reattachQueueView();`（两个改动并存）。
- ⚠️ **`-race` 在本机跑不了**（无 gcc + 网络受限），验证标准是 `build+vet+test`（§3.1 E 步）。
  别因为「没跑 -race」就认为验证不完整——本仓历史从未跑过。
- ⛔ **别为了本机某个客户端的现象去改 `internal/upstream/thinking.go`**（`injectThinking` / `backfillReasoningContent`）—— 那是上游面向**全部客户端**的契约：**issue #43** 的验收项就是「无 effort 裸请求也开思考」（非它则只发 `thinking` 的客户端拿不到思维链）；**issue #157** 维护者结论是「**客户端配置问题，非网关缺陷**」；**issue #91** 明确 `reasoning_content` 是**要被传递出去**的字段。
  2026-09-18 曾偏离两处（① 不注入 thinking ② 不回放历史 reasoning），**A/B/C 同参数多组对照证明收益不成立**（不注入 vs 注入都退化），**已于 `94b2aee` 全部回滚**，5 个文件与两个上游逐字节一致。
  → 若再遇到「卡循环 / 反复 `finish_reason=length` 空正文」，先走**客户端侧**（`maxInputTokens` 压缩点、`reasoning_effort` 档位、`max_tokens` 预算），别动网关。详见技能 `workbuddy-compact-threshold` §九。
- ⛔ **别在 NAS 上跑 `docker compose build`**（会卡 `apk add`，见 §7 A）。构建在 GitHub Actions 上，
  NAS 只 `pull`。**也别让 NAS `git pull` 源码**——它是源头，pull 会冲掉本地提交；「从 Git 部署」= 拉镜像。
- ⛔ **别把 `docker-compose.yml` 的 `image:` 退回 `build: .`** —— 那会同时失去「构建可复现」和「源码异地备份」两件事。
- ⛔ **别删 `.github/workflows/build-image.yml`**，也别把它的 `permissions.packages` 从 `write` 降下来
  （降了 CI 推不进 GHCR，而 `gho_` token 没有 `write:packages` 可兜底）。
- ⚠️ **提交后一定 `git push fork HEAD:main`**。NAS 是单盘，`fork/main` 才是异地备份。
  2026-09-28 之前曾积累 **16 条提交只在 NAS 上**（含两个自研修复）。

---

## 7. 构建与部署（2026-09-28 起：GitHub 构建 + GHCR 拉取）

### A. 为什么改

```
RUN apk add --no-cache wget ca-certificates tzdata python3 bash   → exit 5
dl-cdn.alpinelinux.org 从 NAS 不可达（APKINDEX 拉取挂死）
```

此前几次 `docker compose build` 能过，只是因为该层还在**构建缓存**里；2026-09-28 缓存失效后暴露。
（试过换 `mirrors.tuna.tsinghua.edu.cn`、拉 `golang:1.23` Debian 版——都被 NAS 的网络策略挡住。）

同时暴露第二个问题：**源码只存在于 NAS 单盘**。`fork/main` 停在 2026-09-18，
本地 HEAD 领先 **16 条提交**（含两次自研修复 `e0e1a35` / `816e308`）——盘挂了就全没了。

两件事一个方案解决：**runner 有外网，NAS 只拉产物。**

### B. 拓扑

```
NAS 工作树 ──git push fork HEAD:main──▶ yjzsg/workbuddy2api-panel (GitHub)
                                              │ 触发
                                              ▼
                              .github/workflows/build-image.yml
                              （ubuntu-latest；GITHUB_TOKEN；gha 缓存）
                                              │ push
                                              ▼
                          ghcr.io/yjzsg/workbuddy2api-panel:latest
                                     + :sha-<短sha>（不可变）
                                              │ docker compose pull
                                              ▼
                                          NAS 容器
```

⚠️ **NAS 是「源头」，不要让它 pull 源码。** 「从 Git 拉取部署」指的是**拉镜像**，不是 `git pull`。
NAS 上 `git pull` 有冲掉本地提交的风险（§6 那条 `checkout .` 事故同源）。

### C. 凭据

| 用途 | 凭据 | 位置 |
|---|---|---|
| NAS → GitHub 推送 | `gho_…`（`yjzsg`，scopes `gist, repo, workflow`） | NAS `~/.git-credentials`（perm 600）+ `git config --global credential.helper store` |
| CI → GHCR 推送 | **内置 `GITHUB_TOKEN`**（workflow 里 `permissions: packages: write`） | 不需要任何 PAT |
| NAS → GHCR 拉取 | **无需凭据** | 包是 public（仓库 public，GHCR 继承可见性） |

- 该 `gho_` token **没有** `read:packages`/`write:packages` → **改不了 GHCR 包可见性**（API 报 403）。
  但匿名拉取实测可用，所以不需要。**别为了这个去换 token。**
- 本地机器上同名 token 可从 Git Credential Manager 取：`printf 'protocol=https\nhost=github.com\n\n' | git credential fill`。

### D. 日常操作

```bash
# 提交并部署（在 NAS 上）
cd ~/docker/workbuddy2api-panel
git add -A internal cmd && git commit -m "..."
git push fork HEAD:main                    # 触发 CI
# 等 run 变绿（约 1–2 分钟）：
#   curl -s -H "Authorization: token $TOK" \
#     https://api.github.com/repos/yjzsg/workbuddy2api-panel/actions/runs?per_page=1
docker compose pull && docker compose up -d --force-recreate
curl -s --noproxy '*' http://127.0.0.1:7863/healthz
```

`push` 会**无条件**触发构建（`paths-ignore` 只排除 `**.md`/`docs/**`）。改 `docker-compose.yml` 也会触发，
但 compose 文件不进镜像，重建出来的镜像内容一致——无害。

### E. 回滚

**首选：切不可变 sha tag**（每个提交一个，永不覆盖）

```bash
cd ~/docker/workbuddy2api-panel
sed -i 's#\(ghcr.io/yjzsg/workbuddy2api-panel:\).*#\1sha-<短sha>#' docker-compose.yml
docker compose up -d --force-recreate
```

**次选：切本地留档镜像**（部署前一定先 tag）

```bash
docker tag wb2api-rollback-20260928b ghcr.io/yjzsg/workbuddy2api-panel:latest
docker compose up -d --force-recreate        # ⚠️ 别加 pull，会覆盖回去
```

本地留档镜像：`wb2api-rollback-20260928b`（= `0b4c44b7fed3`，注入式构建）、
`wb2api-rollback-20260928` / `-20260925` / `-20260923` / `-20260922` / `-20260921`。

### F. 坑

- ⛔⛔ **别给 workflow 加 `paths-ignore` 按扩展名忽略**（2026-09-29 踩过）。
  曾写 `paths-ignore: ['**.md', 'docs/**']`，结果改 `internal/prompt/defaultprompt.md`
  （**`go:embed` 的构建输入**）时**构建被静默跳过** —— CI 没跑 → `docker compose pull`
  拿到的是**旧镜像** → **以为部署了新代码，其实跑的还是旧的**（tag 是 `latest`，无从察觉）。
  **本仓构建输入不止 `.go`**，`go:embed` 还吃：
  `internal/panel/index.html`、`internal/panel/app.js`、`internal/prompt/defaultprompt.md`、
  `internal/upstream/model.json`。按扩展名忽略天然不可靠 → **已移除 `paths-ignore`**，
  文档提交多跑一次构建（约 1 分钟）换「绝不静默漏构建」。
- ⛔ **部署前先确认 CI 真的跑了**（防上面那种静默跳过）：
  ```bash
  # 最新 run 的 head_sha 必须 == 本仓 HEAD，且 conclusion=success
  curl -s -H "Authorization: token $TOK" \
    "https://api.github.com/repos/yjzsg/workbuddy2api-panel/actions/runs?per_page=1"
  # 并确认不可变 tag 存在（每个提交一个）：
  docker pull ghcr.io/yjzsg/workbuddy2api-panel:sha-$(git rev-parse --short HEAD)
  ```
- ⛔ **别在 NAS 上恢复 `build: .` 后直接 build** —— 会卡 `apk add`。要用本地构建，先把
  `Dockerfile` 的 `FROM alpine:3.20` 换成 NAS 可达的基础镜像，或把 alpine 源换掉。
- ⚠️ **`docker compose up -d` 不加 `--force-recreate` 时**，若镜像 ID 变了 compose 会重建；
  但若只是 tag 指向变了而 ID 相同，不会重建 —— 回滚时统一加 `--force-recreate` 省心。
- ⚠️ **CI 构建用 `GOPROXY=https://proxy.golang.org,direct`**（workflow 里 `build-args` 覆盖 Dockerfile 默认的 `goproxy.cn`）。
  runner 在境外，官方代理更稳。**改回 `goproxy.cn` 不会立刻出问题，但没必要。**
- ⚠️ **镜像 digest 可用于核对**：`docker inspect -f '{{index .RepoDigests 0}}' ghcr.io/yjzsg/workbuddy2api-panel:latest`。
  workflow 的 Summary 里也打印 digest。
- ✅ **验证新镜像内容**（中文 pattern 用 `grep -a`，`strings` 会滤掉非 ASCII 导致假阴性）：
  ```bash
  docker run --rm --entrypoint /bin/sh ghcr.io/yjzsg/workbuddy2api-panel:latest \
    -c 'for p in "safety review" "内容未通过" "edge_auth_rejected"; do printf "%-22s %s\n" "$p" "$(grep -ac "$p" /app/wb2api)"; done'
  ```
