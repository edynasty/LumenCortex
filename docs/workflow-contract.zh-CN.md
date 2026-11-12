# Workflow Contract（工作流约束）

Workflow Contract 用来明确一项任务要完成什么、允许采取什么行动、经过哪些步骤，以及什么条件满足后才能结束。

它与 Cognitive Graph 分开管理：Graph 保存知识和关系；Workflow 约束当前任务的执行过程。

## Contract 包含什么

- **Facts**：当前任务已确认的事实。
- **Action**：需要完成的动作。
- **Route**：执行步骤和分支。
- **Gate**：必须满足的人工批准或检查条件。

Host 选择并提供 Contract，不能从模型生成的文字中自动推断权限或完成要求。

## 执行规则

Core 根据 Contract 限制当前 Agent 可见的工具和操作。工具结果可以形成持久化事实，满足条件后才允许进入下一步。

如果需要人工批准，Session 会进入 `waiting_gate`，等待明确处理，不会继续消耗模型轮次。

任务不能仅凭模型说“完成了”就结束。必须满足规定的 Action、检查和终态条件。

## 验证和权限

Workflow 只使用已注册、获准的工具和检查。模型不能自行伪造检查结果，也不能通过普通工具调用绕过 Gate。

Workflow 状态与 Cognitive Graph 状态分开保存，避免把任务步骤误当成长期知识。

## 相关资料

- [Agent Runtime](agent-runtime.zh-CN.md)
- [执行流程](execution-flow.zh-CN.md)
- [安全与信任](security-and-trust.zh-CN.md)
