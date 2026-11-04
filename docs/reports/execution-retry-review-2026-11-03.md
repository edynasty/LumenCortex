# 执行步数与故障恢复审查（2026-11-03）

## 范围与基线

状态：2026-11-04 生命周期改造、本机Mock与全量 Go Test/Race/Build/Vet 全部完成，提交为单一 Core 候选 commit（SHA 与构建产物哈希记录在工作区 `bench/evidence/bench008-20261104/core-candidate.json`，不写入本文以免自引用）。不声称独立模型审查或产品发布PASS，未推送、未打 tag。下方上一检查点结果不替代本轮验收。

本轮 Owner 授权针对通用 Go Agent Harness 独立审查 Agent 步数、Provider Retry、退避/Retry-After、Model Fallback、Runtime Failover、Delivery Recovery；允许必要重构，但不允许无限重试或无效循环。仅隔离 worktree 修改和本地无付费测试；不修改 Benchmark、历史数据或冻结产物；不合并、不推送。

- 隔离 worktree：`wt/core-execution-retry-review`（detached HEAD）。
- 固定基线：`350edf438b957395b5f72ace936d4d99971b3609`，来自已有 `feat/provider-retry-resilience`；该工作树检查时干净。
- `main`：`b90cb8f3b680258589df9d345d94cb66465ac7a8`，检查时干净，但未包含当前 Runtime/Delivery/Retry；不以旧主干重新实现已有能力。
- 历史候选和历史报告保持原样；本报告仅描述此基线上的新差异。未承诺产品发布或评测成绩。
- 本项目未配置 LSP，代码导航使用已定位源文件与定向搜索。

## 2026-11-04：Owner 新生命周期设计

- 当前仍为原隔离 worktree / detached `350edf4`，保留上一轮未提交改动；未切基线。
- 新默认 `MaxSteps=-1`，0 为未指定，正整数为宿主显式限制。移除默认工具批次8/只读子代理6的截断；权限、资源容量与明确操作级超时保留。
- 无默认全局Deadline。宿主Deadline/调用上下文贯穿准备、Loop、请求和子任务；Cancel级联。运行结束释放其拥有的子运行和后台任务，不留下脱离控制的工作。明确新Start/Resume仍可由宿主启动。
- `ChildExecutionOptions` 提供立即子Agent的宿主限制覆盖：步数/批次非零覆盖、Deadline只能收紧；继承Retry与Budget副本。
- 移除Delivery最佳缺项/两次无进展纠正、jobs两次后放行、工具失败计数/进展提示。显式Workflow/Checks/Delivery是宿主验收条件，不从调用模式推断进展或循环。历史决策的Replay算法保持原义。
- 候选不因冷却永久过滤；健康/冷却仅调整初次尝试顺序。新 `model.candidates.retained` 可回放规则保留全候选，让A在B失败后按自己的hint到期首次尝试。
- 实施选择：立即子运行采用结构化所有权，正常结束也取消并等待本次启动的子运行；用 `ownerRun` 标识本次调用，不按历史 parent session 误杀宿主独立启动的 proposal/resume。这是执行正确性选择，不冒充 Owner 原文。
- 停止基于工具失败计数调高 effort；移除已失效的 `effort.escalation` 配置及主动接线。旧配置会按未知字段报错，需删该字段；历史 escalation 的私有解码类型/Replay 算法保留，既有 Task State progress 只保留数据不驱动新运行。
- 新发现并复现：同步 `Engine.Close()` 遇准备阶段 auto-effort 请求，旧实现先关闭 DB，返回 `sql: database is closed` 而非取消。`trackRun` 现覆盖准备至资源清理，Supervisor 的执行 context 也提前到入口；同一用例现返回 `context.Canceled`，主模型零请求。
- 工具限制已再次核实：声明的 `opus-dev` 实际未挂载，两任务 preflight 拒绝；reviewer 又回退到 `gpt-6-astra`，已停止并丢弃，不计独立 PASS。本轮由主会话实现；不能将上一轮 RecoveryReview 的 PASS 套用到新生命周期改动。

