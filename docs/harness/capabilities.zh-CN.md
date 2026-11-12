# Agent Harness 能力清单

**基线：Core main `c74ed36`（2026-11-12）。** 下表描述当前 Go main，不把未合并分支或历史 Node 实现算作现有能力。

| 能力 | 当前状态 | 代码位置/依据 | 限制 |
|---|---|---|---|
| Agent Loop 和模型适配 | 已实现 | `runtime/`、`internal/agent/` 和 Provider 包 | 模型效果取决于供应商和模型 |
| Session 保存、恢复和取消 | 已实现 | Session Store、Supervisor 和生命周期测试 | 不会自动回滚所有外部副作用 |
| Work Unit 和 Workflow Contract | 基础已实现 | `internal/workunit/`、`internal/workflow/` | 交付检查不等于语义正确性证明 |
| 工具权限和工作区信任 | 已实现 | Tool Policy、Host 边界和安全测试 | 不是每次调用都必须人工批准；Worktree 隔离也不是所有任务的强制要求 |
| Shell 和进程管理 | 已实现 | Managed Shell、Jobs 和进程生命周期 | 任意 Shell 修改不能保证整次任务回滚 |
| MCP 和 Skills | 已实现 | MCP Registry、模板和 Skills Registry | 浏览器/电脑操作依赖外部集成 |
| LSP 导航、诊断和受控 Rename | 基础已实现 | `internal/lsp/` 和编辑工具 | 不宣称支持全部 Go Code Action 和资源操作 |
| DAP 程序调试 | Core main 已实现 | `internal/debugger/`、`runtime/dap_debugger.go` | 真实 Adapter 和平台验收仍有门槛；Desktop 界面另算 |
| Agent 执行边界调试 | Core main 已实现 | `internal/agent/debug_control.go` | 调试 Agent 的暂停/继续，不等于调试目标程序 |
| 持久化 Python / JavaScript Eval | Core main 已实现 | `evalapi`、`evalruntime`、`runtime/eval.go` | 需显式启用；不同平台的隔离能力不同 |
| Agent Hub | 基础已实现 | `runtime/hub.go`、Hub/Session API | Desktop 界面和发布版本需单独确认 |
| 统一编辑引擎 | Core main 已实现 | `internal/editing/`、`runtime/editing.go` | 写入必须经过版本检查和 CAS |
| Hashline | 基础已实现 | 编辑工具、版本快照和行锚点 | 用于定位和检查前置条件，不保证修改语义正确 |
| Go AST 编辑 | 已实现，仅支持 Go | `internal/editing/goedit/` | 不代表支持 TypeScript、Python 等语言的 AST 编辑 |
| 多语言 AST 编辑 | 未实现为 Go 生产能力 | 仅有历史 Node/研究参考 | 有对应 LSP 能力时优先使用 LSP |
| Browser / 视觉 / 电脑操作 | 外部集成方向 | MCP 模板和 Host 集成 | 不是 Core 内置运行时 |
| Desktop 原生界面 | 独立产品范围 | Desktop 仓库和发布证据 | Core API 存在不代表 Desktop 已接入 |

## 如何判断 Harness Evolution 状态

2026-11-10 的旧审计比较了 Core main、当时依赖旧 Core Tag 的 Desktop，以及尚未合并的执行平台分支。它是历史快照，不能直接当作当前能力表。

Harness Evolution 的本地候选、测试通过和 Desktop 联调，都不等于已经合并、打 Tag 或发布。必须核对准确 Commit、合并记录、Core Tag 和 Desktop 的 `go.mod`。
