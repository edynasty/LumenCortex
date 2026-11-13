# Cognitive Core — Data Model (migrations 0014–0017)

Unmerged completion candidate: Hub 0013 (unchanged), `0014_cognition.sql`, `0015_cognition_support.sql`, `0016_cognitive_graph.sql`, `0017_cognitive_completion.sql`. Historical Cognitive 13/14/15 is refused, not relabeled. Tables are STRICT in the same SQLite database; writes use pinned `BEGIN IMMEDIATE`, without model/Git work inside.

## 1. Tables

| Table | Rows | Authority | Lifecycle |
|---|---|---|---|
| `maintenance_state` | exactly one (`id=1`) when enabled | project's maintenance authority: `dirty_revision`, `cursor_revision ≤ dirty_revision`, `lease_id`, `generation`, `expires_at`, `pending_state ∈ {idle,running,failed}`, `last_error ≤512`, `policy_version` | created by `EnableCognition`; never deleted by Core |
| `maintenance_sources` | one per dirty revision | runtime-built `SourceRef` (`kind`, `source_id`, `digest` = `sourceVersion`, `scope`, `session_id` FK, `run_id`, `sensitivity`, `provenance = runtime`) | terminal status and later version-changing writes enqueue atomically; processed rows are deleted by commit, deletion cascades with the source session |
| `maintenance_dedupe` | one per processing of a source version | `dedupe_key` (PK, sha256 of authority/kind/id/version/scope/policy; `knowledge.RedoKey` of the previous key and its maintenance session for a processing that replaces one whose maintenance session was fenced), `outcome ∈ {candidates,no_candidate,skipped}`, generation, maintenance session | append-only; holds no source text |
| `cognition_candidates` | one per committed proposal | candidate/source/support identities, origin run/session, bounded claim/rationale, cited paths/checks, policy/status; 0015 adds assertion JSON, binding/support digests and purge timestamp | append-only except privacy text purge; commit status is historical |
| `cognition_observations` | one per qualified result per candidate | candidate FK (cascade), `check_id`, `(session_id, result_seq)` FK → `eval_results` (cascade), `episode_id`, `fingerprint`, `spec_digest`, `approval_epoch ≥1`, `outcome ∈ {passed,failed}`, `scope`, `purpose ∈ {adoption,revalidation}`, `run_id` | append-only; every field except the link is copied from the eval result row inside the inserting transaction; `run_id`/`session_id` decide run-bound freshness |
| `cognition_nodes` / `cognition_edges` | Versioned native records / immutable diagnostic relations | Scope, sources, evidence, Experience, epistemic class, confidence, pinned dependencies, memory policy, revision reason | CAS immutable history; lifecycle and privacy rules in ADR-0023 |
| `cognition_applications` | One per adoption (`adoption_id` unique) | Exact run/definition/actions/condition observations and settled execution IDs; 32 KiB JSON | pending → completed/failed/unknown; restart never replays actions; privacy purge |
| `cognition_proposals` | Bounded per session/run | Model-proposed structure, no authority; 16 KiB JSON | pending → completed/refused; terminal/restart settlement |
| `cognition_decisions` | Explicit observed model decision | Exact assessed node versions, applicability and source exposure; 16 KiB JSON | immutable receipt; existing source/privacy lifecycle |
| `cognition_access` | One per reader/node | Access count and last access time, separate from independent use/evidence | updated atomically; private to reader; privacy purge |

Indexes: `cognition_candidates(claim_digest)`, `(subject_key)`, `(support_digest)` (0015); `cognition_observations(candidate_id, check_id, id DESC)` and `(session_id, result_seq)`. Native graph revisions, edges and epoch in 0016 follow [storage decision](COGNITIVE_P0_STORAGE_DECISION.md).

