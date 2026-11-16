# Cognitive Core v1.0 — final integration

[中文](FINAL_COGNITIVE_CORE_V1_REPORT.zh-CN.md)

**2026-11-16 — engineering integration complete.** The Owner-defined v1.0 implementation and verification gates are complete within the documented platform bounds. macOS normal and Race each cover **3005 PASS / 0 unresolved FAIL / 46 SKIP** on the frozen source; original package-timeout failures remain preserved and were completed by runtime-only checks, not relabeled as one clean full-suite invocation. Ready for Owner-authorized main integration. Cognitive Core v1.0 is a capability baseline, not a Core SemVer release, human-level cognition or measured autonomous-learning gain.

## Scope and authority

- Owner-selected input: `8c30f75e063db69a1dbc867741dc8f042c7d0dbc`, including Astra, Fable, Carbon and Opus fixes. Protected local and remote main: `00344b5d6a67673fea9fcbc90e0cab16de17dc57`.
- Isolated branch: `integ/cognitive-core-v1-20261116`; worktree: `wt/core-cognitive-v1-integration-20261116`.
- Freeze the existing Go implementation and schema 0014–0017. No 0018, Context Space implementation, Desktop change, paid model request, production database mutation, merge, push, tag or release.
- Preserve V-D1 fail-closed shared-Experience withdrawal, V-002/V-003 limitations, and the distinction between host SDK capabilities and automatic Agent integration. Experience is a **Verified Procedural Macro**; measured causal learning benefit remains `causal_benefit_unmeasured`.
- Main's pre-existing `AGENTS.md` modification and all historical worktrees/evidence remain outside this change. `bench/evidence/cognitive-core-v1-integration-20261116/BASELINE.json` records branch/HEAD/status and the existing worktree/index diffs. Workspace evidence and `TODO.md` are outside Core Git.

## 1. What was integrated

The selected input is a descendant of protected main and all specified recent review commits. Remote main was checked with `git ls-remote` at task start; no remote reference was changed. An isolated worktree was created from the exact input, without replaying, squashing or dropping fixes.

The 33-commit lineage includes Astra completion (`949b2c6`/`c1d3229`), Fable's 17 FFR fixes (`c6bdfaf`/`f2dfa286`), Carbon ICRA-001–004 (`f8a87f5`), and Opus ICRA-V-001–003 plus V-T1 (`8c30f75`). Older P0, ACR, `e2c69c6`, `61a1d31` and `91af861` work was already imported by completion; preserved parallel branch tips were not cherry-picked again. [Git report](COGNITIVE_CORE_V1_GIT.md) and workspace `GIT_RELATIONSHIPS.json` give the exact chain and protected worktrees.

**Production code, tests and SQL did not change in this integration.** All 893 Go/module/SQL files retain source-map SHA-256 `ff95fd3bcb8585810f121c73dd1be1450d29ad2fb35b4e53c5ef16e56964926a`; Linux ran the same map. No candidate-modified Go file needed formatting. Unrelated pre-existing gofmt deviations were preserved. This task changed current bilingual documentation, reconciled finding closure, corrected migration claims and produced new evidence rather than inventing another cognitive subsystem.

## 2. Stable architecture and seven domains

The stable public boundary is the existing Go `runtime.Engine`; models propose and select actions, while Framework owns source identity, evidence qualification, execution receipts, CAS, fences and lifecycle transactions. Hidden model reasoning is not durable project state. Storage remains the existing SQLite database and Session/Task State/Context Graph infrastructure.

