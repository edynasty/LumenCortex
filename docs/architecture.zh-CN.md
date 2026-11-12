# LumenCortex 总体架构

LumenCortex 采用 Go 作为唯一正式生产 Runtime。系统把 Agent 执行、认知能力、Host 权限和 Desktop 界面分开，但通过明确接口协作。

## 主要部分

| 部分 | 负责什么 |
|---|---|
| Agent Runtime | 模型请求、工具调用、Run/Session 生命周期 |
| Harness | 权限、文件编辑、Shell、调试、Eval、Hub 和工作流 |
| Cognitive Core | 知识、证据、经验、来源和认知状态 |
| Host | 模型凭据、工作区、工具和系统资源授权 |
| Desktop | 展示状态、接收用户操作、调用 Core API |
| SQLite | 保存 Session、知识、事件、索引和状态 |

## 数据和权限边界

模型不能直接写入权威认知状态。模型提出内容后，Core 根据来源、范围、时间、证据和权限决定是否接受。

工具执行由 Host/Core 的权限机制控制。Desktop 不能绕过 Core 自己判定任务是否完成或知识是否可信。

## 运行时约束

- 只保留一套 Go Agent Loop。
- 不建立第二套 Node.js 生产 Runtime。
- 不为每种认知职责单独建立数据库。
- 外部能力优先通过 MCP 和 Host 接口接入。
- 候选分支、历史 Node 实现和已发布版本必须分别标注。
