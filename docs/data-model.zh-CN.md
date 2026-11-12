# 数据模型与持久化

Core 使用 SQLite 保存 Agent 执行状态和认知数据。数据库位于工作区状态目录，不应与项目源码混在一起。

## 主要数据

| 数据 | 用途 |
|---|---|
| Session | 一次持续对话/任务的身份和状态 |
| Session Messages | 模型消息和工具结果 |
| Agent Steps | 每轮执行记录 |
| Runtime Events | 生命周期事件和查询游标 |
| Graph Nodes / Edges | 知识节点和关系 |
| Cognitive Commits / Refs | 认知图版本和引用 |
| Search Documents / Symbols | 文本和代码符号索引 |
| Journal | 需要持久化的运行记录 |
| Jobs | 后台命令状态和输出位置 |
| Notebooks | Run 自己的工作笔记 |

## 一致性

关键状态变更使用 SQLite 事务或 CAS（比较并交换）保护，避免并发 Run 覆盖较新的状态。

WAL 允许读取者并行访问数据库。迁移由统一 Migration Registry 管理，不允许不同模块各自维护一套 Schema 版本。

## 历史和恢复

Session 历史使用增量写入，不会每个 Step 都删除并重写全部消息。Core 重启后可以读取已持久化历史，但不能把没有保存的进程内状态假装恢复。

## 数据边界

- 读取必须检查 Session 和来源权限。
- 删除必须清理关联数据和派生数据。
- 事件通知不是权限凭证。
- Artifact 字节通过所属 Session 的授权 API 读取。
- 任何缓存或索引都不能成为绕过权威数据库状态的第二数据源。
