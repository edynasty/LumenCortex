# Harness Evolution：DAP 实现记录（中文摘要）

英文原文：[Harness Evolution — DAP slice](harness-evolution-dap-20261110.md)

## 目标

把 DAP 从原型推进到 Core 管理的程序调试能力，并与现有进程生命周期、权限和 Agent 执行控制集成。

## 主要实现

- 建立独立 DAP Transport，处理消息 framing、请求关联、事件队列和取消。
- 使用受管理进程生命周期启动 Adapter，并在退出、取消和连接失败时清理。
- 在 Runtime 暴露受限调试 API 和工具面。
- 区分 Agent 执行调试与目标程序调试。
- 将断点、Stack Frame、Scope 和变量引用绑定到当前 Stop Epoch。
- Host 配置 Adapter；模型不能自行指定可执行文件或环境。

## 验证和限制

报告包含真实 Adapter、协议异常、并发、取消和进程清理测试。原生 Wails 交互和 Linux 真实 Adapter 验收必须单独确认，不能用 macOS 结果代替。

## 当前判断

该报告是实现过程记录。当前可用接口和限制以 [DAP 程序调试文档](../dap-debugger.zh-CN.md) 为准。
