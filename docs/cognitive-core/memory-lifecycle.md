# Cognitive Memory Lifecycle

**Cognitive Core v1.0, isolated input `8c30f75`; not merged or released.** [中文](memory-lifecycle.zh-CN.md) · [Architecture](architecture.md) · [Integration evidence](../reports/FINAL_COGNITIVE_CORE_V1_REPORT.md).

## Four roles, one store

| Role | Authoritative data | How it becomes useful later |
|---|---|---|
| Working | Task State, Notes/notebooks and the bounded current context | The ordinary Agent reads its goal, constraints, open questions and checked outcomes. Compaction does not turn private reasoning into durable memory. |
| Episodic | Session/run identity, answered requests, execution IDs, terminal outcomes and `eval_results` | Extraction refers to the actual episode and its source version, not the model's claimed success. Failed and incomplete executions remain distinct. |
| Semantic | Maintenance candidates plus native `belief`, `entity`, `abstraction` and related graph nodes | Structured assertions gain evidence only through host-bound checks. Summaries keep `Sources` and pinned `Dependencies`; text remains a proposal. |
| Procedural | Typed `Experience` in native graph revisions | Current conditions, original action trace and explicit independent application connect a prior episode to a later task. |

Short/long term are roles and retention policies, not separate databases. `MemoryClass()` exposes the graph's logical class; Task State and Session history keep their existing storage contracts.

## From episode to future application

1. `propose_experience` records one bounded structured candidate for the active run. No source identity, result or verification grade is accepted from the model.
2. At the existing terminal boundary, `finalizeCognitiveRun` calls extraction and validation against the real source. Origin task/run, tool argument digests, executions, cited blobs and check identities are immutable evidence. Every run that was offered the Experience tools settles its uses, applications and proposals at this boundary, a subagent or task child included; only maintenance source capture and the background trigger stay with top-level runs.
3. An incomplete/truncated trace, missing approval, failed condition or unsupported expected result cannot validate a positive procedure. Negative expected results retain their negative meaning.
4. Recall selects relevant scoped locators. Adoption independently checks current conditions, records a one-use identity and only then returns strategy.
5. `apply_experience` explicitly receives the observed tool/argument sequence. It checks conditions and definition, commits admission, and calls ordinary policy-controlled tools. Each action observes the live Workflow and atomically settles Task State/execution/source edges before continuing; gates and background-shell refusal cannot be bypassed.
6. A complete sequence of distinct, ordered dispatcher-issued execution identities and terminal outcomes, including real failures, is use evidence. The declared outcome is checked once, when the last action has settled and before the run continues; feedback records that observation and never a later state of the workspace. A check for an outcome that appears with a delay has to wait for it itself. Pending, denied, interrupted, partial or unknown execution remains unknown; identical arguments cannot borrow another action's receipt.
7. A counterexample appends `needs_revision`. The origin may propose a changed definition, but validation resets and historical Uses/feedback remain. The next consumer must establish conditions again.
8. Restart finishes memory settlement under the existing run lock without replaying actions. Check/storage errors appear separately in `AgentResult.Cognition` and recovery events.

Started-run identity is independent of successful request admission: `agent.start` is written by `BeginRun` and ordered by checkpoint sequence. A later pre-admission failure cannot borrow an earlier run's episode, proposal or application. Uses-only revisions preserve a pinned Experience premise; new validation, definition, counterexample, scope, status or host memory-policy changes do not.

## Compression and consolidation

These operations deliberately have different authority:

- **Context selection/compaction:** existing context budgeting and Notes/recall make a bounded request projection. No change to durable truth, evidence or source history.
- **Episode summary:** model-supplied, bounded Experience fields or a host-proposed `abstraction` refer to sources and pinned premises. It is not an independently verified summary of arbitrary prose.
- **Semantic consolidation:** Governor may archive equivalent aliases with a `Memory.Canonical` exact revision. Owner, scope, kind, assertion, body, source/evidence, dependencies and validity must match. A new binding requires a current active target unchanged in the same batch. Generic writes cannot alter aliases; later target changes do not block revocation with the historical pin. History is retained, not deleted.

Non-equivalent facts, failed counterexamples and different valid intervals are not flattened into one summary. Broader interpretation requires a new proposal with provenance and new checks. There is no automatic universal generalization claim.

## Separate signals

`Engine.CognitiveMemory(readerID, nodeID)` exposes only the reader's own access and outcome information.

