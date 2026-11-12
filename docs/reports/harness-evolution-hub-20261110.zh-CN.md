# Harness Evolution：Agent Hub 和控制面（中文摘要）

英文原文：[Harness Evolution — Hub slice](harness-evolution-hub-20261110.md)

## 目标

在不建立第二套 Agent Runtime 的前提下，提供多 Agent 任务依赖、消息、暂停、取消和交接。

## 主要设计

- Hub 复用 RunSupervisor、Task State、Work Unit DAG、Session 和 Worktree。
- Core 根据依赖和真实执行结果决定任务是否可启动。
- Worker 消息绑定 Root、发送者和接收者；消息 ID 幂等。
- 消息只传递信息，不直接修改另一个 Worker 的 Task State 或权限。
- Task Lease 管理任务所有权。
- Handoff 停止源 Run、转移任务租约，并释放源持有的共享写租约。
- 暂停和取消只作用于指定 Worker/子树，不影响兄弟任务。
- Desktop 显示 Hub 状态，但 Core 负责准入、预算和验证。

## 边界

Hub 不提供后台自动合并 Worker Worktree，也不创建第二个调度器。当前接口和限制见 [Agent Hub 中文文档](../agent-hub.zh-CN.md)。
