# Cognitive Core — 数据模型（Migration 0014–0017）

Cognitive Core v1.0 独立整合，输入 `8c30f75`，未合并。Hub 0013 不变，随后为 cognition 0014、support 0015、原生 Graph 0016、completion 0017，不新增 0018。历史 Cognitive 13/14/15 不能通过改版本号迁移。所有表位于同一 SQLite，使用 STRICT 和固定连接的 `BEGIN IMMEDIATE`，事务内不执行模型或 Git 工作。[English](COGNITIVE_CORE_DATA_MODEL.md) · [Migration 验证](../reports/COGNITIVE_CORE_V1_MIGRATION.md)。

## 表、所有权和生命周期

| 表 | 权威字段 / 作用 | 生命周期与来源 |
|---|---|---|
| `maintenance_state` | 启用后唯一 id=1；project、policy、dirty/cursor、lease/generation/expiry、pending/error | EnableCognition 创建项目维护权威；cursor 不得超过 dirty，提交重查 Lease。 |
| `maintenance_sources` | 真实 SourceRef、来源版本摘要、Session/Run、敏感性、Runtime 来源 | 终态及后续版本变化在原写事务排队；处理后删除，随来源会话级联清理。 |
| `maintenance_dedupe` | 来源版本、读者范围、策略的处理键，generation、结果、维护会话 | 追加且不含正文；维护会话被 Fence 后通过 RedoKey 链重新处理，不覆盖旧记录。 |
| `cognition_candidates` | 候选/来源/支持身份、原 Run/Session、有限主张/理由/路径、检查、策略和提交状态 | 追加；仅隐私清理可清空正文。0015 增加 assertion/binding/support/purged_at，提交资格不是当前可用性。 |
| `cognition_observations` | candidate、check、result Session/seq、episode、fingerprint、spec、approval epoch、outcome、scope、purpose、run | 由事务从合格 eval_results 复制字段；追加，外键关联候选/结果，删除级联清理。 |
| `cognition_nodes` / `cognition_edges` | Node 版本、Scope、来源、Evidence、Experience、认知类型、置信度、固定依赖、MemoryPolicy、修订原因 | 同一 Node 用 `(id,version)` 追加历史；CAS、生命周期与隐私规则见 ADR-0023。边是不可变诊断关系，不证明因果。 |
| `cognition_applications` | adoption_id 唯一；精确 Run/定义/动作/前提、执行 ID、应用结束时的结果观察；JSON 32 KiB | pending → completed/failed/unknown；重启不重放动作；纳入隐私清理。 |
| `cognition_proposals` | Session/Run 上的模型结构化提案，无权威；JSON 16 KiB | pending → completed/refused，终态或恢复处理。 |
| `cognition_decisions` | 确切评估版本、适用性、请求动作与来源；JSON 16 KiB | 不可变决策回执；通过 owner、派生关系、sourceSessions 和引用 Node 清理跨会话内容。 |
| `cognition_access` | node/reader 唯一；访问次数和最近时间 | 原子更新，读者私有，随读者清理；不是验证或成功次数。 |
| 既有 `session_checkpoints` 中的 `agent.start` | BeginRun 同事务写 `{runId}`，最新 start 不被裁剪 | 用 seq 排序已启动 Run；只有无记录旧历史才回退 main 请求，不新增表或 Migration。 |

候选索引覆盖 claim_digest、subject_key、support_digest；观察索引覆盖 candidate/check/id 与 result Session/seq。原生 Node 按 owner、kind/project/id/version 索引；应用、提案、决策、访问均有 Session 查找索引。

精确主张绑定复用 ADR-0017 `check_approvals`。认知绑定摘要包含当前策略，普通 Task State 绑定不变。旧认知绑定需要宿主重新批准，没有自动授权。

## 状态词汇不能互换

