# Cognitive Core v1.0 — 最终整合报告

[English](FINAL_COGNITIVE_CORE_V1_REPORT.md)

**2026-11-16：工程整合完成。**Owner 定义的 v1.0 实现及验证门禁已在披露的平台范围内完成。冻结源码的 macOS 普通／Race 各覆盖 **3005 PASS／0 未解决 FAIL／46 SKIP**；首轮包超时失败保留，通过只补完 runtime 包完成覆盖，不冒充一次全量绿。具备待 Owner 授权合入 main 的技术条件。Cognitive Core v1.0 是能力基线，不是 Core SemVer Release，不代表人类级认知或已证明自主学习收益。

## 范围与授权

- Owner 指定的唯一输入：`8c30f75e063db69a1dbc867741dc8f042c7d0dbc`，包含 Astra、Fable、Carbon、Opus 修复。受保护的本地／远端 main：`00344b5d6a67673fea9fcbc90e0cab16de17dc57`。
- 独立分支：`integ/cognitive-core-v1-20261116`；Worktree：`wt/core-cognitive-v1-integration-20261116`。
- 冻结 Go 实现和 Schema 0014–0017；不新增 0018，不实施 Context Space，不改 Desktop，不发起付费模型请求，不操作生产数据库，不 Merge／Push／Tag／Release。
- 保留 V-D1 共享经验撤回的 fail-closed、V-002／V-003 限制；严格区分宿主 SDK 与自动 Agent 路径。Experience 是 **Verified Procedural Macro（受验程序化宏）**，因果学习收益保持 `causal_benefit_unmeasured`。
- main 原有 `AGENTS.md` 修改及历史 Worktree／证据均不在本轮改动中。`bench/evidence/cognitive-core-v1-integration-20261116/BASELINE.json` 保存分支、HEAD、状态和原工作树／暂存区差异。工作区证据及 `TODO.md` 不属于 Core Git。

## 1. 实际整合了什么

指定输入是受保护 main 和全部近期指定审查提交的后代。任务开始用 `git ls-remote` 核实远端 main，没有更新远端引用。独立 Worktree 直接从指定输入创建，不重放、不压缩、不丢弃历史修复。

33 个提交保留 Astra 完成候选（`949b2c6`／`c1d3229`）、Fable 的 17 个 FFR 修复（`c6bdfaf`／`f2dfa286`）、Carbon ICRA-001–004（`f8a87f5`）、Opus ICRA-V-001–003 与 V-T1（`8c30f75`）。更早的 P0、ACR、`e2c69c6`、`61a1d31`、`91af861` 已由完成候选导入，不再盲目 cherry-pick 平行历史分支。精确关系及受保护工作树见 [Git 报告](COGNITIVE_CORE_V1_GIT.md)和工作区 `GIT_RELATIONSHIPS.json`。

**本轮整合没有改变生产代码、测试和 SQL。**全部 893 个 Go／Module／SQL 文件保持输入字节，源码清单 SHA-256：`ff95fd3bcb8585810f121c73dd1be1450d29ad2fb35b4e53c5ef16e56964926a`；Linux 使用同一清单。候选改动的 Go 文件没有需要 gofmt 的项；无关的已有格式偏差保持原样。本轮改动是中英文当前文档、Finding 关闭对账、Migration 承诺更正及新验证证据，不是新增认知子系统。

## 2. 稳定架构与七个领域

稳定公开边界是现有 Go `runtime.Engine`。模型提出候选和选择动作，Framework 管理来源身份、证据资格、执行回执、CAS、Fence 和生命周期事务。隐藏模型推理不属于持久项目状态。存储仍为现有 SQLite、Session、Task State 与 Context Graph。

