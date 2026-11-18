# ADR-0024 Context Space Architecture — 完整实施与最终验收报告

- **实施日期：** 2026-11-18
- **权威架构规格：** `LumenCortex-architecture-audit/design/handoffs/context-space-architecture-master.zh-CN.md`（版本 `v0.4-rc3.5`）
- **规格 SHA256：** `bd8dfb00c9781d4d375ce4779789a71f0cbef9f547918ea1f4aa1a0c7481abd9`（实测一致）
- **冻结源码基线：** Core `c73d17359f5d9dd16ef88b0de4525862593fddaa`（实测一致）
- **隔离工作树与实施分支：** `/Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-context-space-20261118`（分支 `feat/adr-0024-context-space-20261118`，已推送 `origin/feat/adr-0024-context-space-20261118`，未合并 `main`）
- **最终代码候选提交：** `38e4fe0f1ab71a177d07817e813ed60b5e100693`
- **最终交付状态：** `IMPLEMENTATION_COMPLETE_WITH_LIMITATIONS`

> **诚实证据分层声明（严格区分四态，不混用、不夸大）：**
> - **`IMPLEMENTED`：** 规格 `v0.4-rc3.5` 全部 11 项核心能力与中英文 `ADR-0024` 已在纯 Go 源码中完整落地，零新增包、零 SQLite Schema Migration、零改写 `agent_steps` 结构、零修改 Cognitive Core。
> - **`TESTED`：** 离线确定性验收矩阵 `T1–T12`（含 `RC34-F02` 全部 10 项因果并发/崩溃恢复测试、`RC34-F01` 忽略文件矩阵与 `REV-F06` 显式紧凑内置防呆）及 Phase C 新增回归测试（`C-F01`, `QGR-01..03`）全部在 `-race` 下实测通过。
> - **`INTEGRATION_VERIFIED`：** macOS ARM64（`darwin/arm64`, Go `1.26.5`, Node `v24.20.0`）全仓 44 个包 `go build ./...`、`go vet ./...`、`go test -count=1 ./...` 与 `go test -race -count=1 ./...` **100% 零失败、零数据竞争通过**；Ubuntu Linux ARM64（`linux/arm64`, Go `1.27.1`, `lc-ubuntu-go:24.04` 非 root `uid=1000` 只读源码挂载）`go build ./...`、`go vet ./...`、43 个非 `evalruntime` 包单测及全部 Context Space / Cognitive Core / Harness / `runtime` `-race` 套件通过；Windows AMD64（`GOOS=windows GOARCH=amd64 go build ./...`）交叉编译零错误通过。
> - **`INDEPENDENTLY_REVIEWED`（附显式边界说明）：** 已由独立只读审查子代理实例（角色 `oma-reviewer` / `quality gate reviewer`，会话 ID `9d0e94b7-0eed-4841-b9d5-f3cba33d5ecd`，模型标识 `MODEL_IDENTITY_NOT_EXPOSED_TO_SUBAGENT`，继承宿主当前模型环境而非外部异构模型）完成全量生产 Diff + 测试对抗性盲审与修复后增量复核（首轮检出 `0 P0 / 0 P1 / 2 P2 / 2 P3` 共 4 项 Finding，已在 `38e4fe0` 全部修复并获增量复核 `PASS`）。因：① 审查子代理未暴露独立异构模型 ID 且无 Shell 执行工具；② Windows AMD64 原生运行与 Linux 容器内依赖已启动 `systemd-run` 委托 cgroup + Python 内核的 11 项 `evalruntime` 隔离测试未在本机无对应宿主条件下执行；③ 闭源上游 BPE 词表极端分词误差、线上缓存命中率与长期自主学习收益未经外部线上基准实测证明，故按任务规则精确标记为 **`IMPLEMENTATION_COMPLETE_WITH_LIMITATIONS`**。

---

## 1. Evidence Boundary 实施前与收尾基线核验

| 核查项 | 预期契约 | 实测证据 | 状态 |
| :--- | :--- | :--- | :--- |
| 权威架构文档版本与 SHA256 | `v0.4-rc3.5` / `bd8dfb00...` | `shasum -a 256` = `bd8dfb00c9781d4d375ce4779789a71f0cbef9f547918ea1f4aa1a0c7481abd9` | **PASS** |
| Core 主工作树保护 | `HEAD == c73d17359f5d9dd16ef88b0de4525862593fddaa` 且保留未提交 `M AGENTS.md` | `LumenCortex-Core` 保持 `main` @ `c73d17359f5d9dd16ef88b0de4525862593fddaa`，`M AGENTS.md` 原封未动；全部开发隔离于 `wt/core-adr-0024-context-space-20261118` | **PASS** |
| SQLite Schema 与 Migration | 零新增 Migration（冻结于 `0001`–`0017`） | `git diff c73d173..HEAD -- internal/migrate/` 为空；`internal/migrate` 全量升降级与回滚测试通过 | **PASS** |
| Cognitive Core 治理边界 | 零修改事实/信念/隐私/Governor 语义（`ADR-0023`） | `git diff c73d173..HEAD -- internal/cognition/ internal/knowledge/ internal/verify/` 为空；全部认知与隐私回归测试通过 | **PASS** |
| 工具单次写库不变量 | 每次有副作用工具调用严格 `2` 次 SQLite 写事务 | `AdmitExecution`（1）+ `CommitToolResult`/`SettleExecution`（1，`;cm=*` 原子内嵌于 `derived_origins.post_ref <= 92B`），`TestT4` 实测断言通过 | **PASS** |
| Desktop 公共接口兼容性 | 不破坏 `config / docs / protocol / provider / runtime` 导出签名 | `git diff c73d173..HEAD` 仅新增导出常量 `config.{MaxProviderIDBytes, MaxModelKeyBytes}` 与 `skills.{SnapshottedSkill, MaxCompactCatalogBytes}`，零导出破坏性变更 | **PASS** |

---

## 2. 唯一责任模块接线矩阵（杜绝双账本、双预算、双 Transcript）

