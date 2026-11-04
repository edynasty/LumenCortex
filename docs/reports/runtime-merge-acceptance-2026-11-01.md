# Runtime 升级最终集成验收与本地合并方案（2026-11-01）

状态：验收完成，等待 Owner 授权 B1 本地合并。最终测试 SHA = `8c87f45`（`8c87f45598f52bb6cd5696a114c79836d5dffaef`，rt 上全部整改合入后的 HEAD）。`c36bd29..8c87f45` 只改了一个测试文件（`internal/diag/query_test.go`，§5），所以只依赖产品代码的检查（工具面、Browser 生命周期、Schema 兼容、`apidiff`）在 `c36bd29` 上的结果对 `8c87f45` 同样成立；Core 全量测试、无模型 E2E 与 Desktop 验证在 `8c87f45` 上重跑。本报告与证据随后以一个仅含文档的提交加入 rt。

范围：Owner 2026-11-01 08:22 指令的五项最终验收——Core 合并边界、工具暴露、Browser/MCP 生命周期、Desktop 验收、最终验证——以及据此给出的本地合并建议。不设计、不运行付费 Benchmark；不合并 `main`、不 push、不打 tag、不发布。

证据目录：`docs/reports/evidence/runtime-merge-acceptance-20261101/`（下文简写为 `E/`）。其中的 Go 源码（驱动、探针）以 `.go.txt` 保存，避免进入 Core 模块的 `go build/vet/test ./...`；`E/mcp-lifecycle/driver/` 带独立 `go.mod`，本身就不属于 Core 模块。

## 1. 提交关系（Core）

| 引用 | SHA | 说明 |
|---|---|---|
| `main` = `origin/main` | `3e1c3cc` | T0-S1 合并（`cog/r0@c08f5c4` → `v0.1.4`=`42ee234`）；已推送，未打 tag |
| `cog/r1a` | `15bf4eb` | G-R1a=PASS 仅对此 SHA 有效（`G_R1A_REREVIEW_20261031B.md`） |
| `rt/execution-runtime` | `8c87f45` | 以 `cog/r1a` 为基；本次验收的待合并分支（不含其后的报告提交） |

- `main..cog/r1a` = 82 个提交；`cog/r1a..rt` = 95 个（审查基线 `15f9039` 时 57；`5ccc350` 加 1 个测试提交；整改加 28 个非合并提交与 9 个 `--no-ff` 合并提交，见 §5）；`main..rt` = 177；`rt..main` = 1（`3e1c3cc` 合并提交本身，其两个父提交 `42ee234`、`c08f5c4` 都在 `cog/r1a` 中）。
- `git merge-tree --write-tree main rt`：exit 0，合并树 = rt 树（`8c87f45` 时 `631d5a0`；`15f9039` 时 `f030136`）。因此 B1 本地合并在内容上等于把 `main` 换成 rt 树；**无冲突不等于安全**，安全性由 §2–§6 的行为盘点、独立审查和测试证明。
- `git diff --shortstat main rt`（`8c87f45`）：508 文件，+136,936 / −1,256；整改本身（`5ccc350..8c87f45`）61 文件，+3,498 / −368。其中 175 个文件、+78,644 行是 `benchmarks/tb2/` 下的评测记录与原始公开数据（4.65 MB）；密钥模式扫描无命中；其中 7 个 Markdown 文件出现本机内网地址与自有域名（`192.168.65.x` Docker 网关、`ai.edynasty.asia` 网关、`edynasty.asia:38082` 镜像源）。`edynasty/LumenCortex` 是**公开仓库**：push 前由 Owner 决定是否保留这些记录（见 §9）。

## 2. 本地合并会引入的行为

### 2.1 来自 `cog/r1a`（82 个提交，G-R1a=PASS @ `15bf4eb`）

| 方面 | 变化 | 位置 |
|---|---|---|
| Schema v6（0006） | 新表 `request_envelopes`、`derived_origins`、`source_fences`、`check_approvals`；8 个索引；`eval_results` 增 6 列；不回填；有 down 文件（行数据丢失，先备份） | `internal/migrate/sql/0006_provenance_and_approvals.sql`、`sql/down/0006_*.down.sql` |
| **既有用户的策略变化** | 已有 `checks` 配置一律视为**未批准**：required check 判为 `spec_unapproved` 且不执行，非交互 run 以 `failed/spec_unapproved` 结束；批准需 `lcx-go checks approve <set-digest>` 或宿主 API（ADR-0017） | `runtime/checks.go`、`internal/agent/loop.go`、`internal/evaluator/run.go` |
| 审批覆盖脚本 | 批准覆盖 check 命令执行的工作区文件（CHECKS-002） | `internal/evaluator` `ResolveInputs` |
| Journal | 改为白名单：不再写工具参数/输出、目标、最终文本、shell 命令 | `internal/session/store.go` |
| Provenance | 每次 provider 尝试一个 `AdmitRequest`，每个有副作用工具一个 `AdmitExecution`，均检查 fence | `internal/agent/envelope.go`、`execution.go` |
| 隐私删除 | `DeleteSession` 级联、撤回与 fence（T3-S4） | `runtime/deletion.go`、`runtime/revoke.go` |
| 认知（Cognition） | 普通 run **不调用**；仅 `lcx-go governor plan`；curator 在任何 provider 调用前失败关闭；无新配置键/环境变量（O-5 未批准的产品启用不受合并影响） | `runtime/governor.go` |
| Provider/Loop 修正 | LOOP-001、PROVIDER-005、PROVIDER-007（截断/断流/拒答不再完成或执行工具） | 见 TODO 对应条目 |
| 新 CLI | `lcx-go checks status|approve` | `cmd/lcx-go/checks.go` |

