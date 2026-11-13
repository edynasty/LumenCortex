# Go Runtime 与迁移历史

Go 是 LumenCortex 唯一正式生产 Runtime。历史 Node.js 代码和测试只用于追溯旧设计，不构成第二套生产实现，也不能作为 Go 功能验收证据。

## 为什么统一到 Go

- 统一 Agent 执行和状态管理。
- 减少两套 Runtime 的行为差异。
- 让权限、持久化、取消和恢复由同一套机制负责。
- 避免同一功能在 Node 与 Go 各自演进，造成能力口径不一致。

## 迁移规则

迁移必须有明确的 Go 实现、对应测试和调用路径。旧 Node 实现可以作为行为参考，但不能只复制代码或测试名称就认定迁移完成。

数据迁移必须保留原始数据，验证新格式后再归档旧格式。Migration 必须可追踪、按顺序执行，并有失败处理策略。

## 能力判断

- Go main 中的代码才是当前生产基线。
- 未合并 Go 分支是候选，不是 main 已有功能。
- Node 历史功能不计入 Go 能力。
- 交叉编译不等于目标平台原生运行。
- 文档和报告必须写明准确 Commit/Tag。

## Cognitive Core 完整候选

本轮基于 main `00344b5`，整合历史 `91af861` 并补齐认知调用链，尚未合并/发布。Schema 保留 Hub 0013，随后为 cognition 0014、support 0015、原生 Graph 0016、completion 0017。0017 增加应用/提案/决策/私有访问记录；含有新数据或新 Node 语义时拒绝降回旧读取器，空库回滚与故障恢复由现有 Runner 验证。

main 的 Governor 仍是只读基础；候选提供宿主 CAS 生命周期 Apply、普通 Agent 条件经验应用和记忆结算，不启用无准入的模型 Curator 或自主 Epoch。本轮只使用临时 SQLite，未迁移生产数据。详见[Migration 契约](cognitive-core/COGNITIVE_P0_STORAGE_DECISION.md)与[实测报告](reports/cognitive-core-final-completion-2026-11-13.md)。
