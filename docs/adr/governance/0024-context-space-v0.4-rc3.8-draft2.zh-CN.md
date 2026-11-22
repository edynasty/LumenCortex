# LumenCortex 认知上下文预算与候选级确定性投影架构主文档（v0.4-rc3.8 · R7 逐文件提交）

- **本地主文档路径：** `LumenCortex-architecture-audit/design/handoffs/context-space-architecture-master.zh-CN.md`
- **本次修订：** `v0.4-rc3.8`（草案第 2 版，2026-11-21），修订 §4.6 场景 F 的 Editing Engine 事务边界。以该场景及 Core ADR-0024 §8 为准。下文的早期源码行号、审查发现、回滚方案和实验数据保留为历史记录；仍描述 `rollback` 的段落已就地标注为历史。
- **批准状态：** `PENDING_OWNER_APPROVAL`。本版由 R7 实施者按 R7 委托（"更新规格而不是留下矛盾文档"）起草，Owner 尚未显式批准本版文本。草案第 1 版原样归档为 `context-space-architecture-master.v0.4-rc3.8-draft1.zh-CN.md`（SHA256 `a24d8b18b04aa83ff144fd416d1c5b8751e307d8eb1b7baf08efec74b217fb3f`）；`v0.4-rc3.7` 输入归档 SHA256 `5c9edaeb6b917d7cf508677036f17ab1911dd6b5f1a78ee25d8e0082f6cbb89f`。
- **状态：** R7 设计采用整批预检、逐文件提交、首错停止和只读核验。历史 `published_rolled_back` / `rollback_failed` 仍保留原义。来源撤回、执行准入、证据格及冷恢复沿用既有架构。
- **基线变更独立核验声明（强制核查记录）：**
  - **历史审查工作树说明：** 早期 `wt/core-cognitive-core-icra-cross-review-20261115`（`f8a87f5648221d546a77008bfd91f1387369efce`）已在 Cognitive Core v1.0 收口整合时清理（详见 `docs/reports/COGNITIVE_CORE_V1_GIT.md`）。
  - **原始冻结基线与实施分支基线：** 原始基线为 `LumenCortex-Core` `main`（`HEAD: c73d17359f5d9dd16ef88b0de4525862593fddaa`，仅保留历史未提交 `M AGENTS.md`，全程只读未触碰）；R1 修复冻结分支为 `wt/core-adr-0024-context-space-20261118`（`feat/adr-0024-context-space-20261118@a8b478617fe15e8726d4009f94454b027b1a9ef9`，全程保留不动）；R2 修复隔离于新工作树 `wt/core-adr-0024-r2-20261119`（分支 `fix/adr-0024-r2-remediation-20261119`）。
  - **`f8a87f5..c73d173` 源码差异核验：** 中间提交（`701a032`、`a317872`、`35af88d`、`8c30f75`、`c73d173`）仅修改 `internal/session/cognitive_graph.go`、`internal/session/cognitive_lifecycle.go`、`internal/cognition/governor.go`、认知测试及 `docs/*`；Context Space 涉及模块在 `c73d173` 上零改动。
- **历史文档基线链（已保留完整历史记录）：**
  - `v0.4-rc3.7`（SHA256 `5c9edaeb6b917d7cf508677036f17ab1911dd6b5f1a78ee25d8e0082f6cbb89f`，原件 `context-space-architecture-master.v0.4-rc3.7.zh-CN.md`）
  - `v0.4-rc3.6`（SHA256: `3da2b65adda4b43fddf929dfa0d6b345e9181a75bf43c43f1c79ed5abe0ab09a`，原件备份于 `context-space-architecture-master.v0.4-rc3.6.zh-CN.md`）
  - `v0.4-rc3.5`（SHA256: `bd8dfb00c9781d4d375ce4779789a71f0cbef9f547918ea1f4aa1a0c7481abd9`，原件备份于 `context-space-architecture-master.v0.4-rc3.5.zh-CN.md`）
  - `v0.4-rc3.4`（SHA256: `a0fa1ec6ca0cb8925ca378dbf7130933a63ededdfaba0a954b63e8dd3e7d3633`）
  - `v0.4-rc3.3`（SHA256: `cdbe136a4f3eb145f602cb7b72753028b86a9c5f23f9514b3b5f30a888e2dcfe`）
  - `v0.4-rc3.2`（SHA256: `db6980ed434ba48d38b1c2cb17e5be408ff4e027f7d0bc9d3112f878e3e302ed`）
  - `v0.4-rc3.1`（SHA256: `8dfa5398f0ef2b0e5a9df1f101e0f0df15341a8469d60559ad79f5f2920fad1b`）
  - `v0.4-rc3`（SHA256: `4d91edd4048431c78b9042f3457efff32754c670c38e40ddd103a26cea0b7761`）
  - `v0.4-rc2`（SHA256: `ddc409b6fd19dc43dad6c5ab9f5ded03e1b800921f06edb6fc72836c3ac8d2b2`）
  - `v0.4-rc1`（SHA256: `331d7852a36908fa7ffca28c2d6bd173248b34ab91eb66b9f08de61fe891b10b`）
  - `v0.3`（SHA256: `4e132e7737580933c60f4c6031001c078393a0bd1ad74cb526e5bc90f8712a87`）

---

## 零、文档维护契约、基线索引与版本演进台账

