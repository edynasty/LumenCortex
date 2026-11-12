# ADR-0012：Core Plugin 平台和 Session 导入来源

## 决策
Core 提供统一 Plugin API 和由 Core 管理的 Registry。Plugin 通过 Host 接口访问 Core，不直接操作数据库或绕过事务。

主要规则：
- Plugin API 有版本和能力协商。
- Core 管理插件启用、停用和生命周期。
- Plugin 不能拥有第二套 Session Store 或状态权威。
- Listener 只观察事件；Hook 才能在授权边界介入。
- Plugin 不是安全沙箱，也不等于公开插件市场。
- Session Import 是首批来源型能力之一。

## 后续增补
2026-11-01 增加进程外接入路径和统一 Capability View。外部进程必须通过受限协议和 Host 权限提供能力。

## 实现原理
Core Registry 管理身份、版本和能力；Host 提供受控 API；数据和事务仍由 Core 持有。插件故障不能绕过权限、持久化和恢复规则。
