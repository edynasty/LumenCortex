# Delivery Contract 独立复核 — 2026-11-04

## 1. 最终结论

**FAIL**

解耦方向正确，普通无 Git 文件交付及执行生命周期主路径已有有效实现；但当前候选仍存在**旧 verified 在重验失败后继续作为成功证据、宿主决议与持久化自动完成互相矛盾、越界路径被 verified**等实质性缺陷。相关测试通过不足以支持合并准入。此结论不表示 Agent Loop 又依赖 Git，也不表示已发现成功工具被重放。

本轮只读检查原代码、测试及文档；唯一仓库写入是本报告。没有修复、格式化、提交、切换分支、合并、修改 Core main、启动付费请求或 Benchmark；没有修改 TB2 题目、Image、Verifier、冻结 Cohort 和历史证据。

### 独立性与归因限制

- 主会话参与过最初 Astra 实现，不能自称完全无实现参与的第三方。三个新上下文、非实现者 reviewer 分别检查文件系统、执行生命周期、TaskState/子任务。实际模型均为 **local/gpt-6-astra**；不是 Sol，也不是跨模型独立 PASS。
- reviewer 只读分析、不执行测试；本报告中的动态结论由主会话另写的仓库外诊断程序及本地测试独立验证，不以 Opus 报告的 PASS 为依据。
- Astra 和 Opus 修改都未提交，无法从 Git 严格分离作者逐行差异。D1–D4 的归属依据 Opus 报告与现有实现吻合；其他缺陷注明是累计候选缺陷，不能全部归咎于 Opus。

## 2. 审查对象与基线

- 工作树：`wt/core-execution-retry-review`。
- 分支：`bench/execution-lifecycle-candidate`。
- HEAD：`4786db146f73244b35986619cc22b3cba2921e59`，与指定 Astra 基线相同；`4786db1..HEAD` 无新增提交。
- 开始时：staged 0，modified tracked 24，untracked 4；tracked diff 为 +657/-216。四个 untracked 为 Opus 报告、两个 repository 原生/无 Git 测试、runtime 生命周期测试。
- 阅读了完整 `git diff 4786db1`，并另外阅读不在该 diff 中的新增测试和 Opus 报告。不能把 HEAD 直接当作已包含修复的可复现 revision。
- Core `main` 为 `b90cb8f3b680258589df9d345d94cb66465ac7a8`；merge-base 为 `c08f5c44ed753e239bfe3656f63f4d7a0ebc268a`。`main..HEAD` 有 194 个提交，`main` 与 HEAD 的树差异为 638 文件。本轮不是这 194 个提交的整体合并审查，不证明 Desktop 集成或整个候选分支可直接合入。
- 本轮开始记录了 980 个已有 tracked/untracked 文件的内容摘要；报告创建前复核均未改变。既有两份报告未覆盖或改写。

### 两份报告与代码的核对

- `task-delivery-2026-11-03.md` 顶部 2026-11-04 修订与“原生基线、验证移出 Loop、独立 DeliveryResult”实现相符；下方旧 Delivery correction 叙述已明确标为历史。
- `task-delivery-review-2026-11-04.md` 所列 D1–D4 修改确实存在，四组测试也确实检查了所述主场景。
- 不能接受其“未发现可观察的 queued-edit 错误”判断：本轮 F2 已经复现两个公开接口给出不同 resolution。
- 不能接受“持久化失败后存储中保持 pending”对所有重验成立：只对首次验证成立，F1 证明重复验证后可以仍是 verified。
- 原报告的全量 test/race/build/vet 和 Linux 容器结果属于此前证据，本轮不冒充重新执行。

## 3. 实际检查的文件与完整调用链

完整差异覆盖：

- `internal/repository/delivery.go`、`delivery_test.go`、`delivery_native_test.go`、`delivery_nogit_test.go`；相关 `snapshot.go`、blob/路径/Git helper。
- `internal/taskstate/{delivery,state,render,resolution,tasks}.go` 及 Delivery 测试；相关 `edits.go`、工作流与宿主编辑边界。
- `internal/agent/loop.go`、Delivery/执行恢复/生命周期测试及退出、inbox、evidence、reconcile 调用关系。
- `runtime/{agent,delivery,supervisor,taskstate,tasks,subagents}.go`，`delivery_test.go`、`delivery_lifecycle_test.go`、`execution_retry_test.go`；相关 `threads.go`、模型 fixture、状态仓储、工作树 lease、子结果 provenance/withdrawal。
- `cmd/lcx-go/main.go`；ADR-0006、`docs/agent-runtime.md`、两份原报告和 workspace 架构契约。

