# ADR-0024 Context Space Architecture — 完整实施、两轮独立审查修复与最终交付报告

- **初次实施日期：** 2026-11-18 · **R1 修复完成日期：** 2026-11-19 · **R2 增量修复完成日期：** 2026-11-19
- **权威架构规格：** `LumenCortex-architecture-audit/design/handoffs/context-space-architecture-master.zh-CN.md`
  - 当前版本：**`v0.4-rc3.7`**（SHA256 `5c9edaeb6b917d7cf508677036f17ab1911dd6b5f1a78ee25d8e0082f6cbb89f`）
  - 升级前 `v0.4-rc3.6` 原文只读备份：`context-space-architecture-master.v0.4-rc3.6.zh-CN.md`（SHA256 `3da2b65adda4b43fddf929dfa0d6b345e9181a75bf43c43f1c79ed5abe0ab09a`）
  - 初版 `v0.4-rc3.5` 原文只读备份：`context-space-architecture-master.v0.4-rc3.5.zh-CN.md`（SHA256 `bd8dfb00c9781d4d375ce4779789a71f0cbef9f547918ea1f4aa1a0c7481abd9`）
- **冻结源码与审查基线：**
  - 原始 `main` 基线：`c73d17359f5d9dd16ef88b0de4525862593fddaa`（主工作树 `LumenCortex-Core` 原封未动，保留未提交 `M AGENTS.md`）
  - Opus 5.5 第一轮审查输入基线：`b57655cbb517bf7344f7670e13f9d2db82a2ba47`
  - Opus 5.5 第二轮增量审查输入基线：`feat/adr-0024-context-space-20261118@a8b478617fe15e8726d4009f94454b027b1a9ef9`（R1 工作树 `wt/core-adr-0024-context-space-20261118` 保持冻结未动）
- **本轮 R2 独立工作树与分支：** `/Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-r2-20261119`（分支 `fix/adr-0024-r2-remediation-20261119`，已推送 `origin/fix/adr-0024-r2-remediation-20261119`，未合并 `main`）
- **本轮最终代码候选提交：** `7d064d0796844d2925e5257d7bd733b9695e6f76`（R2 原子提交链：`a8b4786` → `c386fee` → `a9ff37a` → `1a68226` → `7d064d0`）
- **最终交付状态：** `AUDIT_AND_REMEDIATION_COMPLETE_PENDING_INDEPENDENT_REVIEW`

> **诚实证据分层声明（严格区分实现状态、自我验证状态、独立审查状态与未验证平台，绝不自授独立 PASS）：**
> - **`IMPLEMENTED` / `FIXED_BY_IMPLEMENTER`：** 权威规格 `v0.4-rc3.7` 全部 11 项核心能力、双语 `ADR-0024`（105 行 1:1 同步）、Opus 5.5 第一轮指派项（`F-01 / F-02 / F-03 / F-04 / F-06(8K 准入) / F-09`）及第二轮增量指派项（`R2-01` 同目录私有临时文件原子不覆盖发布与原地并发写防误删、`R2-02` 三阶段受控门禁状态锚定与门禁后长程修复安全滚动、`R2-04` `baseSM` 增量折叠消除 `O(n^2)` SQLite 扫描、`R2-05` 报告符号与原始日志对齐）已在纯 Go 源码与文档中完整落地；零新增包、零 SQLite Schema Migration（冻结于 `0001`–`0017`）、零修改 `evalruntime/` 与 `internal/{cognition,knowledge,verify,migrate}`。
> - **`TESTED` / `SELF_VERIFIED`：** 离线确定性验收矩阵 `T1–T12`、`F-01..F-04/F-06/F-09` 回归集，以及本轮新增的 `R2-01`（400 轮引擎原地写竞争 + 300 轮 `write_file` 原地写竞争 + 100 轮双真实 OS 进程竞争，`lost == 0`）、`R2-02`（真实 SQLite 40 步门禁后修复、`32K/64K/128K/200K/600K` 五档窗口门禁后长修复与冷 `Resume` 全状态相等、三方重开一致性）、`R2-04`（真实 SQLite `2048/2049/6000/12000` 组性能实测与零每步 SQLite 扫描断言）全部在 `-race` 下实测通过，RED/GREEN 原始日志完整持久化于 `/Users/tangxingpeng/IdeaProjects/me/LumenCortex/bench/evidence/adr-0024-r2-remediation-20261119/`。
> - **`INTEGRATION_VERIFIED`（平台精确包数与未验证边界）：**
>   - **macOS ARM64（`darwin/arm64`, Go `1.26.5`, Node `v24.20.0`）：** `go list ./...` 共 **49 个包**（**43 个含测试包** + 6 个 `[no test files]`）。`go build ./...`、`go vet ./...`、`go test -timeout 35m -count=1 ./...`（43 个含测包 100% `ok`）全部一次通过；全仓 `go test -race -timeout 60m -count=1 ./...` 首轮中 42 个含测包一次通过，未改动文件 `runtime/hub_control_test.go:298` 中的 `TestHubPauseSurvivesRestart` 出现 1 次时序等待偶发失败（单独 `-count=5` 5/5 通过，紧接着完整重跑 `go test -race -timeout 45m -count=1 ./runtime` 耗时 `1641.5s` 零失败零数据竞争通过；两份原始日志均原样保留）。
>   - **Ubuntu Linux ARM64（`linux/arm64`, Go `1.27.1`, `lc-ubuntu-go:24.04` 非 root `uid=1000` 只读挂载）：** 因包含 `//go:build linux` 的 `internal/cgroupv2`，Linux 下 `go list ./...` 共 **50 个包**（除去 `runtime` 与 `evalruntime` 后共 **48 个包**：**42 个含测包 `ok`** + 6 个 `[no test files]`）。`go build ./...`、`go vet ./...`、48 个非 `runtime`/`evalruntime` 包 `go test -count=1`、16 个核心/修复包（15 个 `ok` + 1 个 `retryafter` `[no test files]`）`go test -race -count=1` 及 `./runtime` Context Space / 8K / Prompt / Skill 定向 `-race` 套件全部通过；**容器内完整的 `runtime` 与 `evalruntime` 全包测试因缺少 `systemd` 委托 cgroup、Python 内核与 `curl` 未在容器中执行，显式标记为容器环境 `UNVERIFIED`（已在 macOS ARM64 原生全量运行）**。
>   - **Windows AMD64（`windows/amd64`）：** `GOOS=windows GOARCH=amd64 go build ./...`、`go vet ./...` 及 `./internal/editing`、`./internal/toolset`、`./internal/agent` 测试二进制交叉链接（`go test -c`）全部零错误通过；**因当前环境无原生 Windows 主机，Windows 原生进程运行、`MoveFileExW` 与 `ByHandleFileInformation` 实机行为、FAT/exFAT/ReFS 文件系统文件 ID 差异、SQLite 文件锁与 `-race` 明确标记为 `UNVERIFIED`**。
> - **`INDEPENDENTLY_CONFIRMED_IN_R2` vs `PENDING_INDEPENDENT_REVIEW`：**
>   - 经 Opus 5.5 第二轮只读审查已确认为 `CONFIRMED` 的第一轮修复：`F-02`（P2）、`F-04`（P3）、`F-06`（P3，8K 兼容恢复部分）、`F-09`（P3 源码与跨平台构建层面；Windows 原生运行仍为 `UNVERIFIED`）。
>   - 本轮针对 `F-01 -> R2-01`（P1）、`F-03 -> R2-02`（P2）、`R2-04`（P3）、`R2-05`（P3）的所有修改均处于 **`FIXED_BY_IMPLEMENTER | SELF_VERIFIED | PENDING_INDEPENDENT_REVIEW`** 状态，且 `F-05 / F-06(32K–600K 窗口容量权衡) / F-07 / F-08 / R2-03` 共 **5 项 `P3`** 显式保留在 Finding Ledger 中标记为 **`OPEN (P3)`**，等待 Opus 5.5 独立复审。

---

## 1. Evidence Boundary 基线与治理核验