| 核心能力 | 唯一权威责任模块与符号 | 严禁重复边界 |
| :--- | :--- | :--- |
| Run 级治理锁定与不可变 `SkillSnapshot` | `runtime/agent.go` (`prepareRun`), `runtime/prompt.go` (`assemblePrompt`), `internal/skills/registry.go` (`Registry.Prompt`), `runtime/skills.go` (`newSkillReader`) | 整场 Run 唯一一次扫描磁盘建立 `Eligible []SnapshottedSkill`（含 `SKILL.md` 内存正文），`Prompt / ListSkills / ReadSkill` 共享同一只读快照 |
| 项目指令围栏感知大纲 | `runtime/prompt.go` (`compactProjectInstructions`, `extractFenceAwareHeadings`) | 仅作用于受信工作区 `> 12 KiB` 项目指令；`<globalDir>/AGENTS.md` 零二次截断，不放宽 `toolset/path.go` 沙箱 |
| `TaskState` 渲染与实渲引用同步 | `internal/taskstate/render.go` (`RenderBlock`, `RenderedBlock`) | 单一 `MaxBlockBytes = 10240` 截断偏移 `cutoff` 同时决定渲染文本与 `ItemRefs`（`startByteOffset < cutoff`），零幽灵引用 |
| 保守字节 Token 估算与上界预留 | `internal/window/estimate.go` (`EstimateBytes`, `ReserveTokensForBytes`, `Estimate`) + `provider/{anthropic,openai}/wire.go` | 全系统唯一估算原语；`EchoTokens` 仅在 `GroupTokens` 计费一次，遇 `user` 行即清零，绝不持久化私有思考原文 |
| 候选静态预筛与闭式预算策略 | `internal/agent/policy.go` (`CandidatesByStaticPrefix`, `GuardTokens`, `BaseGateSlack`, `EffectiveGateSlack`, `WindowPrefixDecision`, `ReplayDecision`) | 全系统唯一候选预筛、余量与二元前缀选择公式；`VerifyStep` 100% 离线重放 `v1` 与 `v2` 决策记录 |
| 工具与门禁反馈受约束呈现 | `internal/agent/stepspecs.go` (`IncludedStepTools`), `internal/agent/presentation.go` (`presentStepTools`, `presentFeedback`) | 每次投影分配独立新切片；`presentFeedback` 强制 `1200B/6144B` UTF-8 与 `2458B/12346B` JSON 序列化双重封顶 |
| 编辑落盘因果回执与 `;cm=*` 原子封签 | `internal/editing/engine.go` (`CommitReceipt`, `WithCommitRecorder`), `internal/toolset/editing.go` (`commitPlan`), `internal/agent/{execution,callback}.go` (`postFiles`, `sealFilesMutation`) | 零第二套文件修改表；逐 `ExecutionID` 私有闭包隔离 + `Wrote` + `SnapshotFiles` 前后原始磁盘 `SHA256 == NewSHA256` 夹逼 + Git Blob 净变三重联合证明 |
| 单源恢复与前向单调修复下界 | `internal/session/executions.go` (`ListAnsweredExecutions`), `internal/agent/transcript.go` (`ClassifyExecutionSummary`, `AggregateGroupMutation`, `DeriveRepairContext`, `r.groups`) | 运行期与冷启动 `Resume` 共享唯一 SQLite 只读聚合源；`Unknown > Proven > None` 同组一票否决；`r.groups` 钳位防内存静默裁除 |
| 7 步无环候选投影、`TurnBoundary` 试算与 `BeforeModelCall` 闭环 | `internal/agent/send.go` (`projectCandidate`, `Loop.send`), `internal/agent/transcript.go` (`cloneForTurnPreview`) | 无跨步 `compact` 锁存；`TurnBoundary` 克隆试算零写库；`BeforeModelCall` 注入重算通过后才落盘 `OriginTurn` |
| 带 `.~` 终结符无损模型分片信封 `v2` | `config/schema.go` (`64B/192B`), `internal/agent/envelope.go` (`EncodeEnvelopeModelRef`, `DecodeEnvelopeModelRef`, `requestRefs`), `runtime/{auto,verification}.go` | `projectionVersion = 2`；`model~0~..model~9.~` 最多 10 片（`P0` 最高优），缺 `.~` 终结符立即报 `ErrIncompleteModelRefChunks` 拒绝残片解码 |

---

## 3. 三阶段原子实施与 Commit 清单

| 阶段 | Commit SHA | 提交摘要 | 核心交付物 |
| :--- | :--- | :--- | :--- |
| **Phase A** | `0c9f8cd6f89492d3bf23c4c140e95c8aa2cefa0a` | `docs(adr): add ADR-0024 context space projection and budget guards` | 中英文 `ADR-0024`（14 节 1:1 对齐）、ADR 索引更新、基线核验与接线报告 |
| **Phase B · Stage 1** | `55c684161d38d0cd57170b8fc8865add7626fa30` | `feat(context): add token byte guards, immutable skill snapshot, and project outline (ADR-0024 stage 1)` | `EstimateBytes`/`ReserveTokensForBytes`、Wire `Echo` 对齐、`taskstate.RenderBlock`、`skills.Registry` 读写锁 + `os.CreateTemp` + `SnapshottedSkill` + 双模 `read_skill`、围栏感知项目大纲、`T5 / T6A / T6C / T10` |
| **Phase B · Stage 2** | `4df9e7e9fe4afdbfa14f5fb04d8fe6ef61963188` | `feat(context): add model-ref byte bounds, envelope v2 chunking, and compact presentation rules (ADR-0024 stage 2)` | `64B/192B` 配置与 Schema 校验、候选级 `ProviderPkg`/`Presentation`、`IncludedStepTools`、`presentStepTools`（含 `REV-F06` P3 六内置防呆与 MCP 6 类围栏）、`presentFeedback` 双守卫、`policy.go` 预算与 `ReplayDecision`、`.~` 终结符信封 `v2`、`T6B / T9 / T12` |
| **Phase B · Stage 3** | `565727245473ed91573945adeebf365eada8c8b6` | `feat(context): wire causal commit receipts, repair floor, and 7-step candidate projection (ADR-0024 stage 3)` | `editing.CommitReceipt`（含 `DirtyFailure` 与 `writeCAS` 全字节回读）、`postFiles` 三重夹逼与 `;cm=*` 封签、`ListAnsweredExecutions`、`DeriveRepairContext`、`r.groups` 守卫、7 步 `projectCandidate`、`TurnBoundary` 试算、`BeforeModelCall` 闭环、`T1 / T2 / T3 / T4 / T7 / T8 / T11` |
| **Phase C · 修复闭合** | `38e4fe0f1ab71a177d07817e813ed60b5e100693` | `fix(context): close Phase C review findings, contiguous skill offset, and 8K slack clamp (ADR-0024)` | 闭合全部回归与独立审查 Finding（`C-F01`, `C-F02`, `QGR-01`, `QGR-02`, `QGR-03`, `QGR-04`），新增 RED->GREEN 回归单测并同步中英文 `ADR-0024` |