调用链：

1. `prepareRun → startTaskState → SeedDelivery`：检查契约一致性、清除宿主提供的基线，首次采集 native baseline；采集错误写 `BaselineIssues`，不作为 Agent 错误。
2. `BeginRun`：新 RunID 的 `pending` 与执行 Start 状态一同提交。Resume 不重新采集基线。
3. `Loop.Run`：没有 Delivery checker 字段、交付纠正分支或交付基础设施失败退出；既有 Workflow/Checks/WorkUnits/jobs 与执行控制保留。
4. `preparedRun.execute`：Loop 终态 → `endRunJobs` → `finishOwned/stopChildren` → completed 才 `settleDelivery`，否则 `recordedDelivery`。Supervisor 在清理/验证期间继续持有 run lock/workspace lease。
5. `settleDelivery`：加载状态 → 文件/Git 验证 → 独立结果和 Resolution → 保存 TaskState → `delivery.result`。此处 F1/F2/F6 存在缺陷。
6. `VerifyDelivery`：取得相同锁/lease，要求已 completed 且有契约，只重验、不调用 Loop、不发送模型请求、不重放工具。F1 是该路径缺少重验开始状态的问题。
7. Runtime/CLI 分别输出 execution status 与 DeliveryResult；TaskState、TaskResult、SubagentNode 读取独立验证状态。父任务通过 `TaskEvidence.open()` 保留未验证交付义务。

语言服务器工具返回 `No language server found for this action`；调用关系采用完整差异、源文件与定向搜索核对，未声称有 LSP references 证据。

## 4. D1–D4 逐项复核

| 项目 | 结论 | 独立证据与剩余问题 |
|---|---|---|
| D1 基线错误依赖范围 | 核心状态修复正确，去重不完整 | 原 permission 回归通过。独立三文件 expired-context 采集后，存在/不存在要求不带 baseline 错误，只有 changed 路径保留原 `context deadline exceeded`；移除 changed 要求的诊断副本得到 verified。通用 workspace 原因对两个 changed 文件仍重复两次，见 F8。|
| D2 ENOENT/ENOTDIR | 未完整修复 | 原 native 目录替换回归通过；但悬空越界 symlink 得 verified（F3），相同目录替换加 `Commit:true` 又变 unavailable（F4）。|
| D3 非 completed 返回 pending | 正常读取主路径正确，异常分支有新回归 | auth 失败返回 pending/RunID 的原测试通过。所有成功 BeginRun 的非 completed 退出集中到 recordedDelivery；但无契约失败任务在状态读故障后凭空出现 delivery unavailable（F5）。未把该问题误说成“已持久化 pending 被改写”。|
| D4 无契约重验错误 | 正确 | Runtime 回归通过；本轮实际无契约 Agent completed 后运行真实 CLI `delivery-verify`，exit 1，输出 `session has no delivery contract`，不是成功 `null`。|

**D1–D4 不能整体判为全部正确完成。** D1 对当前仍不可读的普通文件保持 unavailable，与完整快照证据策略一致，不是本轮另一个存在性缺陷。历史基线丢失不能通过重验恢复；原始原因仍保留在契约中。

## 5. 已确认缺陷

严重度按实际影响定为 P2，均非生产紧急事故；F1–F7 是当前候选需修复的正确性问题。合并阻塞不等于必须给每项贴 P0/P1。最终 FAIL 基于实质状态/边界缺陷，而非测试数量或 reviewer 标签。

### F1 — 重验结果保存失败后，旧 verified 继续解除交付义务

