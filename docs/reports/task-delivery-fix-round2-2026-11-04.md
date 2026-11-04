# Delivery Contract 第二轮修复 - 2026-11-04

- 工作树 `wt/core-execution-retry-review`,分支 `bench/execution-lifecycle-candidate`,HEAD `4786db1`(无新提交)。依据:Astra 实施报告 `task-delivery-2026-11-03.md`、Opus 第一轮报告 `task-delivery-review-2026-11-04.md`、Astra 独立复核 `task-delivery-independent-review-2026-11-04.md`(三者均未改写)。
- 本轮没有 Commit/Merge/Rebase/Reset,没有改 Core main、TB2 题目/Image/Verifier/评分器、冻结 Cohort 和历史证据,也没有付费请求或外部模型调用。原有未提交修改全部保留。
- 实施者为本会话(Opus),本报告不是独立审查 PASS。

## 1. 缺陷清单与根因

| ID | 缺陷 | 根因 | 修复(文件) |
|---|---|---|---|
| F1 | 重验保存失败后,旧 `verified` 仍解除义务,`read_thread` 仍显示 done | 只有 Start 先写 pending;`VerifyDelivery` 直接验证,保存失败只改了返回值,存储里还是上一次的 verified | `runtime/delivery.go` `VerifyDelivery`:在同一把锁下先写入 pending,并撤销由 evidence 得出的 done(与 Start 一致)。pending 写不进去就返回错误,不做验证。之后结算写失败时,存储里留下的是本次的 pending |
| F2 | 宿主排队的 dropped/done 被自动完成覆盖 | 结算只读已存储的状态,不应用 inbox 就调用 `AutoDone`,并以 `InboxThrough=0` 保存 | `runtime/delivery.go` `verifyDelivery`:先应用 inbox,再做 Reopen/AutoDone;保存时同时消费 cursor 并写入 `HostStateEdges`。顺序与 `latchEvidence`、Start 相同 |
| F3 | 指向工作区外的悬空符号链接祖先被当成"文件不存在",结果 verified | 祖先目录用 `EvalSymlinks` 解析失败(ENOENT)后按字面回退到父目录,已遇到的越界链接目标就被丢掉了 | `internal/repository/delivery.go` `resolveDelivery`:从真实根目录开始逐个路径分量解析,跟随每个中间符号链接,并逐步检查是否仍在工作区内。只有在工作区内的不存在/ENOTDIR 才算 absent;越界、`..` 越过根目录、绝对路径目标不在真实根下、链接循环(超过 40 次)都返回 unavailable。最终分量的链接本身算"存在"(kind=symlink) |
| F4 | `Absent+Commit` 遇到 ENOTDIR(目录被同名文件替换)时为 unavailable | 提交检查走 `SnapshotFiles`,它只把 ENOENT 当作 BlobAbsent | `deliveryGitMissing` 接收原生快照经 confinement 确认的结果:已确认不存在的路径直接记为 BlobAbsent,只对存在的文件计算 Git blob(`deliveryBlobs`)。共享的 `SnapshotFiles` 没有改 |
| F5 | 无契约任务在状态读取失败时凭空出现 delivery | `recordedDelivery` 用事后读取的结果推断契约是否存在,读取失败时制造出 unavailable | `startTaskState` 返回 Start 写入的 pending 记录,保存在 `preparedRun.delivery`。没有契约就不结算、不读取;有契约但 Agent 未完成,返回 Start 的 pending。删除了 `recordedDelivery` |
| F6 | 异步运行(Supervisor)结算时状态读失败,不发 `delivery.result` | 结算函数提前 return,跳过了事件发布;Supervisor 又丢弃了 execute 的返回值 | `settleDelivery` 统一包装 `verifyDelivery`,所有出口都发布 `delivery.result`。Agent Result 不变 |
| F7 | 非法分支名(如 `bad..branch`)被接受 | 去掉 Git 依赖时一并删掉了 `check-ref-format` 检查 | `internal/taskstate/delivery.go` `validBranchName`:用 Go 实现 `git check-ref-format --branch` 规则,在 `Validate`(Start/CLI 解析阶段)就拒绝,与是否安装 Git 无关 |
| F8 | 通用基线原因按 changed 文件重复出现 | 每个 changed 文件都追加一次通用 issue | `repository.VerifyDelivery` 按 phase/path/reason 全量去重(保持顺序);逐文件的 `unknown` 证据保留 |