### 2.2 来自 `rt/execution-runtime`（`cog/r1a..rt`）

| 方面 | 变化 |
|---|---|
| Schema v7–v9 | `jobs`（0007）、`scheduled_tasks`（0008）、`notebooks`+`notebook_revisions`（0009）；`Latest()`=9；均有 down 文件 |
| 打开时自动执行 | 迁移（先 `VACUUM INTO` 备份）；`reconcileRunning`；Job 管理器恢复（死亡所有者的 running → `outcome_unknown`，永不重跑；其 queued 被接管）与保留期清理；调度器启动（表为空时无事可做）；诊断日志打开（失败退回丢弃型 logger，不阻止 Open） |
| 工具 | `shell`→`bash`（旧名在宿主列表、workflow `allowedTools`/条件中按映射生效；模型调用 `shell` 得到改名提示）；删除 `git_status`/`git_diff` Agent 工具（宿主 Git API 不变）；新增 `job_*`、`notebook_*`、`wakeup_*`；bash `mode:"background"`；LSP 工具按需启动（整改后受信任约束，见 §5） |
| 配置（仅全局文件） | `execution.{shellTimeoutSeconds,maxConcurrentJobs,maxQueuedJobs,jobOutputBytes,jobRetentionDays,logLevel,budget}`、`lsp.*`、模型 `cost` |
| CLI | `LCX_DEADLINE`、`LCX_DEADLINE_RESERVE`；`lcx-go plugins …` |
| MCP 模板 | 仅数据，应用后为 disabled；Open 时不启动任何服务 |
| 诊断日志 | `<state>/logs/lumencortex*.jsonl`，8 MiB × 5，保留 14 天，脱敏 |
| **公开 Go API 不兼容** | `apidiff`（`main`→rt，包 `runtime`/`protocol`/`pluginapi`/`config`，在 `5ccc350` 与最终 `c36bd29` 各跑一次，结果相同）：唯一不兼容项 `(*Engine).LSPStatus` 返回值由 `LSPStatus` 改为 `[]LSPServerStatus`。整改只新增兼容项（如 `ErrNotebookWithdrawn`、`ThreadView`/`TaskResult`/`SubagentNode` 的 `Withdrawn`）。Desktop `main`（pin `v0.1.4`）不受影响；Desktop rt 分支已适配（`d58ed13`） |

证据：`E/apidiff.txt`（`5ccc350`）、`E/apidiff-final.txt`（`c36bd29`）；行为盘点逐项文件行号见合并边界清单（本报告 §2 的来源，`E/merge-boundary.md`）。

### 2.3 B1、数据库回退、发布与 Tag 的区分

| 类别 | 内容 | 是否阻塞本地合并 |
|---|---|---|
| **B1 本地合并**（授权项） | 把 rt 合入本地 Core `main`。工程条件：无冲突、§5 审查问题已整改并复验、§8 测试通过 | 仅需 Owner 授权 |
| 数据库回退保障（B2 / G0b / O-2） | 现有手段：每次升级/降级前自动备份；`lcx-go db downgrade --to N` 可降到 5（`main` 可再打开，§6 实测）。缺：T0-S2 旧库迁移/反向迁移、备份保留期（O-2 未决，备份不清理） | 不阻塞本地合并；阻塞正式发布（或由 Owner 书面豁免 G0b） |
| 正式发布（B3/B4/B6/B7） | 版本常量仍为 `0.1.4-preview`（`runtime/engine.go:40`），`release-check.sh --mode release` 需要 `v<version>` tag；版本提交产生新 SHA，Gate 需在新 SHA 复核；Desktop T7-S1 审批 UI；Desktop 未推送提交 | 不阻塞本地合并 |
| Tag / Push（B5、O-6） | Core tag 与 push `main`，Desktop 再 pin 已发布 tag | 不阻塞本地合并；需 Owner 另行授权 |

## 3. 工具暴露（实测）

方法：真实 Engine + `RunSupervisor.Start`（宿主路径），本地 OpenAI 兼容模拟 Provider 原样记录请求体；不发起任何模型调用。Token 用 `deepseek-ai/DeepSeek-V4.1-Flash-1017` @ `1bae0e10…` 的官方 tokenizer（`tokenizer.json` sha256 `c90dfa01…476b`）与官方 `encoding/encoding.py`（复现其测试输出后使用）。`tok(JSON)` = 线上 `tools` JSON 字符串的 token 数；`tok(rendered)` = 同一请求带/不带工具的渲染 prompt token 差。只下载了 tokenizer/encoding/config，未下载模型权重。Core `006b031`（工具面与 `15f9039`/`5ccc350` 相同）；Browser 行为 `5ccc350`。

**29 个内建工具不会全部暴露给普通 Agent。** 默认策略是 read-only：

