# 架构图目录

本页汇总 LumenCortex 的主要架构图。静态图片用于阅读，Mermaid 文件用于维护结构。

## 图例

- 深蓝：入口和用户界面
- 靛蓝：Workflow 和权限控制
- 青色：认知检索和注意力
- 绿色：任务执行
- 橙色：Host 和外部集成
- 紫色：持久化存储
- 灰色虚线：计划中或尚未完成

## 系统架构

回答的问题：系统有哪些主要层次，它们如何协作？

图片：`assets/architecture/system-architecture.png`

结构源文件：`diagrams/system-architecture.mmd`

## 认知控制平面

回答的问题：判断信号、框架路由、模型链、Think 和 Graph Governor 在历史目标设计中如何配合？

结构源文件：`diagrams/cognitive-control-plane.en.mmd`

注意：该图反映历史 Node.js 设计基线，不代表当前 Go main 的完整实现。

## Agent 执行流程

回答的问题：从用户提出目标，到任务完成、暂停或恢复，中间发生什么？

图片：`assets/architecture/agent-execution-flow.png`

结构源文件：`diagrams/agent-execution-flow.mmd`

具体执行语义见 [Agent 执行流程](execution-flow.zh-CN.md)。

## Workflow 与 Cognitive Graph

两者解决不同问题：

- Workflow：当前任务允许做什么、哪些条件必须满足。
- Cognition：系统保存了什么、当前应该关注什么。
- Execution：Agent 实际做了什么。

图片：`assets/architecture/workflow-cognitive-dual-plane.png`

结构源文件：`diagrams/workflow-cognitive-dual-plane.mmd`

## 持久化和数据流

回答的问题：进程重启后哪些数据还在，分别保存在哪里？

图片：`assets/architecture/persistence-data-flow.png`

结构源文件：`diagrams/persistence-data-flow.mmd`

## 长任务的上下文限制

Session 和知识图可以持续增长，但每次模型请求只使用有限的相关上下文：

```text
完整历史
  -> 检索候选
  -> Attention Light 排序
  -> 有限 Active Subgraph
  -> 有限近期工具轮次
  -> 有限模型输入
```

不需要每轮都把全部历史发送给模型。

## 图与实现状态

架构图可能同时展示已实现和计划中的内容。图中出现一个组件，不代表它已经在 Go main 可用。

当前实现状态以 [独立运行状态](standalone-readiness.zh-CN.md) 和 [能力清单](harness/capabilities.zh-CN.md) 为准。
