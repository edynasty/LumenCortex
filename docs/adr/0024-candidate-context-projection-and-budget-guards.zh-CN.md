# ADR-0024：候选级上下文确定性投影、因果修复下界与精确预算守卫

- **状态：** 基于规格 `v0.4-rc3.7` 正式进入实施与审查闭环（覆盖 Opus 5.5 R1 审查项 `F-01..F-04, F-06, F-09`、R2 审查项 `R2-01..R2-05`，以及 R3 审查项 `R3-01`、`R3-03` 与 `R3-04` 通知措辞；`R3-02` 与 `R3-04` 的 Live/Resume 分歧仍为 P3 未决）；合并与发布仍由 Owner 审批。
- **日期：** 2026-11-19
- **责任人：** Astra，Context Space 实施负责人
- **权威规格文档：** `LumenCortex-architecture-audit/design/handoffs/context-space-architecture-master.zh-CN.md`（`v0.4-rc3.7`，SHA256 `5c9edaeb6b917d7cf508677036f17ab1911dd6b5f1a78ee25d8e0082f6cbb89f`；`v0.4-rc3.6` 备份 `3da2b65adda4b43fddf929dfa0d6b345e9181a75bf43c43f1c79ed5abe0ab09a`；`v0.4-rc3.5` 备份 `bd8dfb00c9781d4d375ce4779789a71f0cbef9f547918ea1f4aa1a0c7481abd9`）
- **源码基线：** Core `c73d17359f5d9dd16ef88b0de4525862593fddaa`（R1 基线 `b57655c`；R2 审查基线 `a8b478617fe15e8726d4009f94454b027b1a9ef9`）
- **取代：** `internal/agent` 中原地修改 `presentSpecs` 与单一 `base` 静态预筛假设，以及 `internal/skills` 超 `32 KiB` 丢弃启用技能的旧行为；不回写任何历史数据库行或证据记录。
- **被取代：** 无

> **实施治理边界声明：** 本 ADR 记录 Context Space Architecture（`v0.4-rc3.7`，在 `v0.4-rc3.5`/`v0.4-rc3.6` 基础上完成两轮独立对抗性审查闭环升级）的实施契约，将冻结架构不变量直接映射为 Go 运行时代码接线与确定性测试，不推翻双平面核心架构。

## 1. 背景与问题

在基线 `c73d17359f5d9dd16ef88b0de4525862593fddaa` 上，LumenCortex 已具备 Cognitive Core v1.0（`ADR-0023`）、请求血缘与隐私撤回围栏（`ADR-0018`）、执行运行时预算（`ADR-0019`）、工作区信任隔离（`ADR-0013`）及原子组滑窗（`internal/window`）。但在持久化会话状态通往 Provider 请求的投影路径上，面对多模型容灾切换、重型 MCP/Skills 目录与多步完成门禁修复时仍存在六项结构性缺口：

1. **跨候选原地污染与预淘汰：** `Loop.Run`（`internal/agent/loop.go:313-336`）在候选筛选前原地修改 `specs` 并计算单一 `base`，导致 `200K` 主模型后的 `32K` 容灾模型被提前淘汰，或小模型压缩污染 `200K` 备选；`requestRefs`（`internal/agent/envelope.go:84-119`）哈希原始 `opts.SystemLayers` 而非当次实发层。
2. **乐观 Token 估算与无界门禁反馈：** `(len(json)+2)/3 + 4`（`internal/window/estimate.go:19-35`）低估高密度 JSON/转义符号，`presentFeedback`（`internal/agent/presentation.go:63-94`）对 `ProfileDetailed` 门禁打回无总上限，使被拒完成提案加门禁输出挤出待修复代码修改依据。
3. **修复上下文与 `TurnBoundary` 最小进展静默丢失：** 仅释放预算余量无法阻止 `window.Build` 单独装入极小的 `[OriginCompletionBlocked]` 或 `[OriginTurn]` 而丢弃前序修改或工具结果组；`TurnBoundary`（`internal/agent/send.go:183-194`）在容量预检前提前写库污染持久化历史，`BeforeModelCall` 暂停期注入 `Steer`（`send.go:302-326`）后未同步重算 `req.Echo`。
4. **快照因果归属盲区（`RC34-F01`、`RC34-F02`）：** `WorkspaceSnapshot`（`git status`）物理排除 `.gitignore` 文件且无执行前文件 Blob，`PreRef == PostRef` 不能证明零修改；而 `SnapshotFiles` 位于 `editing.Engine.Commit` 文件锁之外——当外部宿主先将文件从 `A` 改为 `B`，未曾读过该路径或刚 `Resume` 的 Agent 调用 `write_file(B)`（`internal/editing/engine.go:527-529` 因 `p.next == p.original` 跳过写盘 `wrote == false`）时，`PreRef(A) != PostRef(B)` 会把宿主改动误判为 Agent 修复进展，必须按 `ExecutionID` 绑定 Commit 实际落盘回执并原子封签。
5. **技能目录静默截断与运行中途快照撕裂（`REV-F02`、`RC33-F02`）：** `skills.Registry.Prompt`（`internal/skills/registry.go:305-311`）在超过 `32 KiB` 时直接跳过 `Included`，使尾部启用技能既不可发现也无法被 `read_skill` 读取；`ReadSkill` 每次重读磁盘 `SKILL.md` 且 `Registry` 缺少并发锁。
6. **失败信封缺失模型身份与引用槽位挤占（`RC3-NEW-06`、`REV-F03`、`REV-F05`、`RC33-F03`）：** `request_envelopes` 禁止非 `answered` 状态写 `decision_json`，`requestRefs` 未持久化候选 `ModelRef` 且混入未渲染的 `TaskState` 条目挤占 `128` 槽位，无终结符分片在截断时存在静默部分解码风险。