| 核查项 | 预期契约 | 实测证据 | 状态 |
| :--- | :--- | :--- | :---: |
| 权威架构文档版本与 SHA256 | `v0.4-rc3.7`（备份 `v0.4-rc3.6` 与 `v0.4-rc3.5`） | `v0.4-rc3.7` SHA256 = `5c9edaeb6b917d7cf508677036f17ab1911dd6b5f1a78ee25d8e0082f6cbb89f`；`v0.4-rc3.6` 备份 SHA256 = `3da2b65adda4b43fddf929dfa0d6b345e9181a75bf43c43f1c79ed5abe0ab09a`；`v0.4-rc3.5` 备份 SHA256 = `bd8dfb00c9781d4d375ce4779789a71f0cbef9f547918ea1f4aa1a0c7481abd9` | **PASS** |
| 主工作树与 R1 工作树隔离 | 不修改 `main`（`c73d173`）与 R1 分支（`a8b4786`）及 `/tmp/lc-adr0024-r2` | `LumenCortex-Core` 保持 `main@c73d173`（含原未提交 `M AGENTS.md`）；`wt/core-adr-0024-context-space-20261118` 保持 `a8b4786` 干净；`/tmp/lc-adr0024-r2` 只读未动 | **PASS** |
| SQLite Schema 与 Migration | 零新增 Migration（冻结于 `0001`–`0017`） | `git diff main..HEAD -- internal/migrate/` 完全为空；全量升降级测试通过 | **PASS** |
| Cognitive Core 与 EvalRuntime 边界 | 零修改 `internal/{cognition,knowledge,verify}` 与 `evalruntime/` | `git diff main..HEAD -- internal/cognition/ internal/knowledge/ internal/verify/ evalruntime/` 完全为空 | **PASS** |
| 工具单次写库不变量 | 每次有副作用工具调用严格 `2` 次 SQLite 写事务 | `AdmitExecution`（1）+ `CommitToolResult`/`SettleExecution`（1，`;cm=*` 内嵌于 `derived_origins.post_ref <= 92B`），`TestT4` 实测断言通过 | **PASS** |
| Desktop 公共接口兼容性 | 不破坏 `config / docs / protocol / provider / runtime` 导出签名 | 零导出破坏性变更；`editing.AtomicFileStager` 为可选扩展接口（旧 `Host` 实现自动走非误删 CAS 兜底） | **PASS** |

---

## 2. 唯一责任模块接线矩阵（100% 源码真实符号核验）

| 核心能力 | 唯一权威责任文件与真实 Go 符号 | 严禁重复与安全边界说明 |
| :--- | :--- | :--- |
| Run 级治理锁定与不可变 `SkillSnapshot` | `runtime/agent.go` (`prepareRun`), `runtime/prompt.go` (`assemblePrompt`), `internal/skills/registry.go` (`Registry.Prompt`, `SnapshottedSkill`), `runtime/skills.go` (`newSkillReader`) | 整场 Run 唯一一次扫描磁盘建立 `Eligible []SnapshottedSkill`（含内存缓存 `SKILL.md` 正文），`Prompt / ListSkills / ReadSkill` 共享同一只读快照 |
| 项目指令围栏感知大纲 | `runtime/prompt.go` (`compactProjectInstructions`, `extractFenceAwareHeadings`) | 仅作用于受信工作区 `> 12 KiB` 项目指令；`<globalDir>/AGENTS.md` 零二次截断，不放宽 `toolset/path.go` 沙箱 |
| `TaskState` 渲染与实渲引用同步 | `internal/taskstate/render.go` (`RenderBlock`, `RenderedBlock`) | 单一 `MaxBlockBytes = 10240` 截断偏移 `cutoff` 同步裁定渲染文本与 `ItemRefs`（`startByteOffset < cutoff`），零幽灵引用 |
| 保守字节 Token 估算与上界预留 | `internal/window/estimate.go` (`EstimateBytes`, `ReserveTokensForBytes`, `Estimate`) + `provider/{anthropic,openai}/wire.go` | 全系统唯一估算原语；`EchoTokens` 仅在 `GroupTokens` 计费一次，遇 `user` 行即清零 |
| 候选静态预筛、闭式预算、门禁锚定与审计重放 | `internal/agent/policy.go` (`CandidatesByStaticPrefix`, `GuardTokens`, `BaseGateSlack`, `EffectiveGateSlack`, `RepairContext`, `CausalEvidenceRef`, `WindowPrefixDecision`, `SelectMaxAvailableCandidate`, `FloorFallbackDecision`, `WindowFloorRelaxedDecision`, `ReplayDecision`, `VerifyStep`) | 全系统唯一候选预筛、比例余量（`min(baseSlack, free/4)`）、二元前缀选择、三阶段受控松弛决策（含 `gatePinnedInState` 与 `postBlockCausalFloorSeq`）及离线 `ReplayDecision` / `VerifyStep` |
| 工具与门禁反馈受约束呈现 | `internal/agent/stepspecs.go` (`IncludedStepTools`), `internal/agent/presentation.go` (`presentStepTools`, `presentFeedback`) | 每次投影分配新切片；`presentFeedback` 强制 `1200B/6144B` UTF-8 与 `2458B/12346B` JSON 双重封顶（Stage R2/R3 锚定至 `State` 的门禁状态复用同一 `presentFeedback`） |
| 私有临时文件原子不覆盖发布与路径规范化封签 | `internal/editing/engine.go` (`AtomicFileStager`, `NewCreateTempFile`, `createAtomicNoReplace`, `writeCAS`, `CommitReceipt`), `internal/editing/{fileid_unix,fileid_windows,publish_darwin,publish_linux,publish_other_unix}.go` (`fileIdentity`, `captureFileIdentity`, `publishTempNoReplace`, `removeCreatedWhileOpen`, `removeCreatedAtRollback`, `CleanupExclusivelyCreatedFile`), `internal/toolset/editing.go` (`OpenCreateTemp`), `internal/agent/{execution,callback}.go` (`executionTierOf`, `postFiles`, `sealFilesMutation`) | 创建型写操作先写入同目录私有临时文件 `.lcx-create-<nonce>` 再执行内核级原子不覆盖发布（Unix `os.Link` + `RenameatxNp(RENAME_EXCL)` / `Renameat2(RENAME_NOREPLACE)`；Windows `MoveFileExW(..., 0)`），彻底消除 0 字节目标窗口；自定义 `Host.Open` 兜底路径遇 CAS/超限/并发写绝不删目标路径，仅在设备/卷+inode+mtime+字节内容全匹配时才允许清理（`R2-01`）；`sealFilesMutation` 直接使用 `filepath.ToSlash(p)` 构建 map 键且不折大小写（`F-09`）；`debug_*` 归 `tierWorkspace -> MutationUnknown`（`F-04`） |
| 无正文历史元数据、`baseSM` 增量折叠与统一修复状态机 | `internal/session/executions.go` (`ListAnsweredExecutions`), `internal/agent/transcript.go` (`HistoryGroupMeta`, `runTranscript` 含 `historyGroups / historyOverflowed / baseSM / hasBaseSM`, `foldSettledHistoryPrefix`, `repairStateMachine`, `DeriveRepairContextFromHistory`, `StreamRepairContextFromStore`, `(*run).deriveRepairContext`, `(*run).groups`) | `historyGroups` 严格不含消息正文且内存上限 `MaxInMemoryHistoryGroups = 2048`；达到上限时将最老 `1024` 个已定型组增量折叠进值语义 `baseSM`（`O(1)` 摊还时间、`0` 次每步 SQLite 扫描），保留 `StreamRepairContextFromStore` 作无状态流式校验/兜底（`F-02`, `R2-04`） |
| 7 步无环候选投影、三阶段门禁锚定滚动与重检提示分层 | `internal/agent/send.go` (`compactToolPrefForCandidate`, `projectCandidate`, `projectCandidateForSend`, `sliceTranscriptAfterSeq`, `appendFloorRelaxationNotice`, `appendPinnedGateAndRelaxationNotice`, `Loop.send`) | Failover 优先守卫后在最大可用候选上按 `Stage R1 -> Stage R2 -> Stage R3` 单调松弛：当 `[BlockSeq..LastSeq]` 超窗时将门禁反馈 + 因果修复进度账本锚定入 `State` 并在 `Seq > BlockSeq` 上滚动历史，保证 `State` 与 `Messages` 零重复（`R2-02`）；工作区重检通知为提示词级告知，编辑工具由 `editing.Engine` CAS 保护，最终完成由 Completion Gate 重验（`R2-03`） |
| 带 `.~` 终结符无损模型分片信封 `v2` | `config/schema.go` (`64B/192B`), `internal/agent/envelope.go` (`EncodeEnvelopeModelRef`, `DecodeEnvelopeModelRef`, `requestRefs`), `runtime/{auto,verification}.go` | `projectionVersion = 2`；`model~0~..model~9.~` 最多 10 片（`P0` 最高优），缺 `.~` 终结符立即报 `ErrIncompleteModelRefChunks` |