| Domain | Concrete v1.0 behavior | Cooperation and current limit |
|---|---|---|
| Memory | Working Task State/Notes, episodic runs/executions/checks, semantic candidates/native nodes, procedural Experience; versioned sources and host retention | One store, distinct roles. Logical tiers and equal-evidence consolidation do not erase counterexamples or promote popular claims. No physical cold store or universal prose summarizer. |
| Attention | Existing lexical/symbol Light, task-ranked candidate Start notices, scoped Experience recall and warnings | Relevance is separate from correctness. Experience needs explicit recall; there is no automatic Experience Start index or large-scale retrieval-quality claim. |
| Reasoning | Exact assertions, pinned premises, evidence applicability, conflicting peers and immutable decision receipts | Framework distinguishes fact/inference/hypothesis/experience/decision/unknown and current/stale/conflict. The model reasons; the framework is not a universal causal proof engine. Query mode searches beliefs only; explicit IDs/dependencies remain bounded. |
| Learning | Proposal → real-origin extraction → approved validation → recall → adoption → explicit exact-action application → outcome-at-application-end → feedback/revision/recovery | Real ordinary Agent/tool/SQLite paths are wired. A complete observed procedure is a Verified Procedural Macro, not parameterized transfer or a learned general rule. |
| Belief revision | Source/version/time/dependency checks, CAS history, invalidation and counterexample `needs_revision` | A changed premise stops reuse. Models cannot approve their own revisions; native belief/negative/abstraction writers and definition changes require explicit host calls. |
| Governance | Native SQLite Analyze/Plan/Validate; host-only archive/tier/equivalent-consolidation Apply, retention, revoke/restore | No model Curator or background Apply. Advisory prevalidation is not the transaction authority; invalid plans still fail closed. |
| Verification | Host-approved exact checks, real execution identity, closed episode, scope/fingerprint/run/approval epoch and result | No model self-assessment as PASS; partial/interrupted/unexecuted checks stay unknown. Local result scope cannot certify unrelated claims or global correctness. |

The seven domains cooperate through shared Scope, Time, Provenance, Evidence, ownership and privacy/CAS authority. They are **not** seven disconnected loops. However, `cognition_candidates` and `cognition_nodes` still have separate lifecycle/read/conflict paths; v1.0 does **not** claim one fully merged record model. This is an explicitly accepted architectural boundary, not a repaired defect.

Cognitive Core and Context Space remain independent packages in one Go module. Only `runtime` composes them. No change to `internal/window`, ProjectPrefix, five-region budgets, Reference Directory or Safepoint Compaction was introduced. Context Space starts on a separate branch only after Owner-authorized cognitive integration reaches main.

## 3. SDK availability versus actual production calls

| Entry | What actually runs | What does not happen automatically |
|---|---|---|
| `EnableCognition`, `MaintainCognition`, optional `CognitionOptions.Auto` | Explicit maintenance, or opt-in bounded maintenance after top-level completion, produces checked maintenance candidates | Opt-in is required; this task made no paid maintenance request. Candidates do not automatically become native graph nodes. |
| Candidate Start attention | Up to six scoped candidate locators / 2048 bytes through `BeginRun(Notices, NoticeSources)` | No Experience Start index. |
| `propose_experience`, `recall_experiences`, `adopt_experience`, `apply_experience`, `assess_cognition` | Model-selected tools on eligible ordinary runs; normal workflow/policy/hooks/budget apply | Offering a tool does not prove the model selected it or that an action ran. Adoption is not successful use. |
| `finalizeCognitiveRun`, `AgentResult.Cognition`, `Engine.Open` | Proposal/use settlement after eligible runs including children; interrupted memory settlement on reopen | Recovery does not replay tools or replace the application-end outcome with later workspace state. |
| `PutCognitiveNode`, `VerifyCognitiveNode`, `ReviseExperience` | Explicit trusted host writes/verifications/revisions through existing authority | No automatic native belief, negative or abstraction writer; no autonomous definition rewrite. |
| `GovernorAnalyze/Plan/Validate/Apply`, `SetCognitiveLifecycle`, `MaintainCognitiveMemory` | Explicit host request; owned lifecycle CAS transaction and protected retention | No scheduled autonomous Governor or model-approved lifecycle mutation. |
| `RevokeSource`, `DeleteSession` | Explicit host action, framework fence propagation/purge/recovery | Restore cannot undo a privacy deletion or revive a withdrawn identity. |

The complete method/JSON contracts and production trigger matrix are in [API](../cognitive-core/COGNITIVE_CORE_API.md), [data model](../cognitive-core/COGNITIVE_CORE_DATA_MODEL.md) and [capabilities](../cognitive-core/capabilities.md). “SDK-only” means working host capability, not a stub; it also does not mean automatic Agent activation.