| 领域 | v1.0 的具体行为 | 如何协同及当前限制 |
|---|---|---|
| Memory | 工作状态／Notes，情景 Run／执行／检查，语义候选／原生 Node，程序性 Experience；版本来源与宿主保留策略 | 一个 Store、不同职责。逻辑冷热层及等证据归并不删除反例，不因高频而提高真实性；没有物理冷库或通用自然语言摘要器。 |
| Attention | 现有词法／符号 Attention Light，按任务选择候选 Start 提示，限定范围的 Experience 召回和警告 | 相关性不等于正确性。Experience 需显式召回，没有自动开局索引，也没有大规模检索质量结论。 |
| Reasoning | 精确断言、固定前提、证据适用性、冲突同类项及不可变决策回执 | Framework 区分 fact／inference／hypothesis／experience／decision／unknown 和 current／stale／conflict。模型推理，不实现通用因果证明机；查询模式只搜索 belief，显式 ID／依赖仍有上限。 |
| Learning | 提案→真实来源提取→批准检查验证→召回→采纳→明确原序列应用→应用结束观察→反馈／修订／恢复 | 已接入普通 Agent／工具／SQLite。完整观察程序是受验程序化宏，不是参数化迁移或已学会的通用规则。 |
| Belief Revision | 来源／版本／时间／依赖校验，CAS 历史，失效与反例 `needs_revision` | 前提变化会停止复用。模型不能自行批准修订；原生 belief／negative／abstraction 及定义修改仍需宿主明确调用。 |
| Governance | 正常 SQLite Analyze／Plan／Validate；宿主专用 archive／tier／等价归并 Apply、保留、撤销／恢复 | 没有模型 Curator 或后台 Apply。预校验建议不代替事务权威，无效计划仍失败关闭。 |
| Verification | 宿主批准的精确检查、真实执行身份、闭合 Episode、Scope／Fingerprint／Run／批准纪元及结果 | 模型自评不是 PASS；部分／中断／未执行保持 Unknown。局部结果不能证明其他主张或全局正确。 |

七个领域通过共同的 Scope、Time、Provenance、Evidence、Ownership、Privacy／CAS 权威协同，**不是七套无关循环**。但 `cognition_candidates` 与 `cognition_nodes` 的生命周期、读取和冲突路径仍不同；v1.0 **不宣称所有记录已统一为一个模型**。这是 Owner 明确接受的架构边界，不是已修复缺陷。

Cognitive Core 与 Context Space 在同一 Go Module 中保持包级独立，仅由 `runtime` 组装。本轮未改 `internal/window`，未引入 ProjectPrefix、五区域预算、Reference Directory 或 Safepoint Compaction。Context Space 必须在 Owner 授权的认知整合进入 main 后另切分支。

## 3. SDK 可用与生产实际调用

| 入口 | 实际执行内容 | 不会自动发生的内容 |
|---|---|---|
| `EnableCognition`、`MaintainCognition`、可选 `CognitionOptions.Auto` | 显式维护，或启用后在顶层 Run 完成执行有界维护，产生检查支持的维护候选 | 必须显式启用；本轮没有付费维护请求；候选不自动转成原生 Graph。 |
| 候选 Start Attention | 通过 `BeginRun(Notices, NoticeSources)` 最多提供 6 条／2048 字节范围内定位提示 | 没有 Experience 开局索引。 |
| `propose_experience`、`recall_experiences`、`adopt_experience`、`apply_experience`、`assess_cognition` | 普通合格 Run 中由模型选择；沿用 Workflow／Policy／Hooks／预算 | 提供工具不证明模型选择过，也不证明执行过；采纳不等于成功复用。 |
| `finalizeCognitiveRun`、`AgentResult.Cognition`、`Engine.Open` | 合格 Run（含子 Run）终态结算提案／Use；重启完成中断的记忆结算 | 不重放工具，不把应用结束结果替换为工作区后来的状态。 |
| `PutCognitiveNode`、`VerifyCognitiveNode`、`ReviseExperience` | 可信宿主明确写入、验证和修订 | 没有自动原生 belief／negative／abstraction 写入者，也没有自主定义改写。 |
| `GovernorAnalyze/Plan/Validate/Apply`、`SetCognitiveLifecycle`、`MaintainCognitiveMemory` | 宿主请求、所属记录的 CAS 生命周期事务及保护性保留 | 没有定时自主 Governor 或模型批准的生命周期写入。 |
| `RevokeSource`、`DeleteSession` | 宿主明确操作，Framework 传播 Fence 并清理／恢复 | Restore 不能撤销隐私删除或复活已撤回身份。 |

