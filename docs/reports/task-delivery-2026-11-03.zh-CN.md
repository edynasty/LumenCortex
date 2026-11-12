# Task Delivery 实施记录（中文摘要）

英文原文：[Task delivery](task-delivery-2026-11-03.md)

## 目标
让 Host 明确声明任务交付要求，并由 Core 独立验证文件、分支和提交状态。

## 主要设计
- Delivery Contract 由 Host 提供，不从模型文字推断。
- 任务开始前保存基线。
- 执行完成后独立检查要求。
- Agent 执行状态和 Delivery 验证状态分开。
- 检查基础设施故障时返回 verification_unavailable，不伪装成通过或失败。
- 验证重试不能重新执行 Agent 任务。

## 边界
交付验证只证明合同检查的内容，不证明代码语义一定正确。
