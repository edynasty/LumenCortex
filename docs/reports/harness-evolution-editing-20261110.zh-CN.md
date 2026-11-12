# Harness Evolution：统一编辑引擎、Hashline 和 AST 编辑（中文摘要）

英文原文：[Harness Evolution — Editing slice](harness-evolution-editing-20261110.md)

## 目标

解决多个 Agent 并发修改文件时的陈旧覆盖问题，并提供比纯文本替换更可靠的代码编辑方式。

## 主要设计

- 所有写入统一经过 Editing Engine。
- 编辑操作绑定文件版本和内容摘要，提交前检查版本是否变化。
- Lease 和 CAS 保护并发写入。
- Hashline 使用带哈希的行锚点定位目标，减少行号漂移造成的误编辑。
- AST 编辑根据语法结构、作用域和类型关系执行重命名等操作。
- LSP 编辑也必须经过统一文件保护路径，不能绕过 CAS。

## 重要区别

Hashline 是定位和前置条件检查，不证明修改语义正确。AST 编辑能力取决于语言实现；Go AST 能力不能宣称支持所有语言。

## 验证边界

原报告记录了并发、版本变化、错误 Rename 和旧实现反例。完整限制以当前 Go Editing Engine 和 [Harness 能力清单](../harness/capabilities.zh-CN.md) 为准。
