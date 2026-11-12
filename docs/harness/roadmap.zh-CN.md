# Agent Harness 后续改进方向

这份文档说明后续方向，不代表所有项目都已批准实施。

## 保持现有架构

- Go 是唯一生产 Runtime。
- 保留一个 Agent Loop 和统一执行权限。
- 编辑、DAP、Eval、Hub 和进程管理各司其职，共用必要的权限和生命周期机制。
- 浏览器、电脑操作等外部能力优先通过 MCP/插件接入。
- 不为了追赶 OMP 或 Claude Code 而照搬功能，必须先确认是否符合 LC 的目标。

## 当前基础

Core main 已有 Agent 执行、Workflow Contract、Work Unit、权限检查、MCP/LSP、Managed Shell、DAP、持久化 Eval、Hub 任务控制和统一编辑引擎。

具体状态见[能力清单](capabilities.zh-CN.md)。

## 后续方向

| 方向 | 为什么需要 | 下一步边界 |
|---|---|---|
| Core 正式版本和 Desktop 集成 | Core 的能力必须进入实际 Desktop 产品 | Core 通过发布门槛后，Desktop 才升级到明确的 SemVer 版本 |
| Linux DAP 和执行隔离 | 调试器和 Worker 依赖真实进程隔离 | 在支持的 Linux 主机上验证真实 Adapter、cgroup/UID Broker 和 bwrap |
| Desktop Hub/Debugger/Eval 界面 | Core API 不能代替可用的交互界面 | 使用准确发布版本完成 Wails 实机验收 |
| 多语言结构化编辑 | 不同语言的语法和类型规则不同 | 先使用已有 LSP 能力，不宣称 Go AST 能编辑所有语言 |
| Shell 回滚和工作区恢复 | Shell 可以产生文件编辑事务之外的副作用 | 单独设计恢复机制，不承诺无法覆盖的全量回滚 |
| 运行观察和成本统计 | 长任务需要解释资源和 Token 消耗 | 先定义准确的统计口径和来源 |
| Browser/视觉/电脑操作 | 属于外部环境能力 | 通过 MCP/插件和 Host 权限接入，不另建浏览器 Runtime |
| 进程管理统一 | DAP、Eval、Jobs、Hooks 都会启动进程 | 复用现有进程管理机制，不把不同能力硬合成一个模块 |
| 编辑安全 | 多个 Agent 可能同时修改文件 | 保留版本、租约、CAS、校验和写后检查 |

## 暂不作为目标

- 公共插件市场。
- 第二套 Node.js 生产 Runtime。
- 第二个 Agent Loop 或后台认知协调器。
- 没有语言级语义实现的通用 AST 编辑。
- 自动把 Worker Worktree 合并到 main。
- 为了功能对齐而照搬竞品设计。