- 位置：`runtime/delivery.go:60–64,82–125`，尤其 `119–124`；消费者 `runtime/taskstate.go:216,331–338`、`runtime/tasks.go:570–575`。
- 复现：先验证存在的 `result.txt`，得到 verified；删除文件；仅在临时数据库注入拒绝非 pending settlement 写入的触发器；调用公开 `VerifyDelivery`。
- 注入仅作用于自建临时库，不是生产缺陷触发器：

```sql
CREATE TRIGGER review_reject_settlement BEFORE INSERT ON task_states
WHEN NEW.reason='delivery'
 AND json_extract(NEW.json,'$.deliveryResult.status')!='pending'
BEGIN SELECT RAISE(ABORT,'review forced settled-write failure'); END;
```

- 实测：返回 `verification_unavailable`，同时包含 `regular file required: result.txt` 与 `persistence` issue；之后 `TaskState` 仍是 `verified`、`Obligations=[]`，`ReadThread` 仍是 `done`。移除触发器再重验，正常写入 failed。
- 根因：新 Agent Start 会先写 pending，但只验证重试没有这个边界；失败只改返回值。宿主重读、父任务证据和结果游标仍把旧成功当作当前状态。
- 最小修复：重验在检查前、同一锁下先可靠记录新的 pending/失效状态；不能记录开始状态则不启动本次验证。终态保存失败时保留该 pending，而非旧成功；旧成功仍可保留为历史证据。不得重新运行 Agent。
- 范围：累计 Astra 候选遗留，Opus 未处理。不是要求在完全不可写的数据库中凭空保存错误。实际 SIGKILL 重验窗口尚未动态复现，但缺少开始记录同样可从代码观察到。

### F2 — queued host resolution 被自动完成绕过，公开接口互相矛盾

- 位置：`runtime/delivery.go:110–120`；正确顺序参考 `runtime/taskstate.go:246–280`。
- 复现：临时 session 为 completed、Delivery pending、resolution open，并有可完成的工作流；公开 `ResolveThread(id,"dropped","owner")` 排队宿主编辑，创建要求的文件，然后公开 `VerifyDelivery`。
- 实测：Delivery verified；`TaskState` 投影 `dropped/by=user` 且 `PendingEdits=1`，但持久化状态和公开 `ReadThread.Resolution` 都是 `done/by=evidence`。
- 根因：settle 只加载 stored state，不应用 inbox，却调用 AutoDone 并以 `InboxThrough=0` 保存。不是 Go 内存 data race，也不是编辑丢失；是确定的状态时序错误，重启/下一次 Start 前可持续被 read_thread/recall 错读。
- 最小修复：Resolution 判断前应用已排队编辑，按已有顺序保存消费 cursor 和 `HostStateEdges`，尊重用户 done/dropped；不能简单调用另一个可能不保存的 helper 后仍用旧 state 覆盖。
- 范围：Opus 已列为未处理观察，但“无可观察错误”判断被本轮反例否定。需要修改代码，不只是文档或风险注记。

### F3 — 悬空的工作区外祖先 symlink 被当成合法缺失

- 位置：`internal/repository/delivery.go:169–186`。
- 复现：`workspace/escape -> outside/not-created`，目标不存在；契约 `{"files":[{"path":"escape/output","absent":true}]}`；Capture 后 Verify。
- 实测：`verified`，baseline/current 均 `exists:false`。
- 根因：`EvalSymlinks(parent)` 的 ENOENT 让代码沿词法父目录回退，丢掉已经遇到的越界 symlink 目标；随后 Lstat 缺失被采信。并不需要并发、攻击程序或 Git。
- 应为：`verification_unavailable` 并说明越界祖先，不能解除交付义务。
- 最小修复：在把缺失后缀认定为工作区内缺失前保留并验证所有已遇到的 symlink 目标；覆盖悬空/嵌套路径。
- 范围：原生快照的累计候选缺陷，Opus ENOTDIR 修改未修复它；不能归因为 ENOTDIR 这一行本身新引入。

### F4 — `Absent + Changed + Commit` 未接通 ENOTDIR 语义

