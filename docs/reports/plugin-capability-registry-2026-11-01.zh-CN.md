# Capability Registry、MCP 按需生命周期和模板（中文摘要）

英文原文：[Plugin capability registry](plugin-capability-registry-2026-11-01.md)

## 目标

让 Runtime 能准确回答“当前 Agent 可以使用哪些工具”，并避免 Engine 打开时启动所有 MCP Server。

## 主要实现

- Capability Registry 汇总内置工具、LSP 和 MCP 工具。
- 每项能力包含来源、权限、可用状态、执行约束和配置要求。
- MCP Server 在需要工具目录或执行调用时启动，空闲后关闭。
- Server 有并发、超时、输出大小和错误状态限制。
- 项目配置受工作区信任规则约束。
- 模板是配置数据，不会自动授予额外权限。

## 实现原理

能力目录和进程生命周期分开：Registry 描述“有什么”，MCP Manager 管理“何时启动和停止”，统一权限系统决定“能不能调用”。

性能数字只代表报告中的环境和样本，不是固定保证。
