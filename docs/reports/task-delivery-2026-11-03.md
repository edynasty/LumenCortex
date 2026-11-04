# Task delivery completion — 2026-11-03

## 2026-11-04 生命周期修复（当前工作，覆盖下文历史语义）

- 工作树 `wt/core-execution-retry-review`，基线 `4786db1`，开始时干净；Core main 尚无 Delivery Contract。本轮不提交、不合并、不修改评测材料。
- 根因链：Start → `startTaskState` → `CaptureDelivery` → `SnapshotFiles`/Git；基线错误拒绝 Start。Loop 最终答复 → Delivery → `checkFailureExit` 将验证错误作为执行失败；`latchEvidence` 与 `TaskState` 查询还重复检查。
- 已执行失败复现：无 Git 的已有文件在 Capture 阶段报 `delivery baseline files cannot be inspected`；没有 Delivery checker 的纯 Loop 因交付门禁成为 `interrupted`，而非 Agent Completed。
- 决策：原生文件 SHA256/元数据基线；Delivery 移出 Loop，执行 completed 已提交、owned jobs 已停止后独立验证；结果记录 pending/verified/failed/verification_unavailable，保留问题路径/阶段/原因与前后快照。仅显式 branch/commit 要求才访问 Git。
- 状态：实现与验证完成；`go test -count=1 ./... && go build ./... && go vet ./...` PASS，32 个测试包通过、3 包无测试，整组 239.64s。`go test -race -count=1 ./...` PASS，同样 32 包通过、3 包无测试，323.97s。定向 Delivery、原有 SIGKILL/恢复/快照不可用回归全部通过。开发中一个未使用 import 已清理；该编译失败已记录，不当作产品行为结果。
- 真实 macOS CLI（本机 SSE 假上游，PATH 无 Git、真实 bash/文件）：修改+新建 → completed/verified；读取权限故障 → completed/verification_unavailable（phase/path/reason 持久化）；新进程 `delivery-verify` → verified、额外模型请求 0、非幂等 marker 仍只有 ONCE；缺失交付 → completed/failed。共 5 次本地 HTTP，无付费上游请求；服务及临时工作区已清理。
- Linux arm64 / Go1.23.12 / CGO / 非 root / `--network none`：repository/taskstate/runtime 的全部 TestDelivery 通过（1.945/0.007/0.712 s），CLI 构建通过。无 Git 的 `python:3-slim` 容器实际运行 CLI：Git=null，completed/verified，modified/created/deleted 三类均正确，2 次本地 HTTP；`delivery-verify` 额外请求 0、counter=ONCE。首次误用 amd64 的 python:3.11-slim 未启动 arm64 CLI；更正本地镜像平台后通过，非产品缺陷。未运行/修改原 TB2 Image 或题目。
- 额外调用链核对：旧 execution snapshots 在无 Git 时只记录 CoverageUnavailable，不停止工具；Reconcile 仅结算 admitted/gap，已完成执行记录不重放。保留其 Git blob/lineage 语义，不扩展到本轮之外的全局快照重写。显式 Git worktree/isolated-task 功能仍需 Git，普通非 Git Agent 不需。
- 工具限制：指定开发代理 `opus-dev` 未挂载；reviewer 实际回退到主模型，已停止，未采纳其独立审查结果。本轮无独立模型审查 PASS 声明。

### 关键实现与改动文件