- 位置：`internal/repository/delivery.go:255–264` → `internal/repository/snapshot.go:79–85`。
- 复现：临时 Git 库提交 `pkg/old.go`；Capture 该路径且 `absent:true,changed:true,commit:true`；删除 pkg 目录、改为普通文件 pkg、提交全部变化；Verify。
- 实测：native 文件证据正确为 `deleted`，但整体是 unavailable，原因 `committed delivery files cannot be inspected`。
- 根因：Git helper 的 SnapshotFiles 只把 ENOENT 转成 BlobAbsent，ENOTDIR 仍导致 incomplete；因此不能到达新的“工作区与 HEAD 均已删除”成功分支。
- 最小修复：Delivery 的 Git 文件收集接收经过 confinement 验证的 absent/ENOTDIR 事实，保留真实 Git 错误；无需全面重写执行快照子系统。
- 范围：Opus D2 修复仅覆盖 native，漏掉显式 Commit 消费者。

### F5 — 无契约失败任务在状态读故障时凭空出现 Delivery

- 位置：`runtime/agent.go:202–208`、`runtime/delivery.go:69–76`；completed 的早期 load 路径也有同样结构。
- 复现：实际 Runtime RunAgent、无 Delivery、本地 ProviderFactory 返回 auth error；临时库在 execution terminal update 后插入 JSON `[]` 的 task_state 版本。该值满足 JSON 数据库约束，但无法解码为 State，专门模拟持久状态读取/解码故障。
- 实测：请求 1 次；execution 仍 `failed` 且保留原 auth error，但 AgentResult 包含 `delivery={verification_unavailable,runId,issues:[state/decode...]}`，违背“无契约才省略 delivery”的契约。
- 最小修复：preparedRun 保留已知的 contract-presence/Start pending，使没有契约的运行不进行 Delivery 结算/读回；必要时单独报告状态读错误。不能因无法读取而推断存在契约。
- 范围：D3 新增 recordedDelivery 产生的可定位新回归。不声称真实产品会自行生成 `[]`，也不声称该路径执行了 verifier 或改坏了已持久化 pending。

### F6 — 异步 completed 后状态读失败，验证错误没有任何结果事件

- 位置：`runtime/delivery.go:83–87` 提前返回，绕过 `124`；`runtime/supervisor.go:203–212` 丢弃 execute 的 AgentResult。
- 复现：实际 RunSupervisor、本地成功 provider、有 Delivery；临时库在 completed update 后注入上述不可解码状态；从 Start 前订阅足够大的事件队列，等到 run.stopped。
- 实测：session `completed`，模型 fixture 调用 1 次，`delivery.result` 事件 0 个，`run.stopped.data={}`。不可用原因仅存在于被 Supervisor 丢弃的返回值中。
- 最小修复：所有 Delivery 结算出口统一发布独立结果/诊断，包括早期 load 失败；不要把它变为 Agent error。存储坏了可无法持久化，但不能同时丢掉仍可用的事件出口。
- 范围：累计候选的异步错误证据缺口；没有改变 Agent Completed，问题是宿主无法得到所承诺的验证错误原因。

### F7 — 移除 Git 校验时遗漏分支名称的语法校验

- 位置：`internal/repository/delivery.go:23–29`；`internal/taskstate/delivery.go:75–76`。
- 实测：`Branch:"bad..branch"` 的 Validate/Capture 均返回 nil；同一个名称执行 `git check-ref-format --branch` exit 128，明确不是合法分支名。
- 根因：旧 Capture 的 checkBranchName 被删，但 native Validate 只限制长度、前导 `-`、NUL/换行；新契约可含不可能满足的分支名。未以付费 Agent 请求证明这一点，静态 Start 调用链已足以说明它不再被前置拒绝。
- 最小修复：保留不依赖 Git 可执行文件的分支名语法校验；缺 Git 应是验证不可用，非法 host contract 仍应拒绝。
- 范围：相对 HEAD 的累计候选回归，不是有证据的 Opus-only 新改动。

### F8 — 通用基线原因重复（非阻塞诊断问题）

- 位置：`internal/repository/delivery.go:70–72,90–91,131–148`。
- 独立诊断输入：两个 changed 文件都没有基线，只有一条通用 `workspace unavailable` baseline issue。
- 实测：同一 issue 在结果中重复两次；状态正确为 unavailable。加 commit 的重复及 generic error 组合只经源码检查，未另跑。
- 最小改进：按 phase/path/reason 去重，仍保持逐文件 unknown 证据；不要用去重丢掉不同文件的错误。它不足以单独阻止主线，但 D1“去重完整”声明需收窄。

