# Cognitive Core 总体架构

**候选：**`feat/cognitive-core-final-completion-20261113`，基于 main `00344b5`，引入已提交候选 `91af861`。尚未合并、发布。[English](architecture.md) · [精确提交与验证](../reports/cognitive-core-final-completion-2026-11-13.md)。

## 一个系统，七种职责

生产实现只有一个 Go `runtime.Engine`、一个 Agent Loop、一个 SQLite Store，复用已有 Evaluator、Maintenance Lease、Governor 和来源撤销/隐私清理机制。七个领域不是七个独立服务。

```mermaid
flowchart LR
  T[任务目标与 Task State] --> A[注意力：范围内有界召回]
  A --> R[推理：事实、前提、未知与冲突]
  R --> V[验证：获准检查与实际证据]
  V --> X[普通 Agent 工具执行]
  X --> O[执行记录与真实任务结果]
  O --> L[学习：候选、验证、采纳、应用、反馈]
  L --> B[信念修订与依赖失效]
  B --> G[治理：保留、归并与生命周期]
  G --> A
```

| 领域 | 解决什么问题，如何实现 | 与其他领域的边界 |
|---|---|---|
| Memory 记忆 | Task State 和上下文是工作记忆；Session、执行和检查是经历；候选与 Graph 信念是语义记忆；Experience 是程序性记忆。 | 全部进入同一 Store；版本保留来源，不增加第二个记忆库。 |
| Attention 注意力 | Notes/Thread Recall 和 Context Graph 提供基础。知识索引按当前任务相关性排序；Experience 复用 `threadctx.Tokens`，优先保留匹配的警告。 | 排名决定先看什么，不决定什么正确；保留依赖和明确数量上限。 |
| Reasoning 推理 | 模型理解任务并提出行动；`AssessCognition`/`assess_cognition` 检查精确断言、前提版本、新鲜度和相反命题。 | 区分事实、推断、假设、决策、未知和经验；Framework 不是通用逻辑证明器。 |
| Learning 学习 | `propose_experience` 保存结构化候选；任务结束后从真实经历提取并运行获准检查；未来任务经过召回、采纳、明确应用。 | 采纳不等于执行；实际执行回执与独立结果检查决定成功、失败或未知。 |
| Belief Revision 修订 | 依赖固定到确切版本，前提变化后当前结论失效；反例使经验进入 `needs_revision`，必须修改定义再验证。 | 旧版本、原因和原始结果保留；模型不能自行批准修订。 |
| Governance 治理 | Analyze/Plan/Validate 保持确定性；宿主使用 `GovernorApply`、`SetCognitiveLifecycle`、`MaintainCognitiveMemory`。 | 单一所有者 CAS 事务；仅执行归档、逻辑冷热层和等价归并，不擅自批准新事实。 |
| Verification 验证 | 复用获准 Spec/Binding、真实闭合检查、稳定指纹、来源和批准/隐私 Epoch。 | 没执行、未批准或执行中断的检查不能算通过；进程证据不能跨 Run 借用。 |

## 统一语义

- **范围与所有权：**`Scope`/`Reader` 由 Engine、Session、Project、Workspace 推导。修订不能扩大受众；没有默认全局知识库。`readerID` 是可信宿主参数，不是网络身份认证。
- **时间：**区分观察、记录、验证、生效区间、失效时间和当前读取时间。并发顺序由 CAS 版本决定，不用时间戳替代版本。
- **来源与依赖：**`SourceRef` 指向确切 Session/Run/版本；`NodeRef` 固定前提版本。既有 `derived_origins` 记录摘要、决策和消费者实际接收过什么。
- **证据与置信度：**只有 Framework 构造的观察可以引用批准的结果。`ModelConfidence` 只是模型判断，不产生证据等级或执行权限。
- **事务：**检查在写事务外执行，提交时再次检查版本、来源、权限和 Fence。治理批次全成或全败；别名固定的规范节点不能在同一批次被修改。
- **隐私：**原始经验定义与消费者反馈分开披露。公开读取和变更返回值只暴露当前读者自己的 `Uses`。只读原始策略的消费者不会连带接收其他消费者隐私；读到反例警告会记录实际聚合来源依赖。

## 实际数据流

1. 宿主显式启用认知，并批准精确断言与检查；两者不是同一授权。
2. 普通 Run 获取有限的相关定位信息，把历史当上下文而不是指令。前提未知、过期、冲突或信息不完整时，Framework 不认可 `continue`；补证、提问、暂缓、停止都有可观察记录。
3. 模型在任务中提出有前提、策略和预期结果的候选，不能自填来源真实性、执行成功或证据等级。
4. 原有任务结束边界从实际 Run、Task State、执行记录和检查结果提取经验。失败经历保留；只回答文字或执行轨迹不完整不能验证为程序。
5. 独立消费者先检查条件再采纳，随后显式提交原始观察过的操作序列。`apply_experience` 匹配工具和规范化参数摘要，复用原有工具策略、Allowlist、Workflow、预算和 Hook。存下来的策略文字不会被自动执行。
6. 回执记录确切执行 ID，真实工具失败也属于观察。完整已结算操作加独立结果检查才形成反馈；拒绝、取消、部分执行、结算未知不计为成功。
7. 反例追加新版本并要求修订定义，旧证据不覆盖。撤销的身份不能恢复；归档和恢复都不产生验证证据。
8. 重启在 Session Run Lock 下补全记忆结算；未结算动作变为 Unknown，不重放。记忆阶段错误与已完成的任务结果分别报告。

语义知识整理仍使用既有可选 `MaintainCognition`，受批次、权限和来源约束；它不是第二套 Agent 或替代实际使用反馈的学习循环。

## 明确限制

- 只有绑定到获准检查的结构化断言可成为当前事实。自然语言解释、因果推断、Graph 因果边不自动成为因果证据。
- 经验复用采用**原操作序列**。模型可以识别相关问题，但不能暗中替换参数或步骤；新操作需要新的真实经历候选。不宣称任意任务泛化。
- 等价归并要求所有者、范围、断言、正文、来源、证据、依赖和有效时间一致。更广摘要是新的、保留来源的未验证提案；有损摘要不能代替事实。
- 低频安全知识、失败反例、高重要性和被依赖信息受保护。只有重要/活跃信息时报告容量压力，不强删。冷热层是逻辑元数据，不是物理分库。
- 保证范围是本地可信宿主中的受管数据；不承诺召回外部备份、物理 SSD 擦除、跨项目共享或长期自主学习收益。

[记忆生命周期](memory-lifecycle.zh-CN.md) · [API](COGNITIVE_CORE_API.md) · [实现原理](COGNITIVE_CORE_IMPLEMENTATION.md) · [ADR-0023](../adr/0023-unified-cognitive-lifecycle.zh-CN.md)。