完整方法／JSON 契约和触发矩阵见 [API](../cognitive-core/COGNITIVE_CORE_API.zh-CN.md)、[数据模型](../cognitive-core/COGNITIVE_CORE_DATA_MODEL.zh-CN.md)、[能力表](../cognitive-core/capabilities.zh-CN.md)。“SDK-only”指可用宿主能力，不是空接口，但也不是自动 Agent 写入。

## 4. 学习、记忆与治理边界

提取依据真实终态来源、实际 RunID、观察到的工具／参数摘要和合格检查。`agent.start` 检查点序号阻止首个请求准入前失败借用上次 Run。失败动作不能验证为正例。采纳重查条件并记录单次身份；`apply_experience` 必须在当前权限下复现独立、有序、精确的调度器执行。无关动作、拒绝写入、后台提交和 Unknown 回执不能计成功复用。

预期结果只在最后一个应用动作结算后、后续任务改变工作区前检查一次。反馈保留当时回执；反例停止未来复用，直到宿主修订并验证。仅 Uses 增加不再使未变化的固定前提 stale；真实定义／证据／Scope／状态变化仍失效。消费者投影只返回自己的 Uses 和统计。这些机制由真实公开演示及回归路径证明，不从 API 名称推断。

频率、最近访问、宿主重要性、模型置信度、检查结果分别表达。容量维护保护安全知识、Unknown／未来生效内容、失败证据和反例；只剩受保护或活跃内容时返回 Pressure，不为数量目标删数据。归并只归档等证据别名，保留修订和来源依赖。Restore 重查适用性，不制造验证。

## 5. 验证及保留的失败

[Build／Vet／Test／Race 与平台报告](COGNITIVE_CORE_V1_VERIFICATION.md)逐条记录命令及 PASS／FAIL／SKIP。[Migration 报告](COGNITIVE_CORE_V1_MIGRATION.md)覆盖冻结 0014–0017、升级、备份恢复、注入失败回滚、热 WAL 拒绝及防语义丢失降级。

- macOS、Linux 全 Module Build／Vet 通过。Windows amd64 Build／Vet／测试编译通过，但 CGO=0，不代表 Windows Runtime／SQLite 实跑。
- Linux 原生认知 Race 首轮为 419 PASS／6 FAIL／2 SKIP。失败的两组用例都调用缺失的 `curl`；在本任务临时目录解包匹配版本后，仅补跑这两组，6 PASS／0 FAIL／0 SKIP。同一源码按路径去重为 **425 PASS／0 未解决 FAIL／2 SKIP**，不是一次全绿。中间一次错用空 Module Cache 的 setup failure 同样保留。
- macOS 首轮完整普通／Race 命令触发默认 10 分钟 `runtime` 包级期限，此前没有失败测试终态。仅补完 runtime 包，`-timeout=30m` 下普通／Race 各 **908 PASS／0 FAIL／21 SKIP**（命令耗时分别 845 秒、1143 秒）。42 个已通过包加补完的 runtime，按同源路径去重后普通／Race 各 **3005 PASS／0 未解决 FAIL／46 SKIP**；两种模式测试名集合相同，无 Data Race 报告。首轮计数和栈保留，这是完整同源覆盖，不是一次完整命令退出成功。
- 离线公开 `lcx-experience-demo` 退出 0：真实来源写入／检查、候选／验证／召回／采纳，后续任务生成全新结果，记录一次检查反馈，重启保留，错误条件拒绝。临时状态已删除，没有真实模型或模型网络请求。

Linux 指在既有特权 Docker Desktop／LinuxKit 容器内的 Ubuntu 24.04 userland 上，以 UID 1000 和 systemd 用户委派运行原生 linux/arm64 进程；不是 Ubuntu VM／物理主机，也不是完整 Linux 平台验收。没有关闭 sandbox／隐私规则来让测试变绿。

## 6. Finding 与独立证据

[Finding Ledger](../cognitive-core/finding-ledger.md)在不回写历史报告的前提下核对全部关闭：Fable 的 17 项由 Carbon 独立复核，Carbon 的 4 项由 Opus 独立红绿验证；Opus 的 3 项代码修复取得后续独立源码级架构认可，其原红绿运行仍标为 Opus 自验。不同证据等级分开，不宣称架构审查者重新跑过测试。

