# Agent Harness Evolution — Hub / Control Plane

日期 2026-11-11｜分支 `feat/harness-evo-hub`（worktree `wt/core-harness-evo-hub`）
基线 `696eb74`（`feat/agent-debugger-execution-platform` 合入前的 debugger/platform 分支）

范围：F-STEER-1、自由文本 Steer、统一 Agent Hub（`HubView` / `RunSupervisor.Command`）、事件契约、
Task Lease、推送式工作者消息、Handoff。**不含**写租约（Editing slice）、DAP/Eval/toolset 文件工具。

## 1. 结论

| 验收项 | 状态 | 证据 |
|---|---|---|
| E5 10/10 作为 Go 测试 | 通过 | `runtime/hub_e5_test.go`（C1、C2、C3、C3b、C3c、C4、C5、C6、C7、C8），`-race` 通过 |
| Hub Command API + 幂等 + 过期 revision | 通过 | `runtime/hub_control_test.go` |
| 自由文本 Steer（持久、有序、一次生效） | 通过 | `runtime/hub_e5_test.go`、`TestE5SteerIsDurableAndAppliedOnce` |
| 事件 replay / gap 测试 | 通过 | `TestHubEventReplayReconnectAndGap` |
| Task Lease（claim/两个申请者/过期/续租/释放） | 通过 | `runtime/hub_leases_test.go` |
| 推送消息（忙碌接收者、未 ack 重投、重启持久） | 通过 | `runtime/hub_messages_push_test.go` |
| Handoff（源先停、note、租约转移、写所有权规则） | 通过 | `runtime/hub_handoff_test.go` |
| 迁移 0013 + down | 通过 | `internal/migrate` 全量 `-race` 通过（连续性/降级 fixtures） |
| `go build ./... && go vet ./...` | 通过 | 本次运行 |

未运行（预算用尽，如实记录）：`go test -count=1 ./...` 全仓套件。已运行的分片：
`-race ./runtime/`（新增 + 相关旧测试）、`-race ./internal/agent/... ./internal/taskstate/... ./internal/migrate/... ./internal/session/...`。

## 2. F-STEER-1（暂停时排队的 Steer 晚一个模型轮次）

**根因**：`applyInbox` 在步开始时执行，而 `BeforeModelCall` 边界位于 `internal/agent/send.go` 的
请求窗口构建之后，因此暂停期间到达的 host 输入只能进入下一次请求。

**修复**：`send()` 在 `BeforeModelCall` 边界返回后调用 `applyPendingInputs`；若有新输入，则重渲染
Task State 块、刷新 transcript，并用同一候选者重建窗口再发送（`internal/agent/send.go:293-325`）。
重建后若窗口溢出，走与初次溢出相同的 failover 路径，不新增模型轮次。

语义不变量（测试锁定）：
- 暂停→修改指令→继续，指令出现在**本次**请求（E5 C3）；
- 无额外模型轮次（C3 断言此时只有 1 次请求）；
- 指令只生效一次（`task_state.edits_applied` / `agent.steer.applied` 各一次，C3c）；
- 未暂停时若有输入排队，同样在构建请求前应用，行为与旧版一致。

## 3. Steer（自由文本）

- 存储：迁移 0013 `steer_messages`（`session_id, run_id, command_id UNIQUE(session_id,command_id), body, created_at, applied_at, applied_seq`）。
- Host API：`Engine.SteerAgent(ctx, sessionID, runID, text, commandID)`。
- 注入：作为 user 角色 aside（`protocol.OriginSteer`，`protocol.SteerHeader`）在**下一次请求窗口构建时**写入 transcript；
  与 `BeforeModelCall` 暂停的注入点相同（§2）。
- 事件：`agent.steer.queued`（含 `steerSeq`/`bytes`）、`agent.steer.applied{steerSeq, requestSeq, commandId}`。
- 并发多条按 `seq` 顺序应用（`TestE5MultipleSteersApplyInOrder`）。
- 只持久化 host 文本；无 reasoning、无模型内容（跨重启回归见 `TestE5SteerIsDurableAndAppliedOnce`）。

## 4. 统一 Agent Hub（不新增第二个 runtime）

新增 `runtime/hub_control.go`、`runtime/hub_leases.go`、`runtime/hub_handoff.go`；`Agent Hub` 只做投影与命令路由。

```go
type HubView struct { Root string; GeneratedAt time.Time; Cursor uint64
    Agents []HubAgent; Tasks []HubTask; Leases []HubTaskLease }
type HubAgent struct { SessionID, ParentSessionID, RunID, Role, Agent, Model, State string
    Live bool; Boundary agent.DebugBoundary; CurrentTool *HubTool
    StartedAt time.Time; Spend *HubSpend; Debug []DebugSnapshot; Eval []ExecutionKernel
    Jobs []JobInfo; Holdings []HubTaskLease; Revision uint64; RetryOf string; Paused *session.RunPause }
func (s *RunSupervisor) View(ctx, root) (HubView, error)
func (s *RunSupervisor) Command(ctx, HubCommand) (HubCommandResult, error)
```