---

## 3. 工作区重检机制的真实职责边界声明（`R2-03` 诚实澄清）

为彻底消除前版报告将提示词通知表述为“强制重检/强制拦截”的歧义，现按真实生产代码严格区分三层机制的精确作用范围：

1. **Prompt Notice（提示词级显式告知，非运行时拦截器）：**
   - 当触发 Stage R1（`appendFloorRelaxationNotice`）或 Stage R2/R3（`appendPinnedGateAndRelaxationNotice`）时，系统在当步 `State` 消息中注入 `[Context Window Notice — Floor Relaxation]`（Stage R2/R3 同时注入 `[Pinned Completion-Gate State]` 与近期因果变更摘要），明确告知模型哪些历史序列区间已被省略，并指示模型在修改代码或提议完成前先用 `read_file`、`search_text` 或 `git status`/`git diff` 重新检查工作区。
   - **边界：** 该文本属于面向模型的上下文告知，**Runtime 不会在 `Loop` 或 `toolset` 入口拦截未先调用只读工具的模型工具调用**。
2. **文件编辑工具 CAS 保护（仅覆盖 `editing.Engine` 管辖的编辑工具）：**
   - `write_file`、`replace_in_file`、`edit_lines` 与 `apply_patch` 由 `internal/editing/engine.go` 与 `internal/toolset/editing.go` 的读集版本/哈希 CAS 及原子不覆盖创建机制保护（拒绝陈旧覆盖或并发已存在路径创建）。
   - **边界：** **`bash` 子进程命令与 `mcp:*` 外部工具不经过 `editing.Engine` CAS**；若模型忽略提示词直接执行写盘 `bash` 命令或可变 MCP 工具，Runtime 不会做前置拒绝（但其副作用会被 `executionTierOf -> tierWorkspace` 记为 `MutationUnknown`）。
3. **Completion Gate 最终完成复核（拦截错误完成声明）：**
   - 模型在宣称任务完成时仍必须通过宿主的 Completion Gate / Verification 检查；未消除的失败测试或后台运行作业会再次产生 `OriginCompletionBlocked` 拒绝完成。
4. **治理跟踪状态：** 是否在未来引入运行时级工具前置硬守卫（以及如何避免误伤合法的 `go test` / `git status` 类 `bash` 诊断命令）按 Owner 决策作为独立架构增强项保留在台账中，状态为 **`OPEN (P3)`（`R2-03`）**。

---

## 4. `32K–600K` 模型窗口可用历史容量影响量化披露（`F-06` 容量权衡补充）

除 `8K` 小窗口准入恢复外，`ADR-0024` 为消除客户端字节估算低估导致的真实 Provider `400 context_length_exceeded` 震荡，引入了 `GuardTokens = max(context/16, 512)`（较 `main` 的 `Margin = context/20` 增加 `1.25%` 窗口预留；Anthropic 带工具另加 `512`）、常态 `EffectiveGateSlack = min(baseSlack, free/4)`（`>=32K` 下 `ModeCompact` 为 `1950`，`ModeFull` 为 `7718`）及保守分桶字节估算 `EstimateBytes`（字母/空格 `4/12`、数字/标点/JSON 符号 `7/12`、多字节 UTF-8 `6/12`）。在 `main`（`c73d173`）与当前实现（`7d064d0`，非 Anthropic 协议）的同口径对比如下：

| 窗口配置 (`context / output`) | `main` 扣减 `Margin(5%)` 后池 | 当前 `GuardTokens(6.25%)` | 当前 `ModeCompact` 扣减 `Slack(1950)` 后估算池（较 `main`） | 当前 `ModeFull` 扣减 `Slack(7718)` 后估算池（较 `main`） | 折算同字节混合代码/JSON 载荷等效 Provider Token 容量变化说明 |
| :--- | :---: | :---: | :---: | :---: | :--- |
| **`32768 / 4096`（默认）** | `27,034` | `2,048` | `24,674`（**`-8.7%`**） | `18,906`（**`-30.1%`**） | 在混合代码/工具 JSON 语料下叠加 `EstimateBytes` 保守系数（约 `1.20x` 于旧 `runes/4`），`ModeFull` 等效可容纳历史字节数较 `main` 约低 `~42%`，换取单步峰值按需切 `ModeCompact` 与门禁拒绝零溢出保证 |
| **`64000 / 8192`** | `52,608` | `4,000` | `49,858`（**`-5.2%`**） | `44,090`（**`-16.2%`**） | 混合代码/JSON 下 `ModeFull` 等效可容纳历史字节数较 `main` 约低 `~30%` |
| **`128000 / 8192`** | `113,408` | `8,000` | `109,858`（**`-3.1%`**） | `104,090`（**`-8.2%`**） | 混合代码/JSON 下 `ModeFull` 等效可容纳历史字节数较 `main` 约低 `~24%` |
| **`200000 / 16384`** | `173,616` | `12,500` | `169,166`（**`-2.6%`**） | `163,398`（**`-5.9%`**） | 混合代码/JSON 下 `ModeFull` 等效可容纳历史字节数较 `main` 约低 `~22%` |
| **`600000 / 32768`（评测大窗）** | `537,232` | `37,500` | `527,782`（**`-1.8%`**） | `522,014`（**`-2.8%`**） | 估算池名义减少 **`-2.8%`**；叠加 `EstimateBytes` 对混合代码/JSON 的保守计费系数后，实际可装入的等效 Provider Token 历史容量较 `main` 约减少 **`~19.5%`** |

上述 `32K–600K` 容量收窄属于用保守字节上界换取零猜测预留的已知架构权衡，继续在 Finding Ledger 中保持 `F-06`（窗口容量权衡部分）为 **`OPEN (P3)`** 透明披露，供后续评测阶段结合真实 Provider Tokenizer 校准决定是否收紧估算斜率。

---

## 5. `R2-04` 超长回合性能实测对照表（真实 SQLite，每组 `3,000B` 消息正文）

测试基准：`internal/agent/remediation_r2_04_test.go` — `TestR204MeasureProfile`（真实 `session.Open` SQLite WAL 数据库，`maxContext = 32768`，模拟 10 步 × 每步 4 次 `deriveRepairContext` 调用——对应 `Loop.Run` 1 次 + 3 候选 `Loop.send` 各 1 次）。原始日志见 `r2-04-before.log`（Commit `1a68226`）与 `r2-04-after.log`（Commit `7d064d0`）：

