# Cognitive Core — Go API

Status: Cognitive Core v1.0 integration, input `8c30f75`, protected main `00344b5`, 2026-11-16. Unmerged; not a Core SemVer release. No HTTP/Wails exposure or Desktop consumer. Canonical schema sequence is Hub 0013 → cognition 0014/support 0015 → native graph 0016 → completion 0017; no 0018. [中文](COGNITIVE_CORE_API.zh-CN.md) · [Integration report](../reports/FINAL_COGNITIVE_CORE_V1_REPORT.md).

## 1. Engine (host) API — `runtime`

| Method / type | Contract |
|---|---|
| `Options.Cognition *CognitionOptions` | Per-Engine maintenance options. `nil` = default model, no background maintenance, default bounds. Inert until `EnableCognition`. |
| `CognitionOptions{Maintainer config.ModelRef; Auto bool; MaxSteps int; Timeout time.Duration}` | `Maintainer` routes maintenance runs through the existing catalog (empty = default model). `Auto` starts a background pass after each top-level run ends. Defaults: 6 steps, 90 s. |
| `(*Engine).EnableCognition(ctx) error` | Durable, idempotent per project database: creates `maintenance_state`. Completed/failed top-level runs and their subsequent version-affecting host/delivery writes enqueue a current source in the same transaction. There is no disable call in this task. |
| `(*Engine).CognitionState(ctx) (session.MaintenanceState, error)` | Dirty revision, cursor, lease id/generation/expiry, `pendingState` (`idle`/`running`/`failed`), `lastError`, `backlog`. `ErrCognitionDisabled` before enabling. |
| `(*Engine).MaintainCognition(ctx) (MaintenanceReport, error)` | One pass, at most one batch. Empty report when nothing is pending; `Held=true` when another worker holds an unexpired lease. Failure releases its lease and keeps sources dirty; stale lease/source changes commit no candidates. Manual and automatic passes inherit Engine cancellation; `Close` refuses new passes and joins verification and failure settlement before closing storage. |
| `MaintenanceReport` | `ran, held, generation, from, through, maintenanceSessionId, sources, skipped, inserted[], qualified, candidateOnly, rejected, duplicates, checks[], error`. `checks` are the bound checks Core ran to qualify candidates. |
| `(*Engine).ApproveConstraintBinding(ctx, ApproveBindingRequest)` / `(*Engine).RevokeApproval(ctx, RevokeApprovalRequest{Kind: RevokeBinding})` | Existing ADR-0017 APIs. A cognition binding approves `Assertion.Ref()`, `Assertion.ValueDigest()`, configured check/spec, scope `project` and the current cognition policy. Generic Task State bindings keep their prior digest. Old cognition bindings require explicit host reapproval; no agent can approve them. |
| `(*Engine).ReadKnowledge(ctx, ref) (any, error)` | Host read in the main checkout. `ref==""` → `KnowledgeList`; `ref=kc_…` → `KnowledgeView`. Read-only; runs no check. A host read has no run, so a run-bound observation is never fresh for it (§5). |
| `ErrCognitionDisabled` | Alias of `session.ErrMaintenanceOff`. |

## 2. Agent tool — `read_knowledge`

Offered to every run of a cognition-enabled project except the maintainer's (`internal/toolset/threads.go`, permission `read`, capability `context.read`).

- Arguments: `{"ref": "kc_…"}`; omit `ref` to list.
- List: `KnowledgeList{context, candidates[KnowledgeLocator], truncated}` — at most 20 live candidates, ids/enums/paths only. A content-bearing locator includes `maintenanceSessionId` for provenance; a withdrawn locator has no paths, check ids or source dependency.
- One: `KnowledgeView{context, id, status, applicability, reason, claimType, subject, claim, assertion, bindingDigest, rationale, sources[{ref, sessionId, withdrawn, missing, changed}], citedPaths[{path, blob, state}], checks[{id, freshness, atCurrent, observations[{outcome, purpose, current, episodeId, at}]}], maintenanceSessionId, createdAt}`. A withdrawn or purged candidate returns no text. A rejected or unknown id is “no candidate”.
- An assertion candidate's public `subject` and `claim` are Core's deterministic renderings; `rationale` is empty. Model prose stays in the private maintenance audit record, subject to privacy purge, never displayed as supported content.
- Every result carries `context` = `KnowledgeFraming`: candidates are model output; `current` means the check the host bound to the candidate's structured assertion passed freshly for this reader, nothing more; the workspace wins.
- Provenance: single-result sources and maintenance session, and every content-bearing list locator's maintenance session, become derived edges of the reading run. Start notices record equivalent source edges atomically with their transcript row. Source fences are rechecked inside each write transaction, so pre-withdrawal locators cannot commit afterwards. Deletion reaches notice/list-only readers as well as claim readers.