- Agent 树：root + `SubagentTree`（subagent / execution_task），角色、agent、模型、状态、
  当前边界与工具、startedAt、usage（`CostKnown=false`：Core 不持久化单价，见 ADR-0019 §6）、
  jobs、`DebugSessions`、`ExecutionKernels`、Task Lease holdings、事件游标。
  DAP/Eval 未授权时该字段省略而不使整个投影失败（显式降级，报告记录）。
- 命令：`start | pause | resume | step | cancel | retry | steer | handoff | attach | detach`。
  - `pause/resume/step` 复用 `ControlAgentDebug` → `DebugControl`（E5 已验证的真实阻塞语义）；
  - `cancel` 调 `Cancel` → 取消上下文，暂停中同样生效（E5 C6）；
  - `retry` 用进程内 `lastRuns`（每会话最近一次运行的 options，重建时剥离 subagent/task/ownerRun 闭包）
    重新 `Start`；从最近安全点继续、不重放已提交工具、发布 `agent.retry{retryOf}` 并写 checkpoint；
    重启后无该记录时**显式拒绝**（不猜默认值）；
  - `steer` → `SteerAgent`；`handoff` → `RunSupervisor.Handoff`；
  - `attach/detach` 接受但无执行副作用（观察走 `SubscribeRuntimeEvents`+`View.Cursor`），结果里写明。
- **幂等**：命令 id 在执行动词**之前**写入 `hub_commands`（每会话保留最近 1000 条；`INSERT … ON CONFLICT DO NOTHING`
  原子预留，记录为 `pending`），执行后用结果替换。同一 `CommandID`：已记录结果 → 返回首次结果且 `Status=duplicate`；
  结果尚未记录（首次执行仍在进行、或进程在记录前结束）→ `Status=pending` + `ErrHubCommandPending`，**绝不按猜测重跑**，
  host 应 `View` 刷新后用新 id；同一 id 换 verb/session 直接报错。预留失败则命令不执行；结果无法记录时返回结果并附
  `ErrHubCommandUnrecorded`（显式上报，重试得到 pending）。超过一行上限（16 KiB）的结果记录时去掉 agent 的列表投影
  （debug/eval/jobs/holdings，可用 `View` 重读），保留状态、身份、revision。复审 HUB-F2：此前结果超过 16 KiB
  或记录失败时被 `_ =` 静默丢弃，重试同一 id 会再次执行（`TestHubCommandIDHoldsForAResultLargerThanTheJournalRow`、
  `TestHubCommandIDInFlightIsNotRunAgain`、`TestHubCommandWithoutARecordedOutcomeIsNotRunAfterRestart`、
  `TestHubCommandOutcomeThatCannotBeJournalledIsReported`）。
- **revision fence**：活动运行必须等值于 `HubAgent.Revision`（DebugControl revision）；无活动运行时必须为 0，
  否则 `ErrHubStaleRevision`。`RunID` 不一致同样按 stale 处理（E5 C4 规则扩展到全部 verb）。
- **重启后的暂停投影**：`run_pauses` 在暂停时写入、恢复/运行结束时清除、**仅在 Engine 关闭时保留**；
  重开引擎后 `HubAgent.State == "interrupted(paused)"` 且带 `Paused`。host 已取消的暂停运行在两种关闭顺序下
  （先 `RunSupervisor.Close` 再 `Engine.Close`，或只调 `Engine.Close`）都按取消结束、清除暂停
  （复审 HUB-F5：此前只调 `Engine.Close` 时 `e.closing` 覆盖了 host 的取消，`TestHubCancelThenCloseClearsThePauseOnEveryShutdownPath`）。
  重启后 `resume` 不是恢复旧 goroutine，而是新的 `Start`（`State` 为 interrupted 时 `resume/pause/step` 返回
  `ErrHubRunInactive`）。

## 5. 事件契约

已有类型复用，缺失补齐（均为 metadata，正文留在 transcript/artifact）：

