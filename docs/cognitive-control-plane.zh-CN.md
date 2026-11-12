# Cognitive Control Plane（认知控制平面）

<badge color="warning">历史设计文档，不代表当前 Go main 的实现状态</badge>

本文保留早期 Node.js 认知控制设计。Owner 已明确 Go 是唯一生产 Runtime；Node.js 只作为历史参考，不是第二套生产实现，也不是必须追平的目标。

当前 Go main 的实际能力以 Go 源码和测试为准。Go Graph Governor 目前提供只读 Analyze / Plan / Validate；模型 Curator 在调用 Provider 前会因缺少请求准入条件而拒绝执行。不能宣称 Go main 已有 Graph Apply 或 Cortex Epoch 执行能力。

## 设计要解决什么问题？

认知控制设计把四种职责分开，避免交给同一个模型随意决定：

1. **判断**：估计任务类别、证据是否充分、是否卡住、下一步需要哪类检索。
2. **框架路由**：由确定性的 Runtime 决定接下来做什么、是否需要更深入思考、使用多少推理预算。
3. **模型链**：按任务类别选择用户配置的生成模型顺序。
4. **长期认知维护**：管理知识图的分支、晋升、归档、整理和版本。

## 目标组件

| 组件 | 作用 |
|---|---|
| Cognitive Kernel / Router | 最终路由、预算、合法性检查和 Think 决定 |
| Decision Layer | 提供有限、结构化的判断信号 |
| Category Resolver | 根据任务类别选择生成模型链 |
| Think | 使用框架选定的模型和推理强度进行深入思考 |
| Graph Governor | 管理长期 Context Graph |
| Cognitive Profile | 配置判断 Provider 和各类别模型链 |

## 核心规则

- 最终执行决定由 Framework 掌握。
- Jev/Laya 只提供判断信号，不执行任务，也不能强制 Runtime 进入 Think。
- Category 配置只保存生成模型链，不负责决定推理强度。
- 模型可以提出判断、策略和知识图修改计划；确定性 Runtime 负责权限、预算、持久化和最终修改。
- 知识图的持久化修改必须保留来源，并能审计。

## Decision Layer

Decision Layer 不是任务执行模型。它回答范围明确的问题，例如：

- 当前任务属于哪类工作？
- 现有证据是否足够？
- 当前策略是否停滞？
- 应该做因果、历史还是依赖关系检索？
- 当前歧义是否很高？
- 观察到的进展是否有意义？

判断结果只是建议。Framework 可以接受、覆盖或忽略它。

系统也可以只使用算法判断，不要求部署 Jev、Laya 或其他判断模型。确定性 Framework 在没有模型判断 Provider 时仍然成立。

## 目标流程

```text
Runtime 状态 + Decision Layer 判断
              -> Cognitive Kernel
              -> 选择任务类别和是否深入思考
              -> 按顺序选择可用生成模型
              -> Agent / Think / Tools 执行
              -> 任务结果和知识状态
              -> 后续认知维护
```

## 当前 Go 边界

上面是历史目标设计，不应直接当作 Go main 已实现的架构。当前 Go 的实际能力和限制请看：

- [Cognitive Core 能力现状](cognitive-core/capabilities.zh-CN.md)
- [Cognitive Core 总体架构](cognitive-core/architecture.zh-CN.md)
- [Harness 能力清单](harness/capabilities.zh-CN.md)