### 本轮已执行验证（2026-11-04）

- 定向 Lifecycle：Agent 0.700 s、Runtime 6.680 s PASS。覆盖0/-1无限与正数限额、140次重复失败不推断终止、12工具默认批次、子继承/覆盖、父调用取消/Host Cancel/Deadline、前台shell及owned后台job、独立proposal不误杀、A冷却后重新参与与Replay篡改拒绝。
- 新准备阶段回归先失败：`TestPreparationStopsWithEngineCloseOrDeadline/close`，明确 `sql: database is closed`。修复后连同历史 effort Replay/schema拒绝/生命周期定向包 PASS：runtime 3.616 s、agent 0.529 s、config 0.356 s。
- 真实 CLI 用本轮编译二进制、临时 HOME/XDG、127.0.0.1 SSE Mock，无 Key：140次失败 read_file 后第141请求完成（1.241 s）；502→429→截断含工具SSE→工具→503→完成共6 HTTP（0.294 s），marker精确为单行 `RAN`，截断 `WRONG` 未执行。
- 显式3步中断后新进程 Resume：原session成功，新增1请求；SIGTERM打断60秒Retry等待：1请求，0.254 s退出，无第二请求；持续503：7请求/6重试后中断，无自动重跑。
- SIGTERM打断真实前台shell：1请求，0.290 s退出；保存的owned shell PID已不存在；429 hint=60 s + Deadline=3 s：1请求后拒绝不可能等待（0.158 s），不抢跑hint。
- Mock费用没有真实供应商结算，spend保留unknownCostRequests；不把其knownCostUsd=0解释为真实请求免费。所有服务由场景finally shutdown/join。
- 跨Session真实Engine+标准OpenAI SSE adapter：首轮A返回429/Retry-After=1 s、B返回503后中断；同Engine第二Session先试B，失败后等待A冷却，A成功；共A2/B2，A间隔1.001361167 s；`Retry.Disabled=true`，证明A延后首次尝试不是额外Retry。
- CLI取消后的SQLite读取：唯一tool结果`local-0`、`cancelled=true`、`exitCode=-1`，保留stdout `STARTED`；session为`interrupted/cancelled`，不是成功或未配对调用。
- 首轮全量 Test 返回失败（219.14 s），6处旧契约测试未迁移：自动Progress写入、candidate旧rule、撤销检查返回旧fence错误、caller取消后继续运行、两处子任务脱离父运行。迁移测试到显式Task State写失败、保留候选、准备取消、新handoff终态及宿主明确Resume borrower；保留真正的存储/撤销/锁/工作树安全断言。命令链因此未运行Race/Build/Vet，不记为通过。
- 上述6处测试迁移后的定向验证PASS：agent 0.975 s、runtime 2.105 s。新增断言旧Progress计数9保持原样且140次重复失败期间effort始终low，不驱动新执行。删除仅比较Go embed与原文件的同源复制测试；配置拒绝/往返与历史Replay行为测试保留。
- 最终完整命令链 **PASS，exit 0，528.70 s**：`CGO_ENABLED=1 go test -count=1 ./...`（32包PASS/3包无测试；agent 4.700 s、runtime 206.948 s）→ `go test -race -count=1 ./...`（全仓32包PASS/3包无测试；agent 9.832 s、runtime 284.024 s）→ `go build ./...` → `go vet ./...`。后两项无输出并成功完成；不是只跑受影响包Race。
- 全量包含SIGKILL非幂等恢复矩阵`TestKilledAroundAToolIsUnknownAndNotReplayed`（write_file/bash/MCP × 四个中断点）、`TestEchoStaysInRunMemory`、`TestResumeOnFreshEngineNeverSendsEcho`，以及请求/工具准入、fence、借用工作树和资源清理回归。已有成功工具不因恢复重执行，隐藏echo不进入持久恢复上下文。
- 上述检查点测试后未再修改行为代码；下节复审又修改了一处并重新跑全量门禁。无已知未修复的本轮行为失败；保留平台/真实上游/独立审查边界见下文。