## 4. Learning, memory and governance boundaries

Experience extraction uses the real terminal origin, actual RunID, observed tool/argument digests and qualified checks. `agent.start` checkpoint sequence prevents a pre-admission failure from borrowing a prior run. Validation does not accept a failed action as positive evidence. Adoption rechecks conditions and records one identity; `apply_experience` must reproduce distinct, ordered, exact dispatcher executions under current permissions. It cannot count unrelated actions, denied writes, background submission or unknown receipts as successful reuse.

The expected outcome is observed once when the last application action settles, before later work can change it. Feedback preserves that receipt, and counterexamples stop future reuse until an explicit revision is validated. Uses-only revisions no longer stale an unchanged pinned premise; genuine definition/evidence/scope/state changes still do. Consumer projections reveal only that reader's Uses and telemetry. These mechanisms are exercised by the real public demo and regression paths, not inferred from API names.

Frequency, recent access, host importance, model confidence and checked outcomes remain distinct. Capacity maintenance protects safety knowledge, unknown/future-valid records, negative evidence and counterexamples; if only protected/active data remain it reports pressure rather than evicting them to hit a number. Consolidation archives only equivalent evidence-bound aliases, preserving revisions and source dependencies. Restore rechecks validity; it does not create proof.

## 5. Verification and failures retained

[Build/Vet/Test/Race and platforms](COGNITIVE_CORE_V1_VERIFICATION.md) records every command and separate PASS/FAIL/SKIP accounting. [Migration verification](COGNITIVE_CORE_V1_MIGRATION.md) covers the frozen 0014–0017 chain, upgrade, backup recovery, injected rollback, hot WAL refusal and semantic-loss downgrade guards.

- macOS and Linux full-module Build/Vet passed. Windows amd64 Build/Vet/test compilation passed with CGO disabled; Windows runtime and SQLite remain untested.
- Native Linux cognitive Race initially produced 419 PASS / 6 FAIL / 2 SKIP. Both failing families invoked absent `curl`. A matching curl package was unpacked only in the owned temporary directory; the corrected two-family run produced 6 PASS / 0 FAIL / 0 SKIP. Unique same-source coverage is **425 PASS / 0 unresolved FAIL / 2 SKIP**, not one green full invocation. An intervening wrong-cache setup failure is also preserved.
- macOS original full normal/Race commands hit the default 10-minute `runtime` package deadline, with no failed test terminal event before it. Runtime-only completion with `-timeout=30m` passed: **908 PASS / 0 FAIL / 21 SKIP** for each mode (845 s normal command, 1143 s Race). The 42 already-passing packages plus completed runtime give **3005 PASS / 0 unresolved FAIL / 46 SKIP**, separately for normal and Race. The two modes have identical test-name sets; no data race was reported. Original counts/stack traces remain, so this is complete same-source coverage, not a single successful full invocation.
- The offline public `lcx-experience-demo` exited 0: real origin write/check, candidate/validation/recall/adoption, later fresh output, one checked feedback, persisted reopen and wrong-condition refusal. Its temporary state was removed. No real model or network model request occurred.

Linux means native linux/arm64 processes in Ubuntu 24.04 userland in a pre-existing privileged Docker Desktop/LinuxKit container, UID 1000 with systemd user delegation; not an Ubuntu VM/physical host or full Linux platform certification. No sandbox or privacy guard was disabled to pass tests.

## 6. Findings and independent evidence

[Finding Ledger](../cognitive-core/finding-ledger.md) reconciles all prior closures without rewriting original reports. Fable's 17 fixes were independently checked by Carbon; Carbon's four were independently red/green verified by Opus. Opus's three code fixes received subsequent independent source-level architectural acceptance; their original red/green runs remain Opus self-verification. These are different evidence levels, not a claim that the architecture reviewer reran tests.

