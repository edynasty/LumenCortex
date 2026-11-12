# Agent Working Notebook 实施记录（中文摘要）

英文原文：[Notebook slice](notebook-slice-20261101.md)

## 目标
为长任务提供可持续更新的工作笔记，保存计划、发现、实验、失败、验证和交接内容。

## 主要实现
- Notebook 存在现有 Session Store。
- 正文按需读取，不全部注入模型上下文。
- 更新使用版本号防止并发覆盖。
- 限制每个 Session 的 Notebook 数量、正文大小和历史版本。
- 子 Agent 使用自己的 Notebook，父 Agent 通过读取后整理结果。

## 重要边界
Notebook 是辅助工作记忆，不是 Task State，不能决定任务是否完成，也不能把检查标为通过。
