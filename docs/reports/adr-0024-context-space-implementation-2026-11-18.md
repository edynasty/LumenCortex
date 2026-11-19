# ADR-0024 Context Space Architecture — 完整实施、审查修复与最终交付报告

- **初次实施日期：** 2026-11-18 · **Opus 5.5 审查修复完成日期：** 2026-11-19
- **权威架构规格：** `LumenCortex-architecture-audit/design/handoffs/context-space-architecture-master.zh-CN.md`（当前版本 **`v0.4-rc3.6`**，SHA256 `3da2b65adda4b43fddf929dfa0d6b345e9181a75bf43c43f1c79ed5abe0ab09a`；原 `v0.4-rc3.5` 原文备份于 `context-space-architecture-master.v0.4-rc3.5.zh-CN.md`，SHA256 `bd8dfb00c9781d4d375ce4779789a71f0cbef9f547918ea1f4aa1a0c7481abd9`）
- **冻结源码基线：** Core `c73d17359f5d9dd16ef88b0de4525862593fddaa`（实测一致）；Opus 5.5 审查输入基线 `b57655cbb517bf7344f7670e13f9d2db82a2ba47`
- **隔离工作树与实施分支：** `/Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-context-space-20261118`（分支 `feat/adr-0024-context-space-20261118`，已推送 `origin/feat/adr-0024-context-space-20261118`，未合并 `main`）
- **最终代码候选提交：** `a6ac592f4fe904255be92e6367076d076d073e6b`（初次完成提交 `38e4fe0` → 自主审计 `b57655c` → 本轮 `v0.4-rc3.6` 5 个原子修复提交 `97c1a60..a6ac592`）
- **最终交付状态：** `AUDIT_AND_REMEDIATION_COMPLETE_PENDING_INDEPENDENT_REVIEW`

> **诚实证据分层声明（严格区分实现状态、自我验证状态、独立复审状态与未验证平台，绝不自授独立 PASS）：**
> - **`IMPLEMENTED` / `FIXED_BY_IMPLEMENTER`：** 规格 `v0.4-rc3.6` 全部 11 项核心能力、中英文 `ADR-0024`（`v0.4-rc3.6` 1:1 同步）及 Opus 5.5 独立审查指派修复的 `F-01`（P1）、`F-02`（P2）、`F-03`（P2）、`F-04`（P3）、`F-09`（P3）与 `F-06` `8K` 模型兼容性恢复已在纯 Go 源码中完整落地；零新增包、零 SQLite Schema Migration（冻结于 `0001`–`0017`）、零改写 `agent_steps` 表结构、零修改 Cognitive Core。
> - **`TESTED` / `SELF_VERIFIED`：** 离线确定性验收矩阵 `T1–T12`（含按 `v0.4-rc3.6` 修订的 `T3-A2`）、Opus 5.5 全部并发与长程探针固化回归用例（`F-01` 真实并发文件替换与超限防误删、`F-02` 真实 SQLite `>2048` 组流式冷恢复与在线下界逐位一致、`F-03` Failover 优先守卫 / 最大可用候选受控 Floor Relaxation / 双门禁拒绝 / 冷 `Resume`、`F-04` `debug_*` 真实工作区落盘、`F-09` Windows 反斜杠路径排序匹配）及恢复回原始 `8000` 窗口的 `runtime/editing_test.go` 与 `runtime/session_import_test.go` 全部在 `-race` 下实测通过。
> - **`INTEGRATION_VERIFIED`（平台覆盖与未验证边界）：**
>   - **macOS ARM64（`darwin/arm64`, Go `1.26.5`, Node `v24.20.0`）：** 全仓 48 个包（43 个含测试包）`go build ./...`、`go vet ./...`、`go test -count=1 ./...` 与 `go test -race -count=1 ./...` **100% 零失败、零数据竞争原生通过**。
>   - **Ubuntu Linux ARM64（`linux/arm64`, Go `1.27.1`, `lc-ubuntu-go:24.04` 非 root `uid=1000` 只读源码挂载）：** `go build ./...`、`go vet ./...`、43 个非 `evalruntime`/`runtime` 包单测及 16 个核心/修复包 + `./runtime` Context Space / 8K / Prompt / Skill `-race` 套件全部通过；容器内缺 `systemd` 委托 cgroup / Python 内核 / `curl` 的宿主环境项显式标记为容器环境未执行（已在 macOS ARM64 原生验证）。
>   - **Windows AMD64（`windows/amd64`）：** `GOOS=windows GOARCH=amd64 go build ./...` 与 `GOOS=windows GOARCH=amd64 go vet ./...` 交叉编译及静态检查零错误通过；**因当前环境无原生 Windows 主机，Windows 原生进程运行、Windows 文件系统 `ByHandleFileInformation` 运行时身份比对、SQLite 锁与 `-race` 明确标记为 `UNVERIFIED`（绝不以交叉编译冒充 Windows 原生运行）**。
> - **`PENDING_INDEPENDENT_REVIEW`：** 初次候选 `38e4fe0` 曾由只读子代理 `oma-reviewer`（`9d0e94b7`）审查，随后经 Opus 5.5 在 `b57655c` 上执行独立对抗性审查并检出 `F-01`–`F-09`（`1 P1 + 2 P2 + 6 P3`）。本轮对 `F-01 / F-02 / F-03 / F-04 / F-09` 及 `F-06 (8K 兼容)` 的全部修复均处于 **`FIXED_BY_IMPLEMENTER | SELF_VERIFIED | PENDING_INDEPENDENT_REVIEW`** 状态，且 `F-05 / F-06(小窗容量权衡) / F-07 / F-08` 共 4 项 `P3` 显式保留在 Finding Ledger 中标记为 **`OPEN (P3)`**，等待 Opus 5.5 独立增量复审。

---

## 1. Evidence Boundary 实施前与收尾基线核验

| 核查项 | 预期契约 | 实测证据 | 状态 |
| :--- | :--- | :--- | :---: |
| 权威架构文档版本与 SHA256 | `v0.4-rc3.6`（由 `v0.4-rc3.5` 升级） | `v0.4-rc3.6` SHA256 = `3da2b65adda4b43fddf929dfa0d6b345e9181a75bf43c43f1c79ed5abe0ab09a`；`v0.4-rc3.5` 原文备份 SHA256 = `bd8dfb00c9781d4d375ce4779789a71f0cbef9f547918ea1f4aa1a0c7481abd9` | **PASS** |
| Core 主工作树保护 | `HEAD == c73d17359f5d9dd16ef88b0de4525862593fddaa` 且保留未提交 `M AGENTS.md` | `LumenCortex-Core` 保持 `main` @ `c73d17359f5d9dd16ef88b0de4525862593fddaa`，`M AGENTS.md` 原封未动；全部开发隔离于 `wt/core-adr-0024-context-space-20261118` | **PASS** |
| SQLite Schema 与 Migration | 零新增 Migration（冻结于 `0001`–`0017`） | `git diff main..HEAD -- internal/migrate/` 为空；`internal/migrate` 全量升降级与回滚测试通过 | **PASS** |
| Cognitive Core 治理边界 | 零修改事实/信念/隐私/Governor 语义（`ADR-0023`） | `git diff main..HEAD -- internal/cognition/ internal/knowledge/ internal/verify/` 为空；全部认知与隐私回归测试通过 | **PASS** |
| 工具单次写库不变量 | 每次有副作用工具调用严格 `2` 次 SQLite 写事务 | `AdmitExecution`（1）+ `CommitToolResult`/`SettleExecution`（1，`;cm=*` 原子内嵌于 `derived_origins.post_ref <= 92B`），`TestT4` 实测断言通过 | **PASS** |
| Desktop 公共接口兼容性 | 不破坏 `config / docs / protocol / provider / runtime` 导出签名 | 仅新增向后兼容导出常量/类型（`config.{MaxProviderIDBytes, MaxModelKeyBytes}`、`skills.{SnapshottedSkill, MaxCompactCatalogBytes}`），零导出破坏性变更 | **PASS** |

