# Provider 重试和故障恢复（中文摘要）

英文原文：[Provider retry recovery](provider-retry-recovery-2026-11-03.md)

## 目标
Provider 暂时不可用时恢复请求，但不能因为重试而重复执行工具或掩盖真实失败。

## 主要规则
- 只对可恢复的网络、限流和服务可用性故障重试。
- 参数错误、权限拒绝和模型拒绝不能盲目重试。
- 重试等待响应取消和 Deadline。
- 流式响应必须完整结束后才执行工具调用。
- 不完整响应中携带的工具调用不能提前执行。
- 重试属于当前 Step，不额外增加 Agent Step。

## 实现原理
Provider Adapter 识别错误类型并向 Runtime 返回可重试分类；Runtime 在有限预算内等待和切换候选。非幂等工具执行与 Provider 请求重试分开管理。
