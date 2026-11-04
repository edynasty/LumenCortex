# Delivery Contract 独立审查与修复 — 2026-11-04

- 范围：`wt/core-execution-retry-review`，基线 `4786db1`（`bench/execution-lifecycle-candidate`），审查对象为 Astra 未提交的 Delivery 生命周期改动及其报告 `task-delivery-2026-11-03.md`（未改写）。本轮无 Commit/Merge/Rebase/Reset，未触及 Core main、TB2 题目/Image/Verifier、冻结 Cohort 或历史证据，无付费/外部模型请求。
- 审查者：主会话 Opus（本轮修改的实现者同为本会话，下述 PASS 不是第三方独立 PASS）。

## 调用链核对结论

Start → `startTaskState`：`SeedDelivery` 拒绝畸形/变更契约并清除宿主伪造的基线；`CaptureDelivery` 的基础设施错误只进入 `BaselineIssues`，写入 `pending`（RunID=本次 run）。Loop 已无 Delivery 字段、纠正或 latch 检查；`runEvidence` 不设置 `DeliverySatisfied`，因此运行中不会以证据关闭带契约的线程。`execute`：Loop 提交终态 → `endRunJobs` → `stopChildren`（`finishOwned`）→ 仅 `completed` 时 `settleDelivery`。settle 在原 run lock/workspace lease 内执行，结果只写 TaskState，不改 session 行或 Agent error；panic 被 `inspectDelivery` 归类为 unavailable。`VerifyDelivery` 持锁、要求 `completed`，只读重验，不触发 Loop/工具。Obligations/AutoDone 仅影响线程 Resolution，不影响 Loop 控制。子 Agent 不继承父契约（`executionOptions`/`inheritChildExecution`）。

## 发现与修复

| ID | 级别 | 问题与根因 | 修复 |
|---|---|---|---|
| D1 | P2 | `repository.VerifyDelivery` 把全部 `BaselineIssues` 并入结果：任一基线采集失败（权限、10 s 超时级联）使只要求存在/不存在、根本不比较基线的要求永久 `verification_unavailable`；`changed` 要求则重复报告两条问题且丢失原因。 | 新增 `baselineIssues`：仅当 `changed` 文件缺基线（或 commit 缺 BaseHead）时报告该路径的采集原因，否则回落到无路径的通用问题；存在/缺失仅依据当前状态判断。`.git/HEAD` 抽为 `deliveryHeadPath`。 |
| D2 | P3 | `deliverySnapshot` 遇到祖先为普通文件（ENOTDIR）时报 unavailable；目录被文件替换时，删除/不存在要求无法验证。 | `missingPath` 将 ENOTDIR 与 ENOENT 同等视为不存在；祖先仍须解析在工作区内，越界符号链接仍拒绝。 |
| D3 | P2 | 非 completed 运行的 `AgentResult.delivery` 为空，而 TaskState 记录 `pending`，文档写明“无契约才缺省”：CLI/宿主会把有契约的失败运行误读为无契约。 | `execute` 对非 completed 运行返回已持久化的 `pending`（`recordedDelivery`，读取失败报 unavailable/state），不执行验证。 |
| D4 | P3 | `Engine.VerifyDelivery` 对无契约会话返回 `(nil, nil)`，CLI 打印 `null` 且退出 0。 | 返回 `ErrNoDeliveryContract`。 |
| D5 | 文档 | `ObligationTasks`/`open()` 注释仍写“child 仍在运行”，与“completed 但未 verified 仍是父义务”的新语义不符。 | 注释更正；`docs/agent-runtime.md` 同步 D1–D4。 |

修改文件：`internal/repository/delivery.go`（`VerifyDelivery` 基线问题范围、`baselineIssues`、`missingPath`、`deliverySnapshot`）、`runtime/delivery.go`（`ErrNoDeliveryContract`、`recordedDelivery`）、`runtime/agent.go`（`preparedRun.execute`）、`internal/taskstate/resolution.go`（注释）、`docs/agent-runtime.md`。

## 回归测试（先失败、后通过）