---

## 4. Finding Ledger（实施与独立审查全量闭环台账）

所有在集成回归与独立对抗性代码审查中发现的 Finding 均严格遵循 `Reproduce (RED) -> Diagnose -> Minimal Fix -> Prove GREEN (-race) -> Delta Re-verify` 闭环：

| ID | 级别 | 发现者 / Owner | 涉及文件与行号 | 触发条件与实际影响 | 根因与最小修复 | 回归证据（RED -> GREEN） | 修复 Commit | 状态 |
| :--- | :---: | :--- | :--- | :--- | :--- | :--- | :---: | :---: |
| **C-F01** | P2 | Primary Implementer | `internal/agent/send.go:184-188` | `8K` 小窗口候选通过静态预筛（`compactFixed + guard + output < 8000`），但剩余空间 `0 <= room < BaseGateSlack_compact (1950)` 时，常态固定扣减 `1950` 导致 Step 1 假性报 `ErrOverflow`（触发 `TestAgentCeilingsCapTheGrant` 失败） | 当 `!repair.Active` 且 `0 <= poolBeforeSlack - compactFixed - requiredTranscript < effSlackCompact` 时，将 `effSlackCompact` 钳位至剩余空间（丢弃可选旧组并保留 100% 剩余缓冲，真超限仍报 `ErrOverflow`） | `TestProjectCandidate8KModelClampsCompactSlackWithoutFalseOverflow` + `TestLoopWindowPerCandidate` + `TestAgentCeilingsCapTheGrant` (`-race` PASS) | `38e4fe0` | **CLOSED** |
| **C-F02** | P3 | Primary Implementer | `runtime/editing_test.go:34`, `runtime/prompt_test.go:42`, `runtime/session_import_test.go:114` | 3 个非窗口历史集成测试沿用 `modelConfig` 的 `8000/16000` 合成小窗口，在 ADR-0024 `7/12` JSON 标点保守估算及 `BaseGateSlack` (`1950/7718`) 生效后触发预期内的紧凑投影或历史裁剪 | 该三项测试旨在验证 Hashline 锚点编辑、Canonical Prompt 报告对齐与 OMP 11 条导入消息续跑而非小窗口淘汰；将测试模型 `context` 显式设为 `config.DefaultContext` (`32768`)，保留全部断言与 `models_test.go` 的 `8000` 测试不变 | `TestAgentLoopEditsLinesAndRejectsAStaleEdit`, `TestEffectivePromptMatchesARealRun`, `TestImportedOMPSessionContinues` (`-race` PASS) | `38e4fe0` | **CLOSED** |
| **QGR-01** | P2 | `oma-reviewer` (`9d0e94b7`) | `internal/agent/send.go:409-565` | 主大候选 `503` 暂态故障切至小候选后在 `BeforeModelCall` 注入大 `InboxEdit/Steer` 溢出时：① 漏走 `down.ready` 可用性恢复直接终止 Run（且漏判 `r.transcriptCap` 的 `KindProviderCapacity`）；② `rebuiltInputs && due` 时先向 SQLite 写入孤立 `OriginTurn` 并清空主候选 `r.turn` `Echo` 后才报溢出 | 抽取统一 `handleOverflow` 闭包复用容量切换、`KindProviderCapacity` 与 `recoverFrom`；当 `rebuiltInputs` 为真时先在 `r.cloneForTurnPreview()` 内存试算通过后才落盘 `OriginTurn` 并以真实 `seq` 重投影 | 先写 `TestQGR01_BeforeModelCallOverflowRecoversAndNeverOrphansOriginTurn` 复现 RED（`Status:failed, firstSeq=3`），修复后 `-race` GREEN，增量盲审 PASS | `38e4fe0` | **CLOSED** |
| **QGR-02** | P2 | `oma-reviewer` (`9d0e94b7`) | `internal/skills/registry.go:307-358` | ① 边界处长描述技能超限省略后，后续短描述技能继续内联追加导致 `Included` 非连续前缀，使模型按提示调用 `read_skill(offset=len(Included))` 时漏掉被跳过的长技能；② `Text` 未扣除 `catalogPreamble + footer` 可超 `32 KiB` 约 `572B` | 计算 `fullBudget := MaxCatalogBytes - len(catalogPreamble) - footerReserve` 确保 `len(Text) <= 32768B`；引入独立 `fullStopped` / `compactStopped` 在首个装不下的技能处立即停止内联，严格保证 `Eligible[0:offset]` 连续 | 先写 `TestQGR02_PromptCatalogContiguousOffsetAndMaxBytes` 复现 RED（`len(Text)=32786 > 32768`），修复后 `-race` GREEN，增量盲审 PASS | `38e4fe0` | **CLOSED** |
| **QGR-03** | P3 | `oma-reviewer` (`9d0e94b7`) | `internal/agent/transcript.go:341-355` | 外部导入会话（`OriginImported`）的历史工具调用组在 SQLite 中无 `tool_executions` 行，原 `aggregateGroupMutation` 在 `len(execs)==0` 时返回 `MutationNone` 而非保守的 `MutationUnknown` | 在 `aggregateGroupMutation` 中检测 `assistantMsg.Origin == protocol.OriginImported` 或任一 `toolRow.Origin == protocol.OriginImported` 时立即返回 `MutationUnknown` | 先写 `TestQGR03_OriginImportedToolGroupClassifiesAsMutationUnknown` 复现 RED（`"none"`），修复后 `-race` GREEN，增量盲审 PASS | `38e4fe0` | **CLOSED** |
| **QGR-04** | P3 | `oma-reviewer` (`9d0e94b7`) | `docs/adr/0024*.md`, `runtime/context_space_t4_test.go`, 本报告 | 中英文 `ADR-0024` 需补充说明 `C-F01` 常态小窗紧凑余量钳位与 `QGR-03` `OriginImported` 规则；`context_space_t4_test.go` 存在未用局部变量 `roDB`；实施报告需更新至最终实测证据 | 中英文 `ADR-0024` §6、§8 逐行同步更新（均 106 行）；移除未用变量与导入通过 `go vet ./...`；本报告更新完整验收与平台实测数据 | `go vet ./...` 零告警通过，增量盲审 PASS | `38e4fe0` + 本提交 | **CLOSED** |