本轮由主 Astra（`local/gpt-6-astra`）负责实施和判断，没有替代实施子 Agent，没有第五轮全面审计。整合测试是本轮自验，不是新的独立 PASS。没有确认新的 Runtime 缺陷：观察到的阻断是验证前置缺失和包期限不足。更正一处文档：0017 降级语义保护在**备份／Marker 之后的事务内**拒绝，而 Schema 家族前置校验在修改前拒绝；未改 Migration 或验收预期。

| 接受限制 | 具体影响及后续触发 |
|---|---|
| V-D1 | 已记录消费者之一被撤回会 Fence 共享 Experience owner，所有读者失去该经验；仅依赖 origin 的会话不被连带 Fence，但前提 stale。保留 fail-closed；独立 Use 存储需另行授权，不在本轮新增 0018。 |
| V-002 | tier 修改仍可使前提 stale，并发采纳可能返回 CAS；不新增推测性重试，不弱化版本规则。 |
| V-003 | 部分 stale／dormant／同批无效计划到 Apply 才拒绝；事务权威不变，建议性预校验一致性是 v1.1 工作。 |
| 双记录族 | 共同证据／隐私权威，不同生命周期和冲突视图；不静默把候选桥接成事实。 |
| 有界程序 | 终身 8 Uses，Node 64 KiB，大定义可能更少；反馈／生命周期预留保证正确性，不代表无限复用。 |
| FFR-D1／因果价值 | 忠实复现程序可能匹配预先已存在的结果；统计不证明因果收益，保持 `causal_benefit_unmeasured`。 |
| FFR-D4 | 旧未发布候选中的随机 owner／失败动作记录不转换；保留原库、对应二进制和凭证，用新测试状态。规范 main 升级另有支持。 |
| 自动化与规模 | 没有自动原生 belief／negative／abstraction 写入、Experience 开局索引、自主 Governor Apply、通用迁移、物理分层、长期／大规模收益结论。 |

Owner 已明确这些限制不用于扩大 v1.0；它们仍是可见约束，不描述成“已修复”。[v1.1 与 Cognitive Evolution](../cognitive-core/roadmap.zh-CN.md)记录后续触发，但不授权付费实验、新 Schema 或 Context Space 开发。

## 7. 十二项反向检查

| 问题 | 根据证据的回答 |
|---|---|
| 1. 遗漏历史修复？ | 指定近期审查头均是 `8c30f75` 祖先，保留 33 提交；核对继承导入与当前 Finding 路径，未把脏平行候选误当额外输入。 |
| 2. 测试通过但生产未接入？ | 五种模型工具和终态／重启结算已接入，`TestCompletionLearningThroughOrdinaryTools` 及公开演示实测；原生写入者／Governor 自动化明确为 SDK-only。 |
| 3. 文档超出实现？ | 中英文当前 API／架构／生命周期／能力表区分精确程序复用、双记录族、宿主操作、Unknown 和未测因果收益；历史报告保持历史范围。 |
| 4. Migration 回滚／恢复遗漏？ | 真实 SQLite 规范升级／备份恢复、0014–0017 回滚／拒绝、缺表／热 WAL、重启用例通过；更正备份先于 down guard 的文义，未碰生产库。 |
| 5. 跨 Consumer 隐私泄漏？ | 私有 Uses 投影、外部决策清理、共消费者撤回及派生 Fence 定向 Race 通过；保留 V-D1 可用性代价，不绕过隐私。 |
| 6. 旧版本覆盖新版本？ | Native Graph CAS、来源版本、并发采纳／结算、Uses-only 依赖身份用例通过；真正语义变化仍失效。 |
| 7. 没有执行却记成功？ | 精确有序回执、`TestCompletionFeedbackRequiresActualExperienceApplication`、拒绝／无执行／前次 Run 回归通过；召回和采纳不计成功使用。 |
| 8. Race／事务一致性缺口？ | Callback 原子性、跨进程 Fence、并发消费者均有通过证据。同源完整普通／Race 各3005 PASS／0 未解决 FAIL／46 SKIP，无 Data Race 报告；首轮包超时另行保留。 |
| 9. 整合导致行为变化？ | Go／Module／SQL 清单未变；改变文档和验证命令前置，不改变生产语义。 |
| 10. 为收尾降低安全边界？ | 没有关闭 Approval、Fence、Workflow、sandbox、断言或 Migration Guard，保留首次失败。 |
| 11. 混入 Context Space？ | 无直接 cognitive/window 包依赖、第二 Module、新 Schema 或 window 实施；`runtime` 仍是组合根。 |
| 12. 可直接删除的冗余实现？ | 没加 Runtime 层。两个记录族各有活跃消费者和隐私契约，删除任一个会丢支持行为；没有以臆测死代码扩大清理。 |

