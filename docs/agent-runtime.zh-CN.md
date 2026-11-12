# Agent Runtime（Agent 运行时）

Agent Runtime 负责一次任务从启动到结束的全过程：准备上下文、请求模型、执行工具、保存结果、处理取消和恢复。

## 一次 Run 如何工作

```text
任务目标
  -> 可选 Workflow Contract
  -> 检索项目知识和代码
  -> Attention Light 选择相关上下文
  -> 把有限上下文发送给模型
  -> 检查工具权限
  -> 执行文件、Shell、LSP、MCP 或子 Agent 工具
  -> 保存工具结果并更新索引
  -> 进入下一轮模型请求
  -> 完成或按原因中断
```

完整 Session 历史保存在磁盘。模型每轮只接收当前选中的子图和有限数量的近期完整对话/工具轮次，不会每次加载全部历史。

## 步数、取消和超时

- 默认没有隐式步数上限。
- `MaxSteps=0` 表示未指定；`-1` 表示不限制；正数由 Host 设置上限。
- 一轮模型响应和工具执行算一个 Step，HTTP 重试不额外消耗 Step。
- 达到步数上限时，状态为可恢复的 `interrupted/max_steps`，不能算成功。
- Host 取消、调用方取消和显式 Deadline 会传递到准备阶段、模型请求、重试等待、工具和受管理的子任务。
- Run 结束前会处理自己拥有的子任务和后台 Job。

## 工具与权限

内置文件工具包括读取、批量读取、目录列表、文本搜索、代码搜索、写入、替换和补丁。Shell 通过受管理的进程运行，支持超时、取消和后台 Job。

LSP、MCP 工具只有在对应服务可用且当前权限允许时才会提供。所有工具都经过同一个权限门禁。

权限策略：

- `read-only`：读取工具和当前 Run 自己的状态工具。
- `workspace`：读取和工作区写入，不允许 Host Shell 或外部 MCP 写入。
- `full`：允许已配置的读取、写入和执行工具。

## Working Notebook

Run 可以保存自己的 Markdown 工作笔记，用于计划、检查清单、发现、实验、决定和交接。笔记是辅助工作记忆，不是 Task State；不能靠笔记声明任务完成，也不能把检查标记为通过。

笔记有版本控制和大小上限。冲突时必须重新读取，不允许覆盖其他版本。

## Session 和恢复

Session 保存消息、工具轮次、上下文历史、模型信息、Token/请求用量、状态和最终输出。

恢复时沿用原 Session 的历史和累计信息，但会创建新的 Run 身份。取消和失败原因会持久化；不能把中断伪装成完成。

## 任务交付检查

Host 可以提供明确的交付合同，指定文件、分支或提交要求。Core 在 Agent 执行结束后独立检查交付证据。

交付检查不是 Agent Loop 的完成门禁，也不等于语义正确性证明。执行状态与交付状态分开记录。只有 `verified` 才表示合同要求已被检查满足。

## 数据存储

默认使用工作区状态目录中的 SQLite 数据库，保存 Session、消息、步骤、知识图、事件、索引和笔记。SQLite WAL 支持独立读取者。

## 相关文档

- [执行流程](execution-flow.zh-CN.md)
- [Workflow Contract](workflow-contract.zh-CN.md)
- [Provider 配置和重试](go-provider.zh-CN.md)
- [安全与信任](security-and-trust.zh-CN.md)
- [Agent Hub](agent-hub.zh-CN.md)