| 原子组数 (`groups`) | 指标 | 优化前（`1a68226`，超 `2048` 组回退全表流式重扫） | 优化后（`7d064d0`，`baseSM` `1024` 组批增量折叠） | 实测改善幅度 |
| :---: | :--- | :---: | :---: | :--- |
| **`2,048`** | 单次 `deriveRepairContext` / 40 次总耗时 | `52 µs` / `2.112 ms` | `73 µs` / `2.029 ms` | 持平（均走内存无正文切片） |
| **`2,048`** | 40 次总分配字节 / SQLite `Transcript` 扫描次数（行数） | `0 B` / `0` 次（`0` 行） | `0 B` / `0` 次（`0` 行） | 持平（`0` 次 SQLite 查询） |
| **`2,049`** | 单次 `deriveRepairContext` / 40 次总耗时 | `43,661 µs` / **`1,858.648 ms`** | `27 µs` / **`1.074 ms`** | **提速 `1,730x`（彻底消除 `2048 -> 2049` 悬崖）** |
| **`2,049`** | 40 次总分配字节 / SQLite `Transcript` 扫描次数（行数） | **`895,256,328 B` (`895 MB`)** / **`40` 次（`163,800` 行）** | **`0 B`** / **`0` 次（`0` 行）** | **消除 `100%` 堆分配与 `40` 次全表扫描 + `40` 次执行查询** |
| **`6,000`** | 单次 `deriveRepairContext` / 40 次总耗时 | `119,573 µs` / **`5,483.320 ms`** | `46 µs` / **`1.794 ms`** | **提速 `3,056x`** |
| **`6,000`** | 40 次总分配字节 / SQLite `Transcript` 扫描次数（行数） | **`2,623,581,760 B` (`2.62 GB`)** / **`40` 次（`479,880` 行）** | **`0 B`** / **`0` 次（`0` 行）** | **消除 `2.62 GB` 临时分配与 `479,880` 行正文读取** |
| **`12,000`** | 单次 `deriveRepairContext` / 40 次总耗时 | `264,533 µs` / **`10,356.324 ms`** | `44 µs` / **`1.785 ms`** | **提速 `5,802x`（单次稳定 `< 50 µs`）** |
| **`12,000`** | 40 次总分配字节 / SQLite `Transcript` 扫描次数（行数） | **`5,270,047,856 B` (`5.27 GB`)** / **`40` 次（`959,880` 行）** | **`0 B`** / **`0` 次（`0` 行）** | **消除 `5.27 GB` 临时分配与近百万行 SQLite 扫描** |
| **`12,000`** | 常驻 `len(r.historyGroups)` / `len(r.transcript)` / GC 后 `HeapAlloc` | `0` / `62` / `5.09 MB` | `1,760` (`<=2048`) / `62` / `5.73 MB` | 内存严格受 `MaxInMemoryHistoryGroups = 2048` 与 `maxContext` 双重封顶；冷恢复 `r.load` 亦从 `511.7 ms` 降至 `339.7 ms`（仅 `1` 次扫描） |

---

## 6. 原子实施与两轮审查修复 Commit 全链路清单

| 阶段 | Commit SHA | 提交摘要 | 核心交付物 |
| :--- | :---: | :--- | :--- |
| **Phase A** | `0c9f8cd6f89492d3bf23c4c140e95c8aa2cefa0a` | `docs(adr): add ADR-0024 context space projection and budget guards` | 双语 `ADR-0024` 初版、ADR 索引与接线矩阵 |
| **Phase B · Stage 1–3** | `55c6841` · `4df9e7e` · `5657272` | `feat(context): ... (ADR-0024 stage 1..3)` | 字节估算、不可变 `SkillSnapshot`、项目指令大纲、信封 `v2`、落盘回执、修复下界与 7 步候选投影、`T1–T12` |
| **Phase C & 自审** | `38e4fe0` · `dc4c082` · `cedf9a2` · `bfda8d5` · `b57655c` | 初次闭合与自主审计修复 | 闭合 `QGR-01..04` 与 `AUD-A01..03` |
| **R1 修复 · Commit 1–5 + 报告** | `97c1a60` · `16651ed` · `8cf5de2` · `394e0b5` · `a6ac592` · `a8b4786` | Opus 5.5 第一轮审查修复（`v0.4-rc3.6`） | `F-02`、`F-04`、`F-06(8K 兼容)`、`F-09` 经 Opus 5.5 R2 复核确认；`F-01` 与 `F-03` 部分确认并定位残余问题 `R2-01`、`R2-02` |
| **R2 修复 · Commit 1** | `c386feece70804b68c6438e1791246539249ef2b` | `docs(adr): synchronize ADR-0024 with Context Space v0.4-rc3.7 R2 remediation spec` | 升级主架构规格至 `v0.4-rc3.7`（备份 `v0.4-rc3.6`）并同步中英文 `ADR-0024`（`R2-01..R2-05` 正式契约） |
| **R2 修复 · Commit 2** | `a9ff37ad7a960cb8e71a9cf2fd0013d029db326d` | `fix(editing): stage creates in private sibling temp files with atomic no-replace publish (R2-01)` | 同目录私有临时文件 `.lcx-create-<nonce>` + 内核级原子不覆盖发布（Unix `os.Link` + `RenameatxNp`/`Renameat2`；Windows `MoveFileExW`）+ 自定义 `Host.Open` 非误删 CAS/mtime/字节比对 + 双进程与并发原地写回归集 |
| **R2 修复 · Commit 3** | `1a682267efdbef9b96f62825870a793706cdcf4d` | `fix(agent): pin completion-gate state and roll post-gate repair history safely (R2-02)` | 值类型 `RepairContext`（含 `GateFeedback`、`Stage`、`PostBlockCausalFloorSeq`、`[8]CausalEvidenceRef`）+ 三阶段单调 `projectCandidateForSend`（`Stage R1 -> R2 -> R3`）+ 五档窗口与 40 步门禁后长修复/冷恢复等价回归集 |
| **R2 修复 · Commit 4** | `7d064d0796844d2925e5257d7bd733b9695e6f76` | `perf(agent): fold overflowed HistoryGroupMeta incrementally into baseSM to eliminate O(n^2) SQLite scans (R2-04)` | `foldSettledHistoryPrefix` `1024` 组批增量折叠进 `baseSM` + `r.load` 单次执行摘要预关联 + `StreamRepairContextFromStore` 四方等价与 `12,000` 组实测基准 |

---

## 7. Opus 5.5 两轮全量 Finding Ledger（`F-01`–`F-09` + `R2-01`–`R2-05`）

- **增量生产代码规模：**
  - R2 增量 `a8b4786..7d064d0`：10 个生产 Go 文件（`+878 / -167` 行），含测试与双语 ADR 共 17 个文件（`+2,542 / -191` 行）。
  - 相对 `main`（`c73d173..7d064d0`）累计生产 Go 文件：37 个非测试 Go 文件（`+4,934 / -482` 行），0 个 SQLite Migration，0 个 Cognitive Core / `evalruntime` 文件改动。

### 7.1 第一轮 Finding 复核与当前状态（`F-01`–`F-09`）

