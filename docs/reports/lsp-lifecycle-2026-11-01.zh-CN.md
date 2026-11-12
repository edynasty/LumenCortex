# LSP 生命周期加固（中文摘要）

英文原文：[LSP lifecycle hardening](lsp-lifecycle-2026-11-01.md)

## 目标
统一管理语言服务器启动、请求、空闲关闭、崩溃恢复和工作区信任。

## 主要实现
- 按语言和工作区管理 LSP Server。
- Engine 打开时不启动所有语言服务器；有实际需求时再启动。
- 每个 Server 有启动、请求和空闲超时。
- 同一语言的并发启动会协调；资源繁忙时拒绝，不无限排队。
- 崩溃后标记不可用，后续请求按退避策略重启。
- Engine.Close 和 LSPStop 必须关闭并回收子进程。
- LSP Rename 返回的 Workspace Edit 必须经过统一文件保护和原子批次。

## 实现原理
LSP Manager 管理进程和协议状态；Runtime 根据工作区文件判断是否提供对应工具；写操作仍交给统一 Editing Engine。