Claim bindings reuse ADR-0017 `check_approvals` (`ActiveConstraintBindings`). Cognition binding digests include the current cognition policy; generic Task State bindings are unchanged. The adoption transaction rechecks the active exact binding and check approval. Existing R1 cognition bindings require host reapproval; no schema migration or automatic trust grant is performed.

## 2. Knowledge-state vocabulary

| Term | Stored? | Meaning |
|---|---|---|
| Runtime observation / source | `maintenance_sources` | A terminal top-level run Core recorded; evidence that work happened, not knowledge |
| Candidate (hypothesis) | `cognition_candidates` | Untrusted maintainer output with Core-validated references |
| `qualified` | status | At commit, the candidate is a `fact` with a structured assertion, an active host binding covers exactly that assertion (ref and value digest, project scope, configured check at the bound spec digest), and that bound check passed in a closed episode of the maintenance session |
| `candidate_only` | status | Well-formed and sourced, but free text, no binding covers its assertion, or the bound check failed / produced no qualified result |
| `rejected` | status | Malformed, carrying a self-assigned `checkIds`/`status`, or referencing what Core did not issue; audit only, never served |
| Qualified observation | `cognition_observations` | Bound check C gave outcome V at fingerprint F under approval epoch E, in run R; never “the claim is true”. Reusable per its freshness class (`version` or `run`) |
| Purged candidate | `purged_at` set | A source its maintenance batch read was privacy-deleted: no subject, claim, assertion, rationale, paths or reason remain; served as withdrawn without text |
| Applicability | computed per reader | `current`, `unknown`, `stale`, `source_missing`, `withdrawn`, `conflict`, `candidate_only` (`knowledge.Decide`) |

`qualified`/`validated`/epistemic `fact` describe recorded evidence history, not an unconditional verified-truth state. The strongest read-time applicability remains `current` for a particular reader; conflicts, source change and invalidation override it.

## 3. Invariants and where they are enforced

| Invariant | Enforcement |
|---|---|
| A source's current terminal version and dirty mark commit together | `enqueueSourceChange` is called by terminal status, final/message/context reset and Task State writes in their transaction (`TestTerminalStatusAndDirtyMarkAreAtomic`, `TestCognitionDeliveryFinalizationKeepsSourceQueued`, `TestCognitionHostMutationRequeuesFinishedSource`) |
| Only a current lease commits | `currentLease` re-reads id, generation, expiry, cursor, policy inside the commit (`TestLeaseTakeoverRefusesTheOldGeneration`, `TestCognitionLeaseTakeoverAcrossEngines`) |
| The cursor never skips or passes the dirty revision | `Through ∈ (cursor, dirty]` check + `CHECK (cursor_revision <= dirty_revision)` (`TestCursorCannotJump`) |
| A batch is all or nothing | single transaction with crash-injection hook (`TestCommitIsAllOrNothing`) |
| A source version is processed once per policy by a standing pass | `maintenance_dedupe` PK (`TestConcurrentWorkersCommitEachSourceOnce`, `TestMaintenanceLeaseAcrossProcesses`, `TestDedupeKeyChangesWithVersionScopeAndNothingElse`). A record whose maintenance session is fenced does not stand: `Store.ProcessingKeys` resolves the redo key, and the transaction that writes the fences queues the batch's other unfenced, unchanged sources again (`requeueCoBatchedSources`; `TestFableCoBatchedSourceRecoversAfterWithdrawal`, `TestFableProcessingKeysFollowFencedPasses`). The requeue is idempotent: one version of a source waits on one `maintenance_sources` row whatever the number of fences; while a lease is out the row is moved past the batch in flight instead of being trusted (`TestFableRequeueOfCoBatchedSourceIsIdempotent`) |
| A batch commits only if every source is still the version the maintainer read | `CommitMaintenance` recomputes `sourceVersion` and fences inside the commit, `ErrSourceChanged` (`TestCommitRefusesASourceChangedAfterItsBatchWasRead`, `TestIndependentSourceVersion`) |
| One support is stored once; new support for the same claim is recorded | support-digest duplicate check in the commit against live candidates; rejected rows and rows of a fenced maintenance session never count, a purged row's identities are unique placeholders (`TestSupportIdentityDecidesDuplicates`, `TestDuplicateClaimsAreNotInsertedTwice`, `TestIndependentDedupeAfterWithdrawal`, `TestFableCoBatchedSourceRecoversAfterWithdrawal`) |
| An observation comes only from a qualified eval result of the bound check | `qualifiedResult` + FK to `eval_results` (`TestObservationsAreDerivedFromQualifiedResultsOnly`, `TestAuditFalseClaimPassingUnrelatedCheck`) |
| An applicable persisted failure invalidates older PASS even without a candidate link | `CognitionResults` uses the same qualification predicate as `QualifiedResult`, and selects own/linked results plus supersession in one SQLite snapshot; only stable, approved, closed runtime passed/failed evidence participates. The database is workspace-bound; fingerprint/spec/approval/check identity isolate worktree and definition state. |
| A foreign failure is not the reader's own stale evidence, nor is a foreign PASS recovery | Runtime Run/Session and `Evaluator.InRun` checks still own positive reuse. A newly completed Work Unit in that evaluator can establish fresh evidence; `cognition_observations` stays adoption/revalidation history, not a duplicate result ledger. |
| Privacy success leaves no derived cognition text on Core-managed recovery paths | Derived-run cancellation/locks precede bounded purge, inherited-copy rewriting, VACUUM and WAL truncation; pending retries on Open (`TestCognitionPrivacy*`, `TestCognitionPurgeVacuumFailureRetainsPendingAndRetries`) |
| History is never overwritten | no UPDATE touches candidates or observations except the privacy purge of candidate text |

