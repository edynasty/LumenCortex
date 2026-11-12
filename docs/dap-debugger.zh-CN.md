# DAP 程序调试

DAP 调试的是 Agent 正在开发或运行的目标程序；暂停 Agent 自身执行应使用 [Agent 执行调试器](agent-debugger.zh-CN.md)。

生产 DAP 通信实现位于 `internal/debugger/dap`。

## 通信和进程管理

- 使用严格的 CRLF + Content-Length 消息格式。
- Header 最大 8 KiB，单条消息最大 4 MiB。
- 同时最多处理 64 个未完成请求。
- 响应必须与请求的命令和序号对应。
- 普通输出事件进入有界队列；关键事件无法入队时关闭连接，不能静默丢失调试状态。
- 取消、连接断开、Adapter 退出或 Owner 关闭时，会清理受管理的进程。

Linux 管理器要求 cgroup 隔离。只有进程组、没有 cgroup 的环境不能宣称已完整清理脱离父进程的后代。

## 沙箱

DAP 复用 Eval 的程序启动和权限配置，不另建解释器或 RPC 系统。Host 明确指定 Adapter、工作区、私有目录和环境变量；默认不继承宿主凭据。

Linux 程序调试必须先验证调用方隔离；无法确认隔离时拒绝启动。

macOS 仅为目标程序调试授予受限的调试权限，普通 Eval Worker 不会获得这些权限。调试进程只能向自己的沙箱进程发送信号，不能借此控制用户的其他进程。

## 停止与会话结束

Stack Frame、Scope 和 Variable 引用都绑定创建它们的 Stop Epoch。程序再次暂停后，旧引用会被拒绝，避免把过期变量编号发给 Adapter。

Adapter 自行退出时记录 `debug.session.lost`；Host 主动断开、撤销权限、Run 结束或 Manager 关闭时记录 `debug.session.stopped`。

## 已有验证及限制

历史证据覆盖 LLDB、Delve、debugpy、Node Adapter、请求乱序、消息格式错误、取消、进程崩溃和 Desktop 后端联调。

这些记录不能替代当前目标平台的实机验收。原生 Wails 交互和 Linux Adapter 验收必须分别确认。

## IntelliJ MCP

IntelliJ 通过显式配置的 Host-only MCP Server 接入，不允许模型任意控制 IDE。服务必须绑定指定工作区；Core 复用现有 MCP Registry，并限制会话、断点、状态读取和清理。IDE 进程使用 IDE 自身的 Host 权限，不属于 DAP 沙箱。

Host 配置从绝对路径读取；相对路径、未知字段和超大配置会被拒绝。