This integration was implemented and judged by primary Astra (`local/gpt-6-astra`), without a substitute implementation agent or a new fifth full audit. The current integration checks are self-verification, not a new independent PASS. No new runtime defect was established: the observed blockers were missing verification prerequisites and an inadequate package deadline. A documentation error was corrected: 0017 semantic downgrade guards fail inside the transaction **after** backup/marker creation, whereas schema-family preflight refusal occurs before writes. No migration or test expectation changed.

| Accepted limit | Concrete impact / future trigger |
|---|---|
| V-D1 | One recorded consumer's withdrawal fences the shared Experience owner; all readers lose it. Origin-only dependent sessions are not fenced, but their premise becomes stale. Preserve fail-closed; per-consumer storage requires separate authorization, not 0018 now. |
| V-002 | Tier changes can still stale a premise; concurrent adoption may return CAS. No speculative retries or weaker revision rules. |
| V-003 | Some stale/dormant/same-batch invalid plans are rejected only in Apply. Transaction authority remains intact; advisory parity is v1.1 work. |
| Dual record families | Shared evidence/privacy authority, separate lifecycle and conflict view. No silent candidate-to-fact bridge. |
| Bounded procedures | Eight lifetime Uses and 64 KiB Node, possibly fewer for large definitions. Reserved feedback/lifecycle space preserves correctness, not unlimited reuse. |
| FFR-D1 / causal value | A faithfully reproduced procedure can match an already-held outcome. Usage statistics are not causal benefit; `causal_benefit_unmeasured` remains. |
| FFR-D4 | Early unreleased-candidate stores with random owners/failed-action records are not converted. Preserve them with their binary/evidence; use fresh test state. Canonical main upgrade is separately supported. |
| Activation and scale | No automatic native belief/negative/abstraction writer, Experience Start index, autonomous Governor Apply, general transfer, physical tiers or measured long-term/large-scale benefit. |

The Owner expressly excluded these items from v1.0 expansion. They remain visible constraints, not “fixed” findings. [v1.1 and Cognitive Evolution](../cognitive-core/roadmap.md) records future triggers without authorizing paid experiments, new schema or Context Space work.

## 7. Twelve reverse checks

| Question | Evidence-based answer |
|---|---|
| 1. Any historical fix omitted? | Required review heads are ancestors of `8c30f75`; 33 commits retained. The inherited import and current finding paths were checked; dirty parallel candidates were not mistaken for extra merge input. |
| 2. Tests pass but production is unwired? | Five model tools and terminal/reopen settlement are wired and exercised by `TestCompletionLearningThroughOrdinaryTools` plus the offline demo. Native writers/Governor automation remain explicitly SDK-only. |
| 3. Documentation overstates ability? | Current bilingual API/architecture/lifecycle/capability docs distinguish exact procedural reuse, dual families, SDK-only operations, UNKNOWN and unmeasured causal benefit. Historical reports remain historical. |
| 4. Missing rollback/recovery? | Real SQLite canonical upgrade/backup recovery, 0014–0017 rollback/refusal, missing-schema/hot-WAL and restart tests passed. Backup-before-down-guard semantics corrected; no production database was touched. |
| 5. Consumer privacy leak? | Private-use projections, foreign decision purge, co-consumer withdrawal and descendant fences passed targeted Race. V-D1 availability cost is disclosed, not bypassed. |
| 6. Stale version overwrites new data? | Native graph CAS, source revisions, concurrent adoption/settlement and Uses-only dependency identity tests passed; real semantic changes still invalidate. |
| 7. Success without execution? | Exact ordered receipt validation, `TestCompletionFeedbackRequiresActualExperienceApplication`, denial/no-execution and prior-run regressions pass. Adoption and recall are not credited use. |
| 8. Race/transaction gap? | Cognitive callback atomicity, cross-process fences and concurrent consumers passed. Complete same-source normal/Race coverage is 3005 PASS / 0 unresolved FAIL / 46 SKIP per mode, with no data race reported. Original package timeouts remain separately recorded. |
| 9. Integration changed behavior? | Go/module/SQL source map unchanged. Documentation and test invocation prerequisites changed, not production semantics. |
| 10. Safety weakened to finish? | No approval, fence, workflow, sandbox, assertion or migration guard disabled; original failures retained. |
| 11. Context Space mixed in? | No direct cognitive/window package dependency, new module, schema or window implementation. `runtime` remains composition root. |
| 12. Redundant implementation removable now? | No new runtime layer was added. The two record families have distinct live callers and privacy contracts; deleting one would lose supported behavior. No dead-code claim justified an unrelated cleanup. |

