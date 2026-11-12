# 持久化 Python / JavaScript Eval

Eval 允许 Agent 在同一个 Python 或 JavaScript 内核中连续执行多段代码。变量和对象可以在同一内核的多个 Cell 之间保留，不需要每次重新启动解释器。

它复用现有 `evalapi`、`evalruntime.Manager`、Worker、Notebook、工具桥接、Artifact Store 和进程生命周期，不另建解释器或 Session 管理器。必须由 Host 和当前 Run 显式启用。

## 内核和隔离

一个内核属于一个 LC Session 和一个权限域。其他 Agent 获得的跨 Agent 权限只能读取，不代表可以在别人的内核中执行代码。

每次请求都会重新检查权限、内核代数、截止时间和请求身份。

- Reset 会销毁当前内存状态、清理内核目录并增加代数。
- Worker 崩溃后会启动新 Worker，不会假称旧变量还存在。
- Interrupt 只针对当前请求，迟到的取消不能中断后续 Cell。
- Python 支持协作式中断并保留状态。
- JavaScript 被强制中断或超时后可能丢失整个 Worker 状态，结果会明确报告。

## 资源限制

Linux 要求委派的 cgroup v2 子树。Worker 启动前设置内存、Swap、进程数和 CPU 限额；缺少控制器或写入失败就拒绝启动。cgroup.kill 和守护进程负责清理后代进程及 Owner 异常退出后的资源。

macOS 使用 Seatbelt、进程限制和 RSS 监控。它不是 Linux 那种内核硬内存限制或 CPU 配额。

Worker 的文件和网络权限由 Host 决定，Cell 不能自行选择可执行文件、挂载目录或继承宿主环境。

## Artifact 文件

Artifact 保存文件 ID、所属 Session、名称、MIME、受限路径、大小、SHA256 和创建时间。

读取必须经过 Session 权限检查，不能让调用者指定任意宿主路径。Python、JavaScript 之间传递数据时，通过明确的 Artifact 字节完成，不共享对方的内存堆。

大文件应保存在 Artifact 中，不要把全部内容塞进模型上下文。Notebook Viewer 继续使用现有 Artifact Viewer。

## 已有验证和限制

历史证据覆盖 macOS Python 状态保持、JavaScript 数据交换、Reset、权限隔离，以及 Ubuntu 的资源限制、OOM、重启、后代进程清理和科学计算数据传递。

限制：

- macOS 内存监督不是内核硬限制。
- JavaScript 强制中断可能丢失状态。
- Linux 资源隔离还依赖正确配置的 cgroup。
- 某些科学计算依赖在特定 macOS Python 环境中无法加载，不能把失败场景算作通过。
- 历史测试使用确定性本地 Provider，不代表真实模型效果。