关联发现(报告未列出):
- F4 修复需要把原生观察结果传给 Git 检查;否则 F3 的 confinement 只覆盖普通文件路径,Commit 路径仍会走 Git 自己的解析,两边口径不一致。
- 对照 git 后发现 `@` 是合法分支名(`git branch @` 成功),因此不拒绝。

## 2. 修复前失败的证据

- Repository 和 TaskState:在修复前的代码上直接运行新测试。
  - `outside_dangling_target`、`outside_dangling_relative`、`two-level_link_outside` 结果是 verified。
  - Commit+ENOTDIR 结果是 unavailable(`committed delivery files cannot be inspected`)。
  - 通用 issue 出现两次。
  - 22 个非法分支名全部被接受,Start 也接受了。
- Runtime:在仓库外的临时副本里恢复修复前的 `runtime/delivery.go` 和 `execute`,运行新测试,全部失败:
  - 保存失败后 TaskState 仍是 `verified`。
  - dropped/done(user) 仍排在队列里(`pending=1`),晚到的 dropped 之后 `read_thread=done`。
  - 无契约的 failed/completed 都出现 `verification_unavailable`。
  - 有契约且 failed 时被改写为 unavailable,不是 pending。
  - 同步和异步路径都没有发出 `delivery.result`(0 个)。
  - 临时副本已删除。
- 分支规则差异测试:临时测试拿 3000 个随机名与 `git check-ref-format --branch` 对照,0 处不一致。该文件已删除。

## 3. 新增回归测试

- `internal/repository/delivery_confinement_test.go`:
  - `TestDeliveryAbsenceRequiresConfinedPath` 覆盖:普通缺失、ENOTDIR、工作区内悬空链接、两级链接(内/外)、指向外部存在目标、外部悬空(绝对/相对)、循环、最终分量为悬空链接。
  - `TestDeliveryRegularFileBehindLinks`、`TestDeliveryCommittedDeletionBehindFileAncestor`(未提交时 failed,提交后 verified/deleted)、`TestDeliveryGeneralBaselineIssueReportedOnce`。
- `internal/taskstate/delivery_test.go`:`TestDeliveryBranchNameIsValidatedWithoutGit`(PATH 中没有 Git)。
- `runtime/delivery_settlement_test.go`(用 SQLite 触发器在测试自建的库中注入故障):
  - `TestDeliveryRetrySaveFailureSupersedesVerified`:
    - pending 写失败时重验报错且不验证。
    - 结算写失败时,返回 unavailable 并带 persistence issue,存储为 pending,义务里有 `delivery`,resolution 为 open,`read_thread` 不是 done。
    - 重启后仍是 pending;去掉触发器后重验得到 failed,RunID 不变。
    - 全程模型请求 1 次。
  - `TestDeliveryQueuedHostResolutionWinsOverAutoDone`:排队 dropped/done(user) 后连续重验两次,TaskState、`read_thread` 和存储一致,`PendingEdits=0`;边界后才到的 dropped 由下一次结算应用,没有被覆盖。
  - `TestDeliveryStateReadFailureFabricatesNothing`:有/无契约 × failed/completed 四种组合。
  - `TestDeliveryAsyncSettlementFailureIsPublished`:RunSupervisor 在 `run.stopped` 之前收到 unavailable/state 事件,session 仍是 completed,请求 1 次。
  - `TestDeliveryChildFailureObligationSurvivesRestart`:子任务交付失败,重启后父任务的 `tasks` 义务仍在;在重启后的引擎上重验子任务,义务解除;子 Agent 没有重跑。
- 原有测试没有删除,断言没有放宽。

## 4. 测试结果(最终代码)

