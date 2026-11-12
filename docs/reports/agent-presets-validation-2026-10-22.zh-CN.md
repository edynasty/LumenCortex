# Agent Presets 真实模型验证（中文摘要）

英文原文：[Agent presets validation](agent-presets-validation-2026-10-22.md)

## 验证目标
检查内置 Reviewer、Security Reviewer、Sonic、Thread Historian、Worker 和 Explore 的实际行为，以及只读权限和模型候选回退。

## 验证方法
报告按场景记录模型实际响应、提示词调整、权限边界和执行结果。模型验证与确定性 Runtime 测试分开记录。

## 重点边界
- 提示词符合预期不等于权限安全，权限仍由 Core 执行。
- 只读 Agent 必须在工具注册/调用层拒绝写入，而不是只依赖 Prompt。
- 模型候选回退必须遵守配置顺序和工具能力过滤。
- 环境失败和模型拒绝不能计为通过。

详细场景和原始结果见英文原文。