- **单一事实源原则（Single Source of Truth）：** 本本地主文档统一承载 LumenCortex 上下文窗口预算、历史认知寻址、多轮独立反方审查台账（完整保留 `v0.1`–`v0.4-rc3.4` 全部历史审查发现）、确定性证伪实验矩阵以及下发给 Astra 的 `ADR-0024` 实施规格；严禁将规格层修订（`SPEC_RESOLVED`）冒充生产代码已实施（`IMPLEMENTATION_PENDING`），严禁宣称尚未编写的代码已通过生产测试。
- **上游冻结基线关联：**
  - Cognitive Core v1.0 整合交付证据：`bench/evidence/cognitive-core-v1-integration-20261116/FINAL_DELIVERY.json`（`c73d17359f5d9dd16ef88b0de4525862593fddaa`）
  - 历史 ICRA 交叉审查台账：[REPORT_AND_LEDGER.zh-CN.md](../../../bench/evidence/cognitive-core-icra-cross-review-20261115/REPORT_AND_LEDGER.zh-CN.md)
  - 在线镜像报告：[LumenCortex Cognitive Core — Fable 最终交付独立盲审与交叉验证报告](https://docs.google.com/document/d/1MJh7b6b0PItl4CyK69AACoCiQSc5vDhBaScm8adYZfk/edit)
- **版本演进记录（Changelog）：**
  - `v0.1 (2026-11-15)`：提出五段式上下文空间（Core / Reference / Hot / Temp / Reserve）初版构想，并对外部 JVM 内存管理类比方案给出初步纠偏。
  - `v0.2 (2026-11-15)`：确立单主文档维护体系；补充针对「Z3 超级压缩图片（光学上下文压缩）」的否决论证与二期冷快照预留；起草初版 Astra 任务书。
  - `v0.3 (2026-11-15, SHA256: 4e132e7737580933c60f4c6031001c078393a0bd1ad74cb526e5bc90f8712a87)`：首轮反方审查后砍掉「五区固定百分比配额」「运行期步末自动推进 `WindowStartSeq`」「拆离 `BeginRun` 瞬态 Z3 目录」及「冗余新建 `RenderHot` / `projectionVersion = 2`」，收敛为极简混合架构初稿。
  - `v0.4-rc1 (2026-11-15, SHA256: 331d7852a36908fa7ffca28c2d6bd173248b34ab91eb66b9f08de61fe891b10b)`：完成针对 `v0.3` 的第二轮源码级独立反方盲审，查实并修复 `V03-F01`（P0 跨会话 `NoticeSources` 血缘串线漏洞）及 `V03-F02`–`V03-F07`，提出方案 F 初版（含四级降级 `Level 0..3`、`r.degradeLevel` 锁存与 `r.inGateRepair` 标志）。
  - `v0.4-rc2 (2026-11-16, SHA256: ddc409b6fd19dc43dad6c5ab9f5ded03e1b800921f06edb6fc72836c3ac8d2b2)`：整合 `CSA-01`–`CSA-11`、`NEW-01`–`NEW-04` 与 `RC2-F01`，废除四级有状态降级状态机，收敛为无状态二元 `{ModeFull, ModeCompact}` 候选级确定性投影，修复 `BeforeModelCall` `Echo` 错位与 `TurnBoundary` 提前写库污染。
  - `v0.4-rc3 (2026-11-16, SHA256: 4d91edd4048431c78b9042f3457efff32754c670c38e40ddd103a26cea0b7761)`：闭合 `RC3-F01`–`RC3-F08` 与 `Candidate ProfileCompact` 优先级问题，引入 `minRequiredSeq` 修复上下文硬下界、`ReserveTokensForBytes`、三层验证历史模型、五级重放边界、`read_file(..., anchors=true)` 分页、`read_skill(query, offset)` 原位扩展与去 `320B` 盲截断策略。
  - `v0.4-rc3.1 (2026-11-16, SHA256: 8dfa5398f0ef2b0e5a9df1f101e0f0df15341a8469d60559ad79f5f2920fad1b)`：针对 `RC3-NEW-01`–`RC3-NEW-07` 建立三值 `MutationEvidence` 初版、`presentFeedback` 序列化 JSON 字节双守卫（`2458B / 12346B`）、`ReserveTokensForBytes(1456)=854` 算术修正、`SkillCataloger.ListSkills` `16 KiB` 字节上限、MCP 默认保留代码围栏规则、`Refs[0]` 模型身份绑定初版与 7 步无环 `projectCandidate`。
  - `v0.4-rc3.2 (2026-11-16, SHA256: db6980ed434ba48d38b1c2cb17e5be408ff4e027f7d0bc9d3112f878e3e302ed)`：闭合 `REV-F01`–`REV-F05` 与 `REV-F07`（将 `MutationProven` 收敛至成功且前后 Git Blob 变化的 `tierFiles`、否决 `edit_go / lsp_rename / eval_artifact` 保留 `Proven` 的建议、打通 `read_skill` 双模 Schema 与排序、废除 `model:sha256:` 改为分片无损双射、删除 `agent_steps.toolCallRecord` 双写、以 `taskstate.RenderBlock` 对齐实渲引用，并保留 `REV-F06` 为 P3 实施要求）。
  - `v0.4-rc3.3 (2026-11-17, SHA256: cdbe136a4f3eb145f602cb7b72753028b86a9c5f23f9514b3b5f30a888e2dcfe)`：完成 `RC33-F01`（移除动态 `specs` 参数，全部已准入外部 `mcp__*` 恒判 `MutationUnknown`）、`RC33-F02`（Run 级不可变技能快照 `SkillSnapshot` 与 `Registry` 并发读写锁）及 `RC33-F03`（带末片终结符 `.~` 的 10 片无损模型编码与 `10+7+111=128` 槽位/`12,673B` 字节边界证明）。
  - `v0.4-rc3.4 (2026-11-17, SHA256: a0fa1ec6ca0cb8925ca378dbf7130933a63ededdfaba0a954b63e8dd3e7d3633)`：完成 `RC34-F01`（彻底废除凭 `WorkspaceSnapshot` 判定 `MutationNone` 的分支，将全部内置 `tierWorkspace` 无条件归类为 `MutationUnknown`；核实 `SnapshotFiles` 覆盖 Git-ignored 常规文件，统一运行期步末与 `Resume` `ListAnsweredExecutions` 聚合路径，强化 `r.groups` 防裁剪钳位）。
  - `v0.4-rc3.5 (2026-11-17, SHA256: bd8dfb00c9781d4d375ce4779789a71f0cbef9f547918ea1f4aa1a0c7481abd9)`：完成 `RC34-F02`：逐行核查 `internal/editing/{engine,store,doc}.go`、`internal/toolset/{editing,files,patch,editing_tools}.go` 与 `runtime/events.go`，通过宿主私有 Context 将 `Engine.Commit` 的文件级实际落盘回执（`Wrote / OldSHA256 / NewSHA256 / DirtyFailure`）绑定至单个 `ExecutionID`，在 `postFiles` 执行「Commit 实际落盘 + 写后磁盘 SHA-256 夹逼核验 + 前后 Git Blob 净变化」三重联合裁决，并将因果封签 `;cm=proven|none|unknown` 随 `CommitToolResult / SettleExecution` 单事务原子封入 `derived_origins.post_ref`。
  - `v0.4-rc3.6 (2026-11-19, SHA256: 3da2b65adda4b43fddf929dfa0d6b345e9181a75bf43c43f1c79ed5abe0ab09a)`：针对 Opus 5.5 首轮盲审发现（`F-01`–`F-09`）引入零正文 `HistoryGroupMeta`、Failover 优先守卫的门禁前 Floor Relaxation、`8K` 模型内置紧凑回退与 `25%` 比例 `GateSlack`、`O_CREATE|O_EXCL` 创建身份守卫、`debug_*` `MutationUnknown` 分类及 `filepath.ToSlash` 封签键映射。
  - `v0.4-rc3.7 (2026-11-19，当前并发创建零损、门禁后滚动修复与 R2 审查闭环修订版)`：**针对 Opus 5.5 第二轮独立增量审查发现（`R2-01`–`R2-05`）完成最终规格升级**：① **`R2-01`（P2，创建型写入并发原地写入零误删）**：确立「宁可留下文件也不删外部数据」硬不变量——主路径采用同目录私有临时文件 `.lcx-create-<nonce>` 写满校验后执行平台原子 no-replace 发布（POSIX `link(2)` + 失败回退 `renameat2(RENAME_NOREPLACE)`/`renameatx_np(RENAME_EXCL)`，删除仅针对私有临时文件；Windows `MoveFileExW(..., 0)`），自定义 `Host.Open` 与多文件 `rollback` 路径强制要求「文件身份 + 精确字节内容 + 纳秒时间戳」三重所有权证明，CAS 冲突、`CodeConcurrentWrite` 或超限分支**严禁删除目标文件**；② **`R2-02`（P2，门禁拒绝后长程修复期滚动上下文 — Owner 批准方案 B）**：扩展单一纯状态机 `repairStateMachine` 输出完整可恢复 `RepairContext`（含活跃 `GateFeedback`、`ProposalSeq`、`BlockCount`、`Stage`、`OriginalMinRequiredSeq`、`OriginalEvidence`、`PostBlockCausalFloorSeq` 与有界因果进展台账 `CausalProgress`），在最大可用候选上实施三级单调受控松弛（`Stage R1` 保留 `[BlockSeq..LastSeq]` 原文且不重复注入门禁反馈；`Stage R2` 当 `[BlockSeq..LastSeq]` 超窗时将受 `presentFeedback` 封顶的固定 Gate State + 因果进展台账注入当步 `State` 并仅在 `Seq > BlockSeq` 后缀上锚定 `PostBlockCausalFloorSeq`；`Stage R3` 当阻断后因果组本身亦超窗时在固定 Gate State + 因果台账保护下滚动截取容纳最新原子组的最大后缀，持久化 `GatePinnedInState=true` 的 `RuleWindowFloorRelaxed` 决策），严格保证 `State` 与 `Messages` 零重复、零新 SQLite 表、零第二套 Memory，且连续运行 / 退出重开 / 冷启动 `Resume` 全结构 `reflect.DeepEqual` 一致；③ **`R2-03`（P3，重检提示与硬守卫区分）**：按 Owner 决策维持 `State` 显式重检通知 + 编辑 CAS + 完成门禁重验三层分立职责，不宣称运行时硬拦截工具调用，诚实登记为 `OPEN (P3)`；④ **`R2-04`（P3，消除超 `2048` 组 `O(N^2)` 重复流式扫描）**：利用 `repairStateMachine` 结合律将超出窗口的前缀组增量折叠进 `baseSM`（内存恒定 `<= 2048` 组、步内推导 `0` 次 SQLite 查询、单步耗时 $O(1)$）；⑤ **`R2-05`**：全量订正实施报告与规格中的函数名、测试名、包计数及原始日志索引。

## 一、双平面架构总览与路线演进裁决

### 1.1 双平面正交架构图（Plane A ↔ Plane B）

LumenCortex 严格坚守 **Plane A（Truth & Governance Plane，权威事实与治理平面）** 与 **Plane B（Budgeted Projection Plane，受预算约束只读投影平面）** 的正交职责边界：**Plane B 只决定单次模型请求看到什么呈现形式与历史后缀，绝不改变长期认知事实、宿主权限、门禁规则或数据血缘账本。**

```text
+=========================================================================================+
|                        Plane A: Truth & Governance Plane (权威事实与治理平面)             |
|                                                                                         |
|  +-----------------------+  +------------------------+  +----------------------------+  |
|  | internal/session      |  | internal/taskstate     |  | internal/cognition &       |  |
|  | - session_messages    |  | - State (Version CAS)  |  | internal/evaluator/verify  |  |
|  | - request_envelopes   |  | - RenderBlock(<=10KiB) |  | - Memory / Experience      |  |
|  | - derived_origins     |  | - WindowStartSeq (只读)|  | - Evidence / GateResult    |  |
|  |   (含 post_ref ;cm=*) |  | - Verification (最近次)|  | - Hooks (ResetDenials)     |  |
|  | - agent_steps (零新列)|  |                        |  | - Cognitive Governor       |  |
|  | - source_fences       |  |                        |  |                            |  |
|  +-----------+-----------+  +-----------+------------+  +-------------+--------------+  |
|              |                          |                             |                 |
|              +--------------------------+-----------------------------+                 |
|                                         | 只读权威快照 + 纯持久化列 answered_seq 实证恢复  |
|                         (prepareRun 建立不可变 SkillSnapshot / BeginRun / AdmitRequest)   |
+=========================================|===============================================+
                                          v
+=========================================================================================+
|                     Plane B: Budgeted Projection Plane (受预算约束投影平面)               |
|                                                                                         |
|  1. Canonical Step Input (单步构造一次，严格只读，杜绝跨候选原地污染):                   |
|     - SystemPrompt + SystemLayers (共享 Run 级不可变 SkillSnapshot) + AgentInstructions  |
|     - IncludedStepTools ([]StepTool: 保留精确 Source; 默认保留围栏，严禁盲删业务约束)    |
|     - TaskState RenderBlock (<= 10 KiB 文本 + 严格对齐实际渲染字节的 StateRefs)          |
|     - Active Repair Context (单源 ListAnsweredExecutions 派生三值因果 MutationEvidence: |
|       唯独 Commit 实写 + 写后磁盘一致 + Blob 净变的 tierFiles 封签 cm=proven)           |
|                                         |                                               |
|  2. Static Pre-Screen: CandidatesByStaticPrefix (仅校验静态基础能力与 compactFixed 资格) |
|                                         |                                               |
|                                         v                                               |
|  3. Candidate-Specific Stateless Projection (projectCandidate — 7 步无环流水线):        |
|     +-------------------------------------------------------------------------------+   |
|     |  ① Full/Compact 两套静态前缀 -> ② 两套 Fixed -> ③ 两套序列化安全 BaseGateSlack |   |
|     |  ④ 两套 EffectiveGateSlack -> ⑤ 两套含 CapacityRetry 的 Margin 与 UsableInput  |   |
|     |  ⑥ WindowPrefixDecision 无状态选定 Mode -> ⑦ 单次 window.Build + 下界守卫 + Echo|   |
|     +-----------------------------------+-------------------------------------------+   |
|                                         |                                               |
|  4. Cooldown -> BeforeModelCall -> applyPendingInputs (若有注入则重跑 projectCandidate)  |
|  5. Commit TurnBoundary (若复核仍需) -> 真实 Seq 重确认 -> AdmitRequest                  |
|     (projVer = 2, Refs 严格按 [model (<=10) -> proj/sys/ts:v (<=7) -> ts:*] 优先级排序!) |
|  6. Provider Wire Adapter (anthropic-messages / openai-compatible)                      |
+=========================================================================================+
```

### 1.2 六方案十二维度对比裁决表

| 评价维度 | A. 保留现有 M3.2 | B. v0.2 五区固定比例 | C. 纯动态层级检索 | D. OS/MemGPT 模型自管换页 | F1. v0.4-rc1 四级有状态锁存 | **F2. v0.4-rc3.5 二元无状态候选投影（最终收敛规格）** |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **1. 架构复杂度** | 极低（`window.go` 约 170 行） | 中高（五区账本 + 瞬态 Z3） | 高（每步动态检索重排） | 极高（页表工具 + 状态维护） | 中（4 档位 × `degradeLevel` × `inGateRepair`） | **极低（7 步无环纯函数 `{Full, Compact}` + 宿主每 `ExecutionID` 因果封签守卫）** |
| **2. Token 效率** | 中（小窗口遇重型 MCP 易溢出） | 差（`32K` 死锁、`1M` 空锁） | 高 | 低（换页工具调用步数税极重） | 高（但未定义 `rawEst` 有算术风险） | **最高（显式 Profile + 前缀预算比触发，序列化安全预留零虚胖）** |
| **3. 长程与门禁修复** | 中（多检查长日志易击穿） | 低（推进起点引发失忆） | 中（相关性波动致约束闪烁） | 中低（挤占工具配额） | 中（二次门禁拒绝时 `Slack=0` 会丢修复） | **最高（三重联合证明 `Commit实写 + 写后磁盘一致 + Blob净变` 才推进下界，彻底封堵宿主并发伪 `Proven` 与忽略文件伪 `None`）** |
| **4. 多模型 Failover** | 小模型在 `Candidates` 被预淘汰 | 推进起点令大模型永久失忆 | 正常 | 复杂 | 漏修 `TurnBoundary` 提前写库与孤立组失忆 | **100% 无损一致（静态预筛与 `projectCandidate` 动态容量单一权威分离）** |
| **5. Provider 兼容性** | 100% | 100% | 100% | 强依赖高阶规划模型 | 100% | **100%（`read_skill` 双模无顶层 `oneOf`，兼容全系严格工具 Schema）** |
| **6. 前缀稳定性** | 稳定 | 每步重算 Z3 抖动 | 极差 | 差 | 四级锁存仍可跳变 `3` 次且受工具峰值永久锁死 | **Run 级不可变 `SkillSnapshot` + 候选静态前缀决定常态模式，单步峰值不锁死** |
| **7. 数据血缘** | 严密 | 易超 `MaxEnvelopeRefs=128` | 难追踪精确暴露 | 复杂 | 严密（已封堵 `V03-F01`） | **100% 严密（带末片终结符无损分片 `ModelRef` + `17+111=128` 实渲引用优先级）** |
| **8. 隐私撤回** | 原子安全 | 事务外投影存在 TOCTOU 泄露 | 易残留撤回片段 | 易残留 | 原子安全 | **原子安全（100% 复用 `BeginRun` / `CommitToolResult` 事务）** |
| **9. 崩溃恢复** | 确定性幂等 | 改起点有永久丢历史风险 | 难重现 | 页表恢复复杂 | 内存 `inGateRepair` 重启丢失 | **100% 时间不变性（`;cm=*` 随 `CommitToolResult` 单事务原子落盘，运行期与任意次 `Resume` 逐位一致）** |
| **10. 确定性验证** | 100% 通过 `VerifyStep` | 未定义重放契约 | 无法重放 | 难重放 | 跨步 `degradeLevel` 破坏单步 `VerifyStep` | **严格五级核验边界（离线防截断还原模型身份 + 单步自包含 `Decision Replay`）** |
| **11. 数据库变更** | 0 | 需扩展 | 需向量索引 | 需页表 | 零 Schema Migration | **零 SQLite Schema Migration（复用 `derived_origins.post_ref <= 512B` 合法编码因果封签，零第二套文件账本）** |
| **12. 长期维护成本** | 极低 | 高 | 高 | 极高 | 中低 | **极低（零新包、零新表、零双源同步、零独立 Context Manager）** |

## 二、被正式否决设计的复核归档（防止后续迭代回退踩坑）

经多轮独立审查复核，以下十项设计维持 **永久否决（Rejected）**：

1. **否决运行期自动推进持久化 `TaskState.WindowStartSeq`：**
   - 源码依据：`internal/taskstate/state.go:52-54`、`runtime/taskstate.go:142-166`、`internal/agent/envelope.go:147-151`、`internal/taskstate/notes.go:17-20`。
   - 核心理由：① 推进至末尾且无新消息时直接触发 `envelope.go:147`（`built.FirstSeq < 0`）存储级硬失败；② `Notes` 仅限 32 条 × `512B`，无可靠非幻觉无损摘要器；③ 小模型推进持久化起点会永久剥夺同链 `200K` Failover 模型及后续恢复运行读取完整历史的能力，而现有 `window.Build` 内存后缀选取 + `transcript.go:126` `r.groups` 本就无损且不伤数据库历史。
2. **否决将 `OriginKnowledge` / `OriginNotebook` 拆出 `BeginRun` 改为每步瞬态消息：**
   - 源码依据：`internal/session/start.go:118-169`、`internal/session/requests.go:180-214 & 453-478`、`internal/session/provenance.go:53`。
   - 核心理由：① `AdmitRequest`（`requests.go:189`）仅检查当前会话自身的 `consumerFence`，事务外动态拼装外部会话索引存在「拼装后、准入前」上游撤回的 **TOCTOU 泄露窗口**；② `RequestsExposing`（`requests.go:453`）仅检索 `first_seq <= seq <= last_seq` 而不查 `refs_json`；③ 大量句柄塞入 `requestRefs` 极易突破 `MaxEnvelopeRefs = 128` 导致 `lineage_complete = 0`。
3. **否决五区固定百分比配额（`12%/4%/20%/52%/12%`）及一切静态比例阈值（含 `v0.3` 的 `60%` 阈值）：**
   - 源码依据：`internal/agent/policy.go:55 & 134`、`internal/taskstate/render.go:15`、`provider/anthropic/wire.go:72-182 & 344`。
   - 核心理由：指令、工具与状态体积取决于任务本身而非窗口常数比例；`32K` 下 `12%`（`3,440` tokens）装不下基础指令与工具，`1M` 下 `12%` 空锁逾 11 万 tokens；`v0.3` 残留的 `60%` 阈值同样双向失灵。
4. **否决将寻址句柄做成「超级压缩图片（光学上下文压缩）」：**
   - 外部实证与源码依据：[DeepSeek-OCR: Contexts Optical Compression](https://arxiv.org/abs/2510.18234)、[DeepSeek-OCR GitHub](https://github.com/deepseek-ai/DeepSeek-OCR)、[On the Token Efficiency of Visual Text Inputs in Multimodal LLMs](https://arxiv.org/html/2510.18279v1)、`protocol/agent.go:88`、`internal/window/estimate.go:19`。
   - 核心理由：光学压缩在 `10x` 内精度约 `97%`、`20x` 跌至 `60%`，而逻辑 ID 与 Git Blob 零容错；短句柄在通用多模态切片下反而倒贴 Token；整图破坏单行 `O(1)` 撤回；且 `protocol.Message` 当前为纯字符串 `Content string`。仅保留远期多模态就绪后将「超长只读冷历史文档」按 [渐进光学记忆思路](https://carlosortet.com/blog/how-llms-read-text-as-images) 作为可选附件的探索空间。
5. **否决四级有状态降级状态机（`Level 0..3` + `r.degradeLevel`）与独立持久化 `RepairAnchor`：**
   - 源码依据：`runtime/prompt.go:164-171`、`internal/agent/policy.go:940`、`internal/taskstate/render.go:15`、`internal/agent/verifygate.go:141-145`。
   - 核心理由：四级锁存仍允许 `0->1->2->3` 三次前缀突变，且受单步工具输出峰值影响永久锁死后续步骤，同时破坏单步 `VerifyStep` 自包含性；而既有 `TaskState`、`session_messages` 与 `request_envelopes ⋈ derived_origins` 已能完整承载修复保护状态，无需重复建设独立 `RepairAnchor` 状态机。
6. **否决将 `tierWorkspace` 工具（含 `bash`、`edit_go`、`lsp_rename`、`eval_artifact`）凭 `file` 边或 `PreRef != PostRef` 判为 `MutationProven`（`REV-F01`）：**
   - 源码依据：`internal/repository/snapshot.go:127-222`、`internal/agent/execution.go:60-70 & 292-322`、`internal/toolset/editing_tools.go:173-209`、`internal/toolset/eval.go:155-163`。
   - 核心理由：`WorkspaceSnapshot` 仅采集 `git status --porcelain=v2` 的 `<Index><Worktree> <Size> <ModTimeNano>` 与 `HEAD` 而不保存执行前文件 Blob：`HEAD` 移动时 `ChangedPaths` 恒令 `Coverage = CoveragePartial`；`HEAD` 不变时 `touch`、`git add` 或全工作树并发外部编辑器保存均会生成无前置 Blob 对比的 `file` 边（甚至 `edit_go(preview=true)` 或 `eval_artifact(action="import")` 期间的并发 touch 也会产生 `file` 边），误判为 `MutationProven` 会导致 `MinRequiredSeq` 错误前移丢弃尚未修复的代码依据。
7. **否决单向哈希回退 `model:sha256:<digest>` 及在 `agent_steps.toolCallRecord` 双写变更证据（`REV-F03`、`REV-F04`）：**
   - 源码依据：`internal/session/provenance.go:53 & 109`、`internal/session/agent.go:22`、`internal/agent/loop.go:816`。
   - 核心理由：配置文件中已删除或重命名的历史超长模型无法从单向 SHA-256 离线还原字面身份，带终结符的有序分片 `model~0~..model~9.~` 可在 `10` 条引用内实现 100% 离线防截断无损双射；而 `agent_steps` 在步中崩溃未调用 `recordStep` 时缺失记录且下次 `Resume` 会重用同一 `step` 号，直接单源依赖 `request_envelopes ⋈ derived_origins`（按全局唯一 `answered_seq`）更简洁且无双源分叉。
8. **否决依赖当前内存 `ToolSpec.Permission` 对历史外部 `mcp__*` 调用做差异化变更分类（`RC33-F01`）：**
   - 源码依据：`internal/mcp/registry.go:697-710`、`internal/toolset/mcp.go:25-39`、`internal/session/executions.go:118-123`。
   - 核心理由：MCP 工具权限可被用户在 `mcp.json` 覆盖或随工作区信任切换而改变，且服务器在 `Resume` 时可能已被禁用或删除，而 `derived_origins` 不持久化历史 `Permission`；用当前配置替代历史状态会破坏同一历史调用跨 `Resume` 的时间不变性。
9. **否决凭 `WorkspaceSnapshot` 的 `CoverageComplete && PreRef == PostRef && FileEdgeCount == 0` 判定 `MutationNone`，以及否决通过 `--ignored` 或全盘文件遍历哈希改造 `WorkspaceSnapshot`（`RC34-F01`）：**
   - 源码依据：`internal/repository/snapshot.go:103-155`、`internal/repository/service.go:20`、`internal/repository/blobs.go:10`、`internal/repository/snapshot_test.go:63-91`、`internal/agent/execution.go:33 & 291-319`。
   - 核心理由：`SnapshotWorkspace` 物理排除所有 Git-ignored 文件/目录，`Complete` 仅代表 `git status` 输出未超过 `MaxStatusBytes (512 KiB)` 处截断，因此 `PreRef == PostRef` 对修改、新建、删除或批量生成忽略文件（反例 A–E）存在 100% 盲区，判为 `MutationNone` 会令 `deriveRepairContext` 漏锁 `preBlockFloor`；而追加 `--ignored=matching` 会在含 `node_modules / target / build` 的仓库中瞬间击穿 `512 KiB` 截断上限导致 `lineage_complete = 0`，全盘遍历哈希更会拖垮每次 `bash` 延迟并无法追溯历史数据库记录。
10. **否决仅凭 `tierFiles` 前后快照 `PreRef != PostRef`（或 `DerivedEdge` 文件边）单独判定 `MutationProven`，以及否决把 `edit.committed` 事件或独立文件账本当作恢复依据（`RC34-F02`）：**
    - 源码依据：`internal/editing/engine.go:420-431 & 527-529 & 590-632`、`internal/editing/store.go:25-26`、`internal/agent/execution.go:154-282`、`runtime/events.go:195-207 & 255-260`。
    - 核心理由：① `admitExecution` 与 `postExecution` 的 `SnapshotFiles` 位于 `Engine.Commit` 文件锁窗口之外——当宿主先将 `A` 改为 `B`，而当前会话未曾读过该路径（或重启 `Resume` 清空了内存 `SnapshotStore`）并调用 `write_file(path, B)` 时，`Engine.Commit` 因 `p.next == p.original` 直接跳过写盘（`p.wrote = false`）返回 `OK: true`，此时 `PreRef(A) != PostRef(B)` 且生成 `DerivedEdge`，单独凭快照差异会误将宿主并发写入归功于 Agent 并错误前移 `MinRequiredSeq`；② `runtime/events.go:257` 显式规定 `"edit.committed"` 为 `durableEvent == false`（不入 `journal`、无 `ExecutionID`、慢消费者丢包），绝不可作崩溃恢复依据；③ 再造独立文件修改表违反单一事实源原则。

---

## 三、`v0.3`–`v0.4-rc3.5` 独立反方审查完整演进台账（完整保留历史记录）

### 3.1 独立审查发现清单（`V03-F01` 至 `V03-F07`）

#### V03-F01（P0 — 架构与隐私安全阻断：`BeginRun` 跨会话 `NoticeSources` 血缘串线与漏写漏洞）
- **对应 v0.3 章节：** 第 `1.2`、`3.1（通道 A）`、`4（runtime/agent.go 映射行）` 节
- **源码路径与行号：** `internal/session/start.go:118-169`；`runtime/agent.go:774-789`；`runtime/notebooks.go:509-546`；`runtime/jobs.go:521-555`
- **可复现错误场景：** 在无匹配 `OriginKnowledge` 候选的顶层会话中，按 `v0.3` 第四节建议在 `prepareRun` 生成跨会话 `LocatorCatalog` 行追加进 `start.Notices`，并将其引用的早期会话 `S1` 放入 `start.NoticeSources`。`BeginRun` 提交后，用户对 `S1` 执行隐私删除/撤回，随后恢复当前会话继续运行。
- **根本原因：** `BeginRun` 第 `155` 行硬编码了 `if row.Origin == protocol.OriginKnowledge` 才遍历 `start.NoticeSources` 写入 `derived_edges`。`OriginNotebook`（上限 `600B`）与 `OriginJobs`（上限 `600B`）均属当前会话自有状态，完全不写跨会话边；若塞入非 `OriginKnowledge` 的跨会话 `LocatorCatalog` 行，无 `OriginKnowledge` 时**零条 `DerivedEdge` 被写入数据库**，有 `OriginKnowledge` 时血缘边全部错绑在 `OriginKnowledge` 的 `seq` 上而非 `LocatorCatalog` 的 `seq` 上。
- **影响：** 上游会话 `S1` 撤回后，`projectRun` 无法屏蔽 `LocatorCatalog` 所在 `seq`，已撤回会话引用的文件路径在后续每步请求中持续泄露给外部模型，直接违反 `ADR-0018`。
- **最小修复方案：** 在 `v0.4` 中立即移除「将 `LocatorCatalog` 混塞入 `start.Notices / NoticeSources`」的提议（跨线程检索继续走已具备完整工具结果血缘的 `recall / read_thread`）；同时准确重写文档对三类 Notice 血缘的描述。
- **必要回归测试：** 构造含跨会话启动索引的会话，并发撤回上游会话后运行 `projectRun` 与 `RequestsExposing`，验证对应 `seq` 被 `window.Withhold` 精确屏蔽且无漏边。

#### V03-F02（P1 — 候选模型预淘汰、跨模型原地污染与请求信封摘要失配）
- **对应 v0.3 章节：** 第 `3.2`、`4（envelope.go / policy.go / send.go 映射行）`、`5.3` 节
- **源码路径与行号：** `internal/agent/loop.go:318-338`；`internal/agent/send.go:190-295`；`internal/agent/envelope.go:84-119, 142-160`；`internal/agent/policy.go:139-163, 674-699, 845-942`
- **可复现错误场景：**
  1. **主 `200K` + 备 `32K` 预淘汰：** `loop.go:322` 用未降级 `Fixed` 算出单一 `base = 29,000` tokens，`Candidates()`（`policy.go:144`）判定 `32768 < 29000 + 1638 + 4096`，在进入 `send()` 前直接淘汰 `m-32k`，导致 `m-200k` 故障时无法容灾切换。
  2. **主 `32K` + 备 `200K` 污染或信封失真：** `loop.go:319` 在候选循环前按主模型配置原地执行 `specs = presentSpecs(...)` 污染后续 `200K` 候选；且 `envelope.go:84` 的 `requestRefs(opts, r)` 直接哈希 `opts.SystemLayers` 而非 `in.System`——若按 `v0.3` 所称「`envelope.go` 零改动」，为 `m-32k` 降级 `in.System` 后写入 `request_envelopes.refs_json` 的仍是未降级原文的假 SHA-256；若原地改 `opts.SystemLayers` 则永久污染 `m-200k` 与后续步骤。
- **根本原因：** 现有循环假设单步所有候选模型共享唯一一份 `specs`、`opts.SystemLayers` 与 `base`/`fixed`；`VerifyStep` 重放器亦只校验单一 `fixed`。
- **影响：** 容灾链断裂、大模型上下文被小模型污染、`ADR-0018` 信封失真、`VerifyStep` 重放报错。
- **最小修复方案：** 保持 `canonicalSpecs` 与 `canonicalLayers` 只读不可变；`Candidates()` 改按候选模型最大允许降级下的 `minBase(cand)` 判定参选资格；在 `send.go` 内纯函数生成 `(candSpecs, candLayers, level)` 并扩展 `envelope.go` 的 `requestRefs(opts, candLayers, r)` 记录当次尝试真实发送摘要（**改 Go 函数签名，无需改 SQLite 表**）；同步扩展 `Decision` 与 `ReplayDecision`。
- **必要回归测试：** `32K <-> 200K` 双向 Failover 离线替身测试，逐字节核对两端收到的 `ProviderRequest` 与各自 `RequestEnvelope` 的 `ToolSpecsDigest / refs_json`，并跑通 `VerifyStep`。

#### V03-F03（P1 — `GateSlack = 1024` 算术击穿与多步修复中途重锁自毁）
- **对应 v0.3 章节：** 第 `3.2（GateSlack）`、`4`、`5.3（T2）` 节
- **源码路径与行号：** `internal/agent/loop.go:445, 490-573`；`internal/agent/checks.go:96-111`；`internal/agent/tasktools.go:411-418`；`internal/agent/jobgate.go:24-49`；`internal/verify/gates.go:138`；`internal/agent/presentation.go:28, 58-87`
- **可复现错误场景：**
  1. **单步双消息击穿：** `32K` 模型处于默认 `ProfileDetailed`（`opts.Presentation.Feedback == ""`）。Step 10 编辑代码后余 `200` tokens；Step 11 模型输出 `1,200` tokens 完成总结（在 `loop.go:445` 落盘为 `assistant` 行），被 3 个失败检查打回，`checksCorrection` 输出 `3 × 2 KiB = 6 KiB`（约 `2,050` tokens）在 `loop.go:564` 落盘为 `OriginCompletionBlocked`。Step 12 虽释放 `1024` tokens，但新增两组共占 `3,250 > 1,224` tokens，Step 10 的代码修改记录仍被挤出！
  2. **多步修复中途重锁：** Step 12 模型先调 `read_file` 查看报错行，产生新组；到了 Step 13 真正修改代码时，最新组已是 Step 12 的 `read_file`（不含 `OriginCompletionBlocked`），`GateSlack` 重新锁死扣回预算，当场把门禁报错或刚读的上下文挤出窗口！
- **根本原因：** ① `presentFeedback` 在非 `ProfileCompact` 下完全无总长度上限（`checksCorrection` 随检查数线性增长，`workUnitCompletionCorrection` 拼接无长度限制的 `unit.Goal`，`verify.MaxGateFeedbackBytes = 4096B ≈ 1370 tokens` 亦大于 `1024`）；② 漏算了被拒 `assistant` 终态回答本身的体积；③ 解锁条件只看最新一组而非整个修复周期。
- **影响：** 收尾修 Bug 阶段丢失刚改坏的代码记录或报错详情，导致盲改耗尽 `MaxRepairs`。
- **最小修复方案：** 为 `ProfileDetailed` 增设反馈硬上限（推荐 `MaxDetailedFeedbackBytes = 6144`）；小模型触发前缀降级时门禁反馈同步切 `ProfileCompact`（`1200B ≈ 405 tokens`）；将 `RepairSlack` 定为 `FeedbackCapTokens(cand) + FinalProposalReserve`，且一旦进入门禁打回状态即**在整个修复周期内锁存释放**直至门禁通过或 Run 结束。
- **必要回归测试：** 满载窗口下「`1000`-token 终态提案 + 3 项检查失败打回 + 先 `read_file` 再 `replace_in_file`」连续修复回归测试。

#### V03-F04（P1 — 「无损保级」定性失实、`GlobalDir` 沙箱不可达与裸技能 ID 召回崩溃）
- **对应 v0.3 章节：** 第 `1.1`、`3.2（Level 1–3）`、`5.3` 节
- **源码路径与行号：** `internal/toolset/path.go:43-65`；`internal/instructions/instructions.go:25, 74-105`；`internal/agent/presentation.go:40-55`；`internal/agent/tasktools.go:40-100`；`internal/skills/registry.go:255-316`；`runtime/prompt.go:154-157, 290-415`
- **可复现错误场景：**
  1. Level 3 截断 `<globalDir>/AGENTS.md` 并提示用 `read_file` 查看，模型调用 `read_file` 时被 `toolset/path.go:52` 拦截报错 `path escapes workspace`，在 `PolicyReadOnly` 下连 `bash` 都禁用，全局后半段指令**永久不可恢复**。
  2. Level 1 调用 `compactDescription` 在首句句号处截断，直接剪掉 `work_unit_create`、`note_write`、`read_skill` 第二、三句中的权限与防呆规则，且无任何工具可查完整工具描述。
  3. `skills/registry.go:255` 原本就只输出「`id + description`」（不含技能正文），Level 2 若删掉描述只剩裸 `skill_id`（如 `wf-gate-2`），模型完全无法匹配触发条件，技能发现率归零。
- **根本原因：** 混淆了「磁盘原始数据无损」「请求可见语义无损」与「模型工具自力可恢复」三个不同概念；未核对 `path.go` 沙箱与 `registry.go` 渐进披露现状。
- **最小修复方案：** 更名为**「受约束有损降级（原始数据无损保留）」**；保护 `global-instructions` 不做伪恢复截断；项目 `AGENTS.md` 裁剪保留「首部 + 带行号 `##` 标题大纲 + 尾部」以便 `read_file(..., anchors=true, start_line=...)` 分页回读；Level 1 仅紧凑化外部 `mcp:*` 与通用只读工具描述，保留内置治理工具完整说明；Level 2 保留 `skill_id + 120B 触发短句`；`Engine.EffectivePrompt` 同步输出会话默认模型的降级告警。
- **必要回归测试：** `PolicyReadOnly` 下项目指令按章节行号回读测试、内置工具关键约束保留断言、非自解释 `skill_id` 紧凑目录结构测试。

#### V03-F05（P1 — `60%` 静态阈值双向误判与高密度 JSON/中文分词低估）
- **对应 v0.3 章节：** 第 `3.2` 节
- **源码路径与行号：** `internal/window/estimate.go:19-35`；`internal/agent/policy.go:134`；`provider/anthropic/wire.go:59-66, 307-325, 404`
- **可复现错误场景：** ① `32K` 模型 `HardPool = 26,000`，`Fixed(Level 0) = 15,000`（`57.7% < 60%` 不降级），最新工具组读取文件占 `12,000` tokens，因 `15,000 + 12,000 > 26,000` 直接报 `ErrOverflow` 失败，而降到 Level 1 本可成功；② `45 KiB` 高密度转义 JSON / 报错堆栈真实约 `28,000` tokens，`Estimate` 按 `len/3` 仅估 `15,004` tokens，`5%` `Margin`（`1,638` tokens）被直接击穿触发上游 400。
- **根本原因：** 静态 `60%` 脱离了当步真实对话需求；`len/3` 对单字节标点/转义序列与部分多字节字符存在系统性低估；Anthropic 思考模式 `EchoTokens` 进一步压缩可用池。
- **最小修复方案：** 彻底废除 `60%` 固定阈值，直接按 `Fixed(level) + RequiredTranscriptTokens <= HardPool` 选取最小满足预算的 `level`；对高密度 JSON/符号块引入保守字节加权或动态安全垫 `max(cand.Context/20, RawEstimatedTokens/8)`。
- **必要回归测试：** `58% Fixed + 45% NewestGroup` 自动降级通过测试，以及中英/代码/转义 JSON 分词保守度基准测试。

#### V03-F06（P2 — 步间动态升降级击穿 OpenAI / DeepSeek 自动前缀缓存）
- **对应 v0.3 章节：** 第 `2.3`、`3.2` 节
- **源码路径与行号：** `provider/openai/wire.go:167-212`；`provider/anthropic/wire.go:72-182`
- **根本原因与影响：** 在 OpenAI / DeepSeek Wire 协议中，`Tools` 与 `System` 位于缓存前缀最顶端。若按步动态计算降级且允许随窗口滑动反复在 `Level 0 <-> Level 1` 间横跳，会导致每次跳变时 `20K+` 前缀缓存全部失效。
- **最小修复方案：** 对单次 Run 内的每个候选模型施加**单调锁存（Monotonic Latch：`level = max(r.degradeLevel[cand.Ref], neededLevel)`）**，整场 Run 内最多发生一次单向降级。
- **必要回归测试：** 15 步增长型对话测试，断言同一候选模型的 `ToolSpecsDigest` 与 `System` 前缀哈希跳变次数 `<= 1`。

#### V03-F07（P3 — Provider 缓存统计在 `protocol.Usage` 层不可观测）
- **对应 v0.3 章节：** 第 `2.3` 节
- **源码路径与行号：** `provider/anthropic/wire.go:337-349`；`provider/openai/wire.go:88-92`；`protocol/agent.go:109-114`
- **根本原因与最小修复：** `anthropic/wire.go:344` 直接丢弃 `CacheReadInputTokens`，`openai/wire.go` 未读取 `cached_tokens`。建议作为独立 P3 演进项在 `protocol.Usage` 补充只读缓存观测字段（严守工作区 `AGENTS.md` 不重复计入 `TotalTokens` 的核算规则）。

### 3.2 源码事实核查总表

| 核查点 | 精确源码位置 | 源码真实行为 | 对 `v0.3` 的核查结论 |
| :--- | :--- | :--- | :--- |
| `TaskState` 大小控制 | `internal/taskstate/render.go:15`、`notes.go:17-23` | `MaxBlockBytes = 10240`、`notesRenderBytes = 2048`、隐藏完成单元 | `v0.3` 结论正确，无需 `RenderHot` |
| `BeginRun` 三类 Notice 血缘 | `internal/session/start.go:155-169`、`agent.go:774-789` | `Notebook/Jobs` 属本会话私有无边；**仅 `OriginKnowledge` 写 `DerivedEdge`** | `v0.3` 混淆三者且错塞 `LocatorCatalog`（`V03-F01`） |
| 候选筛选与请求信封 | `loop.go:319-335`、`envelope.go:84-119`、`policy.go:139` | 循环前原地改 `specs` 算单一 `base`；`requestRefs` 读原始 `opts.SystemLayers` | `v0.3` 宣称 `envelope.go` 零改动不成立（`V03-F02`） |
| 完成门禁反馈体积 | `checks.go:96`、`tasktools.go:411`、`verify/gates.go:138`、`presentation.go:58` | `ProfileCompact` 限 `1200B`；默认 `ProfileDetailed` 下多检查输出无总上限 | `v0.3` 固定 `1024` tokens 算术失守（`V03-F03`） |
| `read_file` 与全局指令 | `internal/toolset/path.go:43-65`、`instructions.go:74-105` | `s.resolve` 严禁越出 `s.workspace`，`<globalDir>/AGENTS.md` 不可读 | `v0.3` Level 3 全局指令恢复路径走不通（`V03-F04`） |
| `Skills` 目录渲染与读取 | `internal/skills/registry.go:255-316`、`toolset/skills.go:12-38` | `Prompt()` 本就只含 `id + description`；`read_skill` 绑 `prepareRun` 列表 | `v0.3` 压成裸 ID 会严重损伤发现率（`V03-F04`） |
| Token 估算与 `Margin` | `internal/window/estimate.go:19`、`policy.go:134` | `(len(json)+2)/3 + 4`，固定 `5%` `Margin` | 高密度 JSON/符号低估可超 `35%`（`V03-F05`） |
| Anthropic / OpenAI Wire | `provider/anthropic/wire.go:72-182, 344`、`openai/wire.go:167-212` | Anthropic 无 `cache_control` 零缓存；两端 `Tools` 均位于前缀最前端 | 需加单调锁存防破坏 OpenAI/DeepSeek 自动缓存（`V03-F06`） |

### 3.3 对外部审查者五项意见及三个细节的逐项裁决

1. **意见 1（`GateSlack` 1024 固定上限不足，应按真实门禁反馈预算计算）：`部分认可`**。准确指出了 `ProfileDetailed` 与 `MaxGateFeedbackBytes = 4096` 超出 `1024` tokens 的事实，但还需补上「被拒 `assistant` 终态回答本身占用的窗口体积（`loop.go:445`）」以及「多步修复周期内持续锁存解锁（防第 2 步重锁挤掉报错）」两块拼图才真正严密。
2. **意见 2（不同模型必须独立生成上下文投影，防止小模型污染大模型）：`完全认可`**。`loop.go:319-335` 与 `envelope.go:84` 的真实源码证实了原地覆盖污染与预淘汰双重隐患。
3. **意见 3（工具描述和指令截断不能称为无损折叠，应改为受约束降级、原始数据无损保留）：`完全认可`**。`presentation.go:46` 首句截断会丢失次句安全约束，`toolset/path.go:52` 阻断对 `<globalDir>/AGENTS.md` 的 `read_file` 回读，必须严格区分存储无损、请求语义有损与按需可恢复边界。
4. **意见 4（Skills 压缩必须保留可发现性与 `read_skill` 可访问性）：`完全认可`**。`skills/registry.go:255` 证实目录本就不含正文，保留 `skill_id + 120B 触发短句` 才能兼顾体积与召回。
5. **意见 5（`60%` 阈值缺乏论证，应直接按候选模型实际预算判断）：`完全认可`**。数学反例证明 `60%` 双向失灵，改为基于 `HardPool` 的按需不等式加单调锁存更简更准。
6. **细节核查 A（`BeginRun` 三类 Notice 血缘描述）：`不认可 v0.3 原表述`**（仅 `OriginKnowledge` 写跨会话 `DerivedEdge`，`Notebook` 与 `Jobs` 无跨会话边）。
7. **细节核查 B（零 Migration 能否保证信封精确追踪候选投影）：`部分认可`**（零 SQLite Schema 迁移完全成立，但 `envelope.go` 代码必须改动以接收候选级 `candLayers`）。
8. **细节核查 C（Provider 缓存论证充分性）：`部分认可`**（Anthropic 零缓存有源码实证，但需补齐动态降级破坏 OpenAI/DeepSeek 前缀缓存的单调锁存防护）。

---

### 3.4 `v0.4-rc1 -> v0.4-rc2` 独立审查结论整合矩阵（`CSA-01`–`CSA-11`、`NEW-01`–`NEW-04` 与新增 `RC2-F01`）

> **历史保留说明：** 上文第 `3.1`–`3.3` 节完整保留 `v0.3 -> v0.4-rc1` 阶段的原始审查记录不做删改；下文记录对 `v0.4-rc1`（SHA256: `331d7852a36908fa7ffca28c2d6bd173248b34ab91eb66b9f08de61fe891b10b`）开展交叉反方审查与二次源码深挖后的历史裁决。

#### A. 上一轮 `CSA-01` 至 `CSA-11` 处理矩阵

| 编号 | 审查议题 | 独立源码核查裁决 | `v0.4-rc2` 最终收敛方案（对应章节） | 状态 |
| :--- | :--- | :--- | :--- | :--- |
| **CSA-01** | Candidate Profile 未完整接入 | **部分确认**：`runtime/models.go:195-206` 构造 `agent.Model` 时丢弃了各候选自身的 `ResolvedModel.Profile`；但 `MaxSteps / MaxToolCallsPerStep / Budget` 属 **Run 级治理不变量**（`runtime/agent.go:702-717`），绝不可随 Failover 改变；仅 `Presentation`（`ToolDescriptions`, `Feedback`）属候选级展示属性。 | `agent.Model` 新增 `Presentation PresentationOptions`；Run 级治理约束与宿主授权在 `prepareRun` 一次性锁定；门禁落盘反馈按当步应答模型 `answered.Model` 的有效模式格式化（§4.2, §4.6）。 | **CLOSED** |
| **CSA-02** | Candidate-specific Projection 不完整 | **完全确认**：`System`、`Tools`、`TaskState`、`Fixed`、`window.Build`、`CapacityRetryDecision`（`send.go:350`）、`capTranscript`（`transcript.go:154`）、`Echo` 与 `RequestEnvelope` 必须绑定同一候选最终投影。 | 收口至单一无副作用纯函数 `projectCandidate` 输出 `candidateProjection` 值结构，预算、重试、请求与信封 100% 读取同一份 `proj` 快照（§4.2, §4.3）。 | **CLOSED** |
| **CSA-03** | Repair Anchor 与 Repair Budget 语义 | **完全确认（并修正计数模型）**：① `transcript.go:52` 遇 `OriginCompletionBlocked`（`Role: "user"`）即清空 `r.turn`；② `v0.4-rc1` 令修复期 `Slack=0` 导致第 2 次门禁打回无缓冲；③ 源码共有 **6 类完成阻断路径**（仅 `verify.Evaluate` 消耗 `r.attempts` `DefaultMaxRepairs=2, MaxRepairsLimit=5`；生命周期 Hook 消耗 `Hooks` 拒绝计数；`jobs / workflow / work_unit / checks` 受 `MaxSteps / Budget` 约束），严禁拿 `OriginCompletionBlocked` 消息数当验证修复次数。 | 保持现有 `r.attempts` 与 `Hooks.ResetDenials` 机制不变；证明无需冗余 `RepairAnchor` 结构；详细反馈封顶 `6144B`；`EffectiveGateSlack` 在打回首步释放为 `0`，后续修复工具步重装填但受「不挤出最新活跃 `OriginCompletionBlocked` 尾部」下限约束（§4.6）。 | **CLOSED** |
| **CSA-04** | `TokenizerGuard` 未定义 `rawEst` 与无依据 `99.9%` 承诺 | **部分确认**：`v0.4-rc1` 未界定 `rawEst` 范围（若误取全量未裁剪历史将算出负预算死锁），且离线无词表支撑 `99.9%` 统计承诺；同时查明 `Estimate` 已对 JSON 转义翻倍且 `EchoTokens` 已计入 `GroupTokens`。 | 废除 `rawEst/8` 与 `99.9%` 承诺；将 `window.Estimate` 升级为单遍 UTF-8 字符类别保守整数公式 `EstimateBytes`（区分英文空白、ASCII 符号数字、多字节字符），配合闭式 `GuardTokens` 与既有 `CapacityRetry` 兜底（§4.5）。 | **CLOSED** |
| **CSA-05** | Global 与 Project 指令权威与 `64 KiB` 边界 | **部分确认**：`internal/instructions/instructions.go:25` 已有 `MaxFileBytes = 64 KiB` 截断与告警；`<globalDir>/AGENTS.md` 位于工作区外（严禁放宽 `toolset/path.go:52` 沙箱）。 | Global 指令在投影层零二次截断（小模型装不下则跳过候选 Failover 或报 `KindWindowOverflow`，绝不静默丢安全规则）；受信 Project 指令超 `12 KiB` 时在 `ModeCompact` 生成围栏感知章节行号大纲（无标题退化为 200 行分段索引），由既有只读 `read_file` 分页回读（§4.9）。 | **CLOSED** |
| **CSA-06** | Skills `MaxCatalogBytes` 与 `read_skill` 一致性 | **部分确认（查实既有 `32 KiB` 静默丢技能缺陷）**：`internal/skills/registry.go:283-288` 在完整描述超 `32 KiB` 时直接跳过 `result.Included`，使尾部启用技能既不可发现也无法被 `read_skill` 读取。 | 最小改造 `Registry.Prompt`：同时产出 `Text` 与 `CompactText`（`160B` 触发摘要）；完整版超 `32 KiB` 时自动回退紧凑行以保住全部符合信任边界的启用技能进入 `Included`；严守未受信工作区隔离与 `Resource.Truncated` 显式披露（§4.9）。 | **CLOSED** |
| **CSA-07** | Run-Level 状态与 Step-Level Replay 冲突 | **完全确认**：`policy.go:936` `VerifyStep` 仅读取单步 JSON，跨步可变字典 `r.degradeLevel` 破坏单步自包含重放。 | 废除跨步 `r.degradeLevel`；明确划分 Decision Replay、Request Envelope Verification、Projection Replay 与不支持离线无桩 Full Run Replay 四层验证边界（§4.11）。 | **CLOSED** |
| **CSA-08** | Prefix Cache 四概念混同 | **完全确认**：四级锁存最多仍跳变 3 次；且 `provider/anthropic/wire.go:167-182` 未发 `cache_control`，不能宣称提升所有 Provider 缓存命中率。 | 严格区分 Prefix Stability、Prefix Identity、Provider Cache Eligibility 与 Actual Cache Hit（标记 `UNVERIFIED`）；缓存仅作无害性能副产物，绝不影响语义、权限或正确性（§4.4）。 | **CLOSED** |
| **CSA-09** | Tool `Source` 缺失与首句截断丢安全约束 | **完全确认**：`StepSpecs` 丢弃了 `Source`，而 `stepspecs.go:71` 已有 `StepTools` 完整保留 `builtin / task / delegation / lsp / mcp:<id>`。 | 预算触发 `ModeCompact` 时 **100% 逐字节保留全部 `builtin / task / delegation` 工具描述**（总计仅约 `1.6K` tokens），仅压缩外部 `mcp:*`（及超长 `lsp`）描述至 `320B` 并完整保留参数 Schema 与权限语义（§4.9）。 | **CLOSED** |
| **CSA-10** | `ProjectionVersion` 与信封摘要契约 | **部分确认**：零 SQLite 迁移完全兼容将 Go 常量 `projectionVersion` 升至 `2`（`envelope.go:28`）。 | 升版 `projectionVersion = 2`；`requestRefs` 哈希当次实发 `SentLayers`；明确区分协议层规范化语义摘要与 Provider Wire 字节哈希（§4.10）。 | **CLOSED** |
| **CSA-11** | 测试矩阵 `T1–T8` 缺乏确定性闭环 | **完全确认**：原 `T6/T7` 含非确定性表述，且缺漏连续门禁拒绝、`BeforeModelCall` Steer、`TurnBoundary` 溢出等场景。 | 重构扩展为 `T1–T12` 纯离线确定性验收矩阵，每项具备固定输入、方法、预期结果、失败条件与可重复证据（§6）。 | **CLOSED** |

#### B. Carbon 独立审查发现（`NEW-01`–`NEW-04`）及本次二次核查新增 `RC2-F01` 处理矩阵

| 编号 | 级别 | 源码定位与问题本质 | `v0.4-rc2` 收敛方案与对应测试 | 状态 |
| :--- | :--- | :--- | :--- | :--- |
| **NEW-01** | **P1** | `internal/agent/send.go:228 & 271-290`：先算 `req.Echo` 再进 `BeforeModelCall`，暂停期注入 `Steer` 清空 `r.turn` 并重建 `Messages` 后未重算 `req.Echo` 与候选投影，导致旧 `Echo` 下标错位污染新请求。 | 将 `protocol.ProviderRequest` 构造唯一收口至 `BeforeModelCall` + `applyPendingInputs` **之后**；一旦 `rebuiltInputs == true`，完整重跑 `projectCandidate` 同步刷新 `Messages / Tools / Echo / Budget / Envelope`（§4.7，测试 `T7`）。 | **CLOSED** |
| **NEW-02** | **P1** | `internal/agent/send.go:180-205`：候选循环在容量检查前直接调用 `Store.AppendMessage` 写入持久 `OriginTurn` 并清空 `r.turn`，未被选中或随即溢出的候选永久污染持久化 `Transcript`。 | 候选容量预检改为克隆切片纯内存试算（零写库、零改 `r.turn`）；仅当候选通过预检、Cooldown 与 `BeforeModelCall`（且复核仍需 `TurnBoundary`）后正式提交 `OriginTurn`，再基于真实 `seq` 重确认最终投影（§4.8，测试 `T8`）。 | **CLOSED** |
| **NEW-03** | **P2** | 项目指令 `AGENTS.md` 提取章节大纲时若不识别 ```` ``` ```` 与 `~~~` 围栏，会把代码块中的 `# Heading` 注释误识别为章节标题。 | 采用 CommonMark 围栏状态机扫描，忽略反引号与波浪号围栏内部行，仅提取围栏外真实标题与 1-based 行区间，磁盘原文件零改动（§4.9，测试 `T5`）。 | **CLOSED** |
| **NEW-04** | **P2** | 四级有状态降级机（`Level 0..3` + `r.degradeLevel`）过度复杂，且单次工具大输出峰值会把整场 Run 永久锁入降级态。 | 独立裁决确认：**二元无状态 `{ModeFull, ModeCompact}` 全面优于四级状态机**。常态由候选固定前缀预算比决定，单步最新组峰值仅当步临时切 `ModeCompact` 而不跨步锁死（§4.4，测试 `T1, T2`）。 | **CLOSED** |
| **RC2-F01** *(本次新增)* | **P1** | **二次源码深挖发现**（`internal/window/window.go:75-106 & 154-166` 联动 `send.go:180`）：`OriginTurn` 的 `Role` 为 `"user"`，在 `window.Groups` 中会单独切成仅约 `10` tokens 的最新原子组！当主模型刚产出 `14K` tokens 的 `[assistant + tool]` 组后故障切往 `16K` 小候选时，`window.Build` 会仅装入 `10`-token 的 `[OriginTurn]`（`included = 1 > 0`，**不触发 `ErrOverflow`**），静默丢弃刚执行的 `14K` 工具结果，向备选模型发出只有 `[turn boundary]` 的失忆请求！ | 在 §4.8 `projectCandidate` 中增加 **`TurnBoundary` 最小进展守卫（Minimum Progress Guard）**：凡插入 `OriginTurn` 且紧邻其前存在待消化消息组时，强制要求 `included >= 2`（必须同时容纳前序 `[assistant+tool]` 原子组与 `OriginTurn`），否则返回 `window.ErrOverflow` 继续向更大窗口模型 Failover（§4.8，测试 `T8`）。 | **CLOSED** |

---

### 3.5 `v0.4-rc2 -> v0.4-rc3` 独立复核台账（`RC3-F01`–`RC3-F08` 及 `Candidate Profile` 专项历史记录）

> **历史保留与演进覆盖声明：** 本节完整保留 `v0.4-rc2 -> v0.4-rc3`（SHA256: `4d91edd4...`）的历史审查台账不做删改；其后 `v0.4-rc3.1`（§3.6）、`v0.4-rc3.2`（§3.7）、`v0.4-rc3.3`（§3.8）、`v0.4-rc3.4`（§3.9）与 `v0.4-rc3.5`（§3.10 及第 `四`–`七` 节）完成的递进收紧均以第 `3.10` 节与第 `四`–`七` 节最终规格为准。

#### A. 源码现实四态能力边界声明（基于 `c73d17359f5d9dd16ef88b0de4525862593fddaa` 实证区分）

1. **已经实现的能力（Existing in `c73d173`）：**
   - `internal/window/window.go:59-197`：原子消息组切分 `Groups`、固定前缀汇总 `Fixed`、含当轮 `EchoTokens` 的组开销 `GroupTokens`、连续后缀贪心选择 `Select`/`Build` 及单模型减半退避 `CapacityRetry`；
   - `internal/agent/transcript.go:28-183`：`runTranscript` 内存缓存、遇 `Role == "user"` 自动清空 `r.turn` 与尾段 `EchoTokens`、`requestEcho` 对齐及按 `maxContext` 裁剪超长前导组；
   - `internal/agent/execution.go:55-331`、`callback.go:240-374`、`editing/engine.go:254-632`、`session/executions.go:52-395`：副作用工具执行分级（`tierNone / tierAdmit / tierFiles / tierWorkspace`）、目标文件前后 Git Blob 快照（`SnapshotFiles`，覆盖工作区内含 `.gitignore` 在内的 `<= 4 MiB` 常规文件）与 `git status` 可见子集状态快照（`SnapshotWorkspace`，排除 Git-ignored 文件且无执行前 Blob）、`editing.Engine.Commit` 单路径锁与打开描述符 CAS 写入、`derived_origins` 原子结算与 `apply_experience` 嵌套动作逐条结算；
   - `internal/agent/loop.go:458-573`、`verifygate.go:43-208`、`verify/gates.go:177-247`、`taskstate/verification.go:122-148`：六类完成阻断路径、单次 `Loop.Run` 内 `r.attempts` 修复配额、`Hooks.ResetDenials` 与 `SetVerification` 最近一次门禁结果快照覆盖写；
   - `internal/toolset/files.go:26-99` & `editing/doc.go:70-105, 198-213`：`read_file` 在 `anchors=true` 下基于 `editing.DecodeDoc` 的 `start_line / end_line` 精确分页与 Hashline 渲染（`anchors=false` `files.go:49-65` 时忽略行号参数）；
   - `internal/skills/registry.go:278-341` & `runtime/skills.go:20-36`：工作区信任隔离（`ADR-0013` `Withheld`）与 `newSkillReader` 白名单绑定；
   - `internal/agent/envelope.go:58-166`、`session/requests.go:48-478` & `session/start.go:70-220`：`AdmitRequest` 预发送同步信封落盘、`BeginRun` 事务血缘与 `source_fences` 隔离。
2. **本次设计的目标能力（`SPEC_RESOLVED` — 规格已闭环，待 Astra 编码）：**
   - `RC3-F01`–`RC3-F08`、`Candidate Profile`、`RC3-NEW-01`–`RC3-NEW-07`、`REV-F01`–`REV-F05`、`REV-F07`、`RC33-F01`–`RC33-F03`、`RC34-F01` 以及本轮最终定点 `RC34-F02` 全部收敛规格（详见 §3.8–§3.10 与 §4–§6）。
3. **尚未实现的能力（`IMPLEMENTATION_PENDING`）：**
   - 本文件第 `5.1` 节列出的全部 Go 文件改造、`T1–T12` 自动化回归测试，以及显式保留为实施阶段强制要求的 `REV-F06`（P3：显式 `ProfileCompact` 下内置工具安全约束保留与 `ReplayDecision` 新增规则分支）均尚未写入生产仓库；`Provider` 缓存命中统计字段（`V03-F07`）留作独立后续演进。
4. **无法通过现有源码证明的能力（`UNVERIFIED`）：**
   - 远端闭源 Provider（Anthropic / OpenAI / DeepSeek）未公开的服务端内部系统提示词包装开销与专有 BPE 词表在任意未知输入下的精确 Token 数；真实线上 Prefix Cache 命中率与驱逐 TTL。

#### B. `RC3-F01` 至 `RC3-F08` 及 `Candidate ProfileCompact` 历史处理矩阵（`v0.4-rc3` 记录）

| 编号 | 级别 | 真实源码依据与问题本质 | `v0.4-rc3` 历史修订摘要（标注后续版本收紧处） | 规格状态 / 实施状态 |
| :--- | :--- | :--- | :--- | :--- |
| **RC3-F01** | **P1** | `window/window.go:64-106, 158-197`、`agent/loop.go:458-573`、`transcript.go:126-156`：释放 `EffectiveGateSlack=0` 不等于保护上下文；小体积 `[提案 + OriginCompletionBlocked]` 可单独装入而静默丢弃前序代码修改依据组。 | 引入 `minRequiredSeq` 硬下界守卫与 `r.groups` 防提前裁剪（*注：已在 `v0.4-rc3.5` 收敛为绑定 `ExecutionID` 的 `Engine.Commit` 实际落盘 + 前后快照联合因果封签 `MutationEvidence`*）。 | `SPEC_RESOLVED` / `IMPLEMENTATION_PENDING` |
| **RC3-F02** | **P1** | `window/estimate.go:19-36`：区分 `EstimateBytes([]byte)` 与 `ReserveTokensForBytes(maxBytes)`，废除 `/2` 折算并诚实声明字符启发式边界。 | 确立四项 Token 单位预算公式（*注：已在 `v0.4-rc3.1` 完成 `2458B / 12346B` 序列化双守卫与 `1456B -> 854` 算术修正*）。 | `SPEC_RESOLVED` / `IMPLEMENTATION_PENDING` |
| **RC3-F03** | **P2** | `agent/loop.go:195, 241`、`verifygate.go:105`、`taskstate/verification.go:122-135`：`r.attempts` 每次 `Run/Resume` 从 `nil` 起算，`SetVerification` 为整块覆盖写。 | 拆清当次 `Run` 内存预算、最近次 `TaskState.Verification` 覆盖快照与跨 Run 追加审计三层语义（§4.6）。 | `SPEC_RESOLVED` / `IMPLEMENTATION_PENDING` |
| **RC3-F04** | **P2** | `agent/transcript.go:23-33`、`envelope.go:20-28`、`policy.go:790-995`：内存 Thinking Echo 不落盘，外部文件/MCP 可变。 | 划定严格五级边界，明确声明不支持离线 `Full Projection Replay`（§4.11）。 | `SPEC_RESOLVED` / `IMPLEMENTATION_PENDING` |
| **RC3-F05** | **P2** | `agent/loop.go:328-342`、`send.go:178-230`、`policy.go:144-168`：消除预筛与 Dry-Run 双套动态容量公式。 | `CandidatesByStaticPrefix` 仅管静态资格，`projectCandidate` 为唯一动态容量权威，克隆清零尾段 `EchoTokens`（§4.1, §4.8）。 | `SPEC_RESOLVED` / `IMPLEMENTATION_PENDING` |
| **RC3-F06** | **P2** | `toolset/files.go:47-99`：`read_file` 在 `anchors=false` 时忽略行号参数。 | 紧凑大纲指引与 `T5` 强制携带 `anchors=true`，行号切分严格同构于 `editing.DecodeDoc`（§4.9）。 | `SPEC_RESOLVED` / `IMPLEMENTATION_PENDING` |
| **RC3-F07** | **P2** | `skills/registry.go:278-341`、`toolset/skills.go:12-35`、`runtime/skills.go:20-36`：超大规模启用技能超出内联目录上限时不可发现。 | 划分五类技能集合并原位扩展 `read_skill(query, offset)`（*注：已在 `v0.4-rc3.2/rc3.3` 完成双模 Schema 打通、Run 级不可变 `SkillSnapshot` 与单根 `128` 截断告警*）。 | `SPEC_RESOLVED` / `IMPLEMENTATION_PENDING` |
| **RC3-F08** | **P2** | `toolset/mcp.go:25-56`、`agent/presentation.go:32-55`：废除 `320B` 盲截断与关键词挑选。 | 保留自然语言业务约束，默认保留代码围栏 + 四条件正证折叠纯示例（§4.9）。 | `SPEC_RESOLVED` / `IMPLEMENTATION_PENDING` |
| **Profile 专项** | **P2** | `config/types.go:509-541`、`runtime/models.go:198-219`：恢复候选 `ProfileCompact` 独立最高优先级与 `ProfileDetailed` 跳过 `50%` 规则。 | 三态语义正交且不篡改 Run 级治理上限（§4.2, §4.4，测试 `T11`）。 | `SPEC_RESOLVED` / `IMPLEMENTATION_PENDING` |

---

### 3.6 `v0.4-rc3 -> v0.4-rc3.1` 源码复核历史台账（`RC3-NEW-01`–`RC3-NEW-07`）

> **历史保留与演进覆盖声明：** 本节完整保留 `v0.4-rc3.1`（SHA256: `8dfa5398...`）的历史修订记录不做删改；其中被后续 `v0.4-rc3.2`（§3.7）与 `v0.4-rc3.3`（§3.8）修正收紧的条目已在状态列明确标注。

| 编号 | 级别 | 真实 Go 源码核查依据（`c73d173`）与问题根因 | `v0.4-rc3.1` 历史修订摘要（标注后续修正处） | 规格状态 / 实施状态 |
| :--- | :--- | :--- | :--- | :--- |
| **RC3-NEW-01** | **P1** | `protocol/agent.go:88-131`、`loop.go:938`、`agent/execution.go:55-90, 240-322`：静态工具权限与 `MutatesWorkspace` 不等于实际工作区修改，`session_messages` 不存执行布尔值。 | 提出三值 `MutationEvidence` 初版（*注：已在 `REV-F01/F04`、`RC33-F01`、`RC34-F01` 与 `RC34-F02` 收紧为绑定 `ExecutionID` 的 `Engine.Commit` 实际落盘 + 前后快照联合因果封签*）。 | 已被 `RC34-F01` & `RC34-F02` 覆盖收紧 |
| **RC3-NEW-02** | **P1** | `agent/presentation.go:61-92`、`window/estimate.go:19-36`：`protocol.Message` 58B 外壳与 Go `encoding/json` `2x/6x` 转义膨胀击穿 `256B` 假设。 | 在 `presentFeedback` 施加原始字节（`1200B / 6144B`）+ 实测序列化 JSON 字节（`2458B / 12346B`）双守卫，`BaseGateSlack` 精确锁定 `1950 / 7718` tokens（§4.5，测试 `T6`）。 | `SPEC_RESOLVED` / `IMPLEMENTATION_PENDING` |
| **RC3-NEW-03** | **P2** | `ReserveTokensForBytes(1456)` 精确值为 `854`（`+512 = 1366`），`v0.4-rc3` 误算为 `853`。 | 修正全部整数算术并在 `T6` 固化单调性与边界黄金值测试（§4.5，测试 `T6`）。 | `SPEC_RESOLVED` / `IMPLEMENTATION_PENDING` |
| **RC3-NEW-04** | **P2** | `skills/types.go:13` 单条描述 `1024B`，纯 `50` 条分页响应体积无界。 | 增设 `MaxSkillCatalogResponseBytes = 16384` 与单条超限 `DescriptionTruncated` 保底（*注：已在 `REV-F02` 与 `RC33-F02` 补齐双模接线与 Run 级不可变 `SkillSnapshot`*）。 | `SPEC_RESOLVED` / `IMPLEMENTATION_PENDING` |
| **RC3-NEW-05** | **P2** | `toolset/mcp.go:45` 空格拼接 `(MCP server: <id>)`，泛化删围栏易误伤规范约束。 | 先剥离后拼回 `(MCP server: <id>)`，默认 100% 保留代码围栏，仅只读工具满足四项正证条件时折叠纯示例围栏（§4.9，测试 `T1, T9`）。 | `SPEC_RESOLVED` / `IMPLEMENTATION_PENDING` |
| **RC3-NEW-06** | **P2** | `schema_v6.golden.txt:22` `CHECK (decision_json IS NULL OR state = 'answered')` 禁止失败信封写 `decision_json`。 | 在 `AdmitRequest` `Refs` 头部持久化候选 `ModelRef`（*注：其 `model:sha256:` 回退已在 `REV-F03/F07` 与 `RC33-F03` 升级为带末片终结符的有序分片离线无损双射*）。 | 已被 `REV-F03/F07` & `RC33-F03` 覆盖升级 |
| **RC3-NEW-07** | **P2** | `Mode` 选择与 `GateSlack / UsableInput` 计算顺序循环依赖。 | 重排 `projectCandidate` 为 7 步单向无环计算序列（§4.1, §4.3, §4.4）。 | `SPEC_RESOLVED` / `IMPLEMENTATION_PENDING` |

---

### 3.7 `v0.4-rc3.1 -> v0.4-rc3.2` 对抗审查历史台账（`REV-F01`–`REV-F07`）

> **历史保留与 `v0.4-rc3.3` 补强声明：** 本节完整保留 `v0.4-rc3.2`（SHA256: `db6980ed...`）的历史台账记录；针对 `REV-F01`（MCP 跨 `Resume` 权限一致性）、`REV-F02`（Run 级不可变 `SkillSnapshot`）与 `REV-F05`（`128/129` 极限槽位优先级与末片终结符防部分解码）的最后三项补充收敛详见第 `3.8` 节与第 `四`–`七` 节。

| 编号 | 级别 | 真实 Go 源码实证依据（`c73d173`）与问题根因 | `v0.4-rc3.2` 修订摘要（标注 `v0.4-rc3.3` 补强处） | 规格状态 / 实施状态 |
| :--- | :--- | :--- | :--- | :--- |
| **REV-F01** | **P1** | `repository/snapshot.go:127-222` & `agent/execution.go:60-70, 292-322`：`SnapshotWorkspace` 无执行前 Blob，`HEAD` 变化恒为 `CoveragePartial`（死分支），`touch / git add` 与只读 `edit_go(preview=true)` 并发 touch 均会产生伪 `file` 边。 | 明确否决保留 `edit_go / lsp_rename / eval_artifact` 为 `MutationProven` 的建议，仅成功且前后 Git Blob 变化的 `tierFiles` 判 `MutationProven`（*注：已在 `RC33-F01`、`RC34-F01` 与 `RC34-F02` 完成 MCP/Workspace 全部收紧及 `tierFiles` Commit 因果封签*）。 | `SPEC_RESOLVED` / `IMPLEMENTATION_PENDING` |
| **REV-F02** | **P1** | `toolset/skills.go:24-28` 固定 `required: ["id"]` 拦截无 `id` 分页；`runtime/skills.go:18` `map` 遍历无序；`registry.go:426` 单根 `128` 静默截断。 | 打通无顶层 `required`/`oneOf` 的双模 `read_skill` Schema、互斥校验、有序结构体与单根 `128` 截断告警（*注：已在 `RC33-F02` 补齐含 `SKILL.md` 正文的 Run 级不可变 `SkillSnapshot` 与并发锁*）。 | `SPEC_RESOLVED` / `IMPLEMENTATION_PENDING` |
| **REV-F03** | **P2** | `model:sha256:<digest>` 在配置删除后无法离线还原模型字面名。 | 废除单向哈希回退，改为有序分片百分号转义无损双射（*注：已在 `RC33-F03` 增补末片终结符 `.~` 彻底阻断截断部分解码*）。 | `SPEC_RESOLVED` / `IMPLEMENTATION_PENDING` |
| **REV-F04** | **P2** | `session/agent.go:22` 步中崩溃后 `NextStep` 重用 `step` 号，`toolCallRecord` 双写冗余。 | 删除 `toolCallRecord` 新字段，收敛为 `request_envelopes ⋈ derived_origins` 按唯一 `answered_seq` 原子组保守感染恢复（§4.6，测试 `T4`）。 | `SPEC_RESOLVED` / `IMPLEMENTATION_PENDING` |
| **REV-F05** | **P2** | `taskstate/render.go:60-85` 仅渲染开放单元、`<=2048B` 笔记、`<=10` 任务且在 `10240B` 截断，旧 `requestRefs` 塞入未渲染项耗尽 `128` 上限。 | 由 `taskstate.RenderBlock` 导出 `startByteOffset < cutoff` 实渲引用（*注：已在 `RC33-F03` 补齐 `10+7+111=128` / `129` 极限槽位与 `MaxRefsJSONBytes` 验证*）。 | `SPEC_RESOLVED` / `IMPLEMENTATION_PENDING` |
| **REV-F06** | **P3** | 显式 `ProfileCompact` 首句截断剪掉 `read_file / write_file / bash / eval` 防呆前提；`ReplayDecision` 需补新规则分支。 | 显式保留为实施阶段强制要求（P3 `IMPLEMENTATION_PENDING`），严禁宣称已通过生产测试（§4.9.4, §4.11, §7.2）。 | `SPEC_RESOLVED`（P3 实施契约） / `IMPLEMENTATION_PENDING` |
| **REV-F07** | **P2** | JSON Schema `maxLength` 统计 Unicode 码点而非 UTF-8 字节（单码点 `4B UTF-8 -> 12B %XX`）。 | Go `config` 强制 `MaxProviderIDBytes=64`、`MaxModelKeyBytes=192`，严格证明 `769B` 转义上限与最多 `10` 片分片上界（§4.10.2，测试 `T12`）。 | `SPEC_RESOLVED` / `IMPLEMENTATION_PENDING` |

---

### 3.8 `v0.4-rc3.2 -> v0.4-rc3.3` 最终定点补强与一致性收敛台账（`RC33-F01`–`RC33-F03`）

本节记录针对 `v0.4-rc3.2` 最后三项补充审查议题对照 `c73d17359f5d9dd16ef88b0de4525862593fddaa` 真实源码开展的终审裁决与全篇净化记录：

| 编号 | 对应原项与级别 | 真实 Go 源码实证依据（`c73d173`）与问题根因 | `v0.4-rc3.3` 最终收敛方案与不变量证明（对应章节与测试） | 规格状态 / 实施状态 |
| :--- | :--- | :--- | :--- | :--- |
| **RC33-F01** | `REV-F01` 补充<br>**（P1）** | ① `mcp/registry.go:697-710` & `toolset/mcp.go:25-39`：MCP 工具 `Permission` 受用户 `mcp.json` 覆盖（`cfg.Tools[name].Permission`）及工作区信任切换（`resolveLocked` `395-403`）动态影响，且服务器在 `Resume` 前可能已卸载；而 `derived_origins` 不持久化历史 `Permission`（`read` 与 `write` 级 `mcp__*` 均为 `pre_ref = NULL`），导致 `v0.4-rc3.2` 依赖当前 `specs` 判定只读 MCP 的分支在配置修改后产生跨 `Resume` 分类漂移（且 §4.6 第 654 行残留了未清理的多轮自我讨论）；<br>② **盘点全部 `tierAdmit` 路径的新增源码发现（`agent/callback.go:288-290`）**：`if tier == tierNone && d.application { tier = tierAdmit }` 会将 `apply_experience` 内部调用的固定内置只读工具（如 `read_file, search_text`）提升为 `tierAdmit` 写入 `derived_origins`（`pre_ref = NULL`）！ | **移除动态 `specs` 参数，实现 100% 纯持久化历史时间不变性，并彻底清除未完成推理：** ① 从 `classifyExecutionSummary` 与 `aggregateGroupMutation` 函数签名中删除 `specs []protocol.ToolSpec` 参数，从类型系统上根除对当前工具配置的依赖；② **对所有已写入 `derived_origins` 的外部 `mcp__*` 调用（`strings.HasPrefix(rec.ToolName, "mcp__")`，无论 `read / write / exec`），在运行期与 `Resume` 一律无条件判为 `MutationUnknown`**（既消除跨 `Resume` 漂移，又避免 `exec` 与 `read` 权限倒挂悖论，且杜绝将远端成功响应误认作本地代码修复）；③ 对固定不可变的内置只读/委派工具名集合（`start_task, spawn_subagent` 及 `callbackAllowedBuiltins` 中的只读内置工具）在 `FileEdgeCount == 0` 下恒判为 `MutationNone`，精准兼容 `callback.go:289` 嵌套调用；④ 彻底清理原 §4.6 第 654 行全部口语推理痕迹（§4.6, §5.2，测试 `T3, T4`）。 | `SPEC_RESOLVED` / `IMPLEMENTATION_PENDING` |
| **RC33-F02** | `REV-F02` 补充<br>**（P2）** | ① `runtime/agent.go:420-458` & `runtime/prompt.go:171-237`：`prepareRun` 虽只调用一次 `Registry.Prompt`，但 `skills/registry.go:464-468` 在 `scanRoot` 读取 `SKILL.md` 全文（`item.content`）后并未随 `Skill` 保留，导致 `runtime/skills.go:35` 调用 `skills.ReadResource`（`registry.go:341-409`）时每次重新从磁盘打开 `SKILL.md`——若 Run 中途外部调用 `Registry.Delete` 删除了该技能或 `Registry.Save` 改写了 `SKILL.md`，就会发生「`Prompt` 与 `ListSkills` 显示 `skill-A` 启用，但 `ReadSkill` 报 `ENOENT` 或读到篡改后新版本」的快照撕裂；<br>② `skills/registry.go:21-24, 188, 519`：`Registry` 无并发写锁，且 `Save` / `saveState` 使用固定临时文件名 `path + ".tmp"`，并发修改同一技能/状态文件时存在 `.tmp` 覆写竞争。 | **建立 Run 级不可变技能快照 `SkillSnapshot`（零多余磁盘 I/O）与 `Registry` 并发写互斥：** ① `Registry.Prompt` 直接将 `scanRoot` 已读入内存的 `SKILL.md` 原文封装进不可变条目 `SnapshottedSkill`（随 `PromptResult.Eligible` 返回）；单次 `prepareRun` 构造唯一一份不可变 `SkillSnapshot` 同步供给 `LayerSkills`（`Text / CompactText`）、`SkillCataloger.ListSkills` 与 `SkillReader.ReadSkill`；② `ReadSkill(ctx, id, path)` 先校验快照 `byID` 白名单，当 `path == "" || clean == "SKILL.md"` 时 **100% 直接返回内存快照中的 `SKILL.md` 原文**（彻底免疫中途磁盘 `Delete / Save / SetEnabled`），仅对技能子目录附属参考文件在白名单通过后执行受限 `ReadResource`；③ `Registry` 增加内部 `sync.RWMutex` 并在原子写盘中使用唯一临时文件（`os.CreateTemp`），确保并发修改零数据竞争；④ `Resume` 启动新 Run 时重新执行 `prepareRun` 建立最新合法快照（§4.9.2, §5.1, §5.2，测试 `T10`）。 | `SPEC_RESOLVED` / `IMPLEMENTATION_PENDING` |
| **RC33-F03** | `REV-F05` 补充<br>**（P2）** | ① 在 `v0.4-rc3.2` 的 `model~0~..model~K-1~` 分片格式中，若最后一片因外部存储篡改或异常截断丢失（例如 3 片仅剩 `model~0~, model~1~`），在无配置文件的 `PurposeAutoEffort / PurposeReview` 信封中解码器无法仅凭下标 `0, 1` 识别尾片缺失，存在**静默部分解码**风险；<br>② 需对极限槽位分配（`10` 模型分片 + `7` 固定系统/版本引用 + `111` vs `112` `TaskState` 引用 `= 128` vs `129`）及 `MaxRefsJSONBytes = 16384` 给出精确字节级验证。 | **引入末片终结符 `.~` 封条阻断部分解码，并完成 `128/129` 槽位与 `12,673B` JSON 字节边界闭环验证：** ① 多片编码中非末片头保持 `model~<i>~`（`8B`，载荷 `88B`，保底推进 `>= 77B`），唯独最后一片采用带终结标记的 `model~<K-1>.~`（`9B`，载荷 `87B`；由 $769 - 9 \times 77 = 76 \le 87$ 严格证明最大分片数 **`K_max = 10` 恒定不变**！）；解码器 `DecodeEnvelopeModelRef` 强制要求分片序列以唯一的 `.~` 末片收尾，缺失末片 100% 报 `ErrIncompleteModelRefChunks`，同时准入前强制校验 `len(modelRefs) <= 10` 禁止截断模型分片；② 严格证明 `128` 条最长 `96B` `refRE` 字符串（零 JSON 转义字符）序列化最大字节数为 $2 + 128 \times 98 + 127 = \mathbf{12,673\text{ B}} < 16,384\text{ B}$（余量 `3,711B`）；③ 在 `T12` 增补 `10 + 7 + 111 = 128` 与 `10 + 7 + 112 = 129` 及 `MaxRefsJSONBytes` 实测断言（§4.10.2, §4.10.3, §6 `T12`）。 | `SPEC_RESOLVED` / `IMPLEMENTATION_PENDING` |

---

### 3.9 `v0.4-rc3.3 -> v0.4-rc3.4` `WorkspaceSnapshot` 覆盖边界与 `MutationEvidence` 定点收敛台账（`RC34-F01`）

> **演进补强说明：** 本节完整保留 `v0.4-rc3.4`（SHA256: `a0fa1ec6...`）的历史台账原文；其中对内置 `tierFiles` 单独凭 `PreRef != PostRef` 判定 `MutationProven` 的条件，已在紧接的第 `3.10` 节（`RC34-F02`）进一步收紧为「每 `ExecutionID` 绑定的 `Engine.Commit` 实际落盘 + 写后磁盘 SHA-256 一致性复核 + 前后 Git Blob 净变化」三重联合封签。

| 编号 | 级别 | 真实 Go 源码实证依据（`c73d173`）与问题根因 | `v0.4-rc3.4` 收敛方案摘要（标注 `v0.4-rc3.5` `RC34-F02` 补强处） | 规格状态 / 实施状态 |
| :--- | :--- | :--- | :--- | :--- |
| **RC34-F01** | **P1** | ① `internal/repository/snapshot.go:103-155` & `snapshot_test.go:63-91`：`SnapshotWorkspace` 仅执行 `git status --porcelain=v2 -z --branch --no-ahead-behind --untracked-files=all`，**完全排除所有匹配 `.gitignore`、`.git/info/exclude` 与全局排除规则的 Git-ignored 文件/目录**，且其 `Complete` 字段仅代表标准输出未在 `MaxStatusBytes (512 KiB)` 处截断，绝非全工作区文件系统覆盖；当 `bash`、`edit_go`、`lsp_rename` 或 `eval_artifact(action="export")` 修改、新建、删除或批量生成 Git-ignored 文件（如 `build/generated.go`、`.env`）时，持久化记录呈现 `Coverage == CoverageComplete && PreRef == PostRef && FileEdgeCount == 0`，旧 `v0.4-rc3.3` 分支 3 将其误判为 `MutationNone`，导致首次门禁阻断前 `deriveRepairContext` 漏锁 `preBlockFloor`，使真实修改生成物的工具调用可被后续滑窗静默挤出！<br>② `internal/repository/snapshot.go:46-95` & `blobs.go:18-67`：核实 `SnapshotFiles` 调用 `git hash-object -- <files...>` **完全不受 `.gitignore` 影响**，可精确哈希含忽略文件在内的工作区常规文件，但当参数缺失合法路径时 `Ref()` 返回 `"files:0:sha256:..."`（零目标），且失败调用（`Outcome != OutcomeComplete`）不应混入 `MutationNone`；同时 `loop.go:535-537` 在 `RecordExperienceActions == true` 时亦会将 15 个内置只读工具提升为顶层 `tierAdmit`。 | **采纳方案 A（保守三值分类收敛），重定七维证据边界，彻底废除凭 `WorkspaceSnapshot` 判定 `MutationNone`：** ① 全篇明确界定 `WorkspaceSnapshot` 仅覆盖 `git status` 可见子集，**全部内置 `tierWorkspace`（`bash` 前后台、`edit_go`、`lsp_rename`、`eval_artifact`）无论 `PreRef` 与 `PostRef` 是否相等一律无条件判为 `MutationUnknown`**（根除反例 A–E 伪阴性与反例 F/G 伪阳性，连带消去冗余参数 `hasBackgroundBash`）；② 内置 `tierFiles` 一切失败/空目标/越界/超限调用统一判为 `MutationUnknown`（*注：其 `OutcomeComplete` 下的因果归属已在 §3.10 `RC34-F02` 补齐 `Engine.Commit` 实际落盘联合封签*）；③ 固定白名单 `immutableNonMutatingAdmittedTools` 同步覆盖 `loop.go:535` 顶层与 `callback.go:289` 嵌套只读准入；运行期步末与 `Resume` `r.load` 统一走 `Store.ListAnsweredExecutions`，配合 `r.groups` 的 `groups[keep-1][last].Seq >= minKeepSeq` 防裁剪钳位。 | `SPEC_RESOLVED` / `IMPLEMENTATION_PENDING` |

---

### 3.10 `v0.4-rc3.4 -> v0.4-rc3.5` `tierFiles` 因果归属与跨 `Resume` 证据封签最终定点收敛台账（`RC34-F02`）

| 编号 | 级别 | 真实 Go 源码实证依据（`c73d173`）与问题根因 | `v0.4-rc3.5` 最终收敛方案与不变量证明（对应章节与测试） | 规格状态 / 实施状态 |
| :--- | :--- | :--- | :--- | :--- |
| **RC34-F02** | **P1** | ① **`SnapshotFiles` 处于 `Engine.Commit` 文件锁窗口外，无法区分宿主并发修改与 Agent 实际写入（场景 B 致命反例）：** `admitExecution`（`agent/execution.go:163`）与 `postFiles`（`255-282`）在 `Engine.Commit`（`editing/engine.go:254-325`）单路径锁之外采样 `PreRef` 与 `PostRef`。核查 `engine.go:420-431` 证实：当 `BaseRev == ""` 且 `RejectStaleRead == true` 时，若当前会话此前未曾读取目标路径（`!ok && !e.snaps.incomplete(session)`）或进程重启 `Resume` 清空了纯内存 `SnapshotStore`（`editing/store.go:25-26`），`RejectStaleRead` **直接放行**！紧接着若宿主先将文件从 `A` 改为 `B`，Agent 随后调用 `write_file(path, B)`，`engine.go:527-529` 因 `string(p.next) == string(p.original)` 直接 `continue` **跳过磁盘写入（`p.wrote = false`）** 并返回 `OK: true`（`OutcomeComplete`），但 `postFiles` 测得 `PreRef(A) != PostRef(B)` 且生成 `DerivedEdge`——`v0.4-rc3.4` 将该零写入调用误判为 `MutationProven`，在门禁修复期会错误推进 `MinRequiredSeq` 挤出未修复的报错上下文！<br>② **工具层丢弃 `Engine.Commit` 写入证据、部分写入回滚不完整及 `edit.committed` 非持久化：** `toolset/{files,patch,editing}.go` 丢弃了 `p.wrote` 且硬编码 `MutatesWorkspace: true`；`engine.go:590-632` 单文件 `f.Truncate(0)` 后写失败不回滚、多文件 `e.rollback` 忽略错误（`_ = ...`）；`runtime/events.go:255-260` 显式令 `"edit.committed"` 返回 `durableEvent == false`（不入库、无 `ExecutionID`、慢订阅可丢包）；且 `derived_origins` 原先未记录 `Engine.Commit` 是否实际落盘，导致 `Resume` 无法区分场景 A 与场景 B。 | **采纳方案 A + C1 融合架构（宿主私有 Context 逐 `ExecutionID` 绑定 `Engine.Commit` 落盘回执 + `postFiles` 三重联合核验 + `derived_origins.post_ref` 原子因果封签）：** ① `editing.Engine.Commit` 导出 `CommitReceipt`（逐文件记录全字节判定的 `Wrote`、`Created`、`Deleted`、`OldSHA256`、`NewSHA256` 及 `writeCAS` 全字节重读校验，并在 `*editing.Error` 上精确标记进入写盘窗口或回滚失败的 `DirtyFailure`）；② 在 `loop.go` 与 `callback.go` 每次工具准入后，将仅关联当前单一 `ExecutionID` 的宿主私有 `commitRecorder` 注入 `toolCtx`（嵌套回调覆盖隔离，模型参数与工具文本无法伪造），由 `toolset` 的 `commitPlan` 回填真实 `CommitReceipt`；③ `postFiles` 执行三重联合核验：唯独 `Outcome == OutcomeComplete && Coverage == CoverageComplete && hasPositiveFilesSnapshotRef(PreRef)` 且 `CommitReceipt` 证实至少一个目标文件 `Wrote == true`、所有 `!Wrote` 目标满足 `PreBlob == PostBlob`、所有 `Wrote` 目标满足 `PreBlob != PostBlob` 且写后磁盘文件在 `SnapshotFiles` 前后夹逼校验满足 `SHA256 == NewSHA256`（删除文件满足不存在且 `PostBlob == "-"`）时封签 `;cm=proven`；唯独全部目标 `Wrote == false` 且 `PreBlob == PostBlob`（`PreRef == PostRef && len(changed) == 0`）时封签 `;cm=none`；其余一切并发竞争（场景 B / C2 / C3 / E）、部分失败（场景 F，且 `DirtyFailure` 升格 `OutcomeUnknown`）与未封签旧记录一律封签/归类为 `;cm=unknown`（`MutationUnknown`）；④ 封签随既有 `CommitToolResult / SettleExecution` 单事务写入 `derived_origins.post_ref`（形如 `files:<N>:sha256:<hex>;cm=proven`，`<= 91B << 512B`，严守 2 次写库门禁与零 Schema 迁移，§4.6.3–§4.6.5, §5.1, §5.2，测试 `T3, T4`）。 | `SPEC_RESOLVED` / `IMPLEMENTATION_PENDING` |

### 3.11 `v0.4-rc3.5 -> v0.4-rc3.6` Opus 5.5 独立盲审发现与长程可靠性闭环台账（`F-01`–`F-09`）

| 编号 | 级别 | Opus 5.5 独立盲审发现与源码复现根因（`b57655c`） | `v0.4-rc3.6` 规格修订与修复方案摘要 | 规格状态 / 实施状态 |
| :--- | :--- | :--- | :--- | :--- |
| **F-01** | **P1** | `internal/editing/engine.go:785-804` 创建型写入 `writeCAS` 以非独占 `O_RDWR\|O_CREATE` 打开路径并令 `createdOnOpen = !p.existed`，当 `prepare` 与 `Open` 之间外部进程创建了同名文件时，CAS 冲突或 `CodeTooLarge` 分支调用 `failClean -> os.Remove(p.abs)` **直接删除外部新建文件**；`rollback` 删除前亦未复核文件身份。 | 创建型 `Open(path, true)` 强制使用 `os.O_RDWR \| os.O_CREATE \| os.O_EXCL`（`os.ErrExist` 立即无脏退回 `CodeConflict` 零删除）；既有文件修改使用 `Open(path, false)`；仅本次 `O_EXCL` 真正创建的文件记录平台文件身份 `fileIdentity`（Unix `Dev/Ino` + `os.SameFile`；Windows `GetFileInformationByHandle` `VolumeSerialNumber + FileIndex + CreationTime`），清理与 `rollback` 删除前必须重新验证路径仍指向同一常规文件（`rollback` 同时校验字节未变）。 | `SPEC_RESOLVED` / `REMEDIATION_IN_PROGRESS` |
| **F-02** | **P2** | `internal/agent/transcript.go:211-251` 运行期 `!repair.Active` 时 `r.groups(l.maxContext(), -1)` 裁剪旧组并删去 `r.mutations[seq]`，门禁打回后在线 `MinRequiredSeq` 仅从残余尾部起算，而冷启动 `Resume` `r.load` 从 `WindowStartSeq` 全量加载正文导致 `MinRequiredSeq` 分叉且长程内存无界。 | 分离有界消息正文缓冲（`r.transcript <= l.maxContext()`，移除超 `maxContext` 正文强留钳位）与零正文轻量历史组元数据 `HistoryGroupMeta`（含 `Seq, Role, Origin, HasToolCalls, AnsweredSeq, ToolCallID, ExecutionID, MutationEvidence` 及多调用映射，内存封顶 `MaxInMemoryHistoryGroups = 2048` 并在超限时经 `EachMessageFrom` 从 SQLite 流式重算，新外部用户输入重置旧修复周期），运行期与 `Resume` 100% 共享同一 `DeriveRepairContext`。 | `SPEC_RESOLVED` / `REMEDIATION_IN_PROGRESS` |
| **F-03** | **P2** | 长回合（如 40 步 `bash` 约 `40K` tokens）后触发门禁拒绝时，单一或最大可用 `32K` 模型无法容纳完整 `MinRequiredSeq`，按 `v0.4-rc3.5` 硬阻断直接报 `KindWindowOverflow` 终止可恢复任务。 | 采纳**带 Failover 优先守卫的方案 B（Guarded Floor Relaxation）**：① 先在当前候选检查严格 `MinRequiredSeq`；② 不满足时优先 Failover 更大窗口候选（小模型严禁越级触发放宽）；③ 仅当全部更大候选均耗尽/不可用时，检查最大可用候选能否同时容纳活跃门禁反馈（`BlockSeq`）、最新原子组与显式放宽告警；④ 能容纳则进入受控 Floor Relaxation（保留 `OriginalMinRequiredSeq`、记录 `SentFirstSeq` / 模型 / 原因、持久化 `RuleWindowFloorRelaxed` 与 `RuleFailoverFloorFallback` 决策并接入 `ReplayDecision` & `VerifyStep`、在请求状态块显式通知 Agent 省略历史并要求改码前重检工作区）；⑤ 连门禁反馈或最新原子组也装不下时受控报 `KindWindowOverflow`。同步修订原不变量 9/10 与 `T3-A2`。 | `SPEC_RESOLVED` / `REMEDIATION_IN_PROGRESS` |
| **F-04** | **P3** | `internal/toolset/debugger.go:72-97` 中全部 10 个 `debug_*` 工具均具 `Permission: "exec", MutatesWorkspace: true`，但 `executionTierOf`（`agent/execution.go:61-98`）漏匹配落入 `tierNone`，被静默归为 `MutationNone`。 | `executionTierOf` 将 `strings.HasPrefix(name, "debug_")` 归入 `tierWorkspace`（恒判 `MutationUnknown`），补入 `callbackDeniedPrefixes`，并在历史组聚合层对含 `debug_*` 工具调用施加防御性 `MutationUnknown` 兜底。 | `SPEC_RESOLVED` / `REMEDIATION_IN_PROGRESS` |
| **F-05** | **P3** | `runtime/prompt.go` 的 `EffectivePrompt` 诊断输出与候选实际 `ModeCompact` Wire 请求存在静态/动态视图差异。 | 作为已知 P3 诊断边界保留于审查台账（不删改、不自封 `CLOSED`）。 | `TRACKED_P3` |
| **F-06** | **P3** | `8K` 小窗口模型在 `Policy: "full"`（22 个内置工具）与固定 `BaseGateSlack` 下静态前缀与常态可用池紧张，此前两处 `runtime` 单测被迫从 `8000` 调大至 `12000`。 | **明确 `8K`（`Context >= 8000`）仍在支持范围内：** ① 当候选 `cand.Context < verbatimCompactFixed + guard + outBudget` 时，允许 `ModeCompact` 自动启用已通过 `REV-F06` 安全验证的内置工具紧凑描述以通过静态准入；② 常态非修复期 `EffectiveGateSlack` 按 `min(baseSlack, max(0, (poolBeforeSlack - candFixed) / 4))` 比例封顶（`>=32K` 模型保持 `1950/7718` 不变，`8K` 模型保留 75% 对话空间并恢复 `8000` 单测）；残余小窗口容量权衡继续保留于 P3 台账。 | `SPEC_RESOLVED` / `TRACKED_P3` |
| **F-07** | **P3** | `runtime/prompt.go` 与 `internal/agent/presentation.go` 的行级围栏扫描未完整解析嵌套列表/引用块容器前缀、多行 HTML 注释与 Setext 标题。 | 作为已知 P3 CommonMark 边角项保留于审查台账（不删改、不自封 `CLOSED`）。 | `TRACKED_P3` |
| **F-08** | **P3** | `internal/agent/presentation.go` 的 `presentFeedback` 存在第三套未与 `parseFenceMarker` 统一的围栏扫描器。 | 作为已知 P3 代码一致性项保留于审查台账（不删改、不自封 `CLOSED`）。 | `TRACKED_P3` |
| **F-09** | **P3** | `internal/agent/execution.go:476-481` `sealFilesMutation` 按切片下标配对 `receipt.Files[i]` 与 `SnapshotFiles` 结果，而 Windows 反斜杠 `\`（ASCII 92）与 `/`（ASCII 47）排序顺序不同（如 `"a\\b.go" > "a0\\x.go"` 而 `"a/b.go" < "a0/x.go"`），可致跨文件错配。 | `Engine.Commit` 输出 `FileCommitReceipt.Path` 与 `sealFilesMutation` 统一按 `filepath.ToSlash` 规范化路径键建立映射匹配（区分大小写、不依赖切片下标顺序、重复规范键保守判 `MutationUnknown`）。 | `SPEC_RESOLVED` / `CONFIRMED_SOURCE_LEVEL`（Windows 原生运行 `UNVERIFIED`） |

### 3.12 `v0.4-rc3.6 -> v0.4-rc3.7` Opus 5.5 第二轮独立增量审查台账（`R2-01`–`R2-05`）

| 编号 | 级别 | Opus 5.5 R2 独立增量盲审发现与探针复现（`a8b4786`） | `v0.4-rc3.7` 最终规格修订与闭环方案 | 规格状态 / 治理口径 |
| :--- | :---: | :--- | :--- | :--- |
| **R2-01** | **P2** | `F-01` 残留：当本调用以 `O_CREATE\|O_EXCL` 创建新文件后，另一并发进程/协程在同一 inode 原地写入内容（如 `echo x > f` 或编辑器保存），`writeCAS` CAS 冲突（`:829`）或并发写检测（`:854`）触发 `removeCreatedWhileOpen`，其仅比较文件身份而未证明文件内容未变，导致外部原地写入内容被误删（`editing` 400 次删 348 次，`toolset` 300 次删 298 次）。 | **确立硬安全不变量「宁可保留文件也不删除任何无法证明仍完全属于本次调用的数据」：** ① **主路径（方案 B：私有临时文件 + 原子 no-replace 发布）**：在目标同目录创建私有 `.lcx-create-<nonce>` 写入并校验全部字节与沙箱守卫，再经平台原子 no-replace 发布（Unix `link(2)` + 失败回退 `renameat2(RENAME_NOREPLACE)` / `renameatx_np(RENAME_EXCL)`，全程仅 `unlink` 私有临时文件；Windows `MoveFileExW(..., 0)`），若目标已存在则原子报 `CodeConflict` 且**绝不触碰目标路径**；② **自定义 `Host.Open` 与多文件 `rollback` 防线（方案 A+C）**：CAS 冲突、超限或 `CodeConcurrentWrite` 一律禁止删除目标文件；仅当打开句柄/路径同时满足「创建文件身份匹配 + 精确预期字节一致 + 纳秒时间戳未变」时才允许回滚删除，否则保守保留现场。 | `SPEC_RESOLVED` / `REMEDIATION_IN_PROGRESS` |
| **R2-02** | **P2** | `F-03` 残留：门禁拒绝后松弛下界恒锁 `<= BlockSeq`，因窗口为连续后缀，门禁拒绝后的修复步数一旦使 `[BlockSeq..LastSeq]` 超出窗口（`32K` 第 17 步、`64K` 第 33 步、`128K` 第 19 步、`200K` 第 30 步、`600K` 第 37 步），运行仍确定性死于 `KindWindowOverflow`。 | **Owner 正式选定方案 B（固定 Gate State + 受保护锚点滚动修复历史）：** 复用单一纯状态机 `repairStateMachine` 从持久化 `session_messages` 与 `ListAnsweredExecutions` 导出完整可恢复 `RepairContext`（含活跃 `GateFeedback`、`ProposalSeq`、`BlockCount`、`Stage`、`OriginalMinRequiredSeq`、`OriginalEvidence`、`PostBlockCausalFloorSeq` 与有界因果进展台账 `CausalProgress`），在全部更大候选耗尽后的最大可用候选上按三级单调顺序松弛：**Stage R1** 尝试保留 `[BlockSeq..LastSeq]` 原文（`State` 不重复存门禁反馈）；**Stage R2** 若 `[BlockSeq..LastSeq]` 超窗，将受 `presentFeedback` 封顶的固定 Gate State + 因果进展台账注入当步 `State`，消息窗仅在 `Seq > BlockSeq` 子集上锚定 `PostBlockCausalFloorSeq`（裁除阻断后旧只读/对话正文，保留全部阻断后 `Proven/Unknown` 组）；**Stage R3** 若阻断后因果组本身亦超窗，在固定 Gate State + 因果台账保护下滚动保留容纳最新原子组的最大后缀并记录 `GatePinnedInState=true` 的 `RuleWindowFloorRelaxed`。`State` 与 `Messages` 严格零重复，零新 SQLite 表，零第二套 Memory，在线与冷 `Resume` 全状态 `DeepEqual`。 | `SPEC_RESOLVED` / `REMEDIATION_IN_PROGRESS` |
| **R2-03** | **P3** | `FloorRelaxed` 仅向 `State` 注入提示通知并记录 `Decision`，并未在运行时拦截后续未重检的工具调用或完成提案，原报告将其表述为“强制重检/拦截”不属实。 | **Owner 正式选定保持 `ADR-0024` 现有边界：** 不新增运行时工具拦截硬守卫；严格区分三层机制——① `State` 提示通知（告知省略范围与修复台账）、② 内置行编辑/替换工具的 CAS / `base_rev` 校验（不宣称覆盖 `bash` 或 `mcp`）、③ 最终完成门禁重验；更正报告全部不实措辞并将 `R2-03` 诚实保留为 **`OPEN (P3)`**。 | `OPEN (P3)`（明确设计边界披露） |
| **R2-04** | **P3** | 超过 `MaxInMemoryHistoryGroups = 2048` 组后，`r.deriveRepairContext` 在每步每个候选尝试时重新从 SQLite 流式扫描并解码整轮消息（12,000 条约 `252 ms`/次），形成会话级 $O(N^2)$ 开销。 | 利用 `repairStateMachine` 严格前向结合律，在内存中维护有界增量基态 `baseSM` + 最近 `<= 2048` 组已结算/待结算 `historyGroups`：每满 `2048` 组将最老 `1024` 个已结算组折叠进 `baseSM` 并平移切片；`r.load` 单遍加载同步折叠，使步内任意候选调用 `deriveRepairContext` 均为 **`0` 次 SQLite 查询、$O(1)$ 有界耗时与有界内存**，并与全量流式扫描 100% 等价。 | `SPEC_RESOLVED` / `REMEDIATION_IN_PROGRESS` |
| **R2-05** | **P3** | 原实施报告存在不存在的函数/测试名（如 `removeIfSameCreatedFile`、`canonicalReceiptPathKey`、`historyMeta`）、通知字段描述偏差、包计数表述矛盾及缺少平台验证原始日志文件引用。 | 逐项核对真实 Go 符号与测试名，澄清全仓 `48` 个包 / `43` 个含测试包 / `41` 个非 `evalruntime` 且非 `runtime` 含测包，并将 macOS / Linux 容器 / Windows 交叉编译及 RED→GREEN、性能基准的完整原始日志归档至 `bench/evidence/adr-0024-r2-remediation-20261119/`。 | `SPEC_RESOLVED` / `REMEDIATION_IN_PROGRESS` |

---

## 四、`v0.4-rc3.7` 核心实现规格（直接支撑 `ADR-0024` 与 R2 审查闭环实施）

### 4.1 完整单步请求组装与门禁闭环时序图

```text
Engine.prepareRun (单次 Run / Resume 启动初始化 — 在 Run 锁保护下执行一次)
  |-- 1. assemblePrompt -> registry.Prompt(IncludeProject: plan.trusted):
  |      一次性扫描生成本场 Run 唯一的不可变 SkillSnapshot (含按 ID 升序排列的 Eligible 及预读 SKILL.md 正文)
  |      -> 同时供给 LayerSkills (Text / CompactText) 与 newSkillReader(prompt.Skills.Eligible)!
  |-- 2. Store.Reconcile + Store.BeginRun 提交运行起点事务
  v
Loop.Run (启动有界内存与轻量历史元数据加载阶段)
  |-- r.load(ctx, l.Store, sessionID, opts.Withheld, l.maxContext()):
  |      1. 调用 l.Store.ListAnsweredExecutions(ctx, sessionID, WindowStartSeq) 获取持久化执行摘要;
  |      2. 通过 EachMessageFrom 流式扫描 WindowStartSeq 起的 session_messages:
  |         - 维护当前外部用户回合起点 latestUserSeq 起的零正文 HistoryGroupMeta (上限 MaxInMemoryHistoryGroups=2048，超限置流式重算标志);
  |         - 同步裁剪 r.transcript 消息正文后缀使其累计 Token 严格有界于 l.maxContext() (绝不无界驻留历史正文!)
  v
Loop.Run (step S)
  |-- 1. applyPendingInputs(in=nil): 消化步间排队的 Inbox Edits / Steers / WorkerMessages
  |-- 2. 构造单步只读 Canonical Step Input 与确定性修复下界 (运行期 == Resume 100% 同构):
  |      - stepTools   := IncludedStepTools(l.Tools, stepToolsIn)
  |      - stateBlock  := taskstate.RenderBlock(goal, r.ts, r.wf)
  |      - repair      := r.deriveRepairContext(ctx, l.Store, sessionID) // 基于 HistoryGroupMeta / SQLite 流式重算!
  |      - groups      := r.groups(l.maxContext()) // 消息正文严格按 maxContext 有界裁剪，不破坏 HistoryGroupMeta!
  |
  |-- 3. CandidatesByStaticPrefix 静态资格预筛 (纯函数 — 支持 8K 小窗口安全内置紧凑回退)
  |
  |-- 4. Loop.send 两阶段候选遍历 (Pass 1 严格下界 -> Failover 优先 -> Pass 2 受控 Floor Relaxation):
         |-- Pass 1 (严格下界模式):
         |     按候选顺序执行 projectCandidate(..., allowFloorRelaxation=false):
         |     若 built.FirstSeq > minReqSeq -> 标记 strictFloorOverflowed[i]=true，优先 Failover 更大窗口候选 (零写库!)
         |-- Pass 2 (受控 Floor Relaxation — 仅当 Pass 1 全部更大候选均已耗尽/不可用且 repair.Active 时触发一次):
         |     选定最大可用候选 bestIdx (若 bestIdx != i 记录 RuleFailoverFloorFallback)，执行 projectCandidate(..., allowFloorRelaxation=true):
         |     要求同时容纳活跃门禁反馈 BlockSeq + 最新原子组 + [Context Window Notice — Floor Relaxation] 状态告警;
         |     记录 OriginalMinRequiredSeq / SentFirstSeq / RuleWindowFloorRelaxed; 若仍装不下则受控报 KindWindowOverflow!
```

### 4.2 Candidate Profile 治理边界与核心数据结构规范（闭合 `CSA-01`、`CSA-02`、`REV-F01`、`REV-F04`、`REV-F05`、`RC33-F01`、`RC34-F01`、`RC34-F02`、`F-01`、`F-02`、`F-03`、`F-04`、`F-09`）

#### 1. Run 级治理约束 vs Candidate 级显式展示偏好硬边界
- **Run 级治理不变量（在 `Engine.prepareRun` 一次性锁定，Failover 严禁篡改）：**
  - 宿主安全与授权边界：`Policy`（`read-only` / `workspace` / `full`）、`Delegation`、`ToolAllowlist`、`ToolDenylist`、`Workflow` 工具白名单；
  - 执行配额与门禁约束：`Options.MaxSteps`、`Options.MaxToolCallsPerStep`、`Options.Budget`（`MaxToolCalls`, `MaxModelRequests`, `MaxTokens`, `MaxCostUSD`）、`Options.Deadline`、`Options.Verification`（含 `MaxRepairs`）。会话起始模型的 `Profile.{MaxSteps, MaxToolCallsPerStep, Budget}` 仅在宿主未显式传参时提供 Run 初始默认值，一旦进入 `Loop.Run` 即为整场 Run 常量，**任何步内模型切换（Failover）绝不可放大或缩小上述治理边界**。
- **Candidate 级物理能力与显式展示偏好（随候选模型切换独立生效）：**
  - 模型物理能力：`Context`、`DefaultContext`、`Output`、`OutputSet`、`ToolCalling`、`Efforts`、`Price`、`ProviderPkg`；
  - 候选显式展示偏好：`agent.Model` 携带只读字段 `Presentation PresentationOptions`（由 `runtime/models.go:208-219` 从各候选自身的 `config.ResolvedModel.Profile.{ToolDescriptions, Feedback}` 独立拷贝；仅当 `Model.Presentation` 对应字段为空且直接单测注入了 `opts.Presentation` 时回退读取 `opts.Presentation`）。

#### 2. 核心 Go 数据结构规范（零新增包、零 SQLite Schema 迁移、零第二套文件账本、`agent_steps.toolCallRecord` 零改动）

```go
// internal/agent/types.go — 滑窗原子组级（answered_seq）三值工作区内容因果变更实证
// 契约：完全由持久化字段（request_envelopes ⋈ derived_origins + session_messages）纯函数决定，
// 零 agent_steps 字段扩展，零当前动态 ToolSpec 依赖，零独立文件账本，运行期与任意次 Resume 100% 恒等。
type MutationEvidence string

const (
	// MutationNone 表示持久化证据因果确认该原子组未发生任何本地工作区文件写入且前后目标文件内容未变：
	// 仅包含准入前拒绝未执行调用（无 execution 行）、编译期固定内置只读/状态/子会话委派工具（零 file 边且非 outcome_unknown）、
	// 纯私有内核 eval 工具，以及内置 tierFiles 满足 OutcomeComplete && CoverageComplete && hasPositiveFilesSnapshotRef(PreRef)
	// 且持久化 PostRef 携带显式无变更封签 ";cm=none"（Engine.Commit 零实际写盘 Wrote==false 且 PreBlob==PostBlob 且 FileEdgeCount==0）。
	MutationNone MutationEvidence = "none"
	// MutationUnknown 表示证据不足以同时证明「零写入且未变」或「由当前 Agent 实际写入造成净文件内容改变」：
	// 包含宿主并发修改后 Agent No-op 写相同内容（Wrote==false 但 PreRef!=PostRef）、宿主改 B 后 Agent 改回原 A（Wrote==true 但净 PreRef==PostRef）、
	// Agent 写入后被宿主并发覆盖（写后磁盘 SHA256 != NewSHA256）、CAS 冲突/陈旧读拒绝、单文件截断写失败或多文件部分写入回滚失败（历史；rc3.8 起无回滚，见场景 F）、
	// 未携带 ";cm=" 封签的历史旧行、全部内置 tierWorkspace（bash、edit_go、lsp_rename、eval_artifact）、全部已准入外部 mcp__*、
	// 崩溃中断（OriginToolInterrupted / outcome_unknown）及含任一 Unknown 的复合原子组。
	// 硬约束：MutationUnknown 在首次阻断前锁定潜在致错现场，在门禁阻断后绝不允许前移 MinRequiredSeq!
	MutationUnknown MutationEvidence = "unknown"
	// MutationProven 表示持久化证据同时证明「当前 Agent 执行了真实磁盘写入」且「目标文件内容确实由该写入产生了净变化」：
	// 组内至少存在一次内置 tierFiles（含顶层或回调嵌套）满足 Outcome == OutcomeComplete && Coverage == CoverageComplete
	// && hasPositiveFilesSnapshotRef(PreRef)，且其 PostRef 携带经 Engine.Commit 落盘回执 + 写后磁盘 SHA-256 夹逼核验 +
	// 前后 Git Blob 净变化三重验证封签的 ";cm=proven"，同时同一 answered_seq 原子组内无任何 MutationUnknown 或中断缺口。
	MutationProven MutationEvidence = "proven"
)

// internal/editing/engine.go — 单次 Engine.Commit 文件级实际落盘回执（RC34-F02）
type FileCommitReceipt struct {
	Path      string // 工作区内规范化相对斜杠路径（与 repository.FileState.Path 一致）
	Wrote     bool   // Engine.write 实际执行了 deleteCAS 或 writeCAS 落盘时为 true；p.next == p.original 跳过写盘时为 false
	Created   bool   // 新建文件
	Deleted   bool   // 删除文件
	OldSHA256 string // Commit 持锁读入的 p.original 全量原始字节 "sha256:<hex>"（不存在为 "-"）
	NewSHA256 string // Commit 实际写入并通过打开 fd 全字节重读校验的 p.next "sha256:<hex>"（删除为 "-"）
}

type CommitReceipt struct {
	Committed    bool                // Engine.Commit 整体无错完成时为 true
	DirtyFailure bool                // 错误发生在进入磁盘突变窗口后（Truncate 后写失败、CodeConcurrentWrite；历史 rollback 出错；rc3.8 起含已发布前缀后的失败或不确定发布）时为 true
	Files        []FileCommitReceipt // 按规范化路径排序的目标文件提交明细
}

// SystemLayer 扩展（types.go:124）
type SystemLayer struct {
	Kind        string // "global-instructions", "project-instructions", "skills"
	Source      string
	Text        string // ModeFull 完整层文本
	CompactText string // 组装期预生成的受约束紧凑层文本（global-instructions 恒为空）
}

// internal/skills/types.go — Run 级不可变技能快照条目（RC33-F02）
type SnapshottedSkill struct {
	Skill          // 扫描当刻锁定的技能元数据（ID, Name, Description, Scope, Source, Root, Path, RealPath...）
	Content string // scanRoot 同期读取的 SKILL.md 原始 UTF-8 正文（<= 128 KiB），供 ReadSkill(id, ""|"SKILL.md") 零磁盘依赖读取
}

// internal/taskstate/render.go — 渲染文本与真实暴露条目引用单一事实源（REV-F05）
type RenderedBlock struct {
	Text     string   // 经 MaxBlockBytes (10240B) 截断后的最终渲染文本
	ItemRefs []string // 严格满足 startByteOffset < cutoff 的实际渲染条目逻辑 ID
}

// internal/agent/send.go — 扩展现有 Model
type Model struct {
	Ref            config.ModelRef
	ProviderID     string
	ModelID        string
	Context        int
	DefaultContext bool
	Output         int
	OutputSet      bool
	ToolCalling    bool
	Efforts        []config.Effort
	Client         protocol.Provider
	Price          *config.Cost
	ProviderPkg    string              // "anthropic-messages" / "openai-compatible"
	Presentation   PresentationOptions // 候选自身的 ToolDescriptions 与 Feedback 显式偏好
}

// HistoryGroupMeta 记录当前外部用户回合内单个原子组的零正文轻量元数据（F-02），不持有任何消息 Content 正文。
// 内存缓冲上限为 MaxInMemoryHistoryGroups = 2048；超限时置 historyOverflowed = true 并通过 EachMessageFrom 流式重算。
const MaxInMemoryHistoryGroups = 2048

type HistoryToolCallRef struct {
	ToolCallID  string
	Name        string
	AnsweredSeq int64
	ExecutionID string
}

type HistoryGroupMeta struct {
	Seq              int64                // 原子组首条消息 seq（工具组即 answered_seq）
	LastSeq          int64                // 原子组末条消息 seq
	Role             string               // 组首角色 ("user", "assistant", "tool")
	Origin           string               // 组首来源 ("", "imported", "completion_blocked", "turn", ...)
	HasToolCalls     bool                 // len(assistant.ToolCalls) > 0
	ExpectedCalls    int                  // assistant.ToolCalls 数量
	PairedResults    int                  // 已配对 tool 结果行数
	HasInterrupted   bool                 // 组内是否含 OriginToolInterrupted
	HasDebugToolCall bool                 // 组内是否含 debug_* 工具调用
	AnsweredSeq      int64                // 工具组 answered_seq（非工具组为 -1）
	ToolCallID       string               // 首条工具调用 ID（多调用详见 ToolCalls）
	ExecutionID      string               // 首条已准入执行 ID（多调用详见 ToolCalls）
	ToolCalls        []HistoryToolCallRef // 同组多工具调用精确归属列表
	MutationEvidence MutationEvidence     // 持久化派生的三值因果证据
}

// repairContext 由零正文 HistoryGroupMeta（或 SQLite 流式扫描）纯函数确定性派生，运行期与任意次 Resume 100% 恒等。
type repairContext struct {
	Active         bool  // 当前外部用户周期内是否存在活跃 OriginCompletionBlocked
	NewestIsBlock  bool  // 最新有效对话组是否为刚落盘（或刚 Resume）的 OriginCompletionBlocked
	BlockSeq       int64 // 最新 OriginCompletionBlocked 所在行 seq（非活跃时为 -1）
	FloorIdx       int   // 当前用户回合历史元数据中必须保留的最早修复原子组下标（非活跃时为 -1）
	MinRequiredSeq int64 // 严格模式下窗口 FirstSeq 必须满足 built.FirstSeq <= MinRequiredSeq 的原始硬下界（非活跃时为 -1）
}

// internal/agent/presentation.go — 二元投影模式与原因码
type ProjectionMode string

const (
	ModeFull    ProjectionMode = "full"
	ModeCompact ProjectionMode = "compact"
)

const (
	PrefixReasonFitsFull          = "fits_full"
	PrefixReasonCandidateProfile  = "candidate_profile"
	PrefixReasonFixedPrefixBudget = "fixed_prefix_budget"
	PrefixReasonNewestGroupFit    = "newest_group_fit"
)

type canonicalStepInput struct {
	SystemPrompt  string
	Layers        []SystemLayer
	Instructions  string
	State         protocol.Message // Role: "user", Content: stateBlock.Text
	StateRefs     []string         // 来自 stateBlock.ItemRefs，严格对齐实际渲染暴露项
	IncludedTools []StepTool       // 已按策略/白名单/黑名单过滤的当步可用工具
}

type candidateProjection struct {
	Mode                   ProjectionMode
	Reason                 string
	SentLayers             []SystemLayer
	System                 []protocol.Message
	Tools                  []protocol.ToolSpec
	ToolTokens             int
	FullFixed              int
	CompactFixed           int
	Fixed                  int
	GuardTokens            int
	BaseGateSlack          int
	EffectiveGateSlack     int
	UsableFull             int
	UsableCompact          int
	UsableInput            int
	MinRequiredSeq         int64
	OriginalMinRequiredSeq int64
	FloorRelaxed           bool
	FloorRelaxedReason     string
	FloorRelaxedDecision   *Decision
	Budget                 window.Budget
	Window                 window.Result
	Echo                   []protocol.Echo
}
```

### 4.3 Candidate-Specific Projection 七步单向无环计算流水线（闭合 `CSA-02`、`RC3-NEW-07`）

纯函数 `projectCandidate` 对任一候选 `cand`（`m := l.Models[cand.Ref]`）严格执行以下 **7 步单向无环计算序列**（每一步仅引用前序步骤已算出的确定性标量，全流程零套第二容量公式、零重复构建）：

1. **Step 1 — 构造 `Full` 与 `Compact` 两套静态前缀（Static Prefixes，新切片分配，零原地污染）：**
   - 解析候选有效工具描述偏好 `toolPref := effectiveVariant(m.Presentation.ToolDescriptions, opts.Presentation.ToolDescriptions)`；
   - `fullLayers` 取各层原始 `layer.Text`，生成 `fullSystem` 与 `fullTools = presentStepTools(canonical.IncludedTools, toolPref, ModeFull)`；
   - `compactLayers` 对 `layer.CompactText != ""` 的层替换为 `layer.CompactText`，生成 `compactSystem` 与 `compactTools = presentStepTools(canonical.IncludedTools, toolPref, ModeCompact)`。
2. **Step 2 — 分别计算两套 `Fixed`、基础池与当步必需后缀开销：**
   - `fullToolTokens := window.Estimate(fullTools)`，`compactToolTokens := window.Estimate(compactTools)`；
   - `fullFixed := window.Fixed(window.Input{System: fullSystem, State: canonical.State, ToolTokens: fullToolTokens})`；
   - `compactFixed := window.Fixed(window.Input{System: compactSystem, State: canonical.State, ToolTokens: compactToolTokens})`；
   - 计算与模式无关的候选基础量：`outBudget := OutputBudget(opts.MaxTokens, cand)`，`guard := GuardTokens(cand.Context, m.ProviderPkg, len(canonical.IncludedTools))`，`poolBeforeSlack := cand.Context - outBudget - guard`；
   - 切分当步 `transcript` 原子组：计算 `requiredRepairTokens`（当 `repair.Active && repair.MinRequiredSeq >= 0` 时取所有 `Seq >= repair.MinRequiredSeq` 的原子组 `window.GroupTokens(g)` 之和，否则为 `0`）以及 `requiredTranscript`（当 `minRequiredSeq >= 0` 时取所有 `Seq >= minRequiredSeq` 的原子组 `window.GroupTokens(g)` 之和；否则取最新单个原子组 `window.GroupTokens(groups[len(groups)-1])`，空历史为 `0`）。
3. **Step 3 — 分别计算两套 `BaseGateSlack`：**
   - `baseSlackFull := BaseGateSlack(m, opts.Presentation, ModeFull)`；
   - `baseSlackCompact := BaseGateSlack(m, opts.Presentation, ModeCompact)`。
4. **Step 4 — 分别计算两套 `EffectiveGateSlack`**（所需输入 `poolBeforeSlack`、`fullFixed/compactFixed`、`baseSlackFull/baseSlackCompact`、`requiredRepairTokens` 均已在 Step 2–3 就绪）：
   - `effSlackFull := EffectiveGateSlack(poolBeforeSlack, fullFixed, baseSlackFull, requiredRepairTokens, repair)`；
   - `effSlackCompact := EffectiveGateSlack(poolBeforeSlack, compactFixed, baseSlackCompact, requiredRepairTokens, repair)`。
5. **Step 5 — 分别计算两套最终 `Margin` 与 `UsableInput`（统一纳入 `CapacityRetry` 上限，全系统唯一容量公式）：**
   - `marginFull := guard + effSlackFull`，`marginCompact := guard + effSlackCompact`；
   - 若同模型在当步曾触发 `CapacityRetry` 减半退避（`limit, ok := r.transcriptCap[cand.Ref]; ok`）：
     - `marginFull = max(marginFull, cand.Context - outBudget - fullFixed - limit)`
     - `marginCompact = max(marginCompact, cand.Context - outBudget - compactFixed - limit)`
   - `usableFull := cand.Context - outBudget - marginFull`；
   - `usableCompact := cand.Context - outBudget - marginCompact`。
6. **Step 6 — 执行无状态模式选择 `WindowPrefixDecision`：**
   - `mode, reason, prefixDec := WindowPrefixDecision(cand.Ref, toolPref, usableFull, usableCompact, fullFixed, compactFixed, requiredTranscript)`。
   - 五类运行态（`ProfileCompact`、`ProfileDetailed`、默认态 `""`、门禁修复期、`CapacityRetry` 退避期）100% 收敛于同一套单向顺序。
7. **Step 7 — 仅使用选定 `mode` 的唯一一套投影构造 `window.Build`、`Echo` 与后续 `ProviderRequest`：**
   - 绑定选定模式的 `(sentLayers, sentSystem, sentTools, sentToolTokens, candFixed, baseSlack, effSlack, usableInput, selectedMargin)`；
   - 构造 `candBudget := window.Budget{Context: cand.Context, Output: outBudget, Margin: selectedMargin}`；
   - 唯一一次调用 `built, err := window.Build(candIn, candBudget)`，执行统一下界硬守卫：
     ```go
     if minRequiredSeq >= 0 && (built.FirstSeq < 0 || built.FirstSeq > minRequiredSeq) {
         return candidateProjection{}, Decision{}, fmt.Errorf(
             "%w: required context floor seq %d evicted (window firstSeq=%d)",
             window.ErrOverflow, minRequiredSeq, built.FirstSeq,
         )
     }
     ```
   - 调用 `r.requestEcho(built, cand.Ref)` 生成严格对齐 `built.Messages` 下标的 `Echo`，返回 `candidateProjection`。

### 4.4 `ModeFull / ModeCompact` 二元无状态决策规则与候选 Profile 优先级（闭合 `NEW-04`、`CSA-08`、`RC3-NEW-07`）

直接使用 §4.3 Step 2 与 Step 5 已算出的 `fullFixed, compactFixed, requiredTranscript, usableFull, usableCompact`，`WindowPrefixDecision` 按以下严格有序的四条分支无状态判定：

1. **候选显式紧凑偏好（`PrefixReasonCandidateProfile = "candidate_profile"`，最高优先级）：**
   若 `toolPref == config.ProfileCompact`，**无条件立即返回 `ModeCompact`**。
2. **默认态固定前缀超半数预算（`PrefixReasonFixedPrefixBudget = "fixed_prefix_budget"`，仅对 `toolPref == ""` 生效）：**
   若 `toolPref == ""` 且 `fullFixed > usableFull / 2`，返回 `ModeCompact`（若候选显式配置了 `toolPref == config.ProfileDetailed`，则跳过本条 `50%` 启发式，优先兑现详细展示偏好）。
3. **动态单步必需后缀硬容量兜底（`PrefixReasonNewestGroupFit = "newest_group_fit"`，单步生效、绝不跨步锁死）：**
   若 `fullFixed + requiredTranscript > usableFull` 且 `compactFixed + requiredTranscript <= usableCompact`（对 `toolPref == ""` 与 `toolPref == config.ProfileDetailed` 均生效），仅在当前步骤临时返回 `ModeCompact`；下一步恢复常规体积后自动回到 `ModeFull`。
4. **完整呈现（`PrefixReasonFitsFull = "fits_full"`）：**
   其余情况返回 `ModeFull`（若连 `compactFixed + requiredTranscript > usableCompact` 也无法满足，Step 7 的 `window.Build` / `minRequiredSeq` 守卫确定性返回 `window.ErrOverflow` 并触发跨模型 `Failover`）。

**关于 Prefix Cache 的四层严格区分（`CSA-08`）：**
- **Prefix Stability（前缀字节稳定性）：** 在 `Workflow` 白名单未变且未触发单步容量兜底时，同一候选模型的 `[Tools, System]` 前缀字节在整场 Run 中恒定不变。
- **Prefix Identity（前缀标识）：** 由 `RequestEnvelope` 的 `ToolSpecsDigest` 与 `sys:*` 短摘要唯一标识。
- **Provider Cache Eligibility（服务端缓存准入资格）：** OpenAI 与 DeepSeek 等兼容接口支持自动前缀匹配缓存；**但当前 `provider/anthropic/wire.go:167-182` 未发送 `cache_control` 断点标记，在 Anthropic 原生接口上不会自动触发缓存**。
- **Actual Cache Hit（真实缓存命中，标记 `UNVERIFIED`）：** 取决于上游路由、负载驱逐与 TTL，仅属无副作用性能副产物，绝不介入权限、预算或门禁正确性判定。

### 4.5 Token Budget、JSON 序列化膨胀精确上界与算术统一推导（闭合 `RC3-F02`、`RC3-NEW-02`、`RC3-NEW-03`）

#### 1. 七类上下文与预算分量的严格切分及防 `Echo` 重复计费证明
- **输入侧五类实际计费对象（全部统一为 `int` Input Tokens）：** `System`（`in.System`）、`TaskState`（`in.State <= 10 KiB`）、`Tools`（`in.ToolTokens`）、`Transcript`（`in.Transcript`，物理不含 Thinking 原文）与 `Echo`（`window.Entry.EchoTokens`）。
- **防止 `Echo` 重复计入的源码不变量：** `r.appendAssistant`（`transcript.go:65-71`）仅入账一次 `echo.Tokens()`（统一调用 `window.EstimateBytes`）；`window.GroupTokens` 计 `Estimate(e.Msg) + e.EchoTokens`；一旦追加 `Role == "user"` 行，`r.appendRow` 立即清零前序非 `user` 行 `EchoTokens` 并清空 `r.turn`；`r.requestEcho` 仅按下标提取引用而不重复累加 Token。
- **输出侧预留边界：** `OutputBudget(maxTokens, c)` 独占预留模型回复上限（Anthropic `thinking.budget_tokens` 在 `wire.go:283` 钳位在 `max_tokens - 1024` 内，属其子集）。

#### 2. 实际字节流估算 `EstimateBytes` 与指定字节上限最坏预留 `ReserveTokensForBytes`
1. **实际文本/JSON 估算函数 `EstimateBytes(b []byte) int`：**
   空切片返回 `0`；非空切片单遍统计 `A`（ASCII 字母与空格，权重 `4/12`）、`S`（其余单字节 ASCII `< 0x80`，权重 `7/12`）、`U`（多字节 UTF-8 `>= 0x80`，权重 `6/12`）：
   $$\text{EstimateBytes}(b) = \begin{cases} 0, & \text{len}(b) == 0 \\ \left\lfloor \frac{4A + 7S + 6U + 11}{12} \right\rfloor + 4, & \text{len}(b) > 0 \end{cases}$$
2. **指定序列化字节上限最坏预留函数 `ReserveTokensForBytes(n int) int`：**
   $$\text{ReserveTokensForBytes}(n) = \begin{cases} 0, & n \le 0 \\ \left\lfloor \frac{7n + 11}{12} \right\rfloor + 4, & n > 0 \end{cases}$$
   对任意满足 `0 < len(b) <= n` 的字节切片 `b` 恒有 $\text{EstimateBytes}(b) \le \text{ReserveTokensForBytes}(n)$ 且单调不减。闭源上游 BPE 词表在极端稠密数组或生僻字符上的真实分词误差标记为 `UNVERIFIED`，由 `GuardTokens (~6.25%) + CapacityRetry + Failover` 闭环兜底。

#### 3. `protocol.Message` JSON 序列化膨胀与 `presentFeedback` 双重字节守卫（`RC3-NEW-02, 03`）
- `blocked := protocol.Message{Role: "user", Origin: protocol.OriginCompletionBlocked, Content: s}` 空内容 JSON 外壳精确占 **`GateMessageEmptyJSONBytes = 58B`**；Go `encoding/json` 对 `\n\r\t\"\\` 膨胀 `2x`，对 `<>&` 与控制符/非法 UTF-8 膨胀 `6x`。
- `presentFeedback(text, variant)` 执行三步确定性封顶：① `strings.ToValidUTF8` 净化、`\r\n` 归一为 `\n`、剔除除 `\n\t` 外控制字节；② 含 `"…"` 在内的原始 UTF-8 字节上限 `CompactFeedbackBytes = 1200` / `MaxDetailedFeedbackBytes = 6144`；③ 实测序列化消息字节硬上限 **`MaxCompactGateJSONBytes = 58 + 2*1200 = 2,458B`** / **`MaxDetailedGateJSONBytes = 58 + 2*6144 = 12,346B`**（仅高密度 `<>&` 超限时在 UTF-8 Rune 边界二分收紧）。
- **四项核心预算公式精确核算：**
  1. $\text{GuardTokens}(\text{ctx}, \text{pkg}, N_{\text{tools}}) = \max(\lfloor \text{ctx}/16 \rfloor, 512) + \mathbb{I}(\text{pkg} == \text{"anthropic-messages"} \land N_{\text{tools}} > 0) \cdot 512$
  2. $\text{BaseGateSlack}_{\text{compact}} = \text{ReserveTokensForBytes}(2458) + 512 = 1,438 + 512 = \mathbf{1,950}\text{ tokens}$；
     $\text{BaseGateSlack}_{\text{detailed}} = \text{ReserveTokensForBytes}(12346) + 512 = 7,206 + 512 = \mathbf{7,718}\text{ tokens}$。
  3. $\text{EffectiveGateSlack}$：常态非修复期（`!repair.Active`）取 $\min(\text{baseSlack}, \max(0, \lfloor (\text{poolBeforeSlack} - \text{candFixed}) / 4 \rfloor))$（对 `>= 32K` 模型保持完整 `1,950 / 7,718` 预留，对 `8K` 小窗口模型自动按前缀后可用池的 `25%` 比例预留并保留 `75%` 多步工具历史容量，消除 `F-06` 单步耗尽可用池问题）；`repair.Active && repair.NewestIsBlock` 时为 `0`；多步修复期为 $\min(\text{baseSlack}, \max(0, \text{poolBeforeSlack} - \text{candFixed} - \text{requiredRepairTokens}))$。
  4. $\text{UsableInput}(c, m, \text{mode}) = c.\text{Context} - \text{OutputBudget}(\text{maxTokens}, c) - \max(\text{GuardTokens} + \text{EffectiveGateSlack}(\text{mode}), \text{capRetryMargin}(\text{mode}))$。
  5. **`8K` 小窗口模型（`Context >= 8000`）兼容性边界声明（闭合 `F-06`）：** LumenCortex 明确继续支持 `8K` 窗口模型；当候选 `c.Context < verbatimCompactFixed(c) + GuardTokens(c) + OutputBudget(maxTokens, c)`（例如 `8K` 模型在 `Policy: "full"` 挂载全量 22 个内置工具时）时，`CandidatesByStaticPrefix` 与 `projectCandidate` 的 `ModeCompact` 自动启用经 `REV-F06` 验证的安全内置工具紧凑描述，保证 `8K` 模型稳定通过静态预筛并具备多步会话能力，同时 `>= 16K` 模型的预算紧凑仍 100% 保留内置描述原文。

| 序列化字节上限 `n` | 公式展开 `floor((7*n+11)/12) + 4` | `ReserveTokensForBytes(n)` | `+ ProposalReserve (512)` | 说明 |
| :--- | :--- | :---: | :---: | :--- |
| `n <= 0` / `1` / `12` / `13` | `0` / `5` / `11` / `12` | `0` / `5` / `11` / `12` | — | 非正与模 `12` 单调跳变边界 |
| `n = 58`（空门禁外壳） | `floor(417/12) + 4 = 34 + 4` | `38` | — | `{"role":"user","content":"","origin":"completion_blocked"}` |
| *历史勘误对照* `1456` / `6400` | `floor(10203/12)+4` / `floor(44811/12)+4` | `854` / `3,738` | `1,366` / `4,250` | 修正 `v0.4-rc3` `853` 笔误 |
| **紧凑档 `n = 2,458B`** | `floor(17217/12) + 4 = 1434 + 4` | **`1,438`** | **`1,950`** | 覆盖 `1200B` + 全量 `2x` 转义 + `58B` 外壳 |
| **详细档 `n = 12,346B`** | `floor(86433/12) + 4 = 7202 + 4` | **`7,206`** | **`7,718`** | 覆盖 `6144B` + 全量 `2x` 转义 + `58B` 外壳 |

### 4.6 验证历史三层语义、快照与编辑引擎因果归属证明、方案评估及单源 `answered_seq` 修复保护机制（闭合 `REV-F01`、`REV-F04`、`RC33-F01`、`RC34-F01`、`RC34-F02`）

#### 1. 验证状态的真实三层源码语义（`RC3-F03`，零数据库 Migration）
1. **当前 `Loop.Run` 内存修复预算：** `r.attempts`（`loop.go:241`）与 `Hooks.ResetDenials`（`loop.go:195`）每次 `Run/Resume` 重新初始化；仅 `verificationGate` 消耗 `r.attempts`。
2. **最近一次 `verificationGate` 持久快照（`TaskState.Verification`）：** `SetVerification`（`taskstate/verification.go:122-135`）整块覆盖写，仅存最近一次触发 `verificationGate` 的尝试切片。
3. **跨 Run 永久历史证据：** 追加存储在 `session_messages`、`request_envelopes`、`derived_origins` 与 `agent_steps` 中。

#### 2. `WorkspaceSnapshot` 与 `FilesSnapshot` 七维证据边界物理分离及 `RC34-F01` 忽略文件盲区收敛

经逐行核对 `internal/repository/snapshot.go:40-222`、`internal/repository/blobs.go:18-67`、`internal/repository/path.go:17-49`、`internal/repository/service.go:298-312`、`internal/repository/snapshot_test.go:63-91` 与 `internal/agent/execution.go:53-282`，必须将以下 **七个证据维度** 在概念与代码规则上彻底解耦（**严禁将 `WorkspaceSnapshot` 的 `CoverageComplete` 描述为覆盖整个工作区文件系统，也严禁将 `SnapshotFiles` 的前后差异单独等同于当前 Agent 的因果写入**）：

1. **`git status` 覆盖范围（Git Status Coverage Scope）：** `SnapshotWorkspace`（`snapshot.go:120-122`）执行 `git status --porcelain=v2 -z --branch --no-ahead-behind --untracked-files=all`，仅观察 `HEAD`、已跟踪变更与非忽略未跟踪文件，**物理盲视**一切匹配 `.gitignore`、`.git/info/exclude` 或 `core.excludesFile` 的路径。
2. **Git 可见文件状态变化（Stat Token Change）：** `ws:sha256:...` 的 `PreRef == PostRef` 仅代表 `HEAD` 与 Git 可见子集的 `<XY> <Size> <ModTimeNano>` 未变，对修改/新建/删除 Git-ignored 文件（如 `build/generated.go`、`.env`）存在 100% 盲区。
3. **文件内容 Git Blob 变化（File Content Git Blob Change）：** `WorkspaceSnapshot` 无执行前文件 Blob；唯独 `SnapshotFiles`（`blobs.go:59-67`）通过 `git hash-object -- <files...>` 计算目标路径执行前后的内容 Git Blob（不存在记为 `"-"`）。
4. **Git-ignored 文件可见性：** 在 `SnapshotWorkspace`（`tierWorkspace`）中 100% 不可见；在 `SnapshotFiles`（`tierFiles`）中因 `git hash-object` 直接哈希文件流而 **100% 可见**（限工作区内 `<= 4 MiB` 常规文件）。
5. **非忽略未跟踪文件（Untracked Non-Ignored Files）：** 出现在 `WorkspaceSnapshot` 中，但 `touch` 变动无前置 Blob 可证内容真变。
6. **快照完整性（`Complete` / `CoverageComplete`）：** `WorkspaceSnapshot.Complete` 仅代表 `git status` 输出未超过 `MaxStatusBytes (512 KiB)` 被截断；`FilesSnapshot.Complete` + `hasPositiveFilesSnapshotRef(PreRef)` 代表显式传入的 `N >= 1` 个目标路径全部位于工作区内且每个存在路径均为 `<= 4 MiB` 常规文件（符号链接、目录、`> 4 MiB` 文件均置 `Complete = false`）。
7. **单次工具调用因果归因（Causal Authorship）：** **`SnapshotFiles` 采样发生在 `admitExecution` 与 `postExecution` 两个锁外时间点，只能观测 `[t_pre, t_post]` 区间内的状态净差，不能单独证明是谁写入了磁盘！** 针对 `tierWorkspace` 的三方案评估维持 `v0.4-rc3.4` 裁决（采纳保守收敛：**全部内置 `tierWorkspace` 恒判 `MutationUnknown`**，否决无法覆盖 `bash / lsp_rename` 的分级迁移方案与击穿 `512 KiB` 上限的 `--ignored` / 全盘遍历方案）；而针对 `tierFiles` 的因果归属漏洞（`RC34-F02`），必须深入 `internal/editing` 内核补齐因果写入证明。

#### 3. `internal/editing` 引擎十问源码级实证核查与四类证据源能力矩阵（闭合 `RC34-F02`）

对照 `internal/editing/engine.go`、`internal/editing/store.go`、`internal/editing/doc.go`、`internal/toolset/{editing,files,patch,editing_tools}.go` 与 `runtime/events.go` 逐行核查，十项核心事实逐一实证如下：

1. **`Engine.Commit` 返回什么结构？** `editing/engine.go:84-106 & 254-325`：`Commit(ctx, Plan)` 返回 `(Result, error)`，其中 `Result` 含 `Session string` 与 `Files []FileResult`，每项 `FileResult` 仅含 `{Path string, OldRev Rev, NewRev Rev, Created bool, Deleted bool, Changed bool}`。
2. **`prepared` / `FileResult` / `Result` 是否区分实际写入与未写入？** 引擎内部结构 `prepared`（`engine.go:228-245`）持有私有字段 **`wrote bool`**（`243` 行），仅在 `deleteCAS`（`525` 行）或 `writeCAS`（`539` 行）真实修改文件系统后置为 `true`；但导出的 `FileResult`（`333-339` 行）**未导出 `wrote`**，而是按 40-bit 截断修订号计算 `Changed: p.created || p.deleted || RevOf(p.next) != p.rev`（`doc.go:33-36`：`Rev` 仅为 SHA-256 前 40 bits 的 8 位 base32 串，新建空文件 `""` 时 `OldRev == NewRev == RevOf(nil)`，且存在 `2^-40` 碰撞空间，不如 `p.wrote` 全字节比较与全量 SHA-256 严密）。
3. **`p.next == p.original` 时是否跳过磁盘写入？** **100% 确认跳过磁盘写入！** `engine.go:527-529` 明文执行：`if p.existed && !p.created && string(p.next) == string(p.original) { continue }`——此时不打开写描述符、**写入 `0` 字节**、保持 `p.wrote == false`、不注册新快照（`306` 行）、不发送 `edit.committed` 事件（`314` 行），但 `Commit` 仍返回 `err == nil`！
4. **`BaseRev`、`RejectStaleRead`、`staleUnless` 何时拒绝写入？何时放行？** `engine.go:404-431` & `store.go:163-204`：① 显式传入 `BaseRev != ""` 时（`edit_lines` 必传，其余可选），`!p.existed` 报 `CodeNotFound`、`p.rev != BaseRev` 报 `CodeConflict`；② 当 `BaseRev == ""` 且 `RejectStaleRead == true` 时（`write_file / replace_in_file / apply_patch` 默认路径），查询 `latest, ok := e.snaps.Latest(session, p.key)`：若曾读过且 `p.rev != latest.Rev`（或超 `MaxSessionEntries = 512` 淘汰致 `incomplete == true`）报 `CodeStale`；**致命放行缺口在于：当 `!ok && !e.snaps.incomplete(session)`——即当前会话从未读取过该路径，或进程重启 `Resume` 清空了纯内存 `editing.SnapshotStore`（`store.go:25-26`）时，`RejectStaleRead` 直接放行！** 且 `staleUnless` 仅在会话落盘后通知其他会话，宿主外部编辑器改盘完全不经过 `SnapshotStore`。
5. **CAS 写入、写后重读校验与 `O_CREATE | O_EXCL` 创建身份清理守卫（闭合 `F-01`）：** `engine.go` 排序加进程内单路径互斥锁 `e.lockPaths(keys)` -> `prepare` 读取 `p.original` 算 `p.rev` 并生成 `p.next` -> `write` 阶段：删除调 `deleteCAS`（打开 fd 重读校验 `RevOf(current) == p.rev` + `SameFile` 防掉包后 `os.Remove`）；既有文件修改（`p.existed == true`）强制调用 `host.Open(p.abs, false)`（无 `O_CREATE`，期间被外部删除返 `CodeConflict`）；创建型写入（`!p.existed`）在 `LocalHost.Open(path, true)` 与 `(*toolset.Set).Open(path, true)` 中**强制使用 `os.O_RDWR | os.O_CREATE | os.O_EXCL`**：① 若返回 `os.ErrExist`（外部进程在 `prepare` 与 `Open` 之间创建了同名文件），立即无脏返回 `CodeConflict`（`dirty=false`，**严禁删除既有或外部文件**）；② 唯有本次 `O_CREATE | O_EXCL` 真正创建成功的文件才获得清理资格，当场从打开句柄提取平台文件身份 `fileIdentity`（Unix 校验 `Dev/Ino` + `os.SameFile` 且在保持 `f` 打开防止内核重用 inode 的前提下复核 `os.Lstat` 后删除；Windows 通过 `syscall.GetFileInformationByHandle` 记录 `VolumeSerialNumber + FileIndexHigh/Low + CreationTime` 并在清理及 `rollback` 删除前重新打开/校验路径仍指向同一常规文件且 `rollback` 阶段字节内容仍等于 `p.next`；若文件已被外部删除、替换或重建则一律禁止删除！）。
6. **失败与多文件部分写入后的回滚是否是 best-effort？** **是，存在两类残留副作用窗口：** ① 单文件 `writeCAS` 在 `f.Truncate(0)` 清空原文件后若 `f.Write(p.next)` 报 I/O 错误或重读报 `CodeConcurrentWrite`（`590-605` 行），因 `p` 尚未加入 `written`（`540` 行），直接返回错误**不写回 `p.original`**（且 `toolset/editing.go:108` 漏将 `Truncate` 后的 `CodeIO` 置 `MutatesWorkspace: true`）；② 多文件 `apply_patch` 第 `k` 个文件失败时，`e.rollback(host, written)`（`609-632` 行）执行 `_ = os.Remove` 与 `_ = e.writeRestore` **显式忽略全部恢复错误**，回滚失败或中途崩溃会留下部分写入。
   - 〔历史，rc3.8 起被场景 F 取代〕R7 已删除 `rollback` / `writeRestore` 及一切已发布文件的自动补偿。多文件失败按逐文件收据报告已核实前缀、失败文件和未执行后缀；截断后写失败报告 `partially_modified`（PRE-01）。
7. **工具层返回给 Agent Loop 的 `protocol.ToolResult` 是否保留了 `Engine.Commit` 的细节？** **未保留！** `write_file`（`files.go:147`）与 `replace_in_file`（`files.go:188`）仅输出路径与修订号，`apply_patch`（`patch.go:112`）用 `_, err := s.edit.Commit(...)` 直接丢弃 `Result`，且四者在 `p.wrote == false` 时均硬编码 `MutatesWorkspace: true`。
8. **`edit.committed` 事件是否持久化？** **完全不持久化！** `runtime/events.go:255-260` 对 `"edit.committed"` 硬编码 `durableEvent == false`（不写 `journal` 表），载荷（`engine.go:318-321`）无 `ExecutionID / RequestID / ToolCallID`，且订阅缓冲满时非阻塞丢弃（`events.go:195-207`）。
9. **`Resume` 后能否仅凭现有数据库知道某次 `tierFiles` 调用是否真的执行过磁盘写入？** **现有 `c73d173` 数据库不能！** `derived_origins` 仅存 `pre_ref / post_ref / coverage / outcome`，场景 B（宿主改 `B` 后 Agent `write_file(B)` `p.wrote=false`）与场景 A（Agent 真实写 `B` `p.wrote=true`）持久化列逐字节雷同，必须在结算事务中持久化 Commit 级因果封签。
10. **如何把 `Engine.Commit` 的证据与单个 `ExecutionID` 绑定，避免同一步多调用串线？** 在 `loop.go` 与 `callback.go` 每次完成 `admitExecution / admitCallbackExecution` 生成唯一 `x.id` 后，向 `toolCtx` 注入仅关联该 `x.id` 的宿主私有 `commitRecorder`（嵌套回调创建新子 `commitRecorder` 覆盖父级键），由 `toolset.commitPlan` 将 `CommitReceipt` 直接写入该宿主槽——模型参数与工具返回文本均无法触碰或伪造。

**四类证据源（Snapshot / Editing Commit / DerivedEdge / Execution）能力正交对比表：**

| 证明能力维度 | A. Snapshot Evidence<br>(`PreRef`, `PostRef`, `Coverage`) | B. Editing Commit Evidence<br>(`Wrote`, `OldSHA256`, `NewSHA256`, `DirtyFailure`) | C. DerivedEdge Evidence<br>(`TargetRef`, `FileEdgeCount`, `LineageComplete`) | D. Execution Evidence<br>(`ExecutionID`, `answered_seq`, `Outcome`, `OriginToolInterrupted`) | **三重联合封签 (`A + B + D`)** |
| :--- | :---: | :---: | :---: | :---: | :---: |
| **1. `[t_pre, t_post]` 期间文件内容发生了净变化？** | **能**（`tierFiles` `PreRef != PostRef`） | 仅知 Commit 瞬间前后，不知锁外被改与否 | 仅派生自 `postFiles/postWorkspace` 差集，有漏有假 | 不能 | **100% 能** |
| **2. 当前 Agent 调用实际执行了磁盘写入？** | **不能**（场景 B `Wrote=false` 仍有 `PreRef != PostRef`） | **能**（`Wrote == true` 精确区分实写与跳过） | **不能**（场景 B No-op 与场景 E CAS 拒绝均生成 `file` 边！） | 不能（`OK=true` 含 No-op，`OutcomeFailed` 含部分写失败） | **100% 能** |
| **3. Agent 写入改变了执行前内容且留存至快照？** | 不能单独证明作者 | 不能单独排除「宿主改 `B` 后 Agent 写回原 `A`」或「写后被宿主覆写为 `D`」 | 不能 | 不能 | **100% 能**（`Wrote` + 写后 SHA256 夹逼 `== NewSHA256` + `PreBlob != PostBlob`） |
| **4. 足以作为有效修复进展推进 `MinRequiredSeq`？** | **不足够** | **不足够**（缺锁外净变化核对与持久化） | **不足够** | **不足够** | **唯一充要条件** |
| **5. 可跨进程崩溃与 `Resume` 100% 确定性恢复？** | 能（已在 `derived_origins`） | 原始内存结构不落盘，**必须封入持久层** | 能（但非因果证据） | 能（`Reconcile` + `derived_origins` + `session_messages`） | **100% 能**（随 `CommitToolResult` 单事务原子封入 `post_ref`） |

#### 4. 场景 A–H 因果归属推演与方案 A / B / C 独立评估裁决（闭合 `RC34-F02`）

基于 §4.6.3 核实的 `Engine.Commit` 真实行为，逐一推演全部 **8 类核心因果与并发故障场景（场景 A–H）**：
- **场景 A（`初始 A -> Agent Write B (B != A)`）：** `Engine.Commit` 实写磁盘（`Wrote=true, NewSHA256=sha256(B)`），`postFiles` 写后磁盘夹逼核验 `SHA256 == NewSHA256` 且 `PreBlob(A) != PostBlob(B)` $\implies$ 封签 **`;cm=proven`（`MutationProven`）**。
- **场景 B（`初始 A -> 宿主并发改 B -> Agent Write B`，当前会话未读过该路径或刚 `Resume` 清空内存 `SnapshotStore`）：** `RejectStaleRead` 放行，`engine.go:527` 因 `p.next == p.original` **跳过写盘（`Wrote=false`）** 并返回 `OK: true`（`OutcomeComplete`），而 `postFiles` 观测到 `PreRef(A) != PostRef(B)` 且产出 `DerivedEdge`。**为何既不能是 `Proven` 也不能是 `None`：** Agent 实际写入 `0` 字节，判 `Proven` 会将宿主改动冒充 Agent 修复进展并错误推进 `MinRequiredSeq`；但目标文件在步内确实从 `A` 变成了 `B`，若发生在首次门禁前且 `B` 破坏了构建，判 `None` 会漏锁 `preBlockFloor` 挤出关键现场 $\implies$ 唯一正确归类为封签 **`;cm=unknown`（`MutationUnknown`）**。
- **场景 C（`初始 A -> 宿主并发改 B -> Agent Write C`）：** ① **子场景 C1（`C != B` 且 `Blob(C) != Blob(A)` 且写后磁盘夹逼 `SHA256 == NewSHA256(C)`）**：Agent 在持锁 CAS 下真实覆写了磁盘并最终将文件内容定格为异于 `A` 的 `C` $\implies$ **`;cm=proven`（`MutationProven`）**；② **子场景 C2（`C == A`，改回执行前原内容）**：虽 `Wrote=true`，但净 `PreBlob(A) == PostBlob(A)`（相对步前状态零净进展，且发生过真实写盘非纯只读）$\implies$ **`;cm=unknown`（`MutationUnknown`）**；③ **子场景 C3（`Commit` 释放路径锁后、`postFiles` 完成前宿主再次覆写 `C -> D`）**：写后磁盘 `SHA256(D) != NewSHA256(C)` 夹逼失配 $\implies$ **`;cm=unknown`（`MutationUnknown`）**。
- **场景 D（`初始 A -> Agent Write A` 幂等同内容写入）：** `Wrote=false` 且 `PreBlob(A) == PostBlob(A)`（`PreRef == PostRef && FileEdgeCount == 0`）$\implies$ 封签 **`;cm=none`（`MutationNone`）**。
- **场景 E（`Agent Read A -> 宿主并发改 B -> Agent Write A/C`）：** `RejectStaleRead / BaseRev` 在写盘前拒绝（`CodeStale / CodeConflict`，`Wrote=false, Outcome=OutcomeFailed`）；尽管 `postFiles` 观测到 `PreRef(A) != PostRef(B)` 并写入 `DerivedEdge(path@Blob(B))`，因 `Outcome != OutcomeComplete` 且零实写 $\implies$ 封签 **`;cm=unknown`（`MutationUnknown`）**。
- **场景 F（R7 规范性修订）：** 普通文本编辑先完成整批路径、租约、修订和内容校验，再按规范路径逐个执行。遇到首个写入、发布或核验错误即停止。既有文件更新保留描述符语义，新建及显式重建要求私有暂存与单路径 no-replace 发布；无法安全发布就拒绝。已发布的创建、更新、删除均原样保留，不因后续失败自动补偿。收据覆盖已核实、部分修改、外部改动、失败、不支持、不确定及未执行文件。仅核实的字节或不存在状态具有可信的 `NewSHA256`，仅核实的现存内容具有 `NewRev`。失败且存在已知或可能的副作用时，持久化 `outcome_unknown`，可用的文件 `post_ref` 封签 `;cm=unknown`；崩溃按场景 H 恢复。多个文件并非整体原子，普通文件 `PRE-01` 与 `PRE-02` 仍开放。验证只描述观测时刻，未承诺掉电持久性。
  - **场景 F 补充（rc3.8 草案第 2 版，R7R2）：** ① 同一计划内经硬链接或大小写变体（在折叠大小写的卷上）指向同一物理文件的两个目标，在获取锁和写入前整体拒绝（`CodeInvalid`）；大小写比较只在目标目录实际折叠大小写时启用，不做全局小写化。② 准入准备阶段发现的陈旧修订或过期会话读取（`CodeStale` / `CodeConflict`，LC 尚未打开目标写入）记为 `failed_before_write` 并保留原错误码；`external_modification_detected` 只用于 LC 进入发布后观测到的外部改动。③ `PRE-02` 的截断—写入竞争可能是**静默**的：外部进程在 `Truncate(0)` 与 `Write` 之间写入的字节可被覆盖，而结果仍为 `published_verified`。④ 前缀保留后，整批盲目重放非幂等编辑（如锚点插入）会重复已提交的修改；Agent 必须按收据只重做未完成的部分，这是指引而非机制保证。
- **场景 G（同一 `answered_seq` 下多工具调用隔离与聚合）：** 无论并发/串行调用 `write_file(p1), write_file(p2), write_file(p3)` 还是对同一 `p1` 连续调用（如 `A->B` `;cm=proven` 后再调 `B->B` `;cm=none`），每次 `admitExecution` 均向当次 `toolCtx` 注入独占 `commitRecorder(x.id)`（嵌套回调生成独立子槽覆盖父键，零串线）；聚合严格服从偏序格 `Unknown > Proven > None`（`Proven + None = Proven`；只要任一调用为 `Unknown`，整组一票否决为 **`MutationUnknown`**）。
- **场景 H（崩溃与 `Resume` 恢复）：** ① **H1（`Engine.Commit` 写盘后、`CommitToolResult` 提交前崩溃）**：`Store.Reconcile`（`reconcile.go:118-137, 229-239`）补写 `OriginToolInterrupted` 并将 `admitted` 置为 `outcome_unknown` $\implies$ `Resume` 判为 **`MutationUnknown`**；② **H2（`CommitToolResult` 单事务提交后崩溃）**：`;cm=*` 已原子落盘于 `derived_origins.post_ref`，`Resume` 100% 逐位恢复原 `MutationEvidence`；③ **H3（`apply_experience` 嵌套 #1 已 `SettleExecution` `;cm=proven`，嵌套 #2 或外层提交前崩溃）**：外层 `OriginToolInterrupted` 一票否决整组为 **`MutationUnknown`**。

**三套候选架构方案独立对比裁决（优先级：`正确性 > 可恢复性 > 最小复杂度 > 性能`）：**

| 评估维度 | 方案 A：仅扩展 `Engine.Commit` 内存写入回执供当步消费 | 方案 B：仅收紧锁外快照无冲突判定规则（不改 `Commit` 回执） | **方案 A + C1（最终采纳）：逐 `ExecutionID` 绑定 `CommitReceipt` + 写后磁盘 SHA-256 夹逼 + `post_ref` 原子封签 `;cm=*`** | 方案 C2（否决）：新增 SQLite 列或独立文件修改账本 |
| :--- | :--- | :--- | :--- | :--- |
| **1. 因果归属正确性** | 运行期高，但未考虑 `.gitattributes` clean filter 下 raw 字节与 Git Blob 差异 | **不成立**：无法识别场景 B（未读文件 `BaseRev==""` 下 `p.wrote=false` 无报错放行），除非破坏只写工具契约 | **100% 严密**：`Wrote` 全字节判定 + `SnapshotFiles` 前后 `os.Lstat` 与原始文件 `SHA256 == NewSHA256` 夹逼（天然兼容 `.gitattributes`）+ `PreBlob != PostBlob` | 同左，但存在双源账本失步风险 |
| **2. 跨崩溃与 `Resume` 可恢复性** | **不合格**：内存回执重启丢失，`Resume` 退化回场景 B 伪 `Proven` | 虽持久但含场景 B 伪阳性 | **100% 可恢复**：随既有 `CommitToolResult / SettleExecution` 同一 SQLite 事务写入 `derived_origins.post_ref`，旧库无 `;cm=` 行保守归入 `Unknown` | 可恢复，但需破坏冻结 Schema |
| **3. 架构复杂度与写库开销** | 低 | 低 | **极低**：零新表、零 Schema Migration、合法通过 `checkLooseRef (<=91B << 512B)`、兼容 `HasPrefix("files:...")`、严守单次工具精确 **2 次**写库门禁 | **高（否决）**：需 `0009` 迁移与双表同步，违背单一事实源 |

#### 5. 三值 `MutationEvidence` 八问复核、因果封签协议与最终确定性分类规则（闭合 `REV-F04`、`RC33-F01`、`RC34-F01`、`RC34-F02`）

**三值证据边界八问逐条核定：**
1. **什么证据足以证明「由当前 Agent 造成文件内容改变（`MutationProven`）」？** 唯独内置 `tierFiles`（`write_file, replace_in_file, apply_patch, edit_lines`，含顶层与回调嵌套）同时满足 `Outcome == OutcomeComplete && !PreUnavailable && hasPositiveFilesSnapshotRef(PreRef) && Coverage == CoverageComplete`，且持久化 `PostRef` 携带经三重联合核验签发的 **`;cm=proven`**（`PreSnapRef != PostSnapRef`），同时同一 `answered_seq` 组内无任何 `MutationUnknown` 或中断缺口。
2. **什么证据足以证明「工作区文件内容确实未变且零写入（`MutationNone`）」？** 唯独两类封闭场景：① 准入前已拦截未执行调用、编译期静态确认无工作区文件写能力的内置只读/状态/子会话委派工具（含 `loop.go:535` 与 `callback.go:289` 提升为 `tierAdmit` 的 15 个只读内置工具，满足 `FileEdgeCount == 0 && Outcome != OutcomeUnknown`）及纯私有内核 `eval*`（不含 `eval_artifact`）；② 内置 `tierFiles` 同时满足 `Outcome == OutcomeComplete && !PreUnavailable && hasPositiveFilesSnapshotRef(PreRef) && Coverage == CoverageComplete && FileEdgeCount == 0` 且持久化 `PostRef` 携带显式无变更封签 **`;cm=none`**（`Engine.Commit` 全部目标 `Wrote == false` 且 `PreSnapRef == PostSnapRef`）。
3. **哪些情况必须归类为 `MutationUnknown`？** 全部内置 `tierWorkspace`（`bash` 前后台、`edit_go`、`lsp_rename`、`eval_artifact`）、全部已准入外部 `mcp__*`（无论 `read / write / exec`）、内置 `tierFiles` 携带 `;cm=unknown` 或缺失 `;cm=` 封签的历史旧行（含场景 B 宿主改后 Agent No-op、场景 C2 改回原值、场景 C3 写后被宿主覆盖、场景 E CAS 拒绝、场景 F 写盘/回滚失败、`Outcome != OutcomeComplete`、`files:0:` 空目标、路径越界、符号链接、`> 4 MiB` 超限文件）、崩溃中断 `OriginToolInterrupted` 及含任一 `Unknown` 的复合原子组。
4. **哪些工具能够提供「目标文件级 Git Blob + `Engine.Commit` 落盘回执」双重证据？** 唯独内置 `write_file`、`replace_in_file`、`apply_patch`、`edit_lines`（经 `toolset.commitPlan` 回填每 `ExecutionID` 私有 `CommitReceipt` 并与 `SnapshotFiles` 联合核验）。
5. **哪些工具只能提供不完整的 Git-status 证据？** `bash`、`edit_go`、`lsp_rename`、`eval_artifact` 及 `exec` 级 `mcp__*`（通过 `SnapshotWorkspace`）。
6. **`SnapshotFiles` 与写后夹逼核验对忽略文件与 `.gitattributes` 如何表现？** `SnapshotFiles` 调用 `git hash-object -- <files...>` 完全无视 `.gitignore`，精准覆盖工作区内 `<= 4 MiB` 常规文件；同时 `CommitReceipt` 记录原始磁盘字节 `NewSHA256`，`postFiles` 在调用 `SnapshotFiles` **前后两次**对 `Wrote && !Deleted` 目标执行 `os.Lstat`（校验常规文件且两次 `size, mtime` 未跳变）并计算原始文件 SHA-256 核对 `== f.NewSHA256`——既无需在 `internal/editing` 内派生 `git` 子进程，又在开启 `.gitattributes` clean filter 时 100% 证明 `PostBlob` 采样的正是 `Engine.Commit` 写入的那份 `p.next`！
7. **并发外部修改时如何避免错误归因？** `tierWorkspace` 恒为 `Unknown`；`tierFiles` 在外部预先改为同内容时因 `Wrote == false && PreBlob != PostBlob` 判为 `;cm=unknown`（绝不误判 `Proven`），在外部预先改写触发 `CodeStale / CodeConflict` 时判为 `;cm=unknown`，在 `Commit` 解锁后被外部迅速覆写时因写后原始 `SHA256 != NewSHA256` 夹逼失配判为 `;cm=unknown`。
8. **`Resume` 时如何从持久化数据确定性重算？** `postFiles` 将三重核验结论编码为 `PostRef = postSnap.Ref() + ";cm=" + seal`（最大长度 `len("files:10000:sha256:") + 64 + len(";cm=unknown") = 94B <= 512B`，100% 符合 `executions.go:210` `checkLooseRef` 与 `schema_v6` `CHECK` 约束），随既有 `CommitToolResult / SettleExecution` 单事务落盘；运行期步末与 `Resume` 启动 `r.load` 共享唯一只读查询 `Store.ListAnsweredExecutions` 与无状态纯函数 `classifyExecutionSummary`。

**`postFiles` 因果封签签发协议与单条持久化执行记录确定性分类纯函数 `classifyExecutionSummary(rec)`（零动态参数，三分支互斥完备）：**
- **`postFiles` 三重联合核验与封签写入规则（`internal/agent/execution.go`）：**
  1) 设 `receipt := recorder.Receipt()`（由宿主私有 `commitRecorder(x.id)` 捕获）。若 `receipt.DirtyFailure == true`，将 `post.Outcome` 强制升格为 `session.OutcomeUnknown` 且封签置为 `";cm=unknown"`；
  2) 当 `post.Outcome == session.OutcomeComplete && !x.preUnavailable && hasPositiveFilesSnapshotRef(x.preRef) && post.Coverage == session.CoverageComplete && receipt.Committed && len(receipt.Files) == len(x.targets)` 时，**严禁按切片下标位置 `i` 盲目配对（闭合 `F-09`）**——统一以 `filepath.ToSlash(path)` 规范化斜杠路径键（严格区分大小写，绝不执行 `strings.ToLower` 大小写折叠；若规范化键出现重复则立即保守判为 `";cm=unknown"`）建立 `preByPath` 与 `postByPath` 映射，逐键精确比对 `(preFile, postFile, receiptFile)`：
     - **全零写未变判定：** 若全部目标均满足 `!rf.Wrote && preFile.Blob == postFile.Blob`（故 `x.preRef == postSnap.Ref() && len(changed) == 0`），签发 **`;cm=none`**；
     - **实写净变因果判定：** 若至少存在一个 `rf.Wrote == true`，且① 每一个 `!rf.Wrote` 目标满足 `preFile.Blob == postFile.Blob`，② 每一个 `rf.Wrote` 目标满足 `preFile.Blob != postFile.Blob`，③ 对每个 `rf.Wrote && !rf.Deleted` 目标在 `SnapshotFiles` 调用前后两次 `os.Lstat` 元数据（常规文件、`Size`、`ModTimeNano`）恒等且写后原始文件字节 `SHA256 == rf.NewSHA256`（对 `rf.Wrote && rf.Deleted` 目标满足文件不存在且 `postFile.Blob == "-"`），同时整体 `x.preRef != postSnap.Ref()`，签发 **`;cm=proven`**；
     - 其余任何失配（如场景 B `!rf.Wrote && preFile.Blob != postFile.Blob`、场景 C2 `rf.Wrote && preFile.Blob == postFile.Blob`、场景 C3 写后磁盘 `SHA256 != rf.NewSHA256`、路径键缺失或重复），一律签发 **`;cm=unknown`**；
  3) 当 `post.Outcome != session.OutcomeComplete` 或快照不完整/零目标时，只要 `postSnap.Ref() != ""` 一律追加 **`;cm=unknown`**，拼接为 `post.PostRef = postSnap.Ref() + ";cm=" + seal`（`<= 94B <= 512B`）。
