# Delivery Contract 第二轮审查与修复 — 2026-11-04

## 范围与基线

- 工作树 `wt/core-execution-retry-review`；分支 `bench/execution-lifecycle-candidate`，HEAD `4786db146f73244b35986619cc22b3cba2921e59`。起点是 Opus 第二轮后的未提交工作区，24 个 tracked 修改、8 个 untracked、无 staged 内容，不是裸 HEAD。
- 开始保留 984 个已有文件的内容与 SHA256；清单摘要 `7b6ff8e300220298a111720e4ae4e2f3072ed69d478f456a433393ab2ad093ea`。
- 本轮先审查/复现再修复；主会话同时实现和自检。复用的文件系统/生命周期 reviewer 为独立上下文的同模型辅助审查，不构成跨模型独立验收。本轮任何结论不授权合并，不替代后续独立复核。
- 不修改 main，不 commit/merge/rebase/reset，不修改 TB2/任务镜像/Verifier/cohort/历史证据，不运行付费请求或外部模型。原有正确实现与报告保留。

## F1–F8 独立复核

先执行原始反例对应测试，而不是直接采信 Opus 报告。命令：

```text
go test -count=1 -v ./internal/repository ./internal/taskstate ./runtime -run 'TestDelivery(RetrySaveFailureSupersedesVerified|QueuedHostResolutionWinsOverAutoDone|AbsenceRequiresConfinedPath|CommittedDeletionBehindFileAncestor|StateReadFailureFabricatesNothing|AsyncSettlementFailureIsPublished|BranchNameIsValidatedWithoutGit|GeneralBaselineIssueReportedOnce|ChildFailureObligationSurvivesRestart)'
```

三个包通过（0.875s / 0.610s / 1.854s）。第一轮报告中的失败是历史证据，当前 Opus 起点已经修复；本轮没有回退工作区制造“再次修复”的假象。

| 项目 | 本轮判断 | 证据与实际处理 |
|---|---|---|
| F1 旧 verified | 原有修复正确，保留 | pending 预写失败不检查；结算写失败留下 pending/open；重启保留 pending，恢复后 failed、RunID 不变、模型不重跑。新增 retry-start 后状态读取故障回归。 |
| F2 宿主决议 | 原有修复正确，保留 | inbox 在检查后读取、AutoDone 前应用；写失败不消费编辑。新增 pending 提交边界注入 dropped + 最终保存失败 + 恢复重验，投影/存储/ReadThread 最终一致。 |
| F3 symlink | 原始外链反例通过，但发现三个新 P2 并修复 | 绝对目标提前 Clean、合法根别名被拒、hash 后只检查旧 canonical target。详见 N1–N3。 |
| F4 Absent+Commit | 原有修复正确，保留 | ENOTDIR 产生的 native absent 传给 Git；未提交删除 failed，提交后 verified/deleted。macOS 与 Linux 定向回归均通过。 |
| F5 无契约伪造 | 原有修复正确，保留 | Start 已知 presence 保留在 preparedRun；无契约 completed/failed + 状态坏读都无 Delivery；有契约非 completed 保持 pending。 |
| F6 异步通知 | 原有修复正确，保留 | 错误出口经 settle 包装发事件，在 run.stopped 之前收到 unavailable/state，Agent 仍 completed。 |
| F7 branch | 原有修复正确，保留 | 无 Git 的非法名称表通过；另用真实 `git check-ref-format --branch` 对照 938 个确定性名称，0 差异（含控制字符、Unicode、@、HEAD、随机边界）。 |
| F8 去重 | 原有修复正确，补边界测试 | 相同 phase/path/reason 去重，不合并不同原因、路径或阶段；两文件 unknown 证据保留，顺序确定。 |

`cancelled` 是执行退出原因，不是新的 Thread Resolution 值；既有宿主 resolution 为 open/done/dropped。本轮不引入 cancelled resolution 别名，不把 Cancel 改成自动完成。

## 新发现问题、修复前后证据

### N1 / P2：绝对 symlink 目标的 Clean 改变真实路径语义

