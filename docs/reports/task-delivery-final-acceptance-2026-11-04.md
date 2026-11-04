# Delivery Contract 最终验收 - 2026-11-04

结论:**MERGED**。Delivery Contract 已随候选分支进入 Core `main`。合并后 test、race、build、vet 和无 Git 检查都已在 `main` 上实际运行并通过。全仓整合的记录见 `core-full-integration-2026-11-04.md`。

## 1. 范围与依据

- 审查对象:`wt/core-execution-retry-review`,分支 `bench/execution-lifecycle-candidate`,基线 `4786db1`。
- 读过的报告:Astra 实施报告、Opus 第一轮、Astra 独立复核、Opus 第二轮、Astra 第二轮复核。报告结论只作线索;代码、diff、测试和真实运行结果是本轮自行核实的。
- 没有运行付费 Benchmark,没有调用外部模型,也没有修改 TB2 任务、Task Image、Verifier、冻结 Cohort 或历史证据(`MANIFEST.sha256` 171/171 校验通过)。

## 2. 架构结论

- **Agent Loop 不参与交付验证。** `internal/agent` 的生产代码中没有出现 Delivery、`DeliveryResult` 或 Delivery 义务。旧的 `Loop.Delivery` 回调和"缺失交付时生成纠正提示"的逻辑已删除。
- **Agent 结果与交付结果相互独立。** `AgentResult.Status` 来自 `Loop.Run`。只有执行状态为 `completed` 时,才在 owned jobs 停止后调用 `settleDelivery`。交付验证失败、不可用、panic 或持久化失败,都只体现在 `DeliveryResult` 中,不会改变 session 的执行状态,也不会引起重跑或工具重放。
- **重验只做验证。** `VerifyDelivery` 先持有 run 锁和 workspace 锁,先写 pending 再验证,随后只写 Task State。它不调用 Loop,不发模型请求,不执行工具。

## 3. F1–F8 复核

| ID | 结论 | 依据 |
|---|---|---|
| F1 重验保存失败后旧 `verified` 仍有效 | 已修复 | 重验先写 pending 并撤销由 evidence 得出的 done;pending 写失败时直接返回错误。`TestDeliveryRetrySaveFailureSupersedesVerified`、`TestDeliveryRetryStateReadFailureRetainsPending` |
| F2 宿主决议被自动完成覆盖 | 已修复 | 结算时先应用 inbox、再判断 AutoDone,保存时同时消费 cursor。另有测试在 pending 提交边界注入 dropped 并让保存失败,用户决议保留。`TestDeliveryQueuedHostResolutionWinsOverAutoDone`、`TestDeliveryHostEditAtRetryBoundarySurvivesFailedSave` |
| F3 越界悬空链接被当作"已删除" | 已修复 | 路径逐个分量解析,每一步都检查是否仍在工作区内(另见 N1–N3、N5) |
| F4 `Absent+Commit` 遇到 ENOTDIR | 已修复 | Git 检查使用经 confinement 确认的原生"不存在"结果 |
| F5 无契约任务凭空出现 Delivery | 已修复 | 契约是否存在只看 Start 记录(`preparedRun.delivery`) |
| F6 异步运行不发通知 | 已修复 | 所有出口都发出 `delivery.result`,且先于 `run.stopped` |
| F7 非法分支名被接受 | 已修复 | 用 Go 实现 `check-ref-format --branch` 规则;与真实 git 对照,3000 个随机名和 938 个确定性名称均无差异 |
| F8 相同问题重复出现 | 已修复 | 只去重完全相同的 phase/path/reason;原因、路径或阶段不同的条目都保留 |

## 4. 文件系统安全问题

| ID | 问题 | 结论 |
|---|---|---|
| N1 | 绝对 symlink 目标提前 `Clean`,`x/..` 没有先访问 x | Astra 已修复。代码核实:`deliveryAbsoluteTarget` 只解析已存在的前缀,其余路径原样交给逐级解析器;`TestDeliveryAbsoluteLinkPreservesTraversal` 通过 |
| N2 | 合法的工作区别名被误判为越界 | Astra 已修复绝对路径形式;`TestDeliveryAbsoluteLinkThroughWorkspaceAlias` 通过。本轮发现相对路径形式仍有同类问题,即 N5 |
| N3 | 读取期间祖先链接被替换,仍采用旧文件证据 | Astra 已修复。哈希完成后重新解析**声明路径**,比较 identity、size、mode、mtime;`TestDeliverySnapshotRejectsRetargetedAncestor` 通过 |
| **N5(本轮新发现,P2)** | 相对链接经由工作区的父目录重新进入工作区(例如 `link -> ../actual/real` 或 `../<别名>/real`),物理上仍在工作区内,却被判为 `verification_unavailable`。这与 N2 的绝对路径写法结果不一致 | 修复前 `TestDeliveryRelativeLinkThroughWorkspaceParent` 失败(`delivery path resolves outside workspace`)。修复:在根目录遇到 `..` 时,把剩余路径交给与绝对目标相同的 confinement 逻辑处理。真正越界的情况,以及外部、不存在的目标,仍然返回 unavailable。修复后该测试和 Delivery 全部测试通过 |