- **解析辅助函数：** `splitFilesSnapshotPostRef(postRef)` 将 `postRef` 按最后一个 `";cm="` 切分为 `(snapRef, seal)`（仅当 `seal` 属于 `{"proven", "none", "unknown"}` 且 `hasPositiveFilesSnapshotRef(snapRef)` 时认可有效封签；旧数据库无 `";cm="` 后缀的历史行返回 `seal = ""`，按保守原则归入 `MutationUnknown`）。
1. **分支 1 — 内置 `tierFiles` 工具（`rec.ToolName` 属于 `{"write_file", "replace_in_file", "apply_patch", "edit_lines"}`，含顶层与 `eval / apply_experience` 嵌套回调）：**
   - 令 `snapRef, seal := splitFilesSnapshotPostRef(rec.PostRef)`；
   - 若 `rec.Outcome == session.OutcomeComplete && !rec.PreUnavailable && hasPositiveFilesSnapshotRef(rec.PreRef) && hasPositiveFilesSnapshotRef(snapRef) && rec.Coverage == session.CoverageComplete`：
     - 若 `seal == "proven" && rec.PreRef != snapRef` => 返回 **`MutationProven`**；
     - 若 `seal == "none" && rec.PreRef == snapRef && rec.FileEdgeCount == 0` => 返回 **`MutationNone`**；
   - 其余情况（含 `seal == "unknown"`、缺失 `;cm=` 的历史旧记录、`rec.Outcome != session.OutcomeComplete` 一切失败/中断、`PreUnavailable`、`Coverage != CoverageComplete`、`files:0:` 空目标、路径越界、符号链接、`> 4 MiB` 超限文件）=> 返回 **`MutationUnknown`**。