- 原位置：`internal/repository/delivery.go` 的 `resolveDelivery` absolute-target 分支。
- 根因：先 `filepath.Clean(target)` 把 `jump/..` 去掉，而文件系统必须先访问 jump，再处理 `..`。jump 可能是越界链接、普通文件或不存在，不能词法抵消。
- 本轮先新增 `TestDeliveryAbsoluteLinkPreservesTraversal`，绝对目标通过字符串拼接构造（不在 fixture 中先 Join/Clean）。修复前 outside/missing/file 三种情况全部错误 verified：期望分别 unavailable/failed/failed。真实 inside-link/parent 为正向控制。
- 修复：`deliveryAbsoluteTarget` 仅定位已经存在并解析到工作区的绝对前缀，原样保留尚未遍历的后缀；之后由逐级 resolver 处理 symlink、`.`、`..`、缺失、ENOTDIR 与边界。
- 修复后上述矩阵及原外链、loop、目录替换回归通过；没有特殊处理任何 Benchmark 路径。

### N2 / P2：合法绝对工作区别名被误判越界

- 复现：外部 alias → 实际工作区 R，R/link → alias/real；R/real/result 是普通文件。当前工作区和 target 的拼写不同，但物理上仍在 R。
- 修复前 `TestDeliveryAbsoluteLinkThroughWorkspaceAlias` 为 unavailable，期望 verified。macOS `/var` 与 `/private/var` 是自然实例，测试使用通用自建 alias，不依赖平台目录命名。
- 与 N1 同一个 helper 解决：按真实已存在前缀进入 canonical root，再保留 raw suffix。没有通过简单替换路径前缀绕过 confinement。
- 修复后 macOS 与 Linux 同一测试均通过。

### N3 / P2：读取期间祖先链接被替换仍采信旧文件

- 根因：Opus resolver 返回 canonical 文件路径，末尾 `Lstat(abs)` 也只看该旧目标；声明路径的 intermediate symlink 已被替换时仍可能返回旧 SHA256。
- 确定性复现：`current -> old`，打开 old/result 后在下一读取检查点原子 Rename 链接为 `current -> new`。fixture 通过 context.Err 的本地检查点触发，仅改变测试目录，不改生产代码插桩，不用 sleep。
- 修复前 `TestDeliverySnapshotRejectsRetargetedAncestor` 返回成功旧快照（SHA256 `cba06b...`、Size=3），测试失败。
- 修复：hash 完成后重新解析**声明路径**，比较当前目标与 opened 文件 identity、size、mode、mtime；不是仅 stat 旧 canonical target。
- 修复后测试确认发生了 retarget 且收到 `changed during hashing`，避免“任意错误都算通过”。原普通变更、权限、无 Git、大小/时间戳、Git 提交路径也回归通过。

### 实际变更范围

- 生产代码仅 `internal/repository/delivery.go`：absolute-target helper 与末尾声明路径复核。
- 新增 `internal/repository/delivery_path_semantics_test.go`：N1–N3 与正向控制。
- 新增 `internal/repository/delivery_issue_test.go`：F8 保留不同 phase/path/cause。
- 新增 `runtime/delivery_boundary_test.go`：排队编辑与失败结算、retry-start 后坏读、cancel/deadline/steps/retry/panic 的 pending 与 RunID。
- 更新 `docs/agent-runtime.md` 的路径行为与 IO 非原子边界；新增本报告。
- 未放宽或删除原有测试断言；N4 后对既有 deadline 测试只调整初始化/计时并增加精确继承断言。Opus runtime/taskstate/branch/dedup 正确实现原样保留。原先各份报告不修改。

## 架构与边界复核

- **Agent Loop** 没有重新引入 Delivery verifier/correction。新修复只影响文件证据读取，不更改 AgentStatus、模型请求、工具执行、MaxSteps/Deadline/Cancel/Provider Retry。
- **状态生命周期**：Start 写 pending；非 completed 返回其已知记录；completed 清理 owned jobs/children 后结算；VerifyDelivery 同锁先写 pending 再检查。新边界测试覆盖坏读与结算失败，确认不恢复旧 verified。
- **宿主决议**：已读取的 done/dropped 不被 AutoDone 覆盖，写入与 inbox cursor 消费原子。读取后才提交的编辑继续排队，是既有边界；不声称所有并发时刻两个读 API 必然相同。
- **父子任务**：completed+unverified 仍是 tasks 义务；重启和子重验解除路径通过。结果 cursor 以相同 snapshot 判重，pending→final 是证据更新，不重放子 Agent。
- **恢复**：completed+pending 保留显式 delivery-verify。没有新增启动时后台自动重跑；已有 marker、Resume 和 SIGKILL 工具矩阵由全量套件覆盖。
- **Git**：纯文件没有 Git 依赖。Commit 检查 literal Git 路径，不能把 symlink 下虚拟路径视为 Git 同名文件；native SHA256 不与 Git object ID 混比。既有 sparse/flagged-index 与 ENOTDIR 删除测试通过。
- **异常**：坏状态、保存失败仍与 execution 分离；异步 result 先于 run.stopped。存储完全不可写时不能凭空落盘，只返回/发事件；不伪造“已持久化”。
- **新上限**：没有 Agent 的隐藏步骤、token、请求、费用或无进展终止条件；既有路径 loop 限制与操作 context 不成为 Agent 控制逻辑。
- **辅助复核**：同模型非实现上下文针对当前 runtime F1/F2/F5/F6 给出无新 P1/P2；对 N1–N3 修复差异再次阅读后给出窄域通过。其没有运行测试；测试证据全部由本会话实际运行，不称独立全项目 PASS。

