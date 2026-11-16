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

Cognitive Core v1.0 在 `integ/cognitive-core-v1-20261116` 独立整合 `8c30f75`，受保护 main 仍为 `00344b5`，未合并、未发布。Schema 保留 Hub 0013，随后为 cognition 0014、support 0015、原生 Graph 0016、completion 0017；不新增 0018。0017 增加应用/提案/决策/私有访问记录。新语义的降级保护在 SQL 事务内拒绝并回滚，此前可能已创建备份；不能与 Schema 家族不符时“修改前拒绝”混为一谈。只验证临时库，不转换未发布早期候选数据。

main 的 Governor 仍是只读基础；候选提供宿主 CAS 生命周期 Apply、普通 Agent 条件经验应用和记忆结算，不启用无准入的模型 Curator 或自主 Epoch。本轮只使用临时 SQLite，未迁移生产数据。详见[Migration 验证](reports/COGNITIVE_CORE_V1_MIGRATION.md)与[整合报告](reports/FINAL_COGNITIVE_CORE_V1_REPORT.zh-CN.md)。
