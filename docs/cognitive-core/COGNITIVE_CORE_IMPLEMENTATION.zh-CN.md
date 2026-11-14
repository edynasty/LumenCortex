# Cognitive Core — 实现原理

**2026-11-13 未合并候选。**main `00344b5`，历史认知输入 `91af861`。这是生产 Go 实现说明，不把本地候选写成 main 或长期收益。[English](COGNITIVE_CORE_IMPLEMENTATION.md) · [实际验证/提交](../reports/cognitive-core-final-completion-2026-11-13.md)。

## 为什么不新增一套“认知平台”

原 Engine 已有任务状态、执行、检查、来源、SQLite、Context Graph 和维护 Lease。问题不是缺少第二个 Loop，而是这些记录没有完整接到经验应用、结果修订和长期保留。实现继续复用原权威边界，让模型负责解释与选择，Framework 负责证据、权限、版本和恢复。

| 问题 | 实际 Go 代码 | 数据如何流转 |
|---|---|---|
| 统一范围/时间/证据 | `internal/knowledge/semantics.go`、`graph.go`、`lifecycle.go` | `Scope`/`Reader` 限制受众，`Times` 分开观察/验证/生效/失效，`Evidence` 引用批准结果；`ModelConfidence` 不参与证据晋升。 |
| 当前任务召回 | `runtime/knowledge.go`、`experience.go`、`internal/threadctx` | 复用词面/CJK/camel-case Tokens；按目标/输入选择定位，匹配警告优先；排名不改变事实状态。 |
| 根据证据决定下一步 | `runtime/cognitive_assessment.go`、`internal/session/cognitive_queries.go` | 读取确切节点、前提版本、冲突命题；检验批准/来源/指纹；输出 current/unknown/stale/conflict 和明确建议。 |
| 普通任务产生经验 | `internal/toolset/cognitive.go`、`runtime/experience_execution.go` | 模型提交有界提案，普通任务结束后 Framework 从真实 Run/Task/工具/检查提取并验证。 |
| 实际应用而非只读取 | `internal/session/cognitive_completion.go`、`experience_actions.go`、`internal/agent/callback.go` | 对原工具/参数摘要匹配，事务先写 pending，再经普通权限执行；结算记录实际执行 ID。 |
| 结果反馈与修订 | `runtime/experience_reuse.go`、`internal/knowledge/experience_transition.go` | 完整已结算序列允许独立结果检查；成功/失败/未知追加版本；反例要求新定义再验证。 |
| 保留与归并 | `runtime/cognitive_lifecycle.go`、`internal/session/cognitive_lifecycle.go` | 宿主 CAS 批次管理 tiers、archive、等价 canonical；前提用 NodeRef 固定，关键反例保护，容量不足报告 pressure。 |
| 重启与隐私 | `runtime/engine.go`、既有 Fence/Purge，加新记录族 | 原 Run Lock 下补记忆结算，不重复动作；来源和真实消费者依赖加入 `derived_origins`，新表纳入隐私删除。 |

## 语义知识与程序经验共用什么

`MaintainCognition` 继续使用现有 Maintenance Lease。它读取有限来源投影，只能通过 `read_thread` 执行，不能选检查或自填 verified。Framework 从宿主绑定中选择精确断言检查，检查完成后事务再次验证来源、Lease、批准和 Fence，再写候选/观察。没有第二套自主学习调度器。

程序经验也使用相同 `Scope`、`SourceRef`、`Evidence` 和 Session Store：原始 Episode → 候选定义 → 条件/结果验证 → 独立采纳 → 明确应用 → 独立反馈 → 修订/撤销 → 未来召回。`eval_results` 仍是检查权威，`derived_origins` 仍是执行/来源权威；新回执只引用它们，不另造结果事实。

## 权限与状态不混用

- 宿主 `PutCognitiveNode` 只提交提案，不允许传入 Evidence、VerifiedAt 或 fact 自证。
- `VerifyCognitiveNode` 执行获准检查，记录实际观察和版本；失败时间不覆盖，失效身份不可恢复。
- `adopt_experience` 必须独立重验条件；返回策略不等于执行权限。
- `apply_experience` 匹配原完整工具序列。参数/步骤改变需要新经历；模型不能把检索相似当成可任意替换命令。
- 每个内嵌动作走 Loop 同一 `observeWorkflow`，执行转换 Hook，并将 Task State、执行结果、来源边一起提交；下一步重新计算有效工具。人工 Gate、拒绝转换会停止序列。`mode:"background"` 在提交 Shell 前拒绝，不改成前台偷跑；Eval 不获得该权限。
- `Application.Executed()` 要求完整序列具有互不重复、顺序正确的执行 ID 与已结算结果。身份来自调度器，不按时间/参数猜测；事务核对 Session/Run/工具/摘要/结果。真实失败可以作为反例，未知动作保留记录但不计成功或反例。
- `AssessCognition` 只读；工具决策记录实际评估版本和来源，Unknown/Conflict 不悄悄晋升为事实。因果关系仍是提案，不是检查证明。

## 记忆维护与隐私

保留策略把频率、最近访问、独立采纳、实际结果、重要性和证据分开。`CognitiveMemory` 只返回读者自己的访问/反馈，效用明确为未测。Governor 不能仅按频率删安全知识；容量维护只归档 stale/dormant 且无引用、未保护的所有者记录。

等价归并需要断言、范围、正文、来源、证据、依赖和有效时间相同；只归档别名并保留精确规范版本。一个批次不能同时更改该规范目标。更广摘要是带来源依赖的新提案，不自动验证。

Experience 公开读取、变更和历史响应只包含当前读者自己的 Uses。仅接收原策略的消费者不连带成为其他消费者的读者；实际接收反例警告时则记录聚合来源。来源撤销先拒绝后续读取，隐私删除清理所有相关版本、回执和访问记录，失败清理可重启继续。被清理的经验身份保持撤回：同一来源经历不能再次提取同一身份，随记录一起清除的失效决定和反例不会被一条全新的已验证记录取代；新的真实经历产生新的身份。

内嵌检索在 Hook 装饰前按普通工具解析来源，在结果返回前事务提交依赖，并将来源并入外层响应。`read_thread` 的 Attention 保留有限 Experience 定位和真实反例警告，保持 `requires_condition_check`/`contested` 标签，不晋升事实。通用 Node 写入不能创建或改写规范别名；撤销别名不要求历史规范版本仍是最新有效版本。

Workflow 的共享 `EffectiveAllowlist` 保留 nil/空列表区别：nil 表示不增加限制，显式空列表表示禁止全部工具。普通步骤、应用 Callback 和 Prompt 检查使用相同结果，已持久化的转换后也不会重新开放权限。

## 恢复与兼容性

0017 在原 SQLite 中新增有限回执、提案、决策和访问表。终态记忆失败不改写主任务结果，单列 `AgentResult.Cognition`。重启持原 Run Lock，把遗留 pending 应用标为 unknown，仅补已执行事实的记忆结算，不重复工具。旧版本不能理解新数据时拒绝降级，不伪装兼容或静默丢失。

## 当前限制

五类结构化断言；原操作序列条件复用；每条经验八次 Uses 和 64 KiB Node 上限；大型选择域仍可能需要扫描。没有物理冷热分库、通用因果推理、任意参数迁移、自动 Epoch、跨项目共享或长期学习收益结论。模型质量仍决定提案是否有用；测试证明的是实际机制和边界，不是人类级认知。