| Profile | 工具数 | wire 字节 | tok(JSON) | tok(rendered) |
|---|---:|---:|---:|---:|
| 普通任务（默认 read-only，agent `agent`） | 19 | 9,997 | 2,218 | 2,618 |
| 只读 Agent（`ask`，read-only） | 19 | 9,997 | 2,218 | 2,618 |
| 编辑（`workspace`） | 24 | 12,498 | 2,820 | 3,287 |
| Coding Agent（`full`） | 29 | 15,131 | 3,423 | 3,923 |
| 复杂任务（`full` + `isolated` 委派） | 30 | 16,059 | 3,628 | 4,137 |
| Subagent 子 run | 16 | 8,728 | 1,943 | 2,327 |
| Execution task（worker） | 19 | 10,053 | 2,258 | 2,686 |
| 复杂任务 + LSP（整改前，受信任工作区） | 37 | 18,536 | 4,199 | 4,774 |
| 普通任务 + Browser 模板 | 26（Browser 7/15） | 15,643 | 3,528 | 4,038 |
| Coding Agent + Browser 模板 | 44（Browser 15/15） | 26,048 | 5,965 | 6,709 |

- 集合：read-only 19 = 文件读取/搜索 4 + `read_skill` + 线程 2 + subagent 3 + notebook 3 + work unit 3 + reference/note 3；`workspace` 加写文件 3 + `wakeup_*` 2；`full` 加 `bash` + `job_*` 4；复杂任务加 `start_task`。Agent 身份（`ask` vs `agent`）在同一授权下不改变工具集。
- Browser 在 read-only 下只出现 6 个模板标为 `read` 的工具和 server 标注 `readOnlyHint` 的 `browser_wait_for`；8 个 exec 工具被策略挡掉。
- **按需暴露（现有机制即可，不改 Registry、不改名）**：宿主 `ToolAllowlist`/`ToolDenylist`、workflow `allowedTools`、MCP `include`。实测：只读研究 allowlist 4 个工具 → 1,154 B / 503 tok（比默认 −80.8% token）；`full` 去掉 job/wakeup/notebook/线程/委派 15 个辅助工具 → 7,593 B / 2,106 tok（−46.3%）；MCP `include` 5→2 → −613 B / −153 tok。
- 现有机制**做不到**（不在本次范围，记录事实）：agent 定义没有工具列表字段；拒绝 `job_*` 后 bash 描述仍提 `mode:"background"`；`ToolSchemaCost` 固定按 full 策略计价，与非 full run 不一致（TODO `CAP-001`）；MCP 首次发现前 `EffectivePrompt` 少报（实测报 30、发 35）。
- 子 run 的 prompt 报告比实发少 `recall`/`read_thread` 两个工具（已存在的 TODO `PROMPT-001`，本次实测复现：子 run 报 14 发 16，task 报 17 发 19）。

证据：`E/tool-exposure/`（驱动源码、`summary.json`、`tokens.json`、`browser.json`、原始请求文件的 SHA-256）。

**最终 SHA 复测**：同一驱动在 `bd0160a` 与 `c36bd29` 重跑 base（7 个 profile）、LSP、MCP、MCP `include`、Browser 模板五个场景共 14 个首请求，`tools` JSON 与上表测量时逐字节相同（SHA-256 一致），工具数与字节数不变；token 数因此不变，未重算。整改没有改变任何 profile 的工具面。证据：`E/tool-exposure/recheck-final.txt`。

## 4. Browser / MCP 生命周期（实测，`15f9039`）

环境：@playwright/mcp 0.0.82、Google Chrome 156（headless），按出厂模板 `browser.json`（`idleTimeoutSeconds: 600`）应用并启用；本地模拟 Provider。

| 检查 | 结果 |
|---|---|
| **修复前** `Engine.Close` 带运行中服务 | Chromium 遗留（主进程 ppid=1，4–8 个进程 / 约 550 MB）：Close 立即 SIGKILL server，Playwright 没机会关浏览器 |
| 修复 `15f9039` | server 独立进程组；Close 先关 stdin、最多等 2 s 让 server 自行退出（Playwright 此时关闭浏览器），再信号 + 杀组；server 自行退出时也杀组；新增 2 个进程组测试 |
| 默认空闲（600 s） | 301 s 时 Playwright 自身空闲关闭 Chromium；**600.0 s** Core 以 `reason=idle` 停止 server；之后 0 进程、RSS 0 |
| 禁用 → 重新启用 | 旧 pid 与 7 个 Chromium 全部退出；新 pid 启动；Close 后无残留 |
| 进行中的调用被取消 | run `interrupted`；Close 后 0 进程 |
| 修复后 `Engine.Close` | 0 进程、RSS 0 |
| **外部 SIGKILL server** | **PARTIAL**：Playwright 把 Chromium 放在自己的进程组，server 被杀后 5 个进程 / 约 578 MB 存活（30 s 未自退），Core 无法触达；下一次调用报告工具错误（不是 loop 失败）并以新 pid 重启。已写入模板 `limitations`；TODO `MCP-001` |
| Computer Use 授权隔离 | 模板默认 disabled，应用时强制 disabled；5 个工具全为 `exec`；read-only run、subagent、execution task 都拿不到；无自动批准开关；CodeBridge 的 `approval_required` 作为工具结果返回且不重试。只 `stat` 了 socket，未连接 CodeBridge |
| Exa / GitHub / CodeGraph | **NOT_RUN**（无凭据 / 未安装；按指令不配置真实凭据） |

证据：`E/mcp-lifecycle/`（`SUMMARY.md`、各模式 `*.jsonl` 时间线、驱动源码）。

