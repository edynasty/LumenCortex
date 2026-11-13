# P0 native cognition storage decision

2026-11-11. Design before schema modification. Owner permits a necessary migration after explicit impact analysis and separate commit. No existing database is opened or migrated by this document.

**Current completion amendment (2026-11-13):** the isolated candidate adds 0017 after this unchanged 0016 design. Current API/data/lifecycle are in ADR-0023. Sections describing no Governor Apply are historical P0 scope, not the current host-only lifecycle API.

## Why existing rows cannot meet P0

`0014_cognition`/`0015_cognition_support` represent maintainer candidates with five equality assertions and immutable source/support identities; `proposed_claim` is capped at 2048 bytes. They do not represent native graph nodes/relations, five distinct time meanings, structured conditional strategies or outcome feedback. Reusing assertion JSON or session metadata for that graph would bypass the type/approval contract and privacy cleanup. Node graph tables are forbidden in the Go database.

## Decision

One additive Go migration, `0016_cognitive_graph.sql`, after mainline Hub 0013 and cognition/support 0014/0015. No second database/service, Node tables, or candidate-policy rewrite.

- `eval_results.cognition_epoch INTEGER NOT NULL DEFAULT 0`: a content-free snapshot of the existing maximum source-fence epoch when a result is appended. P0 reads reject reuse after a newer privacy fence. Deleting a failed session therefore cannot revive an older PASS. No check/failure tombstone or deleted source content is retained; original outcome/episode/fingerprint remain unchanged. A newer independent check can recover. Existing results at epoch 0 conservatively require revalidation if any privacy deletion has occurred.
- `cognition_nodes`: append-only revisions keyed `(id, version)`; `session_id` owns content and cascades with that session, `project_id`, `kind`, `created_at`, bounded valid JSON. Latest revision is the current projection; old revisions preserve observations and feedback until privacy deletion. Kinds cover the existing graph vocabulary plus Experimental `experience`; typed Go validation owns field meaning.
- `cognition_edges`: immutable IDs, source/target node IDs, owner `session_id`, bounded typed relation JSON. Endpoint existence, scope compatibility and privacy checks happen inside the same write transaction; no implicit cross-project links.
- Existing `derived_origins` records source-session dependencies for each write and experience use. Source revocation propagates through the existing fence mechanism. All new content joins the existing purge batches, including references owned by a derived maintenance session.
- Experience usage and feedback are append-only node revisions, not another result ledger or mutable success counter. Feedback refers to existing `eval_results` and exact approved bindings. The original proposal/result remains in its initial revision. Usage count is derived from unique run feedback, not model confidence.

Graph APIs are host/runtime operations, not agent tools. A caller cannot acquire verified status by submitting grade/trust. Framework check evidence is constructed and rechecked against existing approvals/results. Governor only reads and proposes; no Apply/Curator/epoch executor.

## Compatibility

`PRAGMA user_version` advances 15 → 16 through the existing lock/backup/transaction runner. Go refuses legacy Node schemas and incompatible historical Cognitive 13/14/15 databases before mutation. Older Go binaries reject version 16. Existing canonical v15 candidate rows retain their meaning.

## Rollback and recovery
- Empty new tables may downgrade 16 → 15 only when no privacy fence exists, through the existing explicit offline workflow; the epoch column is dropped. Any privacy fence refuses downgrade so an older reader cannot revive pre-deletion evidence. Normal Engine.Open never downgrades.
- Migration failure rolls back schema and version together; the runner's existing pre-migration scrubbed backup and checkpoint recovery remain authoritative.
- Nonempty new tables refuse downgrade, rather than discard graph/experience history or privacy dependencies. Preserve the v16 binary and state; destructive removal requires separate authorization.
- Crash during record mutation rolls back the revision, relations and provenance together. Retry uses expected-version CAS and stable dedupe identities. Restart reads the latest committed revision only.
- Privacy deletion erases every revision and related edge in existing bounded secure-delete batches, then the existing rebuild/WAL settlement; a cleaned fence is not removed or restored by graph revision.

## Required checks

Fresh/existing canonical v15 Engine open; graph restart; Node/old-Go/incompatible-branch refusal; collision and injected migration failure rollback; empty downgrade; graph/privacy-loss refusal; unchanged Hub/session rows; backup recovery; source/consumer fences; all-revision purge; read-only Governor. Tests use temporary databases; no real user workspace is migrated.