| 状态 | 存储 / 当前含义 |
|---|---|
| Runtime observation / source | 真实终态来源，证明发生过工作，不直接表示知识正确。 |
| candidate | 不可信模型提案，引用经过 Framework 校验。 |
| qualified | 提交时精确结构化 fact 有当前宿主绑定，绑定检查在闭合 Episode 中通过；只是历史资格。 |
| candidate_only | 结构/来源有效，但缺绑定、只有文字或检查没有合格通过。 |
| rejected | 字段/来源不合法或自填检查/状态；仅审计，不向普通读者返回。 |
| qualified observation | 某检查在某范围、指纹、批准纪元、Run 给出结果；不表示范围外事实成立。 |
| purged | 隐私删除后不保留 subject/claim/assertion/rationale/路径/理由，读取为无正文 withdrawn。 |
| applicability | 按读者实时计算 current/unknown/stale/source_missing/withdrawn/conflict/candidate_only。 |

qualified、validated、epistemic fact 都不能跳过读取时的来源、绑定、时间和冲突检查。

Experience 是受验程序化宏。Node Version、DefinitionVersion、Adoption ID 和 Application ID 是不同身份。仅 Uses/记录时间等变化不改变固定前提；定义、验证、状态、Scope 或生命周期变化仍使前提 stale。返回值只给读者自己的 Uses，存储仍将所有消费者内嵌在共享 Node。因此 V-D1 接受“任一已记录消费者撤回使共享经验整体不可用”；仅依赖已验证 origin 的会话不被连带 Fence，但其前提失效。

`cognition_candidates` 与 `cognition_nodes` 共用检查、来源、隐私边界，不共用一个生命周期，也没有自动桥接或跨族冲突视图。频率、最近使用、模型置信度、宿主重要性和实际结果分别表达；因果效用保持 `causal_benefit_unmeasured`。

## 事务与不变量

- 来源终态/版本和 dirty mark 同事务提交；后续宿主、交付或 Task State 变更继续排队。
- Lease 的 ID、generation、expiry、cursor、policy 都在提交时重查；失效租约不能写入。
- 批次原子提交，来源 CAS 和 Fence 防止晚到的模型结果覆盖已变化内容。
- 去重比较 live SupportDigest；被撤回维护会话不阻挡新的独立支持。其他同批来源以幂等队列恢复，不重复累积任务。
- 观察只能来自当前获准、真实执行、闭合、指纹稳定的结果。error、取消、未执行均不能当 PASS。
- 后来的同检查状态失败，即使没有 cognition_observations 链接，也使旧 PASS 不再可用；其他 Run 的失败不能当作当前读者自己的证据，其他 Run 的 PASS 也不能使旧证据复活。
- `rowid` 只用于单个查询快照的追加顺序，持久证据身份仍是 `(session_id,seq)`，不会因 VACUUM 失效。
- 隐私清理先停止/锁定派生运行，再分批清理，最后重建 SQLite 与截断 WAL；失败保留待恢复状态。所有历史版本都服从当前 Fence，不能借历史读取恢复内容。

## Migration 语义

1. **13→14：**在 Hub 后创建五张维护表与索引。回滚删除派生候选状态，保留其他 Session/Hub 行；碰撞与注入失败不留下半张表。
2. **14→15：**增加带默认值的 assertion/binding/support/purge 列；原候选没有新支持时不自动获得信任。回滚与再升级保留原行。
3. **15→16：**增加原生 Graph 与 eval_results.cognition_epoch。非空 Graph 或任何隐私纪元阻止破坏性降级，避免旧读取器复活删除前证据。
4. **16→17：**增加回执/提案/决策/访问记录及索引，不重写旧反馈。缺表在打开前拒绝。新表有行或 Node 含 completion 语义时，down SQL guard 在事务中失败并回滚；此前 Runner 可能已创建备份和 checkpoint marker。保持逻辑数据/Schema，不等于文件逐字节未触碰。
5. **Schema 家族错误、版本过新等前置拒绝**与上述 SQL 回滚不同：在写库和备份前拒绝，相关测试覆盖热 WAL 的逐字节保持。
6. 本轮仅用临时 SQLite 验证，不迁移生产库，不修改 user_version，不转换旧未发布候选的随机 owner/错误验证数据。原库与其二进制必须一起保留，测试使用新状态目录。

历史 R1/R2/Fable 文档中的旧版本号是当时分支事实，不是当前迁移顺序；本契约唯一当前序列为 Hub 0013 → 0014 → 0015 → 0016 → 0017。