**整改后复跑**：`72d613f`（2026-11-02）跑全部六个模式：idle 在 600.0 s 以 `reason=idle` 停止，之后 0 个浏览器进程；close/restart/cancel/policy 自身进程树 Close 后无残留；crash 仍为 PARTIAL（6 个 Chromium 存活 30 s，下一次调用以新 pid 重启，已知 `MCP-001`）。close 模式记录的 1 个“孤儿”是并行运行的 idle 模式浏览器的 `chrome_crashpad_handler`（列在 idle 自己的 `after-run` 中，idle 浏览器停止后消失），不属于 close 的进程树。之后的整改（`bd0160a`、`c36bd29`：目录缓存指纹、re-enable、session 绑定校验与 stop 工具约束）改动了服务停止路径上的 `closeBoundSessions`，因此在最终 `c36bd29` 上重跑 close/restart/cancel/policy（各自串行运行）：四个模式 Close 后均 0 孤儿、server 进程不存活；cancel 在 Close 前的 1 个孤儿是该 run 自己仍在运行的浏览器辅助进程，Close 后为 0。idle（10 分钟）与 crash 未在 `c36bd29` 重跑：Browser 模板没有 session 绑定，这两条路径的整改前后代码只差上述停止回调。证据：`E/mcp-lifecycle/rerun-72d613f/`、`E/mcp-lifecycle/rerun-c36bd29/`。

## 5. 独立审查与整改

`cog/r1a` 之后的 rt 提交此前只有作者自测（`go test`、race、无模型 E2E），没有独立审查；而 B1 合并会把它们一起带入 `main`。因此本次在 `15f9039` 上做了三个领域的独立审查（审查者不写被审代码；每条发现须有实际运行的探针），整改由独立的实现 Agent 在各自 worktree 完成，再由集成方合入 rt。

审查范围与方法：exec（前台 deadline、bash 改名、durable jobs、budget、MCP stdio 进程组）、state（调度/wake-up、notebook、诊断日志）、capability（改名与删除、权限门、LSP、MCP 生命周期与目录缓存）。每条发现都附探针（一次性 `zz_probe_*_test.go`，跑完删除）与实际输出；报告原文与保留的探针源码见 `E/review/`。初审结论三个领域均为 **REVISE**。

整改：按代码区域分给四个实现 Agent（jobs、调度/wake-up、notebook/诊断、capability），各用独立 worktree（基线 `5ccc350` = `15f9039` + 1 个测试提交），每条发现一个提交，先写在基线上失败的回归测试再修；集成方逐个 `--no-ff` 合入 rt，合入后由**原审查者**在合并后的 SHA 上重跑原探针并检查修复本身的漏洞。