### 复审：隐藏子任务上限（2026-11-04 第二轮）

- 对全部改动做第二轮源码审查（agent/runtime/config/cmd/docs 及新增测试），专查剩余隐藏的步数、批次、子任务和交付上限。仍保留的数字都是显式资源或单次操作限制：每步 `maxRetries`（默认6）、`maxChildrenPerParent`/`MaxAgents`、`wait_subagent`单次≤60 s、展示用的 `SubagentTree`/`read_thread` 分页。未发现其他基于计数推断停止的规则。
- **新发现并修复：子任务数量隐性上限100。** `session.Store.ChildRelations` 最多返回最早的100个子任务，下列8处判断都依赖它：`taskResults`、`executionEvidence`、`activeChildren`、`HasActiveSubagents`、`proposalChild`、`CancelSubtree`（另有100节点循环保护）、`DeletionPreview`、`temporaryDescendants`。步数不再设上限后，父任务在整个运行期可以累计创建超过100个子任务；第101个及以后的子任务因此会出现以下问题：仍在运行却不算未完成义务；不占用父任务并发配额；结果从不交付；`CancelSubtree` 取消不到它；删除父任务时不被列为依赖。
- 先复现失败：新增 `runtime/TestChildBeyondTheHundredthStaysVisible`（100个已完成执行子任务+第101个运行中子任务）；修复前报 `active children = 0`。修复方式：新增 `Store.AllChildRelations`（`LIMIT -1`，同一查询），上述8处判断全部改用它；`CancelSubtree` 改用 visited 集合防环，不再按节点数截断；`ChildRelations` 仅保留给展示分页。修复后该用例同时验证并发计数、`tasks` 义务和删除预览依赖，全部 PASS。
- 修复后全量门禁 **PASS，exit 0，552 s**：`CGO_ENABLED=1 go test -count=1 ./...`（32包ok；agent 5.231 s、runtime 215.273 s）→ `go test -race -count=1 ./...`（32包ok；agent 9.543 s、runtime 299.276 s）→ `go build ./...` → `go vet ./...`。
- CLI Mock：用修复后的二进制重跑 11-03 本机 SSE 夹具（127.0.0.1，无 Key）。recover：completed，6次 HTTP，marker 仅 `RAN`（截断流中的工具未执行），2.85 s。持续503 + `LCX_DEADLINE=4s`：interrupted，6次 HTTP，`stop=deadline`，outage 3098 ms。`LCX_DISABLE_RETRIES=1`：1次 HTTP，`stop=retry_disabled`。
- Linux：Docker `golang:1.23`（Go 1.23.12 linux/arm64，kernel 7.0.14-linuxkit），`--network none --memory 1536m --cpus 4`，源码只读挂载。`internal/{agent,session,jobs,toolset}` 全包，以及 runtime 中按名称筛选的 Lifecycle/Supervisor/Cancel/Deadline/Killed/Preparation/ChildBeyond/Retry/Recover/Shell/Job/Subtree/Isolated/Proposed 用例全部 PASS（runtime 24.750 s）。这不是 Linux 全量 Race，也不涉及 Windows。容器设了内存上限，避免占用共享4 GB VM 中 sub2api 的资源。期间没有运行 BENCH005 Harbor 进程或 Trial 容器，其 `expproxy` 未触碰。
- 跨仓库后续：Desktop 没有使用被移除的 Go 符号，但其副本 `docs/lumencortex.schema.json` 仍描述 `effort.escalation`。Desktop 升级 Core pin 时需同步更新，届时旧配置中的该字段会报未知字段错误。本轮未改动 Desktop。

### 提交前门禁（2026-11-04 第三轮，提交前最终检查）