## 2. 目标与非目标

- **目标：**（1）单步只读不可变 Canonical Context；（2）无状态 Candidate-specific Projection 与 `{ModeFull, ModeCompact}` 二元模式；（3）不跨步锁死的 Demand-Driven Monotonic Projection；（4）统一整数 Token 预算（`EstimateBytes`、`ReserveTokensForBytes`、`GuardTokens`、`BaseGateSlack`、`EffectiveGateSlack`、`OutputBudget`、`UsableInput`）；（5）Repair Context 硬下界保护（`MinRequiredSeq`、`r.groups` 防裁剪钳位、`TurnBoundary` 纯内存试算与最小进展守卫、受控 `KindWindowOverflow`）；（6）绑定每 `ExecutionID` `CommitReceipt` 与 `;cm=proven|none|unknown` 封签的三值因果 `MutationEvidence`；（7）并发安全 Run 级不可变 `SkillSnapshot` 与双模 `read_skill`；（8）`RequestEnvelope` v2 带 `.~` 终结符无损 `ModelRef` 分片与 `10+7+111=128` 槽位优先级；（9）从 SQLite 逐位确定性恢复的 `Resume`。
- **非目标：** 修改 Cognitive Core（`ADR-0023`）事实、资格、Governor 或隐私语义；新建第二套 Memory、Transcript、Context Manager 或文件修改账本；新增任何 SQLite Migration（`0018+`）或修改 `agent_steps` 表结构；捆绑闭源 BPE 分词器；运行期自动推进持久化 `TaskState.WindowStartSeq`；持久化隐藏推理 `Echo` 以支持离线 `Full Projection Replay`。

## 3. 双平面正交架构

- **Plane A — 权威事实与治理平面（`internal/session`、`internal/taskstate`、`internal/cognition`、`internal/evaluator`、`internal/verify`）：** 掌管 `session_messages`、`request_envelopes`、`derived_origins`（含 `;cm=*` 因果封签）、`source_fences`、带版本 CAS 的 `TaskState`（`WindowStartSeq` 运行期只读）、认知记忆/Experience 与完成门禁。
- **Plane B — 受预算约束投影平面（`runtime`、`internal/agent`、`internal/window`、`internal/skills`、`internal/toolset`）：** 对每次候选请求生成只读视图（`ModeFull`/`ModeCompact` 前缀 + 连续历史后缀），绝不改变宿主策略、运行配额、工具权限、认知事实或数据血缘。

## 4. 方案 F2：`ModeFull / ModeCompact` 与按需单调投影