| ID | 级别 | 问题（审查者探针所见） | 修复提交 | 回归测试 | 复验 |
|---|---|---|---|---|---|
| RT-exec-1 = RT-state-1 | P1 | 保留期清理删掉上一次 job 后，shell 定时任务每次触发都记 `failed`（`session not found`） | `c4ce940` | `TestShellScheduleFiresAfterRetentionDeletedItsLastJob` | FIXED |
| RT-state-2 | P1 | 一次性 wake-up 遇到会话忙被记 `skipped_overlap`，永不再触发 | `49d1b87` | `TestBusyOnceTaskFiresWhenTheSessionIsFree`、`TestWakeupDueDuringItsOwnRunFiresAfterTheRunEnds` | FIXED |
| RT-state-3 | P1 | notebook 读取不看 R1a source fence：撤回子会话后父 Agent 和宿主仍读到正文，读取也不留边、不 fence 读者 | `ce5c4ca` | `TestNotebookReadsOfARevokedChildAreWithdrawn`、`TestNotebookReaderIsFencedWhenItsChildIsRevoked` | FIXED |
| RT-state-4 | P1 | 删除子会话后父 transcript 里 `notebook_read` 的正文副本未被清除 | `504a147` | `TestDeletingAChildRemovesItsNotebookFromTheParentTranscript` | FIXED |
| U1 | P1 | wake-up 丢掉创建它的 run 的工具 allow/deny list（被拒 `bash` 的 run 唤醒后拿到 `bash`） | `dd46e25` | `TestWakeupKeepsTheRunToolFilters`、`TestWakeupToolFiltersSurviveAHostUpdate` | FIXED |
| U2 | P1 | 不受信任工作区里 read-only run 的 `lsp_*` 调用会启动语言服务器，执行仓库自带的 JS（`node_modules/typescript/lib/tsserver.js`） | `1ccca9e` | `TestLSPToolsWithheldInUntrustedWorkspace`、`TestManagerForRunStartsOnlyWhenTrusted` | FIXED |
| U3 | P1 | Agent 改项目 `mcp.json` 的 `env` 后，缓存目录仍被采用，按需启动带上注入的 env | `c5c9bb5` | `TestCatalogFingerprintCoversEnvAndHeaders`、`TestMCPProjectEnvEditWithdrawsCachedCatalog` | env/headers 部分 FIXED；残留见下 |
| RT-exec-2 | P2 | 删除会话不删其 job 输出目录，删除预览也不列出 | `1f6c0a8` | `TestSessionDeletionRemovesItsJobOutput`、`TestAResumedDeletionRemovesItsJobOutput` | FIXED |
| RT-exec-3 | P2 | `CancelRun` 在 job 仍在运行时就返回 | `c3ae121` | `TestCancelRunWaitsForEveryStoppedJob` | FIXED |
| RT-exec-4 | P2 | 子进程持有 stdout 时，退出码 0 的 job 记为 `failed`（`WaitDelay expired`） | `7a04a6e` | `TestJobWhoseChildKeepsStdoutOpenCompletes` | FIXED |
| RT-state-5 | P2 | `Update` 与触发竞争时把已认领的旧时间槽写回，同一时间槽触发两次 | `1b595e9` | `TestUpdateRacingAFiringDoesNotRefire` | FIXED |
| RT-state-6 | P2 | 第二个 Engine 启动时把正在被另一个调度器处理的近期时间槽标为 `missed` 并禁用 | `7764690` | `TestSecondStartLeavesRecentOccurrencesToTheLiveScheduler` | FIXED（残留 P3 见 TODO `SCHED-001`） |
| RT-state-7 | P2 | 夏令时跳变后 cron 跳到第二天 | `2baa40d` | `TestCronSpringForwardResumesAtTheNextValidTime` | FIXED |
| RT-state-8 | P2 | 诊断日志在 INFO 级写入会话目标原文 | `b3c76fe` | `TestSessionGoalIsReducedToItsLengthInTheLog`、`TestContentKeysAreReducedToTheirLength` | FIXED |
| RT-state-9 | P2 | `secret_key`、`passphrase`、`pwd` 等键名未脱敏 | `2f34051` | `TestSecretKeysAreRedactedAtEveryLevel` | FIXED |
| RT-state-19（复验新发现） | P2 | wake-up 在 Agent 并发上限时到期被记 `failed`，永不再触发 | `7f13626` | `TestWakeupDueAtTheAgentLimitFiresWhenASlotFrees` | FIXED |
| U4 | P2 | URL 里 `{env:NAME}` 展开后的凭据出现在错误信息中 | `c5c9bb5` | `TestHTTPClientURLReferenceNeverInErrors` | 传输错误 FIXED；残留见下 |
| U5 | P2 | 空闲停止/禁用/删除不关闭绑定的 Computer Use 会话 | `c5c9bb5` | `TestMCPBoundSessionsCloseOnEveryStop` | FIXED |
| U6 | P2 | 迟到的失败调用关闭了已重启的新客户端 | `c5c9bb5` | `TestMCPLateTransportFailureKeepsRestartedClient` | FIXED |
| U7 | P2 | 空闲计时器无视进行中的调用 | `c5c9bb5` | `TestMCPIdleTimerSparesInFlightCall` | FIXED |
| RT-capability-1 | P2 | `reviewer` 预设仍指示使用已删除的 `git_status`/`git_diff` | `421e12c` | `TestAgentPresetsNameNoRemovedTool` | FIXED |
| RT-exec-9 | P3 | `Registry.Close` 串行等待每个服务 2 s | `59f3887` | `TestRegistryCloseStopsServersTogether` | FIXED |
| RT-state-17 = U8 | P3 | `wakeup_cancel` 能删宿主创建的定时任务 | `b2277cc` | `TestWakeupCancelLeavesHostAgentSchedules` | FIXED |
| U9 | P3 | run 启动时会拉起它根本用不到的 MCP 服务 | `c5c9bb5` | `TestMCPRunSkipsDiscoveryOfAServerItCannotUse` | FIXED（无 include 列表时的限制已写入文档） |
| U10 | P3 | include 外的工具调用不被拒绝 | `c5c9bb5` | `TestMCPCallRefusesExcludedTool` | 拒绝部分 FIXED；残留见下 |
| 集成新发现 | P2 | RT-state-3 让宿主读到撤回会话的 notebook 版本号却看不到正文，`SaveNotebook` 仍接受宿主替换：Desktop 编辑器以空正文打开，保存即覆盖真实内容（撤回子会话也会 fence 读过它的父会话） | `3027535` | `TestSaveNotebookOfAWithdrawnSessionIsRefused`、`TestHostNotebookWriteOfAWithdrawnSessionIsRefused` | FIXED @ `4cc5eb0`（state 审查者；含两进程竞争探针：撤回先提交则拒绝，保存先提交则撤回在其后落地，60 组并发无第三种结果） |

复验结论：exec **PASS_WITH_FINDINGS** @ `de6ccac`（仅余 P3）；state **PASS_WITH_FINDINGS** @ `72d613f`（11 项全部 FIXED）、@ `4cc5eb0`（宿主写入修复 FIXED，仅余 P3）；capability 共复验四轮：@ `ba99d5c`（余 U3 残留 P2、U4/U10 残留 P3）→ @ `bd0160a`（三项残留 FIXED，新发现 1 个 P2、1 个 P3）→ @ `5660720`（两项 FIXED，stop 工具约束引入 1 个 P3 回归）→ @ `c36bd29` **PASS_WITH_FINDINGS**（回归 FIXED，无 P0/P1/P2，余 2 个 P3 转 backlog）。

capability 复验发现与整改（每项先写在合并前 SHA 上失败的回归测试）：

