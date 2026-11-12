# ADR-0020：Agent 工作笔记本

## 目标
为长任务保存计划、实验、决定和交接信息，避免上下文窗口裁剪后丢失。

## 决策
- Notebook 属于一个 Session，由该 Session 的 Agent 管理。
- 使用 Markdown 章节保存 Plan、Checklist、Findings、Experiments、Decisions、Failures、Verification 和 Handoff。
- Notebook 不是 Task State，不决定任务状态、完成门禁或检查结果。
- 正文按需读取，不自动把全部内容塞进模型上下文。
- 写入带版本号，防止并发覆盖。
- 限制正文大小、标题长度、每个 Session 的数量和历史版本数量。
- 子 Agent 有自己的 Notebook；父 Agent 读取后再整理到自己的笔记。

## 实现原理
Notebook 保存在现有 Session 数据库。更新使用 expected version 做 CAS；冲突时不写入，必须重新读取。启动时只注入标题、版本、更新时间和章节名，不注入完整正文。