## 6. 三个遗留设计问题的判断

### 6.1 子任务 Delivery 失败作为父任务 tasks 义务

**保留当前设计，不必改状态模型。**

- execution completed 与 delivery failed 是不同维度，不能把交付不符变成子 Agent failed。
- TaskResult/SubagentNode 带完整 Delivery；task-result cursor 对结果（含 RunID）取摘要，验证后更新能再次通知，完全相同的结果不重复通知。
- completed+pending/failed/unavailable 在 `TaskEvidence.open()` 仍是父任务 tasks 义务，阻止 evidence AutoDone；失败不会被吞掉。
- 义务不因时间流逝自动消失是合理的：宿主/父任务需修复、重验或按既有 API 明确结束。原子任务未继承父契约，不需要为了使每个子任务都有 Delivery 而新增推断逻辑。
- F1 会使失败重验的旧 verified 继续绕过此判断；这是证据更新缺陷，不是 tasks 义务设计本身错误。

### 6.2 自动完成前宿主排队编辑

**需要修改代码。** F2 是公开 API 可观察的状态矛盾，不再只是潜在风险。它不使文件 SHA256 判定本身出错，但会使 TaskState 自动完成结论和 read_thread/recall 错误。不能以“下次 Start 会覆盖”作为当前结果正确的依据。

### 6.3 首次完成后崩溃留下 completed+pending

**保留显式只验证恢复，不需要启动自动重验。**

- BeginRun 已先持久化 pending；完成终态已提交时不应回退 Agent execution。
- `TaskState` 可查询 pending，`VerifyDelivery`/CLI 可重验，要求 completed 并持锁，不触发 Agent/tool。
- 不主动调用时可以长期保持 pending，这是诚实的未验证状态；宿主若需调度重验可使用现有入口，不应为了自动化扩展 Core 生命周期。
- 必须修的是 **F1 的重复验证**：不能用首次 pending 的安全性证明已有 verified 的再次验证也安全。

## 7. 状态机、恢复与架构复核

| 执行情况 | 正确 Delivery 语义 | 当前审查结果 |
|---|---|---|
| 未选择契约 | 无 Delivery | 正常路径正确；F5 的读故障路径破坏此约定 |
| 成功 BeginRun，执行尚在运行 | pending + 当前 RunID | Start 原子写入；查询不执行 Delivery verifier |
| Agent failed/interrupted/waiting_gate 等非 completed | 保留 Start pending，不验证 | 正常读取路径正确，auth 回归有直接断言 |
| Agent completed，有效证据满足要求 | verified | 无 Git 修改/新建/删除主路径通过 |
| Agent completed，有效证据确认不符 | failed | 原缺失/未改动等回归及独立删除重验通过 |
| Agent completed，验证工具/IO/panic/cancel 不可用 | verification_unavailable | 不改 execution；早期状态读失败有 F6 证据出口缺口 |
| 首次 completed 提交后中断验证 | completed + pending | 合理；恢复不重放，显式重验 |
| 已 verified 后再次验证中断/保存失败 | 不能让旧成功代表新尝试 | F1 不满足 |

具体边界：