---

## 2. 唯一责任模块接线矩阵（杜绝双账本、双预算、双 Transcript）

| 核心能力 | 唯一权威责任模块与符号 | 严禁重复边界 |
| :--- | :--- | :--- |
| Run 级治理锁定与不可变 `SkillSnapshot` | `runtime/agent.go` (`prepareRun`), `runtime/prompt.go` (`assemblePrompt`), `internal/skills/registry.go` (`Registry.Prompt`), `runtime/skills.go` (`newSkillReader`) | 整场 Run 唯一一次扫描磁盘建立 `Eligible []SnapshottedSkill`（含 `SKILL.md` 内存正文），`Prompt / ListSkills / ReadSkill` 共享同一只读快照 |
| 项目指令围栏感知大纲 | `runtime/prompt.go` (`compactProjectInstructions`, `extractFenceAwareHeadings`) | 仅作用于受信工作区 `> 12 KiB` 项目指令；`<globalDir>/AGENTS.md` 零二次截断，不放宽 `toolset/path.go` 沙箱 |
| `TaskState` 渲染与实渲引用同步 | `internal/taskstate/render.go` (`RenderBlock`, `RenderedBlock`) | 单一 `MaxBlockBytes = 10240` 截断偏移 `cutoff` 同时决定渲染文本与 `ItemRefs`（`startByteOffset < cutoff`），零幽灵引用 |
| 保守字节 Token 估算与上界预留 | `internal/window/estimate.go` (`EstimateBytes`, `ReserveTokensForBytes`, `Estimate`) + `provider/{anthropic,openai}/wire.go` | 全系统唯一估算原语；`EchoTokens` 仅在 `GroupTokens` 计费一次，遇 `user` 行即清零，绝不持久化私有思考原文 |
| 候选静态预筛、闭式预算与受控 Floor Relaxation 策略 | `internal/agent/policy.go` (`CandidatesByStaticPrefix`, `GuardTokens`, `BaseGateSlack`, `EffectiveGateSlack`, `WindowPrefixDecision`, `FloorFallbackDecision`, `WindowFloorRelaxedDecision`, `ReplayDecision`, `VerifyStep`) | 全系统唯一候选预筛、比例余量（`min(baseSlack, free/4)`）、二元前缀选择与 Failover 优先受控 Floor Relaxation 审计公式；`VerifyStep` 100% 离线重放 |
| 工具与门禁反馈受约束呈现 | `internal/agent/stepspecs.go` (`IncludedStepTools`), `internal/agent/presentation.go` (`presentStepTools`, `presentFeedback`) | 每次投影分配独立新切片；`presentFeedback` 强制 `1200B/6144B` UTF-8 与 `2458B/12346B` JSON 序列化双重封顶 |
| 独占创建保护、编辑落盘回执与规范化键 `;cm=*` 封签 | `internal/editing/{engine,fileid_unix,fileid_windows}.go` (`O_CREATE\|O_EXCL`, `capturedFileID`, `CommitReceipt`), `internal/toolset/editing.go`, `internal/agent/{execution,callback}.go` (`executionTierOf`, `postFiles`, `sealFilesMutation`) | 唯真正由本次调用独占创建且文件身份（`dev+ino` / Windows `VolumeSerial+FileIndex`）匹配的路径才允许回滚删除（`F-01`）；`debug_*` 归 `tierWorkspace -> MutationUnknown`（`F-04`）；`sealFilesMutation` 按 `filepath.ToSlash` 键匹配且不折大小写（`F-09`） |
| 零正文历史元数据、SQLite 流式恢复与统一修复下界 | `internal/session/executions.go` (`ListAnsweredExecutions`), `internal/agent/transcript.go` (`HistoryGroupMeta`, `DeriveRepairContextFromHistory`, `r.deriveRepairContext`, `r.groups`) | 内存仅保留无正文 `HistoryGroupMeta`（上限 `MaxInMemoryHistoryGroups = 2048`）及 `maxContext` 窗口内正文组；超出 `2048` 组自动经 `Store.Transcript` 流式扫描 SQLite；在线运行期与冷 `Resume` 共享同一 `repairStateMachine`（`F-02`） |
| 7 步无环候选投影、`TurnBoundary` 试算、Failover 优先守卫与 `BeforeModelCall` 闭环 | `internal/agent/send.go` (`compactToolPrefForCandidate`, `projectCandidate`, `projectCandidateForSend`, `appendFloorRelaxationNotice`, `Loop.send`) | 无跨步 `compact` 锁存；小模型绝不抢先触发 Floor Relaxation，唯全部更大候选耗尽后在最大可用候选上保留最新原子组 + 活跃门禁反馈并注入显式工作区重检通知（`F-03`） |
| 带 `.~` 终结符无损模型分片信封 `v2` | `config/schema.go` (`64B/192B`), `internal/agent/envelope.go` (`EncodeEnvelopeModelRef`, `DecodeEnvelopeModelRef`, `requestRefs`), `runtime/{auto,verification}.go` | `projectionVersion = 2`；`model~0~..model~9.~` 最多 10 片（`P0` 最高优），缺 `.~` 终结符立即报 `ErrIncompleteModelRefChunks` 拒绝残片解码 |

---

## 3. 原子实施与审查修复 Commit 清单

| 阶段 | Commit SHA | 提交摘要 | 核心交付物 |
| :--- | :---: | :--- | :--- |
| **Phase A** | `0c9f8cd6f89492d3bf23c4c140e95c8aa2cefa0a` | `docs(adr): add ADR-0024 context space projection and budget guards` | 中英文 `ADR-0024`（14 节 1:1 对齐）、ADR 索引更新、基线核验与接线报告 |
| **Phase B · Stage 1** | `55c684161d38d0cd57170b8fc8865add7626fa30` | `feat(context): add token byte guards, immutable skill snapshot, and project outline (ADR-0024 stage 1)` | `EstimateBytes`/`ReserveTokensForBytes`、Wire `Echo` 对齐、`taskstate.RenderBlock`、`skills.Registry` 读写锁 + `os.CreateTemp` + `SnapshottedSkill` + 双模 `read_skill`、围栏感知项目大纲、`T5 / T6A / T6C / T10` |
| **Phase B · Stage 2** | `4df9e7e9fe4afdbfa14f5fb04d8fe6ef61963188` | `feat(context): add model-ref byte bounds, envelope v2 chunking, and compact presentation rules (ADR-0024 stage 2)` | `64B/192B` 配置与 Schema 校验、候选级 `ProviderPkg`/`Presentation`、`IncludedStepTools`、`presentStepTools`、`presentFeedback` 双守卫、`policy.go` 预算与 `ReplayDecision`、`.~` 终结符信封 `v2`、`T6B / T9 / T12` |
| **Phase B · Stage 3** | `565727245473ed91573945adeebf365eada8c8b6` | `feat(context): wire causal commit receipts, repair floor, and 7-step candidate projection (ADR-0024 stage 3)` | `editing.CommitReceipt`、`postFiles` 三重夹逼与 `;cm=*` 封签、`ListAnsweredExecutions`、`DeriveRepairContext`、7 步 `projectCandidate`、`TurnBoundary` 试算、`BeforeModelCall` 闭环、`T1 / T2 / T3 / T4 / T7 / T8 / T11` |
| **Phase C · 初次闭合** | `38e4fe0f1ab71a177d07817e813ed60b5e100693` | `fix(context): close Phase C review findings, contiguous skill offset, and 8K slack clamp (ADR-0024)` | 闭合 `C-F01`、`QGR-01`、`QGR-02`、`QGR-03`、`QGR-04`（注：其中 `C-F02` 对 `8000` 测试夹具的改动已被 Opus 5.5 识别为掩盖 `F-06`，现已在本轮 `a6ac592` 恢复并根治） |
| **自主审计修复** | `dc4c082` · `cedf9a2` · `bfda8d5` · `b57655c` | `fix(context): require >=3 fence chars` / `refactor(agent): drop dead code` / `style: gofmt` / `docs(reports)` | 修复 `AUD-A02`（围栏 `>=3` 字符）、`AUD-A01`（清理死代码）、`AUD-A03`（`gofmt`）并记录审计台账 |
| **Remediation · Commit 1** | `97c1a60ecb58655b9107234d4c853606d804fa6f` | `docs(adr): synchronize ADR-0024 with Context Space v0.4-rc3.6 remediation spec` | 将主架构文档升级至 `v0.4-rc3.6`（备份 `v0.4-rc3.5`）并同步中英文 `ADR-0024`（不变量 9/10、`F-01..F-04`、`F-06`、`F-09`、`T3-A2`） |
| **Remediation · Commit 2** | `16651ed76204bfc1ad913c9172c342542f9a1ae2` | `fix(editing): enforce O_EXCL and OS file identity checks on create writes (F-01)` | `O_CREATE\|O_EXCL` + Unix (`Dev+Ino`) / Windows (`VolumeSerial+FileIndex`) 文件身份双重验证，固化并发创建/替换/超限防误删测试 |
| **Remediation · Commit 3** | `8cf5de265e75fcfdcbec8a17a55c4e02712c389f` | `fix(agent): classify debug_* as MutationUnknown and match file seals by slash path key (F-04, F-09)` | `debug_*` 纳入 `tierWorkspace -> MutationUnknown` + 真实工作区写文件测试；`sealFilesMutation` 按 `filepath.ToSlash` 规范化键配对且不折大小写 |
| **Remediation · Commit 4** | `394e0b5fd96f8ddf8cebee24c6489a858930a56a` | `fix(agent): unify live and Resume repair floors via body-free HistoryGroupMeta and SQLite streaming (F-02)` | 无正文 `HistoryGroupMeta`（上限 `2048` 组）+ 超限 SQLite `Transcript` 流式扫描 + 永久消息 Token 有界 `r.transcript`，消除在线/冷恢复下界漂移 |
| **Remediation · Commit 5** | `a6ac592f4fe904255be92e6367076d076d073e6b` | `fix(agent): add failover-first guarded floor relaxation and restore 8K model compatibility (F-03, F-06)` | Failover 优先受控 Floor Relaxation（`RuleFailoverFloorFallback` + `RuleWindowFloorRelaxed` + 工作区重检通知 + `ReplayDecision`/`VerifyStep`）+ `8K` 内置紧凑回退与 `25%` 比例余量，恢复 `8000` 窗口既有测试 |