- 对 `internal/agent`、`runtime`、`internal/session`、`internal/taskstate`、`cmd` 重新做数值上限扫描。仍保留的数字均为显式资源/展示上限：`MaxSubagentTreeNodes=100`（仅 `SubagentTree` 展示）、`maxThreadChildren=20`（仅 `read_thread` 展示）、`MaxTasks=100`（线程记录的执行任务数，达到时在启动子任务前返回 `ErrTooManyTasks`，不会留下未记录的子任务）、`maxPendingWakeups=3`（调度）、`ChildRelations` 50/100（展示分页）。无基于计数推断“卡住/完成”的规则。
- 全量 Race：`CGO_ENABLED=1 go test -race -count=1 ./...` 32包ok（agent 9.699 s、runtime 294.903 s），3包无测试，无失败。
- Linux arm64（`golang:1.23` 本地镜像，go1.23.12，gcc 12.2.0，kernel 7.0.14-linuxkit aarch64，`--network none --memory 2g --cpus 4`，非 root uid 1000，`CGO_ENABLED=1`，源码只读挂载）：`internal/{agent,session,jobs,toolset}`、`config` 全包 PASS；runtime 按名称筛选的 Lifecycle/Supervisor/Cancel/Deadline/Killed/Preparation/ChildBeyond/Retry/Recover/Shell/Job/Subtree/Isolated/Proposed/UnlimitedSteps/Cooldown/Delivery 用例 PASS（25.446 s）。前一次尝试误用 linux/amd64 镜像且 `CGO_ENABLED=0`、root，产生 sqlite stub 与只读目录权限失败，属夹具错误，不计入结果。仍不是 Linux 全量 Race，也不涉及 Windows。
- 候选 CLI（`go build ./cmd/lcx-go`，darwin/arm64）对 127.0.0.1 SSE 夹具重跑，无 Key、无付费请求，全部符合预期：
  - 默认无步数上限：140 次失败的 `read_file` 后第 141 次请求给出最终答案，`status=completed`，141 次 HTTP，1.45 s。
  - 502 → 429(`Retry-After: 1`) → 截断工具流 → 工具 → 503 → 答案：`completed`，6 次 HTTP，7.36 s，marker 仅 `RAN`（截断流中的 `bash` 未执行）。请求间隔 0.76/1.71/3.55/0.18/0.90 s，指数退避且遵守 Retry-After。
  - 持续 503 + `LCX_DEADLINE=300s`：`interrupted`，7 次 HTTP、6 次重试，50.8 s，因每步重试上限停止，非无限循环。
  - `LCX_DISABLE_RETRIES=1`：1 次 HTTP，`stopped because retries are disabled`。
  - `Retry-After: 60` 等待期间 SIGTERM：3.01 s 退出，只发过 1 次 HTTP，`context canceled`，等待结束后没有第二次发送。
- `git diff --check` 无输出；未跟踪文件仅为本报告与4个新增测试；差异与新增文件中的密钥模式扫描（sk-/AKIA/ghp_/BEGIN KEY/Bearer/`BENCH_UPSTREAM_API_KEY=`）无命中；忽略项仅 `.history/`、`.lumencortex/` 与既有 ompsession fixture 日志，不入库。

### 当前参数、兼容性与交付边界

| 控制 | 当前值/语义 | 理由 |
|---|---|---|
| MaxSteps | 默认-1；0未指定；正数显式限制 | 任务长度由宿主/用户控制，不用固定数字推断完成或循环 |
| 工具批次 | 默认不截断；正数显式限额 | 不丢模型正常工具批次；权限、并发容量和操作级超时独立保留 |
| Deadline | 默认无；子继承、只能收紧 | 端到端取消，不将单工具超时误作全局期限 |
| Provider recovery | 初次候选之后，每响应步共享最多6次 | 避免持久在线时盲目循环；成功响应后才开启下一步额度 |
| Backoff | 1 s指数增长至30 s，75–100%抖动 | 6次累计45.75–61 s，不含请求耗时；遵守更长Retry-After |
| MaxOutage | 仅显式配置生效 | 不用隐式30分钟窗口代替宿主任务生命周期 |
| Delivery/Jobs纠正 | 无次数推断结束；显式条件仍必须满足 | 不把重复调用当进展，也不在两次纠正后虚假放行 |

