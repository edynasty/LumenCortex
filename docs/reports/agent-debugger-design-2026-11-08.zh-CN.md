# Agent Debugger 详细设计与原型报告（中文摘要）

英文原文：[Agent Debugger design](agent-debugger-design-2026-11-08.md)

## 这份报告解决什么问题？

报告区分两种调试对象：一是调试 Agent 自己的执行过程；二是调试 Agent 正在开发的目标程序。前者由 Agent Debugger 控制安全执行边界，后者通过 DAP 与真实调试 Adapter 通信。

## 设计结论

- Agent Debugger 依附现有 Run 和 Agent Loop，不建立第二套 Session/Run 管理器。
- Pause 在安全边界生效，不冻结任意 Goroutine 或操作系统进程。
- Step Model、Step Tool、Step Turn 必须保存已经完成的结果，不能因单步或恢复而重复执行。
- DAP 负责目标程序断点、调用栈、变量和执行控制。
- Adapter、目标程序和 IDE 接入必须由 Host 明确配置并受权限约束。
- Desktop Workbench 负责展示和控制入口，Core 才是执行和权限权威。

## DAP 设计

使用有界的 Content-Length 协议传输，校验请求/响应关联，限制消息大小和并发请求。Adapter 由受管理进程启动器创建，退出、取消和断连时必须清理。

调试变量引用绑定 Stop Epoch；程序下一次暂停后，旧引用必须失效，不能把过期 Frame/Variable ID 发给 Adapter。

## 安全边界

DAP 表达式可能执行目标程序代码，不能默认视为只读。查看变量与执行任意表达式必须分开授权。沙箱只能访问授权工作区和目标进程。

## 报告的证据边界

原报告记录了 P0 协议实验、真实 Adapter 和后续 Core/Desktop 阶段。它不等于所有平台和 Desktop 原生交互均已验收。请以当前 [DAP 文档](../dap-debugger.zh-CN.md) 和平台验证记录判断现状。
