# Cognitive Core（认知核心）

**Cognitive Core v1.0 整合，2026-11-16。**受保护 main `00344b5`，指定输入 `8c30f75`，独立分支 `integ/cognitive-core-v1-20261116`。未合并、未发布。[整合报告](../reports/FINAL_COGNITIVE_CORE_V1_REPORT.zh-CN.md)绑定工程结果与精确提交。[English](README.md)。

**审查关闭：**已包含 Astra 完成候选、Fable FFR-001–017、Carbon ICRA-001–004、Opus ICRA-V-001–003/V-T1。独立认可来自后续审查者及独立架构审查，不由本轮整合者自签。[Finding Ledger](finding-ledger.md)逐项记录范围和接受限制，不回写历史报告。

Cognitive Core 的目标，是让 Agent 不只完成当前任务，还能利用过去积累的知识和经验，判断信息是否可靠，在发现错误时修正，并把有效经验用于后续任务。

**不要把“存下来了”当成“学会了”，也不要把“检索到了”当成“理解了”。**

## 文档导航

- [总体架构](architecture.zh-CN.md)：七个认知领域分别负责什么，如何配合。
- [能力现状](capabilities.zh-CN.md)：哪些已经进入 Go main，哪些只在候选分支，哪些还没有实现。
- [记忆管理](memory-lifecycle.zh-CN.md)：短期、长期、压缩、频率、保留和删除。
- [v1.1 与认知演进路线](roadmap.zh-CN.md)：后续接入、双记录族、实证和规模工作；不授权 0018 或付费实验。
- [实现原理](COGNITIVE_CORE_IMPLEMENTATION.zh-CN.md)、[公开 API](COGNITIVE_CORE_API.zh-CN.md)、[数据模型](COGNITIVE_CORE_DATA_MODEL.zh-CN.md)、[Migration](COGNITIVE_P0_STORAGE_DECISION.md)与 [Finding Ledger](finding-ledger.md)。
- [ADR-0023](../adr/0023-unified-cognitive-lifecycle.zh-CN.md)：当前生命周期、权限和原操作序列学习的统一决策。
- [v1.0 最终整合](../reports/FINAL_COGNITIVE_CORE_V1_REPORT.zh-CN.md)：工程门禁、平台限制、Migration 与 Git 交付。

## 当前状态

核实的 GitHub main 为 `00344b5`，已有 `208ad6b` Go Harness、Context、Knowledge、Attention Light 和只读 Governor 基础。

本候选补齐范围/时间/证据、正常 SQLite 原生 Graph 与 Experience、普通 Agent 学习工具、证据评估、依赖修订、保留归并及恢复。**候选代码不能算作 main 或 Release 已有能力。**是否合并以提交记录为准。

Experience 是受验程序化宏；原生 belief/negative/abstraction 写入和 Governor 生命周期仍需宿主 SDK 调用。Start 只提示维护候选，不包含 Experience。保留 V-D1 共享 owner 撤回及 V-002/V-003 失败关闭限制。

两记录族共用证据、来源、隐私权威，但生命周期与读取路径不同。Cognitive Core 和 Context Space 保持正交，位于同一 Go Module，仅由 `runtime` 组装。

## 设计原则

1. 生产实现统一使用 Go，不维护第二套 Node.js Runtime。
2. 知识必须保留来源、适用范围和时间信息。
3. 模型可以提出判断和经验，不能自己批准自己的判断。
4. Framework 负责验证、权限、隐私、状态变更和恢复。
5. 信息不足、证据冲突或知识过期时，要明确表示未知或暂不可用。
6. 记忆、经验和推理必须互相配合，不能各自维护矛盾的状态。

## 什么证据能证明能力存在？

- 源码证明功能有实现。
- 测试证明特定行为符合预期。
- 运行记录证明指定环境实际运行过。
- 只有后续任务的行为确实因经验而改善，才支持“经验带来学习效果”的说法。

前三项不能代替第四项。