2. **分支 2 — 编译期固定内置只读与子会话委派 `tierAdmit` 工具（`rec.ToolName` 属于编译期常量白名单 `immutableNonMutatingAdmittedTools`：`start_task`、`spawn_subagent` 及 `callbackAllowedBuiltins` 中的 15 个只读内置工具，同时覆盖 `loop.go:535` 顶层 `RecordExperienceActions` 与 `callback.go:289` 嵌套回调）：**
   - 若 `rec.FileEdgeCount == 0 && rec.Outcome != session.OutcomeUnknown` => 返回 **`MutationNone`**；
   - 其余情况 => 返回 **`MutationUnknown`**。
3. **分支 3 — 内置 `tierWorkspace` 工具（`bash, edit_go, lsp_rename, eval_artifact` 及全部 `debug_*` 调试器工具，`strings.HasPrefix(name, "debug_")`，闭合 `F-04`）、外部 `mcp__*` 工具（`strings.HasPrefix(rec.ToolName, "mcp__")`）及其他一切未证明完备覆盖的工具：**
   - `executionTierOf` 将全部 `debug_*` 工具（`debug_start, debug_attach, debug_launch_command, debug_breakpoint, debug_continue, debug_pause, debug_step, debug_stack, debug_variables, debug_evaluate, debug_stop` 等）统一归入 `tierWorkspace`（同步加入 `callbackDeniedPrefixes`），并在历史组聚合层对含任一 `debug_*` 调用的原子组施加防御性 `MutationUnknown` 兜底；
   - 无论 `PreRef` 与 `PostRef` 是否相等、无论 `Coverage` 与 `Outcome` 取何值，在运行期与 `Resume` **一律无条件返回 `MutationUnknown`**。

