# Cognitive Core — Go API

**2026-11-13 未合并候选，main 基线 `00344b5`。**[English](COGNITIVE_CORE_API.md)。无 HTTP/Wails 或 Desktop 接入。Schema 顺序：Hub 0013 → cognition 0014/support 0015 → 原生 Graph 0016 → completion 0017。

## 宿主接口

| API | 契约 |
|---|---|
| `EnableCognition(ctx)` | 按项目持久、幂等启用认知；不等于批准检查或启用自动模型维护。 |
| `CognitionState` / `MaintainCognition` | 读取 dirty revision、cursor、lease、pending/error；按现有 Lease 执行一个有限维护批次。默认不自动调用模型，Auto 需宿主选择。 |
| `ApproveConstraintBinding` / `RevokeApproval` | 复用 ADR-0017，把精确断言绑定到宿主批准的检查；模型不能调用。 |
| `ReadKnowledge(ctx, ref)` | 只读语义候选或定位列表，不执行检查；current 仅表示此读者当前有相应证据。 |
| `CognitiveScopeFor` | 从 Engine/Session 推导范围；没有默认全局授权。 |
| `PutCognitiveNode(ctx, owner, node, expected)` | 新建/修订宿主提案；拒绝自填 Evidence、VerifiedAt、fact，不能扩大受众或丢失来源。规范绑定和已有别名只能走生命周期事务。 |
| `VerifyCognitiveNode` | 执行绑定检查并追加真实结果；失败保留失效时间，不能复活失效身份。 |
| `ReadCognitiveNode` / `ReadExperience` | 历史或最新版本，受当前 Scope/Fence/来源约束；只返回该读者私有 Uses。 |
| `AssessCognition(ctx, reader, query, ids)` | 只读评估，最多 16 条，包含依赖与相反命题；返回类型、适用性、原因、证据、建议、来源和 incomplete。不授权执行。 |
| `CognitiveMemory(ctx, reader, id)` | 逻辑类别、宿主策略/保护、读者访问次数/时间和自己的结果统计；效用为 `causal_benefit_unmeasured`。 |
| `GovernorAnalyze` / `GovernorPlan` / `GovernorValidate` | 正常 Go SQLite 上的确定性只读分析；模型 Curator 缺少请求准入时在调用模型前拒绝。 |
| `GovernorApply(ctx, owner, plan, expected, reason)` | 宿主授权的归档、逻辑冷热层、等价规范归并；单一所有者、CAS、事务全成全败。拒绝 branch/promote/epoch；规范目标不能在同批改变。 |
| `SetCognitiveLifecycle` | archive/restore/revoke；保护安全知识与反例，恢复重查来源/依赖/时间，撤销身份不可复活。规范目标升版或撤销不阻止别名撤销，历史绑定不改写。 |
| `MaintainCognitiveMemory` | 仅归档 stale/dormant、未保护、无引用的所有者记录；返回 active/protected/archived/pressure，不做破坏性 GC。 |
| `AgentResult.Cognition` | 提取/反馈计数和记忆阶段错误，与任务主结果分开。 |

`ExtractExperience`、`ValidateExperience`、`AdoptExperience`、`ReviseExperience`、`RecordExperienceFeedback` 继续提供给宿主。实际应用走已准入 Agent 的普通工具 Callback，不新增宿主命令重放接口。宿主只读诊断不自动成为模型接收。

## 普通 Agent 工具

仅在已启用认知且不是 Maintenance Run 时注册。Session/Run 身份由 Engine 固定，模型不能选择其他消费者。

| 工具 | 输入 | 行为 |
|---|---|---|
| `recall_experiences` | `{query}` | 最多 8 条相关定位、来源与读者访问统计，不计实际复用。`applicability` 为 `requires_condition_check`；记录的 8 个 Uses 用尽后为 `use_limit_reached`，不再接受采纳。 |
| `adopt_experience` | `{id,version}` | 当前消费者独立检查条件，先提交单次身份，再返回策略。 |
| `apply_experience` | `{id,adoptionId,actions:[{tool,arguments}]}` | 匹配原完整操作序列，重查前提，提交 pending 回执，普通工具执行并记录确切 ID；最后一个动作结算后、运行继续之前，用宿主绑定的检查观察预期结果，并在返回中给出 `outcomeCheck`：`held`、`not_held` 或 `not_observed`。每次采纳只能应用一次，不自动重放。 |
| `propose_experience` | `{proposal}` | 队列只保存有界结构；任务结束后 Framework 提取实际经历并执行获准检查。 |
| `assess_cognition` | `{query,ids?,action}` | 评估前提与冲突，记录确切版本和来源。action 为 continue/gather_evidence/ask/defer/stop；没有充分证据时 continue 仍返回 gather_evidence。 |

应用仍受普通 Policy、有效工具集合、Workflow、Hook、预算与超时约束；模型没有 Governor、检查批准或隐私删除权限。Eval 不能递归利用经验工具获得 Shell。

每个内嵌动作都观察当前 Workflow 和转换 Hook，原子提交执行/Task State/来源后才继续。后台 Shell 在提交前拒绝。执行 ID/结果是调度器专属宿主元数据（`json:"-"`），不会按最新同参数记录推测身份；内嵌结果的来源在返回前提交，并进入外层 `sourceSessions` 并集。

## 证据与恢复

- `read_knowledge` 及 Start 索引只暴露有限定位，正文按读取时适用性判断。Start 最多 6 条/2048 字节、最多重验 2 个检查/15 秒。
- 维护输出不能自选 `checkIds` 或 status；Framework 按精确断言选择获准检查。进程检查不能跨 Run 复用，Schema 仅在批准输入/指纹一致时复用。
- 完整已结算工具失败可成为反例；负向预期可以由独立失败检查确认警告。拒绝、中断、部分、未知应用不计成功，也不补造结果检查。
- 应用的结果以回执在最后一个动作结算时的观察为准。运行结束时的结算、宿主调用 `RecordExperienceFeedback` 和 `Engine.Open` 的恢复只记录这次观察，不再运行检查：应用之后本次运行或其他人对工作区的改动，既不算该做法成功，也不算反例。观察的批准在结算前失效（绑定撤销、隐私纪元）时不补做检查，该次使用记为 unknown。
- `Engine.Open` 将遗留 pending 应用设为 unknown，并补全可恢复的记忆结算；不重复动作。
- 来源和消费者撤销优先于历史可读性；Uses、访问统计和应用回执都加入现有隐私清理。

[生命周期](memory-lifecycle.zh-CN.md) · [实现](COGNITIVE_CORE_IMPLEMENTATION.zh-CN.md) · [验证报告](../reports/cognitive-core-final-completion-2026-11-13.md)。