- `internal/repository/delivery.go`：独立 Go SHA256 流式文件快照；不存在、类型、大小、权限和内容分开；created/modified/deleted/unchanged/unknown 证据。Git 仅用于显式分支/提交要求；command error、截断、不可读检查均为 unavailable，不伪装契约失败。保留 clean-filter/HEAD/assume-unchanged/skip-worktree 原提交验证。
- `internal/taskstate/{delivery,state,render}.go`：`Absent`、持久化 `Baseline`/`BaselineIssues`、`DeliveryResult`；不采信调用方提供的 baseline，不刷新恢复基线。原 `BaseBlob` 只保留历史可读，不能当 SHA256 或重采集。
- `internal/agent/loop.go`：删除 Delivery checker、最终答复纠正和重复 latch 检查，原 Workflow/Checks/WorkUnits/jobs/输出截断及执行控制不变。
- `runtime/{delivery,agent,taskstate,supervisor}.go`：Loop terminal completed 与 owned cleanup 后验证；结果不能改写 execution error/status。Start 只验证契约结构，采集故障进入证据；TaskState 查询不执行 Delivery 验证。`Engine.VerifyDelivery` 受 session/workspace lease 及 Engine 生命周期保护，仅重新验证完成后的产物。完成与验证之间崩溃保存 completed/pending，后续重验不重跑 Agent。
- `runtime/{tasks,subagents}.go`、`internal/taskstate/{tasks,resolution}.go`：子结果独立携带 Delivery；验证变更进入结果游标，保证完成后的验证更新不会被旧 terminal status 吞掉或重复通知。未验证子产物仍是 parent obligation，不冒充 Agent Failed。withdrawn 子结果不泄露交付证据。
- `cmd/lcx-go/main.go`：`delivery-verify <session-id>`；agent/resume JSON 独立输出 delivery，退出码仍依据 Agent 执行结果。
- 文档：`docs/agent-runtime.md`、ADR-0006 与本报告。所有修改仅在本 worktree；无 Commit、stage、push、tag、Desktop pin 或 Benchmark 冻结数据修改。

### 回归覆盖（真实文件/存储/工具，模型响应为确定性夹具）

- 新增 `internal/repository/{delivery_nogit_test,delivery_native_test}.go`：缺 Git 的已有文件修改；创建/删除/不应删除/未变化；存在要求；同大小同时间内容变化；超过旧 4MiB 限制；取消哈希；旧 Git 基线不可用；越界路径与非文件。
- 新增 `runtime/delivery_lifecycle_test.go`：缺 Git 的修改/新建/删除完整 Agent；Agent 运行期间查询/重验不干扰；基线权限错误仍执行；结束时权限错误保留 Completed+Unavailable，重启后仅验证；验证 panic 分类；Agent 完成后的 Cancel；真正 Agent auth failure 仍 Failed+Pending；MaxSteps 后 Resume 不重放成功 append；显式 Git 不可用；完成/验证间崩溃；子结果验证更新与去重。
- 迁移现有 `runtime/{delivery_test,execution_retry_test}.go`、`internal/taskstate/delivery_test.go`、`internal/agent/{delivery_test,execution_recovery_test,lifecycle_control_test}.go`：取消不再成立的 Delivery correction 假设，保留真实负交付/基线/执行控制验收。
- 已执行定向：`go test -count=1 ./internal/agent ./internal/taskstate ./internal/repository ./runtime ./cmd/lcx-go -run 'TestDelivery|TestUnavailablePreStateStillRunsAndIsIncomplete|TestStuckSnapshotIsBoundedAndUnavailable|TestKilledAroundAToolIsUnknownAndNotReplayed'` PASS；含原 SIGKILL write_file/bash/MCP 非幂等工具矩阵。
- 最终全量：`go test -count=1 ./...`、`go build ./...`、`go vet ./...` 全部 PASS；全量 `go test -race -count=1 ./...` PASS。保留原 Cancel/Deadline/Retry、owned child/job 清理、隐藏 reasoning 不进入持久化及 crash/reconcile 测试，未绕过原测试集。

### 已知边界

- Baseline 真正丢失时不能恢复历史事实；保持 unavailable，不重新运行 Agent 或伪造基线。
- 状态库本身不可写时无法保证把新错误写进已失效存储；返回结果/event 明确给出 persistence issue，绝不改成 Agent Failed。宿主可以修复存储后调用只读重验。
- 10s 是验证操作自身超时，5s 是终态证据写入清理超时；没有新增 Agent MaxSteps、Token、累计请求/费用或无进展终止机制。
- 原 Git-only 工作树操作仍需 Git；非 Git admission lineage 可显示 CoverageUnavailable，已证明不停止 Agent，也不代表 Delivery unavailable。未把两种证据混成同一状态。
- 不宣称官方 prove-plus-comm Reward、Coq 证明能力、Benchmark 成功率或产品发布门禁已验证；本轮无付费 Trial。
- 临时 Linux build/cache/脚本/工作区和 macOS CLI 已清理；本机 SSE server 已 shutdown/join，容器均为 `--rm`，未修改共享 Docker 配置。
- 本轮无剩余已知的 Delivery 生命周期验收缺口；上列旧基线不可恢复、存储不可写和显式 Git 功能限制均显式可见，不伪装交付验证成功。