| ID | 级别 | Opus 5.5 R2 复核结论 | 涉及文件与真实符号 | 状态演进与残余闭环关联 | 当前状态 |
| :--- | :---: | :--- | :--- | :--- | :--- |
| **F-01** | **P1** | `PARTIALLY_CONFIRMED` → 由 **`R2-01`** 闭环 | `internal/editing/{engine,fileid_unix,fileid_windows,create_race_test}.go`, `internal/toolset/{editing,create_race_test}.go` | R1（`16651ed`）已修好路径已存在、两方并发创建、替换/删除重建与符号链接（`TestCreateRaceForeignFilePreserved`、`TestCreateRaceOversizedForeignFilePreserved`、`TestCreateCleanupNeverDeletesReplacedFile`、`TestCreateCleanupRemovesOwnUnmodifiedCreatedFile`、`TestWriteFileCreateRaceForeignFilePreserved`）；R2 针对“独占创建后外部写入者原地写入同一 inode 被清理删除”在 **`R2-01`（`a9ff37a`）** 中根治 | `FIXED_BY_IMPLEMENTER` \| `SELF_VERIFIED` \| `PENDING_INDEPENDENT_REVIEW` |
| **F-02** | **P2** | **`CONFIRMED`**（附性能项 `R2-04`） | `internal/agent/{transcript,remediation_f02_test}.go` (`HistoryGroupMeta`, `repairStateMachine`, `DeriveRepairContextFromHistory`, `(*run).deriveRepairContext`) | R1（`394e0b5`）在真实 SQLite 上消除在线与冷 `Resume` 下界漂移（`TestF02LiveAndColdSQLiteResumeRepairFloorParity`、`TestF02MultiToolCallAttributionAndUserResetOnRealSQLite`、`TestF02HistoryOverflowStreamsFromRealSQLite`）；R2 进一步在 **`R2-04`（`7d064d0`）** 中消除 `>2048` 组的每步重扫性能退化 | R1 核心修复：**`INDEPENDENTLY_CONFIRMED_IN_R2`**；<br>`R2-04` 增量折叠：`FIXED_BY_IMPLEMENTER` \| `SELF_VERIFIED` \| `PENDING_INDEPENDENT_REVIEW` |
| **F-03** | **P2** | `PARTIALLY_CONFIRMED` → 由 **`R2-02`** 闭环 | `internal/agent/{policy,send,transcript,remediation_f03_test}.go` (`SelectMaxAvailableCandidate`, `FloorFallbackDecision`, `WindowFloorRelaxedDecision`, `projectCandidateForSend`) | R1（`a6ac592`）已修好门禁拒绝前长历史与 Failover 优先守卫（`TestF03LiveLongGatedTurnSurvivesViaFloorRelaxation`、`TestF03ColdResumeLongGatedTurnSurvivesOnRealSQLite`、`TestF03FailoverFirstGuardAndAllScenarios`）；R2 针对“门禁拒绝之后修复阶段超过单窗再次 `KindWindowOverflow`”在 **`R2-02`（`1a68226`）** 中根治 | `FIXED_BY_IMPLEMENTER` \| `SELF_VERIFIED` \| `PENDING_INDEPENDENT_REVIEW` |
| **F-04** | **P3** | **`CONFIRMED`** | `internal/agent/{execution,remediation_f04_f09_test}.go` (`executionTierOf`, `TestF04DebugToolsClassifiedAsMutationUnknown`) | 全部 `debug_*` 工具归入 `tierWorkspace -> MutationUnknown`，在线与 SQLite 重开一致 | **`INDEPENDENTLY_CONFIRMED_IN_R2`** |
| **F-05** | **P3** | `OPEN`（按范围保留） | `runtime/prompt.go` (`EffectivePrompt`, `assemblePrompt`) | `EffectivePrompt` 返回 Canonical `ModeFull` 视图，与候选实际 `ModeCompact` / Floor Relaxation 线上视图存在观测差异；留待宿主候选级 Prompt Inspector 演进时对齐 | **`OPEN (P3)`** |
| **F-06** | **P3** | 8K 兼容：**`CONFIRMED`**；<br>32K–600K 容量权衡：**`OPEN (P3)`** | `internal/agent/{policy,send}.go` (`EffectiveGateSlack`, `compactToolPrefForCandidate`), `runtime/{editing_test,session_import_test}.go` | 原始 `8000` 窗口测试已恢复并原生通过；`32K–600K` 窗口因 `GuardTokens`、`BaseGateSlack` 与 `EstimateBytes` 带来的可用容量收窄已在本文 **§4** 完整量化披露，继续作为已知架构权衡跟踪 | 8K 兼容：**`INDEPENDENTLY_CONFIRMED_IN_R2`**；<br>32K–600K 容量权衡：**`OPEN (P3)`** |
| **F-07** | **P3** | `OPEN`（按范围保留） | `runtime/prompt.go` (`extractFenceAwareHeadings`), `internal/agent/presentation.go` (`foldVerifiedExampleFences`) | 逐行围栏扫描未完整解析引用块/列表缩进容器、HTML 注释与 Setext 标题；不影响原文件与沙箱安全 | **`OPEN (P3)`** |
| **F-08** | **P3** | `OPEN`（按范围保留） | `internal/agent/presentation.go` (`presentFeedback`) | `presentFeedback` 紧凑模式下的独立反引号扫描器未覆盖波浪线 `~~~` 围栏；`2458B/12346B` 硬上限不受影响 | **`OPEN (P3)`** |
| **F-09** | **P3** | **`CONFIRMED`（源码与跨平台构建层面）** | `internal/agent/{execution,remediation_f04_f09_test}.go` (`sealFilesMutation`, `TestF09SealFilesMutationCanonicalSlashKeyMatching`) | `sealFilesMutation` 直接以 `filepath.ToSlash` 规范化键建立 map 配对且不折大小写；Windows 原生进程运行仍显式标为 `UNVERIFIED` | **`INDEPENDENTLY_CONFIRMED_IN_R2`**（Windows 原生运行 `UNVERIFIED`） |

### 7.2 第二轮增量审查 Finding 闭环台账（`R2-01`–`R2-05`）

