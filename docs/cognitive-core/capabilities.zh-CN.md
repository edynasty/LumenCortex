# Cognitive Core 能力与证据矩阵

**Cognitive Core v1.0：**整合分支 `integ/cognitive-core-v1-20261116`，指定输入 `8c30f75`，main `00344b5`，尚未合并。[English](capabilities.md)。[整合报告](../reports/FINAL_COGNITIVE_CORE_V1_REPORT.zh-CN.md)绑定实际工程与平台结果；[Finding Ledger](finding-ledger.md)区分历史自验与独立关闭。

| 能力 | main | 本候选实现 | 证据与限制 |
|---|---|---|---|
| 工作/情景记忆 | 已有 Task State、Notes、Session | 复用，不新增记忆系统 | 持久化、恢复和来源回归；隐藏推理不进入记忆。 |
| Scope/Time/Evidence | 已有 Harness 边界 | 统一 `Scope`、`Times`、`Evidence` 和原生 Graph | 范围、历史收窄、CAS、证据/隐私回归；只有五类结构化断言。 |
| 任务相关注意力 | 已有 Light 与 Thread Recall | 任务知识索引、分词经验召回 | 相关定位与警告优先；相似不等于适用，未测通用召回质量。 |
| 证据判断 | 模型加 Work Unit 约束 | `AssessCognition`、工具、固定前提和冲突命题 | CC-005 红绿及真实决策工具/撤销回归；不是通用因果推理。 |
| 经验提取 | 不含扩展候选 | Run 内提案、终态提取与验证 | `TestCompletionLearningThroughOrdinaryTools`；不接受模型自填执行证据。 |
| 独立采纳与应用 | 不含扩展候选 | adopt/apply 工具复用普通执行 | 匹配原操作、重查条件、策略/Hook/预算和 SQLite 回执；不做参数化重放。 |
| 反馈与负向学习 | 不含扩展候选 | 完整执行身份、独立结果检查、`needs_revision` | 无关/拒绝/未知与真实工具失败反例；结果不证明因果收益。 |
| 信念和依赖修订 | 部分基础 | Node CAS、`NodeRef` 和来源依赖 | 前提变化、失效历史、撤销/删除回归；按读取/使用检查，不另建后台观察者。 |
| 保留与归并 | 尚无统一机制 | MemoryPolicy、GovernorApply、容量维护 | 保护反例、等价别名、原子拒绝与容量压力；无物理分层或有损事实合并。 |
| 只读 Governor | 已有 | 正常 Go SQLite 分析/计划/验证 | 原生 Graph 无写入测试；模型 Curator 在请求前拒绝。 |
| 获准 Governor Apply | main 没有 | 宿主所有者事务 | 仅归档、逻辑冷热层和规范归并；branch/promote/epoch 仍是建议。 |
| 重启恢复 | 已有 Run/存储恢复 | Open 补全记忆结算 | 真实 SQLite 注入故障，不重复动作；pending 变 Unknown。 |
| 消费者隐私 | 已有来源 Fence | 私有 Uses、决策/应用/访问清理 | 公开 API、实际工具、历史和重开；本地可信宿主边界。 |
| 频率与效用 | 已有时间排名 | 读者访问与独立使用/结果分开 | 召回不计成功复用，因果价值明确未测。 |
| 任意任务迁移/持续学习收益 | 未建立 | 不宣称 | 确定性场景证明机制，不证明真实任务成功率提升或人类级认知。 |

## SDK 能力与生产自动调用的区别

“自动”仅指显式启用且满足前提后由 Framework 执行，不表示系统会自主选择所有认知动作。