## 本轮实际验证

- 原始 F1–F8 定向命令三包通过，完整输出保留在会话工具记录。
- N1–N3 单独运行均有本轮失败前证据，修复后 `go test -count=1 ./internal/repository -run 'TestDelivery(AbsoluteLink|SnapshotRejects|AbsenceRequires|RegularFileBehind|CommittedDeletion)'` 通过。
- 新 runtime boundary 三组通过；后续最终名称为 `TestDeliveryHostEditAtRetryBoundarySurvivesFailedSave`、`TestDeliveryRetryStateReadFailureRetainsPending`、`TestDeliveryPendingAcrossExecutionExitKinds`。deadline/cancel 等 fixture 全部是本地内存 provider。
- Git branch 独立确定性 oracle 938 例零差异；临时 oracle 未加入生产代码或永久测试。

### 实际 CLI smoke（非 mock 文件系统/工具）

| 平台 | Git | 执行 / Delivery | 文件变化 | 本地 HTTP | 重验额外 HTTP | 非幂等 marker |
|---|---|---|---|---:|---:|---|
| macOS arm64 | PATH 不存在 | completed / verified | modified、created、deleted | 2 | 0 | ONCE |
| Linux arm64、非 root、禁网容器 | PATH 不存在 | completed / verified | modified、created、deleted | 2 | 0 | ONCE |

真实 CLI、真实 bash/文件与数据库；只有 provider 是同进程本机 loopback 假上游，绝无外部模型。重验是新的 CLI 进程，RunID 保持。没有测试原 TB2 Image/Verifier，不声称 Coq/Reward。

另在两个平台各执行 3 个**本次修复路径**的真实 CLI 场景（共额外 6 次 loopback HTTP、无外部请求）：absolute outside-link/parent → completed/unavailable；absolute missing/parent → completed/failed；absolute workspace alias → completed/verified。证明修复不只在 helper 单测生效，三种 Delivery 结果均不污染 Agent Completed。

### Linux 验证

- 已存在的 `golang:1.23`，Go 1.23.12/linux arm64/CGO/gcc；源码与 module cache 只读挂载，`GOPROXY=off GOSUMDB=off --network none`。只在临时目录写二进制/cache。
- 编译真实 Linux CLI 与 repository/runtime test binary 后，在 `--user 65534:65534 --network none` 容器运行两包全部 `TestDelivery`，全部 PASS，包括权限测试、F1–F6/F8、新 N1–N3/边界测试。F7 在 macOS taskstate/full test 与 938 例 Git oracle 验证。Git 专属测试在带 Git 的构建镜像内跑，普通无 Git CLI 在 `python:3-slim` 跑；二者区分。
- Docker VM 约 4GB；本轮单容器限 768MiB，构建限 1536MiB，不修改共享 Docker 设置，不启动 Trial、不 pull/修改任何 TB2 镜像。

### 全量门禁

- `go test -count=1 ./... && go build ./... && go vet ./...`：整条命令 exit 0，32 包通过、3 包无测试；runtime 222.959s，整组 246.43s。build/vet 同一成功命令链已执行，不引用 Opus 旧记录。
- 首次 `go test -race -count=1 ./...`：FAIL，327.84s。`runtime/TestChildDeadlineInheritsAndCannotExtendParent` 在 explore 的 Start 前报 `context deadline exceeded`，结果尚未建立、模型调用为 0；其他 31 个测试包通过。未把首次失败抹掉或当成通过。
- N4 修复后，最终独立串行命令 `go test -race -count=1 ./... && go test -count=1 ./... && go build ./... && go vet ./...` **exit 0**，整组 549.36s。Race 与普通测试各 32 包通过、3 包无测试；runtime 分别 300.478s / 216.274s，无 DATA RACE。生产代码与测试冻结后运行，只有报告继续记录。