- 兼容性变更：旧`effort.escalation`配置必须删除；新`ChildExecutionOptions`由宿主选择立即子运行覆盖。`RunSupervisor.Start`不再把调用方context脱离为后台无限生命周期，调用方应传真实运行生命周期context。
- 非幂等保护、截断流工具拒绝、执行准入/结算、存储fence与历史消息保留；没有额外Retry层或自动重启任务。
- 本轮是隔离源码候选提交，不是产品发布：不改Desktop pin、不改真实配置、不推送/不打tag/不合并，不修改Benchmark、历史评测或冻结产物。
- 验证边界：macOS arm64/CGO；不声称Windows/Linux实机或真实上游恢复率。内存冷却不跨Engine重启持久化。新生命周期未取得合规独立模型审查PASS（工具不可用/回退已记录）；无自动放行发布。

### 本轮交付文件与Git状态

- 配置与入口：`config/{types,load,merge}.go`、配置fixtures/验证测试、`cmd/lcx-go/main.go`、`docs/lumencortex.schema.json`。
- 执行与恢复：`internal/agent/{loop,send,recovery,policy,envelope,jobgate,types,tasktools}.go`，`internal/taskstate/{progress,render}.go`；新增`execution_recovery_test.go`/`lifecycle_control_test.go`及受影响契约测试。
- 生命周期：`runtime/{agent,supervisor,revoke,engine,subagents,tasks,models,diagnostics,taskstate,deletion,worktree_safety}.go`、`internal/session/relations.go`；新增`execution_retry_test.go`/`lifecycle_control_test.go`，迁移handoff/borrower/撤销/candidate/Delivery相关测试。
- 文档：`docs/agent-runtime.md`、`docs/go-provider.md`、ADR-0004/0006/0019的明确增补及本报告；旧ADR正文和上一检查点记录保留作历史，不代表最新行为。
- 已实际检查Git：detached `350edf438b957395b5f72ace936d4d99971b3609`，staged=0、unstaged=54、untracked=5（4个新增回归文件+本报告）。改动仅本隔离worktree，无Benchmark/历史评测/冻结产物路径；无提交、合并或推送。
- 本次临时CLI二进制、cross-session smoke源码、临时工作区/DB已清理；Mock服务已shutdown/join。Go测试自行回收临时目录与进程。



## 上一检查点（2026-11-03，历史记录）

## 已确认设计事实

| 机制 | 基线行为 | 本轮关注 |
|---|---|---|
| Agent step | Loop 与 CLI 默认 24；每个模型响应及其工具批次为一步；传输恢复在同一步内部 | 默认是否适合真实编程任务；终止原因与 Resume |
| 探索子代理 | 父级 MaxSteps 非正或大于 12 时一律截为 12 | 隐藏且不可通过宿主提高的上限 |
| Provider Retry | 较早适配器 2 次重试、0.5/1 s 退避、Retry-After >5 s 立即返回；当前适配器每次 Complete 一次 HTTP | 保留集中所有权和逐请求准入/记账，避免恢复与 HTTP 重试相乘 |
| Runtime recovery | 当前无次数上限；1 s 基础、30 s 上限、75–100% 抖动；无 Deadline 默认 outage 1800 s，有 Deadline 等到 Deadline | 有限且合理的恢复次数；截止与取消；逐候选 Retry-After |
| Model fallback | Agent model list 是唯一 failover chain；availability 顺序切换，capacity 缩一次窗口后只切更大上下文，auth/bad_request 停止 | 不引入第二个模型路由器；保留显式列表、可重放切换记录 |
| Delivery | 显式持久化交付契约；两次纠正后再次失败则 delivery_incomplete | 区分持续无进展与已取得可验证进展，不能仅调用任意工具就刷新恢复额度 |

