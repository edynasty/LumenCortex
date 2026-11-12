# UX / Provider：凭据引用（中文摘要）

英文原文：[Provider credentials](ux-provider-completion-credentials-2026-10-19.md)

## 目标
统一 Core 和 Desktop 的凭据管理，避免 API Key 明文写入项目配置、日志或模型上下文。

## 主要设计
- 配置中使用 secret 引用，而不是密钥值。
- 凭据存储由用户级配置管理。
- CLI 和 Desktop 使用同一凭据来源。
- 读取凭据时按需解析，不把明文写回配置。
- 日志和错误消息必须脱敏。

## 实现原理
配置保存引用名；Credential Store 根据引用读取真实凭据。调用 Provider 前解析，完成后不将密钥持久化到 Session 或诊断日志。