---

## 5. 离线确定性验收矩阵（`T1–T12`）实测证据总表

全部 `T1–T12` 均在真实生产调用链与真实本地 SQLite / Git 工作树上执行，零付费外部网络调用，且全部通过 `go test -race -count=1`：

| 编号 | 对应测试函数与源码位置 | 真实执行调用链与核心验证断言 | `-race` 结果 |
| :---: | :--- | :--- | :---: |
| **T1** | `internal/agent/context_space_stage3_test.go` — `TestT1_BidirectionalCandidateProjectionAndFailover` | `Loop.Run` 双向测试 `[m-32k, m-200k]` 与 `[m-200k, m-32k]`（40 个 `mcp:ext` 1KiB 示例块工具 + `40 KiB` `AGENTS.md` + 60 技能）：两候选均过静态预筛，`m-32k` 发 `ModeCompact (fixed_prefix_budget)` 保留 `(MCP server: ext)`，`m-200k` 发 100% 原始 `ModeFull (fits_full)`，`VerifyStep` 全量重放通过 | **PASS** |
| **T2** | `internal/agent/context_space_stage3_test.go` — `TestT2_DemandDrivenMonotonicSingleStepCompactRecovery` | `m-64k` 连续三步 `Loop.Run`：Step 2 返回 `22K`-token 工具大输出按需切 `ModeCompact (newest_group_fit)`，Step 3 普通 `300`-token 输出立即恢复 `ModeFull (fits_full)`，零跨步锁存 | **PASS** |
| **T3** | `internal/agent/context_space_stage3_test.go` — `TestT3A_*`；`runtime/context_space_t3_test.go` — `TestT3B_CausalAuthorshipConcurrencyAndIgnoredFilesMatrix` | **A1/A2：** `14K` 修复保留区在 `m-32k` 溢出自动升级 `m-200k`；`80K` 不可裁保留区拒绝静默删证据并受控报 `StatusFailed / KindWindowOverflow`。<br>**B（`RC34-F02` T1–T6 + `RC34-F01` 7–15）：** 真实 Git 仓库 + `.gitignore` + `.gitattributes` clean filter + 真实 MCP Server：实证验证宿主先改 `B` 后 Agent No-op 写 `B`（`;cm=unknown` 绝不推进下界）、Agent 覆写 `C != A`（`;cm=proven` 唯一推进）、改回 `A` / 写后被宿主覆写 `D`（`;cm=unknown`）、`CodeStale` 写前拒绝（含宿主文件边仍 `;cm=unknown`）、`writeCAS` 截断后写失败与 `apply_patch` 部分写回滚失败（`DirtyFailure=true -> OutcomeUnknown + ;cm=unknown`），以及 `tierWorkspace` 修改忽略文件与 `mcp__*` 100% `MutationUnknown` | **PASS** |
| **T4** | `runtime/context_space_t4_test.go` — `TestT4_MultiCallVetoCrashReconcileResumeAndMonotonicChain` | **T7：** 同 `answered_seq` 多工具调用与嵌套 `apply_experience` 逐 `ExecutionID` 回执零串线，组内任一 `Unknown` 一票否决整组为 `MutationUnknown`；**T8：** `Engine.Commit` 落盘后、`CommitToolResult` 前模拟崩溃重启，`Reconcile` 补 `OriginToolInterrupted + outcome_unknown`，`Resume` 判 `MutationUnknown`；**T9：** `CommitToolResult` 后冷启动 `Resume` 与在线态 `MutationEvidence` / `MinRequiredSeq` 逐位恒等（单有副作用调用严格 `2` 次写事务；旧版无 `;cm=` 行安全归入 `MutationUnknown`）；**T10：** 5 节点连续多轮门禁阻断单调链在线与冷恢复 100% 一致 | **PASS** |
| **T5** | `runtime/context_space_stage1_test.go` — `TestT5_ProjectInstructionsOutlineAndAnchoredRead` | `36 KiB` 含 ```` ```bash ````、`~~~py` 与未闭合围栏伪标题 `AGENTS.md` + `24 KiB` 无标题 `AGENTS.md`：伪标题 0 混入，附 `anchors=true` 指引，`PolicyReadOnly` 下 `read_file` 行号精确匹配，`<globalDir>/AGENTS.md` 越界读取被沙箱拦截，原文件 SHA256 不变 | **PASS** |
| **T6** | `internal/window/estimate_test.go` (`T6A/T6C`) + `internal/agent/context_space_stage2_test.go` — `TestT6B_PresentFeedbackDualGuardsAndBaseGateSlack` | 8 类中英日/代码/Emoji/稠密 JSON 语料 + `ReserveTokensForBytes` 全断点黄金值（`1456->854`, `2458->1438`, `6400->3738`, `12346->7206`）单调不减 + 6 类 `8 KiB` 极端转义/控制符/非法 UTF-8 输入经 `presentFeedback` 后 `json.Marshal(blocked) <= 2458B / 12346B`，`BaseGateSlack == 1950 / 7718` | **PASS** |
| **T7** | `internal/agent/context_space_stage3_test.go` — `TestT7_*` + `TestQGR01_*` | `BeforeModelCall` 暂停期间注入 `OriginSteer` 与 `InboxEdit`：自动重跑 7 步投影，清空过时 `req.Echo == nil`，`RequestEnvelope.LastSeq` 精确更新至 `Steer` 行 `seq`；暂态故障下小候选重算溢出时零孤立 `OriginTurn` 写库并正确恢复回主候选 | **PASS** |
| **T8** | `internal/agent/context_space_stage3_test.go` — `TestT8_TurnBoundaryDryRunZeroDBWritesAndMinimumProgressGuard` | Step 1 `m-200k` 产出 `14K` 工具组 + `2K` `EchoTokens`；Step 2 `[m-200k(503), m-16k, m-128k]`：`m-16k` 克隆清零 `EchoTokens` 试算触发最小进展守卫溢出且数据库 `OriginTurn` 写入数为 `0`；`m-128k` 写入唯一 `1` 条 `OriginTurn` 并完整携带 `14K` 工具结果 | **PASS** |
| **T9** | `internal/agent/context_space_stage2_test.go` — `TestT9_MCPCodeFencesAndExplicitProfileCompactSafety` | 6 类 MCP 代码围栏对照（仅正证纯示例块折叠为 `[example omitted]`，100% 保留规范围栏与 `(MCP server: ops)`）+ `REV-F06` P3 显式 `ProfileCompact` 下 `read_file, write_file, replace_in_file, edit_lines, bash, eval` 六个内置工具防呆与分页前提逐项保留 | **PASS** |
| **T10** | `runtime/context_space_stage1_test.go` — `TestT10_SkillSnapshotLifecyclePaginationAndConcurrency` + `internal/skills/snapshot_race_test.go` | 跨 3 全局根 + 1 项目根 `245` 个技能（含单根 `135 > 128` 截断告警、`16 KiB` 超限桩、`Disabled`、`Shadowed`、非受信 `Withheld`）+ 8 并发写 Goroutine：同一 Run 内 10 次全量分页顺序恒等（`<=50` 条、`<=16384B`）、中途磁盘删改 `SKILL.md` 仍无损返回初始快照正文、`Resume` 新 Run 精确刷新快照、连续前缀 `offset` 零跳项、`-race` 零竞争零残留 `.tmp` | **PASS** |
| **T11** | `runtime/context_space_t3_test.go` — `TestT11_CandidateProfileIsolationAndGovernanceInvariants` | `[m-compact, m-detailed]` 故障切换各自独立使用 `ModeCompact (candidate_profile)` 与 `ModeFull`，而 Run 级 `MaxSteps == 10` 与 `Budget == $1` 恒定不变；`64K` 下 `m-detailed` 保 `ModeFull`、`m-auto` 按 `fixed_prefix_budget` 切 `ModeCompact` | **PASS** |
| **T12** | `config/model_ref_bounds_test.go`, `internal/taskstate/render_block_test.go`, `internal/agent/context_space_stage2_test.go` — `TestT12_*` | `64B/192B` UTF-8 边界校验；`257B` 全 4 字节 Emoji 模型名 10 片双射（`model~0~..model~9.~`）与删末片 `.~` 防部分解码（`ErrIncompleteModelRefChunks`）；`10 + 7 + 111 = 128` 槽位 `lineage_complete = 1` vs `129` 槽位仅裁 `P4` 尾部而 100% 保全模型与系统引用；`128 × 96B` 实测 `refs_json = 12,673B < 16,384B`；`10240B` 截断外 `TaskState` 引用 0% 混入；`ReplayDecision` `v1`/`v2` 全分支兼容通过 | **PASS** |

---

## 6. 跨平台与全模块回归验证记录

| 目标平台与环境 | 执行命令 | 实测结果 | 覆盖与边界说明 |
| :--- | :--- | :--- | :--- |
| **macOS ARM64**（原生宿主 `darwin/arm64`, Go `1.26.5`, CGO SQLite, Node `v24.20.0`） | `go build ./...`<br>`go vet ./...`<br>`go test -timeout 35m -count=1 ./...`<br>`go test -race -timeout 45m -count=1 ./...` | **全部 Exit 0（44/44 包全绿，0 FAIL，0 DATA RACE）**<br>单测耗时：`evalruntime` `115.1s`、`internal/session` `41.4s`、`internal/toolset` `25.8s`、`runtime` `801.5s`<br>`-race` 耗时：`evalruntime` `121.1s`、`internal/session` `45.1s`、`internal/window` `25.4s`、`internal/toolset` `30.9s`、`runtime` `1285.0s` | **100% 全仓原生覆盖**（含 Cognitive Core、Harness、TaskState、Session/Notes、Knowledge、Governor、Toolset、Delegation、Provider、Migration 0001–0017、EvalRuntime） |
| **Ubuntu Linux ARM64**（Docker `lc-ubuntu-go:24.04`, `linux/arm64`, Go `1.27.1`, GCC `13.3.0`, `--init --user 1000:1000`, 只读源码挂载） | `go build ./...`<br>`go vet ./...`<br>`go test -count=1`（43 个非 `evalruntime`/`runtime` 包）<br>`go test -race -count=1`（19 个核心包 + `./runtime` 非 `Eval` 套件与 `--init` 进程回收回归） | **全部 Exit 0（0 FAIL，0 DATA RACE）**<br>`internal/agent` `-race` `12.6s`、`internal/session` `-race` `41.8s`、`internal/window` `-race` `35.0s`、`internal/migrate` `-race` `15.8s`、`runtime` Context Space / Cognition / Jobs `-race` `40.4s` | 原生验证 Linux ARM64 编译、CGO SQLite、文件系统权限（非 root `0500` 只读目录校验）、Git 快照与 `-race`；容器内未启动完整 `systemd` PID 1 委托 cgroup 及未装 Python 内核的 `evalruntime` 11 项宿主隔离测试显式标记为容器环境未执行（已在 macOS ARM64 原生验证） |
| **Windows AMD64**（交叉编译 `windows/amd64`） | `GOOS=windows GOARCH=amd64 go build ./...` | **Exit 0（零编译错误）** | 验证全部新增/修改 Go 包无 POSIX 专属编译耦合；因当前环境无原生 Windows 主机，Windows 原生运行与 `-race` 明确标记为 **`UNVERIFIED`** |

---

## 7. Strong Self-Criticism 十五项反向自检结论

1. **是否出现第二套 Context 管理？** 否。全系统唯一保留 `internal/window.Build` + `internal/agent` 7 步无环 `projectCandidate`，旧 `Loop.send` 内联逻辑已原位升级而非并存两套管线。
2. **是否重复存储 Memory 或 Transcript？** 否。`session_messages`、`task_states`、`cognition_nodes` 与 `derived_origins` 保持唯一持久源，`r.mutations` 仅为 `ListAnsweredExecutions` 的运行期只读投影缓存。
3. **是否存在预算重复计算？** 否。`EchoTokens` 仅在 `r.appendAssistant` 入账 `window.EstimateBytes` 一次并在 `GroupTokens` 计入一次；`r.requestEchoFrom` 仅提取切片引用，遇 `user` 行即清零。
4. **是否存在 Failover 状态污染？** 否。`canonicalStepInput` 只读不可变，`projectLayers` 与 `presentStepTools` 每次分配新切片，候选 `Model.Presentation` 与 `ProviderPkg` 彼此完全隔离。
5. **是否静默丢弃必要上下文？** 否。`MinRequiredSeq` 同时把守 `r.groups` 内存裁剪与 `window.Build` 后置守卫，全候选装不下时受控返回 `StatusFailed / KindWindowOverflow`。
6. **是否错误认定 `MutationProven`？** 否。彻底废除 `PreRef != PostRef` 单独判据，强制要求 `CommitReceipt.Wrote == true` + `SnapshotFiles` 前后原始磁盘 `SHA256 == NewSHA256` 夹逼 + `PreBlob != PostBlob` 并封签 `;cm=proven`。
7. **是否存在 `CommitReceipt` 与 `ExecutionID` 串线？** 否。每次 `admitExecution` / `admitCallbackExecution` 创建绑定唯一 `x.id` 的局部 `commitRecorder` 闭包（未准入调用绑定丢弃槽，嵌套回调自动遮蔽外层 Context 键）。
8. **是否能从 SQLite 恢复相同结论？** 是。`TestT4` 实证验证 `CommitToolResult` 前崩溃（`Reconcile -> OriginToolInterrupted + outcome_unknown -> MutationUnknown`）与提交后冷启动 `Resume` 的 `MutationEvidence` 和 `MinRequiredSeq` 100% 逐位一致。
9. **是否存在工具权限绕过？** 否。`presentStepTools` 绝不改动 `Permission / Parameters / MutatesWorkspace`，`<globalDir>/AGENTS.md` 不放宽 `toolset/path.go` 沙箱。
10. **是否存在 Skills 信息泄漏？** 否。`Disabled`、`Shadowed` 与非受信工作区 `Withheld` 项目技能均被排除在 `Eligible` 快照之外，`read_skill(id)` 100% 拒绝访问。
11. **是否破坏 Cognitive Core？** 否。`internal/cognition`、`internal/knowledge`、`internal/verify` 零改动，全量认知生命周期与隐私围栏回归 100% 通过。
12. **是否为了测试而放宽生产约束？** 否。生产预算常量（`1950 / 7718`、`2458B / 12346B`、`64B / 192B`、`128` 槽位、`2` 次写库）100% 严格执行；仅更新了 3 个历史非窗口测试夹具的合成 `context` 配置值（`C-F02`）。
13. **是否引入不必要 Migration？** 否。`0` 条新 SQLite Migration，复用 `derived_origins.post_ref`（`<= 92B << 512B`）与 `request_envelopes.refs_json`（`projection_version = 2`）。
14. **是否存在隐藏性能退化？** 否。`EstimateBytes` 为零分配单遍字节扫描，`SkillSnapshot` 在单场 Run 启动时仅扫描一次磁盘，`postFiles` 夹逼仅对 `tierFiles` 显式目标路径（通常 1–5 个文件）做本地 SHA-256 校验，单工具 SQLite 写事务严格保持 `2` 次。
15. **是否能在六个月后维护？** 是。所有核心规则均为纯函数（`EstimateBytes`, `ReserveTokensForBytes`, `WindowPrefixDecision`, `ClassifyExecutionSummary`, `AggregateGroupMutation`, `DeriveRepairContext`, `EncodeEnvelopeModelRef`/`DecodeEnvelopeModelRef`），并配有 `VerifyStep` 离线可重放决策审计与中英文 `ADR-0024`。

---

## 8. 最终交付强制十四问逐项回答

1. **架构是否完整实现？**
   - **是。** 权威规格 `v0.4-rc3.5` 定义的全部 11 项能力（不可变 Canonical Context、7 步无环候选级投影、`ModeFull/ModeCompact`、按需单步紧凑与自动恢复、闭式整数预算与 JSON 序列化双守卫、门禁修复下界保护、三值因果 `MutationEvidence` 与 `;cm=*` 原子封签、Run 级不可变 `SkillSnapshot` 与双模 `read_skill`、带 `.~` 终结符无损模型分片 `RequestEnvelope v2`、确定性冷启动 `Resume`、`T1–T12` 验收矩阵）已在 Go 生产代码中 100% 落地。
2. **七个认知相关领域是否正确协同？**
   - **是。** Memory（`TaskState` / Episodes / `cognition_nodes`）、Attention（`taskstate.RenderBlock` 预算内聚焦 + 7 步候选滑窗）、Reasoning（当轮内存 `Echo` 保全与跨模型 `TurnBoundary` 清零切换）、Learning（嵌套 `apply_experience` 因果追踪与经验召回）、Belief Revision（`derived_origins` 精确文件边与 `OutcomeUnknown` 保守降级）、Governance（Run 级策略/预算/权限锁定 + `source_fences` 撤回阻断）与 Verification（门禁反馈双守卫 + 修复期 `EffectiveGateSlack` 重装填与 `MinRequiredSeq` 保护）在同一 `Engine/Store/Loop` 上无缝协同。
3. **Context Space 是否真正影响 Agent 的后续行为？**
   - **是。** 在真实执行循环中：小窗口或重工具目录候选自动通过 `ModeCompact` 获得可分页回读的项目指令大纲（`anchors=true`）与可搜索/连续分页的 `read_skill` 目录而不再溢出死锁；单步大工具输出触发紧凑模式后下一步自动恢复 `ModeFull`；门禁打回后最近修改依据与门禁报错被强制保留在滑窗内供下一步修复读取；`BeforeModelCall` 注入的新指令立即进入当步请求并重置过时思考回显。
4. **经验和证据是否正确进入上下文？**
   - **是。** `TaskState` 中的引用、笔记、工作单元与验证记录经 `taskstate.RenderBlock` 在 `10 KiB` 上限内渲染进入首条 `user` 状态块，且唯有实际暴露在 `cutoff` 字节内的条目才会写入 `RequestEnvelope` 的 `ItemRefs`（`REV-F05`），既供模型决策又与 `source_fences` 血缘追踪精确对齐。
5. **修复期是否保留必要依据？**
   - **是。** `deriveRepairContext` 与 `r.groups` 共同锁定 `MinRequiredSeq`：首次门禁阻断前保留最早具有 `MutationProven` 或 `MutationUnknown` 的工具组及阻断提案/反馈；打回首步释放 `EffectiveGateSlack = 0` 容纳阻断消息，后续修复步重装填余量；唯有证明当前 Agent 实际落盘且产生净新内容的 `;cm=proven` 才允许在二次阻断时前移下界；超出最大候选容量时受控报 `KindWindowOverflow`，绝不静默丢弃修复依据。
6. **Candidate Failover 是否正确？**
   - **是。** `CandidatesByStaticPrefix` 按各候选自身最小静态前缀预筛而不误杀小模型；切换候选时从只读 `canonicalStepInput` 独立重算 `ModeFull/ModeCompact`、工具呈现、余量与滑窗；`TurnBoundary` 先在克隆切片上清零旧模型 `EchoTokens` 试算通过后才写入唯一一条 `OriginTurn`；`BeforeModelCall` 注入溢出时同样遵循容量切换与暂态故障恢复（`QGR-01`），且绝不篡改 Run 级 `MaxSteps / Budget / Policy`。
7. **Resume 是否确定性？**
   - **是。** `Resume` 通过 `Store.Reconcile` + `Store.BeginRun` + `Store.ListAnsweredExecutions` 纯粹从 SQLite 持久化记录（`session_messages` 与带 `;cm=proven|none|unknown` 封签的 `derived_origins`）重建 `r.mutations` 与 `MinRequiredSeq`，不依赖内存 `SnapshotStore` 或当前 MCP 配置表，旧版无封签行保守归入 `MutationUnknown`，运行期与任意次冷恢复逐位一致。
8. **Skills 是否安全且可发现？**
   - **是。** `skills.Registry` 具备读写锁与 `os.CreateTemp` 原子写盘保护；每次 Run 启动锁定唯一不可变 `Eligible` 快照（含 `SKILL.md` 正文），中途磁盘增删改不影响当前 Run，新 Run/`Resume` 自动刷新；`Prompt` 内联目录严格保证 `<= 32 KiB`（紧凑 `<= 8 KiB`）且 `Eligible[0:offset]` 连续不跳项（`QGR-02`）；双模 `read_skill` 支持 `<= 50` 条且 `<= 16 KiB` 搜索分页，并彻底隔离 `Disabled / Shadowed / Withheld` 技能。
9. **RequestEnvelope 是否完整？**
   - **是。** `projectionVersion = 2` 信封对任意合法 `ModelRef`（`64B + 192B` UTF-8）编码为 `model:...` 或带 `.~` 末片终结符的 `model~0~..model~9.~`（`<= 10` 片），在 `[P0 模型分片 (<=10) -> P1 投影模式 (1) -> P2 系统层摘要 (<=5) -> P3 TaskState 版本 (1) -> P4 实渲条目引用]` 优先级下 100% 保全模型身份与系统前缀标识，覆盖 Agent 主循环、`auto` 档位判定与语义验证器调用，脱离当前配置表即可离线无损解码并拒绝残片。
10. **Cognitive Core 是否完全保持原有治理边界？**
    - **是。** `internal/cognition`、`internal/knowledge`、`internal/verify`、`internal/migrate` 与 `source_fences` 零改动，所有既有认知核心、核查审批、经验采纳与隐私删除回归测试全部原样通过。
11. **T1–T12 是否全部通过？**
    - **是。** `T1` 至 `T12` 全部子集（含 `RC34-F02` T1–T10、`RC34-F01` 忽略文件对照集、`REV-F06` P3 内置紧凑防呆及 Phase C 新增 `QGR-01..03` / `C-F01` 回归用例）均已在真实调用路径与 `-race` 下 100% 实测通过。
12. **所有平台验证覆盖到哪里？**
    - **macOS ARM64（`darwin/arm64`）：** 全仓 44 个包 `go build ./...`、`go vet ./...`、`go test -count=1 ./...`、`go test -race -count=1 ./...` 100% 原生实测通过。
    - **Ubuntu Linux ARM64（`linux/arm64` 容器）：** 全仓 `go build ./...`、`go vet ./...`、43 个非 `evalruntime` 包单测及全部 Context Space / Cognitive Core / `runtime`（非 `systemd-run`/Python `Eval` 宿主隔离项）`-race` 实测通过；依赖完整启动 `systemd` VM + Python 运行时的 11 个 `evalruntime` 测试显式标记容器环境未覆盖（macOS ARM64 已原生覆盖）。
    - **Windows AMD64（`windows/amd64`）：** 全仓 `GOOS=windows GOARCH=amd64 go build ./...` 交叉编译通过；无 Windows 宿主，原生执行标记 `UNVERIFIED`。
13. **是否存在未解决 P0/P1/P2？**
    - **否（0 个未解决 P0 / P1 / P2 / P3 Finding）。** 实施回归与独立审查发现的全部 6 项记录（`C-F01`, `C-F02`, `QGR-01`..`QGR-04`）均已修复并完成 RED/GREEN 与增量复核闭环。
14. **是否可以 Merge main？**
    - **工程与测试门禁已就绪，按 Git 治理规则保持在独立分支 `feat/adr-0024-context-space-20261118` 等待 Owner 最终合并审批（本次未合并 `main`）。** 若 Owner 需要跨异构模型外部复核，可直接对干净候选分支 `feat/adr-0024-context-space-20261118` 执行审查后合入 `main`。

---

## 9. SHA256 关键交付物清单

| 文件路径 | SHA256 |
| :--- | :--- |
| `LumenCortex-architecture-audit/design/handoffs/context-space-architecture-master.zh-CN.md` | `bd8dfb00c9781d4d375ce4779789a71f0cbef9f547918ea1f4aa1a0c7481abd9` |
| `docs/adr/0024-candidate-context-projection-and-budget-guards.md` | `a36be1c8bac5f4dd208ae2466262739d8cae06e226e430845375fb2e3e8b8cba` |
| `docs/adr/0024-candidate-context-projection-and-budget-guards.zh-CN.md` | `cc037f3f7a86a352100d9282f4b0b677e0969909bd8c79203e0eba97195d792f` |
| `docs/lumencortex.schema.json` | `289856b69557229318803c3198b023f777e94e52e2cde7be0fecf84c1b654744` |

---

## 10. 实施后自主审计与修复（Autonomous Audit & Remediation，2026-11-18）

- **审计范围：** `main...feat/adr-0024-context-space-20261118` 全部生产 Diff（32 个非测试 Go 文件，+2 897 / −453 行）及被修改的既有测试；审计起点 `7e4c89d248f2c224eaafc59a04c5eb6177d51218`。
- **审计方法：** 逐文件阅读 Diff；对 `;cm=*` 封签（`sealFilesMutation`）、执行分级（`executionTierOf`）、变更证据分类与聚合、修复下界（`deriveRepairContext`）、`r.groups` 钳位、7 步候选投影、`read_skill` 分页字节上限、`EstimateBytes` 做源码级反例推演；`staticcheck -checks U1000` 与 `gofmt -l` 机械检查。
- **本节状态口径：** 以下 Finding 均为 `FIXED_BY_IMPLEMENTER` + `SELF_VERIFIED`，状态 **`PENDING_INDEPENDENT_REVIEW`**；本审计者未对自身修复授予独立 PASS。

### 10.1 Finding Ledger（审计新增）

| ID | 级别 | Owner | 文件 | 触发条件与影响 | 根因与最小修复 | RED → GREEN 证据 | 修复 Commit | 状态 |
| :--- | :---: | :--- | :--- | :--- | :--- | :--- | :---: | :---: |
| **AUD-A02** | P2 | 本审计者 | `internal/agent/presentation.go`（`parseFenceOpen`），`runtime/prompt.go`（`extractFenceAwareHeadings`） | 行首 1–2 个 `~`/`` ` ``（删除线 `~~x~~`、`~/path`、双反引号行内代码）被当作代码围栏开启：紧凑工具描述把真实说明文字折叠为 `[example omitted]`；紧凑项目大纲丢弃其后全部标题直至出现匹配闭合行 | CommonMark 围栏至少 3 个同字符；两处解析器均补 `run >= 3` 判定 | RED：`TestFoldVerifiedExampleFencesNeedsThreeFenceChars`（正文被折叠），`TestFenceAwareHeadingsNeedThreeFenceChars`（`headings=["# Setup"]`）；GREEN：两测试及 `TestT9_*`/`TestPresent*`/`TestT5_*`/`TestEffectivePrompt*` `-race` PASS | `dc4c082` | PENDING_INDEPENDENT_REVIEW |
| **AUD-A01** | P3 | 本审计者 | `internal/agent/{loop,transcript,presentation,send}.go` | Stage 3 以 `buildSystemMessages` / `projectCandidate` 取代旧路径后，`systemMessages`、`(*run).budget`、`utf8Valid` 在生产与测试中均无调用（`staticcheck U1000`）；`(*run).budget` 与新投影的 `transcriptCap` 逻辑并存易被误用 | 删除三者；把系统前缀排序契约注释迁移到 `buildSystemMessages`；`transcriptCap` 仍由 `projectCandidate` 第 5 步执行 | `staticcheck -checks U1000 ./internal/agent/` 不再报告这三项（仅剩 `main` 既有的 `requiredChecks`、`metadataJSON`）；`go test ./internal/agent/` PASS | `cedf9a2` | PENDING_INDEPENDENT_REVIEW |
| **AUD-A03** | P3 | 本审计者 | `internal/agent/context_space_stage{2,3}_test.go`，`internal/skills/snapshot_race_test.go`，`runtime/context_space_stage1_test.go` | 分支新增测试文件未 `gofmt` | 仅格式化，无行为变化 | `gofmt -l $(git diff --name-only main -- '*.go')` 输出为空 | `bfda8d5` | PENDING_INDEPENDENT_REVIEW |

