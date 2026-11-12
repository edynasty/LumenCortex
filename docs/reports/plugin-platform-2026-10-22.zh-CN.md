# Core Plugin 平台和通用 Session 导入（中文摘要）

英文原文：[Plugin platform](plugin-platform-2026-10-22.md)

## 目标
提供 Core 管理的插件接口，并把 Session 导入作为第一类来源能力。

## 主要设计
- Plugin 通过公开 Host 合同访问 Core。
- Core 管理 Registry、版本、能力和生命周期。
- Plugin 不直接写 Session 数据库。
- 数据、事务和权限仍由 Core 持有。
- Listener 负责观察，Hook 只能在授权边界内介入。
- 插件平台不是安全沙箱，也不是公共插件市场。

## 实现原理
插件只通过 Host API 请求能力；Core 校验来源和权限并执行数据变更，避免插件形成第二套持久化权威。
