# Harness Evolution：持久化 Eval 与进程管理（中文摘要）

英文原文：[Harness Evolution — Eval slice](harness-evolution-eval-20261110.md)

## 目标

让 Agent 在同一个 Python/JavaScript 内核中连续执行代码，并对 Worker 的资源、权限和生命周期进行统一管理。

## 主要实现

- 复用 Eval API、Manager、Worker、Notebook 和 Artifact Store。
- 每个内核绑定 Session 和权限域。
- 每次执行重新检查身份、代数、Deadline 和请求编号。
- Reset 销毁旧内存状态；崩溃恢复启动新 Worker，不假称旧变量仍存在。
- Python 协作式中断可保留状态；JavaScript 强制中断可能丢失 Worker。
- Linux 使用 cgroup v2 控制内存、CPU、进程数和后代进程。
- macOS 使用 Seatbelt 和资源监控，不能宣称与 Linux 硬限制相同。
- Artifact 通过所属 Session 的授权 API 读取，跨语言传递显式字节，不共享内存堆。

## 验证边界

报告包含 macOS 与 Linux 的资源、重启、OOM、取消和 Artifact 场景。特定 Python 科学计算环境失败的场景被保留为失败证据，不计为通过。

当前能力和限制见 [持久化 Eval 中文文档](../persistent-eval.zh-CN.md)。