LumenCortex 采纳 **方案 F2**——基于当步标量预算的无状态二元模式决策函数 `WindowPrefixDecision`（`RuleWindowPrefix`），按严格顺序判定：
1. **`candidate_profile`（`ModeCompact`）：** `toolPref == config.ProfileCompact`（最高优先级，无条件紧凑）。
2. **`fixed_prefix_budget`（`ModeCompact`）：** `toolPref == ""` 且 `fullFixed > usableFull / 2`（当显式配置 `ProfileDetailed` 时跳过本条 `50%` 启发式）。
3. **`newest_group_fit`（`ModeCompact`）：** `fullFixed + requiredTranscript > usableFull` 且 `compactFixed + requiredTranscript <= usableCompact`（对 `""` 与 `ProfileDetailed` 均生效的单步按需兜底；绝不跨步锁存，后续步骤体积回落后自动恢复 `ModeFull`）。
4. **`fits_full`（`ModeFull`）：** 其余情况（若 `compactFixed + requiredTranscript > usableCompact` 亦不满足，由第 7 步统一返回 `window.ErrOverflow` 触发 Failover）。

前缀缓存严格区分四层：**Prefix Stability**（常态下同候选 `[Tools, System]` 字节恒定）、**Prefix Identity**（`ToolSpecsDigest` + `sys:*`）、**Provider Cache Eligibility**（OpenAI 兼容端支持自动前缀匹配；Anthropic Wire 当前不发 `cache_control`）与 **Actual Cache Hit**（标记 `UNVERIFIED`，仅作无害性能副产物）。

## 5. Candidate-Specific Projection 与治理边界

- **治理边界：** `Policy`、`Delegation`、`ToolAllowlist/Denylist`、`Workflow`、`MaxSteps`、`MaxToolCallsPerStep`、`Budget`、`Deadline` 与 `Verification` 在 `Engine.prepareRun` 一次性锁定，Failover 严禁篡改；每个 `agent.Model` 独立携带自身 `Context`、`Output`、`ToolCalling`、`Efforts`、`Price`、`ProviderPkg` 与 `Presentation`（`ToolDescriptions`、`Feedback`）。
- **静态预筛（`CandidatesByStaticPrefix`）：** 仅检查 `ToolCalling`、健康度建议与最小静态可行性 `c.Context >= compactFixed(c) + GuardTokens(c) + OutputBudget(maxTokens, c)`。
- **7 步单向无环 `projectCandidate`：**（1）新切片构造 `Full` 与 `Compact` 两套静态前缀（零原地污染）；（2）计算 `fullFixed`、`compactFixed`、`outBudget`、`guard`、`poolBeforeSlack`、`requiredRepairTokens`、`requiredTranscript`；（3）计算 `baseSlackFull/Compact`；（4）计算 `effSlackFull/Compact`；（5）叠加 `r.transcriptCap[cand.Ref]` 得出 `marginFull/Compact` 与 `usableFull/Compact`；（6）无状态执行 `WindowPrefixDecision`；（7）对选定模式唯一调用一次 `window.Build`，执行 `built.FirstSeq <= minRequiredSeq` 下界硬守卫，并生成 `r.requestEcho(built, cand.Ref)`。
- **受约束紧凑呈现：** `<globalDir>/AGENTS.md` 零二次截断且严守 `toolset/path.go` 沙箱；受信项目指令 `> 12 KiB` 生成「首 `6 KiB` + CommonMark 围栏感知 1-based 标题/200 行大纲 + 尾 `2 KiB` + `read_file(..., anchors=true)` 指引」。预算触发 `ModeCompact` 下，`builtin/task/delegation` 与非只读外部工具 100% 保留原文，完整保留 `(MCP server: <id>)`，只读 `mcp:*/lsp` 默认保留全部代码围栏，仅满足四项正证条件时替换纯示例围栏为 `[example omitted]`。显式 `ProfileCompact`（`REV-F06`）强制保留 `read_file`、`write_file`、`replace_in_file`、`edit_lines`、`bash`、`eval` 六类内置工具防呆与分页前提。

## 6. Context Budget 与 JSON 序列化膨胀守卫

