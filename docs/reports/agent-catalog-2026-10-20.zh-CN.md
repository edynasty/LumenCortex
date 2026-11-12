# Agent Catalog 和模型列表（中文摘要）

英文原文：[Agent catalog and model lists](agent-catalog-2026-10-20.md)

## 目标
由 Core 统一管理内置和自定义 Agent，并为每个 Agent 配置模型候选、推理强度和权限上限。

## 主要变化
- 增加 Agent Catalog 配置。
- Agent 有有序模型列表，作为自己的故障切换链。
- 内置 Agent 身份和权限上限固定。
- 自定义 Agent 受工作区信任和配置校验约束。
- 子 Agent 启动时可选择允许的 Agent 类型。
- 旧 subagentModel 配置需要显式迁移。

## 实现原理
配置合并时校验名称、模型、字段和权限；Run 固定 Agent 和首选模型，后续候选仅在允许的故障场景下尝试。Host 权限和 Agent ceiling 取更严格者。