下文为 2026-11-03 历史实现及验证，不代表本轮生命周期契约。

Status: isolated candidate on `fix/bench003-shell-timeout`, based on `7111ddb182f230fcf956ca6bc99c28e3c3ceba4d`; clean before work. No merge to main, push, tag, release or official-score rewrite. Product Gate remains unapproved.

## Problem and decision

The original loop checks Workflow, Work Units and configured required Checks. With none declared, text `stop` completes. `TestDeliveryStopIsNotProof` first failed with `Status:completed Final:The implementation is not finished.` after one request. The added host contract makes that same response interrupt instead of proving delivery.

`AgentOptions.delivery` / CLI `LCX_DELIVERY` declares files (exist/change), branch and commit separately. Task State captures pre-execution blobs/HEAD once, persists them and renders the requirements each request. Existing approved Checks and Workflow remain behavioral verification. No natural-language-to-shell inference, DSML execution, automatic commit, unconditional dirty gate or new recovery FSM. Two corrections, then `interrupted/delivery_incomplete`; deadline, budget and maxSteps still apply. Conversation turn completion remains ADR-0006 D3, with unmet delivery retained as an objective obligation. A notebook/summary is not proof.

This is not automatic understanding of arbitrary prose. Existing callers that do not select a contract retain their prior semantics; a file condition is not proof of semantic correctness. Historical True-Myth's 80,697 B dirty patch / empty official patch is motivation, not a new benchmark success. Tests use real files/Git and runtime tools but deterministic provider responses.

## Verification actually executed

- Failing-before regression above, then passing `go test ./internal/agent -run '^TestDeliveryStopIsNotProof$'`.
- `go test ./internal/agent ./internal/taskstate ./internal/repository ./runtime ./cmd/lcx-go -run 'TestDelivery' -count=1`: pass after correcting fixture compilation/import and explicit reset input. Missing files, bounded repeated answers, unchanged baseline after reset/resume, actual `write_file` delivery, branch mismatch, empty commit, dirty commit, and no-commit dirty delivery covered.
- `go test -race ./internal/agent ./internal/taskstate ./internal/repository ./runtime -run TestDelivery -count=1`: pass; final runtime rerun after committed-blob comparison also pass.
- `go test ./runtime ./internal/agent ./internal/taskstate ./internal/repository ./provider/openai ./cmd/lcx-go -run 'TestDelivery|TestInteractive|TestContextReset' -count=1`: pass.
- `go test ./provider/openai ./internal/agent ./internal/taskstate ./internal/repository ./cmd/lcx-go`: pass. `go vet ./...` and `go build -o /tmp/lcx-delivery-candidate ./cmd/lcx-go`: pass.
- Initial full `go test ./...` failed `internal/jobs/TestOutputIsBoundedOnDisk`: it incorrectly combined stdout+stderr against a per-stream bound. Owner's correction checks each stream independently; `TestOutputPerStreamDiskBoundWithBothChannels` fills both streams. Production limits unchanged. BENCH005 initial full test/vet/build/Delivery race and captured SSE replay all exited0; final rerun follows independently reproduced sparse/large-index correction below.
- Actual CLI against isolated local SSE fixture (not a paid model): three missing-file answers produced exit1/interrupted, then explicit `resume` after operator-created file produced exit0/completed. Receipt `bench/evidence/task-delivery-20261103/cli-smoke.json`; temporary server and workspace removed. Initial fixture mistakes (literal credential schema, JSON response to streaming client) failed and are not success evidence.
- Original captured SSE: Tengo/Testem reject as availability, True-Myth remains valid stop with no tools. `LCX_BENCH004_TRACE_ROOT=.../bench004-20261103 /opt/homebrew/bin/go test ./provider/openai -run 'TestCapturedFinalResponsesReplay|TestMalformedDSML|Test.*(Incomplete|Truncated|Partial|EOF|Availability)' -count=1 -v` pass. Output `artifact://1864`; exact raw paths, sizes and SHA256 in `bench/evidence/task-delivery-20261103/sse-replay-sources.json`. No raw reasoning added to runtime recovery.
- Final corrected candidate: full `go test ./... -count=1` exit0 (214.74s), `go vet ./...` exit0, `go build ./...`/CLI build exit0, Delivery Race exit0 and original captured SSE replay exit0. Exact commands/times/logs: `bench/evidence/bench005-20261103/acceptance.json`. Final review closes reproduced P2s; many thousands of materialized flagged paths can still hit the bounded inspection ceiling (P3, explicit error, never false completion). Product/Opus approval remains separate.

