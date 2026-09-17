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

### internal/upstream
6. `client.go` — 追加 `webBase(a)` + `WebBaseCN` 可覆盖字段（默认 `https://www.workbuddy.cn`）；追加 `CreditPackage` 类型 + `CreditPackages(a)`（面板"积分构成"）；`ModelInfo` 加 `CanDisableThinking` 字段（含 `dynModelEntry` 解析 + `modelInfo()` 映射）
7. `client_token.go` — 面板层抽取文件（`clientToken()`，school.go 依赖）

### internal/scheduler
8. `scheduler.go` — 结构体加热配置字段（`schedMu`/`rearmSchedule`/`rearmBalance`/`balanceInterval`）；`New` 初始化 rearm channel；`nextWake` 在 schedMu 下快照；`Run` 响应 rearmSchedule；`dispatch` 的 school/cat 改接 Go 路线（`RunSchoolAllNow`/`RunBlackcatNow`）；追加 `Reconfigure`/`poke`/`StartBalanceRefresh`/`SetBalanceInterval`/`RunBalanceRefreshNow`
9. `school_api.go` — 面板层新文件（Go 版开学季闭环：`RunSchoolAllNow`/`RunSchoolAccountNow` + 助手）

### internal/server
10. `handler.go` — Config 加 `Panel`/`Live`/`Usage` 字段；追加 `loadLive()`/`softCooldown()`/`SetMaxBodyBytes`；Handler 加 `maxBodyBytes atomic.Int64`；`NewHandler` 镜像上限 + 挂载 `/panel/`；`withAuth` 走 `httpauth.VerifyBearer`；`modelList` 加静态兜底（`staticModels` + `upstream.GlobalModelNames`）与 `can_disable_thinking`；`chatCompletions` 加 `recordAttempt`（5 处调用点）+ `attemptStarted` + 413 文案；`applyErrorPolicy` 软冷却基数改 `h.softCooldown()`
11. `logging.go` — 加 `chatLogOut`/`SetChatLogOutput`（默认 `os.Stdout`）；`chatStatsReader` 扩展为并集（分字段 token + `Usage()`，保留上游 `Credit()`/`Tokens()`/`TotalTokens()`）；`parseSSELine` 并集解析；加 `usageDeltaFromResponse`

### cmd/server（本仓库主导的装配层）
12. `config.go` — 内联 `Schedule` 结构（面板词汇 `blackcat_hours` 等）+ `school_*`/`activity_report_count`/`max_in_flight_global`/`degrade_*` 键；`WriteDefault`/`ParseConfigInto`/`ParseConfig`；`validateScheduleHours`
13. `main.go` — `appVersion`/`usagePathFor`/`stateSibling`/`modelJSONPath`；livecfg/usage/panel 装配；`saveConfig`（含 `SetDegrade`/`SetMaxInFlightGlobal`/新签名 `Reconfigure`）；`StartBalanceRefresh`；`scheduler.Config` 字段映射（`CatHours←blackcat_hours`、`SchoolHours`、`ActivityReportCount`）；恢复上游的 `SetModelCatalogPath`/`store.Close`

### 测试适配点（上游更新后需重新适配）
- `cmd/server/config_test.go` — 词汇 `Cat*` → `Blackcat*`（默认 `[23]`）
- `internal/server/logging_test.go` — `captureStdout` 需同步调 `SetChatLogOutput`
- `internal/server/{handler,handler_effort_models,handler_global_models,handler_models_name,handler_global}_test.go` — 8 个 modelList 测试按"静态兜底"断言
- `internal/scheduler/school_test.go` — dispatch 测试按 Go 路线断言

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

```bash
# A) 根上游新增提交 → 净 diff 直接打（先 --check，失败再手工解）
git -C /vol4/_upstream_wb2api fetch origin
git -C /vol4/_upstream_wb2api diff <上次已合的上游 SHA>..origin/master > /tmp/up.patch
cd <tree> && git apply --check -p1 /tmp/up.patch && git apply -p1 /tmp/up.patch

# B) 面板仓库新增提交 → 只合「面板层独有」文件，其余不反向合
#    每个文件三方合并：git merge-file -p <ours> <base=面板仓库上次已合的 SHA> <theirs=origin/main>
git -C <家目录>/docker/workbuddy2api-panel cat-file -p origin/main:internal/panel/app.js > /tmp/theirs
```

**判据（关键）**：上游派生文件（`pool/`、`server/`、`upstream/`、`session/`、`cmd/*`）**永远以我们的树为准**——
我们的树取自更新的上游；只有 `internal/panel/*`、`config.example.json`、`cmd/server/main.go` 的 `appVersion`
这类**面板层文件**需要合。参考实现：`fix_iter8.py`。

```bash
# C) 增量落地只需 internal/ + cmd/（+ 有变动时的 config.example.json）
rsync -a --delete <tree>/internal/ internal/ && rsync -a --delete <tree>/cmd/ cmd/
docker compose build && docker compose up -d
```

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

## 6. 禁止事项

- ⛔ 别用上游 `Dockerfile`/`docker-compose.yml`/`config.example.json` 覆盖（L0 补丁：镜像站 401 绕行、entrypoint 指向 `/app/data/config.json`、PUID/PGID）
- ⛔ 别把 `blackcat_hours` 改成 `cat_hours`（生产配置在用）
- ⛔ 别 `git clean -fd`（会删掉未跟踪的 `activate_friend.py`/`bind_invite.sh` 等）
- ⛔ 别用上游 `scripts/` 覆盖本仓库 `scripts/`（面板脚本会丢）
- ⚠️ 上游带 Revert 历史的改动不要跟着搬（先看 `git log`）
- ✅ 日期敏感测试的 TZ 坑已由上游 **#130** 修掉（测试桩改 CST 自然日口径）→ **2026-09-17 起不再需要 `TZ=Asia/Shanghai`**
- ⛔ 别把 `internal/panel/*`（app.js/index.html/panel.go）退回旧版——面板仓库 1.10.0 的 UI 增量（连败降权显示、成本台账 tooltip、模型能力徽标）必须保留
