# 文档目录

LumenCortex 是 Go 实现的认知型编程 Agent。Go 是唯一正式生产 Runtime；历史 Node.js 代码只用于理解旧设计，不能算作当前生产能力。

## 从这里开始

- [系统介绍](system-overview.md)
- [总体架构](architecture.md)
- [架构图](architecture-diagrams.md)
- [Agent 执行流程](execution-flow.md)
- [Agent Runtime](agent-runtime.md)
- [Go Runtime 和迁移历史](go-runtime-migration.md)

## Agent Harness（执行框架）

- [Harness 总览（中文）](harness/README.zh-CN.md) · [English](harness/README.md)
- [Harness 能力清单（中文）](harness/capabilities.zh-CN.md) · [English](harness/capabilities.md)
- [Harness 后续方向（中文）](harness/roadmap.zh-CN.md) · [English](harness/roadmap.md)
- [Hashline 和 AST 编辑](reports/harness-evolution-editing-20261110.md)
- [DAP 程序调试](dap-debugger.md)
- [持久化 Eval](persistent-eval.md)
- [Agent Hub](agent-hub.md)
- [Workflow Contract](workflow-contract.md)
- [安全与信任](security-and-trust.md)

## Cognitive Core（认知核心）

- [Cognitive Core 总览（中文）](cognitive-core/README.zh-CN.md) · [English](cognitive-core/README.md)
- [总体架构（中文）](cognitive-core/architecture.zh-CN.md) · [English](cognitive-core/architecture.md)
- [能力现状（中文）](cognitive-core/capabilities.zh-CN.md) · [English](cognitive-core/capabilities.md)
- [记忆管理（中文）](cognitive-core/memory-lifecycle.zh-CN.md) · [English](cognitive-core/memory-lifecycle.md)
- [后续工作（中文）](cognitive-core/roadmap.zh-CN.md) · [English](cognitive-core/roadmap.md)
- [认知控制平面](cognitive-control-plane.md)
- [Attention Light 算法](attention-light-algorithm.md)
- [数据模型](data-model.md)
- [架构决策记录](adr/README.md)

## 开发与验证

- [独立运行状态和已知限制](standalone-readiness.md)
- [验证记录](validation.md)
- [Go Provider 配置](go-provider.md)
- [Plugin 和 MCP 模板](plugins-mcp-templates.md)

## 当前主要限制

以下能力不能仅凭设计文档或候选分支宣称已经交付：

1. **Shell 回滚**：本地 Session 中任意 Shell 命令造成的外部修改，不能保证整次任务回滚。
2. **强制隔离**：Worktree 隔离可以使用，但不是每次 Agent Run 都强制启用。
3. **视觉和浏览器自动化**：不是 Core 内置能力。
4. **TUI 交互**：当前 Go CLI 不是完整 IDE。
5. **模型校准**：真实模型路由和长期效果仍需单独确认。
6. **认知治理**：Go main 的 Governor 只提供 Analyze / Plan / Validate；不宣称自动执行全部知识修改。
7. **物理存储分层和垃圾回收**：已有冷热标记不代表已经建立独立物理存储。
8. **Linux 和 Desktop 发布门槛**：真实 Linux DAP、主机/VM cgroup 与 UID Broker、Wails 原生交互需要各自的验证。
9. **扩展 Cognitive Core**：Scope / Time / Evidence 和 Experience Learning 的扩展实现曾在独立候选分支开发，未合并前不能算作 main 已有能力。

历史 Node.js 功能只能作为历史资料，不能写成当前 Go 生产能力。