## 8. 直接交付回答

1. **能否形成 Cognitive Core v1.0？**可以。Owner 定义的有界架构、实现和工程门禁已完成，不需为本次接受基线新增认知功能或 Schema。
2. **承诺的 v1.0 能力是否全部实现？**在明确接受范围内是：共同来源／证据／隐私权威、受控 Experience 闭环、原生 SQLite 治理、修订及保护性记忆。“全部自主认知行为”不属于最新承诺。
3. **哪些仅 SDK 暴露？**原生 belief／negative／abstraction 写入／验证、显式定义修订、Governor 生命周期／容量以及管理性隐私／批准操作；是真实可用 API，不是自动写入者。
4. **哪些进入生产调用链？**候选 Start 提示、五种模型选择工具、普通权限控制下的应用、应用结束检查、终态提案／Use 结算和重启恢复；仍需 cognition／Auto 前置。
5. **哪些未实现？**统一跨族生命周期／冲突视图、Experience 开局索引、自动原生写入／Governor、通用语义／参数迁移、物理分层及已证明的长期收益。
6. **是否存在未解决 P0／P1／P2？**没有范围内未解决的产品 Finding；验证事件均有关闭证据。历史 V-D1 P2 仍是明确接受的可用性权衡，不是未授权遗留修复；其他仓库待办不在本结论范围内。
7. **是否有接受限制？**第 6 节逐项列明，均未描述为已解决。工程完成不代表自主学习收益或人类级认知。
8. **是否可以合入 main？**对冻结提交和披露的平台边界，技术条件已具备。Merge／Push／Tag／Release 与历史分支清理仍需 Owner 授权，本轮均未执行。

最初广义目标在有界、证据治理的 Go Runtime 机制上得到实现，不等于已经证明完整自主认知。记忆、学习和推理协同，但双记录族及自动接入缺口仍在。main 仍只有既有 Context Graph、Notes／Recall、Attention Light 和只读 Governor 基础；**所有扩展 v1.0 能力在授权合并前仍仅属于候选**。

## 9. 交付物及授权

- [英文 API](../cognitive-core/COGNITIVE_CORE_API.md)／[中文 API](../cognitive-core/COGNITIVE_CORE_API.zh-CN.md)，中英文架构、实现原理、数据模型、记忆生命周期、能力矩阵。
- [Finding Ledger](../cognitive-core/finding-ledger.md)、[ADR-0023](../adr/0023-unified-cognitive-lifecycle.zh-CN.md)、[验证／平台报告](COGNITIVE_CORE_V1_VERIFICATION.md)、[Migration 报告](COGNITIVE_CORE_V1_MIGRATION.md)、[Git／合并／清理计划](COGNITIVE_CORE_V1_GIT.md)、[v1.1 路线](../cognitive-core/roadmap.zh-CN.md)。
- 工作区 `bench/evidence/cognitive-core-v1-integration-20261116/`：Owner 范围、基线、源码清单、原日志、失败归因、平台计数、校验清单，以及提交后绑定准确整合 SHA 的 `FINAL_DELIVERY.json`。
- 整合根目录 `SHA256SUMS` 覆盖交付源码、测试／夹具、当前文档与发布支持；外部证据清单覆盖日志和提交后身份。`MANIFEST_SCOPE.md` 定义排除项并避免自引用，`CHECKSUM_VERIFICATION.json` 记录实际核验。

当前仅授权范围内本地提交。没有 Merge main、Push、Tag、Release、Desktop 改动、生产迁移或删除历史分支／Worktree。共享 Linux 容器不属于本任务，只能清理唯一临时验证目录与本任务编译产物。工作区证据／TODO 不宣称已进入 Core 提交。
