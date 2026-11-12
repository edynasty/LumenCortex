# 执行步数与故障恢复审查（中文摘要）

英文原文：[Execution retry review](execution-retry-review-2026-11-03.md)

## 审查重点
检查 Agent Step、Provider 重试、Run Deadline、子任务上限、后台 Job 和取消/恢复之间的关系。

## 核心结论
- Step 上限、请求重试和工具执行必须使用不同计数口径。
- 取消和 Deadline 必须传递到实际执行的工具与子进程。
- 不能把重试成功当成新的任务步骤。
- 子任务和后台 Job 的归属必须明确，Run 结束前处理自己拥有的工作。
- 无法确认外部操作结果时保留 Unknown，不猜测成功。

## 记录边界
报告包含多个审查轮次和历史检查点；当前行为应以 Go Runtime 文档和最新源码为准。
