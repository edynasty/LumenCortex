# ADR-0022：可观察、可控制的 Agent 执行平台

## 目标
让用户看清 Agent 正在做什么，并能在安全边界暂停、单步、取消或检查执行；同时支持多个 Agent 协作，但不建立第二套 Runtime。

## 决策
- **事件**：持久化事件先写入 Session SQLite，再通知实时订阅者。实时输出有界；持久化历史有游标和分页。事件不是权限或完成证明。
- **Agent 调试**：控制器附着现有 Agent Loop。暂停发生在安全边界，不冻结任意 Goroutine 或系统进程；单步不能重放已完成工具。
- **程序调试**：DAP 使用独立协议传输和 Manager，Adapter 由 Host 配置。表达式执行和 Attach 需要额外授权。
- **Agent Hub**：复用 Subagent、Work Unit、Session 和 Worktree。依赖由 Core 检查；消息保留来源；交接处理租约。
- **Persistent Eval**：复用现有 Worker、权限和进程管理。Linux 不可信执行必须有已验证的资源和身份隔离；缺少证明时拒绝启动。
- **Lifecycle Hook**：扩展现有 Registry，可以拒绝或收窄操作，不能提高权限或伪造验证结果。

## 实现原理
只扩展现有执行权威，不新增第二套 Supervisor、Session Store、Notebook 或任务调度器。事件、工具、Worker 和 Artifact 都必须保留来源、权限和生命周期。

## 当前边界
该 ADR 不代表所有平台和 Desktop 界面都已验收。Linux DAP、cgroup/UID Broker 和原生 Wails 交互必须分别按平台证据判断。