## 5. 状态机与恢复

- **Agent 状态与 Delivery 状态的组合:**
  - 无契约:没有 Delivery 字段。
  - 有契约且 Agent 未完成:返回 Start 写入的 pending。
  - Agent completed:结果为 `verified`、`failed` 或 `verification_unavailable`;如果持久化失败,存储中保留本次的 pending。
- **completed + pending(崩溃发生在完成与验证之间):** 保留手动 `delivery-verify`,不会自动重跑。`TestDeliveryCompletionCrashWindowDoesNotReplayAgent` 通过。
- **子任务:** 已完成但未验证的子任务,仍是父任务的 `tasks` 义务;重启后义务还在;子任务重验通过后义务解除,子 Agent 没有重跑。对应 `TestDeliveryChildFailureObligationSurvivesRestart`。
- **MaxSteps、Deadline、Cancel、Provider Retry:** 生产代码只改了 Delivery 相关路径。对 N4 的审查结论:`TestChildDeadlineInheritsAndCannotExtendParent` 只把 Engine 初始化移到截止时间之前,并新增"子任务截止时间必须严格等于父任务"的断言,没有放宽任何原有断言。`main` 上与这些语义相关的 125 个 runtime 测试全部通过。

## 6. 验证结果

- **候选 worktree:**
  - 修复后先 `go test -race -count=1 ./...`,再 `go test -count=1 ./...`,两组退出码均为 0,各 32 个包通过、3 个包没有测试。
  - `build` 和 `vet` 通过。
- **Core `main`(`f9d2e6b`):**
  - race 和 full 两组退出码均为 0,各 32 个包通过、3 个包没有测试,没有 DATA RACE;runtime 包分别用时 307 s 和 216 s。
  - `go build ./...` 和 `go vet ./...` 通过。
- **无 Git 真实 CLI**(本机回环假上游,provider 是测试替身,文件、bash 和 SQLite 都是真实的):

| 平台 | 用户 | Git | 执行 / 交付 | 模型请求 | 重验额外请求 | 非幂等标记 |
|---|---|---|---|---:|---:|---|
| macOS arm64 | 501 | 不在 PATH | completed / verified(modified、created、deleted) | 2 | 0 | 仅一次 ONCE |
| Linux arm64 | 65534 | 镜像中没有 Git | completed / verified(同上) | 2 | 0 | 仅一次 ONCE |

  两个平台的重验步骤相同:
  1. 先让交付文件不可读,重验得到 `verification_unavailable`,并带 path/phase 信息。
  2. 恢复权限后再重验,得到 `verified`,RunID 不变。
  3. 整个过程中 session 一直是 `completed`。

- **Linux 非 root 测试:**
  - 有 Git 的镜像:repository、taskstate、runtime 的 Delivery 测试全部通过。
  - 无 Git 的镜像:除两个需要用 git 搭建测试环境的 fixture(`TestDeliveryCommitOnlySparseAndLargeIndex`、`TestDeliveryCommitAndBranchAreExplicit`,在 `git init` 处失败,不是产品失败)外,全部通过。这两个测试在有 Git 的镜像中通过。

## 7. 剩余风险

- 没有多文件原子快照;路径检查与 open 之间仍有系统调用间隙。阻塞型文件系统 I/O 不能保证严格的 wall-clock 上限。数据库完全不可写时,错误无法落盘,只能通过返回值和事件报告。
- 从别名进入工作区的 symlink 能通过验证,前提是该别名在验证时仍然存在。
- 本轮没有验证 Windows,也没有在 Delivery 结算窗口内做真实 SIGKILL,也没有运行 TB2 官方 Verifier。本报告不对 Benchmark Reward 作任何声明。

## 8. 三类结论

- 代码实现:通过。
- 架构审查:通过。
- Benchmark 验证:未执行,不声称通过。