---

## 4. 历史实施与子代理审查台账（含 `C-F02` 错误表述更正）

| ID | 级别 | 发现者 / Owner | 涉及文件与行号 | 触发条件与实际影响 | 根因、修复与更正说明 | 回归证据（RED -> GREEN） | 修复 Commit | 状态 |
| :--- | :---: | :--- | :--- | :--- | :--- | :--- | :---: | :---: |
| **C-F01** | P2 | Primary Implementer | `internal/agent/send.go`, `internal/agent/policy.go` | `8K` 小窗口候选通过静态预筛但剩余空间小于 `BaseGateSlack_compact (1950)` 时，常态固定扣减 `1950` 导致 Step 1 假性报 `ErrOverflow` | 初版钳位吃掉了 `newestGroup` 之外 100% 历史空间；现已随 `F-06` 升级为 `EffectiveGateSlack = min(baseSlack, free/4)`，保留 75% 近期历史容量 | `TestProjectCandidate8KModelClampsCompactSlackWithoutFalseOverflow` + `TestAgentCeilingsCapTheGrant` (`-race` PASS) | `38e4fe0` → `a6ac592` | `FIXED_BY_IMPLEMENTER` / `SELF_VERIFIED` / `PENDING_INDEPENDENT_REVIEW` |
| **C-F02（已更正）** | P3 | Primary Implementer → Opus 5.5 纠偏（见 `F-06`） | `runtime/editing_test.go:31`, `runtime/session_import_test.go:112`, `runtime/prompt_test.go:42` | **更正原报告错误表述：** 原报告将 `editing_test.go` 与 `session_import_test.go` 在 `8000` 窗口下的失败错误归结为“预期内的紧凑投影或历史裁剪”并上调测试 `context` 至 `32768`，实质上掩盖了 `8K` 模型在内置全量工具集与固定 `1950` 余量下的真实兼容性退化（`F-06`） | **已在本轮 `a6ac592` 彻底纠正：** `runtime/editing_test.go` 与 `runtime/session_import_test.go` 已恢复回原始 `useGlobalConfig(t, modelConfig(""))`（`8000` tokens），并通过生产代码修复（内置紧凑工具回退 + `min(baseSlack, free/4)` 比例余量）原生通过；仅 `runtime/prompt_test.go` 因专门校验 `ModeFull` Canonical Prompt 报告一致性（`fullFixed <= usableFull/2` 数学前提需 `>=32768`）保留 `32768` | `TestAgentLoopEditsLinesAndRejectsAStaleEdit`（`8000` PASS）、`TestImportedOMPSessionContinues`（`8000` 保留全部 11 行 PASS）、`TestEffectivePromptMatchesARealRun` PASS | `a6ac592` | `FIXED_BY_IMPLEMENTER` / `SELF_VERIFIED` / `PENDING_INDEPENDENT_REVIEW` |
| **QGR-01** | P2 | `oma-reviewer` (`9d0e94b7`) | `internal/agent/send.go` | 主大候选 `503` 暂态故障切至小候选后在 `BeforeModelCall` 注入大 `InboxEdit/Steer` 溢出时漏走可用性恢复并可能写入孤立 `OriginTurn` | 统一 `handleOverflow` 闭包复用容量切换与 `recoverFrom`；`rebuiltInputs && due` 先在 `cloneForTurnPreview()` 试算通过后再落盘 `OriginTurn` | `TestQGR01_BeforeModelCallOverflowRecoversAndNeverOrphansOriginTurn` (`-race` PASS) | `38e4fe0` | `SELF_VERIFIED`（子代理已复核，待外部统一签核） |
| **QGR-02** | P2 | `oma-reviewer` (`9d0e94b7`) | `internal/skills/registry.go` | 长描述技能超限省略后继续追加后续短技能导致 `Included` 非连续前缀，且 `Text` 未扣前言/页脚可超 `32 KiB` | 扣除前言/页脚预留保证 `len(Text) <= 32768B`；在首个超限技能处停止内联以保证 `Eligible[0:offset]` 连续 | `TestQGR02_PromptCatalogContiguousOffsetAndMaxBytes` (`-race` PASS) | `38e4fe0` | `SELF_VERIFIED`（子代理已复核，待外部统一签核） |
| **QGR-03** | P3 | `oma-reviewer` (`9d0e94b7`) | `internal/agent/transcript.go` | 外部导入会话（`OriginImported`）历史工具调用组无 `tool_executions` 行，原聚合返回 `MutationNone` 而非 `MutationUnknown` | 检测 `OriginImported` 工具组强制返回 `MutationUnknown` | `TestQGR03_OriginImportedToolGroupClassifiesAsMutationUnknown` (`-race` PASS) | `38e4fe0` | `SELF_VERIFIED`（子代理已复核，待外部统一签核） |
| **QGR-04** | P3 | `oma-reviewer` (`9d0e94b7`) | `docs/adr/0024*.md`, `runtime/context_space_t4_test.go` | ADR 条款同步与测试未用变量清理 | 同步中英文 ADR 并移除未用变量通过 `go vet ./...` | `go vet ./...` Exit 0 | `38e4fe0` | `SELF_VERIFIED` |

---

## 5. 离线确定性验收矩阵（`T1–T12`）实测证据总表

全部 `T1–T12` 均在真实生产调用链与真实本地 SQLite / Git 工作树上执行，零付费外部网络调用，且全部通过 `go test -race -count=1`：

