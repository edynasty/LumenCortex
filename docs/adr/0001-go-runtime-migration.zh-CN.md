# ADR-0001：将 LumenCortex Runtime 逐步迁移到 Go

## 背景
早期 Runtime 使用 Node.js。两套 Runtime 并行会导致权限、持久化、执行和恢复行为不一致。

## 决策
生产 Runtime 统一使用 Go，按阶段迁移：
1. Runtime Kernel、事件流和资源统计；
2. 兼容现有数据库的 SQLite Session 访问；
3. 流式 Shell、工具和模型响应；
4. Workflow Contract 与 Agent Loop；
5. Cognitive 检索、注意力和知识图；
6. LSP、MCP、Subagent 和 TUI。

## 实现原理
每个阶段都要有 Go 实现、真实调用路径和测试。数据迁移必须兼容并可恢复。Node.js 代码只作为历史行为参考，不能计入 Go 能力，也不能成为第二套生产 Runtime。

## 影响
统一权限、Session、取消、恢复和资源管理，降低双 Runtime 的维护成本。具体阶段验收见 [英文原文](0001-go-runtime-migration.md)。