| 语义 | 事件 |
|---|---|
| agent started / stopped | `run.started` / `run.stopped` |
| thinking（模型请求开始，无内容） | `llm.request` |
| tool started / output / completed / failed | `tool.start` / `tool.output` / `tool.end`（`ok:false` = failed） |
| paused / resumed | `agent.paused` / `agent.resumed`（新增，附带 `debug.agent.state`） |
| steered | `agent.steer.queued` / `agent.steer.applied` |
| subagent started / completed | `subagent.spawned`、`task.started` / `subagent.stopped`、`worker.task.completed` |
| verification | 复用既有 `delivery.*` / checks 结算事件（本切片未改 verification） |
| handoff / retry | `agent.handoff`、`agent.handoff.failed` / `agent.retry` |
| 命令 | `hub.command.accepted` / `hub.command.rejected` |
| 事件丢失 | `hub.events.gap{from,to,lost}` |
| 租约 | `task.lease.claimed/renewed/released/expired/transferred/rejected/error` |
| 推送消息 | `worker.message.delivered` / `worker.message.acked` |

- 每会话单调：持久事件 `cursor` 单调，`(session,run,sequence)` 由 0011 保证；实时 `Seq` 全局单调。
- 有界缓冲：订阅者缓冲 ≤4096，满则丢最旧并记录丢失的 durable cursor。
- 重连：`ResumeRuntimeEvents(cursor)` 重放；`hub.events.gap` 在丢失后的下一次投递前先发出，读者按 cursor 补齐。
- 运行隔离：`subscribeSession` 过滤 session；测试用同一会话的 12 次发布验证 gap 与补齐数量一致。

## 6. 协作增量

### 6.1 Task Lease（`work_unit_leases`）

- `ClaimHubTask`：单条 `INSERT … ON CONFLICT DO UPDATE … WHERE expires_at <= now OR holder = excluded.holder`
  → 原子性；`RowsAffected==0` 即 `ErrHubLeaseHeld`。同时校验 DAG（单元存在、依赖已验证、非终态）。
- 两个并发申请者：4 个 goroutine 同时申请，全部被拒（`TestHubTaskLeaseClaimExpiryAndRecovery`）。
- 过期：过期行由 `RecoverExpiredHubTaskLeases`/读取路径回收，单元回到 ready（`leases` 为空，可被新 worker 领取）。
- 续租：持有人在**模型步边界**（`AfterModelCall`）自动续租（`renewHeldLeases`），只续仍有效的租约；
  过期后不能续租，必须重新 claim（避免与已夺走者并存）。
- 释放：run 结束（完成/取消）时 `releaseHeldLeases` 在 run 离开 supervisor **之前**执行，宿主看到 run 停止即看到声明已释放。
- 投影：`HubTask.Lease` + `State="leased"`。

### 6.2 推送式工作者消息（0012 + 0013 列）

- 0013 给 `worker_messages` 增加 `delivery_state(queued|delivered|acked)`、`delivered_run`、`delivered_seq`、
  `delivered_at`、`acked_at`、`attempts`。
- 注入点与 steer 相同：接收者的下一次请求窗口写入 bounded aside（`protocol.OriginWorkerMessage`），
  无需接收者调用 `worker_inbox`（`TestBusyWorkerReceivesMessageWithoutInbox`：忙碌运行的第 2 次请求携带消息）。
- 未 ack 重投：每个新 run 最多重投一次，总尝试 ≤3（`MaxWorkerMessageAttempts`），之后仍可用 `worker_inbox` 读取。
- ack：`Engine.AckWorkerMessages(root, recipient, through)`；显式 `worker_inbox` 读取也视为 ack，包括已推送进请求窗口的消息
  （复审 HUB-F4：推送不再写 `received_at`，该列只记录显式读取；读取按 `acked_at` 判断，故已推送消息读后不再重投）。
- 重启持久：排队与 ack 均跨 `Open` 保留（`TestWorkerMessageAckSurvivesRestart`）。
- 唤醒空闲接收者：`RunSupervisor.Wake(ctx, sessionID, input)`（显式 host 调用，不自动唤醒关闭/撤销会话）。

### 6.3 Handoff

- 语义：停止源（cancel + 等待离开 supervisor，上限 20s）→ 在源的 Task State 写 `## Handoff` note（≤512 B，无 reasoning）
  → 目标 session 若不同则转移 Task Lease（`TransferHubTaskLeases`）→ `Start` 目标（`Input` 只指向 note）。
  源运行因 handoff 而停止时**不释放**其租约（复审 HUB-F1：此前运行结束清理先释放，活动源的租约在转移前
  已丢失、第三方可抢占）；租约由 handoff 转给目标，同 session 换 agent 时留在该 session，由新运行续租。
- 源与目标不会并发：目标 `Start` 前源必须已离开 supervisor；测试用 in-flight 计数断言 max in-flight == 1。
- 失败恢复：目标 Start 失败 → 租约回滚到源；目标在启动前被撤销 → 在写 note 前直接拒绝，租约完全不动。
  目标忙（`ErrRunHeld`）或源拥有工作树（`ErrHubWorktreeOwned`）在停止源**之前**拒绝（复审 HUB-F6：此前先取消
  活动源再拒绝）；仅当目标在检查与停止之间被并发启动时，停止后的复检仍会拒绝，此时源已停止、租约留在源。
