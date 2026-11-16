# ADR-0023：统一认知生命周期与实际应用证据

- **状态：**独立候选中 Accepted；合并和发布仍由 Owner 决定。
- **日期：**2026-11-13
- **责任人：**Astra，Cognitive Core 实施负责人
- **取代：**候选中仅宿主演示调用和“任何 tool.end 都算复用”的旧假设；历史证据不回写。

## 问题

`91af861` 已有范围、证据、原生 Graph、Fence 和 Experience API，但普通 Agent 没有经过完整学习链；无关成功工具可能被计为策略复用。保留、固定依赖和证据判断也缺少统一执行边界。

## 决定

1. 复用一个 Engine、Agent Loop、SQLite Store、Evaluator、来源图、Governor 和既有 Maintenance Lease，不新建服务、数据库、调度器或自主 Agent。
2. 原生 Node 增加小型共用字段：认知类型、可选模型置信度、固定版本 `NodeRef`、宿主 `MemoryPolicy` 和修订原因。存储资格是历史，当前适用性按读者计算，不造万能记忆对象。
3. 只在启用认知的普通 Run 注册 propose/recall/adopt/apply/assess。Session/Run 由 Framework 固定；检查批准、治理、共享和隐私权限留在宿主。
4. 应用必须显式匹配原始完整工具序列与规范化参数摘要；采纳、应用及后续步骤重查前提，仍受普通 Policy、Workflow、Hook、预算和超时约束。存储文字不自动重放，原操作序列是有意选择的安全边界。
5. 准入、执行 ID 与结果证据分开。完整已结算工具失败也是观察；拒绝、中断、部分、未知不算确认复用。成功/失败反馈必须来自独立执行的获准结果检查，并且在应用的最后一个动作结算时执行：观察写入回执，之后的检查不能替代它。
6. 原任务终态之后结算提案和反馈。恢复持原 Run Lock，pending 变 unknown，补全记忆但不重复动作；记忆错误不改写任务实际结果。
7. 宿主 Governor 只执行逻辑冷热层、未保护所有者记录归档和等价证据归并；CAS、全成全败，规范目标同批不可修改。恢复检查当前来源/时间/依赖，不能复活失效身份。branch/promote/epoch 引入新主张，仍仅建议。
8. 保留历史和来源/消费者隐私。返回值仅显示当前读者自己的 Uses；依赖与决策暴露用 `derived_origins`，新内容加入既有隐私删除/恢复。

应用中的动作复用普通 Workflow 观察和 Hook。执行 ID/结果、Task State、内嵌来源依赖在下一步前原子提交；权限每步重算，拒绝后台任务，人工 Gate 或拒绝转换停止序列。不得按时间/工具/摘要推测调用身份。通用 Node 提案不能创建或改写规范绑定；未改变的非活跃别名绑定仅是历史，不是撤销前提。`read_thread` Attention 保留有限条件经验定位和真实反例警告，不升级成事实。

## 不变量

- 模型不能自证、提权、推测执行成功、伪造 PASS 或把 Unknown 变成事实。
- 检索排名、频率、重要性和置信度不替代证据；相同支持等级的冲突保持冲突，证据数组顺序不决定真伪。
- 低频安全知识、反例受保护，容量不足明确报告，不靠 TTL/LRU 强删。
- 旧来源/版本/Fence 不覆盖新状态，隐私删除优先于保留历史。
- 不新增隐藏推理、凭据或原命令正文作为认知记忆；命令仍在既有可观察 Transcript，Experience 只记录摘要/定位。

## 不采用的方案

- 存摘要后把任意成功工具当复用：会把无关操作、预存产物误算为学习。
- 自动参数化重放或任意 Graph 提升：Framework 无法证明语义等价或自行授权，新方法需新真实经历和检查。
- 第二套学习 Loop、数据库或 Embedding 服务：现有 Go 契约足以承担生命周期和词面检索。
- TTL/LRU 删除或有损事实合并：新旧和相似度不能证明可以丢弃安全、负向证据或来源。

## 持久化与兼容性

0017 在保留 Hub 0013 和认知 0014–0016 后增加有界 STRICT 应用、提案、决策、私有访问表及类型索引。不将旧历史回填成成功。原 Migration Runner 负责锁、备份、失败回滚；缺表与历史分支冲突在修改前拒绝。降级遇到新数据/语义时拒绝，不丢证据。本任务不迁移生产或用户数据库。

最终候选为每个动作保存 `outcome`，不再用成功布尔值代替结算结果。冻结前旧候选回执缺少确切结果时保持未确认，不回写旧反馈或凭空补计成功。这是候选 JSON 契约修正，不是生产 Schema 迁移。

### v1.0 整合澄清（2026-11-16）

Owner 指定 `8c30f75` 为唯一输入，接受 V-D1 共享经验失败关闭及 V-002/V-003 限制，冻结 0014–0017；不授权 0018 或旧候选数据转换。仅 Uses 改动不改变固定经验前提，定义、验证、状态、范围或宿主记忆策略变化仍失效。Run 身份使用事务性 agent.start 检查点序号，不按壁钟排序。

两记录族共用证据/来源/隐私权威，但生命周期和读取路径不同。Experience 是受验程序化宏，不是通用语义学习。原生 belief/negative/abstraction 创建及 Governor 保留仍是宿主 SDK；模型选择工具不等于自动后台行为。V-D1 可因一个消费者删除而撤回整个聚合，原始定义依赖方不连带 Fence，但其前提变 stale。

Cognitive Core 和 Context Space 是同一 Go Module 中的正交职责，仅由 runtime 组装，彼此无直接依赖，保留 Start/工具内容暴露事务。本澄清不新增 ProjectPrefix、Reference Directory、Safepoint Compaction、第二个 Manager，不改变 Core 版本或 Tag。[v1.0 报告](../reports/FINAL_COGNITIVE_CORE_V1_REPORT.zh-CN.md)。

## 结果、限制与验证

普通 Agent 可达条件学习、纠错与记忆治理。原操作匹配、每条经验八次 Uses、64 KiB Node 限制泛化与规模；不宣称任意迁移或长期收益。确定性本地 Provider 驱动真实 Engine、工具、检查和 SQLite；红绿、build/vet/全量测试、定向 Race、Migration 和平台结果见[交付报告](../reports/cognitive-core-final-completion-2026-11-13.md)，独立审查单列。

[English](0023-unified-cognitive-lifecycle.md) · [架构](../cognitive-core/architecture.zh-CN.md) · [生命周期](../cognitive-core/memory-lifecycle.zh-CN.md)。
