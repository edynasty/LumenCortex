# ADR-0010：基于 Session Store 的 Notes 和历史检索

## 背景
对话窗口会裁掉较早消息；新 Session 也需要找到以前的决定、尝试和结果。

## 决策
- Notes 保存在 Task State，不另建重复的 Context 数据库。
- Notes 可由 Agent 或用户明确写入，并限制长度、数量和引用路径。
- `read_thread` 读取指定 Session 的目标、状态、Notes、未完成任务和结果。
- `recall` 在有限 Session 候选中按目标、Brief、Notes、Resolution 和最终回答检索。
- 引用文件变化或删除时将 Note 标为 stale，不静默隐藏或删除。
- Note 文本不能满足检查、Work Unit 证据或完成条件。

## 实现原理
Session Store 本身就是索引，避免重复存储。Recall 在读取时计算分数，不保存临时排名。删除 Session 后，它自然不再出现在 Recall 结果中。