| ID | 级别 | 责任人 | 涉及文件与真实符号 | 触发条件与根因 | 最小可靠修复方案 | RED → GREEN 可复现证据（含原始日志） | Commit | 当前状态 |
| :--- | :---: | :--- | :--- | :--- | :--- | :--- | :---: | :--- |
| **R2-01** | **P1** | R2 Remediation Owner | `internal/editing/{engine,fileid_unix,fileid_windows,publish_darwin,publish_linux,publish_other_unix,remediation_r2_01_test}.go`, `internal/toolset/{editing,remediation_r2_01_test}.go` | R1 创建分支虽用 `O_CREATE\|O_EXCL` 在目标路径上创建空文件，但在创建后至 CAS/超限检查或回滚之间，目标路径已暴露在目录命名空间中；并发外部进程/编辑器若原地打开同一文件截断或追加写入，inode 未变，导致 `removeCreatedWhileOpen` / `removeCreatedAtRollback` 将外部已写入的数据一并 `unlink` 删除 | **首选架构修复 + 自定义 Host 防护双保险：** 1) 默认本地主机与 `toolset.Set` 实现 `AtomicFileStager`（`NewCreateTempFile` / `OpenCreateTemp`），所有新建文件先写入同目录私有临时文件 `.lcx-create-<128bit-hex>` 并 `Sync`+`Close`，再通过 `publishTempNoReplace` 原子不覆盖发布至目标路径（Unix 优先 `os.Link` + `os.Remove(temp)`，EPERM/ENOTSUP 回退 `unix.RenameatxNp(..., RENAME_EXCL)` 或 `unix.Renameat2(..., RENAME_NOREPLACE)`；Windows 调用 `kernel32.MoveFileExW(..., 0)`），若目标已在并发期间出现则仅删私有临时文件并返回 CAS 冲突，**目标路径全程零 0 字节暴露窗口、零对目标路径执行 `unlink`**；2) 对未实现 `AtomicFileStager` 的自定义 `Host.Open` 兜底路径：CAS 冲突、`ErrFileTooLarge` 与 `CodeConcurrentWrite` 分支彻底禁止删除目标文件；仅自身真实 I/O 写错或多文件补丁回滚时，必须在 `fileIdentity`、`ModTime`、精确字节长度与逐字节内容四重核验全部匹配本次写入快照时才允许清理（宁可残留空文件，绝不误删外部字节） | **RED（`r2-01-red.log`）：** `TestR201InPlaceForeignMutationsNeverDeleted` 4 子项全败；`TestR201InPlaceForeignWriterRealRace` `lost=325/400`；双真实 OS 进程 `TestR201TwoOSProcessesCreateRace/inplace` `lost=25/40`；`TestR201WriteFileInPlaceForeignWriterRealRace` `lost=291/300`。<br>**GREEN（`r2-01-green.log`）：** 400/400 引擎并发原地写 `lost=0, mixed=0`、300/300 `write_file` 并发原地写 `lost=0, mixed=0`、100/100 双真实 OS 进程竞争 `lost=0, mixed=0`，macOS/Linux `-race` 与 Windows `build/vet/test -c` 全通过 | `a9ff37a` | `FIXED_BY_IMPLEMENTER` \| `SELF_VERIFIED` \| `PENDING_INDEPENDENT_REVIEW` |
| **R2-02** | **P2** | R2 Remediation Owner | `internal/agent/{policy,transcript,send,remediation_r2_02_test}.go` | R1 的 Floor Relaxation 将松弛下界硬锁在 `min(BlockSeq, NewestGroupSeq)` 以保住 `Messages` 中的 `OriginCompletionBlocked` 行；一旦模型在门禁拒绝**之后**执行较长修复（如 32K 下 `>17` 步、600K 下 `>37` 步），`[BlockSeq .. LastSeq]` 自身超出最大可用候选窗口，再次触发 `StatusFailed / KindWindowOverflow` 终止长任务 | 按 Owner 选定的**方案 B（受控门禁状态锚定 + 三阶段单调后门禁滚动）**实现：1) 在值类型 `RepairContext` 与 `repairStateMachine` 中从持久化 `session_messages` + `ListAnsweredExecutions` 确定性推导 `GateFeedback`、`Stage`（`blocked / inspecting / proven_progress / unknown_mutated`）、`OriginalMinRequiredSeq`、`PostBlockCausalFloorSeq`、`PostBlockProvenCount / UnknownCount` 与最近 `<=8` 条无正文 `CausalEvidenceRef`；2) `projectCandidateForSend` 仅在全部更大候选耗尽后在最大可用候选上按三阶段单调求值：**Stage R1** 优先完整保留 `Messages` 中的 `[BlockSeq .. LastSeq]`；**Stage R2** 当 `[BlockSeq .. LastSeq]` 超窗时，用 `presentFeedback` 受界格式化门禁反馈连同因果修复账本锚定进当步 `State`（`appendPinnedGateAndRelaxationNotice`），通过 `sliceTranscriptAfterSeq` 仅投影 `Seq > BlockSeq` 并优先保留 `[PostBlockCausalFloorSeq .. LastSeq]`（裁掉门禁后纯只读组而保留已发生变更组）；**Stage R3** 当门禁后因果组本身也超窗时，保持 `State` 门禁状态与因果账本锚定不变，滚动 `Seq > BlockSeq` 以确保最新原子组发出；3) `OriginCompletionBlocked` 要么仅在 `Messages`（R1），要么仅在 `State`（R2/R3），**绝不同时重复出现**；`WindowFloorRelaxedDecision` 新增 `gatePinnedInState` 与 `postBlockCausalFloorSeq` 并完整接入 `ReplayDecision` 与 `VerifyStep` | **RED（`r2-02-red.log`）：** `TestR202LiveGatedRepairPhase40StepsSurvivesOnRealSQLite` 在第 17 步报 `required context floor seq 12 evicted` 死亡；`TestR202FiveWindowConfigsPostBlockLongRepairAndColdResume` 五档配置（`32K/64K/128K/200K/600K`）100% 失败。<br>**GREEN（`r2-02-green.log`）：** 真实 SQLite 40 步门禁后修复（每步校验门禁反馈精确出现 `1` 次、0 丢失 0 重复）、五档窗口超越原崩溃阈值（`30/50/32/45/50` 步）在线与冷 `Resume` `RepairContext` 全结构体相等且 `VerifyStep` 全通过、Stage R2 保留 `MutationProven` 组并裁只读组、`32K->200K` Failover 优先零松弛、三方 SQLite 重开与 `Unknown` 一票否决全部 `-race` PASS | `1a68226` | `FIXED_BY_IMPLEMENTER` \| `SELF_VERIFIED` \| `PENDING_INDEPENDENT_REVIEW` |
| **R2-03** | **P3** | Open（按 Owner 决策澄清并保留跟踪） | `internal/agent/send.go` (`appendFloorRelaxationNotice`, `appendPinnedGateAndRelaxationNotice`), 本文 §3 | 前版报告将 `[Context Window Notice — Floor Relaxation]` 提示词表述为“强制要求/强制重检”，而实际上编辑工具靠 `editing.Engine` CAS 防陈旧覆盖，`bash`/`mcp` 无运行时前置拦截 | 按 Owner 选定的**选项 1**：保持 `ADR-0024` §7(3) 当前运行时设计（不新增可能误伤诊断命令的工具硬拦截器），在双语 `ADR-0024` 与本文 **§3** 明确区分 Prompt Notice、编辑工具 CAS（不覆盖 `bash`/`mcp`）与 Completion Gate 三层边界，删除一切“强制拦截/强制重检”夸大措辞，并将 `R2-03` 正式列为 `OPEN (P3)` | 文档与 ADR 已完成精确澄清，无未声明行为主张 | `c386fee` + 本报告提交 | **`OPEN (P3)`** |
| **R2-04** | **P3** | R2 Remediation Owner | `internal/agent/{transcript,remediation_f02_test,remediation_r2_04_test}.go` (`foldSettledHistoryPrefix`, `baseSM`, `StreamRepairContextFromStore`) | R1 在活跃用户轮次超过 `MaxInMemoryHistoryGroups = 2048` 时直接清空 `historyGroups` 并在每次 `deriveRepairContext`（每步多候选可达 4 次）从 SQLite 重扫全部历史消息正文，导致 `2048 -> 2049` 组出现 `840x` 延迟跳变及超长回合 `O(n^2)` 退化（`12,000` 组 40 次调用耗 `10.36 s`、分配 `5.27 GB`） | 利用 `repairStateMachine` 严格前向单遍结合律：1) 在 `runTranscript` 中新增值类型 `baseSM repairStateMachine` 与 `hasBaseSM`；2) 当 `pushHistoryGroup` 达到 `2048` 上限时，`foldSettledHistoryPrefix` 将最老的 `1024`（`historyFoldBatchGroups`）个已定型组折叠进 `baseSM` 并前移保留最新 `1024` 个组，使 `len(historyGroups)` 恒 `<= 2048`；3) `r.load` 预先执行 `1` 次 `ListAnsweredExecutions` 并在流式加载折叠前缀前即时绑定执行摘要；4) `(*run).deriveRepairContext` 直接复制值结构体 `sm := r.baseSM` 并仅遍历内存尾部的 `<=2048` 个组，实现 **`0` 次每步 SQLite 查询、`0 B` 堆分配与 `O(1)` 耗时**，同时保留 `StreamRepairContextFromStore` 作无状态流式校验/兜底 | **RED / BEFORE（`r2-04-before.log`）：** `TestR204IncrementalBaseSMParityAndZeroPerStepSQLiteScans` 在 `2049` 组因执行 `4` 次全表扫描失败；`12,000` 组 40 次调用耗时 `10,356.324 ms`、分配 `5.27 GB`、扫描 `959,880` 行。<br>**GREEN / AFTER（`r2-04-after.log`）：** `2047/2048/2049/2050/6000/12000` 组及超限后新外部用户轮次下，在线增量 `baseSM`、冷 `Resume`、全量参考切片与 `StreamRepairContextFromStore` 四方 `RepairContext` `==` 100% 恒等，每步 SQLite 扫描恒为 `0`；`12,000` 组 40 次调用降至 `1.785 ms`（`5,802x` 提速）、分配 `0 B` | `7d064d0` | `FIXED_BY_IMPLEMENTER` \| `SELF_VERIFIED` \| `PENDING_INDEPENDENT_REVIEW` |
| **R2-05** | **P3** | R2 Remediation Owner | `docs/reports/adr-0024-context-space-implementation-2026-11-18.md`, `bench/evidence/adr-0024-r2-remediation-20261119/*.log` | 前版报告存在 5 处不准确：引用了已重命名或未抽出的旧符号名（如旧草稿名而非真实符号 `removeCreatedWhileOpen` / `filepath.ToSlash` / `historyGroups` / `TestF02HistoryOverflowStreamsFromRealSQLite`）、Linux 包数误写为 43、遗漏 `WindowFloorRelaxedDecision` 部分字段、将提示词通知写为“强制要求”、未归档平台原始日志 | 1) 逐一使用 `grep` 对照当前 Go AST 核验本报告中每一个函数名、结构体字段名与测试名；2) 修正 macOS（49 总包 / 43 含测包）与 Linux（50 总包；非 `runtime`/`evalruntime` 48 包 = 42 `ok` + 6 `[no test files]`）精确计数，显式标注容器内全量 `runtime`/`evalruntime` 与 Windows 原生运行为 `UNVERIFIED`；3) 完整列出 `WindowFloorRelaxedDecision` 全部字段（`originalMinRequiredSeq`、`originalAnchorSeq`、`postBlockCausalFloorSeq`、`relaxedFloorSeq`、`blockSeq`、`newestGroupFirstSeq`、`sentFirstSeq`、`windowTokens`、`gatePinnedInState`、`reason`）；4) 将全部 RED/GREEN、性能对照与三平台验证原始日志落盘至 `bench/evidence/adr-0024-r2-remediation-20261119/` 并附 SHA256 | 全量原始日志与符号核验均已落盘可查（见 §8、§11） | 本报告提交 | `FIXED_BY_IMPLEMENTER` \| `SELF_VERIFIED` \| `PENDING_INDEPENDENT_REVIEW` |

---

## 8. 最终代码候选（`7d064d0`）跨平台验证与原始日志索引

所有原始终端输出均持久化保存在 `/Users/tangxingpeng/IdeaProjects/me/LumenCortex/bench/evidence/adr-0024-r2-remediation-20261119/`：