| 编号 | 对应测试函数与源码位置 | 真实执行调用链与核心验证断言 | `-race` 结果 |
| :---: | :--- | :--- | :---: |
| **T1** | `internal/agent/context_space_stage3_test.go` — `TestT1_BidirectionalCandidateProjectionAndFailover` | `Loop.Run` 双向测试 `[m-32k, m-200k]` 与 `[m-200k, m-32k]`（40 个 `mcp:ext` 1KiB 示例块工具 + `40 KiB` `AGENTS.md` + 60 技能）：两候选均过静态预筛，`m-32k` 发 `ModeCompact (fixed_prefix_budget)` 保留 `(MCP server: ext)`，`m-200k` 发 100% 原始 `ModeFull (fits_full)`，`VerifyStep` 全量重放通过 | **PASS** |
| **T2** | `internal/agent/context_space_stage3_test.go` — `TestT2_DemandDrivenMonotonicSingleStepCompactRecovery` | `m-64k` 连续三步 `Loop.Run`：Step 2 返回 `22K`-token 工具大输出按需切 `ModeCompact (newest_group_fit)`，Step 3 普通 `300`-token 输出立即恢复 `ModeFull (fits_full)`，零跨步锁存 | **PASS** |
| **T3** | `internal/agent/context_space_stage3_test.go` — `TestT3A_*`（已升级至 `v0.4-rc3.6`）；`runtime/context_space_t3_test.go` — `TestT3B_CausalAuthorshipConcurrencyAndIgnoredFilesMatrix` | **A1：** `14K` 修复保留区在 `m-32k` 溢出自动升级 `m-200k`（小模型不降级）；**A2a（`v0.4-rc3.6`）：** 阻断前 `80K` `bash` 组在 `[m-32k, m-64k]` 上先切换至最大候选 `m-64k`，再进入受控 Floor Relaxation（保留 `BlockSeq` + 最新原子组、注入工作区重检通知、记录 `window.floor_relaxed`）并完成；**A2b：** 阻断后 `80K` 最新原子组连最大候选也装不下时受控报 `StatusFailed / KindWindowOverflow`。<br>**B（`RC34-F02` T1–T6 + `RC34-F01` 7–15）：** 真实 Git 仓库 + `.gitignore` + `.gitattributes` clean filter + 真实 MCP Server 全矩阵断言通过 | **PASS** |
| **T4** | `runtime/context_space_t4_test.go` — `TestT4_MultiCallVetoCrashReconcileResumeAndMonotonicChain` | **T7：** 同 `answered_seq` 多工具调用与嵌套 `apply_experience` 逐 `ExecutionID` 回执零串线，组内任一 `Unknown` 一票否决整组为 `MutationUnknown`；**T8：** 落盘后、`CommitToolResult` 前模拟崩溃重启，`Reconcile` 补 `OriginToolInterrupted + outcome_unknown`，`Resume` 判 `MutationUnknown`；**T9：** 冷启动 `Resume` 与在线态 `MutationEvidence` / `MinRequiredSeq` 逐位恒等（单有副作用调用严格 `2` 次写事务）；**T10：** 5 节点连续多轮门禁阻断单调链在线与冷恢复 100% 一致 | **PASS** |
| **T5** | `runtime/context_space_stage1_test.go` — `TestT5_ProjectInstructionsOutlineAndAnchoredRead` | `36 KiB` 含代码围栏伪标题 `AGENTS.md` + `24 KiB` 无标题 `AGENTS.md`：伪标题 0 混入，附 `anchors=true` 指引，`PolicyReadOnly` 下 `read_file` 行号精确匹配，`<globalDir>/AGENTS.md` 越界读取被沙箱拦截，原文件 SHA256 不变 | **PASS** |
| **T6** | `internal/window/estimate_test.go` (`T6A/T6C`) + `internal/agent/context_space_stage2_test.go` — `TestT6B_PresentFeedbackDualGuardsAndBaseGateSlack` | 8 类中英日/代码/Emoji/稠密 JSON 语料 + `ReserveTokensForBytes` 全断点黄金值单调不减 + 6 类 `8 KiB` 极端转义/控制符/非法 UTF-8 输入经 `presentFeedback` 后 `json.Marshal(blocked) <= 2458B / 12346B`，`BaseGateSlack == 1950 / 7718` | **PASS** |
| **T7** | `internal/agent/context_space_stage3_test.go` — `TestT7_*` + `TestQGR01_*` | `BeforeModelCall` 暂停期间注入 `OriginSteer` 与 `InboxEdit`：自动重跑投影，清空过时 `req.Echo == nil`，`RequestEnvelope.LastSeq` 精确更新至 `Steer` 行 `seq`；暂态故障下小候选重算溢出时零孤立 `OriginTurn` 写库并正确恢复回主候选 | **PASS** |
| **T8** | `internal/agent/context_space_stage3_test.go` — `TestT8_TurnBoundaryDryRunZeroDBWritesAndMinimumProgressGuard` | Step 1 `m-200k` 产出 `14K` 工具组 + `2K` `EchoTokens`；Step 2 `[m-200k(503), m-16k, m-128k]`：`m-16k` 克隆清零 `EchoTokens` 试算触发最小进展守卫溢出且数据库 `OriginTurn` 写入数为 `0`；`m-128k` 写入唯一 `1` 条 `OriginTurn` 并完整携带 `14K` 工具结果 | **PASS** |
| **T9** | `internal/agent/context_space_stage2_test.go` — `TestT9_MCPCodeFencesAndExplicitProfileCompactSafety` | 6 类 MCP 代码围栏对照（仅正证纯示例块折叠为 `[example omitted]`，100% 保留规范围栏与 `(MCP server: ops)`）+ `REV-F06` P3 显式 `ProfileCompact` 下 6 个内置工具防呆与分页前提逐项保留 | **PASS** |
| **T10** | `runtime/context_space_stage1_test.go` — `TestT10_SkillSnapshotLifecyclePaginationAndConcurrency` + `internal/skills/snapshot_race_test.go` | 跨 3 全局根 + 1 项目根 `245` 个技能 + 8 并发写 Goroutine：同一 Run 内 10 次全量分页顺序恒等（`<=50` 条、`<=16384B`）、中途磁盘删改 `SKILL.md` 仍无损返回初始快照正文、`Resume` 新 Run 精确刷新快照、连续前缀 `offset` 零跳项、`-race` 零竞争零残留 `.tmp` | **PASS** |
| **T11** | `runtime/context_space_t3_test.go` — `TestT11_CandidateProfileIsolationAndGovernanceInvariants` | `[m-compact, m-detailed]` 故障切换各自独立使用 `ModeCompact (candidate_profile)` 与 `ModeFull`，而 Run 级 `MaxSteps == 10` 与 `Budget == $1` 恒定不变；`64K` 下 `m-detailed` 保 `ModeFull`、`m-auto` 按 `fixed_prefix_budget` 切 `ModeCompact` | **PASS** |
| **T12** | `config/model_ref_bounds_test.go`, `internal/taskstate/render_block_test.go`, `internal/agent/context_space_stage2_test.go` — `TestT12_*` | `64B/192B` UTF-8 边界校验；`257B` 全 4 字节 Emoji 模型名 10 片双射与删末片 `.~` 防部分解码；`10 + 7 + 111 = 128` 槽位 `lineage_complete = 1` vs `129` 槽位仅裁 `P4` 尾部；`10240B` 截断外 `TaskState` 引用 0% 混入；`ReplayDecision` `v1`/`v2`（含 `floor_relaxed` 与 `floor_fallback`）全分支兼容通过 | **PASS** |

---

## 6. 最终候选（`a6ac592`）跨平台与全模块回归验证记录

