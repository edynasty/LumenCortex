# Runtime 升级最终整合验收（中文摘要）

英文原文：[Runtime merge acceptance](runtime-merge-acceptance-2026-11-01.md)

## 报告内容
记录 Runtime 升级的提交关系、合并后行为、工具暴露、Browser/MCP 生命周期、独立审查、SQLite 兼容、Desktop 依赖和最终验证。

## 关键原则
- 合并前核对提交祖先和工作树状态。
- 合并后重新检查 Migration、API 和行为。
- Desktop 版本必须与 Core 发布版本明确对应。
- PASS、PARTIAL 和 NOT_RUN 分开报告。
- 本地合并、Push、Tag 和 Release 是不同动作。

## 边界
这是 2026-11-01 的历史验收记录，不能代表当前 main 的最新状态。