## Full-gate compatibility correction

Historical full-gate evidence found a latest-schema test permitting downgrade with a cleaned privacy fence. Native graph epoch semantics forbid it. `TestCognitiveGraphDowngradeRefusesKnowledgeAndPrivacyLoss` requires refusal for cleaned privacy fences; only cleaned non-privacy fences allow the older downgrade case. Original failed evidence is retained.

## Finalization sequence and branch collision (CF-MIGRATION-001)

Canonical sequence: platform 0010–0012 → **existing main Hub 0013** → cognition 0014 → support 0015 → native graph 0016. Main advanced externally to `208ad6b`; this task does not alter Hub SQL or the original branches. Earlier Cognitive-only 0013/0014/0015 and its reports remain historical, not the deployable sequence.

Before any writer opens, verify Hub tables and worker-delivery columns for v13+, cognition tables for v14+, support columns for v15+, graph tables/epoch for v16. Historical Cognitive 13/14/15 lacks the Hub family and returns `ErrSchemaMismatch` before backup, checkpoint marker or migration. `user_version` stays the only version authority; version-too-new remains `ErrSchemaTooNew`.

Do not change `user_version` manually to move between development branches. Preserve the old
database and its binary together; open a fresh state directory for the canonical candidate. The
existing offline, backed-up downgrade path is valid only for the matching schema family and still
refuses nonempty native graph or privacy-epoch loss. Historical branch evidence is never migrated.

`TestCanonicalSequenceRejectsHistoricalCognitionBeforeMutation` covers original SQL at 13/14/15, equal-version open, upgrade and downgrade, checkpointed and hot WAL, byte-identical rejection. `TestCanonicalSequenceRejectsIncompleteCurrentSchema` covers missing Hub/support/epoch fields. `TestCanonicalSequencePreservesHubAndRecoversBackup` preserves main13 Hub/session rows across 13→16→13→16 and recovery from the scrubbed pre-upgrade backup.

## Astra final audit continuation (2026-11-12)

Go/test candidate `9fa93bf` leaves all migration SQL and the registry byte-identical to audited source `e2c69c6`. Hub 0013 remains preserved; this audit creates no new migration. CF-ASTRA-001 uses existing transactionally committed `derived_origins` rather than trusting Experience JSON ownership during narrow-read selection. It therefore requires neither backfill nor schema relabeling. Final source-bound platform/migration results are in workspace `bench/evidence/cognitive-core-astra-final-audit-20261112/MIGRATION_REVIEW.md` and `TEST_RESULTS.md`. No existing user database was migrated, and this continuation is not production approval or independent acceptance.

## Completion schema 0017

`0017_cognitive_completion.sql` adds `cognition_applications`, `cognition_proposals`, `cognition_decisions`, `cognition_access` and lookup indexes inside the existing SQLite store. Applications separate pending admission, settled execution and independently checked feedback. Proposals/decisions remain bounded and source-derived; access counts are reader-local, not evidence. No old feedback is rewritten or reclassified.

The registry validates all four tables at version 17 before accepting a database. Upgrade/failure rollback uses existing transactions/backups. The down guard refuses any completion row or native memory/dependency/epistemic/revision/Run-bound-use metadata, because a v16 reader cannot preserve those semantics. A clean empty 17 can return to 16; nonempty original graph/epoch guards still apply to earlier downgrades.

New rows are included in source/consumer secure purge and recovery. `TestCognitiveCompletionMigrationRoundTripAndFailure`, `TestCognitiveCompletionDowngradeRefusesNewSemantics`, `TestCognitiveCompletionMissingTableRefusedWithoutMutation` exercise real temporary SQLite and hot WAL. No production migration, user database or manual user_version edit occurred.

### 中文：0017 的影响

0017 只在原 Go SQLite 中增加应用回执、待提案、决策和读者访问统计，不增加数据库、不重写旧反馈。现有 Runner 负责锁、备份和事务回滚；缺少任一新表时先拒绝再退出，不能部分打开。降级遇到新表数据或新 Node 语义时拒绝，避免旧程序丢失证据、依赖或隐私含义。仅空且无新语义状态可降回 16；更早降级继续受原生 Graph/隐私 Epoch 保护。测试均使用临时真实 SQLite，本轮未操作生产数据。