来源：`internal/agent/{loop,send,recovery,policy}.go`、`runtime/{agent,subagents,tasks}.go`、`config/types.go`、`cmd/lcx-go/main.go`；既有契约 ADR-0009、ADR-0018/0019、`docs/reports/provider-retry-recovery-2026-11-03.md`。

## 验收

1. 步数、HTTP 尝试、模型切换、交付纠正语义分离；默认值和覆盖优先级可说明。
2. 有限重试覆盖瞬时故障；Retry-After 不被提前重试；取消/Deadline 正确终止；认证/坏请求不空转。
3. 部分流不执行工具，故障后成功工具只执行一次；每次请求独立准入和结算，未知使用不伪造为零。
4. Runtime 取消/故障/Resume 保留工具配对、交付契约和持久状态；不序列化隐藏推理。
5. 完整 Go 测试、build、vet、受影响包 race 与实际 CLI/本机假上游故障恢复验证；报告所有失败和平台边界。

## 验证记录

- 基线新增行为复现：短退避（1 ms）下 200 ms 内 155 请求，直到 Deadline 而非有限次数停止；24 步截止；交付缺项 `[a,b,c,d] → [b,c,d] → [c,d]` 第三次即中断。初次长步骤夹具使用了未提供的 read_file 工具，后改为真实提供的 bash；该次只证明 24 次响应上限，不当作 24 次成功工具执行证据。
- 跨步骤冷却复现：预置 50 ms Retry-After，仅约 1.095 ms 即发送（修复前失败）。
- 修改后完整 `go test -count=1 ./internal/agent`：通过，6.902 s。
- 新增 Runtime 定向测试：子代理 10 步硬限额 / 17 次响应成功、立即子任务禁用 Retry 的拷贝继承、真实四文件逐次交付、无关写入不刷新纠正额度：通过，1.429 s。
- 开发中夹具问题已修：Go Final 为可空指针；capacity 夹具无旧上下文时第二请求后即本地 overflow，增加旧 transcript 后才能覆盖第三次远端 capacity 拒绝。未把这些失败当产品缺陷；最终完整验证单独记录。
- 最终冻结代码：`go test -count=1 ./...` **PASS**，exit 0，225.983 s（runtime 213.311 s）；真实模型 opt-in 环境已清除，没有 realmodel build tag。此前代码检查点也通过全量测试；不以旧检查点代替最终结果。
- 受影响包 Race：`go test -race -count=1 ./internal/agent ./runtime ./config ./provider/...` 全部通过，298.258 s（runtime 291.891 s）。最后冷却条件调整后，完整 Agent 测试 7.367 s、Agent Race 10.175 s、`go build ./...` 和 `go vet ./...` 全部通过。
- 全量包含现有真实子进程 SIGKILL 工具恢复矩阵 `runtime/TestKilledAroundAToolIsUnknownAndNotReplayed`（write_file/bash/MCP）、checkpoint Resume、取消 shell 配对、Delivery Reset/Resume、隐藏推理/echo 不进入持久状态回归。它们是零模型测试，不是远端 provider 可用性证明。

### 实际 CLI 与本机假上游

均使用临时 HOME/XDG_STATE_HOME/XDG_CONFIG_HOME、127.0.0.1 HTTP 服务、无 API key；没有真实付费请求。测试脚本和临时服务不进入仓库。