## 3. Start notice — `origin=knowledge`

`protocol.OriginKnowledge` user row appended to a top-level Start when scoped qualified candidates have positive relevance to its goal/input. At most 6 locators and 2048 bytes; task relevance precedes budget selection and matching conflict/unknown information is retained. Never subject, claim or rationale. Child/maintenance runs receive none. Before writing it, Start reruns at most 2 bound checks within 15 seconds; retrieval rank never grants evidence or authority.

## 4. Maintainer input and output contract

Input (`maintenanceInput`, the maintenance session's first message): one instruction line and one JSON document with the policy version, each included source's projection with its ref, and the **catalog of supportable assertions** — `{kind, target, key}` of every assertion an active host binding covers (no values, no check ids, ≤64). The whole serialized message is ≤ `MaxBatchInputBytes` (32 KiB); a source that would exceed it ends the batch and stays dirty.

The maintainer's final answer must be exactly `{"candidates":[…]}` (a ```json fence is tolerated), ≤16 KiB, ≤8 entries, each:

```json
{"claimType":"procedure|fact|failure|decision","subject":"≤120 B","claim":"≤1024 B (free text, absent with an assertion)",
 "rationale":"≤1024 B","sourceRefs":["session:<id>@sha256:<64 hex>"],"citedPaths":["workspace/relative"],
 "assertion":{"kind":"config_value|dependency_version|symbol_signature|file_exists|port_value","target":"…","key":"≤128 B","value":"≤256 B"}}
```

Only a `fact` may carry an `assertion`. Unknown fields reject the entry — a self-assigned `status` **or `checkIds`** is rejected: the maintainer cannot pick a check. Validation: `knowledge.Validate` + cited-path blob resolution + binding lookup and bound-check execution (§Security).

## 5. Source-neutral contract — `internal/knowledge`

`SourceRef{Kind, ID, Digest, Scope, RunID, SessionID, Sensitivity, Provenance}`; `Ref()` = `kind:id@digest`. For a session source, `Digest` is the session's **version** (`session.sourceVersion`: status, newest message seq, newest Task State version, final answer). `RunID` names the newest started run: `BeginRun` commits `agent.start{runId}` with status/rows; `sourceRunSQL` selects its greatest checkpoint sequence, and only when no such checkpoint exists falls back to the latest `main` request's run. A failed pre-admission run therefore cannot borrow an earlier episode. Timestamp truncation, ties or clock rollback do not affect identity. Auto-effort requests issued before a Start that never begins do not replace it. Checkpoint pruning preserves the newest `agent.start` (ICRA-002 / ICRA-V-001).

- `Kind` must be registered in `kinds`; only `session` has a producer (`enqueueSourceChange`). New kinds require a corresponding runtime producer; unknown kinds are refused by `Validate`/`ParseRef`.
- `Provenance` must be `runtime` (Core committed the record). `Sensitivity` `private` never enters a batch.
- `DedupeKey(ref, readerScope)` = digest of authority, kind, id, version digest, scope, `PolicyVersion`: a new version of a source is a new key and is maintained again. `RedoKey(key, maintenanceSessionID)` is the key of the next processing of a source version whose recorded processing no longer stands because its maintenance session was fenced (a source of its batch was revoked or deleted, which withdrew everything that session proposed). `Store.ProcessingKeys` resolves the chain; the earlier record is kept as written, and a `skipped` record always stands.
- `ClaimDigest` (claim identity) = an assertion's value digest, or a free-text claim's normalized subject and text. `SupportDigest` (support identity) = claim identity + source versions + source scope + policy + binding digest. Dedupe compares the support identity against live candidates only: a candidate of a fenced maintenance session is withdrawn and holds no support identity.
- `Freshness`: `version` for in-process schema checks whose dependencies are bounded regular files in the approved `Spec.Inputs`; reused only at the same fingerprint/spec/approval/scope. `run` for process checks; reused only by the producing run/session, with no intervening mutation or inherited environment change, within 5 minutes. Environment digests are transient, never persisted. The same qualification rule serves adoption and freshness: runtime provenance, approved definition, closed episode, stable pre/post/result fingerprint, and `passed` or `failed` (not `error`, open, cancelled, changed-scope or unapproved episodes). A later applicable failure in `eval_results`, including an unlinked Work Unit result, invalidates an older PASS at the next read. Another run's failure yields `unknown`, not borrowed `stale`; another run's later PASS cannot revive the old PASS. A new qualified check performed by the reader's own evaluator can restore `current` without requiring a synthetic cognition observation. `checks[].observations` remains the appended adoption/revalidation history; `atCurrent` may also be decided by the reader's own unlinked Work Unit result.
- `Decide(Facts) (Applicability, reason)`: fixed order withdrawn → source_missing (session or cited path) → conflict → stale (cited change) → stale (source version changed) → candidate_only (not qualified at commit) → candidate_only (binding no longer active) → current/stale (fresh bound-check outcome) → unknown.
- `EvidenceLevel(Applicability)`: `current` 3, `unknown` 2 (host-bound, no fresh result), `candidate_only` 1, anything else 0. A candidate is in `conflict` only when another candidate about the same subject, with a different claim identity, has at least its own level before any conflict is applied. Model output without host support never changes what a host-supported fact is to a reader; among equals Core does not pick. A read by id compares the candidate with the live candidates of its own subject (`CandidateFilter.SubjectKey`), not with a window over all subjects.
- `PolicyVersion` hashes the caps and participates in support identity and cognition binding digests. A policy change does not replay consumed sources automatically: explicit host reapproval plus a new independent source or source version establishes new support.

The cognition packages import no DAP, LSP, MCP or Eval package. DAP/LSP/MCP/Eval APIs are unchanged.

## 6. Session store API — `internal/session` (internal)

`EnsureMaintenanceState`, `ReadMaintenanceState`, `AcquireMaintenanceLease`, `RenewMaintenanceLease`, `MaintenanceSources`, `ProcessingKeys`, `CommitMaintenance` (source CAS and commit-time active exact support), `FailMaintenance`, `QualifiedResult`, `CognitionResults`, `AppendObservation`, `Candidates`, `Candidate`, `Observations`, `SessionPurpose`, `SourceVersion`, `CreateDerivedSession`; reused: `ActiveConstraintBindings`, `BeginPrivacyDeletion`/`PurgeStep`. `CognitionResults` selects qualified own/linked evidence and later same-state failures in one read snapshot. Append order uses `rowid` only inside that statement; durable identities remain `(session_id, seq)`. Privacy deletion stops derived runs, purges actual dependencies, then rebuilds/checkpoints SQLite. Errors: `ErrMaintenanceOff`, `ErrNothingPending`, `ErrMaintenanceLeaseHeld`, `ErrStaleLease`, `ErrSourceChanged`, `ErrNotQualified`, `ErrCandidateNotFound`. Hub's unrelated `ErrLeaseHeld` still means a work-unit Task Lease conflict.

`internal/evaluator`: `(*Evaluator).InRun(seq)` exposes in-run reuse (closed by this evaluator, no mutation or environment change since). Start re-reads binding authority after revalidation; a revoked binding cannot appear `current` in the index.

## 7. Runtime events (notifications only)

`cognition.maintained`, `cognition.inherited`, `cognition.read`, `cognition.revalidation.failed`, `cognition.inheritance.failed`. Dirty tracking never depends on them.

## 8. Experience read and privacy boundary

`ReadExperience(ctx, readerID, id, version)` and `ReadCognitiveNode` return the shared definition and original validation plus only that reader's own `Uses`. Empty, origin-only and unrelated readers receive no consumer uses. Experience mutation and idempotent responses apply the same projection; internal transitions retain the complete immutable history. Consequently `Experience.Statistics()` reports the returned reader's uses, not project-wide totals.

Origin-only Context / `read_thread` locators derive from the origin; a `needs_revision` warning also derives from the aggregate owner because it conveys a consumer counterexample. Existing historical lineage is never narrowed. Adoption still requires current independent checks and a commit before returning strategy. See [the privacy model](EXPERIENCE_LEARNING_DESIGN.md#privacy-and-safety) and [regression evidence](COGNITIVE_CORE_TEST_PLAN.md#3g-owner-authorized-astra-continuation-review-of-546692d).

V-D1 is an accepted v1.0 availability limit: any recorded consumer's revocation/deletion fences the aggregate owner, making the shared Experience unavailable to every reader; privacy deletion purges its derived content. A dependent that only cited the validated origin is not itself fenced, but its missing premise is stale. This is not per-consumer storage separation. Emptying the returned `Uses` never bypasses the underlying fence, and history reads do not restore access.

## 9. Completion host API

| API | Contract |
|---|---|
| `CognitiveScopeFor(ctx, sessionID, kind)` | Derives host-owned project/workspace/session/reader/source scope; no global store. |
| `PutCognitiveNode(ctx, owner, node, expected)` | Creates/revises a proposal. No caller Evidence/VerifiedAt/fact promotion, audience widening or lineage drop. Canonical bindings and existing aliases require the lifecycle path. |
| `VerifyCognitiveNode(ctx, owner, id, expected)` | Runs exact host-bound check; appends the actual observation. A failed check retains invalidation, never refreshes an invalid identity. |
| `ReadCognitiveNode` / `ReadExperience` | Historical/latest read subject to today's source/scope/fences and private Uses projection. Stored status is not current-use authorization. |
| `AssessCognition(ctx, reader, query, ids)` | Read-only assertion assessment, at most 16 records including dependencies/conflicting peers. Returns items, recommendation, source sessions and incomplete coverage. No execution permission. |
| `CognitiveMemory(ctx, reader, id)` | Logical class, host policy/protection, reader-local access count/time and private outcome counts. Utility stays `causal_benefit_unmeasured`. |
| `GovernorAnalyze(ctx, GovernorAnalyzeOptions)` / `GovernorPlan(ctx, nil, options)` / `GovernorValidate(ctx, GovernorPlan)` | Real normal-SQLite host diagnostics and deterministic planning. `runtime` aliases expose the types to external Go hosts. The model curator returns `ErrGovernorCuratorDisabled` before calling any provider; validation is not an execution authorization. |
| `GovernorApply(ctx, owner, plan, expected, reason)` | Host-selected owned lifecycle batch: tiers, archive, equivalent canonicalization. CAS and all-or-nothing transaction. Branch/promote/epoch rejected; canonical target cannot change in the batch. |
| `SetCognitiveLifecycle(ctx, owner, id, expected, action, reason)` | `archive`, `restore`, `revoke`; protects safety/counterexamples and forbids invalid identity revival. Restore checks current sources/dependencies/time; revoking an alias preserves its historical canonical pin even after target advancement/withdrawal. |
| `MaintainCognitiveMemory(ctx, owner, capacity)` | Archives only stale/dormant unprotected unreferenced owned nodes. Reports active/protected/archived and pressure; no destructive GC. |
| `AgentResult.Cognition *CognitionCompletion` | Extracted/feedback counts and memory-phase errors, separate from main Agent outcome. |

Host Experience extraction/validation/adoption/revision/feedback APIs remain available. Real application is intentionally through the admitted Agent's tool callback, not a host command-replay API. Host reads are diagnostics, not automatic model exposure.

`AssessCognition` query mode searches native `belief` records only, including title/body/tags and structured assertion fields. Explicit IDs can select other native kinds. Invisible/missing dependencies are not disclosed; their dependents become `stale / dependency_changed`. Uses-only Experience revisions preserve the pinned premise, while definition/state/validation/scope/lifecycle changes still invalidate it. Host tier changes also remain invalidating (accepted V-002 limit). Concurrent adoption can return `ErrCognitiveRevision`; there is no automatic adoption retry. Governor validation rejects alias chains, but some dormant/stale or same-batch target conflicts are refused only by the lifecycle transaction (accepted V-003 limit).

## 10. Ordinary Agent tools

Registered only with cognition enabled, outside maintenance. The Engine binds session/run identity; model input cannot select another consumer or grant authority.

| Tool | Input | Effect |
|---|---|---|
| `recall_experiences` | `{query}` | At most 8 relevant locators, source provenance and reader-local access telemetry; no strategy-use credit. `applicability` is `requires_condition_check`, or `use_limit_reached` for a record that can take no further use: its eight Uses are spent, or its 64 KiB cannot hold another use together with the feedback of the uses still open. Such a record accepts no adoption. |
| `adopt_experience` | `{id, version}` | Independently checks conditions and commits one-use identity for this run before returning strategy. |
| `apply_experience` | `{id, adoptionId, actions:[{tool,arguments}]}` | Matches the origin's complete canonical action sequence, rechecks conditions, commits pending receipt, dispatches ordinary authorized tools, settles execution IDs, then observes the expected outcome with its host-bound check before the run continues. The result reports `outcomeCheck`: `held`, `not_held` or `not_observed`. One application per adoption; no replay. |
| `propose_experience` | `{proposal}` | Queues a bounded structured Experience proposal; terminal finalization supplies actual episode and checks. |
| `assess_cognition` | `{query, ids?, action}` | Evaluates exact premises and contradictory peers, commits a revision-bound decision and source exposure. `action`: continue/gather_evidence/ask/defer/stop. Unsupported continue stays gather_evidence. |

The model cannot call Governor lifecycle/approval APIs. `apply_experience` grants no broader tool access: ordinary policy, effective specs, workflow, hooks and budget still apply. Eval remains non-recursive and cannot gain foreground shell access through Experience.

Each nested action observes the current Workflow and transition hooks, atomically commits its execution/Task State/provenance, and recomputes authority before the next action. Background shell mode is refused before submission. Exact execution ID/outcome is dispatcher-owned host metadata (`json:"-"`); settlement never reconstructs identity from the latest matching call. Nested result sources commit before exposure and appear in the outer `sourceSessions` union.

Complete settled failed actions can produce independently checked counterexamples; failed negative procedures can confirm a warning. Denied, interrupted, partial and unknown applications remain unknown without an outcome check. `Engine.Open` settles abandoned pending applications as unknown and finishes recoverable memory work; it never repeats tools.

The outcome of an application is the observation on its receipt, made when its last action had settled. The settlement at the end of the run, a host `RecordExperienceFeedback` and the recovery of `Engine.Open` record that observation and run no check: what the run, or anyone, changes after the application is neither a success nor a counterexample of the procedure. An observation whose approval lapsed before the settlement (a revoked binding, a privacy epoch) is not replaced by a later check; the use is recorded as unknown.

## 11. Frozen activation and compatibility boundary

- Import the public `runtime` package, not `internal/*`. The host owns reader/session identity; these SDK arguments are not an authentication mechanism for an untrusted network client.
- `EnableCognition` enables tracking and ordinary tools; `CognitionOptions.Auto` separately opts into bounded model maintenance. No automatic model Governor or hidden background learner is enabled.
- Start advertises maintenance candidates only. Experience needs explicit `recall_experiences` / `read_thread`; applying stored procedures is never automatic. Proposal finalization, application outcome recording, terminal settlement and restart recovery are automatic once their prerequisites exist.
- Native belief/negative/abstraction creation and verification, Governor Apply, archive/restore/revoke and memory capacity maintenance require explicit host calls. SDK availability is not automatic production use.
- `cognition_candidates` and `cognition_nodes` share assertion/check/source/privacy contracts, but not a merged lifecycle, automatic bridge or cross-family conflict view.
- Experience is a **Verified Procedural Macro**: exact observed actions, at most eight lifetime Uses and 64 KiB per node, often less because feedback/lifecycle room is reserved. `success` means reproduced actions plus the receipt's qualified outcome; a pre-existing outcome can satisfy it. `causal_benefit_unmeasured` remains unchanged.
- The Go signatures and JSON meanings documented here are the v1.0 capability baseline. No runtime version constant, Core release tag, Desktop pin or database schema is bumped by naming it v1.0.
- Supported migration begins at canonical main Hub 0013 or a fresh Go store. Unreleased early-candidate stores with random Experience owners or previously misvalidated failed actions are not converted; preserve the original database and use a fresh test state directory. Historical branch collisions and legacy Node stores remain refused. See [migration verification](../reports/COGNITIVE_CORE_V1_MIGRATION.md).

[Production entrypoint matrix](capabilities.md) · [Accepted limits and evolution](roadmap.md) · [Finding closure](finding-ledger.md).