**原子组（`answered_seq`）保守感染聚合纯函数 `aggregateGroupMutation(assistantMsg, toolRows, execs)`：**
1. 若 `len(toolRows) < len(assistantMsg.ToolCalls)` 存在未配对工具结果缺口，或任一 `toolRows` 的 `Origin == protocol.OriginToolInterrupted`，立即返回 **`MutationUnknown`**；
2. 遍历关联 `execs` 计算 `classifyExecutionSummary(rec)`（按严格偏序格 `Unknown > Proven > None` 聚合）：
   - 若存在任一条记录为 `MutationUnknown`，整组返回 **`MutationUnknown`**（组内 `Unknown` 一票否决，严禁部分成功掩盖同组未知副作用）；
   - 否则若存在至少一条记录为 `MutationProven`，整组返回 **`MutationProven`**；
   - 否则（零 `execution` 行且无中断，如准入前拒绝未执行调用、纯只读 `tierNone` 或纯私有内核 `eval`；或全部 `execution` 行均为 `MutationNone`）整组返回 **`MutationNone`**。

**全工具因果归属与边界场景 18 类真值总表（运行期与任意次 `Resume` 100% 逐位一致）：**

| 场景编号与操作 | 持久化特征（`derived_origins` / `session_messages`） | 确定性分类 | 对 `MinRequiredSeq` 的影响 |
| :--- | :--- | :---: | :--- |
| **1（场景 A / C1）.** `tierFiles` 实际落盘改写/新建/删除文件（含已跟踪、未跟踪、**Git-ignored 常规文件**及宿主先改 `B` 后 Agent 覆写为 `C != A` 且写后夹逼通过） | `Outcome=completed`, `Coverage=complete`, `files:N>=1`, `PreRef != snapRef`, `post_ref` 封签 **`;cm=proven`** | **`MutationProven`** | 阻断前锁定修复依据；阻断后**唯一合法推进**下界至 `b_last` |
| **2（场景 D）.** `tierFiles` 幂等写入与磁盘原文件逐字节相同的内容（`Wrote=false`） | `Outcome=completed`, `Coverage=complete`, `files:N>=1`, `PreRef == snapRef`, `FileEdgeCount=0`, **`;cm=none`** | **`MutationNone`** | 绝不推进 |
| **3（场景 B 致命反例）.** 宿主并发先改 `A -> B`，Agent 未读过或 `Resume` 后调用 `write_file(B)`（`Wrote=false` No-op 返回 `OK: true`） | `Outcome=completed`, `Coverage=complete`, `PreRef(A) != snapRef(B)`, `FileEdgeCount=1`, **`;cm=unknown`** | **`MutationUnknown`** | **根除伪 `Proven`**：阻断后**绝不推进**下界；首次阻断前锁定现场防漏判 `None` |
| **4（场景 C2 / C3）.** 宿主先改 `A -> B` 后 Agent 写回 `A`（净 `PreRef == snapRef`），或 `Commit` 后被宿主迅速覆写为 `D`（写后 `SHA256 != NewSHA256`） | `Outcome=completed`, **`;cm=unknown`**（或旧库无 `;cm=` 后缀历史行） | **`MutationUnknown`** | **绝不推进** |
| **5（场景 E）.** `Agent Read A -> 宿主改 B -> Agent Write A/C` 触发 `CodeStale / CodeConflict` 写前拒绝 | `Outcome=failed`, `PreRef(A) != snapRef(B)`, `FileEdgeCount=1`, **`;cm=unknown`** | **`MutationUnknown`** | **绝不推进**（即使存在 `file` 边亦不误判） |
| **6（场景 F）.** `tierFiles` 写前失败（`old_text not found`、`files:0:`、陈旧修订或过期读取在准备阶段被拒）或写盘中途失败（`Truncate` 后 I/O 错、`CodeConcurrentWrite`、多文件部分提交即已发布前缀加失败/不确定后缀、超时；历史记录中的 `rollback` 成功/失败） | 写前失败 `Outcome=failed`；进入写盘窗口失败 `DirtyFailure=true -> Outcome=outcome_unknown`，均带 **`;cm=unknown`** | **`MutationUnknown`** | **绝不推进**（绝不假设失败等于零副作用） |
| **7–10（RC34-F01 反例 A–E）.** `tierWorkspace` 修改 / 新建 / 删除 / 批量覆盖 Git-ignored 文件（如 `build/generated.go`、`.env`、`build/*`） | `Coverage=complete`, `PreRef == PostRef == "ws:..."`, `FileEdgeCount=0` | **`MutationUnknown`** | **根除伪 `None`**：首次阻断前锁定 `preBlockFloor`；阻断后绝不推进 |
| **11–13（RC34-F01 反例 F）.** `bash("touch a.go")` 改 `mtime`、`bash("git add a.go")` 改暂存区、或 `git commit / checkout / reset` 改 `HEAD` | `ws:...`（`Coverage=complete` 或 `partial`，`FileEdgeCount >= 0`） | **`MutationUnknown`** | **绝不推进**（零 `mtime` / 索引 / `HEAD` 伪阳性） |
| **14.** `edit_go / lsp_rename / eval_artifact`（含 `preview=true`、`import/export/copy`） | `tierWorkspace`（`ws:...`） | **`MutationUnknown`** | **绝不判 `Proven` 或 `None`、绝不推进** |
| **15.** 外部 `mcp__*` 工具（无论 `Permission="read" / "write" / "exec"`） | `tool_name LIKE 'mcp__%'` | **`MutationUnknown`** | **绝不推进**；权限覆盖或卸载后 `Resume` 100% 不变 |
| **16.** 路径越界调用（`write_file("../escape.txt", "x")`）与快照截断/超限（`>512KiB` 状态、`>4MiB` 文件、目录、符号链接） | `PreUnavailable=true` 或 `Coverage=partial/unavailable`, `Outcome=failed/completed` | **`MutationUnknown`** | **绝不推进** |
| **17（场景 G）.** 同一 `answered_seq` 多工具调用（`[write_file(p1), write_file(p2), write_file(p3)]` 或含任一 `Unknown` 调用） | 逐 `ExecutionID` 独立封签；`Proven + None = Proven`，含任一 `Unknown` 则整组否决 | **`Proven / Unknown`** | `Unknown` 一票否决，严禁部分成功掩盖未知副作用 |
| **18（场景 H & 对照）.** 写盘后 `CommitToolResult` 前崩溃（H1）、提交后崩溃（H2）、嵌套回调崩溃（H3）及只读/`eval` 对照 | H1/H3 经 `Reconcile` 写 `OriginToolInterrupted` + `outcome_unknown`；H2 保留 `;cm=*`；纯只读/`eval` 为 `None` | **`Unknown / 原值 / None`** | H1/H3 恒为 `Unknown`；H2 跨 `Resume` 100% 恒等 |