| ID | 级别 | 问题（审查者探针所见） | 修复提交 | 回归测试 | 复验 |
|---|---|---|---|---|---|
| U3 残留 | P2 | 受信任工作区内，`workspace` 策略的 run（无 bash）用 `write_file` 只改项目 `mcp.json` 的 `include` 与 `tools.<name>.permission`，就能把 exec 工具标成 `read`；Engine 重开后缓存目录仍匹配，之后的 read-only run 拿到并按需启动执行它，全程无宿主操作 | `69b7d7f` | `TestMCPProjectExposureEditWithdrawsCachedCatalog`、`TestMCPHostExposureEditKeepsCachedCatalog` | FIXED @ `bd0160a`（含从零新增项目条目、禁用时记录的目录、宿主写入覆盖 run 的编辑、同一次编辑改进程字段时撤回而非迁移；Desktop 的工具开关与启停仍保留缓存） |
| U4 残留 | P3 | 凭据含 userinfo 不允许的字符时，`parseEndpoint` 错误仍带展开后的 URL | `3be622e` | `TestHTTPClientURLReferenceNeverInErrors/{invalid_userinfo,invalid_port}` | FIXED @ `bd0160a`（userinfo/host/port/控制字符/scheme 五种） |
| U10 残留 | P3 | 重新启用名称被规范化的工具（真实名 `browser.navigate`）时写入错误名称并报告成功（`2f969b2` 引入） | `661b380` | `TestSetCapabilityEnabledReenablesSanitizedToolName` | FIXED @ `bd0160a`（从未启动的服务仍按 id 回退，已写入文档） |
| RV2-session | P2 | run 在项目条目里加 `session` 绑定（`stopTool` 指向 exec 工具，include 不变）：`session` 不在目录指纹里，缓存仍被采用；服务停止时 `closeBoundSessions` 直接调用该 stop 工具，绕过 include 与 run 的策略（探针日志：`mutate {"value":"PAYLOAD-from-read-only-run"}`） | `4ab37f3` | `TestMCPProjectExposureEditWithdrawsCachedCatalog/session_binding_with_an_excluded_stop_tool`、`TestCatalogFingerprintCoversEnvAndHeaders` | FIXED @ `5660720`（宿主改绑定迁移目录、run 改绑定撤回目录） |
| RV2-delete | P3 | 宿主删除的服务被 run 原样加回后，`DeleteScope` 留下的记录目录让它复活，read-only run 无宿主操作即可启动 | `2bfb3c9` | `TestMCPDeletedProjectServerCatalogIsNotReused` | FIXED @ `5660720` |
| RV3-stop（`4ab37f3` 引入的回归） | P3 | 宿主自己的绑定若 stop 工具不在 include 中（如在 Desktop 关掉 `computer_stop`），`4ab37f3` 的 stop 约束让关闭时静默跳过 stop，CodeBridge 会话一直打开 | `8fb149a` | `TestMCPHostWriteRefusesBindingOutsideInclude`、`TestSetCapabilityEnabledKeepsBoundSessionTools`、`TestMCPInvalidBindingOnDiskLeavesOtherServersUsable`、`TestBoundSessionStopOutsideIncludeIsReported` | FIXED @ `c36bd29`：start/stop 工具不在非空 include 中的绑定在所有宿主写入时被拒绝（错误指名工具）；磁盘上已有的此类条目不阻止 Engine 打开，但不启动、不提供工具、在 `lastError` 写明原因；兜底跳过时同样写入 `lastError`。Computer Use 模板应用、启用与关闭会话不变 |

- 备注（非本分支引入）：只有工作区包含 `$XDG_CONFIG_HOME`（例如工作区就是 `~`）时，run 才能经 `write_file` 改全局配置；该布局下 run 本来就能改全局 `lumencortex.json`。
- 测试基础设施（仅测试，两项）：
  - `6b654a0`：整改期间 `TestCrossProcessRevocationIsLinearizedWithAdmission` 偶发一次 `lumencortex.json: document is empty or truncated`。原因：每个辅助进程的 `TestMain` 都用 `os.WriteFile`（先截断再写）重写父测试共享的全局配置，辅助进程启动时恰好另一个辅助进程的 Engine 在读。改为只有父测试写。该问题自 `main` 起就存在；未复现到确定性失败，修复依据是代码推理。
  - `8c87f45`：`c36bd29` 的第二次全量测试中 `internal/diag` `TestQueryMemoryIsBoundedByLimit` 失败（`record 0 = 18570, want i=19999`）。诊断队列满时按设计丢弃记录、从不让调用方等待，而该测试把 20000 条记录塞进默认 4096 条的队列，写入方跟不上就丢掉最新的记录。`-cpu 1` 下修复前 10 次失败 3 次，修复后（队列容量设为 20000）10 次全过。产品代码未改。

未修、转入 backlog 的 P3：RT-exec-5/6/7/8/10（`JOBS-001`）、RT-state-10–14（`DIAG-001`）、RT-state-15/16、RT-state-20（`NOTEBOOK-001`）、RT-state-18、RT-state-6r、RT-state-17r（`SCHED-001`）、capability 第四轮的同 id 跨作用域删除与启动中删除（`MCP-002`）。RT-exec-10 的建议修法经实现方反证（捕获 TERM 后以 0 退出的命令会被误报为未超时），审查者接受推迟。RT-state-20：撤回会话上宿主仍可 `CreateNotebook`，随后却读不到也存不了。另有一个产品决定（`NOTEBOOK-001` 的 `OWNER_CHECK`）：撤回 fence 了用户主线程后，宿主失去对其全部 notebook 的读写，包括主线程合法继续后 Agent 新写的。

## 6. SQLite Schema 兼容与迁移风险（实测）

脚本 `E/schema-compat/run.sh`：同一工作区、隔离 `XDG_STATE_HOME`，依次用 `main`（schema 5）、`cog/r1a`（6）、rt（9）三个二进制操作。

