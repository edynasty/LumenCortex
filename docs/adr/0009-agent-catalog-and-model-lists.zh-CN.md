# ADR-0009：Agent 目录和每个 Agent 的模型候选列表

## 决策
Core 维护统一 Agent Catalog，配置位于全局 `lumencortex.json` 的 `agents` 字段。每个 Agent 定义指令、用途、有顺序的模型候选、默认推理强度和权限上限。

内置 Agent 的身份和权限上限固定；用户按规则调整指令、推理强度和模型列表。自定义 Agent 受工作区信任规则约束。

## 模型选择
Agent 自己的模型列表就是故障切换链。Session 固定所选 Agent 和模型；后续候选只在允许的故障条件下尝试。修改列表不会悄悄改变当前模型固定值。

子 Agent 的有效权限不能超过 Host 授权和 Agent 自身上限，取更严格者。

## 实现原理
配置加载时检查 Schema、名称、模型存在性和权限。每个 Agent 带有定义摘要和来源。旧 `subagentModel` 配置通过明确迁移替换，避免两套配置语义并存。
