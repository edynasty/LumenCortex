# Delivery Contract 最终验收（中文摘要）

英文原文：[Task delivery final acceptance](task-delivery-final-acceptance-2026-11-04.md)

## 验收重点
报告从架构、F1–F8 缺陷、文件系统安全、状态机恢复、测试结果和剩余风险评估 Delivery Contract。

## 核心规则
- 执行完成和交付验证是两个独立结果。
- 基线在任务开始时捕获，不能在任务结束后重新定义。
- 文件路径检查必须处理符号链接、目录替换和读取期间路径变化。
- 验证失败、验证不可用和未验证必须区分。
- 崩溃后可以只重试验证，不能因此重新执行 Agent。

## 边界
报告结论只覆盖指定候选和测试平台。
