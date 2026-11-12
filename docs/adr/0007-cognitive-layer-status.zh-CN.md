# ADR-0007：认知层现状和发展路径

## 背景
早期 Go Runtime 没有自己的 Context Graph，Governor 依赖 Node 数据表，因此不能在 Go 数据库上工作。

## 决策
- 没有 Go 原生 Context Store 时，Governor 必须明确不可用，不能假装能操作。
- 认知图从现有 Context Index 逐步发展，先使用 Thread 引用、已解决任务和用户确认的 Notes。
- 每条知识必须有来源和新鲜度规则；模型陈述本身不是证据。
- 代码知识使用 `path@blob` 或符号引用，读取时解析当前文件，不复制整个仓库。
- 被丢弃 Worktree 的决策和失败经验保留为历史；只依赖已丢弃分支代码的事实应失效。
- 更复杂的图传播能力应由跨 Session 连续性证据决定是否引入。

## 当前状态说明
本 ADR 是 2026-10-15 的历史决策。后续 Go Context Store、Attention Light 和只读 Governor 已有演进。当前状态见 [Cognitive Core 能力矩阵](../cognitive-core/capabilities.zh-CN.md)，不能把旧 ADR 的“Governor dormant”当成今天的状态。
