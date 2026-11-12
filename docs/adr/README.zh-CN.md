# 架构决策记录（ADR）

ADR 用来记录重要架构决定、选择原因和长期约束。英文文件是原始决策记录；本目录的 .zh-CN.md 文件提供中文说明。若中英文有差异，以英文原文和后续正式修订为准。

## 状态含义
- **Proposed**：提议中，尚未正式接受。
- **Accepted**：已接受。
- **Superseded**：已被后续决策取代。
- **Rejected**：明确不采用。

## 决策索引

| 编号 | 决策 | 中文说明 |
|---|---|---|
| 0001 | Go Runtime 迁移 | [中文](0001-go-runtime-migration.zh-CN.md) |
| 0002 | Runtime 内存上限 | [中文](0002-bounded-memory-runtime.zh-CN.md) |
| 0003 | 认知控制平面 | [中文](0003-adaptive-cognitive-control-plane.zh-CN.md) |
| 0004 | M2.3 模型 Runtime | [中文](0004-m23-model-runtime-decisions.zh-CN.md) |
| 0005 | Session Worktree 所有权 | [中文](0005-session-worktree-ownership-and-deletion.zh-CN.md) |
| 0006 | 任务上下文和执行状态 | [中文](0006-task-context-resolution-and-conversational-execution.zh-CN.md) |
| 0007 | 认知层现状 | [中文](0007-cognitive-layer-status.zh-CN.md) |
| 0008 | Agent 并发配置 | [中文](0008-configurable-agent-concurrency.zh-CN.md) |
| 0009 | Agent 目录和模型列表 | [中文](0009-agent-catalog-and-model-lists.zh-CN.md) |
| 0010 | Notes 和历史检索 | [中文](0010-thread-notes-and-recall.zh-CN.md) |
| 0011 | OMP Session 导入 | [中文](0011-omp-session-import.zh-CN.md) |
| 0012 | Core Plugin 平台 | [中文](0012-core-plugin-platform.zh-CN.md) |
| 0013 | 项目输入和工作区信任 | [中文](0013-project-inputs-and-workspace-trust.zh-CN.md) |
| 0014 | 单次工具调用额外批准 | [中文](0014-per-call-approval-above-the-ceiling.zh-CN.md) |
| 0015 | Skill 发现和加载 | [中文](0015-skill-discovery-and-catalog-loading.zh-CN.md) |
| 0016 | 外部状态和工作区绑定 | [中文](0016-external-state-and-workspace-binding.zh-CN.md) |
| 0017 | 获准检查和任务经历 | [中文](0017-approved-checks-and-closed-episodes.zh-CN.md) |
| 0018 | 请求来源和 Fence | [中文](0018-request-provenance-and-fences.zh-CN.md) |
| 0019 | 执行 Runtime | [中文](0019-execution-runtime.zh-CN.md) |
| 0020 | Agent 工作笔记本 | [中文](0020-agent-notebook.zh-CN.md) |
| 0021 | Capability Registry 和 MCP 生命周期 | [中文](0021-capability-registry-and-mcp-lifecycle.zh-CN.md) |
| 0022 | 可观察、可控制的执行平台 | [中文](0022-cognitive-execution-platform.zh-CN.md) |

模板：[ADR 中文模板](0000-template.zh-CN.md)。新增 ADR 使用下一个编号；取代旧决策时必须双向链接。普通 Bug 修复不需要 ADR。