- **工作树所有权按现有 owner/borrower 规则**：工作树目录按持有 session 命名绑定（`worktreeRole`），
  因此「源拥有工作树时跨 session handoff」被显式拒绝（`ErrHubWorktreeOwned`），要求先执行
  `WorktreeHandoffPlan/ApplyWorktreeHandoff`；同一 session 内换 agent 则工作树、transcript、Task State 自然延续。
  这是刻意的：不做目录迁移、不伪造所有权。

## 7. 迁移 0013 与重编号风险

`0013_hub_control.sql`（+ `down/0013_hub_control.down.sql`）：`steer_messages`、`run_pauses`、
`work_unit_leases`、`hub_commands`，以及 `worker_messages` 的 6 列 `ALTER TABLE … ADD COLUMN` 与 1 个索引。

**风险（重要）**：
1. 迁移编号是**顺序且无空洞**的（`internal/migrate.Load` 强制 1..N）。本切片占用 0013；同批其他切片
   （Editing 写租约、DAP/Eval）若也新增迁移，必须**统一分配**编号并在合并时重编号/重排 down 文件，
   否则 `Load` 直接失败（这是显式失败，不会静默错序）。
2. `ALTER TABLE worker_messages` 与 0012 同表：若 0012 在合并过程中被改写（而不是新增），本迁移的列语义会漂移；
   规范做法是 0012 冻结，只通过 0013 增量。
3. 迁移不可回滚地改变严格表结构之外的东西：`down` 依赖 SQLite `DROP COLUMN`（0002 已有先例，已验证）；
   `hub_commands`/`steer_messages` 等拥有 `ON DELETE CASCADE`，隐私删除通过 `purgeTables` 一并清理
   （`work_unit_leases` 按 `root_id` 清理，holder 已删的声明交给过期回收——避免 purge 语句出现重复占位符）。
4. 已发布数据库的 schema version 会升到 13；`ErrSchemaTooNew` 会让旧二进制拒绝打开（预期行为），
   Desktop 必须随 Core 版本升级。

## 8. 变更文件

- 迁移：`internal/migrate/sql/0013_hub_control.sql`、`internal/migrate/sql/down/0013_hub_control.down.sql`
- 存储：`internal/session/hub.go`（steer/pause/lease/command）、`internal/session/worker_messages.go`（推送状态）、`internal/session/purge.go`
- Agent：`internal/agent/steer.go`（注入点）、`internal/agent/send.go`（F-STEER-1）、`internal/agent/loop.go`、`internal/agent/types.go`
- Runtime：`runtime/hub_control.go`、`runtime/hub_leases.go`、`runtime/hub_handoff.go`、`runtime/hub.go`、`runtime/supervisor.go`、
  `runtime/agent.go`、`runtime/events.go`、`runtime/runtime_events.go`、`runtime/hub_messages.go`、`runtime/prompt.go`、`runtime/revoke.go`
- 协议：`protocol/agent.go`（`OriginSteer`、`OriginWorkerMessage`、`SteerHeader`、`WorkerMessageHeader`）
- 测试：`runtime/hub_e5_test.go`、`hub_control_test.go`、`hub_leases_test.go`、`hub_messages_push_test.go`、`hub_handoff_test.go`

## 9. 未验证 / 风险

- 全仓 `go test ./...` 未在预算内运行；已运行分片见 §1。
- `HubAgent.Spend` 是会话累计 usage，run 级 spend 只在 `budget.spend` 事件中；成本恒为未知（无持久化单价）。
- DAP/Eval 列表在未授权的会话上省略（不使投影失败）；若需要区分「无授权」与「无对象」，后续加显式字段。
- `retry` 的 options 记录是进程内、有界（256）；重启后无记录 → 显式拒绝，需 host 显式 `start`。
- 跨 session handoff 在源拥有工作树时被拒绝（见 §6.3），需要先 apply worktree。
- `attach/detach` 只作语义占位，真正订阅仍是 `SubscribeRuntimeEvents`。
- 事件 gap 依赖「缓冲区满」这一确定信号；若事件在别处丢失（例如持久化失败），由 `EventStats.PersistenceFailures`
  与 `Reliability="unavailable"` 暴露，不伪造成 gap。

## 10. 下一步

1. 与 Editing slice 协调迁移编号（见 §7.1），合并前统一重编号。
2. 全仓 `go test -count=1 ./...` + `-race ./...` 收尾。
3. Desktop：`HubView` + `Command`（每按钮带 revision）、Steer 输入框、Handoff 目标选择；过期 revision 提示刷新。
4. 若产品需要 run 级成本，增加持久化单价/usage 投影（独立切片）。
