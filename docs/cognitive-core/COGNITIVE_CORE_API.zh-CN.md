# Cognitive Core — Go API

**Cognitive Core v1.0 整合，2026-11-16。**输入 `8c30f75`，受保护 main `00344b5`，尚未合并；不是 Core SemVer Release。无 HTTP/Wails 或 Desktop 接入。Schema 顺序：Hub 0013 → cognition 0014/support 0015 → 原生 Graph 0016 → completion 0017，不新增 0018。[English](COGNITIVE_CORE_API.md) · [整合报告](../reports/FINAL_COGNITIVE_CORE_V1_REPORT.zh-CN.md)。

## 1. Engine 宿主 API — `runtime`

| 方法 / 类型 | 契约 |
|---|---|
| `Options.Cognition *CognitionOptions` | Engine 的维护配置。nil 使用默认模型、不启动后台维护、采用默认上限；在 `EnableCognition` 前不生效。 |
| `CognitionOptions{Maintainer config.ModelRef; Auto bool; MaxSteps int; Timeout time.Duration}` | Maintainer 走现有模型目录，空值使用默认模型。Auto 在顶层运行结束后触发后台批次；默认 6 步、90 秒。 |
| `EnableCognition(ctx) error` | 按项目数据库持久、幂等创建 `maintenance_state`。顶层 completed/failed 及后续改变来源版本的宿主/交付写入在同一事务排队。本版本没有 Disable 方法。 |
| `CognitionState(ctx)` | dirty revision、cursor、lease id/generation/expiry、pendingState（idle/running/failed）、lastError、backlog；启用前返回 `ErrCognitionDisabled`。 |
| `MaintainCognition(ctx) (MaintenanceReport, error)` | 一次调用最多一个批次；无待处理则空报告，有有效竞争 Lease 则 Held=true。失败释放 Lease、保留 dirty 来源；过期 Lease 或来源变动不提交候选。手动和自动批次继承 Engine 取消；Close 拒绝新批次并等待验证/失败结算结束后关闭存储。 |
| `MaintenanceReport` | ran、held、generation、from、through、maintenanceSessionId、sources、skipped、inserted、qualified、candidateOnly、rejected、duplicates、checks、error。checks 仅表示 Core 实际运行的绑定检查。 |
| `ApproveConstraintBinding` / `RevokeApproval(Kind: RevokeBinding)` | 复用 ADR-0017，批准精确 Assertion.Ref/ValueDigest、配置的 Check/Spec、project 范围和当前认知策略。旧认知绑定要由宿主重新批准；模型无权批准。普通 Task State 绑定不变。 |
| `ReadKnowledge(ctx, ref) (any, error)` | 宿主在主工作区的只读诊断；空 ref 返回 KnowledgeList，kc_… 返回 KnowledgeView。不运行检查；宿主无 Run，因此不能借用 Run 绑定的新鲜证据。 |
| `ErrCognitionDisabled` | `session.ErrMaintenanceOff` 的公开别名。 |

## 2. Agent 工具 — `read_knowledge`

已启用认知的非维护 Run 提供此工具；permission=read、capability=context.read。

- 参数 `{"ref":"kc_…"}`；省略 ref 则列举。
- `KnowledgeList{context,candidates,truncated}` 最多 20 个 live 定位，只有 ID、枚举和路径；带内容的定位包含维护会话来源。已撤回定位不带路径、检查 ID 或来源依赖。
- `KnowledgeView` 返回 context、id、status、applicability、reason、claimType、subject、claim、assertion、bindingDigest、rationale、sources（ref/sessionId/withdrawn/missing/changed）、citedPaths（path/blob/state）、checks（id/freshness/atCurrent/observations）、maintenanceSessionId、createdAt。观察包含 outcome、purpose、current、episodeId、at。已撤回/清理候选不返回正文；拒绝或未知 ID 返回 no candidate。
- 有断言的 subject/claim 由 Core 确定性生成，rationale 为空。模型文字保留在私有维护审计记录中并受隐私清理，不冒充已验证内容。
- `KnowledgeFraming` 明确：候选是模型输出，current 仅表示宿主绑定到该精确断言的检查对当前读者新鲜通过，工作区事实优先。
- 单条和带内容列表的来源/维护会话进入读取 Run 的 `derived_origins`。Start 同事务记录 Notice 来源；提交时重查 Fence，撤回前取得的定位不能在撤回后写入。删除也覆盖只看过列表或 Notice 的读者。

## 3. Start Notice — `origin=knowledge`

