# Context 连续性：Notes、read_thread 和 recall（中文摘要）

英文原文：[Context continuity](context-continuity-2026-10-21.md)

## 要解决的问题

上下文窗口裁剪后，Agent 可能丢失早期任务中的决定、失败尝试和关键数字。新 Session 也无法直接读取旧 Session 的工作结果。

## 设计选择

- 在 Task State 中保存明确 Notes，不建立重复的 Context 数据库。
- 使用 read_thread 读取指定 Session 的目标、状态、Notes、工作单元和最终结果。
- 使用 recall 在有限候选 Session 中按目标、Brief、Notes、Resolution 和最终回答检索。
- 对引用文件变化或删除的 Note 标记 stale。
- Notes 不能满足检查或任务完成条件。

## 评估结果边界

报告记录了跨 Session 连续性实验，支持保留 Recall 默认能力；它只证明该实验任务中跨线程读取路径有价值，不证明所有检索场景都最优。

当前行为见 [Cognitive Core 记忆管理](../cognitive-core/memory-lifecycle.zh-CN.md) 和 ADR-0010。