#### 6. 零正文历史组元数据 `HistoryGroupMeta`、带 Failover 优先守卫的受控 Floor Relaxation（方案 B）与十项核心不变量证明（闭合 `F-02`、`F-03`）

1. **有界正文窗口与零正文 `HistoryGroupMeta` 确定性修复推导（闭合 `F-02`）：**
   - **双层内存分离（根除 `maxContext` 裁剪下界漂移与长程内存无界）：**
     1) **有界正文缓冲 `r.transcript`：** `r.groups(maxContext)` 与 `r.load` 仅保留累计 Token 不超过 `l.maxContext()` 的尾部消息正文后缀（移除旧版超 `maxContext` 正文强留钳位，因为没有任何候选模型 `Context <= maxContext` 能装下超出 `maxContext` 的正文前缀）；
     2) **零正文轻量元数据 `r.historyGroups`（`[]HistoryGroupMeta`）：** 从 `max(WindowStartSeq, latestExternalUserSeq)` 起按原子组维护 `Seq, LastSeq, Role, Origin, HasToolCalls, ExpectedCalls, PairedResults, HasInterrupted, HasDebugToolCall, AnsweredSeq, ToolCallID, ExecutionID, ToolCalls, MutationEvidence`（零 `Content` 正文字节！）。每当追加新的外部用户输入行（`Role == "user" && (Origin == "" || Origin == protocol.OriginImported)`）时，立即推进 `r.latestUserSeq = seq` 并清空旧周期 `r.historyGroups`，干净重置旧修复状态；
     3) **有界增量折叠基态 `baseSM`（闭合 `R2-04`，根除 $O(N^2)$ SQLite 重扫）：** `r.historyGroups` 严格封顶 `MaxInMemoryHistoryGroups = 2048`（约 `< 500 KiB`）；由于 `repairStateMachine.step` 满足严格前向结合律，当已结算 `r.historyGroups` 达到 `2048` 组时，将最老 `1024` 个已结算组前向折叠进 `r.baseSM` 并原地前移剩余切片（`r.load` 在单遍 `Transcript + ListAnsweredExecutions` 加载时执行完全相同的折叠），使后续每步任意次候选尝试执行 `deriveRepairContext` 仅需克隆 `r.baseSM` 并扫描内存中 `<= 2048` 个轻量结构体（**`0` 次步内 SQLite 查询、$O(1)$ 时间与有界内存**）。
   - **单一前向单调自动机 `DeriveRepairContextFromHistory` 与完整可恢复 `RepairContext`（运行期与冷 `Resume` 全结构恒等）：**
     - 前向扫描当前外部用户周期内的原子组元数据，除输出 `MinRequiredSeq` 与 `BlockSeq` 外，同步确定性维护完整修复状态：`ProposalSeq`、`BlockCount`、活跃门禁原文 `GateFeedback`（仅保留最新一条活跃 `OriginCompletionBlocked` 文本，非阻断组零正文驻留）、修复阶段 `Stage`（`blocked / inspecting / proven_progress / unknown_mutated`）、原始下界 `OriginalMinRequiredSeq` 与 `OriginalEvidence`、当前阻断后首个未替代因果组 `PostBlockCausalFloorSeq`，以及阻断后 `Proven/Unknown` 计数与最近有界因果执行引用切片 `CausalProgress`（上限 `8` 条）。`Unknown` 一票否决语义完全保留。
2. **带 Failover 优先守卫与固定 Gate State 的三级受控 Floor Relaxation（方案 B，闭合 `F-03` + `R2-02`）：**
   - **严格决策顺序（严禁小模型越级放宽、严禁静默滑窗、严禁 `State` 与 `Messages` 双重保存门禁反馈）：**
     1) **Step 1–4（严格预检与 Failover 优先守卫）：** 先在当前候选校验严格 `built.FirstSeq <= minRequiredSeq`；溢出时必须优先尝试全部更大窗口候选（含 `down.ready` 恢复候选），唯全部更大候选均耗尽/不可用时由 `SelectMaxAvailableCandidate` 选定最大可用候选（必要时记录 `RuleFailoverFloorFallback`）；
     2) **Step 5a — Stage R1（门禁前历史省略、`[BlockSeq .. LastSeq]` 原文完整保留）：** 若 `r.transcript` 仍包含 `BlockSeq`，先以 `floorR1 = minPositiveSeq(repair.BlockSeq, newestGroupFirstSeq, turnProgressSeq)` 试算，仅在 `State` 追加 `[Context Window Notice — Floor Relaxation]`（**不复制 `GateFeedback`**，因 `OriginCompletionBlocked` 原文已在 `Messages` 内）；
     3) **Step 5b — Stage R2（固定 Gate State 注入 `State` + 阻断后未替代因果锚点 `PostBlockCausalFloorSeq`）：** 当 `[BlockSeq .. LastSeq]` 超出最大可用候选（或 `BlockSeq` 已早于有界内存正文起点）时，将受 `presentFeedback`（按候选 `ModeFull/ModeCompact` 对应 `ProfileDetailed/ProfileCompact` 双守卫封顶）格式化的 **Pinned Gate State + 因果修复进展台账 + 省略范围与重检提示通知** 注入当次候选投影的 `State`，并严格仅在 `Seq > repair.BlockSeq` 的消息子集 `transcriptAfterBlock` 上投影（**数学保证 `OriginCompletionBlocked` 绝不在 `State` 与 `Messages` 重复出现**），优先以 `PostBlockCausalFloorSeq` 为下界裁除阻断后旧只读/普通对话组而完整保留阻断后 `Proven/Unknown` 组；
     4) **Step 5c — Stage R3（固定 Gate State + 因果进展台账保护下的滚动修复后缀）：** 当阻断后因果组本身累计亦超出最大可用候选（如门禁后连续数十步 `bash` 输出）时，在 `State` 完整保留固定 Gate State 与因果进展台账的前提下，将 `transcriptAfterBlock` 的硬下界收紧至 `minPositiveSeq(newestGroupFirstSeq, turnProgressSeq)`，贪心保留可容纳的最大近期后缀并持久化 `GatePinnedInState = true` 的可重放决策 `RuleWindowFloorRelaxed`（接入 `ReplayDecision` 与 `VerifyStep`）；
     5) **Step 6（终极受控失败边界）：** 若连固定 Gate State + 最新原子组也无法容纳（或 `NewestIsBlock` 时门禁反馈本身超窗），返回 `window.ErrOverflow` 并受控终止为 `StatusFailed / KindWindowOverflow`。
   - **提示通知与实际守卫的诚实边界（`R2-03`）：** `State` 中的工作区重检提示属于面向模型的显式上下文告知（非运行时工具阻断器）；内置行级编辑/替换工具继续由 `editing.Engine` 的 CAS / `base_rev` 阻止陈旧覆写（`bash` 与外部 `mcp__*` 不受编辑 CAS 约束），最终任务完成继续由完成门禁重验把关。
3. **十项核心正确性不变量逐条形式化核验（`v0.4-rc3.7` 同步修订不变量 3/5/9/10）：**
   - **不变量 1（`MutationUnknown` 绝不推进 `MinRequiredSeq`）：** `postBlockProven` 仅在 `ev == MutationProven` 时置真；任意次 `MutationUnknown`（含场景 B、`debug_*`、`tierWorkspace`、`mcp__*`）均保持 `postBlockProven == false`。
   - **不变量 2（`MutationNone` 绝不推进 `MinRequiredSeq` 且零忽略文件/并发改动/`debug_*` 伪阴性）：** `MutationNone` 不置真 `postBlockProven`，且 `tierWorkspace`、`debug_*` 与场景 B 均排除在 `MutationNone` 之外。
   - **不变量 3（`MutationProven` 必具当前 Agent 因果写入 + 写后磁盘一致 + 净 Git Blob 变化三重证据，且创建型写入绝不误删外部进程数据）：** 创建型写入主路径经同目录私有临时文件 + 平台原子 no-replace 发布，任何无法证明仍完全属于本次调用的目标文件严禁删除（`R2-01`）；文件级封签统一按 `filepath.ToSlash` 键精确配对（`F-09`）。
   - **不变量 4（同 `answered_seq` 组内 `Unknown` 一票否决）：** `aggregateHistoryGroupMutation` 对缺口、`OriginToolInterrupted`、`HasDebugToolCall` 或任一 `MutationUnknown` 执行整组否决。
   - **不变量 5（运行期与冷启动 `Resume` 的完整 `RepairContext` 100% 逐字段恒等、内存有界且步内推导 $O(1)$）：** 两者共享结合律 `baseSM` + 有界 `HistoryGroupMeta` 状态机与 `Store.ListAnsweredExecutions`，连续运行、进程退出与冷启动 `Resume` 产出 `DeepEqual` 的 Gate State、Repair State、滚动起点与 `RuleWindowFloorRelaxed` 决策。
   - **不变量 6（新外部用户输入正确重置旧修复上下文）：** 新 `Origin == ""` 或 `OriginImported` 用户行重置 `baseSM` 与 `r.historyGroups` 并将 `MinRequiredSeq` 归 `-1`。
   - **不变量 7（多次连续门禁打回保留最早未解决原始下界并推进最新门禁锚点）：** 两次阻断间无 `MutationProven` 时 `OriginalMinRequiredSeq` 保持首次 `preBlockFloor`，即便前移亦绝不越过阻断间最早的 `MutationUnknown` 组。
   - **不变量 8（小窗口模型装不下严格保留区时必须优先 `Failover`）：** Pass 1 校验 `built.FirstSeq > minRequiredSeq` 返回 `window.ErrOverflow`（零写库），只要存在可用更大窗口候选即切往大模型，绝不提前触发 Floor Relaxation。
   - **不变量 9（更大候选全部耗尽时的三级受控 Floor Relaxation、固定 Gate State 零重复与终极溢出边界）：** 仅当全部更大候选均耗尽/不可用时，最大可用候选按 `R1 ([BlockSeq..LastSeq]) -> R2 (PinnedGateState + PostBlockCausalFloorSeq) -> R3 (PinnedGateState + RollingSuffix)` 单调松弛；`OriginCompletionBlocked` 在 `Messages` 与 `State` 中严格互斥零重复；唯固定 Gate State + 最新原子组亦无法容纳时受控报 `StatusFailed / KindWindowOverflow`。
   - **不变量 10（严禁静默滑窗省略必需修复历史）：** 任何省略严格下界历史的行为均必须同时满足「全更大候选已耗尽 + 保留持久化 Gate State 与因果进展台账 + 记录 `RuleWindowFloorRelaxed`（含 `GatePinnedInState`）可重放决策 + 向 Agent 显式告知省略范围」。

### 4.7 `BeforeModelCall` 暂停恢复后的 `Echo` 与全字段同步重建（闭合 `NEW-01`）

1. 在进入 `BeforeModelCall`（`send.go:294`）前，仅调用 `projectCandidate` 完成容量、最小修复下界与 `TurnBoundary` 溢出预检，**不提前构造 `protocol.ProviderRequest`**。
2. 从 `BeforeModelCall` 放行后，执行 `rebuilt, err := l.applyPendingInputs(ctx, sessionID, goal, opts, r, nil)`：一旦 `rebuilt == true`（或后续 `4d` 提交了 `OriginTurn`），重新计算 `repair = deriveRepairContext(r.transcript, r.mutations)`、调用 `taskstate.RenderBlock` 刷新 `canonical.{State, StateRefs}` 与 `groups`，并**完整重跑 7 步 `projectCandidate`** 同步刷新全部投影字段与 `Echo`。
3. 仅在最终 `proj` 确定无误后唯一一次组装 `protocol.ProviderRequest` 与 `RequestEnvelope`。

### 4.8 单一动态容量判断权威、`TurnBoundary` 试算与提交顺序（闭合 `NEW-02`、`RC2-F01`、`RC3-F05`）

1. **单一容量权威分工：** `CandidatesByStaticPrefix`（`policy.go`）仅检验 `ToolCalling == true`、健康度排序以及候选最小静态前缀资格 `c.Context >= compactFixed(c) + GuardTokens(c) + OutputBudget(maxTokens, c)`；一切动态窗口、`TurnBoundary`、`repair.MinRequiredSeq` 与 `transcriptCap` 均唯一收口于 `projectCandidate`。
2. **预检纯内存精确试算与克隆 `EchoTokens` 清零（零写库）：** 候选循环开头若 `TurnBoundary(r.turnRows(), cand.Ref)` 为真，克隆 `r.transcript` 并将克隆尾段非 `"user"` 行的 `EchoTokens` 置 `0`，追加合成 `OriginTurn`，令 `minRequiredSeq = minPositiveSeq(turnProgressSeq, repair.MinRequiredSeq)` 送入 `projectCandidate`。若溢出，零写库置 `exhausted[i] = true` 并记录 `FailoverDecision(ClassCapacity)`。
3. **唤醒后复核与原子落盘重确认：** 通过 `Cooldown`、`BeforeModelCall` 与 `applyPendingInputs` 后复核 `due`；若仍需插入 `OriginTurn`，先 `Store.AppendMessage` 落盘获得真实 `seq`，再重跑 `projectCandidate` 使 `RequestEnvelope.LastSeq` 精确绑定持久化 `seq`。

### 4.9 Instructions、Run 级不可变 `SkillSnapshot`、MCP 保围栏压缩与内置紧凑防呆契约（闭合 `REV-F02`、`RC33-F02`、`REV-F06`、`RC3-NEW-04..05`）

#### 1. Instructions 权威、围栏感知大纲与 `anchors=true` 分页契约（`RC3-F06`）
- 权威顺序固定：`Builtin` > `Global (<globalDir>/AGENTS.md)` > `Project (受信 <workspace>/AGENTS.md 或 CLAUDE.md)` > `Skills` > `AgentInstructions`；`Global` 指令 `CompactText` 恒为空（零二次裁剪，不放宽 `toolset/path.go:43-65` 沙箱）。
- **受信 Project 指令（`Bytes > 12 KiB`）：** 复用与 `editing.DecodeDoc`（`toolset/files.go:73`）严格同构的 1-based UTF-8 行切分与 CommonMark 围栏状态机提取围栏外真实标题区间（无标题按每 `200` 行分段），拼接「首 `6 KiB` + 行区间表 + 尾 `2 KiB` + 带 `anchors=true` 的 `read_file(path="...", start_line=..., end_line=..., anchors=true)` 指引」，磁盘原文件零改动。

#### 2. Run 级不可变技能快照（`SkillSnapshot`）生命周期、双模 `read_skill` 接线与权限隔离契约（闭合 `REV-F02`、`RC33-F02`、`RC3-NEW-04`）

经核查 `runtime/agent.go:420-458`、`runtime/prompt.go:171-237`、`runtime/skills.go:18-37` 与 `internal/skills/registry.go:167-473`，现将 `Skills Registry` 的生命周期与单次 `Run` 的不可变快照边界明确定义如下（逐条回答全部八项快照一致性问题）：

1. **快照建立时机与唯一数据源（回答问题 1–3）：**
   - 在每次 `Engine.prepareRun`（`runtime/agent.go:420`，持有会话 Run 锁且位于 `BeginRun` 之前）中，`assemblePrompt` 唯一调用一次 `e.skillRegistry.Prompt(skills.PromptOptions{IncludeProject: plan.trusted})`；
   - 在该次调用内部（由 `Registry.mu.RLock()` 读锁保护），`rawEffective(opts.IncludeProject)` 一次性扫描全局与（仅当 `plan.trusted == true` 时的）项目根目录及 `skills-state.json`，过滤出满足 `Enabled && Error == "" && ShadowedBy == ""` 的有效技能，按 `Skill.ID` 严格字典升序排序生成 **`PromptResult.Eligible []SnapshottedSkill`**（其中 `SnapshottedSkill` 直接保留 `scanRoot` 在 `registry.go:464` 已经读入内存的 `SKILL.md` 正文 `Content string`，零额外磁盘读取！），同时在同一次扫描结果上生成 `PromptResult.Text`（`<= 32 KiB`）与 `PromptResult.CompactText`（`<= 8 KiB`），并对单根触及 `MaxSkills = 128` 截断输出显式 `Warnings`；
   - 紧接着 `prepareRun`（`agent.go:458`）直接将该次返回的 `prompt.Skills.Eligible` 传入 `newSkillReader(prompt.Skills.Eligible)` 注入 `toolset.New`——**整场 `Run` 从不二次查询 `e.skillRegistry`**！
2. **`SkillCataloger.ListSkills` 与 `SkillReader.ReadSkill` 的快照共享及运行期变更隔离（回答问题 4–7）：**
   - `runtime/skills.go` 定义不可变读取器结构（构造后所有字段只读，不持有 `*skills.Registry` 指针）：
     ```go
     type skillReader struct {
         ordered []skills.SnapshottedSkill // 按 Skill.ID 严格升序排列的拷贝切片
         byID    map[string]int           // ID -> ordered 下标映射
     }
     ```
   - **`ListSkills(ctx, query, offset)` 数据源：** 100% 读取内存不可变切片 `r.ordered`。Run 运行中途外部宿主调用 `Registry.Save`（新增或修改技能）、`Registry.Delete`（删除技能）或 `Registry.SetEnabled`（切换启用状态）时，当前正在执行的 `Run` 内的 `SystemPrompt`、连续 10 次乃至任意次 `ListSkills` 分页目录看到的始终是 Run 启动当刻锁定的同一份 `Eligible` 快照，绝不出现「Prompt 列出了 `skill-A` 但分页目录中消失」或「未授权新技能中途混入目录」的撕裂；
   - **`ReadSkill(ctx, id, path)` 数据源与磁盘隔离：**
     1) 首先在 `r.byID` 中查找 `strings.TrimSpace(id)`：凡不在本场 Run `Eligible` 快照内的技能（含 `Disabled`、`Shadowed`、未受信工作区 `Withheld`、未知技能及 Run 启动后新注册技能）**100% 立即拒绝**，绝不因支持分页而放宽半步读取权限；
     2) 当 `strings.TrimSpace(path) == ""` 或规范化相对路径等于 `"SKILL.md"` 时，**直接构造并返回 `r.ordered[idx].Content` 内存快照（按 `MaxResourceBytes = 128 KiB` 边界返回 `skills.Resource`）**——即便外部在 Run 中途调用 `Registry.Delete` 删除了磁盘上的 `SKILL.md` 或调用 `Registry.Save` 覆写了新内容，当前 Run 的 `ReadSkill` 依然与 `Prompt` / `ListSkills` 100% 一致无损；
     3) 仅当请求技能包内附属相对子路径（如 `references/api.md`）时，基于快照锁定的技能目录调用 `skills.ReadResource(entry.Skill, path)`（严格执行符号链接防逃逸与文本校验；若外部中途物理删掉了该附属参考文件，返回受控工具错误而不崩溃、不越权）；
   - **`Registry` 并发线程安全（`go test -race` 保证）：** 在 `skills.Registry` 内部增加 `mu sync.RWMutex`（`List / Content / Prompt` 加读锁，`Save / Delete / SetEnabled` 加写锁），并将 `Save` 与 `saveState` 中的固定 `path + ".tmp"` 改为同目录 `os.CreateTemp` 唯一临时文件后 `os.Rename`，彻底消除多线程并发读写 `Registry` 时的内存与文件系统竞争。
3. **`Resume` 跨 Run 快照重建语义（回答问题 8）：**
   - 每次 `Resume`（对中断或等待门禁的会话再次调用 `RunAgent` / `RunSupervisor.Start`）均是一次全新的 `Run`（分配新 `runID`），重新完整执行 `Engine.prepareRun`：根据最新配置的工作区信任状态（`plan.trusted`）、最新磁盘技能文件与最新 `skills-state.json` **重新构建合法的新 Run `SkillSnapshot`**，同步刷新新 Run 的 `LayerSkills`、`ListSkills` 与 `ReadSkill`（历史消息行中已落盘的旧工具返回结果作为历史事实保留不变）。
4. **`internal/toolset/skills.go` 双模 Schema、互斥校验与双限分页契约（`REV-F02`、`RC3-NEW-04`）：**
   - 当 `s.skills` 实现 `SkillCataloger` 时，注册无顶层 `required`、无顶层 `oneOf / anyOf` 的扁平四参数 Schema `{id, path, query, offset(minimum: 0)}`（仅实现旧 `SkillReader` 的桩维持旧 `required: ["id"]`）；
   - `readSkill` 严格拦截违规参数组合：显式传空串 `{"id": ""}` 报错拒绝；读取模式（`id != ""`）禁止携带 `query` 或 `offset`；目录模式（省略 `id`）禁止携带 `path` 且拒绝负值/非整数 `offset`；
   - `ListSkills` 同时受 `MaxSkillCatalogPage = 50` 与**含 `SkillCatalogPage` 外层 JSON 包装在内**的 `MaxSkillCatalogResponseBytes = 16384`（`16 KiB`）约束；当页首第 1 条记录（`k == 0`）单独序列化即超 `16384B` 时，对该副本置 `DescriptionTruncated: true` 在 UTF-8 Rune 边界二分缩减 `Description`（必要时再缩 `Name`，不改 `ID/Scope`）保底返回 `1` 条；`HasMore = (offset + len(Skills) < len(matched))`，仅 `HasMore == true` 时输出 `NextOffset = offset + len(Skills)`。

#### 3. 外部 MCP / LSP 工具描述默认保留代码围栏规则（`RC3-NEW-05`）
- 进入压缩前先精确剥离并锁定 `toolset/mcp.go:45` 拼接的尾部 `" (MCP server: <id>)"` 后缀，处理完毕原样拼回；
- 预算触发的 `ModeCompact` 下，`builtin / task / delegation` 及一切非只读外部工具 **100% 逐字节保留全部描述与代码围栏**；只读外部 `mcp:*/lsp` 工具**默认保留所有代码围栏**，仅当同时满足「紧邻上方为独立 `Example:/示例:` 标题 + 围栏闭合 + 标题与围栏内零规范/Schema/环境/顺序/否定信号词 + 局部替换不伤其余段落」四项正证条件时才将单个纯示例围栏替换为 `[example omitted]`。

#### 4. 显式 `ProfileCompact` 下内置工具关键安全约束保留要求（`REV-F06` — P3 实施阶段强制契约，`IMPLEMENTATION_PENDING`）
当候选模型显式配置 `Profile.ToolDescriptions == "compact"` 时，作为 Stage 2 实施阶段强制要求（`IMPLEMENTATION_PENDING`），禁止对以下内置关键工具做盲首句截断：① `read_file` 保留 `anchors=true` 与行号分页前提；② `write_file / replace_in_file / edit_lines` 保留陈旧读拒绝与 `base_rev / rev` 前提；③ `bash`（`background == true`）保留活跃后台作业阻断完成回答约束；④ `eval` 保留变量留存、异常不回滚、取消/超时销毁状态不自动重放及洁净进程验证语义。

### 4.10 `RequestEnvelope` 防截断无损模型身份编码、极限槽位优先级与 `MaxRefsJSONBytes` 字节证明（闭合 `REV-F03`、`REV-F05`、`REV-F07`、`RC33-F03`）

#### 1. 源码与数据库约束核查结论
`request_envelopes` 建表 DDL 含 `CHECK (decision_json IS NULL OR state = 'answered')` 与 `CHECK (json_valid(refs_json) AND json_type(refs_json) = 'array' AND length(refs_json) <= 16384)`（`schema_v6.golden.txt:22`）；`Store.AdmitRequest`（`session/requests.go:138-165`）校验每条引用匹配 `refRE = ^[A-Za-z0-9._:/@#+=%~-]{1,96}$`（`MaxEnvelopeRefBytes = 96`），当 `len(refs) > MaxEnvelopeRefs (128)` 时截断保留 `refs[:128]` 并将 `lineage_complete` 置为 `0`。

#### 2. 带末片终结符的 `EncodeEnvelopeModelRef` 无损双射编码与 Unicode 字节上界证明（闭合 `REV-F03`、`REV-F07`、`RC33-F03`）

1. **配置层双约束（消除 JSON Schema Unicode 码点与 UTF-8 字节差异）：**
   - JSON Schema `maxLength` 按 Unicode 码点计数（单码点最多 `4` UTF-8 字节 $\to$ `12` `%XX` ASCII 字节）；故在 `docs/lumencortex.schema.json` 设置码点 `maxLength` 的同时，在 Go `config` 包强制校验 UTF-8 字节长度：**`MaxProviderIDBytes = 64`**、**`MaxModelKeyBytes = 192`**（原始 `<provider>/<model>` 最大 UTF-8 字节数 $B_{\text{raw}} = 64 + 1 + 192 = \mathbf{257}\text{ B}$，最大转义 ASCII 字节数 $B_{\text{escaped}} \le 3 \times 64 + 1 + 3 \times 192 = \mathbf{769}\text{ B}$）。