## Evaluation boundary

Read `bench/EVALUATION_ROADMAP_20261103.md`: TB2 W13 first, SWE-bench Verified12 next, same 1017 route, independent cohorts, no historical reward changes. GET model catalog confirms both `deepseek-v4.1-flash` and `deepseek-v4.1-flash-1017`; cached fix-git image is amd64. No inference request sent. No full cohort started.

Paid TB2 smoke follows final passing acceptance, bounded independent review and exact Core commit/Linux binary. SWE environment preparation runs independently. No claim that this candidate improves reward, Token efficiency, RSS or mainstream-harness parity.

## Scope and risk

Paths are literal relative regular files, bounded to 32 entries/128 bytes each; no deletion/artifact content validator. Existing snapshot hashing bounds individual files to 4 MiB. Commit delivery needs a descendant baseline with a real tree delta, clean scoped files and actual blob equality with HEAD, including index-flag bypass protection. Same-UID full-shell actors can change local runtime state; this does not create an OS sandbox. The host must declare requirements; Checks approve behavior, not model self-assertions. No Desktop change is needed for this optional Core API; Desktop does not yet expose contract selection. Independent Opus final audit is not replaced by this session's review.

## Independent implementation review

`DeliveryReview` found one P2: commit-only contracts with no file list could miss dirty tracked files hidden by `assume-unchanged`/`skip-worktree`. Fixed by bounded `ls-files -v -z` inspection of flagged tracked paths, actual file hashing and HEAD blob comparison; incomplete inspection fails closed. Both flags now have a commit-only consumer-visible regression. The targeted regression and `go test -race ./runtime -run TestDelivery -count=1` passed after the fix; vet/build also passed. Reviewer re-read the correction and closed the finding with no additional defect; no reviewer checks were run. Baseline persistence, Checks/Workflow order, context reset and Conversation semantics had no additional concrete review blocker. This is not a product or Opus approval.

FinalDeliveryReview found no P0/P1 blocker and one P2: commit-only inspection rejected large ordinary indices and absent sparse-checkout files. A real Git regression reproduced both failures before correction. Stream NUL index entries, retain only bounded flagged paths, ignore absent skip-worktree files only in sparse mode, and batch committed-blob comparison; materialized hidden modifications still fail. Targeted regression/runtime Delivery pass after correction. Final full checks and commit receipts belong in `bench/evidence/bench005-20261103/`. Native uncapped admission is already implemented and validated; no fake infinite caps, account/provider changes, main merge or publication.

`DSMLConformance` independently inspected the captured raw responses and all resolvable BENCH004 attempts, made no edits and ran no tests. Exactly two captured closing-DSML text-only stops matched the existing guard; True-Myth's ordinary stop is outside that guard. No concrete observed false-positive/negative was found. The parent's actual adapter replay is the test evidence, not the reviewer's source inspection. Original benchmark classifications and scores are unchanged.

Durable evidence: original `bench/evidence/task-delivery-20261103/` and new `bench/evidence/bench005-20261103/` acceptance/commit/build/real-Trial receipts. Workspace evidence and TODO are outside Core Git; this report is included with the candidate commit.
