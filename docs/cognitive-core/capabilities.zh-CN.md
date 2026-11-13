# Cognitive Core 能力与证据矩阵

**2026-11-13：**main 为 `00344b5`，`feat/cognitive-core-final-completion-20261113` 尚未合并。[English](capabilities.md)。精确命令、结果、平台限制和提交见[交付报告](../reports/cognitive-core-final-completion-2026-11-13.md)；有源码和自验不等于独立验收。

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

## 证据口径

- “候选已实现”指真实 Go 代码和可达路径，不代表已进 main、已发布或独立 PASS。
- 普通测试、Race、Migration、离线示例、交叉编译和目标平台运行分别报告，SKIP 和未运行不能算 PASS。
- 模型原因解释、置信度、未验证摘要不自动成为事实；未知、冲突和过期状态保持可见。
- 历史 Node 代码与报告只作输入，不能替代生产 Go 或本轮实测。