- `go build ./...` 和 `go vet ./...` 通过。
- `go test -count=1 ./...` 退出码 0:32 个包通过,3 个包没有测试。runtime 203 s。
- `go test -race -count=1 ./...` 退出码 0:32 个包通过,3 个包没有测试,没有 DATA RACE。runtime 287 s。两组合计 527 s。
- 已有的 SIGKILL 工具矩阵、无 Git 生命周期、权限、恢复、Supervisor、Subagent、CLI 包测试都在以上全量运行中。
- 真实 CLI(macOS,本机回环假上游,无付费请求,共 3 次请求):
  - F3(悬空外链 + absent):completed / `verification_unavailable`(`delivery path resolves outside workspace`)。
  - F4(提交删除 + 目录被文件替换):Agent 运行后 failed(尚未提交);提交后 `delivery-verify` 结果为 verified/deleted。
  - F7:`invalid delivery branch "bad..branch"`,退出码 1,新增模型请求 0 次。
  - 无契约:输出中没有 delivery 字段;`delivery-verify` 退出码 1。
- F1/F2/F5/F6 需要注入数据库故障,由上面的 Runtime 测试经真实 Engine/Supervisor 覆盖,没有在 CLI 上重复。
- 未执行:Linux 无 Git 容器、Windows、TB2 镜像或 Verifier。

## 5. 状态机与恢复语义

- 契约是否存在只取决于 Start 的记录:
  - 无契约:没有 delivery。
  - 有契约且 Agent 未完成:返回 Start 的 pending。
  - Agent completed:结算为 verified / failed / unavailable;结算写失败时存储保留本次的 pending。
- 每次重验都先把上一次的结果作废(写 pending,撤销由 evidence 得出的 done),然后只验证、只写 Task State。不调用 Loop,不发模型请求,不执行工具。可以重复执行。
- 宿主决议的优先级:在本次读取前排队的编辑先应用,`AutoDone` 不会覆盖 done/dropped。读取之后才到的编辑,按既有队列语义由下一次 Start 或结算应用。在这段窗口里,`TaskState` 已经显示用户的决议,而 `read_thread` 显示上一次存储的结果,两者可能不同。这是宿主编辑原有的排队设计,本轮没有改变,也有测试证明用户决议最终不会被覆盖。
- completed + pending 的崩溃恢复:保留手动 `delivery-verify`,不在启动时自动重验。
- MaxSteps、Deadline、Cancel、Provider Retry 和恢复路径都没有改动。没有新增步数、请求或费用上限,也没有新增无进展检测。新增的 40 次链接展开上限只用于路径解析,相当于内核的 ELOOP 限制,不限制 Agent。

## 6. 仍存在的风险与证据缺口

- 绝对路径的符号链接,如果目标写成另一种拼写(例如 macOS 上 `/var/...` 对应真实根 `/private/var/...`),会判为 unavailable 而不是被接受。这是保守失败,不会误判 verified。
- 解析完成到打开文件之间存在 TOCTOU 窗口,由 `EvalSymlinks`/`SameFile`/元数据复核兜底。对外部并发写者无法提供原子的多文件快照。阻塞型文件系统 I/O 不受 context 严格限时。这些都是此前已记录的风险,本轮没有新增证据。
- 存储完全不可写时,只能通过返回值和事件报告,无法把错误落盘。
- 重验写入 pending 后,如果父任务在两次写入之间 Start,可能收到一条中间的 pending 通知,随后再收到最终结果。通知内容是真实状态,不是重复执行。
- Linux 容器和 TB2 环境本轮没有重跑。

## 7. 结论

- 是否有新增缺陷:没有发现。全量、Race、Build、Vet 通过,所有新反例在修复前失败、修复后通过。
- 代码实现:**PASS_WITH_FIXES**。F1–F8 已修复;第 6 节的风险和证据缺口仍然存在。
- 架构:Delivery 仍在 Loop 之外;Agent Result 与 Delivery Result 分离;宿主决议优先于自动完成。
- Benchmark 验证:没有执行,不对 Reward 作任何声明。
- 下一步:具备交给 Astra 做第二轮独立只读复核的条件。本轮结果不等于独立审查通过,也不代表可以合并 Core main。