1. **Loop 独立性：** Delivery 校验/纠正分支已删除。现有 workflow/checks/owned jobs 仍是原执行语义，没有把 Delivery 结果作为新 Loop 终止条件。
2. **执行状态隔离：** 未发现验证错误把已 committed completed 改成 failed；本轮故障程序也保留 completed。失败读取造成的是 Delivery 投影/证据问题。
3. **Completed 不等于最终成功：** AgentResult 有独立 Delivery；TaskState `accepted()` 仍需 Workflow、Checks 或合格 WorkUnits，单独 verified 不证明语义任务成功。F1/F2 是当前真实的成功证据错误。
4. **普通文件无 Git：** SHA256 由 Go 流式计算；type/size/mode/exists 分开记录；同大小、恢复 mtime 的跨快照内容变化仍可识别；未使用 Git hash 作为 native digest。
5. **Git 隔离：** 仅显式 Branch/Commit 调 Git，不初始化仓库、不替 Agent commit、不要求安装 Git。缺 Git 报 unavailable；显式 worktree/isolated task 本就需要 Git，不属于纯文件验证。
6. **基线恢复：** Seed 丢弃宿主基线证据；Resume 保持原基线与契约。旧 Git-only baseline 不伪装 native baseline，不从执行后当前文件回填。
7. **文件类型与权限：** directory/symlink/FIFO 等非 regular 不作普通内容读取；不可读为 unavailable；真实 regular-file requirement 不满足为 failed。F3 是缺失祖先 confinement 例外。
8. **锁与并发：** VerifyDelivery 与 Agent 使用同 run lock/workspace lease；不能在活跃运行中重验。文件外部写者不受该锁约束，见风险范围。F2 是逻辑顺序，不是 race detector 能自动发现的内存竞争。
9. **SIGKILL/非幂等：** 原 SIGKILL 工具矩阵和恢复测试在本轮定向 Race 中通过；成功 marker 的 Resume/仅验证恢复测试通过。没有发现新的成功工具重放路径。
10. **跨接口：** TaskResult/子树有独立 Delivery，withdrawn 结果不返回交付证据，验证变化通过 cursor 通知；F1/F5/F6 是一致性例外。
11. **持久化：** optimistic version 和事务保留；坏存储无法凭空写证据。F1 要求可写时先记录 retry，F6 要求保存不了时仍通过可用事件出口报告，不要求伪造持久化成功。
12. **执行控制：** MaxSteps 默认/正值显式上限、Deadline、Cancel、Provider Retry、recover/reconcile 相关代码未因本轮 Delta 改策略，相关五包完整测试通过。
13. **无隐藏限制：** 新 10 秒采集/验证与 5 秒证据 cleanup 是操作范围，不是 Agent 累计步数、token、请求或费用上限。没有新无进展/重复工具调用终止逻辑。
14. **Benchmark 边界：** 本次生产代码 Delta 无 prove-plus-comm 或模型/cohort 特判。没有改 TB2 或用修改评分器弥补错误。

## 8. 实际验证与未覆盖范围

### 本轮执行

| 命令/场景 | 结果 |
|---|---|
| `go test -count=1 ./internal/repository ./internal/taskstate ./internal/agent ./runtime ./cmd/lcx-go` | 五包全部通过；runtime 240.017s，整组 242.21s |
| `go test -race -count=1 ./internal/repository ./internal/taskstate ./internal/agent ./runtime ./cmd/lcx-go -run 'Delivery\|KilledAroundAToolIsUnknownAndNotReplayed\|UnavailablePreStateStillRunsAndIsIncomplete\|StuckSnapshotIsBoundedAndUnavailable'` | 四个有匹配测试的包通过；CLI 包 `no tests to run`，不冒充其 Delivery Race 已覆盖；整组 14.44s |
| `go build -o <临时目录>/lcx-go ./cmd/lcx-go` | 成功，真实 CLI 被下列独立程序调用 |
| 仓库外独立诊断程序：悬空外链、ENOTDIR+Commit、通用 baseline 重复、queued drop、重验写失败 | F1–F4/F8 均复现；去掉写故障后重验得到 failed |
| 仓库外独立诊断程序：三文件过期基线过滤、非法分支、同步/异步终态读故障、真实 CLI | D1 主过滤通过；F5–F7 复现；D4 CLI exit 1 |

两个诊断程序通过临时 Go module 的本地 replace 导入真实候选，使用 `GOPROXY=off GOSUMDB=off`；没有拷贝实现替代被测代码，没有覆盖 Go/test 文件、overlay 或降低断言。文件/Git 操作仅针对程序自建临时目录。Runtime 状态组合通过自建临时数据库构造；模型调用全部由内存 ProviderFactory 返回，不访问外部 provider，不收费。第二个程序使用私有 HOME/XDG 配置，事件订阅在 Start 前建立并等待 run.stopped，不靠固定睡眠推断完成。

### 测试质量与证据缺口