## 4. Migration behaviour

Upgrade 13→14 creates five cognition tables and four indexes after Hub. `TestCognitionMigrationRoundTrip`, `TestCognitionMigrationRefusesExistingObject`, `TestCognitionMigrationFailureRollsBack` cover rollback, collisions, recovery and preservation of preceding rows.

Upgrade 14→15 adds defaulted assertion/binding/support/purge columns and a support index. An older candidate has no current support until explicitly bound; no automatic trust is granted. `TestCognitionSupportMigrationRoundTrip`, `TestCognitionSupportMigrationRefusesExistingObject`, `TestCognitionSupportMigrationFailureRollsBack` cover rows and failure atomicity. Graph 15→16 and safe downgrade/refusal are covered by `TestCognitiveGraph*` and `TestCanonicalSequence*`. Offline cognition downgrade deliberately removes derived candidate state; graph or privacy epoch loss is refused.

Upgrade 16→17 adds application/proposal/decision/access records and a native kind index without rewriting old rows. Partial schema is refused before mutation. Downgrade 17→16 is allowed only with no completion rows and no native completion metadata/Run-bound uses; otherwise it refuses before backup/mutation. This preserves evidence and dependency semantics rather than dropping them. `TestCognitiveCompletion*` covers round trip, injected failure, hot-WAL missing tables and semantic-loss refusal.

Astra R2 preserves the v14 schema and old audit rows. Private rows are blanked only by privacy deletion. Append-only observation ordering is newest-first by ID; revalidation appends rather than overwrites. `qualified` is immutable adoption history, while `current` is computed for a reader and can never be inferred from that stored status alone. The full transition matrix and tested failure/recovery paths are in the R2 evidence directory.

The FABLE-04 follow-up also preserves schema v14 and the existing result/observation writers. `eval_results` append order is compared using `rowid` only inside one read statement; no rowid is exported or saved as an identity. `(session_id, seq)` remains the durable reference, including across privacy VACUUM/reopen. A result must be qualified at that statement's snapshot; a concurrently closing episode is either visible to the whole statement or to the next read. The ordering is result append order, not when a delayed cognition link is inserted. Qualification filtering does not delete error, interrupted or unapproved historical records.