顶层 Start 在 qualified 候选与目标/输入有正相关时追加 `protocol.OriginKnowledge` 用户行。最多 6 条、2048 字节；相关性筛选先于预算，保留匹配冲突/未知信息，不含 subject/claim/rationale。子会话和维护 Run 不注入。写入前最多重验 2 个绑定检查、15 秒；检索排名不产生证据或权限。**此索引不包含 Experience。**

## 4. Maintainer 输入 / 输出

首条消息是指令行加 JSON：策略版本、每个来源的有界投影/引用、宿主绑定所支持的断言目录 `{kind,target,key}`（最多 64，不含值或检查 ID）。整个消息不超过 32 KiB；放不下的来源留在 dirty 队列。

最终回答必须是 `{"candidates":[…]}`，允许 JSON 代码围栏，总计最多 16 KiB、8 条：

```json
{"claimType":"procedure|fact|failure|decision","subject":"≤120 B","claim":"≤1024 B，断言存在时省略",
 "rationale":"≤1024 B","sourceRefs":["session:<id>@sha256:<64 hex>"],"citedPaths":["workspace/relative"],
 "assertion":{"kind":"config_value|dependency_version|symbol_signature|file_exists|port_value","target":"…","key":"≤128 B","value":"≤256 B"}}
```

只有 fact 可带 assertion。未知字段拒绝该条；自填 status 或 checkIds 同样拒绝，模型不能选择检查。Framework 完成 `knowledge.Validate`、引用路径 Blob 解析、绑定查找和实际检查。

## 5. 来源、证据与适用性 — `internal/knowledge`

`SourceRef{Kind,ID,Digest,Scope,RunID,SessionID,Sensitivity,Provenance}`，Ref 为 `kind:id@digest`。Session Digest 来自真实 sourceVersion（状态、最新消息 seq、Task State 版本、最终回答）。RunID 表示最新**已启动** Run：`BeginRun` 同事务写 `agent.start{runId}`，`sourceRunSQL` 按最大检查点 seq 读取；仅无该记录的旧 Run 回退到最新 main 请求。首个请求前失败不能借用旧经历；时间戳截零、相同时间和时钟回拨不影响身份。没有真正 BeginRun 的 auto-effort 请求不替换它；裁剪保留最新 agent.start（ICRA-002 / V-001）。

- Kind 必须注册，目前只有 session 有生产来源。Provenance 必须为 runtime；private 敏感来源不进入批次。
- `DedupeKey(ref,readerScope)` 包含权威、类型、ID、版本、范围、PolicyVersion。来源新版本形成新键。维护会话被 Fence 后，原处理不再有效；`RedoKey` 和 `ProcessingKeys` 解析后继处理链，历史记录不覆盖，skipped 仍有效。
- `ClaimDigest` 表示断言值或规范化文字；`SupportDigest` 还包含来源版本、范围、策略和绑定。去重只比较 live 支持，不让已撤回候选阻挡独立新支持。
- Freshness=version 仅适用于批准 Inputs 内有界普通文件的进程内 Schema 检查，相同 fingerprint/spec/approval/scope 才复用。进程检查为 run，只能在原 Session/Run、无中途修改/环境变化且 5 分钟内复用；环境摘要不持久化。
- 证据必须是 Runtime 产生、获准定义、闭合 Episode、稳定前后/结果指纹及 passed/failed；error、执行中、取消、范围变化、未批准不合格。`eval_results` 中后来的同状态失败（含未链接 Work Unit 结果）使旧 PASS 失效。其他 Run 的失败产生 unknown，不借用为 stale；其他 Run 的 PASS 不复活旧证据。当前读者自身的新合格检查可恢复 current。
- `Decide(Facts)` 顺序：withdrawn → source_missing → conflict → cited/source changed 的 stale → candidate_only（提交资格或绑定不足）→ 当前绑定检查的 current/stale → unknown。
- EvidenceLevel：current=3，unknown=2，candidate_only=1，其余=0。同 subject 的不同主张只有支持等级至少相等才相互冲突；无支持模型输出不能压过已受检事实，同级冲突不强行选边。按 ID 读取在自身 subject 范围内比较。
- PolicyVersion 包含上限并参与支持/绑定摘要。策略变化不自动重跑已消费来源；宿主重新批准并提供独立新来源/版本才建立新支持。

认知包不导入 DAP、LSP、MCP、Eval；这些接口不变。

## 6. Session Store 内部 API

维护状态/Lease：`EnsureMaintenanceState`、`ReadMaintenanceState`、`AcquireMaintenanceLease`、`RenewMaintenanceLease`、`MaintenanceSources`、`ProcessingKeys`、`CommitMaintenance`、`FailMaintenance`。提交重查来源 CAS 与当前精确支持。