- Opus 四组回归不是空测试：检查了基线失败依赖、ENOTDIR deletion、failed/pending RunID、ErrNoDeliveryContract。但场景窄，未覆盖本轮反例。
- D3 的直接 pending 断言只有 auth failure；原 cancel/deadline/panic/gate/storage 测试多数不带 Delivery。共享退出分支提供静态证据，不能把它说成完整状态笛卡尔积动态验证。
- `TestDeliveryCompletionCrashWindowDoesNotReplayAgent` 是直接运行 Loop 后 Close/Open 的窗口模拟，不是该窗口真实 SIGKILL；真正 SIGKILL 矩阵独立覆盖工具 write_file/bash/MCP。二者不能合称“每个 Delivery crash 窗口均实际杀进程”。
- verifier panic 测试检查 `inspectDelivery` 分类，并非真实哈希器产生 panic；没有宣称跑了该路径的实际文件系统 panic。
- 本轮未重新执行全仓 `go test ./...`、全仓 Race、`go vet ./...` 或全仓 build；没有生产代码改动，采用相关五包全测 + 定向 Race + 独立行为反例。此前 Opus 的全仓结果仍只标为此前报告记录。
- 本轮未重跑 Linux 容器、Windows、TB2 原镜像或 Coq/官方 verifier；不声称 benchmark Reward 或产品 Gate 已通过。

## 9. 尚未验证的风险与非缺陷限制

- native snapshot 在多个 syscall 之间检查 inode/size/mode/mtime，能发现普通替换/变化，但不能提供整个多文件集合的原子快照。外部进程在同 inode 写入并恢复元数据时的 torn digest 未用同步探针验证，标记风险，不据此追加“已证实攻击”。
- `EvalSymlinks`、`os.Open`、`Read` 本身未必被 Go context 及时中断。已有取消测试在读取前取消，不能证明阻塞/NFS/FUSE I/O 的严格 10 秒 wall-clock 上界。普通 FIFO 类型检查不等于证明所有阻塞 IO 有界。本地普通文件范围内没有本轮新增实际挂死证据。
- 历史无 native baseline 的 changed 要求只能 unavailable，是信息不可恢复，不建议重采样或重跑 Agent。
- 当前不可读但存在的文件被 unavailable，是当前快照不可观察；不是自动得出文件不存在或不符合交付。
- Git 专属快照仍有既有 blob/output 等边界；未把它们扩成无关快照重构任务。
- 磁盘/DB 完全不可写时只能返回/发事件，无法保证新错误落盘；必须如实报告而非伪造 saved 状态。

## 10. 对修复质量及合并准入的明确回答

1. **D1–D4 是否全部正确修复？否。** D1 主过滤正确但通用错误去重不全；D2 缺失路径 confinement 和 Commit 消费者未完成；D3 正常失败路径正确但引入无契约误投影；D4 正确。
2. **Opus 是否引入新缺陷？是。** F5 可直接归到 D3 的 recordedDelivery 新逻辑。F1/F2/F3/F6 等是累计候选遗留或 Opus 未处理问题，不虚构其作者归属。
3. **三个遗留问题是否需要改代码？** 子任务义务设计不需要重做；queued-edit 自动完成需要修复 F2；首次 completed+pending 不需要自动恢复，但重复验证的 F1 必须修复。
4. **Delivery Contract 是否符合 Core 整体架构？** 职责划分与两维状态模型符合通用 Harness 架构；当前实现仍违反部分持久状态、宿主编辑优先级和证据可靠性要求，不能给架构/实现无条件 PASS。
5. **当前是否可以进入 Core main 合并评估？不能作为通过审查、具备合并准入的候选。** 先修复 F1–F7 并补定向反例；复核这些差异及受影响接口，再评估真实集成基线。无需重跑付费 Benchmark或扩大审查到无关平台。

下一步仅是向实现者交付上述反例与最小修复边界。本轮按只读约束没有自行修复；已有源码、测试、原报告及历史结果保持不变。

收尾核验：再次核对全部 980 个已有文件摘要，变化为 0；已删除本轮自建临时 module、诊断程序和 CLI 二进制。各程序自建测试目录/数据库已在退出时清理；没有保留测试服务或仓库内脚手架。指定报告是唯一新增仓库文件。