2. **百分号转义与带末片终结符 `.~` 的正交分片格式（100% 阻断部分解码）：**
   - 安全直通字符集 `modelRefPassthrough` 为 `A-Z`、`a-z`、`0-9` 及 `. _ : / @ # + = -`（共 71 个单字节字符，严格不含 `%`、`~` 与空格）；`%` 编码为 `%25`，`~` 编码为 `%7E`，其余非直通字节编码为大写 `%XX`（故 `escaped` 中**绝对不含字符 `~`**）。
   - **单条格式（当 `len(escaped) <= 90` 时）：** 输出 `[]string{"model:" + escaped}`（第 6 字节为 `':'`，总长 `9 .. 96` B）。
   - **带末片终结符的有序多分片格式（当 `len(escaped) > 90` 时）：**
     - **非末片（$0 \le i < K - 1$）：** `"model~" + strconv.Itoa(i) + "~" + chunk_i`（头长 **`8` B**，单片最大载荷 `capCont = 96 - 8 = 88` B）；
     - **唯一末片（$i = K - 1$）：** `"model~" + strconv.Itoa(i) + ".~" + chunk_{K-1}`（头长 **`9` B**，在下标数字后紧跟终结符 `.` 再接 `~`，全部字符均属 `refRE`，单片最大载荷 `capTerm = 96 - 9 = 87` B）！
   - **分片边界不变量与最大 `10` 片严格数学证明：**
     - 切分非末片时取 `limit = min(capCont, len(rem)-1)` 并向回退至最近一个完整原始 UTF-8 码点起始位置（最多回退 `11` B，绝不劈开 `%XX` 三元组或多字节 UTF-8 码点序列，每一分片单独逆转义均为合法 UTF-8 且载荷非空）；
     - 当 `len(rem) > 88` 时，每个非末片至少消耗 $88 - 11 = \mathbf{77}$ 个转义字节；因此经过最多 `9` 个非末片（$i = 0 \dots 8$）后，已消耗至少 $9 \times 77 = \mathbf{693}$ 字节，剩余待编码字节至多为：
       $$769 - 693 = \mathbf{76}\text{ bytes} \le \mathbf{87}\text{ bytes}\ (\text{capTerm})!$$
       （若在更早下标 $i \le 8$ 处恰遇 `len(rem) == 88`，`limit = 87` 使第 $i$ 片消耗 `76..87` B、余 `1..12` B $\le 87$ B 在第 $i+1 \le 9$ 片立即终结）。因此，**对 100% 合法 Unicode 输入，总分片数严格满足 $2 \le K \le K_{\max} = \mathbf{10}$，分片下标恒落在单数字 `0..9` 内！**
3. **准入前完整性硬守卫与解码器零部分解码契约：**
   - **编码准入守卫（禁止截断模型分片）：** `EncodeEnvelopeModelRef(ref)` 校验 `ref.Split()`、`utf8.ValidString`、`len(provider) <= 64`、`len(model) <= 192` 且产出 `1 <= len(chunks) <= 10`；若异常（如测试绕过配置直传超界字符串）立即返回错误阻止请求准入，**绝不允许截断 `ModelRef` 分片后写库**；
   - **解码校验（禁止部分解码模型身份）：** `DecodeEnvelopeModelRef(refs)` 扫描全部 `"model:"` 与 `"model~"` 引用：若为空、混用 `"model:"` 与 `"model~"`、存在多条 `"model:"`、分片未从 `0` 严格连续递增、中间片误带 `.~`、**或最后一片缺少终结标记 `.~`（`ErrIncompleteModelRefChunks`）**、或总长/转义/UTF-8 校验失败，一律立即返回错误，**在数学上 100% 杜绝将残缺分片部分解码为错误模型名**！

#### 3. 极限槽位预算（`10 + 7 + 111 = 128`）、严格优先级与 `MaxRefsJSONBytes` 字节证明（闭合 `REV-F05`、`RC33-F03`）

1. **`PurposeMain` 信封 `Refs` 严格优先级布局与极限槽位核算：**
   `requestRefs` 严格按以下不可颠倒的四级优先级顺序构造 `[]string`：

   | 优先级顺序 | 引用类别与格式 | 最坏极端条数 | 常规条数 | `len(refs) > 128` 时的截断保护保证 |
   | :---: | :--- | :---: | :---: | :--- |
   | **P0（最高）** | 模型身份：`EncodeEnvelopeModelRef(m.Ref)`（`model:...` 或 `model~0~..model~9.~`） | **`10`** | `1` | 位于 `Refs[0..K-1]`（`K <= 10`），**100% 永不截断、顺序恒定不变** |
   | **P1** | 投影模式：`proj:mode:<full\|compact>` | **`1`** | `1` | 位于 `Refs[K]`（下标 `<= 10`），**100% 永不截断** |
   | **P2** | 系统身份：`sys:prompt:<h>`（`1`）+ 实发 `SentLayers` `sys:<kind>:<h>`（最多 `3`：`global-instructions, project-instructions, skills`）+ `sys:instructions:<h>`（最多 `1`） | **`5`** | `2..5` | 位于下标 `<= 15`，**优先于一切 `TaskState` 引用，100% 永不截断** |
   | **P3** | 状态版本头：`ts:v<version>` | **`1`** | `1` | 位于下标 `<= 16`，**固定头合计最多 `10 + 1 + 5 + 1 = 17` 条，100% 完整保留** |
   | **P4（尾部）** | 实渲 `TaskState` 条目：`RenderBlock.ItemRefs`（仅含 `startByteOffset < cutoff` 真实暴露给模型的 `ts:brief, ts:ref:*, ts:workflow, ts:unit:*, ts:note:*, ts:task:*`） | 剩余可用槽位 **`128 - 17 = 111`**（常规 `120`） | `0..30` | `<= 111` 条（总计 `<= 128`）时 100% 保留且 `LineageComplete = true`；`>= 112` 条（总计 `>= 129`）时仅从 P4 尾部第 `112` 条起截断并**诚实置 `LineageComplete = false`** |

