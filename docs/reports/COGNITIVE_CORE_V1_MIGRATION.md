# Cognitive Core v1.0 — Migration verification / 迁移验证

Input / 输入：`8c30f75e063db69a1dbc867741dc8f042c7d0dbc`。Integration / 整合：`integ/cognitive-core-v1-20261116`。This report distinguishes source contract from command evidence; execution results are recorded in the [engineering report](COGNITIVE_CORE_V1_VERIFICATION.md). 本文区分源码契约和实际运行结果，不将测试存在当作 PASS。

## Frozen sequence / 冻结顺序

| Step | Changes / 改动 | Existing regression / 既有回归 |
|---|---|---|
| Hub 0013 | Preserved mainline tables / 保留主线 Hub | `TestCanonicalSequencePreservesHubAndRecoversBackup` |
| 0014 cognition | Maintenance state, sources, dedupe, candidates, observations / 五张维护表 | `TestCognitionMigrationRoundTrip`, `TestCognitionMigrationRefusesExistingObject`, `TestCognitionMigrationFailureRollsBack` |
| 0015 support | Defaulted assertion/binding/support/purge columns / 带默认值的支持列 | `TestCognitionSupportMigrationRoundTrip`, `TestCognitionSupportMigrationRefusesExistingObject`, `TestCognitionSupportMigrationFailureRollsBack` |
| 0016 native graph | Native node revisions/edges and result privacy epoch / 原生图及检查隐私纪元 | `TestCognitiveGraphMigrationRoundTrip`, `TestCognitiveGraphMigrationFailureRollsBack`, `TestCognitiveGraphDowngradeRefusesKnowledgeAndPrivacyLoss`, `TestCognitiveGraphRefusesSchemaCollision` |
| 0017 completion | Applications, proposals, decisions, reader-local access and indexes / 回执、提案、决策、私有访问 | `TestCognitiveCompletionMigrationRoundTripAndFailure`, `TestCognitiveCompletionDowngradeRefusesNewSemantics`, `TestCognitiveCompletionMissingTableRefusedWithoutMutation` |

All eight 0014–0017 up/down SQL files and the existing registry are inherited unchanged. ICRA started-run identity uses the existing `session_checkpoints`, not a migration. No 0018, manual `user_version` edit or new schema authority. 八个 up/down SQL 与注册逻辑保持输入原样；已启动 Run 身份复用既有检查点，不新增版本。

## Upgrade and failure / 升级与失败

The production runner validates the schema family, owns `db.lock` exclusively during schema changes, creates its scrubbed versioned backup, and applies each upgrade with its `user_version` in one transaction. An injected SQL failure rolls back that step, not previously committed steps. Checkpoint markers retain unfinished scrub/WAL work for the next Open. 升级按步事务执行；失败步骤回滚，已提交步骤不假装没发生；剩余重建和 WAL 清理在恢复时继续。

Existing canonical-sequence tests preserve Session/Hub rows and restore from the pre-upgrade backup. Legacy Node databases, historical colliding Cognitive 13/14/15 layouts, missing current tables and version-too-new stores are refused rather than relabeled or partially opened. 热 WAL / 缺表 / 分支碰撞的拒绝测试覆盖原文件保持，不依赖读取后“看起来可用”。

## Downgrade semantics / 降级的两种拒绝

1. **Preflight refusal / 前置拒绝：**schema mismatch, too-new version, invalid target or missing down file fail before opening the writer or making a backup. Crossing below version 6 with live fences also refuses before backup. Applicable tests assert byte-identical database/WAL and unchanged backup set.
2. **SQL semantic guard / SQL 语义保护：**0016 refuses native graph/edge or privacy-epoch loss; 0017 refuses completion rows and new native dependency/memory/epistemic/revision/Run-bound-use meanings. These guards execute after backup/marker creation inside the single downgrade transaction. Failure rolls back schema and logical rows; it is **not** a claim that no backup, marker, checkpoint or file-level change occurred.

The inherited data-model sentence “0017 refuses before backup/mutation” was corrected in both languages. No down SQL, expected test outcome or production behavior changed. 该更正只消除文档过强承诺，不改变测试或迁移实现。

Empty compatible state can downgrade through the original offline API. Nonempty new semantics must stay with a compatible binary; deleting evidence to make a downgrade succeed is not permitted. Restore does not undo a privacy deletion or remove retained fences.

## Development and production boundary / 开发与生产边界

- Verification uses temporary fresh databases and committed test fixtures only. No production, user or historical development database is opened for migration by this integration.
- Old unmerged candidates may have random Experience owners or records validated over failed actions (FFR-D4). They are not converted. Preserve that database with its matching binary/evidence; use a fresh test state directory. Do not delete historical evidence or weaken the decoder.
- Supported baseline is canonical main Hub 0013 or a new Go store. Schema collisions are not version upgrades.
- Windows CGO-disabled compilation uses the SQLite stub and proves no Windows database behavior. Linux and macOS runtime results must be read with their recorded scope.

## Evidence / 证据

Workspace `bench/evidence/cognitive-core-v1-integration-20261116/` contains `SOURCE.json`, command TSV files and raw JSON test logs. The [engineering report](COGNITIVE_CORE_V1_VERIFICATION.md) records actual PASS/FAIL/SKIP and any failures; the [final report](FINAL_COGNITIVE_CORE_V1_REPORT.md) states readiness. Nothing in this migration contract authorizes a production migration or release.