| 步 | 结果（`15f9039`；在 `bd0160a` 与 `c36bd29` 重跑，输出除时间戳与随机 id 外相同） |
|---|---|
| 1 `main` 创建状态 | `user_version=5` |
| 2 rt 打开 | 升级到 9，先写备份 `lumencortex.db.v5.<ts>` |
| 3 rt API 写入新表 | jobs/schedules/notebooks 各 1 行 |
| 4 `main`、`cog/r1a` 打开 v9 库 | 均拒绝：`database schema is newer than this binary: have user_version 9, binary knows 5/6`，exit 1；数据库文件前后**字节一致** |
| 5 rt `db downgrade --to 5` | exit 0，先写备份 `lumencortex.db.v9.<ts>` |
| 6 `main` 打开降级后的库 | 成功，会话保留并可继续写 |
| 7 rt 再次升级 | 回到 9；新表为空（降级丢弃其数据） |
| 8 v9 备份 | 仍含降级前的 jobs/schedules/notebooks 行 |

风险：降级会丢弃 v6–v9 新表数据（有备份，但没有自动恢复工具）；备份不清理（O-2 未决）；没有 T0-S2 旧库迁移/反向迁移（G0b）。这些是发布前置，不是本地合并阻塞。

证据：`E/schema-compat/`（`run.sh`、`populate.go.txt`、三个 SHA 的输出日志）。

## 7. Desktop

分支 `rt/execution-runtime-desktop`（worktree `.worktrees/desktop-rt`）@ `a452128`：`main..rt` = 19、`rt..main` = 0，可快进 Desktop `main`（`4a6c2f3`）。详细记录在 Desktop `docs/reports/desktop-runtime-integration-2026-11-01.md`。

| 项 | 结果 |
|---|---|
| Core 引用方式 | 分支的 `go.mod` 是 `replace github.com/edynasty/LumenCortex => ../../wt/core-rt`，指向本工作区的 Core worktree。它只在本机这一目录布局下可构建，**不能随 Desktop 合入 `main`**（CI 与其他检出没有该路径），也不满足发布要求（须 pin 已发布的 Core tag） |
| 无 `replace` 路径验证 | 临时去掉 `replace`，`require` 指向由最终 Core `8c87f45` 生成的伪版本 `v0.1.5-0.20261102065252-8c87f45598f5`（`GOPROXY=direct`，经 `insteadOf` 从本地 Core 仓库解析，未访问网络上的 Core）；`go build ./... && go vet ./...`、`go test -count=1 ./...`、`npm run build`、`npm test`（82 文件 / 455 测试）全部通过；运行后恢复 `go.mod`/`go.sum`。同样的检查在 `15f9039`、`bd0160a`、`c36bd29` 上也通过 |
| Vitest 失败证据（`622793c`） | 2026-11-01 一次全量 Vitest 失败因只保留了截尾输出而无法定位（`TEST-FLAKE-001`）。`npm test` 现在运行 `scripts/test-run.sh`：保存完整控制台日志、`junit.xml`（测试名、失败信息、每个测试的输出）和退出码，失败时另存 `failed-<UTC>/`，CI 失败时上传。此后未再失败（`15f9039` 上连续 5 次 454/454；`bd0160a`、`c36bd29`、`8c87f45` 上各 455/455） |
| 撤回会话的 notebook（`a452128`） | 对应 Core RT-state-3 与 `3027535`：撤回会话的 notebook 显示为“已撤回的笔记本”，不显示正文，无编辑、导出与 Handoff，保留删除；撤回的历史版本同样只显示说明。回归测试在没有组件改动时失败 |
| 能力开关 | Core `c36bd29` 起，关闭绑定会话的 `computer_start`/`computer_stop` 会被拒绝；Desktop 能力页把 `SetCapabilityEnabled` 的错误原样显示（`useCapabilitiesController.ts` `setActionError`） |
| 渲染 UI 检查 | 2026-11-01 在 Core `9dcf95c` 上用 `wails dev` + headless Chromium 完成（Runtime/Jobs/Schedules/Diagnostics/Notebook/能力页、三种宽度、step 18 实时 job 同步）；**未在最终 SHA 上重做**，原生 WebKit 窗口的视觉验收仍属 `UX-001`（`OWNER_CHECK`） |
| 合入前置 | Desktop 合入 `main` 需要：Core `main` 上存在可解析的提交并据此替换 `replace`；B6——Desktop 改用含 R1a 的 Core 前须先有 T7-S1 审批 UI，否则带 required check 的工作区里 run 都会以 `failed/spec_unapproved` 结束（`R1A_RELEASE_PREP_20261031.md` §2） |

## 8. 最终验证

最终 SHA `8c87f45`，go1.26.5，`wt/core-rt` 无已跟踪文件改动（`tracked_changes=0`，未跟踪的只有本报告与证据目录）。

| 检查 | 命令 | 结果 |
|---|---|---|
| 构建与静态检查 | `go build ./... && go vet ./...` | 通过 |
| 全量测试 | `go test -count=1 ./...` | 通过（`test_exit=0`） |
| race | `go test -count=1 -race ./...` | 通过（`race_exit=0`） |
| gofmt | `gofmt -l $(git ls-files '*.go')` | 19 个文件不一致，全部在 `main`/`cog/r1a` 上就已存在（18 个在 `main`），rt 未新增（`FMT-001`） |
| 公开 API | `apidiff -incompatible`（`main` → `c36bd29`） | 仅 `LSPStatus`（§2.2） |
| Schema 兼容 | `E/schema-compat/run.sh`（`c36bd29`） | 8 步全部符合 §6 |
| 无模型 E2E | `runtime-e2e-20261101/driver`（Engine、SQLite、bash、job、调度、notebook、诊断全部真实，Provider 为进程内脚本） | `failures=0`，21/21 PASS |
| 工具面 | `lcx-tools` 五个场景（`c36bd29`） | 14 个首请求的 `tools` JSON 与测量时逐字节相同（§3） |
| Browser 生命周期 | `lcxmcp -mode close/restart/cancel/policy`（`c36bd29`） | Close 后 0 孤儿（§4） |
| Desktop | §7 的无 `replace` 构建与测试（pin `8c87f45`） | Go build/vet/test、`npm run build`、Vitest 455/455 通过 |