| 目标平台与环境 | 执行命令 | 实测结果（基于最终代码候选 `a6ac592`） | 覆盖与未验证边界说明 |
| :--- | :--- | :--- | :--- |
| **macOS ARM64**（原生宿主 `darwin/arm64`, Go `1.26.5`, CGO SQLite, Node `v24.20.0`） | `go build ./...`<br>`go vet ./...`<br>`go test -timeout 35m -count=1 ./...`<br>`go test -race -timeout 60m -count=1 ./...` | **全部 Exit 0（43 个含测试包 100% `ok`，0 FAIL，0 DATA RACE）**<br>单测耗时：`evalruntime` `106.9s`、`internal/agent` `7.6s`、`internal/editing` `1.6s`、`internal/session` `40.4s`、`internal/toolset` `23.8s`、`runtime` `665.2s`<br>`-race` 耗时：`evalruntime` `108.4s`、`internal/agent` `15.6s`、`internal/editing` `3.9s`、`internal/session` `42.0s`、`internal/toolset` `27.1s`、`internal/window` `23.7s`、`runtime` `962.4s` | **100% 全仓原生覆盖**（含全部 `F-01..F-04`/`F-06`/`F-09` 新增回归、真实 SQLite 冷恢复、Cognitive Core、Harness、TaskState、Session、Governor、Toolset、Migration 0001–0017、EvalRuntime） |
| **Ubuntu Linux ARM64**（Docker `lc-ubuntu-go:24.04`, `linux/arm64`, Go `1.27.1`, GCC `13.3.0`, `--init --user 1000:1000`, 只读源码挂载, `GOPROXY=off`） | `go build ./...`<br>`go vet ./...`<br>`go test -count=1`（43 个非 `evalruntime`/`runtime` 包）<br>`go test -race -count=1`（`config`, `provider/...`, `internal/{window,editing,skills,taskstate,toolset,session,agent,cognition,migrate}` + `./runtime` Context Space / 8K / Prompt / Skill / Fence） | **全部 Exit 0（0 FAIL，0 DATA RACE）**<br>`internal/agent` `-race` `15.5s`、`internal/editing` `-race` `3.5s`、`internal/toolset` `-race` `13.3s`、`internal/session` `-race` `40.1s`、`internal/window` `-race` `28.1s`、`internal/migrate` `-race` `12.8s`、`runtime` `-race` `39.4s` | 原生验证 Linux ARM64 编译、CGO SQLite、`O_EXCL` + `dev/ino` 并发创建防误删、`>2048` 组 SQLite 流式恢复、Floor Relaxation 与 `-race`；容器内未启动完整 `systemd` PID 1 委托 cgroup、未装 Python 内核与缺 `curl` 的宿主测试项显式标记为容器环境未执行（已在 macOS ARM64 原生验证） |
| **Windows AMD64**（交叉编译 `windows/amd64`） | `GOOS=windows GOARCH=amd64 go build ./...`<br>`GOOS=windows GOARCH=amd64 go vet ./...` | **全部 Exit 0（零编译错误、零静态检查告警，含 `internal/editing/fileid_windows.go`）** | **显式限制声明：** 仅完成交叉编译与 `go vet` 静态检查（验证 `syscall.GetFileInformationByHandle` / `ByHandleFileInformation` 类型与调用签名合法）；**未在原生 Windows 主机上执行运行时测试、文件身份替换实机验证或 `-race`，故 Windows 运行时明确标记为 `UNVERIFIED`** |

---

## 7. Strong Self-Criticism 反向自检结论（含本轮修复 11 问）

1. **`O_EXCL` 是否仍有路径替换竞争？** 无。创建分支强制以 `os.O_WRONLY | os.O_CREATE | os.O_EXCL` 打开目标路径并立即在已打开 `*os.File` 句柄上采集 `capturedFileID`；若路径已存在（`os.ErrExist`）则 `createdNew=false`，零修改既有文件且零清理资格。回滚清理前再次打开/检查路径（Unix `Lstat` 不跟随符号链接；Windows `FILE_FLAG_BACKUP_SEMANTICS | FILE_FLAG_OPEN_REPARSE_POINT` + `GetFileInformationByHandle`）核验与创建句柄的卷/设备号及文件索引完全一致，若已被外部删除、替换或重建则直接放弃 `os.Remove`，剩余极窄 POSIX `Lstat -> unlink` 窗口亦在注释与文档中诚实记录。
2. **文件身份检查是否跨平台可靠？** Unix 采用 `(Stat_t.Dev, Stat_t.Ino)`；Windows 采用 `syscall.ByHandleFileInformation` 的 `(VolumeSerialNumber, FileIndexHigh, FileIndexLow)` 并带 `FILE_FLAG_OPEN_REPARSE_POINT` 防止穿过符号链接/重解析点。Windows 部分已完成 `GOOS=windows GOARCH=amd64 go build/vet` 验证，但因无 Windows 宿主，原生运行时语义明确列为 `UNVERIFIED` 待 Windows 环境复核。
3. **运行期和 `Resume` 是否真正使用同一历史证据？** 是。两者均调用同一纯状态机 `repairStateMachine`（`DeriveRepairContextFromHistory` / 流式 `r.deriveRepairContext`），以全历史 `session_messages` 元数据与 `ListAnsweredExecutions` 为唯一输入，彻底消除 `r.transcript` 因 `maxContext` 裁剪正文造成的在线下界漂移（由 `TestF02LiveAndColdSQLiteResumeRepairFloorParity` 在真实 SQLite 上实证）。
4. **元数据是否可能无限增长？** 否。`r.historyMeta` 严格不含任何消息正文、工具参数或输出字节，且硬上限为 `MaxInMemoryHistoryGroups = 2048`（约数百 KB）；当会话超过 `2048` 个原子组时（`r.historyOverflow == true`），`r.deriveRepairContext` 自动通过 `Store.Transcript` 游标从 SQLite 流式扫描计算下界而不驻留内存，同时 `r.transcript` 按永久消息 Token 上界裁剪（由 `TestF02HistoryMetaBoundedAndStreamsPast2048GroupsFromSQLite` 验证 `2105` 组时 `len(r.historyMeta) <= 2048` 且 `len(r.transcript) < 60`）。
5. **Floor Relaxation 是否会在小模型上过早发生？** 否。`tryEnterFloorRelaxation` 仅在：① `repair.Active == true`；② 当前及所有更大窗口候选均已尝试完毕（容量超限或不可用且 `down.ready` 为空）；③ 选定 `SelectMaxAvailableCandidate(cands, unavailableForRelax)`（最大可用候选）之后才允许触发。`TestF03FailoverFirstGuardAndAllScenarios/32k_fails_over_to_200k_without_floor_relaxation` 实证验证 `32K` 绝不在 `200K` 可用时触发降级。
6. **是否可能静默丢失门禁反馈？** 否。受控松弛下界硬锁定为 `relaxedFloorSeq = minPositiveSeq(repair.BlockSeq, newestGroupSeq, turnProgressSeq)`，强制保留当前活跃 `OriginCompletionBlocked` 门禁反馈与最新原子消息组；若连最大可用候选也无法容纳二者，立即受控返回 `StatusFailed / KindWindowOverflow`（0 次模型请求）。
7. **降级 `Decision` 是否能够完整重放？** 是。回退至最大可用候选记录 `DecisionFailover / RuleFailoverFloorFallback`，窗口下界松弛记录 `DecisionWindow / RuleWindowFloorRelaxed`（含 `originalMinRequiredSeq`、`relaxedFloorSeq`、`sentFirstSeq`、`reason` 与完整组 Token 明细），全部接入 `ReplayDecision` 与 `VerifyStep`，所有 `TestF03*` 用例均通过全步骤 `VerifyStep` 校验。
8. **Agent 是否明确知道上下文已经不完整？** 是。一旦某步触发 `FloorRelaxed`，`appendFloorRelaxationNotice` 立即在当步投影的 `State` 消息末尾注入 `[Context Window Notice — Floor Relaxation]` 显式通知，写明被省略的序列区间 `[originalMinRequiredSeq, sentFirstSeq)` 并强制要求 Agent 在继续代码修改或宣称完成前用只读工具重新检查当前工作区状态。
9. **`debug_*` 工具是否仍可能伪装成 `MutationNone`？** 否。`executionTierOf` 已将全部 `debug_*`（`debug_start / debug_Attach / debug_eval / debug_continue / debug_step_* / debug_set_breakpoint / debug_disconnect` 等）纳入 `tierWorkspace`，无论被调试进程是否向工作区写入忽略文件或未跟踪产物，均无 `;cm=proven` 封签而确定性落入 `MutationUnknown`（由 `TestF04DebugToolsClassifiedAsMutationUnknown` 在真实工作区写入下验证）。
10. **Windows 路径配对是否存在误合并？** 否。`sealFilesMutation` 与 `editing.Engine.Commit` 统一使用 `canonicalReceiptPathKey`（`filepath.ToSlash` 规范化分隔符，去除前导 `./`），以 `map[string]FileCommitReceipt` 精确键查找替代切片下标配对；显式禁止 `strings.ToLower` 大小写折叠（防止大小写敏感文件系统上 `Foo.go` 与 `foo.go` 误合并），且重复路径立即拒绝返回 `"unknown"`（`TestF09SealFilesMutationCanonicalSlashKeyMatching`）。
11. **是否存在对 Cognitive Core 的意外影响？** 否。`git diff main..HEAD -- internal/cognition/ internal/knowledge/ internal/verify/ internal/migrate/` 完全为空，全仓认知与隐私回归 100% 通过。

