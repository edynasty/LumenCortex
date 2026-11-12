# Agent Presets 和内置子 Agent Prompt（中文摘要）

英文原文：[Agent presets](agent-presets-2026-10-22.md)

## 目标
定义 LC 内置子 Agent 的提示词、权限边界和模型候选配置，借鉴外部 Agent 工具的有效做法，但保持 LC 自己的执行和权限体系。

## 主要决定
- Explore 是只读调查 Agent，不得写文件、执行命令或继续创建子 Agent。
- Worker 是有明确目标和工作区边界的执行 Agent。
- Reviewer、Security Reviewer、Sonic、Thread Historian 等是普通 Agent Catalog 项。
- 内置 Agent 的身份和权限上限由 Core 固定，用户只能按规定调整指令、推理强度和模型列表。
- 外部模型角色配置只在迁移时转换，不在 Runtime 中保留第二套角色系统。

## 实现原理
Core 将 Agent 定义合并为 Catalog，检查模型列表、工具能力和权限上限。子 Agent 启动时选择允许的 Agent，并继承 Host 授权的上限。