修复前实际执行均失败：
- `TestDeliveryBaselineIssueOnlyAffectsChangeRequirement`（新）：存在要求在基线不可读时得到 unavailable；修复后 verified，`changed` 要求仍 unavailable 且只有一条带 `permission denied` 的 baseline 问题。
- `TestDeliveryNativeFileAncestorMeansAbsent`（新）：`pkg/` 被同名文件替换后两条要求均报 `not a directory`；修复后 verified，`pkg/old.go` 为 `deleted`。
- `TestDeliveryExecutionFailureRemainsSeparate`（加强断言）：失败运行 `result.Delivery` 为 nil；修复后为 `pending`，RunID 与 TaskState 一致。
- `TestDeliveryVerifyWithoutContractIsAnError`（新）：返回 `<nil> <nil>`；修复后 `ErrNoDeliveryContract`。

未删除测试、未降低既有断言。

## 实际执行的验证

- 定向：`go test -count=1 ./internal/repository ./runtime ./internal/taskstate ./internal/agent ./cmd/lcx-go -run Delivery` PASS。
- 全量（最终代码）：`go vet ./...`、`go build ./...` PASS；`go test -count=1 ./...` exit 0（32 包 ok、3 包无测试）；`go test -race -count=1 ./...` exit 0（32 ok、3 无测试，无 DATA RACE）；两组合计 559 s。改动文件 `gofmt -l` 为空（仓库内其他未触及文件存在既有 gofmt 差异，未处理）。
- 真实 CLI（macOS，本机 loopback 非流式假上游，PATH 仅 bash/chmod/rm/printf，非付费）：
  - A：基线时 `result.txt` 为 0000（存在要求）、`pkg/old.go` 要求删除；Agent 用 bash 改权限/写文件/把 `pkg` 换成文件 → `completed`/`verified`（`unknown`、`deleted`），2 次请求，marker 仅 `RAN` 一次；`delivery-verify` 再验 verified、同 RunID、0 次额外请求。
  - B：上游 401 → exit 1、`failed`，`delivery={pending, runId}`；`delivery-verify` 拒绝（需 completed）。
  - C：无契约 completed → JSON 无 delivery；`delivery-verify` → `session has no delivery contract`，exit 1。
  - 一次误跑：持久 shell 残留 `LCX_DELIVERY` 使首次 C 带了契约（结果 completed/failed，属于测试环境错误，非产品行为）；`unset` 后重跑即上述 C。临时目录、stub 已清理，端口无监听。
- 未在本轮重跑 Linux 无 Git 容器；沿用 Astra 报告中的 Linux arm64 非 root 无 Git 证据，且本轮改动未涉及 Git 路径以外的平台差异（ENOTDIR 为 POSIX 语义）。

## 保留的观察（未改）

- 子 Agent `delivery=failed` 计入父线程 `tasks` 义务（而非像 failed/interrupted 那样“待审查”）。这是 Astra 文档化的设计选择，两种方式都阻止 AutoDone；如需“failed 交付 = 待审查”需 Owner 决定。
- `settleDelivery` 直接调用 `AutoDone` 而不先应用队列中的宿主编辑（`latchEvidence` 会先应用）。用户编辑在下一次 Start 仍会覆盖，未发现可观察的错误结果，按 P3 记录。
- 崩溃于“completed 提交 → 验证”之间时保持 `completed+pending`，需宿主显式 `delivery-verify`；Engine 启动时不自动重验（无重放风险，属可选增强）。
- 基线/验证共享 10 s 操作超时（整个契约，含 32 个文件与 Git）；超大文件会得到诚实的 unavailable，不是 Agent 限制。

## 结论

- 代码实现：PASS_WITH_FIXES（D1–D4 已修复并有失败前/通过后回归；全量、Race、Build、Vet 通过）。
- 架构审查：PASS — Loop 不再读取 Delivery；验证在执行终态与 owned 清理之后、持锁且只读；Agent Result 与 Delivery Result 分离且一致地出现在 Runtime/CLI/Subagent/TaskResult。
- Benchmark 验证：未执行（无 Trial、无 Reward 声明）。
- 可进入 Core main 合并评估；合并前仍建议由非本会话的独立审查者复核本报告 D1–D4 diff。