证据/候选：`QualifiedResult`、`CognitionResults`、`AppendObservation`、`Candidates`、`Candidate`、`Observations`、`SessionPurpose`、`SourceVersion`、`CreateDerivedSession`，复用 `ActiveConstraintBindings`、`BeginPrivacyDeletion`/`PurgeStep`。CognitionResults 在一个快照选择当前/关联证据及后来的失败；rowid 只用于该语句内顺序，持久身份仍是 `(session_id,seq)`。删除先停派生运行，再清实际依赖，最后重建/截断 WAL。

内部错误：`ErrMaintenanceOff`、`ErrNothingPending`、`ErrMaintenanceLeaseHeld`、`ErrStaleLease`、`ErrSourceChanged`、`ErrNotQualified`、`ErrCandidateNotFound`。Hub 的 `ErrLeaseHeld` 仍表示任务 Lease 冲突。`Evaluator.InRun(seq)` 只暴露本 Evaluator 闭合且未受后续修改/环境变化影响的结果；Start 重验后重新读取绑定，撤销不能混入 current 索引。

## 7. Runtime 事件

`cognition.maintained`、`cognition.inherited`、`cognition.read`、`cognition.revalidation.failed`、`cognition.inheritance.failed` 仅通知，dirty 状态不依赖事件交付。

## 8. Experience 读取与隐私

`ReadExperience(ctx,readerID,id,version)` 和 `ReadCognitiveNode` 返回共享定义/原验证，加**当前读者自己的 Uses**。空、仅来源或无关读者看不到其他消费者使用记录；变更/幂等响应同样投影。内部历史完整保存。因此 `Experience.Statistics()` 只统计返回视图，不是项目总数。

原始 Context/read_thread 定位依赖 origin；needs_revision 警告另依赖聚合 owner，因为它传达消费者反例。历史来源不收窄；采纳必须重查并先提交身份。

**V-D1 为 v1.0 接受限制：**任一已有 Use 的消费者撤销/删除会 fence 聚合 owner，使全部读者无法读取共享经验；隐私删除清理派生内容。仅依赖已验证 origin 的会话不连带 fence，但前提变 stale。返回值隐藏 Uses 不等于存储已按消费者拆分；历史读取和 Restore 不能绕过 Fence。

## 9. 完整宿主 API

| API | 契约 |
|---|---|
| `CognitiveScopeFor(ctx,sessionID,kind)` | 从 Engine/Session 推导项目、工作区、会话、读者和来源；没有全局默认授权。 |
| `PutCognitiveNode(ctx,owner,node,expected)` | 新建/修订提案；拒绝自填 Evidence、VerifiedAt、fact、扩大受众或丢失来源。规范绑定和已有别名只能走生命周期事务。 |
| `VerifyCognitiveNode(ctx,owner,id,expected)` | 执行精确宿主绑定检查、追加实际观察；失败保留失效时间，不能复活失效身份。 |
| `ReadCognitiveNode` / `ReadExperience` | 历史或最新记录受当前来源、范围、Fence 约束；仅返回读者自己的 Uses，历史状态不授权当前使用。 |
| `AssessCognition(ctx,reader,query,ids)` | 最多 16 条，含依赖与冲突对象；返回 items、recommendation、sources、incomplete，不授予执行权限。 |
| `CognitiveMemory(ctx,reader,id)` | 逻辑类别、宿主保护/策略、读者访问次数/时间和自己的反馈统计；Utility=`causal_benefit_unmeasured`。 |
| `GovernorAnalyze` / `GovernorPlan(ctx,nil,options)` / `GovernorValidate` | 正常 Go SQLite 的宿主诊断与确定性计划；runtime 别名供外部 Go 宿主使用。模型 Curator 在请求前返回 `ErrGovernorCuratorDisabled`，预校验不等于执行授权。 |
| `GovernorApply(ctx,owner,plan,expected,reason)` | 所有者明确选择的 tiers/archive/等价归并；CAS、事务全成全败。拒绝 branch/promote/epoch；规范目标同批不能改动。 |
| `SetCognitiveLifecycle(ctx,owner,id,expected,action,reason)` | archive/restore/revoke。保护安全/反例；恢复检查来源/依赖/时间，不复活失效身份。目标升版或撤销不阻止别名保留历史 pin 后撤销。 |
| `MaintainCognitiveMemory(ctx,owner,capacity)` | 仅归档 stale/dormant、未保护、无引用的所有者记录；返回 active/protected/archived/pressure，不做破坏性 GC。 |
| `AgentResult.Cognition *CognitionCompletion` | 提取/反馈计数与记忆阶段错误，独立于任务主结果。 |

宿主仍可调用 `ExtractExperience`、`ValidateExperience`、`AdoptExperience`、`ReviseExperience`、`RecordExperienceFeedback`。真正应用仅走已准入 Agent 的工具 Callback，没有宿主命令重放接口；诊断读取不自动暴露给模型。