- **`EstimateBytes(b []byte) int`：** 空切片为 `0`；非空单遍统计 `A`（ASCII 字母与空格，权重 `4`）、`S`（其余 ASCII `<0x80`，权重 `7`）、`U`（多字节 UTF-8 `>=0x80`，权重 `6`），返回 `floor((4*A + 7*S + 6*U + 11)/12) + 4`；`window.Estimate` 与 `provider/{anthropic,openai}` `Echo` 统一调用。`EchoTokens` 仅在 `GroupTokens` 计入一次，遇 `user` 行即清零，绝不重复计费。
- **`ReserveTokensForBytes(n int) int`：** `n <= 0` 返回 `0`，否则 `floor((7*n + 11)/12) + 4`（`1456 -> 854`、`2458 -> 1438`、`12346 -> 7206`），对任意 `<= n` 字节切片恒为 `EstimateBytes` 上界。
- **`presentFeedback` 双重字节守卫：** 净化非法 UTF-8 与控制字节，施加原始 UTF-8 上限 `1200B`（紧凑）/ `6144B`（详细）与实测 `OriginCompletionBlocked` JSON 序列化上限 `MaxCompactGateJSONBytes = 2458` / `MaxDetailedGateJSONBytes = 12346`（外壳 `58B`）。
- **闭式预算余量与 `8K` 模型兼容性（`F-06`）：** `GuardTokens = max(ctx/16, 512) + 512*[pkg=="anthropic-messages" && nTools>0]`；`BaseGateSlack = 1950`（紧凑）/ `7718`（详细，含 `ProposalReserveTokens = 512`）。LumenCortex 明确继续支持 `8K`（`Context >= 8000`）窗口模型：当候选 `cand.Context < verbatimCompactFixed + guard + outBudget`（如 `8K` 模型挂载 `Policy: "full"` 全量内置工具）时，`ModeCompact` 自动启用经 `REV-F06` 验证的安全内置紧凑工具描述以通过静态准入；常态非修复期（`!repair.Active`）`EffectiveGateSlack` 取 `min(baseSlack, max(0, (poolBeforeSlack - candFixed) / 4))`（`>=32K` 模型保持完整 `1950/7718`，`8K` 模型保留 `75%` 前缀后对话容量），阻断首步（`repair.Active && repair.NewestIsBlock`）释放为 `0`，多步修复期取 `min(baseSlack, max(0, poolBeforeSlack - candFixed - requiredRepairTokens))`。`CapacityRetry` 通过 `r.transcriptCap[cand.Ref]` 提高对应候选 `Margin`，不覆盖原始预算且不跨候选污染。

## 7. 滑窗选择、结合律增量历史折叠与固定 Gate State 滚动修复松弛

- **零正文 `HistoryGroupMeta`、$O(1)$ 结合律增量折叠与运行期/`Resume` 全结构恒等（`F-02`、`R2-04`）：** 消息正文缓冲 `r.transcript` 严格有界于 `l.maxContext()`。完整 `RepairContext`（含活跃 `GateFeedback`、`ProposalSeq`、`BlockCount`、`Stage`、`OriginalMinRequiredSeq`、`OriginalEvidence`、`PostBlockCausalFloorSeq` 与有界因果进展台账 `CausalProgress`）由单一前向结合律状态机 `repairStateMachine` 在 `HistoryGroupMeta`（上限 `MaxInMemoryHistoryGroups = 2048`）上确定性导出。当已结算组数达到 `2048` 时，最老 `1024` 组增量折叠进 `r.baseSM`（`r.load` 单遍加载执行完全相同的折叠），实现步内 $O(1)$ 内存/耗时与零候选级 SQLite 查询，并保证连续运行、进程重启与冷启动 `Resume` 的完整状态 `DeepEqual` 一致；新外部用户输入（`Origin == ""` 或 `OriginImported`）重置 `baseSM` 与 `historyGroups`。
- **带 Failover 优先守卫与固定 Gate State 的三级受控 Floor Relaxation — 方案 B（`F-03`、`R2-02`、`R2-03`、`R3-03`、`R3-04`）：**（1）Pass 1 先在当前候选校验严格 `built.FirstSeq <= minRequiredSeq`；（2）溢出时必须优先尝试全部更大窗口候选（含 `down.ready` 恢复候选），严禁小模型提前松弛；（3）唯全部更大候选均耗尽/不可用时由 `SelectMaxAvailableCandidate` 选定最大可用候选（回退切换记录 `RuleFailoverFloorFallback`），按三级单调顺序松弛：**Stage R1** 在 `Messages` 原文保留 `[BlockSeq .. LastSeq]` 并仅在 `State` 追加 `[Context Window Notice — Floor Relaxation]`（不重复复制 `GateFeedback`）；**Stage R2**（当 `[BlockSeq .. LastSeq]` 超窗时）将受 `presentFeedback` 封顶的固定 Gate State + 因果修复进展台账 + 省略范围通知注入 `State`，并严格仅在 `Seq > BlockSeq` 子集上锚定 `PostBlockCausalFloorSeq`（裁除阻断后旧只读/对话组，完整保留阻断后 `Proven/Unknown` 组）；**Stage R3**（当阻断后因果组本身亦超窗时）在 `State` 保持固定 Gate State + 因果台账的前提下，将 `Seq > BlockSeq` 滚动收紧至容纳最新原子组的最大后缀，持久化 `GatePinnedInState = true` 的可重放决策 `RuleWindowFloorRelaxed`（接入 `ReplayDecision` 与 `VerifyStep`）；因果台账每条记录的工具名、`ToolCallID`、`ExecutionID`、`PostRef` 与证据均取自所在组的同一个工具调用（`R3-03`）；通知只在保证下界不晚于最新原子步（最新的助手组或完成门禁组，`latestAtomicStepSeq`）时声称该步已保留，否则写明它可能被省略（`R3-04`：`Resume` 追加用户行后，单个超过窗口的工具结果可能被省略，而在线运行会直接失败；该分歧仍为 P3 未决）；（4）`State` 重检提示为上下文披露（非运行时工具硬阻断器，陈旧行/文本覆写由 `editing.Engine` CAS 拦截，任务完成由最终门禁重验把关，`R2-03`）；（5）若连固定 Gate State + 最新原子组也无法容纳，则受控终止为 `StatusFailed / KindWindowOverflow`（`T3-A2`）。
- **`TurnBoundary` 试算与 `BeforeModelCall` 闭环：** 预检克隆 `r.transcript`、清零尾段 `EchoTokens`、追加合成 `OriginTurn` 并校验下界（溢出零写库）。仅在通过 `Cooldown`、`BeforeModelCall` 与 `applyPendingInputs` 后才正式落盘 `OriginTurn`、重跑投影并唯一构造 `ProviderRequest`。