---

## 8. 最终交付强制十四问逐项回答（按 `v0.4-rc3.6` 与真实状态更新）

1. **架构是否完整实现？** 是。权威规格 `v0.4-rc3.6` 全部能力已在纯 Go 生产代码中落地，保持双平面正交与 `{ModeFull, ModeCompact}` 二元投影（方案 F2），零第二套 Memory/Context Manager。
2. **七个认知相关领域是否正确协同？** 是。Memory、Attention、Reasoning、Learning、Belief Revision、Governance 与 Verification 在同一 `Engine/Store/Loop` 上协同运行。
3. **Context Space 是否真正影响 Agent 的后续行为？** 是。包括 `8K` 小窗紧凑工具/比例余量准入、项目大纲行号回读、技能连续分页、单步峰值自动恢复，以及长回合门禁拒绝后注入显式工作区重检通知引导 Agent 重新核实文件状态。
4. **经验和证据是否正确进入上下文？** 是。`taskstate.RenderBlock` 在 `10 KiB` 内同步渲染文本与 `ItemRefs`，零幽灵引用。
5. **修复期是否保留必要依据？** 是。优先在当前候选或 Failover 更大候选上严格保留 `MinRequiredSeq`；唯全部更大候选耗尽且原始下界超出最大可用候选时，才进入可重放的受控 Floor Relaxation（严格保留活跃门禁反馈 + 最新原子组 + 显式状态重检通知），绝不静默滑窗；下界由完整持久化历史确定性推导。
6. **Candidate Failover 是否正确？** 是。按候选独立重算投影与预算，严格执行 Failover 优先守卫（小模型不抢先松弛下界，`200K` 暂态 `503` 时精确回退至剩余最大可用 `32K` 而非 `16K`），`TurnBoundary` 试算零孤立写库。
7. **Resume 是否确定性？** 是。在线运行期与冷启动 `Resume` 共用同一套基于持久化历史（无正文 `HistoryGroupMeta` + 超 `2048` 组 SQLite 流式重建 + `ListAnsweredExecutions`）的 `repairStateMachine`，在真实 SQLite 上在线与冷恢复的 `MinRequiredSeq` 和 Floor Relaxation 行为逐位一致。
8. **Skills 是否安全且可发现？** 是。Run 级不可变快照、读写锁 + 原子写盘、`<=32 KiB / <=8 KiB` 连续前缀目录与 `<=50` 条 / `<=16 KiB` 双模 `read_skill`，隔离 `Disabled/Shadowed/Withheld`。
9. **RequestEnvelope 是否完整？** 是。`projectionVersion = 2` 支持最多 10 片 `.~` 终结符无损模型分片解码与 `10+7+111=128` 槽位优先级保护。
10. **Cognitive Core 是否完全保持原有治理边界？** 是。`internal/cognition`、`internal/knowledge`、`internal/verify`、`internal/migrate` 零改动。
11. **T1–T12 是否全部通过？** 是。`T1–T12`（含升级至 `v0.4-rc3.6` 的 `T3-A2`）及 `F-01/F-02/F-03/F-04/F-06/F-09` 新增回归套件均在 `-race` 下通过。
12. **所有平台验证覆盖到哪里？** macOS ARM64 全仓 43 个含测试包 `build/vet/test/race` 100% 原生通过；Ubuntu Linux ARM64 容器内 `build/vet`、43 包单测及 16 包 `-race` 通过（宿主 `systemd`/Python/`curl` 依赖项已说明）；Windows AMD64 仅完成 `build/vet` 交叉编译，原生运行显式标记为 `UNVERIFIED`。
13. **是否存在未解决 P0/P1/P2？是否存在未解决 P3？**
    - **未解决 P0 / P1 / P2：`0` 个**（Opus 5.5 检出的 `F-01` P1、`F-02` P2、`F-03` P2 已全部修复并通过自测，当前状态为 `FIXED_BY_IMPLEMENTER | SELF_VERIFIED | PENDING_INDEPENDENT_REVIEW`）。
    - **未解决 P3：`4` 个显式保留在 Finding Ledger（`OPEN (P3)`）**——即 `F-05`、`F-06`（小窗口 `GateSlack` 剩余容量权衡部分）、`F-07`、`F-08`（详见 §11.1，绝不谎称“0 个未解决 Finding”）。
14. **是否可以 Merge main？**
    - **当前不合并 `main`。** 代码与报告已提交并推送至独立分支 `feat/adr-0024-context-space-20261118`，状态为 `AUDIT_AND_REMEDIATION_COMPLETE_PENDING_INDEPENDENT_REVIEW`，等待 Opus 5.5 独立增量复审与 Owner 最终合并授权。

---

## 9. SHA256 关键交付物清单

| 文件路径 | 版本 / 说明 | SHA256 |
| :--- | :--- | :--- |
| `LumenCortex-architecture-audit/design/handoffs/context-space-architecture-master.zh-CN.md` | **`v0.4-rc3.6`（当前权威规格）** | `3da2b65adda4b43fddf929dfa0d6b345e9181a75bf43c43f1c79ed5abe0ab09a` |
| `LumenCortex-architecture-audit/design/handoffs/context-space-architecture-master.v0.4-rc3.5.zh-CN.md` | `v0.4-rc3.5`（升级前只读备份） | `bd8dfb00c9781d4d375ce4779789a71f0cbef9f547918ea1f4aa1a0c7481abd9` |
| `docs/adr/0024-candidate-context-projection-and-budget-guards.md` | 英文 ADR-0024（同步 `v0.4-rc3.6`） | `707b229c7f950b24523848ebadda9beb983e8bfec380ba716cd2ceb252f02fc5` |
| `docs/adr/0024-candidate-context-projection-and-budget-guards.zh-CN.md` | 中文 ADR-0024（同步 `v0.4-rc3.6`） | `f4a3648d5113889b226d8f2582e0a814619519b91a315ac23817431aea341518` |
| `docs/lumencortex.schema.json` | 配置 JSON Schema（`64B/192B` 模型键边界） | `289856b69557229318803c3198b023f777e94e52e2cde7be0fecf84c1b654744` |

---

## 10. 历史自主审计记录（Autonomous Audit，2026-11-18，基线 `7e4c89d -> b57655c`）

- **历史记录保留说明：** 本节保留 `b57655c` 时点的自主审计台账（`AUD-A01..A03`）。注意原 §10.2 中关于“`apply_patch` 路径不会误判”的推演未覆盖 Windows 反斜杠排序顺序差异，已被 Opus 5.5 检出为 `F-09` 并在本轮 `8cf5de2` 修复。