AssessCognition 词法查询只检索原生 belief 的标题/正文/标签/断言字段，显式 ID 可选其他类型。缺失/不可见前提不披露，依赖结论变 `stale/dependency_changed`。Experience 仅新增 Uses 不改变固定前提；定义、状态、验证、范围、生命周期变化仍失效，宿主 tier 变化也仍失效（V-002）。并发采纳可返回 `ErrCognitiveRevision`，没有自动采纳重试。Governor 拒绝别名链，但部分 dormant/stale 或同批目标冲突到事务提交才拒绝（V-003）。

## 10. 普通 Agent 工具

仅在启用认知、非维护 Run 中注册；Session/Run 由 Engine 绑定，模型不能选择其他消费者。

| 工具 | 输入 | 行为 |
|---|---|---|
| `recall_experiences` | `{query}` | 最多 8 条相关定位与来源/读者访问统计，不计成功复用。requires_condition_check 表示仍需检查；八次 Uses 耗尽或剩余字节无法覆盖新 Use 及未结算反馈时为 use_limit_reached，拒绝采纳。 |
| `adopt_experience` | `{id,version}` | 当前消费者独立检查条件，先提交此 Run 的单次身份，再返回策略。 |
| `apply_experience` | `{id,adoptionId,actions:[{tool,arguments}]}` | 匹配原完整动作序列，重查条件、先提交 pending 回执，再走普通工具执行；最后一个动作结算后、运行继续前检查结果。返回 outcomeCheck=held/not_held/not_observed。每次采纳一次应用，不重放。 |
| `propose_experience` | `{proposal}` | 只保存有限结构；终态 Framework 提取真实 Episode 并执行获准检查。 |
| `assess_cognition` | `{query,ids?,action}` | 评估前提/冲突并提交确切版本决策与来源。action 为 continue/gather_evidence/ask/defer/stop；证据不足时不认可 continue。 |

模型没有 Governor 生命周期或批准 API。应用仍受 Policy、有效工具集合、Workflow、Hook、预算约束；Eval 不得递归通过 Experience 获得前台 Shell。

每个动作观察当前 Workflow/转换 Hook，原子提交执行、Task State 和来源后才继续；下一步重算权限，后台 Shell 在提交前拒绝。执行 ID/结果来自调度器宿主元数据（json:"-"），不按“最近同参数”推测。内嵌检索来源先提交再暴露，并加入外层 sourceSessions。

完整已结算失败动作可支持独立反例；负向原程序可确认警告。拒绝、中断、部分、未知应用保持 unknown，不制造检查。Engine.Open 将遗留 pending 标 unknown 并补记忆结算，不重复工具。

结果以最后一个动作结算时的回执观察为准；终态、宿主反馈和重启结算都不重新检查后来工作区状态。结算前批准/隐私纪元失效时记 unknown，不用后来 PASS 替代。

## 11. 冻结启用和兼容性边界

- 外部宿主导入 runtime，不导入 internal 包；reader/session ID 是可信宿主参数，不是对不可信网络客户端的认证。
- EnableCognition 启用追踪和工具；Auto 另行选择有界模型维护，不启用自主 Governor 或隐藏学习后台。
- Start 只提示维护候选；Experience 需明确召回/应用。存在前提后，提案终态处理、应用检查、反馈和重启恢复由 Framework 自动执行。
- 原生 belief/negative/abstraction 创建/验证、Governor Apply、archive/restore/revoke、容量维护必须由宿主明确调用。SDK 可用不等于生产自动调用。
- 两记录族共用断言、检查、来源、隐私，但没有合并生命周期、自动桥接或跨族冲突视图。
- Experience 是 **Verified Procedural Macro（受验程序化宏）**：精确已观察动作、终身八次 Uses、64 KiB Node，反馈/生命周期预留可能提前限制容量。success 表示重现动作并符合回执上的合格结果；预存结果也可能满足，不证明因果收益。Utility 保持 causal_benefit_unmeasured。
- 此 Go/JSON 契约定义 v1.0 能力基线，不变更 Runtime 版本常量、Core Tag、Desktop pin 或 Schema。
- 支持从规范 main Hub 0013 或新 Go Store 升级。不转换旧未发布候选的随机 Experience owner/错误验证记录；保留原库，使用新测试状态目录。历史分支冲突和 Node 库继续拒绝。详见 [Migration 验证](../reports/COGNITIVE_CORE_V1_MIGRATION.md)。

[生产入口矩阵](capabilities.zh-CN.md) · [接受限制与演进](roadmap.zh-CN.md) · [Finding 关闭](finding-ledger.md)。