过程中的失败如实记录：`c36bd29` 上第一次全量运行与生命周期驱动、Desktop 测试、E2E 并行，race 中 `internal/toolset` `TestShellTimeoutSourcesAndResult/argument` 失败一次（1 s 超时内 bash 尚未输出 `begun`；之后不加负载的运行均通过，记为 `TEST-FLAKE-002`），同时把误放进模块目录的探针 `_test.go` 编进了 `go test ./...`（已改为 `.go.txt`）；第二次不加负载的运行只有 `TestQueryMemoryIsBoundedByLimit` 失败，由 `8c87f45` 修正（§5）。`8c87f45` 上的运行全部通过。

证据：`E/core-tests-8c87f45.log`（最终）、`E/core-tests-c36bd29-run1.log`、`E/core-tests-c36bd29.log`、更早 SHA 的日志；`E/desktop-tests-pin-*.log`；`E/runtime-e2e/`。

## 9. 合并建议

**Core：建议 Owner 授权 B1**——把 `rt/execution-runtime`（`8c87f45` 加其后的报告提交）以 `--no-ff` 合入本地 Core `main`。依据：

1. 无冲突，合并树等于 rt 树（§1）；公开 API 唯一不兼容项是 `LSPStatus`，Desktop `main` 不受影响（§2.2）。
2. 独立审查的全部 P1（RT-exec-1/RT-state-1–4、U1–U3）与 P2 都已整改，并由原审查者在合并后的 SHA 上复验 FIXED；最终三域结论均为 PASS_WITH_FINDINGS，剩余都是 P3，已逐条转入 backlog（§5）。
3. 最终 SHA 上的全量测试、race、Schema 兼容、无模型 E2E、工具面复测、Desktop 无 `replace` 构建与测试都通过（§8）。
4. B1 只是本地合并，不改变发布状态：不 push、不打 tag、不改版本常量。

授权后的步骤（均未执行）：在 `LumenCortex-Core`（`main` 已检出、工作区干净）执行 `git merge --no-ff rt/execution-runtime`；确认合并后的树等于 rt 树（`git rev-parse HEAD^{tree}`），在合并提交上再跑一次 `go test -race ./...`。随后把 workspace `TODO.md` 的 `RT-MERGE-001`、`COG-001`、`RUNTIME-DEADLINE-001` 状态更新为“已合入本地 main”。

**Desktop：暂不合入 `main`。** 分支可快进，验证也通过（§7），但 `go.mod` 的 `replace` 指向本工作区，且 B6（T7-S1 审批 UI）未完成。Core B1 完成后，下一步是在 Desktop 分支上把 `replace` 换成 Core `main` 提交的伪版本并重跑 §7 的检查；合入 Desktop `main` 仍以 T7-S1 为前置，除非 Owner 书面接受“带 required check 的工作区 run 失败”这一后果。

**发布前仍需处理（不阻塞 B1）**：B2（G0b：T0-S2 迁移/反向迁移、O-2 备份保留期）、B3（版本号）、B4（版本提交的门禁）、B5（Core tag 与 push）、B6、B7（Desktop `main` 上未推送的 `4a6c2f3`），以及公开仓库的内容决定：`benchmarks/tb2/` 中出现内网地址与自有域名的 7 个 Markdown 文件，和本证据目录里驱动 `go.mod`/源码中的本机绝对路径（`/Users/…`）。

**需要 Owner 决定**：B1 授权；`NOTEBOOK-001` 的 `OWNER_CHECK`（主线程被撤回 fence 后、继续运行时宿主是否恢复对其 notebook 的访问；不影响 B1）；上述公开仓库内容。

## 10. PARTIAL / NOT_RUN

| 项 | 状态 | 说明 |
|---|---|---|
| 外部 SIGKILL Browser MCP server | PARTIAL | Chromium 在 Playwright 自己的进程组，Core 触达不到；已写入模板 `limitations`，`MCP-001` |
| Exa / GitHub / CodeGraph MCP | NOT_RUN | 无凭据 / 未安装；按指令不配置真实凭据 |
| Computer Use 实连 CodeBridge | NOT_RUN | 只 `stat` 了 socket；会话绑定用本地替身 stdio server 测试 |
| idle（600 s）与 crash 模式在 `c36bd29` | NOT_RUN | 在 `72d613f` 上跑过；之后的改动只涉及停止回调，close/restart/cancel/policy 已在 `c36bd29` 重跑（§4） |
| 工具 token 数在最终 SHA | 未重算 | `tools` JSON 逐字节相同，token 数不可能变化（§3） |
| Desktop 渲染 UI 检查在 `c36bd29` | NOT_RUN | 最后一次在 Core `9dcf95c`；原生 WebKit 视觉验收属 `UX-001`（`OWNER_CHECK`）；未为 `c36bd29` 构建 `.app` |
| 付费模型 / Benchmark | NOT_RUN | 按指令不运行 |
| T0-S2 迁移/反向迁移（G0b） | 未实现 | 发布前置，见 §2.3 |
| `6b654a0` 测试基础设施修复 | 未复现 | 依据代码推理（§5）；修复后相关测试全部通过 |