| ID | 级别 | Owner | 文件 | 触发条件与影响 | 根因与最小修复 | 修复 Commit | 状态 |
| :--- | :---: | :--- | :--- | :--- | :--- | :---: | :---: |
| **AUD-A02** | P2 | 实施者自审 | `internal/agent/presentation.go`, `runtime/prompt.go` | 行首 1–2 个 `~`/`` ` `` 被误判为代码围栏开启 | 补齐 CommonMark `run >= 3` 判定并新增回归单测 | `dc4c082` | `FIXED_BY_IMPLEMENTER` \| `SELF_VERIFIED` \| `PENDING_INDEPENDENT_REVIEW` |
| **AUD-A01** | P3 | 实施者自审 | `internal/agent/{loop,transcript,presentation,send}.go` | 旧路径遗留未用函数 `systemMessages`、`(*run).budget`、`utf8Valid` | 移除未用死代码并迁移注释 | `cedf9a2` | `FIXED_BY_IMPLEMENTER` \| `SELF_VERIFIED` \| `PENDING_INDEPENDENT_REVIEW` |
| **AUD-A03** | P3 | 实施者自审 | 新增测试文件 | 部分新增测试未执行 `gofmt` | 格式化新增测试文件 | `bfda8d5` | `FIXED_BY_IMPLEMENTER` \| `SELF_VERIFIED` \| `PENDING_INDEPENDENT_REVIEW` |

---

## 11. Opus 5.5 独立对抗性审查修复与全量台账（`F-01`–`F-09`，2026-11-19）

- **审查来源：** Opus 5.5 对 `feat/adr-0024-context-space-20261118@b57655c` 的独立对抗性审查报告及探针集。
- **修复范围与生产 Diff 规模：**
  - 增量修复范围 `b57655c..a6ac592`：10 个生产 Go 文件（`+1,331 / -190` 行），总计 23 个文件（含新增测试与双语 ADR，`+3,597 / -266` 行）。
  - 相对 `main`（`c73d173..a6ac592`）总生产 Go Diff：34 个非测试 Go 文件（`+4,223 / -482` 行），0 个 SQLite Migration，0 个 Cognitive Core 文件改动。

### 11.1 Opus 5.5 Finding Ledger（`F-01` 至 `F-09` 完整入账）

| ID | 级别 | 责任人 | 涉及文件与符号 | 触发条件与实际影响 | 根因与实施方案（或未修保留说明） | RED → GREEN 可复现证据 | Commit | 当前状态 |
| :--- | :---: | :--- | :--- | :--- | :--- | :--- | :---: | :--- |
| **F-01** | **P1** | Remediation Owner | `internal/editing/{engine,fileid_unix,fileid_windows}.go`, `internal/toolset/editing.go` | `Engine.Commit` 创建型写操作（`!existed`）此前使用非独占 `O_CREATE\|O_TRUNC` 且在任意错误（CAS 冲突、`maxFileSize` 超限、写失败、`apply_patch` 多文件回滚）时对 `!existed` 路径无条件调用 `os.Remove`，并发外部进程/用户在 Stat 后率先创建或替换的新文件会被直接覆写或误删 | 1) 创建分支改用 `os.O_WRONLY\|os.O_CREATE\|os.O_EXCL` 独占创建，遇 `os.ErrExist` 立即转为 CAS 冲突错误且 `createdNew=false`；2) 创建成功后立即从已打开 `*os.File` 句柄读取 OS 文件身份（Unix `Dev+Ino`；Windows `ByHandleFileInformation` `VolumeSerialNumber+FileIndex`）；3) 新增 `removeIfSameCreatedFile`，清理/回滚前重新校验路径不跟随符号链接且文件身份与本次创建句柄一致，被外部替换/重建时绝不删除 | **RED：** `TestCreateRaceForeignFilePreserved`、`TestCreateRaceOversizedForeignFilePreserved`、`TestCreateCleanupNeverDeletesReplacedFile`、`TestWriteFileCreateRaceForeignFilePreserved`（`write_file` & `apply_patch`）均复现外部文件被删；<br>**GREEN：** 上述全部用例在 macOS/Linux `-race` 下 100% PASS，Windows `build/vet` PASS（Windows 原生运行 `UNVERIFIED`） | `16651ed` | `FIXED_BY_IMPLEMENTER` \| `SELF_VERIFIED` \| `PENDING_INDEPENDENT_REVIEW` |
| **F-02** | **P2** | Remediation Owner | `internal/agent/{transcript,loop,send}.go` | 在线长会话中 `r.groups(maxContext)` 裁剪了早于内存窗口的旧消息组，使在线 `r.deriveRepairContext` 仅能看到被裁剪后的残余组，而冷启动 `Resume` 从 SQLite 读取历史，导致在线与冷恢复算出不同的 `MinRequiredSeq`（实测下界从 `seq 1` 漂移到 `seq 33`） | 1) 引入不含任何消息正文的轻量 `HistoryGroupMeta`（仅存 `Seq, Role, Origin, HasToolCalls, AnsweredSeq, ToolCalls{ID,Name}`），内存上限 `MaxInMemoryHistoryGroups = 2048`；2) 抽取唯一确定性纯状态机 `repairStateMachine` 供在线与 `Resume` 共用；3) 当会话超过 `2048` 组（`historyOverflow=true`）时通过 `Store.Transcript`（SQLite `EachMessageFrom` 游标）+ `ListAnsweredExecutions` 流式重建下界，零正文常驻内存；4) `r.groups` 按永久消息 Token 有界裁剪 `r.transcript` | **RED：** 真实 SQLite（`session.Open`）端到端测试 `TestF02LiveAndColdSQLiteResumeRepairFloorParity` 复现 `live MinRequiredSeq = 33 != cold Resume MinRequiredSeq = 1`；<br>**GREEN：** 该测试 + `TestF02HistoryMetaBoundedAndStreamsPast2048GroupsFromSQLite`（真实 SQLite `2105` 组流式一致性与内存上界断言）+ 多工具调用/新用户轮次测试 `-race` PASS | `394e0b5` | `FIXED_BY_IMPLEMENTER` \| `SELF_VERIFIED` \| `PENDING_INDEPENDENT_REVIEW` |
| **F-03** | **P2** | Remediation Owner | `internal/agent/{policy,send}.go`, `docs/adr/0024*.md` | 长程自主回合（如 40 步 `bash` 输出累计 `>32K` tokens）在触发一次完成门禁拒绝后，旧不变量无条件锁死最早 `MutationUnknown` 组 `MinRequiredSeq`，当单模型或最大可用候选也装不下完整修复跨度时直接以 `KindWindowOverflow` 终止整场长任务 | 升级架构至 `v0.4-rc3.6` 实施**带 Failover 优先守卫的受控 Floor Relaxation（方案 B）**：1) 小模型严禁直接触发松弛，必须先尝试所有更大候选；2) 唯全部更大候选均已耗尽后，选出最大可用候选（`SelectMaxAvailableCandidate` / `RuleFailoverFloorFallback`）；3) 若该候选能容纳最新原子消息组 + 当前活跃 `OriginCompletionBlocked` 门禁反馈，则松弛至 `min(BlockSeq, NewestGroupSeq)`，持久化可重放决策 `RuleWindowFloorRelaxed`（含原始 `MinRequiredSeq`、实际 `SentFirstSeq`、候选模型与原因，接入 `ReplayDecision`/`VerifyStep`），并向 `State` 注入显式 `[Context Window Notice — Floor Relaxation]` 要求 Agent 修改代码前先重检工作区；4) 若连门禁反馈 + 最新原子组也装不下，受控报 `KindWindowOverflow` | **RED：** 固化 Opus 5.5 在线与真实 SQLite 冷恢复探针（40 步 `bash` + 门禁拒绝报 `required context floor seq 1 evicted`、`200K 503` 终止）；<br>**GREEN：** `TestF03LiveLongGatedTurnSurvivesViaFloorRelaxation`、`TestF03ColdResumeLongGatedTurnSurvivesOnRealSQLite`、`TestF03FailoverFirstGuardAndAllScenarios`（含 `32K->200K` 零松弛、`[16K,32K,200K(503)]->32K`、双连续门禁拒绝 + `MutationProven` 修复解除松弛、松弛后真实 SQLite 冷 `Resume`、极端超限受控失败）+ `VerifyStep` 全通过 | `97c1a60`, `a6ac592` | `FIXED_BY_IMPLEMENTER` \| `SELF_VERIFIED` \| `PENDING_INDEPENDENT_REVIEW` |
| **F-04** | **P3** | Remediation Owner | `internal/agent/{execution,callback,transcript}.go` | `executionTierOf` 原先未匹配 `debug_*` 工具（返回 `tierNone`），使具有启动真实子进程并修改工作区（含 `.gitignore` 构建/覆盖率产物）副作用能力的调试工具未写入 `tool_executions`，在 `DeriveRepairContext` 中被静默误判为 `MutationNone` | 在 `executionTierOf` 中将 `strings.HasPrefix(name, "debug_")` 统一归入 `tierWorkspace`（不赋予 `;cm=proven` 封签），使其在在线执行与 SQLite `Resume` 中确定性归类为 `MutationUnknown` | **RED：** `TestF04DebugToolsClassifiedAsMutationUnknown` 复现 `executionTierOf("debug_start") == 0` 且真实工作区写文件后 `derived_origins == 0`；<br>**GREEN：** 全部 9 个 `debug_*` 工具及真实工作区写入（跟踪文件 + `.gitignore` 忽略文件）在在线与重开恢复下均为 `MutationUnknown`（`-race` PASS） | `8cf5de2` | `FIXED_BY_IMPLEMENTER` \| `SELF_VERIFIED` \| `PENDING_INDEPENDENT_REVIEW` |
| **F-05** | **P3** | Open（待后续规划） | `runtime/prompt.go` (`EffectivePrompt`, `assemblePrompt`) | `Engine.EffectivePrompt` 始终返回 Canonical 未压缩（`ModeFull`）提示词层与全量工具目录报告，当实际发送阶段候选模型进入 `ModeCompact`（紧凑项目大纲、紧凑技能目录、紧凑工具描述或 Floor Relaxation 状态通知）时，宿主观测到的报告与实际线上面向该候选发送的内容不一致 | 按本轮边界保持 **`OPEN (P3)`**，不无故扩大范围重构宿主 Prompt 预览 API（与工作区 `PROMPT-001 / HARNESS-OPT-001` 同属宿主观测对齐议题）；触发条件：下一轮宿主 Prompt Inspector / 候选级预览接口演进时统一对齐 | 已完整入账保留，未标记修复 | — | **`OPEN (P3)`** |
| **F-06** | **P3** | Remediation Owner（8K 准入修复） / Open（小窗余量权衡） | `internal/agent/{policy,send}.go`, `runtime/{editing_test,session_import_test}.go` | 1) **8K 兼容性与最小前缀问题（已修复）：** `8000` 窗口模型在 `PolicyFull` 下因按原始详细内置工具描述做静态预筛及常态全额扣除 `1950` `GateSlack`，导致 `editing_test.go` 预筛直接拒收、`session_import_test.go` 11 条导入历史被裁至 4 条；<br>2) **小窗口容量权衡（保留 P3）：** 即便改为比例余量，在小窗口模型上预留门禁余量与保守字节估算仍会占用一部分可选历史预算 | 1) **已实施（`a6ac592`）：** `compactToolPrefForCandidate` 在候选 `detailed` 内置工具令静态最小前缀超窗而紧凑内置工具可容纳时按需回退紧凑内置描述；`EffectiveGateSlack` 在 `!repair.Active` 时采用 `min(baseSlack, free/4)`（`>=32K` 仍保留完整 `1950/7718`，`8K` 保留 75% 近期对话空间），并将 `editing_test.go` 与 `session_import_test.go` 恢复为原始 `8000` 窗口；<br>2) **显式保留 `OPEN (P3)`：** `GateSlack` 与 `7/12` 保守字节估算对小窗口模型可选历史容量的结构性挤占作为已知架构权衡保持 `OPEN (P3)` 跟踪 | **RED：** 恢复 `8000` 后 `editing_test.go` 报 `no model can take this request`、`session_import_test.go` 报 `4 rows, imported 11`；<br>**GREEN：** 两项原始 `8000` 测试及 `TestProjectCandidate8KModelClampsCompactSlackWithoutFalseOverflow` 在 `-race` 下 100% PASS | `a6ac592` | 8K 兼容恢复：`FIXED_BY_IMPLEMENTER` \| `SELF_VERIFIED` \| `PENDING_INDEPENDENT_REVIEW`；<br>小窗容量影响：**`OPEN (P3)`** |
| **F-07** | **P3** | Open（待后续规划） | `runtime/prompt.go` (`extractFenceAwareHeadings`), `internal/agent/presentation.go` (`foldVerifiedExampleFences`) | 项目指令大纲提取器与工具示例折叠器采用逐行前缀扫描，未完整解析 CommonMark 块容器（如引用块 `> ``` ` 或列表缩进内的围栏/标题）、HTML 注释/原始 HTML 块以及 Setext 标题（`===` / `---`） | 按本轮第三阶段要求正式入账保持 **`OPEN (P3)`**（仅影响 `ModeCompact` 下的大纲导航提示或示例折叠精度，不影响文件原始内容或权限边界，模型仍可通过 `read_file` 读取原文） | 已完整入账保留，未标记修复 | — | **`OPEN (P3)`** |
| **F-08** | **P3** | Open（待后续规划） | `internal/agent/presentation.go:337-343` (`presentFeedback`) | `presentFeedback` 在 `ProfileCompact` 下包含第三套独立的反引号围栏扫描器（`strings.HasPrefix(trimmed, "```")`），与 `parseFenceOpen` / `isFenceClose` 规则不一致（未识别波浪线 `~~~` 围栏、反引号信息串约束或截断后的悬挂围栏闭合） | 按本轮第三阶段要求正式入账保持 **`OPEN (P3)`**（`presentFeedback` 的 UTF-8 与 JSON 字节硬上限 `2458B / 12346B` 不受影响，待后续统一抽成单一围栏扫描原语） | 已完整入账保留，未标记修复 | — | **`OPEN (P3)`** |
| **F-09** | **P3** | Remediation Owner | `internal/agent/execution.go` (`sealFilesMutation`, `canonicalReceiptPathKey`), `internal/editing/engine.go` | Windows 上 `apply_patch` 按含反斜杠 `\` 的原生路径排序（`\` ASCII `92` 大于 `.` `46` 与 `A–Z`，而 `/` ASCII `47` 仅大于 `.` `46` 但小于字母，导致如 `a\b.go` 与 `a.txt` 在原生 `\` 排序与 `SnapshotFiles` `/` 排序下顺序相反），原 `sealFilesMutation` 依赖切片位置/长度假设会导致配对错位或误降级 | 新增确定性 `canonicalReceiptPathKey`：统一使用 `filepath.ToSlash` 归一化分隔符并去除冗余前导 `./`，**严禁大小写折叠**（防止将 `Foo.go` 与 `foo.go` 误合并）；`sealFilesMutation` 建立 `map[string]FileCommitReceipt` 按规范化键双向完备匹配，遇重复键立即保守返回 `"unknown"` | **RED：** `TestF09SealFilesMutationCanonicalSlashKeyMatching` 复现 Windows `\` 排序（`["a.txt", "a\\b.go"]` vs `["a/b.go", "a.txt"]`）下原实现漏判 `"proven"` 且重复路径未拦截；<br>**GREEN：** 反斜杠乱序配对、大小写区分（`Foo.go` vs `foo.go`）、重复键拦截全部 `-race` PASS，Windows `build/vet` PASS（Windows 原生运行 `UNVERIFIED`） | `8cf5de2` | `FIXED_BY_IMPLEMENTER` \| `SELF_VERIFIED` \| `PENDING_INDEPENDENT_REVIEW` |
