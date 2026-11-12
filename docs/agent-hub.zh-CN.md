# Agent Hub

Agent Hub 用来管理多个 Agent 之间的任务分工、依赖、消息和交接。它复用现有 RunSupervisor、Task State、Work Unit DAG 和 Worktree 所有权，不另建后台调度器，也不会自动把 Worker 分支合并到 main。

## 任务图

Host 可以通过 `Engine.ConfigureHub` 为一个空闲的 Root Session 创建任务图。每个任务包含 ID、目标、依赖和可选 Agent。Core 会检查依赖图，调用方不能直接伪造任务完成或验证通过。

`RunSupervisor.Hub` 根据持久化 Session、Work Unit、验证结果和来源隔离状态生成任务视图。

`StartProposedTask` 只允许启动依赖已满足的任务，并使用现有资源和并发限制。需要写文件的 Worker 使用独立 Worktree。重启子任务时也会重新检查依赖。

## Worker 消息

- Worker 只能向同一 Root 下的成员发送消息。
- 相同消息 ID 和相同内容可以安全重试；相同 ID 搭配不同内容会被拒绝。
- Root 可以查看消息时间线；Agent 只能读取发给自己的收件箱。
- 确认消息只表示已读，不代表任务已执行或验证通过。
- 消息不能直接修改 Task State，也不能授予权限。

消息保存在现有 Session SQLite 数据库中。正文有大小和数量上限，超过保留容量时拒绝新消息，不会静默删除尚未读取的内容。被隔离的来源只返回撤回提示，不返回正文；隐私删除会清理涉及被删除成员的消息。

## 控制与结果

Hub Worker 使用现有 Agent 安全边界调试能力。暂停只影响指定 Worker；取消使用 CancelSubtree，不会取消兄弟任务。

任务完成状态来自实际执行、必要检查和 Work Unit 结果，不由模型自行声明。

Handoff 会停止源 Run，把任务租约转移给目标 Session，并释放源 Run 持有的共享工作区写租约。Supervisor 关闭时会等待正在进行的 Handoff；如果目标启动失败，会把租约退还给源。

## Desktop 边界

Desktop Inspector 可以展示任务依赖、Worker、消息和调试控制，但任务准入、资源预算和验证仍由 Core 决定。Core 有 Hub API 不代表每个 Desktop 版本都已经集成对应界面。

## 验证依据与限制

历史证据位于工作区 `bench/evidence/cognitive-agent-execution-platform-20261108/raw/`，覆盖并发 Worker、依赖门禁、幂等、成员权限、重启、隔离、消息和真实 Desktop 后端联调。测试使用本地确定性 Provider，不代表真实模型任务质量。原生 Wails 交互验收仍需单独确认。
