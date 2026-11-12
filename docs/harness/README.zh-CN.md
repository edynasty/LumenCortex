# Agent Harness（执行框架）

Harness 负责让 Agent 真正执行工作：调用工具、读写代码、运行命令、调试程序、管理任务和处理权限。Cognitive Core 提供知识和经验，Harness 负责把 Agent 的行动安全地执行出来。

**状态基线：Core main `c74ed36`，2026-11-12。** Core main、未合并候选和 Desktop 发布版本必须分开判断。

## 文档入口

- [Harness 能力清单](capabilities.zh-CN.md)
- [后续改进方向](roadmap.zh-CN.md)
- [Agent Runtime](../agent-runtime.md)
- [Agent Hub](../agent-hub.md)
- [Agent 调试器](../agent-debugger.md)
- [DAP 程序调试](../dap-debugger.md)
- [持久化 Eval](../persistent-eval.md)
- [编辑能力演进](../reports/harness-evolution-editing-20261110.md)
- [安全与信任](../security-and-trust.md)

## Core main 已有能力

Go Core main 已有：

- 多轮 Agent 执行和 Session 持久化；
- 工具权限和工作区信任控制；
- Work Unit、Workflow Contract 和交付检查；
- MCP、LSP 和受管理的 Shell；
- Agent 执行边界调试；
- DAP 程序调试；
- 持久化 Python / JavaScript Eval；
- Agent Hub 的任务、消息、租约和交接能力；
- 统一编辑引擎、Hashline 和 Go AST 编辑。

Core 有接口，不代表 Desktop 已经提供对应界面。Desktop 是否可用，要看它依赖的 Core 版本和原生 Wails 验收。

## 明确限制

- Linux 真实 DAP Adapter、cgroup/UID Broker 和 bwrap 隔离仍需对应主机环境验证。
- Desktop 的原生交互界面需要单独验收。
- macOS Eval 的内存限制由监督机制执行，不是内核硬限制。
- JavaScript Worker 被强制中断时，内存中的状态可能丢失。
- AST 编辑目前是 Go 专用能力，不代表支持所有语言。
- LSP 导航和安全 Rename 不代表已经支持所有 Code Action。
- 本地 Shell 命令造成的任意副作用不能保证整次任务回滚。
- Browser、视觉和电脑操作目前通过外部 MCP/插件方向接入，不是 Core 内置浏览器运行时。

## 版本规则

查看功能时必须同时确认 Core 的 Commit/Tag 和 Desktop 的 Core 依赖版本。Core main 有某项能力，不代表仍依赖旧 Tag 的 Desktop 已经拥有它。
