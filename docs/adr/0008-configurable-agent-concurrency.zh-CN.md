# ADR-0008：Agent 并发数可配置

## 决策
并发限制由 Core 全局配置管理，不允许项目仓库自行提高。

- 配置保存在全局 `lumencortex.json`。
- 分别限制机器上的 Agent 总数和每个父 Agent 的子 Agent 数量。
- 每次启动 Run 或创建子任务时读取当前配置，不需要重启 Engine。
- 调低上限不会中止运行中的 Agent，只拒绝新的启动。
- 超限立即返回错误，不排队。
- Host 显式设置的 Engine Budget 可以覆盖全局配置。

## 实现原理
RunSupervisor 在启动前申请并发 Slot；成功后才创建 Run/子任务。拒绝时不能留下 Session、Worktree 或半初始化任务。立即拒绝避免引入额外排队状态、取消和资源占用。