## 8. `MutationEvidence`、原子 No-Replace 创建零误删安全与 `CommitReceipt` 因果封签

1. **`editing.Engine` 创建零误删不变量与 `CommitReceipt`（`internal/editing/engine.go`，`F-01`，`R2-01`，`F-09`，`R3-01`，`R3-02`）：** 严禁删除任何无法同时证明文件身份与内容完全属于本次创建调用的目标文件。主创建路径（`LocalHost` 与 `*toolset.Set`）在目标同目录私有临时文件 `.lcx-create-<nonce>`（`O_RDWR | O_CREATE | O_EXCL`）内完成写入与沙箱校验后，执行平台原子 no-replace 发布（Unix `link(2)` 配合 `renameat2(RENAME_NOREPLACE)` / `renameatx_np(RENAME_EXCL)` 回退，仅删除私有临时文件；Windows `MoveFileExW(..., 0)`）；遇 `os.ErrExist` 立即无脏返回 `CodeConflict` 且绝不触碰目标路径。暂存文件只关闭、不执行 `fsync`，持久性与基线相同。文件系统两种原语都不支持时（如 macOS 上的 exFAT），新建返回 `CodeUnsupportedFilesystem` 且不写入任何内容，不提供直接创建兜底，编辑既有文件不受影响（`R3-01`，已记录限制）。私有临时文件的删除为尽力而为：创建中断或失败时可能残留 `.lcx-create-<nonce>`，今后任何清理只能删除可证明属于本引擎的临时文件（名称、父目录、普通文件、文件身份与内容均匹配），严禁按前缀或修改时间删除（`R3-02`，P3 未决）。自定义 `Host.Open(path, true)` 与多文件 `rollback` 路径在 CAS 冲突、超限或 `CodeConcurrentWrite` 时严禁删除目标文件，仅当平台 `fileIdentity`、精确预期字节与修改时间戳全部匹配时才允许回滚删除。`CommitReceipt` 导出 `Committed`、`DirtyFailure` 及 `filepath.ToSlash` 规范化的 `[]FileCommitReceipt`。
2. **逐 `ExecutionID` 私有 `commitRecorder`：** 在 `loop.go` 与 `callback.go` 每次准入分配 `x.id` 后通过 `WithCommitRecorder` 注入 `toolCtx`（嵌套回调覆盖隔离，模型参数与工具输出无法伪造），由 `toolset.commitPlan` 回填回执并按 `anyWrote || receipt.DirtyFailure` 设置 `MutatesWorkspace`。
3. **`postFiles` 斜杠规范键匹配与 `;cm=*` 原子封签（`F-09`）：** 统一以区分大小写的 `filepath.ToSlash(path)` 规范路径键（绝不依赖切片下标位置，重复规范键保守判 `;cm=unknown`）配对 `preByPath`、`postByPath` 与 `receipt.Files`，随 `CommitToolResult / SettleExecution` 单事务写入 `derived_origins.post_ref`（`DirtyFailure` 升格 `OutcomeUnknown` 并封签 `;cm=unknown`）。
4. **纯函数分类与同组 `Unknown` 一票否决（`F-04`）：** 全部内置 `tierWorkspace`（`bash`、`edit_go`、`lsp_rename`、`eval_artifact` 及全部 `debug_*` 调试器工具）、全部已准入外部 `mcp__*`、外部导入工具调用组（`OriginImported`）及历史无 `;cm=` 旧行统一归为 `MutationUnknown`；编译期固定内置只读/委派 `tierAdmit` 工具在零文件边且非 `OutcomeUnknown` 时为 `MutationNone`；按 `Unknown > Proven > None` 聚合同一 `answered_seq`。