## 剩余边界

- 最终 re-resolve 防止本轮已证实的 retarget 窗口，但不是多文件原子快照，也不是对任意外部敌对写者的 descriptor sandbox；路径检查与 open 之间仍有 syscall 间隔。不得声称已解决所有 TOCTOU。
- 同 inode 并发改写并恢复元数据、特殊阻塞文件系统读的严格 wall-clock 上界未实测。context 不能抢占所有内核 IO。本轮没有因此新增猜测性终止器或新服务。
- Linux 无 Git证据已补齐；Windows、本轮 Delivery 窗口真实 SIGKILL（不是已有工具 SIGKILL）及正式 TB2 verifier 未单独验证。
- 外部事件是通知，不替代存储；进程崩溃后的 pending 是准确状态，不要求假造 unavailable 原因或重放成功工具。

## 保全与交付检查

- 收尾按开始时的 984 文件快照核对：已有文件中只修改 `internal/repository/delivery.go`、`docs/agent-runtime.md`、`runtime/lifecycle_control_test.go`，其余 981 个内容保持不变，包含全部原报告、TB2 材料与原有未提交修复。
- 新增本轮三个测试文件及本报告。不把本来就被 Git ignore 的目录/数据库当成此次新建或擅自删除。
- 最终生产文件 SHA256：`4960044ebe052ca7d787ca8b74f9ecf32e34da0e53a455df8e4c978ee814779c`。无 commit、stage、merge、reset、main 修改或发布。
- 本轮临时 CLI、Linux test binaries、build cache、oracle module、smoke 脚本已全部删除；本地 HTTP 服务已 shutdown/join，容器全部 `--rm` 退出。保留永久回归测试与报告，不留运行服务或脚手架。

## 全量 Race 发现的邻接测试缺陷 N4

- `runtime/lifecycle_control_test.go` 原测试在 `notebookEngine`（含迁移/初始化）之前设置 150ms 截止时间，初始化时间能耗尽被测运行的整个 deadline。Race 实测出现准备拒绝而不是测试期望的模型调用后 interrupted。
- 修复测试夹具：先初始化 Engine，再建立 1s 的执行期限；补充子 options 的 deadline **精确等于**父值断言。保留原有两种子代理、真实 deadline 中断、cause 和恰好一次模型调用断言，不改任何生产 Deadline/Cancel 逻辑，不跳过/放宽状态验收。
- 这是测试环境与计时边界修正，不是 Delivery 功能缺陷；`go test -race -count=1 ./runtime -run '^TestChildDeadlineInheritsAndCannotExtendParent$'` 通过（4.122s）。辅助 reviewer 只阅读该差异，确认没有削弱原断言。修复后的全量四门禁已串行通过，避免先前并行验证的额外负载。


## 最终结论

**PASS_WITH_FIXES**

- 原始 F1–F8 逐项独立复核完成；保留 Opus 已正确的 F1/F2/F4/F5/F6/F7/F8，实现未重复改写。F3 深入检查发现 N1–N3 三个真实 P2，均有本轮失败前证据、实际代码修复及通过后回归。全量 Race 又暴露 N4 测试时序缺陷，修复后完整四门禁通过。
- 没有剩余已确认但未修复的本轮 P1/P2；上述多文件原子性、阻塞 IO、未执行平台/特定 crash 窗口继续明确标为边界，不伪装已全面解决。
- Agent 执行状态与 Delivery 分离，Git 不成为普通文件硬依赖，失败/重验不重放 Agent 或成功非幂等工具，MaxSteps/Deadline/Cancel/Provider Retry 的生产语义未改。
- Linux arm64 无 Git 实际 CLI 验证已补齐；无付费 Benchmark、无外部模型、无 TB2/cohort/历史证据修改。
- **本结论仅代表本轮审查、修复和自检完成，不是实现者给自己授予独立审查通过，不替代后续独立复核，不授权合并 Core main。** 后续复核应聚焦本轮实际差异与受影响路径，使用此报告中的失败前/通过后证据。