### 10.2 审计中排除的候选（非缺陷，附依据）

- **`deriveRepairContext` 在 `mutations` 缺项时以空执行集回退为 `MutationNone`：** `load` 经 `populateMutations` 为窗口内全部工具组建项，每步末 `refreshGroupMutation` 为新组建项，`r.groups` 裁剪与 `mutations` 删除同步；`TurnBoundary` 预览与 steer/inbox 只追加 `user` 行。未找到可达触发路径，不构成缺陷；如需纵深防御可改为回退 `MutationUnknown`（P3 建议，未实施）。
- **`apply_patch` 同一路径多次出现：** `len(receipt.Files) != len(x.targets)` 使封签保守落为 `unknown`，只会多锁修复下界，不会误判 `none`/`proven`。
- **`apply_experience`（`tierNone`、`MutatesWorkspace: true`）：** 其嵌套写操作经回调各自 `admitCallbackExecution`，同一 `request_id` 下产生执行行，参与组聚合；不会遗漏。

### 10.3 验证记录（本节实测）

| 平台 | 命令 | 结果 |
| :--- | :--- | :--- |
| macOS ARM64（Go 1.26.5） | `go build ./...`、`go vet ./...` | PASS |
| macOS ARM64 | `go test -count=1 ./...` | 除 `runtime` 外全部 PASS；`runtime` 在默认 10 分钟超时处被终止（603.7 s，同时有 Docker 验证并行占用 CPU）；以 `-timeout 25m` 单独重跑 `go test -count=1 ./runtime/` **PASS（829.8 s）** |
| macOS ARM64 | `go test -race -count=1 -timeout 60m ./...` | **PASS**：43 个含测试的包全部 `ok`，0 FAIL，0 DATA RACE（`runtime` 1 114.7 s，`evalruntime` 132.5 s） |
| Windows AMD64 | `GOOS=windows GOARCH=amd64 go build ./... && go vet ./...` | PASS（仅交叉编译，无原生执行） |
| Ubuntu 24.04 ARM64（`lc-ubuntu-go:24.04`，Go 1.27.1，`uid=1000`，只读源码挂载，`GOPROXY=off`） | `go build ./...`、`go vet ./...` | PASS |
| 同上 | `go test -count=1`（除 `evalruntime` 外全部包） | 除 `runtime` 外全部 PASS；`runtime` 14 项失败均为环境原因：12 项 `no python/javascript runtime discovered`（Eval 宿主隔离需求，与既有 §6 例外一致），2 项 `TestCognitionCloseWaitsForManualVerification`、`TestCognitionWorkUnitFailureEpisodeInterleaving` 因镜像缺少 `curl`（`command -v curl` 为空）；在 `main@c73d173` 上同一容器复跑这 2 项**以相同方式失败**，确认非本分支回归，macOS 上 PASS |
| 同上 | `go test -race -count=1` `internal/{agent,window,skills,editing,toolset,taskstate}`、`config`；`-run "ContextSpace\|T[0-9]+_\|Prompt\|Skill\|Fence" ./runtime/` | 全部 PASS |

### 10.4 剩余限制

- 三项修复均待**不参与修复的独立审查者**复核（`PENDING_INDEPENDENT_REVIEW`）。
- `runtime` 包全量单测在 macOS 上耗时超过 `go test` 默认 10 分钟；未与 `main` 同条件计时对比，是否由本分支引起未定（`[INFERENCE]` 并行 Docker 负载放大了耗时）。
- Windows 仅交叉编译；Ubuntu 容器缺 `curl` 与 Eval 宿主条件，相应测试由 macOS 原生覆盖。
- `staticcheck U1000` 仍报告 `main` 既有的未用符号（`requiredChecks`、`metadataJSON`、`editingEngine` 等），不在 ADR-0024 范围内，未改动。