| 目标平台与环境 | 实际执行命令 | 实测结果（代码候选 `7d064d0`） | 原始日志文件与覆盖/未验证边界 |
| :--- | :--- | :--- | :--- |
| **macOS ARM64 — 编译与静态检查**（`darwin/arm64`, Go `1.26.5`, Node `v24.20.0`） | `go build ./...`<br>`go vet ./...` | **全部 Exit 0**（全仓 **49 个包** 零编译错误、零静态检查告警） | `macos-arm64-build-vet.log` |
| **macOS ARM64 — 全仓单测** | `go test -timeout 35m -count=1 ./...` | **全部 Exit 0**（49 个包：**43 个含测包 100% `ok`** + 6 个 `[no test files]`，0 FAIL）<br>关键包耗时：`evalruntime` `133.1s`、`internal/agent` `30.8s`、`internal/editing` `5.3s`、`internal/session` `45.0s`、`internal/toolset` `33.6s`、`runtime` `812.7s` | `macos-arm64-test-full.log`（100% 全仓原生覆盖） |
| **macOS ARM64 — 全仓 `-race`** | 首轮：`go test -race -timeout 60m -count=1 ./...`<br>复测：`go test -v -race -count=5 -run '^TestHubPauseSurvivesRestart$' ./runtime` + `go test -race -timeout 45m -count=1 ./runtime` | 首轮 42 个含测包一次 `ok`（`evalruntime` `142.7s`、`internal/agent` `69.8s`、`internal/editing` `12.1s`、`internal/session` `47.7s`、`internal/toolset` `36.9s`、`internal/window` `29.7s`），未改动文件 `runtime/hub_control_test.go:298` 的 `TestHubPauseSurvivesRestart` 出现 1 次等待落盘时序偶发失败（0 数据竞争）；随即单独 `-count=5` 5/5 `PASS`（`8.1s`），并完整重跑 `./runtime` 全包 `-race` **100% `ok`（`1641.5s`，0 FAIL，0 DATA RACE）** | 首轮：`macos-arm64-race-full.log`；<br>完整重跑：`macos-arm64-runtime-race-rerun.log`（两份日志均完整保留，绝不隐瞒首轮偶发项） |
| **Ubuntu Linux ARM64**（Docker `lc-ubuntu-go:24.04`, `linux/arm64`, Go `1.27.1`, GCC `13.3.0`, `--init --user 1000:1000`, 只读挂载, `GOPROXY=off`） | `go build ./...`<br>`go vet ./...`<br>`go test -count=1`（48 个非 `runtime`/`evalruntime` 包）<br>`go test -race -count=1`（16 个核心/修复包 + `./runtime` 定向套件） | **全部 Exit 0（0 FAIL，0 DATA RACE）**<br>Linux 全仓共 **50 个包**（含 Linux 专有 `internal/cgroupv2`）；非 `runtime`/`evalruntime` 共 **48 个包**（**42 个 `ok`** + 6 个 `[no test files]`）；16 个核心包 `-race`（15 个 `ok` + 1 个 `[no test files]`：`internal/agent` `48.1s`、`internal/editing` `6.1s`、`internal/toolset` `15.6s`、`internal/session` `42.7s`、`internal/window` `31.9s`）及 `./runtime` 定向 `-race`（`20.3s`）全通过 | `linux-arm64-docker.log`<br>**未验证边界：** 容器内完整的 `./runtime` 与 `./evalruntime` 全包套件因缺 `systemd` PID 1 cgroup 委托、Python 内核与 `curl` 未在容器中执行，显式标记为容器环境 **`UNVERIFIED`** |
| **Windows AMD64**（交叉编译 `windows/amd64`） | `GOOS=windows GOARCH=amd64 go build ./...`<br>`GOOS=windows GOARCH=amd64 go vet ./...`<br>`GOOS=windows GOARCH=amd64 go test -c`（`./internal/editing`, `./internal/toolset`, `./internal/agent`） | **全部 Exit 0**（全仓交叉编译与静态检查通过，并成功链接生成 `editing.test.exe` `5.6MB`、`toolset.test.exe` `16MB`、`agent.test.exe` `19MB`） | `windows-amd64-cross.log`<br>**未验证边界：** **Windows 原生运行时（进程执行、`MoveFileExW` 不覆盖原子移动、NTFS vs FAT/exFAT/ReFS `ByHandleFileInformation` 文件 ID 稳定性、关闭句柄后 `os.Remove` 极窄窗口、SQLite 锁与 `-race`）因无原生 Windows 宿主，全部显式标记为 `UNVERIFIED`** |

---

## 9. Strong Self-Criticism 反向自检与强制十四问回答

1. **架构是否完整实现？** 是。权威规格 `v0.4-rc3.7` 全部能力已在纯 Go 源码中落地，保持双平面正交与 `{ModeFull, ModeCompact}` 二元投影，零新增 SQLite 表或第二套 Memory。
2. **七个认知相关领域是否正确协同？** 是。Memory、Attention、Reasoning、Learning、Belief Revision、Governance 与 Verification 在同一 `Engine/Store/Loop` 上协同运行。
3. **Context Space 是否真正影响 Agent 的后续行为？** 是。涵盖 `8K` 小窗紧凑工具准入、项目大纲行号回读、技能连续分页、单步峰值自动恢复，以及长程门禁修复阶段在 `State` 中锚定门禁失败原因与近期因果修复账本并给出工作区重检告知。
4. **经验和证据是否正确进入上下文？** 是。`taskstate.RenderBlock` 在 `10 KiB` 内同步裁定渲染文本与 `ItemRefs`，零幽灵引用。
5. **修复期是否保留必要依据？** 是。严格优先在当前候选及更大 Failover 候选上完整保留 `MinRequiredSeq`；唯全部更大候选耗尽后，在最大可用候选上按 `Stage R1 -> R2 -> R3` 单调松弛：当 `[BlockSeq .. LastSeq]` 超窗时，将经 `presentFeedback` 受界处理的门禁状态与因果修复进度账本锚定入 `State` 并仅在 `Seq > BlockSeq` 上滚动历史，确保门禁反馈与因果进展既永不丢失、亦绝不在 `State` 与 `Messages` 中重复出现。
6. **Candidate Failover 是否正确？** 是。小模型绝不在存在更大可用候选时触发任何松弛（`TestR202PostBlockStageR2RetainsProvenAnchorAndFailoverFirst/post_block_overflow_fails_over_from_32k_to_200k_without_relaxation` 实证），`200K` 暂态 `503` 精确回退剩余最大可用候选。
7. **Resume 是否确定性？** 是。在线运行期与冷启动 `Resume` 共用同一纯值状态机 `repairStateMachine`；五档窗口长程门禁后修复与 `12,000` 组超长回合下，在线增量 `baseSM` 与冷 SQLite `Resume` 的完整 `RepairContext` 结构体逐字段 `==` 恒等。
8. **Skills 是否安全且可发现？** 是。Run 级不可变快照、读写锁 + 原子写盘、`<=32 KiB / <=8 KiB` 连续前缀目录与 `<=50` 条 / `<=16 KiB` 双模 `read_skill`，严格隔离 `Disabled/Shadowed/Withheld`。
9. **RequestEnvelope 是否完整？** 是。`projectionVersion = 2` 支持最多 10 片 `.~` 终结符无损模型分片解码与 `10+7+111=128` 槽位优先级保护。
10. **Cognitive Core 是否完全保持原有治理边界？** 是。`internal/cognition`、`internal/knowledge`、`internal/verify`、`internal/migrate` 与 `evalruntime` 均为零改动。
11. **`R2-01` 创建型写操作是否仍会误删外部原地写入的数据？** 不会。默认本地主机与 `toolset.Set` 先写入同目录私有临时文件 `.lcx-create-<nonce>` 再执行内核级原子不覆盖发布（`publishTempNoReplace`），目标路径上不存在 0 字节窗口且回滚只删除私有临时文件；自定义 `Host.Open` 兜底路径在 CAS/超限/并发写时绝不执行删除，其余回滚仅在文件身份、`ModTime`、长度与逐字节内容四重核验一致时才允许清理（Windows 关闭句柄至 `os.Remove` 之间的极窄自定义 Host 兜底窗口与非 NTFS 卷文件 ID 限制已如实披露）。
12. **所有平台验证覆盖到哪里？** macOS ARM64 全仓 49 个包（43 个含测包）原生编译、`go vet`、单测与 `-race` 通过（含首轮 `TestHubPauseSurvivesRestart` 偶发记录与 `./runtime` 全量重跑通过日志）；Ubuntu Linux ARM64 容器内 50 包编译/`vet`、48 个非 `runtime`/`evalruntime` 包测试、16 核心包 `-race` 及 `./runtime` 定向 `-race` 通过（容器内全量 `runtime`/`evalruntime` 标为 `UNVERIFIED`）；Windows AMD64 完成交叉编译/`vet`/`go test -c`（Windows 原生运行标为 `UNVERIFIED`）。
13. **是否存在未解决 P0/P1/P2？是否存在未解决 P3？**
    - **未解决 P0 / P1 / P2：`0` 个**（`F-01/R2-01` P1、`F-02` P2、`F-03/R2-02` P2 已全部修复并通过自测，处于 `FIXED_BY_IMPLEMENTER | SELF_VERIFIED | PENDING_INDEPENDENT_REVIEW`）。
    - **未解决 P3：`5` 个显式保留在 Finding Ledger（`OPEN (P3)`）**——即 `F-05`、`F-06`（`32K–600K` 窗口容量收窄权衡）、`F-07`、`F-08` 与 **`R2-03`**（工作区重检为提示词级告知而非 `bash`/`mcp` 运行时硬拦截）。