2. **`MaxRefsJSONBytes = 16,384` 序列化字节安全边界严格证明：**
   - `AdmitRequest` 经 `refs[:128]` 截断后再执行 `raw, err := json.Marshal(refs)`（`session/requests.go:154-165`）；
   - 每条合法 `ref` 严格匹配 `refRE = ^[A-Za-z0-9._:/@#+=%~-]{1,96}$`，其 73 个允许字符全部为无需 JSON 转义的纯单字节可打印 ASCII（不含 `"`, `\`, `<`, `>`, `&` 或控制字符）；
   - 因此，当 `len(refs) == 128` 且每条均取满最大长度 `MaxEnvelopeRefBytes = 96` 字节时，`json.Marshal(refs)` 的精确最大字节长度为：
     $$\max \text{len}(\text{refs\_json}) = 2\ (\text{外括号 }[]) + 128 \times (96 + 2\text{ 引号}) + 127\ (\text{逗号}) = \mathbf{12,673}\text{ bytes} < \mathbf{16,384}\text{ bytes}\ (\text{MaxRefsJSONBytes})!$$
   - 距离 SQLite 表级 `CHECK (length(refs_json) <= 16384)` 恒有至少 **`3,711` 字节**的绝对安全余量，任何合法截断后的信封永远不会触发 `MaxRefsJSONBytes` 存储拒绝！

### 4.11 五级验证与重放能力边界及 `ReplayDecision` 实施要求（闭合 `RC3-F04`、`REV-F06`）

| 验证 / 重放层级 | 权威持久化/内存输入 | 精确可验证内容 | 支持状态与边界声明 |
| :--- | :--- | :--- | :--- |
| **1. Decision Replay**<br>（`VerifyStep` / `ReplayDecision`） | `agent_steps.json` 中单步自包含的 `Decisions`（纯数值/摘要 `Inputs` 与 `Output`） | 离线重算并校验候选静态预筛（含 `CompactFixedByModel` 与 `v1` `Base` 兼容）、前缀模式选择（`RuleWindowPrefix`）、受控修复下界放宽（`RuleWindowFloorRelaxed`）及最大可用候选回退（`RuleFailoverFloorFallback`）、滑窗丢弃组数（`window.dropped`）、容量减半重试、`TurnBoundary` 与 `Failover` 跳转链一致 | **100% 支持（单步自包含可验证）** |
| **2. Request Envelope Verification**<br>（请求信封、防截断模型身份与血缘核验） | SQLite `request_envelopes`（含 `Refs` 前置带 `.~` 终结符的模型分片）、`session_messages`、`derived_origins`、`source_fences` | 验证每次外部调用前必有预检信封、**脱离配置文件离线 100% 无损且防部分解码还原每次尝试的真实 `ModelRef`**、系统引用优先保全、`Refs` 精确匹配实渲 `TaskState`、撤回源阻断正确 | **100% 支持（离线数据库独立可验证）** |
| **3. Static Projection Verification**<br>（同快照静态投影确定性验证） | 调用当刻相同的内存输入快照 `(cand, Model, Options, canonicalStepInput, transcript含EchoTokens, repairContext)` | 验证 7 步无环 `projectCandidate` 纯函数无副作用、多次调用字节级一致，且 `SentLayers / Tools / Window` 哈希与 `RequestEnvelope` 完全吻合 | **仅在给定相同内存输入快照时支持（运行期与确定性单测可验证）** |
| **4. Full Projection Replay**<br>（从历史数据库重建完整 `ProviderRequest`） | 历史 SQLite 数据库（缺历史瞬态 `r.turn` Thinking Echo、缺历史外部文件/MCP 原文） | 企图离线逐字节还原历史某步发出的完整 `ProviderRequest`（含 `Messages` 原文、`Tools` 原文与私有 `Echo`） | **明确不支持（严禁为重放持久化模型私有 Thinking Echo）** |
| **5. Full Run Replay**<br>（整场运行重放） | 外部非确定性 LLM 采样、网络时序与外部工具状态 | 重跑整场多步 Agent 任务得到相同轨迹 | **明确非目标（Non-Goal）** |

## 五、Go 源码映射、具体接口签名与分阶段实施计划

### 5.1 文件级实施映射清单（基于 `c73d173` 核验行号，零新包、零第二套账本、零 SQLite Schema 改动）

| 序号 | 文件路径（相对 `wt/core-cognitive-v1-integration-20261116`） | 现有符号与真实行号（`c73d173`） | `v0.4-rc3.5` 实施改动摘要 |
| :--- | :--- | :--- | :--- |
| **1** | `internal/window/estimate.go` | `Estimate` (`19-36`) | 新增 `EstimateBytes(b []byte) int` 与 `ReserveTokensForBytes(n int) int`（`floor((7*n+11)/12) + 4`）；`Estimate` 序列化后调 `EstimateBytes`。 |
| **2** | `provider/anthropic/wire.go` & `provider/openai/wire.go` | `thinkingEcho` (`479`), `responseEcho` (`222`) | 改调 `window.EstimateBytes` 统一 Thinking Echo 的输入 Token 估算口径。 |
| **3** | `config/types.go`, `config/schema.go` & `docs/lumencortex.schema.json` | `ModelRef.Split` (`23`), `providers` (`261`), `models` (`317`) | 新增常量 `MaxProviderIDBytes = 64`、`MaxModelKeyBytes = 192` 并在配置加载/保存与 JSON Schema 中同步强制校验（闭合 `REV-F07`）。 |
| **4** | `internal/skills/types.go` & `internal/skills/registry.go` | `Registry` (`21`), `PromptResult` (`75`), `Save` (`167`), `Prompt` (`278`), `scanRoot` (`426`) | `Registry` 增加内部 `sync.RWMutex` 并改用 `os.CreateTemp` 原子写盘消除并发竞争；`PromptResult` 返回按 `ID` 排序的不可变 `Eligible []SnapshottedSkill`（保留 `scanRoot` 已读 `SKILL.md` 正文）、`Text (<=32KiB)`、`CompactText (<=8KiB)` 与单根 `MaxSkills=128` 截断 `Warnings`（闭合 `RC33-F02`）。 |
| **5** | `internal/toolset/skills.go` & `runtime/skills.go` | `SkillReader` (`11-38`), `skillReader` (`18-37`) | `runtime.skillReader` 持有不可变 `[]skills.SnapshottedSkill` 与 `byID`（`ReadSkill` 对 `""/"SKILL.md"` 直接返回快照正文，彻底免疫运行中途磁盘删改）；`toolset/skills.go` 按 `SkillCataloger` 暴露无顶层 `required`/`oneOf` 双模 Schema，强制互斥、`50` 条 + `16384B` 双限与 `DescriptionTruncated` 保底（闭合 `REV-F02`、`RC33-F02`）。 |
| **6** | `runtime/prompt.go` & `runtime/agent.go` | `skillsLayer` (`206`), `EffectivePrompt` (`295`), `prepareRun` (`420-458`) | 实现围栏感知 `compactProjectInstructions`（附 `anchors=true` 指引）；`prepareRun` 与 `EffectivePrompt` 将单次 `assemblePrompt` 产出的 `prompt.Skills.Eligible` 唯一传入 `newSkillReader`，保证 `Prompt / ListSkills / ReadSkill` 严格共享同一 Run 快照。 |
| **7** | `internal/taskstate/render.go` | `Render` (`60-85`) | 新增 `RenderBlock(goal, s, wf) RenderedBlock`，按 `MaxBlockBytes = 10240` 真实截断偏移 `cutoff` 导出 `startByteOffset < cutoff` 的实渲 `ItemRefs`；`Render` 委托 `RenderBlock(...).Text`（闭合 `REV-F05`）。 |
| **8** | `runtime/models.go`, `runtime/auto.go` & `runtime/verification.go` | `planModels` (`208`), `admitAutoJudge` (`173`), `admit` (`518`) | `planModels` 填入候选 `ProviderPkg` 与独立 `Presentation`；`admitAutoJudge` 与 `providerReviewer.admit` 调 `agent.EncodeEnvelopeModelRef(ref)` 写入信封 `Refs`。 |
| **9** | `internal/editing/engine.go` & `internal/toolset/{editing,files,patch,editing_tools}.go` | `Commit` (`254-325`), `write` (`520-544`), `writeCAS` (`568-606`), `rollback` (`609-632`), `commitFailure` (`95-110`) | `Engine.Commit` 输出精确 `CommitReceipt`（含全字节判定 `Wrote`、`OldSHA256`、`NewSHA256`、`writeCAS` 全字节重读校验，以及 `Truncate` 后写失败或 `rollback` 出错的 `DirtyFailure`）；`toolset` 统一经 `commitPlan` 将 `CommitReceipt` 写入宿主私有 Context 槽并按实际 `Wrote || DirtyFailure` 诚实设置 `MutatesWorkspace`（闭合 `RC34-F02`）。 |
| | | 〔历史行号与 `rollback` 引用；rc3.8 起见场景 F〕 | R7 删除 `rollback`；`DirtyFailure` 覆盖已发布前缀后的失败、部分修改及不确定发布。 |
| **10** | `internal/agent/types.go`, `internal/agent/stepspecs.go` & `internal/session/executions.go` | `Store` (`40`), `StepTools` (`71`), `Execution` (`376`) | 定义三值 `MutationEvidence`（`toolCallRecord` 零改动）；`stepspecs.go` 新增 `IncludedStepTools`；`session/executions.go` 新增只读查询 `ListAnsweredExecutions(ctx, sessionID, fromSeq)`（供 `r.load` 与步末统一调用，闭合 `REV-F04`、`RC34-F01`）。 |
| **11** | `internal/agent/presentation.go` & `internal/agent/policy.go` | `presentSpecs` (`29`), `presentFeedback` (`61`), `Candidates` (`144`), `ReplayDecision` (`792`) | 新增 `presentStepTools`、`compactExternalToolDescription`（默认保围栏）、显式 `ProfileCompact` 内置防呆描述（`REV-F06` P3）、`presentFeedback` `2458B / 12346B` 双守卫，以及 `CandidatesByStaticPrefix / GuardTokens / BaseGateSlack / EffectiveGateSlack / WindowPrefixDecision / ReplayDecision`。 |
| **12** | `internal/agent/{execution,callback,transcript,envelope,loop,send}.go` | `postFiles` (`255-282`), `callback.go:292-360`, `load` (`37`), `groups` (`126`), `requestRefs` (`82`), `loop.go:530-615`, `send.go:180-365` | 每次准入后在 `loop.go` 与 `callback.go` 注入逐 `ExecutionID` 隔离的宿主私有 `commitRecorder`；`postFiles` 执行「`CommitReceipt` 实写 + `SnapshotFiles` 前后原始磁盘 `SHA256 == NewSHA256` 夹逼 + Git Blob 净变」三重核验并在 `post.PostRef` 原子追加 `;cm=proven|none|unknown`（`DirtyFailure` 升格 `OutcomeUnknown`）；`transcript.go` 实现 `splitFilesSnapshotPostRef / classifyExecutionSummary / aggregateGroupMutation / deriveRepairContext` 与 `r.groups` 防裁剪守卫；接入带 `.~` 终结符信封 `v2` 与 7 步无环 `projectCandidate`（闭合 `RC34-F01`、`RC34-F02`）。 |
| **—** | `internal/repository/{snapshot,blobs}.go`, `internal/window/window.go`, `internal/migrate/*`, `internal/cognition/*`, `internal/verify/*` | 快照采集原语、滑窗内核与 SQLite Schema | **100% 零改动**（零触碰快照采集命令、零新增数据库表或列、零第二套文件修改账本、零 SQLite Migration、零修改 `agent_steps` Schema、零修改 Cognitive Core 与验证门禁语义）。 |

### 5.2 具体 Go 函数签名草案（与 §4 公式及 §6 测试 100% 一致）

```go
// config/types.go
const (
	MaxProviderIDBytes = 64
	MaxModelKeyBytes   = 192
)

// internal/window/estimate.go
func EstimateBytes(b []byte) int
func ReserveTokensForBytes(n int) int

// internal/taskstate/render.go (REV-F05)
type RenderedBlock struct {
	Text     string
	ItemRefs []string
}
func RenderBlock(goal string, s State, wf *Workflow) RenderedBlock

// internal/skills/types.go, internal/toolset/skills.go & runtime/skills.go (REV-F02 + RC33-F02)
type SnapshottedSkill struct {
	Skill
	Content string // SKILL.md body captured atomically during Registry.Prompt
}

const (
	MaxSkillCatalogPage          = 50
	MaxSkillCatalogResponseBytes = 16 << 10 // 16,384 bytes (measured on json.Marshal(SkillCatalogPage))
)

type SkillCatalogEntry struct {
	ID                   string `json:"id"`
	Name                 string `json:"name,omitempty"`
	Scope                string `json:"scope"`
	Description          string `json:"description"`
	DescriptionTruncated bool   `json:"descriptionTruncated,omitempty"`
}

type SkillCatalogPage struct {
	TotalEligible int                 `json:"totalEligible"`
	Matched       int                 `json:"matched"`
	Offset        int                 `json:"offset"`
	NextOffset    int                 `json:"nextOffset,omitempty"`
	HasMore       bool                `json:"hasMore"`
	Skills        []SkillCatalogEntry `json:"skills"`
}

type SkillReader interface {
	ReadSkill(ctx context.Context, id, path string) (any, error)
}

type SkillCataloger interface {
	SkillReader
	ListSkills(ctx context.Context, query string, offset int) (SkillCatalogPage, error)
}

// runtime/skills.go: newSkillReader(eligible []skills.SnapshottedSkill) toolset.SkillReader

// internal/editing/engine.go & internal/agent/execution.go (RC34-F02 — 每 ExecutionID 私有 Commit 回执与 post_ref 封签)
type FileCommitReceipt struct {
	Path      string
	Wrote     bool
	Created   bool
	Deleted   bool
	OldSHA256 string
	NewSHA256 string
}

type CommitReceipt struct {
	Committed    bool
	DirtyFailure bool
	Files        []FileCommitReceipt
}

func WithCommitRecorder(ctx context.Context, fn func(CommitReceipt)) context.Context
func RecordCommitReceipt(ctx context.Context, r CommitReceipt)

// internal/session/executions.go (REV-F04 — 只读聚合查询，零 DDL 变更)
type AnsweredExecutionSummary struct {
	AnsweredSeq    int64
	RequestID      string
	ExecutionID    string
	ToolName       string
	PreRef         string
	PostRef        string // tierFiles 格式: "files:<N>:sha256:<64hex>;cm=<proven|none|unknown>"
	PreUnavailable bool
	Coverage       Coverage
	Outcome        ExecutionOutcome
	FileEdgeCount  int
}
func (s *Store) ListAnsweredExecutions(ctx context.Context, sessionID string, fromSeq int64) ([]AnsweredExecutionSummary, error)

// internal/agent/transcript.go (REV-F01 + REV-F04 + RC33-F01 + RC34-F01 + RC34-F02 + F-02 + F-04 — 零正文 HistoryGroupMeta + SQLite 流式重建!)
const MaxInMemoryHistoryGroups = 2048

type MessageStreamer interface {
	EachMessageFrom(ctx context.Context, sessionID string, fromSeq int64, fn func(seq int64, msg protocol.Message) error) error
}

func hasPositiveFilesSnapshotRef(ref string) bool
func splitFilesSnapshotPostRef(postRef string) (snapRef string, seal string)
func classifyExecutionSummary(rec session.AnsweredExecutionSummary) MutationEvidence
func aggregateHistoryGroupMutation(meta HistoryGroupMeta, execs []session.AnsweredExecutionSummary) MutationEvidence
func DeriveRepairContextFromHistory(groups []HistoryGroupMeta) repairContext
func (r *run) deriveRepairContext(ctx context.Context, store Store, sessionID string) (repairContext, error)
func (r *run) groups(maxContext int) ([][]window.Entry, error)

// internal/agent/presentation.go & policy.go (REV-F06 + F-03 + F-06)
const (
	CompactFeedbackBytes                     = 1200
	MaxDetailedFeedbackBytes                 = 6144
	GateMessageEmptyJSONBytes                = 58
	MaxCompactGateJSONBytes                  = 2458  // 58 + 2*1200
	MaxDetailedGateJSONBytes                 = 12346 // 58 + 2*6144
	ProposalReserveTokens                    = 512
	RuleWindowPrefix                         = "window.prefix"
	RuleWindowFloorRelaxed                   = "window.floor_relaxed"
	RuleFailoverFloorFallback                = "failover.floor_fallback"
	FloorRelaxReasonMaxCandidateOverflow     = "max_candidate_repair_floor_overflow"
)
func presentStepTools(tools []StepTool, profileVariant string, mode ProjectionMode) []protocol.ToolSpec
func compactBuiltinToolDescription(spec protocol.ToolSpec) string
func compactExternalToolDescription(spec protocol.ToolSpec, readOnly bool) string
func presentFeedback(text, variant string) string

// internal/agent/envelope.go (REV-F03 + REV-F05 + REV-F07 + RC33-F03)
const (
	projectionVersion = 2
	MaxModelRefChunks = 10 // proven upper bound with terminal seal "model~<K-1>.~"
)
func EncodeEnvelopeModelRef(ref config.ModelRef) ([]string, error)
func DecodeEnvelopeModelRef(refs []string) (config.ModelRef, error)
func requestRefs(modelRefs []string, opts Options, sentLayers []SystemLayer, mode ProjectionMode, version int64, renderedStateRefs []string) []string
```

### 5.3 分阶段实施顺序（Astra 执行蓝图）

- **Stage 0（ADR 审批阶段）：** 仅依据本 `v0.4-rc3.5` 提交 `docs/adr/0024-candidate-context-projection-and-budget-guards.md`（及 `.zh-CN.md`），不改动任何 Go 生产代码，获批后进入三阶段原子编码。
- **Stage 1（估算与序列化预留基元、Run 级不可变 `SkillSnapshot` 与双模 `read_skill`、围栏感知 `anchors=true` 大纲、`RenderBlock` — Commit 1）：** 完成 `window.{EstimateBytes, ReserveTokensForBytes}` + Wire `Echo` 对齐、`presentFeedback` 双守卫、`skills.Registry` 读写锁 + `SnapshottedSkill` + 单根 `128` 截断告警 + `skillReader` 不可变快照 + `toolset/skills.go` 双模互斥接线、`runtime/prompt.go` 围栏大纲、`taskstate.RenderBlock`，通过 `T5, T6, T10` 单测。
- **Stage 2（配置 UTF-8 字节上限、候选 Profile、内置紧凑保底与 MCP 保围栏呈现、静态预筛、`ReplayDecision` 补齐与带 `.~` 终结符无损分片信封 `v2` — Commit 2）：** 完成 `config` + `lumencortex.schema.json` 字节上限、`runtime/{models,auto,verification}.go`、`stepspecs.go`、`presentation.go`（含 `REV-F06` 内置工具保底描述）、`policy.go`（含 `REV-F06` `ReplayDecision` 新分支）、`envelope.go`（`EncodeEnvelopeModelRef / DecodeEnvelopeModelRef` + `17+111=128` 优先级组装），通过 `T9, T11, T12` 单测。
- **Stage 3（`editing.CommitReceipt` 因果回执、`postFiles` 三重联合核验与 `;cm=*` 原子封签、单源 `answered_seq` 三值 `MutationEvidence`、前向单调 `deriveRepairContext`、`r.groups` 防裁剪守卫、7 步无环 `projectCandidate`、`TurnBoundary` 试算与 `BeforeModelCall` 闭环 — Commit 3）：** 完成 `editing/engine.go` + `toolset/{editing,files,patch,editing_tools}.go`、`agent/{execution,callback}.go`、`session.ListAnsweredExecutions`、`transcript.go`、`loop.go` 与 `send.go` 接线，跑通全量 `T1–T12`（含 `RC34-F02` 全部 10 项强制因果并发与崩溃恢复测试）及全仓 `go test -race -count=1 ./...`。

## 六、离线确定性测试验收矩阵（`T1–T12`）

所有验收用例均使用离线脚本化 `protocol.Provider` 替身、确定性固定语料与本地 SQLite 执行，**零付费模型请求、每项均具备可编程验证断言**：

| 编号 | 确定性输入 | 执行路径（真实函数调用链） | 预期结果 | 失败判定条件 | 需要捕获的证据 |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **T1** | 候选链 `[m-32k, m-200k]` 及反向 `[m-200k, m-32k]`；40 个只读 `mcp:ext` 工具（各含 `200B` 说明 + 带独立 `### Example:` 标题的非规范 `1 KiB` ```` ```json ```` 纯示例块）+ `40 KiB` 项目 `AGENTS.md` + 60 个启用技能 | `Loop.Run` -> `CandidatesByStaticPrefix` -> `Loop.send`（首候选模拟 503）-> 7 步无环 `projectCandidate` -> `Provider.Complete` | 两候选均通过静态预筛；`m-32k` 以 `fixed_prefix_budget` 发送 `ModeCompact`（折叠已正证纯示例块并完整保留 `(MCP server: ext)`、大纲化项目指令、`8 KiB` 技能目录）；`m-200k` 以 `fits_full` 发送 100% 原始 `ModeFull` | `m-32k` 被预筛淘汰、`m-200k` 收到被压缩污染的前缀，或 `(MCP server: ext)` 后缀丢失 | 两侧 `ProviderRequest` JSON 快照、`RuleModelCandidates` 与 `RuleWindowPrefix` 决策记录 |
| **T2** | `m-64k` 模型（`ToolDescriptions=""`，`fullFixed = 8K <= usableFull/2`）；Step 2 工具返回 `22K`-token 大输出（仅 `ModeCompact` 装得下），Step 3 普通 `300`-token 返回 | 连续执行 `Loop.Run` Step 1 -> Step 2 -> Step 3 -> 7 步 `projectCandidate` -> `WindowPrefixDecision` | 三步模式严格为 `[full (fits_full), compact (newest_group_fit), full (fits_full)]`，Step 3 不被 Step 2 峰值跨步锁死 | Step 2 溢出失败，或 Step 3 仍停留在 `compact` | 三步 `agent_steps.json` 中 `RuleWindowPrefix` 的 `Inputs/Output` 序列 |
| **T3** | **子集 A（Failover 优先守卫、固定 Gate State + 三级受控 Floor Relaxation `R1->R2->R3` 与终极溢出边界 — `v0.4-rc3.7` `F-03 + R2-02` 修订）：** A1 候选 `[m-32k, m-200k]`，大体积修复保留区验证 `m-32k` 绝不越级放宽而是优先 `Failover` 至 `m-200k` 完整携带 `MinRequiredSeq`；A2 全部更大候选不可用/耗尽时，验证最大可用候选在 **5 档模型窗口配置（`32768/4096`、`64000/8192`、`128000/8192`、`200000/16384`、`600000/32768`）** 下同时通过「门禁前长回合」与「门禁后长修复期（40+ 步工具调用）」：先走 `Stage R1`（`[BlockSeq..LastSeq]` 原文无重复），当 `[BlockSeq..LastSeq]` 亦超窗时自动升级 `Stage R2/R3`（将受 `presentFeedback` 封顶的固定 Gate State + 因果进展台账固定进当步 `State`、消息窗仅在 `Seq > BlockSeq` 上按因果锚点/最新后缀滚动、记录 `GatePinnedInState=true` 的 `RuleWindowFloorRelaxed`、真实 SQLite 连续运行 / 退出 / 冷启动 `Resume` 全结构 `DeepEqual`），而当连固定 Gate State + 最新原子组也无法容纳（如 `80K` 最新原子组对最大 `64K` 候选）时受控报 `StatusFailed / KindWindowOverflow`；<br>**子集 B（RC34-F02 强制因果并发测试 1–6 + RC34-F01 全工作区/忽略文件矩阵 + `F-01 / R2-01` 同目录临时文件原子 no-replace 发布与真实双 OS 进程原地写防误删 15 场景 + `F-04` `debug_*` 写盘测试 + `F-09` Windows 斜杠路径配对）：** 在 `.gitignore` 配置 `build/` 与 `*.secret` 的真实临时 Git 工作树中，分别在「首次门禁阻断前」与「门禁阻断 `b_last` 后修复段」按精确时序钩子执行：<br>  - **RC34-F02-T1（`A -> Agent Write B`）：** 初始 `a.go=A` -> `admitExecution(PreRef=Blob(A))` -> `Engine.Commit` 实写 `B`（`Wrote=true, NewSHA256=H(B)`）-> `postFiles` 夹逼核验通过；<br>  - **RC34-F02-T2（`A -> Host Write B -> Agent Write B`，未读过或 `Resume` 清空 `SnapshotStore`）：** 初始 `a.go=A` -> `admitExecution(PreRef=Blob(A))` -> 测试钩子模拟宿主外部落盘 `B` -> Agent 调 `write_file("a.go", B)`，`Engine.Commit` 因 `p.next==p.original` 跳过写盘（`Wrote=false`）返回 `OK=true` -> `postFiles` 测得 `snapRef=Blob(B) != PreRef`；<br>  - **RC34-F02-T3（`A -> Host Write B -> Agent Write C` 三分支）：** 初始 `a.go=A` -> `admitExecution(PreRef=Blob(A))` -> 宿主外部改 `B` -> 分别执行 **C1**（Agent 覆写 `C != B && C != A`，`Wrote=true` 且写后磁盘夹逼 `==H(C)`）、**C2**（Agent 写回 `C == A`，`Wrote=true` 但 `PreRef == snapRef`）、**C3**（Agent 写 `C` 后在 `postFiles` 夹逼前由宿主迅速覆写为 `D`，写后 `H(D) != H(C)`）；<br>  - **RC34-F02-T4（`A -> Agent Write A`）：** 初始 `a.go=A` -> Agent 调 `write_file("a.go", A)`（`Wrote=false, PreRef==snapRef`）；<br>  - **RC34-F02-T5（`Agent Read A -> Host Write B -> Agent Write A/C`）：** 同 Run 先调 `read_file("a.go")` 注册快照 `Rev(A)` -> `admitExecution(PreRef=Blob(A))` -> 宿主外部改 `B` -> Agent 调 `write_file/replace_in_file/edit_lines` 触发 `CodeStale / CodeConflict` 写前拒绝；<br>  - **RC34-F02-T6（`apply_patch` 多文件部分成功/回滚与单文件截断写失败）：** 分别执行 ① 补丁含 `[f1(合法), f2(写盘期注入错误触发 best-effort rollback 成功)]`、② `rollback` 恢复失败残留部分写入、③ 单文件 `writeCAS` `f.Truncate(0)` 后注入 `Write` I/O 错或 `CodeConcurrentWrite`、④ 写前校验失败（`old_text not found`、`files:0:`、路径越界 `../escape.txt`）；<br>  - **RC34-F01 对照集（7–15）及 `F-01 / R2-01 / F-04 / F-09`：** 保持覆盖 `tierFiles` 改写 Git-ignored 文件、`tierWorkspace` 修改/新建/删除/批量生成 Git-ignored 文件（`build/generated.go, app.secret`）、`bash("touch / git add / git commit")`、`edit_go / lsp_rename / eval_artifact`、真实工作区写入 `debug_*`、`mcp__*`（`read/write/exec`）、`"a\\b.go"` vs `"a0\\x.go"` 斜杠规范匹配、`R2-01` 外部原地写/追加/覆盖/双 OS 进程创建竞争零误删、`>512KiB / >4MiB` 截断与符号链接 | `admitExecution` -> `WithCommitRecorder(toolCtx, x.id)` -> `Tools.Execute`（`Engine.Commit` 回填 `CommitReceipt`）-> `postFiles`（`filepath.ToSlash` 键映射三重联合核验签发 `;cm=*`）-> `CommitToolResult` -> `Store.ListAnsweredExecutions` -> `classifyExecutionSummary` -> `aggregateHistoryGroupMutation` -> `DeriveRepairContextFromHistory`（含 `baseSM` 结合律增量折叠）-> `r.groups` -> `projectCandidateForSend`（含 `R1->R2->R3` 三级受控松弛） | **A1/A2：** `m-32k` 严格下界溢出时优先 `Failover` 至 `m-200k`（零放宽）；全部更大候选耗尽时 5 档配置在门禁前与门禁后长修复期均不死于旧断崖，`State` 与 `Messages` 零重复，仅在固定 Gate State + 最新原子组无法容纳时受控报 `StatusFailed / KindWindowOverflow`；<br>**子集 B 持久化记录、期望 `MutationEvidence` 与 `MinRequiredSeq` 变化：** <br>  - **RC34-F02-T1 & T3-C1（及 `tierFiles` 实写忽略文件）：** 持久化 `Outcome=completed, PreRef=Blob(A), PostRef="files:1:...;cm=proven", FileEdgeCount=1` $\implies$ **`MutationProven`**，门禁阻断后**唯一合法将 `MinRequiredSeq` 推进至 `b_last`**；<br>  - **RC34-F02-T2（宿主改 `B` 后 Agent No-op 写 `B`）：** 持久化 `Outcome=completed, PreRef=Blob(A) != snapRef(B), FileEdgeCount=1, PostRef="...;cm=unknown"` $\implies$ **`MutationUnknown`**，门禁阻断后 **`MinRequiredSeq` 保持原值绝不推进**，首次阻断前成功锁定 `preBlockFloor`；<br>  - **RC34-F02-T3-C2/C3：** 持久化 `Outcome=completed, PostRef="...;cm=unknown"` $\implies$ **`MutationUnknown`**，`MinRequiredSeq` 绝不推进；<br>  - **RC34-F02-T4：** 持久化 `Outcome=completed, PreRef==snapRef, FileEdgeCount=0, PostRef="...;cm=none"` $\implies$ **`MutationNone`**，`MinRequiredSeq` 绝不推进；<br>  - **RC34-F02-T5：** 持久化 `Outcome=failed, PreRef(A)!=snapRef(B), FileEdgeCount=1, PostRef="...;cm=unknown"` $\implies$ **`MutationUnknown`**，`MinRequiredSeq` 绝不推进；<br>  - **RC34-F02-T6：** ①–③ 因 `DirtyFailure=true` 持久化 `Outcome=outcome_unknown, PostRef="...;cm=unknown"`（且 `MutatesWorkspace=true`），④ 持久化 `Outcome=failed, PostRef="...;cm=unknown"` $\implies$ 全部 **`MutationUnknown`**，`MinRequiredSeq` 绝不推进；<br>  - **RC34-F01 7–15 + `F-04` + `R2-01`：** 全部 `tierWorkspace`（含忽略文件增删改与 `debug_*`）与 `mcp__*` 100% 为 **`MutationUnknown`**，一切并发外部创建或原地写入文件 100% 逐字节保留 | 小模型在更大候选可用时触发 Floor Relaxation、门禁后修复期未满单原子组上限即发生溢出断崖、`State` 与 `Messages` 重复包含门禁反馈、`R2-01` 误删外部原地写文件、`debug_*` 误判 `MutationNone`、`F-09` 反斜杠错配 | 每项用例的完整五元组转储与 `bench/evidence/adr-0024-r2-remediation-20261119/` 持久化原始日志 |
| **T4** | **RC34-F02 强制多调用隔离与崩溃恢复测试 7–10 + 全 8 类跨 `Resume` 一致性矩阵：** <br>  - **RC34-F02-T7（同一 `answered_seq` 多工具调用隔离与 `Unknown` 一票否决）：** 单步 `assistant` 在门禁阻断后依次测试 ① `[write_file(p1, B) 实写, write_file(p2, A) 同内容幂等, read_file(p3)]`、② 同文件连续两次调用 `[write_file(p1, B) 实写, write_file(p1, B) 幂等]`、③ `[write_file(p1, B) 实写 (;cm=proven), write_file(p2, B) 遇宿主先改 B 跳过写盘 (;cm=unknown)]`、④ `[write_file(p1, B) 实写, bash("./gen.sh") / write_file("../escape", "x")]`（含 `apply_experience` 嵌套多动作对照）；<br>  - **RC34-F02-T8（工具 `Engine.Commit` 实际落盘后、`CommitToolResult` 提交前进程崩溃）：** `write_file("a.go", B)` 已完成 `AdmitExecution`（`outcome='admitted'`）并已将 `B` 落盘，在进入 `CommitToolResult` 写库前模拟崩溃并启动 `Resume`（另覆盖 `apply_experience` 嵌套动作 #1 已 `SettleExecution(;cm=proven)`、动作 #2 或外层 `CommitToolResult` 前崩溃，以及 `NextStep` 重用同一 `step` 号）；<br>  - **RC34-F02-T9（`CommitToolResult` 单事务落盘后崩溃并 `Resume`，以及历史无 `;cm=` 旧行升级兼容）：** 分别对 `;cm=proven`（场景 A）、`;cm=unknown`（场景 B 宿主先改 `B` 后 Agent No-op 写 `B`）、`;cm=none`（场景 D）完成 `CommitToolResult` 后立即模拟进程崩溃重启（清空内存 `editing.SnapshotStore` 与 `r.mutations`），执行 `Resume` 比对恢复前后的 `r.mutations` 与 `MinRequiredSeq`；同时注入一条无 `;cm=` 后缀的旧版 `v0.4-rc3.4` `derived_origins` 行验证向后安全降级；<br>  - **RC34-F02-T10（多轮连续门禁打回下 `MinRequiredSeq` 单调性与冷恢复恒等）：** 构造完整长链路 `u -> G1(bash 改忽略文件 Unknown) -> G2(write_file 实写 Proven) -> Block1 -> G3(宿主先改 B + Agent write_file(B) No-op ;cm=unknown) -> Block2 -> G4(write_file 实写 C ;cm=proven) -> Block3 -> 新外部用户输入 u2`，在每一步同时记录在线值与冷启动 `Resume` 重算值（并保留 Git-ignored 跨 `Resume` 与 MCP 权限改写/卸载跨 `Resume` 验证） | `Store.Reconcile`（`reconcile.go:118-137, 229-239` 补写 `OriginToolInterrupted` 并将残留 `admitted` 置 `outcome_unknown`）-> `Store.BeginRun` -> `r.load` -> `Store.ListAnsweredExecutions` -> `splitFilesSnapshotPostRef` -> `classifyExecutionSummary` -> `aggregateGroupMutation` -> `deriveRepairContext` | **RC34-F02-T7：** 每条 `ExecutionID` 独立持有自身 `;cm=*` 零串线；工况 ① 与 ② 整组为 **`MutationProven`**（推进 `MinRequiredSeq` 至 `b_last`）；工况 ③ 与 ④ 因组内 `Unknown` 一票否决整组恒为 **`MutationUnknown`**（`MinRequiredSeq` 原地不动！）；<br>**RC34-F02-T8：** `Reconcile` 后持久化 `OriginToolInterrupted` + `outcome='outcome_unknown'`（嵌套崩溃中外层 `OriginToolInterrupted` 否决内层 `;cm=proven`），`Resume` 100% 判为 **`MutationUnknown`**，`MinRequiredSeq` 绝不推进，重用 `step` 号零冲突；<br>**RC34-F02-T9：** `CommitToolResult` 后崩溃重启与在线态的 `MutationEvidence`（`Proven / Unknown / None`）及 `MinRequiredSeq` **100% 逐位恒等**（特别是场景 B 跨 `Resume` 依然保持 `MutationUnknown` 绝不退化为 `Proven`！无 `;cm=` 历史旧行安全归入 `MutationUnknown`）；<br>**RC34-F02-T10：** `MinRequiredSeq` 沿 `[Seq(G1), Seq(G1), Seq(Block2), Seq(G4), -1(u2重置)]` 在 `u2` 前严格单调不减（`G3` 伪写入绝不在 `Block2` 前移下界！），且每节点在线值与冷 `Resume` 值 100% 相等 | T7 不同工具调用回执串线或部分 `Proven` 绕过同组 `Unknown` 否决；T8 写盘后未提交崩溃被判为 `Proven` 或 `None`；T9 `Resume` 后场景 B 退化为 `MutationProven` 或单工具写库突破 `2` 次门禁；T10 在 `G3/Block2` 错误推进或发生非单调回退 | T7–T10 崩溃前后 SQLite（`session_messages, derived_origins.post_ref`）完整转储、写库次数计数断言（`=2`）、在线 vs 冷 `Resume` 的 `mutations` 与 `MinRequiredSeq` 逐点对照表 |
| **T5** | `36 KiB` 工作区 `AGENTS.md`（含 ```` ```bash ````、`~~~py` 及未闭合围栏内 `# Fake Heading` + 围栏外 6 个真实标题）与 `24 KiB` 无标题 `AGENTS.md` | `compactProjectInstructions` -> `PolicyReadOnly` + `ModeCompact` -> `read_file(path="AGENTS.md", start_line=1, end_line=200, anchors=true)` | 围栏内伪标题零误收录；指引含 `anchors=true`；`read_file` 返回行号与大纲 100% 一致；读 `<globalDir>/AGENTS.md` 被沙箱拦截；原文件 SHA256 不变 | 漏传 `anchors=true`、伪标题混入、行号错位或原文件哈希改变 | 紧凑大纲黄金文本、文件 SHA256 与 `read_file` 起止行断言 |
| **T6** | **子集 A：** 8 类中英日/代码/Emoji/稠密数组语料；**子集 B：** `8 KiB` 全 `\n`、全 `"`、全 `\`、全 `<>&`、全控制字符 `\x00..\x1f` 及混杂非法 UTF-8 `0xff` 送入 `presentFeedback`；**子集 C：** 遍历 `n ∈ {-5, 0, 1..25, 58, 1200, 1456, 2458, 6144, 6400, 12346, 36922}` | `presentFeedback` -> `json.Marshal(blocked)` -> `window.Estimate`、`ReserveTokensForBytes`、`BaseGateSlack` -> `CapacityRetry` | **B：** 输出恒为合法 UTF-8，`len(json.Marshal(blocked)) <= 2,458B / 12,346B`，`window.Estimate(blocked) <= 1,438 / 7,206` tokens（`BaseGateSlack == 1,950 / 7,718`）；**C：** `1456->854`、`6400->3738`、`2458->1438`、`12346->7206` 全区间单调不减 | 任一转义语料估算突破预留、`1456` 算错、非法 UTF-8 残留或 `GuardTokens` 虚胖 | 序列化字节与估算 Token 对照表、`ReserveTokensForBytes` 黄金值断言 |
| **T7** | 开启思考回显 + `BeforeModelCall` 暂停钩子；Step 1 产出带非空 `Echo` 的工具调用 | Step 2 `BeforeModelCall` 暂停注入 `OriginSteer` -> `applyPendingInputs` -> 重跑 7 步 `projectCandidate` -> `AdmitRequest` | `req.Messages` 含 `Steer` 行，`req.Echo == nil`，`RequestEnvelope.LastSeq` 精确等于 `Steer` 行 `seq` | 残留旧 `Echo` 下标错位，或信封漏记 `Steer` 行 | `ProviderRequest` 与 `request_envelopes` 查询结果 |
| **T8** | Step 1 由 `m-200k`（含 `2K` `EchoTokens`）产出 `[assistant + 14K-token tool_result]`；Step 2 `m-200k` 返回 503，候选链 `[m-200k, m-16k, m-128k]` | `CandidatesByStaticPrefix` -> `Loop.send`：`m-200k` 503 -> `m-16k` Dry-Run（`TurnBoundary=true`，克隆清零 `EchoTokens`）-> `ErrOverflow` -> `m-128k` 提交 `OriginTurn` 发送 | `m-16k` Dry-Run 返回 `window.ErrOverflow`、置 `exhausted[1]=true` 且数据库 `OriginTurn` 写入数为 `0`；`m-128k` 写入唯一 `1` 条 `OriginTurn` 并完整携带 `14K` 工具组发送 | `m-16k` 被预筛剔除、未清零旧 `EchoTokens`、写库 `>1` 条 `OriginTurn` 或丢弃 `14K` 工具组 | `FailoverDecision` 链、SQLite `origin='turn'` 计数与请求消息断言 |
| **T9** | **子集 A（MCP 6 类代码围栏对照）：** 20 个内置工具 + 10 个 `exec` MCP + 20 个只读 `mcp:ops`（6 类围栏对照）；**子集 B（REV-F06 P3 显式 `ProfileCompact` 内置防呆）：** 对 `read_file, write_file, replace_in_file, edit_lines, bash(background=true), eval` 执行 `ProfileCompact` 呈现 | 调用 `presentStepTools(tools, "", ModeCompact)` 与 `presentStepTools(tools, config.ProfileCompact, ModeCompact)` | **A：** 规范/无标题围栏 ①–⑤ 100% 保留，仅 ⑥ 替换为 `[example omitted]`，`(MCP server: ops)` 100% 保留；**B（P3 实施验收）：** 显式 `ProfileCompact` 完整保留 6 个内置关键工具防呆与分页前提 | 误删规范围栏、破坏 `(MCP server: ops)`，或 `ProfileCompact` 截丢内置防呆前提 | 6 类围栏与 6 个内置工具压缩前后逐字断言 |
| **T10** | **REV-F02 & RC33-F02 真实 Agent Tool Loop 全生命周期与并发快照测试：** 跨 3 个全局根分布 `240` 个已启用全局技能（含 50 条 `1024B` 高转义描述、1 条单条超 `16 KiB` 桩；另测单根 `135 > 128` 截断告警）+ `5` 个项目技能 + `3` 个 `Disabled` + `2` 个 `Shadowed`；在真实 `Engine.RunAgent` -> `Loop.Run`（经 `validateToolArgs -> Tools.Execute("read_skill", ...)`）中依次执行全部 **10 项强制操作**：① Run 启动建立 `SkillSnapshot`；② Run Step 2 期间外部调用 `Registry.Save` 新增 `skill-new` 并覆写 `skill-001`；③ 外部调用 `Registry.Delete` 物理删除 `skill-002`；④ 外部调用 `Registry.SetEnabled` 禁用 `skill-003` 并启用原 `Disabled` 的 `skill-disabled-1`；⑤–⑥ 在同一 Run 内连续全量分页遍历 **10 次**并核对顺序；⑦ 在同一 Run 内调用 `read_skill({"id":"skill-001"})`、`read_skill({"id":"skill-002"})`、`read_skill({"id":"skill-new"})`、`read_skill({"id":"skill-disabled-1"})`；⑧ Run 中断后执行 **`Resume` 启动新 Run** 再次查询目录与读取；⑨ 在 `Trusted=false` 下尝试按 `id` 绕过读取 `Withheld` 项目技能及互斥非法参数测试；⑩ 开启 `-race` 由 8 个后台 Goroutine 并发循环调用 `Registry.{Save, Delete, SetEnabled, List, Prompt}` 同时运行 Agent Loop 工具调用 | `Engine.prepareRun` -> `assemblePrompt` -> `newSkillReader(prompt.Skills.Eligible)` -> `Loop.Run` 真实工具循环（`validateToolArgs` -> `readSkill`）-> `Resume` 二次 `prepareRun`（全程 `go test -race`） | ①–⑦ **同一 Run 内 100% 快照隔离一致**：10 次全量分页顺序逐项恒等、每页 `<= 16,384B` 且 `1..50` 条（单条超限桩 `DescriptionTruncated: true`），`Prompt`、`ListSkills` 与 `ReadSkill` 看到完全相同的初始 `Eligible` 快照（中途新增的 `skill-new` 与新启用的 `skill-disabled-1` 在当前 Run 中不可见不可读；中途磁盘被删的 `skill-002` 与被改写的 `skill-001` 仍无损返回快照初始 `SKILL.md` 正文！）；⑧ **`Resume` 新 Run 精确更新**：新快照包含 `skill-new`、`skill-disabled-1` 与新版 `skill-001`，排除已删 `skill-002` 与已禁 `skill-003`；⑨ `Withheld / Disabled / Shadowed` 0% 绕过泄漏，互斥参数 100% 报错拦截；⑩ `go test -race` **零数据竞争、零 `.tmp` 文件冲突** | Run 中途新增/启用的技能泄漏进当前 Run、中途磁盘删除导致当前 Run `read_skill(id)` 报 `ENOENT`、`Prompt/ListSkills/ReadSkill` 三者不一致、`Resume` 未更新快照、`Withheld` 被 `id` 绕过，或 `-race` 检出竞争 | Run 1（变更前/变更后）与 `Resume` Run 2 的 `(Prompt, ListSkills×10, ReadSkill)` 对照矩阵、`-race` 通过日志 |
| **T11** | **子集 A：** `128K` `m-compact`（`ProfileCompact`, `MaxSteps: 10, Budget: $1`）503 切至 `128K` `m-detailed`（`ProfileDetailed`, `MaxSteps: 50, Budget: $20`）；**子集 B：** `64K` `m-detailed` vs `64K` `m-auto`（`fullFixed > usableFull/2` 但装得下当步消息） | `runtime.planModels` -> `prepareRun` -> 7 步 `projectCandidate` -> `Loop.send` Failover | **A：** `m-compact` 用 `ModeCompact (candidate_profile)`，`m-detailed` 独立用 `ModeFull`，Run 级 `MaxSteps == 10 / Budget == $1` 恒定不变；**B：** `m-detailed` 保持 `ModeFull`，`m-auto` 触发 `ModeCompact (fixed_prefix_budget)` | 轻载 `ProfileCompact` 失效、`ProfileDetailed` 被误压、或 Failover 篡改 Run 级治理上限 | `RuleWindowPrefix` 原因码与退出记录 |
| **T12** | **子集 A（REV-F03 + REV-F07 + RC33-F03 离线防截断双射）：** 常规名、含空格/`%`/`~` 名、极限 `10` 片全 4 字节 Emoji/中文模型名（`64B + 1 + 192B = 257B` UTF-8 -> `769B` `%XX` -> `model~0~ .. model~9.~`），以及篡改测试（缺失、混用、乱序、断号、重复、**删去末片 `model~9.~` 仅留 `model~0~..model~8~` 企图部分解码**、中间片伪造 `.~`），清空内存配置表离线调用 `DecodeEnvelopeModelRef`；<br>**子集 B（RC33-F03 精确 `128` vs `129` 极限槽位与 `MaxRefsJSONBytes` 验证）：** 固定构造 **`10` 个 `ModelRef` 分片** + **`7` 个固定系统/投影/版本引用**（`proj:mode:compact` + `sys:prompt` + `3` 个 `SentLayers` + `sys:instructions` + `ts:v1` = **`17` 个高优固定引用**），分别搭配：<br>  - **工况 B1（`111` 个实渲 `TaskState` 引用：`1` `brief` + `1` `workflow` + `64` `RefIncluded` + `35` 开放 `WorkUnits` + `10` `Tasks`，总计精确 `10 + 7 + 111 = 128` 条引用）**；<br>  - **工况 B2（`112` 个实渲 `TaskState` 引用：开放 `WorkUnits` 增至 `36` 个，总计精确 `10 + 7 + 112 = 129` 条引用）**；<br>  - **工况 B3（字节极限验证：构造 `128` 条每条满长 `96B` 的合法 `refRE` 引用写入 SQLite，实测 `len(refs_json)` 字节数与 `MaxRefsJSONBytes = 16384` 边界；另验证 `50` 个已完成 `WorkUnits` + `90` 条旧 `Tasks` + `10240B` 截断外条目 0% 混入）**；<br>**子集 C（REV-F06 P3 `ReplayDecision` 与撤回）：** 重放 `RuleWindowPrefix`、`CompactFixedByModel` 与 `v1` 步骤，并发 `RevokeSession` | `EncodeEnvelopeModelRef` -> `AdmitRequest` -> 读取真实 SQLite `request_envelopes` 行（`refs_json, length(refs_json), lineage_complete`）-> 离线无配置 `DecodeEnvelopeModelRef` -> `VerifyStep` | **A：** 合法模型 100% 离线逐字符无损还原；**删去末片 `model~9.~` 的残缺分片 100% 被拦截报 `ErrIncompleteModelRefChunks`，零部分解码**；<br>**B1（`128` 条）：** SQLite 实存精确 `128` 条，`Refs[0:10]` 模型分片、`Refs[10:17]` 系统固定引用、`Refs[17:128]` 全部 `111` 条 `TaskState` 引用 100% 完整保留，`lineage_complete == 1`（`true`）；<br>**B2（`129` 条）：** SQLite 实存精确 `128` 条，**`Refs[0:10]` 全部 `10` 个模型分片与 `Refs[10:17]` 全部 `7` 个系统引用 100% 完好无损、顺序未变**（离线解码 `ModelRef` 100% 成功），仅第 `112` 条 `TaskState` 引用被尾部截断，且 SQLite `lineage_complete == 0`（`false`，诚实降级）；<br>**B3：** `128 × 96B` 实测 `length(refs_json) == 12,673 <= 16,384` 成功入库，未渲染 `TaskState` 项 0% 混入；<br>**C：** `VerifyStep` 与撤回 100% 通过 | `129` 条时截断了任何模型分片或系统引用、残缺模型分片被部分解码、`128` 条误置 `lineage_complete=0`、`129` 条伪造 `lineage_complete=1`、或 `128×96B` 超 `MaxRefsJSONBytes` | SQLite `request_envelopes` 实存 `refs_json` 切片快照、`length(refs_json) == 12673` 断言、`B1(128)/B2(129)` `lineage_complete` 对照表 |

## 七、Self-Criticism 十二问终极反向挑战、架构边界与 `ADR-0024` 准入条件

### 7.1 Self-Criticism 十二问逐项反向挑战与实证回答（严格对齐 `v0.4-rc3.5`）

1. **`tierFiles` 前后 Git Blob 差异（`PreRef != PostRef`）是否仍被单独当作 `MutationProven`？**
   - **彻底废除、全篇清零。** 本版已在 §2（否决项 10）、§3.10、§4.2 与 §4.6.3–§4.6.5 严格规定：锁外快照 `PreRef != PostRef`（及 `DerivedEdge` 文件边）只能证明 `[t_pre, t_post]` 时间窗内文件内容存在净差异，绝不能单独证明作者是当前 Agent；唯独同时满足「`CommitReceipt` 证实 `Engine.Commit` 实际落盘 `Wrote == true` + `postFiles` 写后磁盘原始 `SHA256 == NewSHA256` 夹逼通过 + 前后 Git Blob 净变化 `PreBlob != PostBlob`」三重联合条件并封签 `;cm=proven` 时，才判为 `MutationProven`。
2. **宿主先将 `A` 改为 `B`、Agent 随后调用 `write_file(B)` 跳过实际写盘（场景 B，`Wrote == false`）时，是否还会被误判为 `Proven` 或 `None`？**
   - **100% 不会。** 即使当前会话此前未读过该路径或刚经 `Resume` 清空了内存 `editing.SnapshotStore` 使 `RejectStaleRead` 放行，`Engine.Commit` 在 `string(p.next) == string(p.original)` 分支返回 `Wrote = false`；`postFiles` 检测到 `!rf.Wrote && preFile.Blob != postFile.Blob`，强制签发 `;cm=unknown`（`MutationUnknown`）——既防止把宿主改动冒充 Agent 修复进展推进 `MinRequiredSeq`，又在首次门禁阻断前锁住 `preBlockFloor` 防止漏判 `None`。
3. **宿主先将 `A` 改为 `B`、Agent 再写入 `C`（场景 C）时，各子分支因果归属是否严密？**
   - **三分支全部严密闭环：** ① 若 `C != B` 且 `Blob(C) != Blob(A)` 且写后磁盘夹逼 `SHA256 == NewSHA256(C)`（C1），Agent 确实持锁覆写了磁盘并产出了异于步前的净新内容，判为 `;cm=proven`；② 若 `C == A` 改回步前原内容（C2，`PreBlob == PostBlob`），相对步前零净修复进展但发生过写盘，判为 `;cm=unknown`；③ 若 `Commit` 解锁后在 `postFiles` 前被宿主迅速覆写为 `D != C`（C3），写后夹逼 `SHA256(D) != NewSHA256(C)` 捕获并发篡改，判为 `;cm=unknown`。
4. **是否把 `edit.committed` 运行时事件当作了持久化或崩溃恢复依据？**
   - **绝对没有。** 已通过 `runtime/events.go:255-260` 源码查明 `"edit.committed"` 的 `durableEvent == false`（不写 `journal` 表、无 `ExecutionID`、慢消费者非阻塞丢弃），并在 §2 否决项 10 明令禁止将其用作证据或恢复源。
5. **工具校验失败、CAS/陈旧读拒绝、`f.Truncate(0)` 后写失败或 `apply_patch` 多文件部分提交（场景 E / F）时，是否会假设「失败等于零副作用」？**
   - **绝不假设失败等于零副作用。** 场景 E（`CodeStale / CodeConflict`，哪怕 `postFiles` 因宿主改盘生成了 `DerivedEdge`）与场景 F 全部失败调用 100% 签发 `;cm=unknown`（`MutationUnknown`）；且针对进入磁盘突变窗口后的失败（`writeCAS` 在 `f.Truncate(0)` 后写失败/`CodeConcurrentWrite`，或 `apply_patch` 在已发布前缀后失败；历史版本中为进入 `rollback`），`CommitReceipt.DirtyFailure = true` 同步将 `ToolResult.MutatesWorkspace` 置 `true` 并将 `post.Outcome` 升格为 `OutcomeUnknown`。
6. **同一 `answered_seq` 下多工具调用（含对同一文件连续调用及 `eval / apply_experience` 嵌套调用，场景 G）是否会发生证据串线或局部覆盖？**
   - **100% 物理隔离且保守聚合。** 每次 `admitExecution` / `admitCallbackExecution` 生成唯一 `ExecutionID` 后，在当次局部 `toolCtx` 上绑定独占 `commitRecorder` 闭包（嵌套回调创建新子闭包遮蔽外层 Context 键，工具参数与输出文本无法伪造），每条 `derived_origins` 行独立保存各自的 `;cm=*`；聚合函数 `aggregateGroupMutation` 严格按偏序格 `Unknown > Proven > None` 归并，同文件 `[A->B (;cm=proven), B->B (;cm=none)]` 精确得 `Proven`，而只要同组任一调用为 `Unknown` 即一票否决整组为 `MutationUnknown`。
7. **崩溃与冷启动 `Resume`（场景 H）后是否会出现运行期与恢复期结论分叉？旧版无 `;cm=` 记录如何兼容？**
   - **100% 逐位一致，向后安全降级。** ① `Engine.Commit` 落盘后、`CommitToolResult` 前崩溃（H1/H3）由 `Store.Reconcile` 原子写入 `OriginToolInterrupted` 与 `outcome = 'outcome_unknown'`，恢复期恒判 `MutationUnknown`；② `CommitToolResult` 提交后崩溃（H2）因 `;cm=proven|none|unknown` 已在同一 SQLite 事务中写入 `derived_origins.post_ref`，即使 `Resume` 清空内存 `SnapshotStore`，`ListAnsweredExecutions` + `classifyExecutionSummary` 仍无状态还原出逐位相同的 `MutationEvidence`；③ 历史无 `;cm=` 后缀的旧行由 `splitFilesSnapshotPostRef` 返回 `seal == ""` 保守归入 `MutationUnknown`。
8. **在 `derived_origins.post_ref` 追加 `;cm=*` 是否违反零 Schema 迁移、破坏现有查询约束、引入第二套文件账本或增加写库次数？**
   - **四项全部零违规：** ① 零新增表、零新增列、零 SQLite Migration、零第二套文件修改账本；② 字符串形如 `"files:1:sha256:<64hex>;cm=proven"`（最长 `<= 94B << 512B`），100% 通过 `schema_v6` `length(post_ref) <= 512` 与 `executions.go:210` `checkLooseRef` 校验，且完全兼容 `strings.HasPrefix(post, "files:1:sha256:")`（仓库内无任何 SQL 将 `post_ref` 作外键等值连接）；③ 随既有 `CommitToolResult / SettleExecution` 单事务落盘，严守 `agent/execution_test.go:346` 每次副作用工具精确 **2 次**写库门禁。
9. **在 `.gitattributes` clean 过滤器、Git-ignored 文件、符号链接、超 `4 MiB` 大文件与删除文件下，原始 SHA-256 夹逼与 Git Blob 是否存在冲突？**
   - **完全正交无冲突。** `CommitReceipt` 比较的是 `Engine.Commit` 写入磁盘的原始字节 `NewSHA256` 与 `postFiles` 在 `SnapshotFiles` 前后读取的原始磁盘字节 SHA-256（证明磁盘文件未被外部篡改），而净内容变化比较的是同由 `git hash-object` 算出的 `preFile.Blob` 与 `postFile.Blob`（即便配置了 CRLF/ident clean filter 亦自洽！）；Git-ignored `<= 4 MiB` 常规文件正常哈希，删除文件校验 `os.ErrNotExist && postFile.Blob == "-"`，符号链接、目录与 `> 4 MiB` 文件因 `Complete == false` 统一归入 `MutationUnknown`。
10. **将 `tierWorkspace`、`mcp__*` 与并发竞争场景全部归为 `MutationUnknown` 是否会导致窗口死锁或频繁 `Failover`？**
    - **不会。** 常态非修复期（`!repair.Active`）`MinRequiredSeq == -1` 对滑窗零影响；门禁修复期内，绝大多数正常 Agent 代码修复通过内置 `write_file / replace_in_file / apply_patch / edit_lines` 无并发冲突落盘获得 `;cm=proven`，立即将 `FloorIdx` 推进至最新 `OriginCompletionBlocked` 释放历史组；仅在纯 `bash` 盲试且累积超小模型容量时按设计切往大窗口候选模型。
11. **全篇是否还有残留的旧版规则矛盾或隐藏的 P0 / P1 / P2 缺陷？**
    - **零矛盾、零残留阻塞缺陷。** 已全篇同步 §0–§7 规则、数据结构、Go 映射（将 `agent/execution.go` 移出零改动行并补入 `editing/engine.go` 与 `toolset`）、18 类真值表与 10 项强制因果测试。
12. **当前 `v0.4-rc3.5` 是否真正满足 `ADR-0024` 准入标准？**
    - **完全满足。** 最后一个已知 P1 缺陷 `RC34-F02` 与全部历史 P0/P1/P2 审查项均已在规格层严密闭合（`SPEC_RESOLVED`），可直接呈交 Owner 审批后进入 `ADR-0024`。

### 7.2 尚未实施项与已知架构边界清单（诚实区分四态）

1. **生产代码与自动化测试尚未实施（状态：`IMPLEMENTATION_PENDING`）：**
   - 本次修订严格只更新 `context-space-architecture-master.zh-CN.md`，`wt/core-cognitive-v1-integration-20261116` 与 `LumenCortex-Core` 生产代码、测试及数据库零修改；
   - 第 `5.1` 节全部 12 组源码改动（含 `RC34-F02` 的 `editing.CommitReceipt`、`postFiles` 三重联合核验与 `;cm=*` 原子封签，以及 **`REV-F06` P3：显式 `ProfileCompact` 下内置关键工具 `read_file / write_file / replace_in_file / edit_lines / bash / eval` 防呆语义保留，与 `ReplayDecision` 对 `RuleWindowPrefix` 和 `CompactFixedByModel` 的重放分支**）和第 `六` 节 `T1–T12` 确定性测试（含 `RC34-F02` 全部 10 项强制因果并发与崩溃恢复测试）均处于 `IMPLEMENTATION_PENDING` 状态，待 Owner 批准后由 Astra 编码实施并跑通全仓 `go test -race -count=1 ./...`。
2. **外部闭源系统与非隔离宿主环境物理边界（状态：`UNVERIFIED` / 物理边界）：**
   - **闭源 Tokenizer 与真实缓存命中率（`UNVERIFIED`）：** 本地字符分类估算器不捆绑闭源词表，极端分词偏差由 `GuardTokens (~6.25%) + CapacityRetry + Failover` 闭环兜底；`protocol.Usage` 缓存统计字段（`V03-F07`）留作后续独立 P3 演进；
   - **非隔离工作树同路径先改再覆写的因果边界（场景 C1）：** 在共享工作树中，若宿主先将 `A` 改为 `B`，而当前会话未曾读取该路径随后调用 `write_file(C)`（`C != B` 且 `C != A`）实际覆写磁盘且写后夹逼核验通过 `SHA256 == NewSHA256(C)`，由于该目标文件的最终字节内容确由当前 Agent 持锁写入并相对步前 `A` 产生了净变化，归为 `MutationProven` 符合因果作者定义；其余一切宿主改动残留（场景 B No-op、场景 C2 改回 `A`、场景 C3 写后被宿主再改 `D`、场景 E CAS 拒绝）均 100% 归入 `MutationUnknown`；
   - **技能包内附属参考文件磁盘读取边界：** `SKILL.md` 主指令正文 100% 驻留 Run 内存快照，而技能子目录中的可选附属文件（如 `references/*`）按渐进披露原则保留受白名单约束的按需磁盘读取。
3. **刻意保持的设计硬边界（非缺陷）：**
   - **只有同时证明当前 Agent 实际落盘写入且产生净文件内容变化才判 `MutationProven`：** 锁外快照差异绝不单独构成 `Proven`；证据不足或存在并发/部分失败缺口时一律保留 `MutationUnknown`，绝不错误推进 `MinRequiredSeq`；
   - **`WorkspaceSnapshot` 绝不用作全工作区无变更证明：** 全部内置 `tierWorkspace`（`bash, edit_go, lsp_rename, eval_artifact` 及 `debug_*`）与全部已准入外部 `mcp__*` 恒判 `MutationUnknown`；
   - **变更证据粒度为 `answered_seq` 原子组级**（零正文 `HistoryGroupMeta` 与 `window.Groups` 最小裁剪单位严格同构，组内 `Unknown` 一票否决）；
   - **严格 Failover 优先 + 受控 Floor Relaxation + 终极溢出边界：** 小窗口候选严禁越级放宽修复下界；仅在全部更大候选均耗尽/不可用时，最大可用候选方可在完整保留门禁反馈与最新原子组并注入工作区重检通知、持久化 `RuleWindowFloorRelaxed` 决策的前提下放宽下界，若连门禁反馈或最新原子组也无法容纳则受控报 `KindWindowOverflow` 退出；
   - **`<globalDir>/AGENTS.md` 零二次截断且严守工作区外沙箱**；
   - **不支持离线 `Full Projection Replay`**（严禁落盘模型私有 Thinking `Echo`）。

### 7.3 `ADR-0024` 审查闭环状态（`v0.4-rc3.7`）

- **`v0.4-rc3.7` 规格升级核查（在完整保留 `v0.4-rc3.5` 与 `v0.4-rc3.6` 历史闭环结论基础上）：**
  - `R2-01`（P2，创建型写入同目录私有临时文件 + 平台原子 no-replace 发布 + 严格内容/身份/时间戳所有权防误删外部原地写入数据）：`SPEC_RESOLVED`；
  - `R2-02`（P2，门禁拒绝后固定 Gate State + 因果进展台账 + `R1->R2->R3` 三级滚动修复历史，覆盖 5 档模型窗口与真实 SQLite 冷 `Resume` 全状态恒等）：`SPEC_RESOLVED`；
  - `R2-03`（P3，工作区重检提示通知与编辑 CAS / 完成门禁重验的职责边界披露，无运行时工具硬拦截）：`OPEN (P3)`；
  - `R2-04`（P3，前向结合律 `baseSM` 增量折叠消除超 `2048` 组 `O(N^2)` SQLite 流式重扫）：`SPEC_RESOLVED`；
  - `R2-05`（P3，报告真实函数/测试符号名、包计数与持久化原始验证日志归档对齐）：`SPEC_RESOLVED`；
  - `F-02, F-04, F-06(8K兼容), F-09(源码级)`：`CONFIRMED`（Opus 5.5 R2 独立确认）；
  - `F-05, F-06(小窗口及各档位容量权衡), F-07, F-08`（P3）：诚实保留于 Finding Ledger，绝不删改或自封 `CLOSED`。
- **当前文档最终状态：** `ARCHITECTURE_RC3_7_R2_REMEDIATION_SPEC_COMPLETE`
