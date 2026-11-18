# ADR-0024：候选级上下文确定性投影、因果修复下界与精确预算守卫

- **状态：** 基于已批准规格 `v0.4-rc3.5` 正式进入实施（Accepted for implementation）；合并与发布仍由 Owner 审批。
- **日期：** 2026-11-18
- **责任人：** Astra，Context Space 实施负责人
- **权威规格文档：** `LumenCortex-architecture-audit/design/handoffs/context-space-architecture-master.zh-CN.md`（`v0.4-rc3.5`，SHA256 `bd8dfb00c9781d4d375ce4779789a71f0cbef9f547918ea1f4aa1a0c7481abd9`）
- **源码基线：** Core `c73d17359f5d9dd16ef88b0de4525862593fddaa`
- **取代：** `internal/agent` 中原地修改 `presentSpecs` 与单一 `base` 静态预筛假设，以及 `internal/skills` 超 `32 KiB` 丢弃启用技能的旧行为；不回写任何历史数据库行或证据记录。
- **被取代：** 无

> **实施治理边界声明：** 本 ADR 记录已批准的 Context Space Architecture（`v0.4-rc3.5`）实施契约，将冻结架构不变量直接映射为 Go 运行时代码接线与确定性测试，**本次是架构规格批准后的正式实施，不是重新进行架构设计。**

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
- **闭式预算余量：** `GuardTokens = max(ctx/16, 512) + 512*[pkg=="anthropic-messages" && nTools>0]`；`BaseGateSlack = 1950`（紧凑）/ `7718`（详细，含 `ProposalReserveTokens = 512`）；`EffectiveGateSlack` 在常态取 `baseSlack`，阻断首步（`repair.Active && repair.NewestIsBlock`）释放为 `0`，多步修复期取 `min(baseSlack, max(0, poolBeforeSlack - candFixed - requiredRepairTokens))`。`CapacityRetry` 通过 `r.transcriptCap[cand.Ref]` 提高对应候选 `Margin`，不覆盖原始预算且不跨候选污染。

## 7. 滑窗选择与 Repair Context 保护

- **前向单调 `deriveRepairContext`：** 从最近外部用户输入（`Origin == ""` 或 `OriginImported`）向后扫描原子组；首次 `OriginCompletionBlocked` 前锁定首个 `MutationProven` 或 `MutationUnknown` 工具组为 `preBlockFloor`；跨多次连续阻断时，**唯独**两次阻断间出现 `postBlockProven == true` 才前移 `preBlockFloor`（`None` 与 `Unknown` 绝不推进 `MinRequiredSeq`）；最新阻断后若已 `postBlockProven == true` 则 `FloorIdx = activeBlock`。
- **`r.groups(maxContext, minKeepSeq)` 内存防裁剪守卫：** 钳位 `keep` 确保任何 `Seq >= minKeepSeq` 的受保护组绝不从 `r.transcript` 静默裁除。
- **`TurnBoundary` 试算与 `BeforeModelCall` 闭环：** 预检克隆 `r.transcript`、清零尾段 `EchoTokens`、追加合成 `OriginTurn`，并要求 `minRequiredSeq = minPositiveSeq(prevGroupFirstSeq, repair.MinRequiredSeq)`（最小进展守卫，溢出零写库）。仅在通过 `Cooldown`、`BeforeModelCall` 与 `applyPendingInputs` 后才正式落盘 `OriginTurn`、重跑 7 步 `projectCandidate` 并唯一构造 `ProviderRequest`。最大候选仍装不下时受控返回 `StatusFailed / KindWindowOverflow`。

## 8. `MutationEvidence` 与 `CommitReceipt` 因果封签

1. **`editing.CommitReceipt`（`internal/editing/engine.go`）：** 导出 `Committed`、`DirtyFailure`（`f.Truncate(0)` 后写/重读失败、`CodeConcurrentWrite` 或进入 `rollback` 时为 `true`）及按规范路径排序的 `[]FileCommitReceipt{Path, Wrote, Created, Deleted, OldSHA256, NewSHA256}`，采用全字节 `Wrote` 判定与 `writeCAS` 全字节 SHA-256 重读校验。
2. **逐 `ExecutionID` 私有 `commitRecorder`：** 在 `loop.go` 与 `callback.go` 每次准入分配 `x.id` 后通过 `WithCommitRecorder` 注入 `toolCtx`（嵌套回调覆盖隔离，模型参数与工具输出无法伪造），由 `toolset.commitPlan` 回填回执并按 `anyWrote || receipt.DirtyFailure` 设置 `MutatesWorkspace`。
3. **`postFiles` 三重联合核验与 `;cm=*` 原子封签：** 随既有 `CommitToolResult / SettleExecution` 单事务写入 `derived_origins.post_ref`（`DirtyFailure` 升格 `OutcomeUnknown` 并封签 `;cm=unknown`）。唯独 `OutcomeComplete && CoverageComplete && hasPositiveFilesSnapshotRef(PreRef) && receipt.Committed`、至少一个目标 `Wrote == true`、全部 `!Wrote` 目标 `preBlob == postBlob`、全部 `Wrote` 目标 `preBlob != postBlob`、`Wrote && !Deleted` 目标在 `SnapshotFiles` 前后 `os.Lstat` 元数据恒定且原始磁盘 `SHA256 == NewSHA256`（兼容 `.gitattributes` clean filter，`Deleted` 目标满足不存在且 `postBlob == "-"`）且 `PreRef != PostSnapRef` 时签发 `;cm=proven`；唯独全部目标 `!Wrote && preBlob == postBlob` 且零文件边时签发 `;cm=none`；其余一切场景（含宿主改 `B` 后 Agent No-op 写 `B`、改回原值 `A`、写后被宿主覆写 `D`、CAS 冲突、部分写失败、空目标、超限文件与历史无 `;cm=` 旧行）一律签发/归类为 `;cm=unknown`（`MutationUnknown`）。
4. **纯函数分类与同组 `Unknown` 一票否决：** 全部内置 `tierWorkspace`（`bash`、`edit_go`、`lsp_rename`、`eval_artifact`）与全部已准入外部 `mcp__*` 恒为 `MutationUnknown`；编译期固定内置只读/委派 `tierAdmit` 工具在零文件边且非 `OutcomeUnknown` 时为 `MutationNone`；`aggregateGroupMutation` 按 `Unknown > Proven > None` 聚合同一 `answered_seq`（含未配对或 `OriginToolInterrupted` 立即否决为 `MutationUnknown`）。

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
- 五级验证边界：100% 支持（1）**Decision Replay**（`VerifyStep` / `ReplayDecision` 含 `RuleWindowPrefix` 与 `CompactFixedByModel`）与（2）**Request Envelope Verification**（离线 SQLite），同内存快照支持（3）**Static Projection Verification**；明确不支持离线持久化私有 `Echo` 的（4）**Full Projection Replay** 与跨非确定性采样的（5）**Full Run Replay**。

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