| 场景 | 观测结果 |
|---|---|
| 502 → 429(50 ms hint) → 含工具的截断 SSE → 正常工具 → 503 → final | completed；6 HTTP；marker 只有 `RAN` 一行，截断流中的 `WRONG` 未执行；0.655 s |
| 完整默认参数下 503、503、成功 | completed；3 HTTP；间隔 816.6 / 1601.4 ms，符合 1 s/2 s 下抖动；2.599 s |
| 持续 503，1 ms 测试退避，无 outage window | 第 7 请求后 interrupted，6 retries，stop=max_retries；不执行工具 |
| 等待中 SIGTERM | 1 HTTP 后 interrupted，context canceled；没有第二次请求 |
| 429 Retry-After=60 s、Deadline=3 s | 1 HTTP 后立即拒绝等待，stop=deadline；不提前请求 |
| 30 次真实 read_file 后 final，未覆盖 MaxSteps | completed；31 HTTP、30 tools；0.328 s |
| 显式 MaxSteps=3 后新进程 Resume | 首次3 HTTP/3 tools 后 max_steps；Resume同session成功，新增1 HTTP |
| 同一真实 Engine，两 Session，共享冷却 A=60 s，B 先失败后恢复 | 最终代码重验：首Session interrupted；第二Session completed、2 HTTP；全程 A1/B3，A未提前重试；标准OpenAI adapter+localhost SSE，2.72 s |

假上游缺失 usage/price 时 Spend 仍标记 estimated/unknown；表中不把未知真实费用计为零。取消 smoke 初次断言把 Go 的 `canceled` 拼成 `cancelled`，已直接修正断言检查现有结果，未重复执行场景。

### 独立静态审查闭环

`RecoveryReview`（Claude Opus 5.5）确认基线4项：跨步骤hint绕过、capacity误报availability、立即子任务Retry丢失、Delivery累计纠正。后续指出冷却选择组合缺陷并提供反例：先修最早可用顺序，随后收敛到保守规则——仅当前候选等待无法满足Deadline/Outage且已有候选完整恢复等待可满足时才切回恢复；否则保留初次sweep，不引入第三套候选调度状态机。最初冷却反例已本地复现；50 ms可行候选的两个反例补回归并通过。最终定向静态审查 **PASS**：没有剩余影响本任务的已证实问题；先前通过的不变边界保持通过。审查未运行测试，执行证据由主会话提供。

### 验证边界

- 当前平台 macOS arm64、CGO SQLite；未声明 Windows/Linux 实机验证或真实上游恢复率。
- 没有进行 Benchmark、用户模型收费请求、提交、暂存、合并、推送或发布；Core main、原候选分支、Desktop 均未改。
- 显式 Retry-After 缓存限于当前 Engine，重启不保存；不将其包装为跨进程供应商限流器。128/6 是解释充分且可覆盖的默认策略，不是普适最优值。
- Optional MaxOutage 仍是下一次发送前的检查，不中断已经在途的 HTTP；在途请求由 Provider timeout 与 run Deadline/取消约束。独立 worker 与父级分别有有限 step/retry，既有独立生命周期不变。

## 架构调整与参数依据