| 能力 | 公开宿主 / SDK 入口 | 正常生产触发方式 | v1.0 限制 |
|---|---|---|---|
| 工作 / 情景记忆 | Session、Task State、Notes、执行与检查 | 既有 Agent Loop 和持久终态 | 不保存私有推理。 |
| 语义候选维护 | `EnableCognition`、`MaintainCognition`、`CognitionOptions.Auto` | 显式批次，或选择 Auto 后顶层运行结束触发有界批次 | `cognition_candidates` 不自动转为原生 Graph。Auto 可调用配置模型，本轮未启用付费测试。 |
| 候选启动注意力 | `ReadKnowledge` 诊断 | 顶层 `knowledgeIndexRow` 与 `BeginRun(Notices, NoticeSources)` | 最多 6 条 / 2048 字节，不包含 Experience。 |
| 经验发现 | `RecallExperiences`、`ReadExperience` | 模型选择 `recall_experiences` 或 `read_thread` | 没有自动开局索引，不保证模型一定调用。 |
| 经验提案 / 提取 | `ExtractExperience`、`ValidateExperience` | 模型调用 `propose_experience`；含子会话在内的合格 Run 结束后自动结算 | 必须有完整真实来源和获准检查，不能只学一段总结。 |
| 采纳 / 实际复用 | `AdoptExperience` | 模型调用 `adopt_experience`，再经普通 Callback 调用 `apply_experience` | 精确步骤/参数；无宿主重放 API；终身八次 Uses 及字节限制。 |
| 结果 / 恢复 | `RecordExperienceFeedback`、`AgentResult.Cognition` | 应用结束检查、终态结算、`Engine.Open` 恢复 | 不归因后来的工作区状态，不重复工具；效用保持 `causal_benefit_unmeasured`。 |
| 原生 belief / negative / abstraction | `PutCognitiveNode`、`VerifyCognitiveNode`、Graph API | 显式宿主调用，没有自动 Agent 写入方 | 模型或宿主提案不等于检查证据。 |
| 证据评估 | `AssessCognition` | 模型选用 `assess_cognition`，记录不可变决策 | 词法查询仅检索 belief；显式 ID、依赖/冲突扩展合计最多 16 条。 |
| 经验修订 | `ReviseExperience` | 来源宿主按反馈显式修改定义 | 反例停止复用，不自动改写定义或自行批准。 |
| Governor 分析 / 计划 / 验证 | `GovernorAnalyze`、`GovernorPlan(ctx, nil, …)`、`GovernorValidate` | 显式宿主请求 | 不启用模型 Curator 或自主 Epoch。 |
| Governor Apply / 保留 | `GovernorApply`、`SetCognitiveLifecycle`、`MaintainCognitiveMemory` | 显式所有者 CAS 事务 | 不是后台定时优化器；保护反例与安全知识。 |
| 来源撤回 / 隐私删除 | 既有 `RevokeSource`、`DeleteSession` | 宿主显式操作；Framework 传播 Fence、清理并恢复 | V-D1 使单个消费者删除可能导致整条共享经验撤回。 |

## v1.0 接受的限制

- 两类记录共用范围、检查、来源和隐私权威，但没有统一记录生命周期或跨族冲突视图。这是已记录的架构边界，不能宣称已修复。
- 保留 V-D1 共享所有者可用性、V-002 tier 失效/并发采纳 CAS、V-003 预校验差异；不因此放宽事务或 Fence 检查。
- Experience 是 **Verified Procedural Macro（受验程序化宏）**，不是参数迁移、通用语义学习、自主信念生成或因果收益测量。
- 长期净收益、大规模召回质量和物理存储分层尚未证明或实现。[路线图](roadmap.zh-CN.md)要求后续单独立项和证据。

## 证据口径

- “候选已实现”指真实 Go 代码和可达路径，不代表已进 main、已发布或独立 PASS。
- 普通测试、Race、Migration、离线示例、交叉编译和目标平台运行分别报告，SKIP 和未运行不能算 PASS。
- 模型原因解释、置信度、未验证摘要不自动成为事实；未知、冲突和过期状态保持可见。
- 历史 Node 代码与报告只作输入，不能替代生产 Go 或本轮实测。