| Signal | Stored/computed meaning | Not a correctness claim |
|---|---|---|
| `Access.Count` / `LastAccess` | Reader-local `recall_experiences` tool exposure count/time in `cognition_access`; direct host reads are uncounted | Repeated viewing is not independent use or evidence. |
| `Outcomes.Adoptions` | Unique visible adoption records | Adoption itself does not mean any action happened. |
| Success/failure/unknown/unconfirmed | Derived from immutable visible feedback | Outcome match does not establish causal benefit. |
| Model confidence | Optional proposal confidence | Never upgrades applicability or bypasses a check. |
| Importance/retention | Host-owned `MemoryPolicy` | Important information can be uncertain or false. |
| Evidence/applicability | Exact approved result plus current source/time/scope | Retrieval rank, age and frequency cannot manufacture evidence. |
| `Utility` | `causal_benefit_unmeasured` | No synthetic reward or unmeasured learning gain. |

Thread recall's existing 30-day recency component remains a ranking weight, not a TTL. All native revisions preserve actual feedback; summaries do not replace it with a mutable score.

## Retention, archive, revoke and restore

- `MaintainCognitiveMemory(owner, capacity)` controls the active owned set. It archives only already stale/dormant, unprotected, unreferenced records, lowest host importance first, at most 512 revisions per transaction.
- Protected: task nodes, negative nodes, failed/contradicting evidence, negative-origin/negative-result Experiences, counterexample feedback, `needs_revision`, safety/counterexample retention and importance at least 0.8. Dependencies and graph references are protected during capacity maintenance.
- **Archive** removes a record from active use but keeps its history. Generic node revisions and lifecycle APIs cannot evade protection by dropping retention metadata in the same archival write.
- **Restore** requires current source, dependency and valid-time checks; it does not validate content. Revoked/invalidated identities cannot be revived; a corrected claim requires a new identity.
- **Revoke** appends an immutable invalidation. Source revocation uses the existing fence graph and prevents dependent reuse.
- **Privacy delete** uses the existing stop/fence/secure-purge/rebuild/WAL recovery path and removes all relevant revisions, applications and access rows. It is not a capacity shortcut and is not reversed by Restore.
- **Shared-Experience availability (V-D1):** any recorded consumer's withdrawal fences the shared owner; all readers lose that Experience, and privacy deletion erases its derived content. Origin-only dependent sessions are not themselves fenced, but assessment marks the missing premise stale. Restore cannot bypass the fence. This is an accepted v1.0 limit, not a pending permission to weaken deletion.

If the active/protected set exceeds the requested capacity, `Pressure=true` is returned. The system does not delete a rare safety fact or counterexample merely to hit a number. Hot/warm/cold is logical metadata, not physical storage tiers.

## Bounds and limitations

Native nodes remain at most 64 KiB; source refs at most 8, dependencies at most 16, dependency depth at most 32. Assessment returns at most 16 records and marks incomplete coverage; an application has at most 8 actions, an Experience at most 8 Uses. The serialized byte bound may be reached earlier. These are refusal boundaries, not silent eviction or compression of evidence. The eight Uses are a bound on the record for its whole life: once they are spent, recall reports `use_limit_reached` and adoption is refused.

Two revisions must still fit a record that content has filled, so every other revision leaves their room. One is the host's lifecycle decision: 4 KiB are kept in every node, and a full record can still be archived, revoked, tiered or consolidated with any reason the API accepts. The other is the settlement of an Experience use: the room of its feedback is kept from the adoption on, so a use that was applied can record its checked outcome, a counterexample included. A record whose bytes cannot hold another use with that room admits none: recall reports `use_limit_reached` and adoption is refused before anything is applied. The room is measured for sessions Core created and worktree paths up to 256 bytes; beyond that the store may refuse an admitted adoption, and a longer feedback is taken from the lifecycle room.

Large Experience sets and whole-graph Governor diagnostics still scan the selected domain; this candidate adds no ANN/embedding database or automatic destructive GC. Source advancement can make old Experiences unavailable; re-extraction from a new genuine episode is required. Privacy protection covers Core-owned live state, not external backups or forensic erasure. No longitudinal learning benefit is measured.

Maintenance candidates and native graph nodes share evidence and privacy authority, not a single record lifecycle. Automatic Start notices cover the former, while Experience needs explicit recall. Host-created belief/negative/abstraction and host-requested retention are SDK capabilities; no automatic writer or background Governor is added. See [production entrypoints](capabilities.md) and the [v1.1 roadmap](roadmap.md).