- **保持一个 Retry 所有者**：Provider `Complete` 一次 HTTP；Loop 内承担等待、切换、准入、未知结算。没有恢复层×适配器层相乘；没有新服务或新路由器。
- **有限恢复**：`execution.retry.maxRetries` 默认 6（0/缺省取默认，不是无限），是每个 response step 在初次 failover sweep 后共享的可用性恢复次数。每个候选最多一次 capacity 缩窗仍独立有界。最坏请求数量 ≤ 2×候选数+6；成功响应开始下一步时重新计数。取消等待不算已发送 retry。
- **退避**：保留 1/2/4/8/16/30 s × 75–100% 抖动，6 次累计等待 45.75–61 s（不含请求耗时及更长 Retry-After）。比原适配器 0.5/1 s 更能覆盖短暂故障，又避免长任务在不可用上游上盲目运行到 Deadline。此前口头 47–63 s 为误算，以本处为准。这是工程选择，不声称模型成功率已量化提升。
- **时间限制**：不再因无 Deadline 自动设 1800 s outage；明确配置的 `maxOutageSeconds` 仍有效；绝对 Deadline/取消优先。长 Retry-After 不被 maxDelay 截短；超出已配置时间边界则拒绝发送，不假装故障恢复。
- **跨步骤冷却**：Engine `Health` 分开保存 advisory 与显式 `notBefore`；全候选不健康时仍可探测无 hint 的候选，但每次发送必须遵守显式 hint。并发短 hint/成功不得擦除未到期长 hint。状态为 Engine 内存，不添加跨进程持久化冷却系统。
- **步数**：`config.DefaultMaxSteps=128`，CLI/Loop 共用。明确正数宿主上限原样保留，<=0 取有限默认；一次成功 response+工具批次算一步，completion correction 也耗步，失败 HTTP 不额外耗步。128 是为多文件检查、修改、验证提供空间的保守有限默认，不是 OMP 参数或 Benchmark 调参。探索子代理继承父级步数，不再硬截 12；权限与每批 6 工具限制不变。
- **交付恢复**：两次纠正只约束没有改善的交付验收；与历史最少缺项集合比较，严格子集才刷新额度。只读查询、无关成功写入、相同缺项重排、退步再恢复都不刷新。总步骤仍有限；任意局部代码变化不等于完成。
- **模型与 Runtime 生命周期**：Model Fallback 是配置列表，Runtime Failover 是按 error class 执行该列表，不建第二套机制。auth/bad_request 终止，capacity 仅一次缩窗及更大上下文切换；当全部候选最终 capacity-exhausted 时，保留最新 capacity 原因，不错报旧 availability。立即子任务拷贝 Retry override；延期 proposal/显式 Resume 重新采用当前执行策略。只读子代理继续继承工具 Deadline；独立 execution worker 原契约允许超过父任务生命周期，保留显式 CancelSubtree，而非隐式级联取消。

## OMP 参考与明确不采用的部分

参考可读本机包 **18.13.3**，不是声称当前 OMP 二进制源码一致：`~/.bun/install/cache/@oh-my-pi/pi-coding-agent@18.13.3@@@1/src/`。

- `session/settings.ts:697–733`：retry 默认 10、500 ms、provider hint 等待上限 5 分钟；`retry-fallback-chains.ts:89–97`：增长上限 8 s、75–100% jitter。LC 借鉴职责和抖动，不照抄参数。
- `turn-recovery.ts:543–586,861–897`：成功后结束 retry saga，finally 清理；`:2967–2980`：切模型会刷新 retry 计数。LC 不复制按模型刷新预算和凭证轮换，所有恢复候选共享 6 次。
- OMP core 没有固定 step cap（`pi-agent-core@18.13.3@@@1/src/agent-loop.ts:1328–1337`）；LC 保留有限步数。其 semantic continuation 多为 3 次、DSML nudge 为 2 次；LC 不引入额外判断模型或基于活动摘要的无上限 goal continuation。
- 参考 agent `OMPReference` 实际模型 `combo/deepseek-v4.1-flash`，只读源码；独立静态 reviewer `RecoveryReview` 实际 `claude-opus-5-5`，非实现者。工具未挂载声明的 opus-dev，两个实现任务 preflight 拒绝，因此所有代码由主会话完成；不冒充 Opus 实现或 Sol 审查。

## 交付文件与状态

- 配置/入口：`config/types.go`、`docs/lumencortex.schema.json`、`cmd/lcx-go/main.go`。
- Agent：`internal/agent/{loop,send,recovery,policy,delivery}.go`；回归在 `execution_recovery_test.go`，并迁移现有受影响测试。
- Runtime：`runtime/{agent,subagents,tasks,diagnostics}.go`、`execution_retry_test.go`、`subagents_test.go`。
- 文档：`docs/agent-runtime.md`、`docs/go-provider.md`、本报告；旧报告和Benchmark资料未改。
- 历史检查点（2026-11-03）时工作树为 detached `350edf438b957395b5f72ace936d4d99971b3609`、改动未提交；本轮已在其上提交为单一候选 commit（见顶部状态）。默认变化仅在该候选生效，不改变现有主干或冻结二进制。