## 8. Direct delivery answers

1. **Can this form Cognitive Core v1.0?** Yes. The Owner-defined bounded architecture, implementation and engineering gates are complete. No further cognitive feature or new schema is required for this accepted baseline.
2. **Are all promised v1.0 capabilities implemented?** Yes within the explicitly accepted scope: shared source/evidence/privacy authority, controlled Experience loop, native SQLite governance, revision and protected memory. “Every autonomous cognitive behavior” was not promised by the latest scope.
3. **What is SDK-only?** Native belief/negative/abstraction writing/verification, explicit definition revisions, Governor lifecycle/capacity and administrative privacy/approval actions. Working APIs, not automatic writers.
4. **What is wired into production paths?** Candidate Start notices, five model-selected cognitive tools, ordinary policy-controlled application, application-end checks, terminal proposal/use settlement and reopen recovery. Opt-in cognition/Auto prerequisites still apply.
5. **What is absent?** Unified cross-family lifecycle/conflict view, Experience Start index, automatic native writers/Governor, generic semantic/parameter transfer, physical storage tiers, and proven long-term benefit.
6. **Any unresolved P0/P1/P2?** No unresolved in-scope product findings. Verification incidents are closed with evidence. Historical V-D1 P2 remains an explicitly accepted availability tradeoff, not an unapproved repair left open. Other repository backlog items are outside this verdict.
7. **Accepted limits?** All listed in §6; none is described as solved. Engineering completion does not prove autonomous-learning gain or human-level cognition.
8. **Ready to merge main?** Yes technically, for the frozen commit and disclosed platform bounds. Merge/push/tag/release and historical branch removal still require Owner authorization; none was performed.

The original broad cognitive objective is met as a bounded, evidence-governed Go runtime mechanism, not a demonstration of full autonomous cognition. Memory/learning/reasoning cooperate, but the acknowledged dual-family and activation gaps remain. Main still has its existing Context Graph, Notes/Recall, Attention Light and read-only Governor foundation; **all expanded v1.0 capability remains candidate-only until an authorized merge**.

## 9. Delivery artifacts and authorization

- [English API](../cognitive-core/COGNITIVE_CORE_API.md) / [Chinese API](../cognitive-core/COGNITIVE_CORE_API.zh-CN.md), bilingual architecture, implementation, data model, memory lifecycle and capability matrix.
- [Finding Ledger](../cognitive-core/finding-ledger.md), [ADR-0023](../adr/0023-unified-cognitive-lifecycle.md), [verification/platform report](COGNITIVE_CORE_V1_VERIFICATION.md), [migration report](COGNITIVE_CORE_V1_MIGRATION.md), [Git/merge/cleanup plan](COGNITIVE_CORE_V1_GIT.md), [v1.1 roadmap](../cognitive-core/roadmap.md).
- Workspace evidence `bench/evidence/cognitive-core-v1-integration-20261116/`: owner scope, baseline, source map, raw logs, failure diagnoses, per-platform counts, manifests and the post-commit `FINAL_DELIVERY.json` binding the exact integration SHA.
- The integration root's `SHA256SUMS` covers delivered source, tests/fixtures, current docs and release support; the external evidence manifest covers logs and post-commit identity. `MANIFEST_SCOPE.md` defines exclusions and avoids self-referential hashes; `CHECKSUM_VERIFICATION.json` records actual verification.

Only a scoped local integration commit is authorized now. No main merge, push, tag, release, Desktop change, production migration or historical branch/worktree deletion was performed. The shared Linux container is not owned by this task; only its unique temporary verification directory and this task's temporary compile artifacts may be removed. Workspace evidence/TODO are not claimed as Core-committed files.