14. **是否可以 Merge `main`？**
    - **当前不合并 `main`。** 全部修改位于独立分支 `fix/adr-0024-r2-remediation-20261119`，状态为 `AUDIT_AND_REMEDIATION_COMPLETE_PENDING_INDEPENDENT_REVIEW`，等待 Opus 5.5 独立增量复审与 Owner 最终合并授权。

---

## 10. 历史记录归档（Phase C 与 R1 审计修正备忘）

- **Phase C `C-F02` 错误表述已于 R1 `a6ac592` 纠正：** `runtime/editing_test.go` 与 `runtime/session_import_test.go` 均已恢复原始 `8000` 窗口并通过生产代码修复原生通过；仅 `runtime/prompt_test.go` 因测试 `ModeFull` Canonical Prompt 报告一致性（数学前提要求 `fullFixed <= usableFull/2`）保留 `32768`。
- **Phase C 子代理与自审项（`QGR-01..04`、`AUD-A01..03`）：** 均在 `38e4fe0..b57655c` 闭合且在当前 `7d064d0` 全量回归中持续通过。

---

## 11. 关键交付物与原始证据日志 SHA256 清单

### 11.1 架构规格、ADR 与 Schema SHA256

| 文件路径 | 版本 / 说明 | SHA256 |
| :--- | :--- | :--- |
| `LumenCortex-architecture-audit/design/handoffs/context-space-architecture-master.zh-CN.md` | **`v0.4-rc3.7`（当前权威规格）** | `5c9edaeb6b917d7cf508677036f17ab1911dd6b5f1a78ee25d8e0082f6cbb89f` |
| `LumenCortex-architecture-audit/design/handoffs/context-space-architecture-master.v0.4-rc3.6.zh-CN.md` | `v0.4-rc3.6`（R2 升级前只读备份） | `3da2b65adda4b43fddf929dfa0d6b345e9181a75bf43c43f1c79ed5abe0ab09a` |
| `LumenCortex-architecture-audit/design/handoffs/context-space-architecture-master.v0.4-rc3.5.zh-CN.md` | `v0.4-rc3.5`（R1 升级前只读备份） | `bd8dfb00c9781d4d375ce4779789a71f0cbef9f547918ea1f4aa1a0c7481abd9` |
| `docs/adr/0024-candidate-context-projection-and-budget-guards.md` | 英文 ADR-0024（105 行，同步 `v0.4-rc3.7`） | `eaf5962cfc73dc15288ddef748c863f0ce9fab8b111f7f9163bbe4788ee479dd` |
| `docs/adr/0024-candidate-context-projection-and-budget-guards.zh-CN.md` | 中文 ADR-0024（105 行，同步 `v0.4-rc3.7`） | `df091fd4e892d03f098602be6e3fd748ab6bbe83b50dcba167bdccc6e48e2011` |
| `docs/lumencortex.schema.json` | 配置 JSON Schema（`64B/192B` 模型键边界） | `289856b69557229318803c3198b023f777e94e52e2cde7be0fecf84c1b654744` |

### 11.2 持久化原始测试与平台验证日志 SHA256（位于 `bench/evidence/adr-0024-r2-remediation-20261119/`）

| 原始日志文件名 | 用途与对应证据 | SHA256 |
| :--- | :--- | :--- |
| `r2-01-red.log` | `R2-01` 修复前 RED 复现（引擎原地写 `lost=325/400`、`write_file` `lost=291/300`、双 OS 进程 `lost=25/40`） | `6cf4cae22b663fc57fbdd261291b634b38c7f83fccc18d75d933bb24ded248b0` |
| `r2-01-green.log` | `R2-01` 修复后 GREEN 验证（全并发与双进程竞争 `lost=0, mixed=0` + `-race` + Linux/Windows 交叉验证） | `693e2f53660fe1b736d8fee17c8571654c3a5eaf8051a203ebf50a4649668b24` |
| `r2-02-red.log` | `R2-02` 修复前 RED 复现（40 步门禁后修复在第 17 步死亡 + `32K–600K` 五档窗口全败） | `6d85eff33a4698248ea8e0e149e061cf2206c09522cfe8eb84e0d8a8fe178f4e` |
| `r2-02-green.log` | `R2-02` 修复后 GREEN 验证（40 步门禁后修复 + 五档窗口冷/热等价 + `./internal/agent` & `./runtime` `-race`） | `eeaee6585eb9ebdef0c094c9ea68a6f35f2899971be528061a03029ff40718f2` |
| `r2-04-before.log` | `R2-04` 优化前性能基线与 RED 断言（`12,000` 组 40 次调用 `10,356.324 ms`、`5.27 GB`、`40` 次全表扫描） | `af60f94e0f86dd88d16affa2d2d80f0c26ba0982a91d16779f7b928e774a1b5d` |
| `r2-04-after.log` | `R2-04` 优化后性能实测与 `-race` 等价验证（`12,000` 组 40 次调用 `1.785 ms`、`0 B`、`0` 次每步扫描） | `6fb3a946580420595664977d1accce7673fd3b278fa3744b4334d4185b11a15e` |
| `macos-arm64-build-vet.log` | macOS ARM64 全仓 `go build ./...` + `go vet ./...`（Commit `7d064d0`） | `d18e2876debff176a359a86ab96d141f519339d71517649f091d41575265816b` |
| `macos-arm64-test-full.log` | macOS ARM64 全仓 49 包（43 含测包）`go test -timeout 35m -count=1 ./...` 100% `ok` | `c7abc762eba8ef922e3050c7faa267f3989cb75726d883f928b3ce639f7b9282` |
| `macos-arm64-race-full.log` | macOS ARM64 全仓 `-race` 首轮记录（42 包一次 `ok`，记录未改动 `TestHubPauseSurvivesRestart` 1 次时序偶发） | `f39370d81e55ec3f3960dc9589b2e425e29b13eb7e41cfcc976823063e422f77` |
| `macos-arm64-runtime-race-rerun.log` | macOS ARM64 `TestHubPauseSurvivesRestart` `-count=5`（5/5 PASS）+ `./runtime` 全包 `-race` 完整重跑 `ok`（`1641.5s`） | `8c425f75e1242321f985c46103f6945cef0b116c653779b8c01386a4e485f3b8` |
| `linux-arm64-docker.log` | Ubuntu Linux ARM64 容器 50 包 `build/vet`、48 非 `runtime`/`evalruntime` 包单测、16 核心包 + 定向 `runtime` `-race` | `68f6ada95db1eeea81d5358ea4bee3dbf18aa2de01c0017b823a81041cb95679` |
| `windows-amd64-cross.log` | Windows AMD64 全仓 `build/vet` + `editing/toolset/agent` `.test.exe` 交叉链接 | `1d9bbb1d3a9c837b2adffee23709d0d357ac0c6554bb03e76fb77d1c7a205853` |
