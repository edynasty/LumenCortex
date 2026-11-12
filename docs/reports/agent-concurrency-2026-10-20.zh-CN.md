# Agent 并发配置（中文摘要）

英文原文：[Agent concurrency](agent-concurrency-2026-10-20.md)

## 目标
让用户控制机器可同时运行的 Agent 数量，以及单个父 Agent 可启动的子 Agent 数量。

## 主要决定
- 限制保存在全局配置，不允许项目仓库提高机器资源上限。
- 启动时读取当前限制，降低上限不影响已经运行的 Agent。
- 超限立即拒绝，不建立等待队列。
- Host 可以为嵌入式 Engine 设置更严格或明确的覆盖值。

## 实现原理
RunSupervisor 在创建 Run 或子任务前申请并发 Slot。申请失败时不应留下 Session、Worktree 或半初始化任务。
