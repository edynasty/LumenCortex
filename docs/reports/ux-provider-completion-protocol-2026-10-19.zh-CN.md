# UX Provider：Anthropic Messages 协议（中文摘要）

英文原文：[Anthropic protocol](ux-provider-completion-protocol-2026-10-19.md)

## 目标
在 Go Runtime 中支持 Anthropic Messages 协议，并统一 Provider 分发和 Runtime/CLI 调用。

## 主要实现
- 新增 Anthropic Provider 适配。
- 统一 Provider Dispatcher。
- 配置模型、凭据和协议能力。
- 明确 reasoning 内容边界，不把私有推理过程写入普通日志或持久化上下文。
- CLI 和 Runtime 使用相同 Provider 语义。

## 验证边界
报告中的真实探测是可选项，未运行的探测不能算作通过。
