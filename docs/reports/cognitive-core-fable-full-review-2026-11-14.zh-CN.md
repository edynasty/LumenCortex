# Cognitive Core 独立完整审查与持续修复（2026-11-14）

状态：**已审查；17 项 Finding 已修复并自验证；本地审查候选已冻结。** 未合并、未推送、未打 Tag、未发布。

审查者与修复负责人：Fable 5.2（`local-anthropic/claude-fable-5-2`），单一实例，未使用 Subagent，未使用其他模型。这些修复只由修复者本人验证过，因此本报告不给出独立 PASS（工作区规则：Finding Owner 不能给自己签发独立 PASS）。工程完整性不等于已证明长期自主学习收益；该收益未测量。

English: [cognitive-core-fable-full-review-2026-11-14.md](cognitive-core-fable-full-review-2026-11-14.md)。Finding Ledger：[finding-ledger.md](../cognitive-core/finding-ledger.md#fable-full-review-2026-11-14-ffr)。

## 1. 基线与提交关系

| 引用 | SHA | 角色 |
|---|---|---|
| Core `main` | `00344b5d6a67673fea9fcbc90e0cab16de17dc57` | 受保护基线；未修改 |
| 候选代码基线 | `949b2c6533dcf66f1401d0b72c5d2c44cb560354` | 候选最后一个生产/测试代码提交 |
| 冻结候选 | `c1d322938942c9bb4a82f184d1624103b92a5bf4` | `feat/cognitive-core-final-completion-20261113` 的分支头（在 `949b2c6` 之上的文档冻结） |
| 审查最终代码 | `c6bdfaf066f9c9fab7be824523471fa1b0bdcf92` | 本次审查最后一个 Go 改动；第 8 节的全部套件都在它上面运行 |
| 审查冻结头 | 见证据目录 `final/GIT_STATE.txt` | 在 `c6bdfaf` 之上加入本报告与 Ledger；Go 文件无差异 |

`main` 是候选的 merge base，候选领先 5 个提交（`2151f17`、`445e667`、`eb47a7d`、`949b2c6`、`c1d3229`）。审查分支 `review/cognitive-core-fable-full-review-20261114`（Worktree `wt/core-cognitive-core-fable-full-review-20261114`）从 `c1d3229` 开始，新增 18 个提交（17 个修复、1 个仅测试）和文档。没有重写历史；`main`、候选分支和其他所有 Worktree 均未改动。

规模：候选相对 `main`，190 个文件，+24353/−1073，其中生产 Go 代码 63 个文件，+9955/−469。审查相对候选（`c1d3229..c6bdfaf`）：生产 Go 代码 19 个文件，+418/−44；新增 22 个测试文件；调整 1 个已有测试文件（+16/−3：一个 Store 签名变化和新增一个用例）。没有删除测试，没有新增 Skip。没有新增或修改 Migration。

## 2. 范围与方法

- 完整阅读了 `main..c1d3229` 的全部生产 Diff，而不是只看最新提交，并覆盖它调用的继承代码：Session Store、Fence 与隐私清除、Evaluator、Agent Loop 与 Callback Dispatcher、Migration 0014–0017 及其 down 脚本。
- 每个疑似缺陷都写成一个具体反例：一个 Go 测试，通过公开 `Engine` API（或 Store 边界）在真实 CGO SQLite Store 上用脚本化 Provider 驱动。先在未修复代码上跑红（每项 Finding 都在 `c1d3229` 的原始 `git archive` 副本上跑红，17 项中的 15 项还在修复前的审查分支头上跑红），再修复根因，跑绿，并重跑受影响的包。没有反例的怀疑不算 Finding。
- 不把任何历史结论当作结论：Astra 的报告、其他审查者的关闭记录、本审查者自己以前的 PASS 都只是输入。十类历史缺陷用新的探针重新推导（第 4 节）。
- 没有付费模型请求，没有碰生产数据库，没有改 Desktop，没有 Merge、Push、Tag 或 Release。

## 3. Findings

17 项都能在原始 `c1d3229` 上复现。每项的细节、根因、回归测试和回执见 Ledger。

| ID | 级别 | 领域 | 候选上的缺陷 | 修复 |
|---|---|---|---|---|
| FFR-001 | P1 | Learning、Verification | 消费者的动作失败，但产物事先已存在，被记录为成功复用。 | `cee114a` |
| FFR-002 | P2 | Learning | 所属 Run 未结算就结束的采纳，永远无法关闭。 | `22601ed` |
| FFR-003 | P1 | Learning、Belief Revision | 来源线程继续之后，消费者的结果（包括反例）永远无法记录。 | `5fb8bfb` |
| FFR-004 | P2 | Memory、Governance | 反例无法结算到已归档的记录上；已归档的反例无法再管理。 | `c27f7ad` |
| FFR-005 | P2 | Reasoning、Belief Revision | 尚未生效的记录被评估为过期。 | `24e7038` |
| FFR-006 | P3 | 恢复 | 每次打开 Engine 都为一个只是“还没到期”的采纳报告恢复失败。 | `6337bef` |
| FFR-007 | P2 | Learning、并发 | 共享记录被并发修订时，消费者已检查的结果丢失。 | `e443e8f` |
| FFR-008 | P1 | Learning、Verification | 来源动作失败的过程被验证为正向经验。 | `129c2e5` |
| FFR-009 | P2 | Learning、并发 | 记录用满时的并发竞争下，结算放弃，Use 保持未关闭。 | `9e491f8` |
| FFR-010 | P3 | 恢复 | 每次打开 Engine 都为一个已被 Fence 的消费者报告恢复失败。 | `7b75581` |
| FFR-011 | P3 | Attention、Memory | Use 次数已用尽的记录仍被召回为可采纳。 | `c73e8a8` |
| FFR-012 | P2 | Privacy、Belief Revision | 删除一个消费者后，已失效的身份可以被重新提取为新记录。 | `b8bd7ba` |
| FFR-013 | P3 | Attention | 替换了工具的应用被拒绝，但提示语是“来源不可复现”的那一条。 | `b27823b` |
| FFR-014 | P2 | Privacy、Belief Revision | 删除记录的 Owner Session 后，已失效的身份可以被重新提取。 | `e0c9319` |
| FFR-015 | P2 | Learning | 子 Run 已应用的 Use、反例和提案在它结束时没有结算。 | `a9250c3` |
| FFR-016 | P1 | Learning、Verification | Use 的结果在 Run 结束时或更晚才检查，之后发生的变化被算到过程头上，或被当作它的反例。 | `d4fde6d` |
| FFR-017 | P2 | Learning、Memory | 记录接受了 64 KiB 放不下其反馈的 Use：已应用的 Use 永远无法记录结果，被 Use 写满的记录无法撤销。 | `c6bdfaf` |

4 项 P1，9 项 P2，4 项 P3。17 项中有 16 项涉及 Experience 记录（FFR-005 除外），其中 12 项位于“采纳”到“反馈”之间。候选的测试覆盖的是：一个消费者、一条小记录、一个静止的顶层 Run。缺陷都出在这个假设不成立的地方：一条记录有多个消费者（FFR-007、FFR-009）、来源线程继续工作（FFR-003）、采纳与结算之间发生宿主生命周期操作（FFR-004）、子 Run（FFR-015）、恢复（FFR-002、FFR-006、FFR-010）、删除与身份（FFR-012、FFR-014）、记录被写满（FFR-011、FFR-017），以及一次通过的检查究竟证明了什么（FFR-001、FFR-008、FFR-016）。

## 4. 十类历史缺陷的重新推导

探针是一次性测试，以文本形式保存在证据目录（`probes/`）；发现缺陷的探针已变成上面的回归测试。

| # | 类别 | 候选上的结果 | 现状 |
|---|---|---|---|
| 1 | Experience 没有真实执行却被记录为成功使用 | 可用三种方式复现（FFR-001、FFR-008、FFR-016） | 已修复 |
| 2 | 越权写入或权限拒绝被算作成功 | 成立：被拒绝的动作留下 `unknown` 回执，Use 为 `unknown`、`used=false` | 成立 |
| 3 | `read_file` 等真实读取被错误识别 | 成立：读取是来源 Trace 中的步骤，消费者必须复现；只有拒绝提示语有误（FFR-013） | 成立，提示语已修 |
| 4 | 未来生效的知识被报告为过期 | 在 `assess_cognition` 中可复现（FFR-005） | 已修复 |
| 5 | 调用方可以指定失效时间 | 成立：`InvalidatedAt` 与 `VerifiedAt` 被拒绝，`RecordedAt` 由 Core 设置，失效不能被清除、回填或重新激活 | 成立 |
| 6 | 隐私删除后仍可读取旧知识 | 内容、复制的工具行和派生记录都已清除且不可读。但身份在删除之后可以复活（FFR-012、FFR-014） | 已修复 |
| 7 | Consumer 之间的反馈或所有权泄漏 | 成立：其他读者只看到状态和置信度，看不到 Uses，也不能写入别人的反馈 | 成立 |
| 8 | 来源撤销后旧请求仍能读写 | 成立：进行中的消费者 Run 以 `fenced` 失败；之后 8 条写入路径都被拒绝。只有恢复时的噪声（FFR-010） | 成立，噪声已修 |
| 9 | Fence、CAS 与并发提交竞态 | 竞争下结算丢失可复现（FFR-007、FFR-009） | 已修复 |
| 10 | Governor 接口存在但调用路径不可用 | 候选上成立：Analyze、Plan、Validate、Apply、归档、恢复、撤销和删除都能通过公开 API 在正常 SQLite Store 上工作（由 `runtime/governor_owned_lifecycle_review_test.go` 固定，`fde3bcd`）。`main` 上该类问题仍然存在，见 10.7 | 成立 |

## 5. 七个领域

**Memory（记忆）。** 工作记忆是 Task State 和 `task` 记录；情景记忆是来源 Trace（Run 的每一次执行，最多 16 步，超过则 Trace 被截断且无法验证）、检查结果以及 `evidence`/`negative` 记录；语义记忆是带类型的 Assertion；程序记忆是 Experience 记录。从情景到长期记忆的转换发生在 Run 的终止边界（`propose_experience` → 从真实 Run 记录提取 → 由宿主绑定的检查验证）和有界的维护过程（`MaintainCognition`）中。压缩不会丢证据：生命周期修订如果和前一版本相比，除了状态、记忆策略、记录时间和原因之外还有任何不同，就被拒绝（`CommitCognitiveLifecycle`）；合并只在 Assertion、证据、来源、依赖、正文和有效期都相同的记录之间被接受；版本不可变；反例、失败证据和安全记录受保护，不能归档。访问次数和最近访问按读者单独存储，与结果、证据分开，任何归档决定都不读取它们：`MaintainCognitiveMemory` 只归档已标记为 stale 或 dormant、未受保护、未被引用且属于自己的记录，重要度低的先归档；预算被受保护内容占满时报告压力，而不是为了凑数字去删除；Engine 不向 Governor 的分析提供访问统计（宿主可以自己传入，此时它在价值分数中最多占 10%，而这个分数只用来给已经是 stale 或 dormant、未受保护且至多有一条边的记录排序）。容量不足时只拒绝、不淘汰：每个 Node 都为宿主的生命周期决定留出空间，Experience 记录还为未结算 Use 的反馈留出空间，所以写满的记录仍可撤销，已应用的 Use 仍可结算（FFR-017）。发现的缺陷：FFR-004、FFR-011、FFR-012、FFR-014、FFR-017。

**Attention（注意力）。** 召回最多返回 8 个带 Scope 的定位符，并明确说明适用性不等于正确性；`read_thread` 携带有界的条件定位符和有争议的警告；顶层 Start 在有界的重新验证之后得到一行有界的定位符索引，从不包含 Claim 的文本。相关性是词法匹配。与任务用词不匹配的警告不会显示，这是一次注意力遗漏，不是安全漏洞：采纳和应用不依赖注意力，有争议的记录无论怎样被找到都不能采纳。发现的缺陷：FFR-011、FFR-013。

**Reasoning（推理）。** 记录带有认识论类别（fact、inference、hypothesis、decision、unknown、experience）。只有 Framework 的验证能产生 fact；宿主或模型的提案如果带证据、验证时间或类别 `fact`，会被拒绝。`assess_cognition` 针对记录的确切版本返回 current、unknown、stale、conflict 及原因，并给出 continue、gather_evidence、ask、defer、stop 之一；证据不足的 `continue` 仍然是 `gather_evidence`。决策连同它读取的确切版本一起保存。模型负责推理，Framework 约束状态和权限。发现的缺陷：FFR-005。

**Learning（经验学习）。** 闭环是：任务经历 → `propose_experience` → 从 Run 的真实记录提取 → 由宿主绑定的条件检查和结果检查验证 → 召回 → 消费者自己的条件检查通过后采纳 → `apply_experience` 经普通工具 Dispatcher 执行 → 最后一个动作结算时观察结果 → Run 结束时反馈 → 出现反例则进入 `needs_revision` → 修订定义，验证重置，Uses 保留 → 下一个消费者。本次审查之后，一次成功必须同时满足：完整且已结算的回执，执行身份由 Dispatcher 签发；每个动作的结算结果与来源步骤一致（FFR-001）；来源自己的动作没有失败（FFR-008）；结果由宿主绑定的检查在应用边界上观察到（FFR-016）；消费者 Run 正常完成。反例在以下情况下都能被记录，并且不会重放任何动作：并发竞争（FFR-007、FFR-009）、来源线程继续之后（FFR-003）、记录已归档（FFR-004）、子 Run（FFR-015）、记录被 Use 写满（FFR-017）、重启之后。保存、召回或总结在任何地方都不算学习：召回和采纳不产生任何成功记录。限制：只支持与来源动作完全一致的复用；一条记录一生最多接受 8 次 Use，记录较大时更少。

**Belief Revision（信念修订）。** 没有任何内容被覆盖：每次变化都是 CAS 下的一个新的不可变版本，历史在读者的 Scope 内仍可读取。来源变化使记录变为 stale；尚未生效的记录是 unknown（FFR-005）；失效不可变，失效的身份不能恢复，删除消费者或 Owner 再重新提取也不行（FFR-012、FFR-014）。依赖固定到确切版本，所以更新的前提会使适用性失效，而不是悄悄替换前提。Assertion 主题相同的记录之间的冲突会被报告，不按先后顺序自动解决。

**Governance（认知治理）。** 在候选上，Go Governor 可以通过公开 `Engine` API 在正常 SQLite Store 上使用：Analyze、Plan、Validate 和宿主 Apply（分层、归档自己拥有且未受保护的记录、合并等价证据），以及 `SetCognitiveLifecycle`（归档、恢复、撤销）和 `DeleteSession`。模型或 Curator 只提议；只有宿主能应用，必须给出确切的期望版本，批次要么全部成功要么全部不做。Branch、Promote 和 Epoch 提案保持为建议，因为它们会引入新的 Claim。没有任何模型工具能触达生命周期操作。

**Verification（证据验证）。** 证据是宿主批准的检查的合格结果，绑定到确切的 Assertion（kind、target、key 和 value）：它记录执行的 Session 和 Run、结果序号、检查定义摘要、批准 Epoch、工作区指纹、Scope 和时间；绑定或定义批准被撤销、或隐私 Epoch 越过它之后，它不再成立。没有执行的检查永远不是成功；模型置信度是单独的字段，从不作为证据。这个领域的三项 P1 都不是伪造证据，而是一次真实通过的检查被当成了它并未证明的事情的证据（FFR-001、FFR-008、FFR-016）。

## 6. 统一架构

各领域共用 `internal/knowledge` 中的一套契约，并由同一个 Store 执行：`Scope`（可见性与包含关系）、`Times`（观察、有效期、记录、验证、失效）、`Evidence`、带来源版本的 `SourceRef`、`NodeRef` 依赖、按 Session 的所有权、生命周期状态、带隐私 Epoch 的来源 Fence，以及 CAS 下的不可变版本。我没有发现两个权威之间相互矛盾的状态，没有发现绕过治理的公开 API（`PutCognitiveNode` 拒绝 Experience 记录、证据、验证时间和类别 `fact`），在候选上也没有发现只有接口而没有可达调用路径的能力。

还有一个结构性限制（FFR-D2）。存在两个记录族：维护器候选（`cognition_candidates`，通过 `read_knowledge` 和 Start 索引读取）和原生记录（`cognition_nodes`，供 `assess_cognition`、Governor 和 Experience 使用）。它们共用 Assertion 类型、宿主绑定、检查证据、Fence 和隐私清除，但没有共用同一套生命周期、冲突检测和读取路径。每个事实只存在于一个记录族里，而且任何一族在读者当前状态下没有通过宿主绑定的检查，就不能把 Claim 显示为 current，所以除非宿主批准了相互矛盾的检查，两族不会对已验证的 Claim 给出相反结论。两族之间未验证 Claim 的矛盾不会被报告为冲突，Governor 也不管理维护器候选。合并它们是 Schema 和 API 变更，不属于审查修复；最小的一步是把候选只读投影进评估，而不增加第二条写入路径。

复杂度：候选复用了同一个 Engine、Agent Loop、Store、Evaluator 和维护租约，没有增加服务、调度器或第二个 Agent。本次审查新增 418 行生产代码，没有新增抽象：一个回执字段、Node 上的一个空间计算和几个小谓词。

## 7. 工程审查

- **真实 SQLite 路径。** 每个回归测试都通过 `Engine` 在临时状态目录中的 CGO `mattn/go-sqlite3` Store 上运行；Provider 以下没有任何 Mock。
- **Migration。** 阅读了 0014–0017 及其 down 脚本；0017 的 down guard 会拒绝会留下孤立数据的降级。本次审查不改 Schema。FFR-016 在应用回执里增加一个可选 JSON 字段；没有该字段的回执结算为 `unknown`。FFR-017 不改变存储格式：它只收紧内容修订可以写满的范围（Node 64 KiB 中的 60 KiB）以及记录何时还能接受 Use。
- **事务、CAS、Lease、Fence。** 准入、结算、生命周期批次和隐私删除都是单个写事务，提交时重新检查版本和 Fence。共享记录的结算在良性冲突后重新计算，次数上限按“用满的记录的写入者数量”设定（FFR-007、FFR-009）。继承的跨进程 Fence 守卫（F05）与其历史修复逐字节相同，并且仍能检出该缺陷：在临时副本中恢复被 `44bcda9` 移除的 Fence 读取后，确定性守卫测试失败（`probes/f05-mutant-c6bdfaf.*`）。
- **Consumer 隔离。** 读取和变更的返回值只投影读者自己的 Uses；写入别人的反馈被拒绝。
- **隐私删除与来源撤销。** 来源内容、复制的工具行、派生记录和回执都被清除；进行中的消费者以 `fenced` 失败；任何删除之后身份都保持撤回状态（FFR-012、FFR-014）。
- **重启与恢复。** 恢复过程在 Session 的 Run 锁下结算，把未完成的应用标记为 unknown，从不重放动作；不再为未到期的 Use 或已被 Fence 的 Session 报告失败（FFR-006、FFR-010）；引用的是应用当时观察到的结果，而不是恢复时工作区的样子（FFR-016）。
- **取消与错误处理。** Run 被取消后的结算使用有界的脱离上下文；记忆侧的错误通过 `AgentResult.Cognition` 报告，从不改变任务结果。
- **资源限制。** 每条记录 8 次 Use、3 个条件，Trace 16 步，8 个检查，每个节点 64 KiB（其中每次内容修订都要为生命周期修订留出 4 KiB，并为每个未结算 Use 留出其反馈所需的字节，FFR-017），每个回执 32 KiB，每个生命周期批次 512 条记录。

## 8. 验证

下表中的每一项都由本审查者在本次任务中、在所列提交上运行。PASS、FAIL、SKIP 是 `go test -json` 的测试事件数。

| 检查 | 提交 | 结果 |
|---|---|---|
| `go build ./...` | `c1d3229` | exit 0 |
| `go vet ./...` | `c1d3229` | exit 0 |
| `go test -count=1 ./...` | `c1d3229` | 2930 PASS / 0 FAIL / 46 SKIP；43 个包通过，6 个包没有测试 |
| `go test -count=1 ./...` 和 `go test -race -count=1 ./...` | `d4fde6d`（FFR-017 之前的头；已被取代） | 各 2978 PASS / 0 FAIL / 46 SKIP，exit 0（`final/superseded-d4fde6d/`） |
| `go build ./...` | `c6bdfaf` | exit 0 |
| `go vet ./...` | `c6bdfaf` | exit 0 |
| 对审查改动过的每个 Go 文件运行 `gofmt -l` | `c6bdfaf` | 无差异 |
| `go test -count=1 ./...` | `c6bdfaf` | 2985 PASS / 0 FAIL / 46 SKIP；43 个包通过，6 个包没有测试；exit 0 |
| `go test -race -count=1 ./...` | `c6bdfaf` | 2985 PASS / 0 FAIL / 46 SKIP；43 个包通过，6 个包没有测试；exit 0；未报告数据竞争 |
| Migration 测试（包 `internal/migrate`） | `c6bdfaf` | 150 PASS / 0 FAIL / 2 SKIP，普通与 Race 相同 |
| 隐私与来源隔离测试（完整运行中按名称统计的子集） | `c6bdfaf` | 213 PASS / 0 FAIL / 4 SKIP，普通与 Race 相同 |
| 重启与恢复测试（完整运行中按名称统计的子集） | `c6bdfaf` | 106 PASS / 0 FAIL / 6 SKIP，普通与 Race 相同 |
| 审查回归测试，22 个文件 | `c6bdfaf` | 34 个顶层测试，含子测试共 54 个：普通与 Race 全部 PASS |
| 每个回归测试的红色运行 | 17 项都在原始 `c1d3229` 上；其中 15 项还在修复前的审查分支头上 | 每次都是 exit 1（`findings/FFR-0NN/`） |
| 继承的 Fence 守卫（F05）的反向对照 | `c6bdfaf`，在临时副本中恢复 `44bcda9` 之前的 Fence 读取 | 确定性守卫测试失败，exit 1（`probes/f05-mutant-c6bdfaf.*`）；未改动的代码上为绿 |
| 针对 linux/arm64、linux/amd64、windows/amd64 运行 `CGO_ENABLED=0 go vet ./...` | `c6bdfaf` | 各 exit 0：对全部包和测试文件的交叉类型检查，不是原生运行 |

46 个被跳过的测试，在 `c1d3229` 的基线运行和两次最终运行中是同一组。其中没有认知测试，本次审查也没有新增 Skip：15 个是辅助进程的入口，19 个需要通过环境变量指定真实的调试适配器或解释器，4 个需要 Linux（PID namespace、cgroup、bubblewrap），7 个是需要显式开启的探测、回放、测量、实验和工具，1 个是不适用的用例（非流式响应没有 `[DONE]`）。原因和数量见 `final/full-test-c6bdfaf.summary.txt`。

不是本审查者运行的、只引自候选自己报告的内容：早期提交的 Linux ARM64 运行，以及历史审查的关闭记录。它们不能说明 `c6bdfaf` 的任何情况。

## 9. 平台限制

原生运行环境：macOS 27.0.1，Apple M1 Pro（arm64），Go 1.26.5，CGO 开启，`go-sqlite3` v1.14.49。`c6bdfaf` 没有在 Linux 和 Windows 上原生运行。针对 linux/arm64、linux/amd64、windows/amd64 对全部包和测试文件做的交叉类型检查通过（第 8 节）；那里没有链接或运行任何东西，它不是原生运行。Windows 的 Worker 沙箱仍不支持（`evalruntime/signal_windows.go`）。没有调用真实模型：Provider 是脚本化的，所以模型一侧的行为（模型是否会选择召回、采纳和应用）没有测量。

## 10. 明确回答

1. **七大领域是否真正协同？** 对原生记录和 Experience，是：同一套 Scope、Time、Evidence、Provenance、Lifecycle、Privacy 和 Revision 契约由同一个 Store 执行，一次 Use 在同一条记录上依次经过注意力、验证、学习、信念修订和治理。有一个限制：维护器候选是同一证据与隐私权威之下的平行记录族（FFR-D2）。
2. **经验学习闭环是否正确？** 在候选上，不正确：虚假的成功使用、被错误验证的经验、错误的反例和丢失的反例都可以复现（涉及 Experience 记录的有 4 项 P1、8 项 P2）。在审查分支头上，对我能构造出的每一种情况，闭环都是正确的。仍然存在并已记录的：忠实应用之前就已成立的结果仍然算成功（FFR-D1）；只支持动作完全一致的复用；每条记录最多 8 次 Use。
3. **记忆管理是否统一？** 对原生记录，是：一套生命周期、一条保护规则、一个容量过程，按读者的访问统计与结果分开。维护器候选有自己的适用性生命周期（FFR-D2）。
4. **Governor 是否正常可用？** 在候选和审查分支头上，可用：通过公开 API 在正常 SQLite Store 上工作，并由测试固定。在 `main` 上不可用（10.7）。
5. **是否存在架构级问题？** 没有阻断性的。有一个结构性限制（两个记录族，FFR-D2）和一个规模上限（Uses 存在记录内部，FFR-D3），都已记录，等待 Owner 决策。
6. **是否存在未修复的 P0/P1/P2？** 已知没有。17 项 Finding 已修复并自验证；3 项 P3 观察已连同所需决策一起记录。
7. **哪些能力已经在 `main`？** Harness、Task State、Session 与 Notes、Context Graph 与线程召回、检查批准与绑定（ADR-0017）、来源 Fence 与隐私删除（ADR-0018），以及 Governor 的 Analyze/Plan/Validate 入口。这些入口在 `main` 上无法工作：`CognitiveGraphSnapshot` 需要 `graph_nodes` 和 `graph_edges` 两张表，而没有任何 Go Migration 创建它们，Go 代码也不写入它们（Migration 层把它们列为遗留的 Node 运行时表）。在 `00344b5` 的 `git archive` 副本上做了探测（`probes/main-governor-00344b5.stdout`）：在只由 Go 创建的 Store 上，三个入口都返回 `cognitive graph tables are not initialized in shared SQLite store`；`main` 自己的 Governor 测试之所以通过，是因为测试手工创建了这两张表。
8. **哪些能力仍然只在候选中？** `internal/knowledge`；Migration 0014–0017；维护器与 `read_knowledge`；原生认知图；Experience 的提取、验证、召回、采纳、应用、反馈和修订；`assess_cognition`；Governor Apply 与记忆生命周期；五个认知工具；以及本次审查修复的全部内容。
9. **是否具备最终整合条件？** 可以进入下一道关口，但不能自我认证。条件：（a）由没有参与这些修复的审查者验证 18 个审查提交：17 个修复提交（合计新增 418 行生产代码，每个都有红、绿回执）和 1 个只含测试的提交；（b）Owner 决定是否合并；（c）如果 Linux 是发布平台，需要在 Linux 上原生运行 `c6bdfaf`。应当整合的基础是审查分支头，而不是 `c1d3229`：冻结时的候选有 4 项 P1 缺陷。

## 11. 已记录、未修复

- **FFR-D1**（P3，需要产品决策）。忠实复现的应用之前就已成立的结果，会被记录为成功。如果要求“未成立 → 成立”的转变，就会排除“测试仍然通过”这类守护型结果。统计信息只作参考，并标记为 `causal_benefit_unmeasured`。
- **FFR-D2**（P3，架构）。两个记录族，见第 6 节。
- **FFR-D3**（P3，规模）。每条记录一生最多 8 次 Use（记录较大时更少）、每个节点 64 KiB，这是设计上的选择（ADR-0023）。FFR-017 让字节上限变得安全，但没有解除它；要解除就得把 Uses 移到独立的表，属于 Schema 变更。
- `gofmt -l .` 列出 17 个文件（`final/gofmt-whole-tree.txt`）。这 17 个文件与 `main` 逐字节相同：差异是继承来的，不是候选或本次审查引入的，保持原样。
- 结果延迟出现的检查必须自己等待：结果只在最后一个动作结算时观察一次。

## 12. 证据

工作区 `bench/evidence/cognitive-core-fable-full-review-20261114/`（在 Core Git 之外，不受版本控制）：

- `baseline/`：`c1d3229` 上的 build、vet 和完整测试。
- `findings/FFR-0NN/`：每项 Finding 的红色和绿色运行；`commits/`：每个修复提交一份记录。
- `affected/`：审查过程中受影响包的套件运行。
- `probes/`：各类探针的文本及其输出。
- `final/`：冻结头上的环境、命令、build、vet、完整测试、完整 Race、分类计数和 Git 状态；`final/superseded-d4fde6d/`：FFR-017 之前那个头的同一套验证，按运行时原样保留。
- `SHA256SUMS`：以上所有文件的摘要。
