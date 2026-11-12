# LumenCortex 系统介绍

LumenCortex 是一个面向长时间软件开发任务的认知型 Coding Agent。它有自己的 Go Agent Runtime 和 Harness，并通过 Cognitive Core 提供知识、上下文和经验能力。

它不是某个模型的简单包装，也不是对其他 Agent 产品的直接复制。

## 系统由什么组成？

### Agent Runtime

负责模型请求、工具调用、Session、取消、恢复和任务执行。

### Agent Harness

负责工具权限、代码编辑、Shell、调试、Eval、Agent Hub、工作流和任务交付。

### Cognitive Core

负责保存和使用知识、来源、证据、经验及其状态。它的目标是让过去任务中形成的可靠信息能够帮助后续任务。

### Host / Desktop

Host 提供模型、凭据、工作区和系统资源。Desktop 提供用户交互界面。Core API 存在不代表每个 Desktop 版本都已接入。

## 核心原则

- Go 是唯一正式生产 Runtime。
- 模型可以提出判断和经验，不能自行批准。
- Framework 负责权限、验证、持久化、隐私和恢复。
- 任务历史、知识和经验必须保留来源。
- 未知、冲突和过期状态必须明确表达。
- 不能把保存历史或检索内容直接称为学习。

## 当前状态

具体能力以 [Harness 能力清单](harness/capabilities.zh-CN.md) 和 [Cognitive Core 能力现状](cognitive-core/capabilities.zh-CN.md) 为准。未合并候选不能算作 main 已有能力。