## 9. Skills Run 快照与渐进发现

- `skills.Registry` 增加 `sync.RWMutex`、`os.CreateTemp` 唯一临时文件原子写盘、单根 `MaxSkills = 128` 截断告警，以及包含预读 `SKILL.md` 原文的 `PromptResult{Eligible []SnapshottedSkill, Text (<=32KiB), CompactText (<=8KiB)}`。
- `Engine.prepareRun` 构造唯一不可变 `skillReader`（`ordered`、`byID`），由 `SystemPrompt`、`ListSkills` 与 `ReadSkill` 严格共享；`ReadSkill(ctx, id, ""|"SKILL.md")` 直接返回内存快照正文，彻底免疫运行中途外部 `Save/Delete/SetEnabled`；附属相对文件继续经白名单与 `skills.ReadResource` 符号链接隔离读取。
- `toolset/skills.go` 注册无顶层 `required`/`oneOf` 的扁平双模 Schema `{id, path, query, offset}`，强制参数互斥、`MaxSkillCatalogPage = 50` 与 `MaxSkillCatalogResponseBytes = 16384`（首条超限通过 `DescriptionTruncated: true` 保底返回）。

## 10. `RequestEnvelope` v2、无损模型分片与血缘优先级

- `config` 强制校验 UTF-8 字节上限 `MaxProviderIDBytes = 64`、`MaxModelKeyBytes = 192`（原始 `<= 257B`，转义后 `<= 769B`）。
- `EncodeEnvelopeModelRef` 输出单条 `model:<escaped>`（`<= 90B`）或按完整 UTF-8 码点切分、以唯一末片终结符 `model~<K-1>.~` 收尾的有序分片 `model~0~..model~9.~`（严格证明 $2 \le K \le 10$）；`DecodeEnvelopeModelRef` 离线无配置还原并在缺失末片时返回 `ErrIncompleteModelRefChunks`，覆盖 `PurposeMain`、`PurposeAutoEffort` 与 `PurposeReview`（含失败/未知信封）。
- `PurposeMain` `requestRefs`（`projectionVersion = 2`）严格按优先级排列：**P0** `ModelRef`（`<=10` 永不截断）、**P1** `proj:mode:<full|compact>`（`1`）、**P2** `sys:*`（实发 `SentLayers`，`<=5`）、**P3** `ts:v<ver>`（`1`）、**P4** `taskstate.RenderBlock.ItemRefs`（仅含 `startByteOffset < cutoff` 实渲项，剩余 `111` 槽位）。超 `128` 条仅截断 P4 尾部并诚实置 `LineageComplete = false`；`128 × 96B` 序列化上限为 `12,673B < 16,384B`。

## 11. 确定性 `Resume` 与五级重放边界

- `CommitToolResult` 前崩溃由 `Store.Reconcile` 补写 `OriginToolInterrupted` 与 `outcome_unknown`（恢复为 `MutationUnknown`）；提交后崩溃通过 `Store.ListAnsweredExecutions` 逐位恢复 `;cm=*`、`MutationEvidence` 与 `MinRequiredSeq`；新 `Resume` Run 在 `prepareRun` 重建最新合法 `SkillSnapshot`。
- 五级验证边界：100% 支持（1）**Decision Replay**（`VerifyStep` / `ReplayDecision` 含 `RuleWindowPrefix`、`CompactFixedByModel`、`RuleWindowFloorRelaxed` 与 `RuleFailoverFloorFallback`）与（2）**Request Envelope Verification**（离线 SQLite），同内存快照支持（3）**Static Projection Verification**；明确不支持离线持久化私有 `Echo` 的（4）**Full Projection Replay** 与跨非确定性采样的（5）**Full Run Replay**。

## 12. 安全边界与数据库兼容性

- **安全与隐私：** 100% 保持 `ADR-0018` `consumerFence`、`OriginKnowledge` 血缘与 `window.Withhold`，保持 `ADR-0013` 未受信工作区隔离与 `toolset/path.go` 路径沙箱，`commitRecorder` 使用私有 Context 键杜绝伪造。
- **零数据库迁移：** SQLite `0001`–`0017` 零改动；`derived_origins.post_ref`（`<= 94B <= 512B`）合法通过 `checkLooseRef`、严守单次副作用工具精确 **2 次写库**，无 `;cm=` 历史旧行保守归入 `MutationUnknown`。

## 13. 模块唯一责任、替代方案与风险边界

- **唯一责任模块：** `internal/window`（字节估算与连续后缀构建）、`internal/skills` + `runtime/skills.go` + `internal/toolset/skills.go`（Run 技能快照与分页目录）、`runtime/prompt.go`（系统层与围栏大纲）、`internal/taskstate`（`RenderBlock`）、`internal/editing` + `internal/toolset` + `internal/agent/execution.go`（`CommitReceipt` 与 `;cm=*`）、`internal/session`（`ListAnsweredExecutions`）、`internal/agent`（7 步投影、修复下界、信封 v2 与重放）。
- **否决方案（1–10）：** 自动推进持久化 `WindowStartSeq`；拆出 `BeginRun` 跨会话 Notice；五区/`60%` 固定比例；光学图像压缩句柄；四级有状态 `r.degradeLevel`/`RepairAnchor`；凭 `git status` 判 `tierWorkspace` `Proven/None`；单向 `model:sha256:` 或 `agent_steps` 双写；按当前 `ToolSpec.Permission` 分类历史 `mcp__*`；无 `CommitReceipt` 仅凭 `PreRef != PostRef` 判 `Proven` 或依赖非持久 `edit.committed`；新增 `0018` 迁移或第二套文件账本。
- **风险与 `UNVERIFIED` 限制：** 闭源 BPE 分词误差与线上缓存命中率标记为 `UNVERIFIED`（由 `GuardTokens + CapacityRetry + Failover` 兜底）；共享工作树场景 C1 按真实覆写归因；技能附属参考文件保持白名单按需磁盘读取。

## 14. 三阶段实施计划与 `T1–T12` 验收映射

- **Phase A：** 提交中英文 `ADR-0024`、ADR 索引与接线/追踪报告 `docs/reports/adr-0024-context-space-implementation-2026-11-18.md`，通过基线编译、`go vet` 与包级回归。
- **Phase B：** 按原子子提交完成生产实现：（B-Stage 1）`window` 估算、`taskstate.RenderBlock`、`SkillSnapshot` + 双模 `read_skill`、项目指令大纲；（B-Stage 2）`config` UTF-8 字节上限、候选 `Presentation`/`ProviderPkg`、`stepspecs`、`presentation`（含 `REV-F06` 与反馈双守卫）、`policy`（含 `ReplayDecision`）、`envelope` v2；（B-Stage 3）`editing.CommitReceipt`、`postFiles` `;cm=*`、`ListAnsweredExecutions`、`deriveRepairContext`、`r.groups` 防裁剪、7 步 `projectCandidate`、`TurnBoundary` 试算与 `BeforeModelCall` 重建。
- **Phase C：** 完整实现并跑通 `T1–T12` 离线确定性验收矩阵（含 `RC34-F02` 全部 10 项因果并发与崩溃恢复测试），完成 macOS ARM64 与 Linux ARM64 全量 `build/vet/test/race` 及 Windows AMD64 交叉编译，执行 Cognitive Core 与全模块回归，并由独立审查流程完成代码盲审与 Finding 闭环。

[English](0024-candidate-context-projection-and-budget-guards.md) · [ADR-0018](0018-request-provenance-and-fences.zh-CN.md) · [ADR-0023](0023-unified-cognitive-lifecycle.zh-CN.md)。
