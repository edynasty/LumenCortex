# Cognitive Capability and Evidence Matrix

**Cognitive Core v1.0:** integration branch `integ/cognitive-core-v1-20261116`, selected input `8c30f75`, main `00344b5`; unmerged. [中文](capabilities.zh-CN.md). [The integration report](../reports/FINAL_COGNITIVE_CORE_V1_REPORT.md) binds actual engineering and platform results; [Finding Ledger](finding-ledger.md) distinguishes historical self-verification from independent closure.

| Capability | Main | Completion candidate / source | Evidence and limit |
|---|---|---|---|
| Working/episodic memory | Existing Task State, Notes and Sessions | Reused, no second memory system | Durable context, restart and provenance regressions; hidden reasoning is not memory. |
| Scope/time/evidence | Existing harness boundary | `knowledge.Scope`, `Times`, `Evidence`, native graph | Scope, narrowed history, CAS, current evidence and privacy regressions. Five structured assertion families only. |
| Task-relevant attention | Existing lexical/symbol Light and thread recall | `knowledgeIndexRow`, tokenized `RecallExperiences` | Relevant locators and warning priority; similarity is not applicability or broad retrieval-quality evidence. |
| Evidence-based decisions | Model + Work Unit framework | `AssessCognition`, `assess_cognition`, pinned premises and conflicting peers | CC-005 red/green and actual decision-tool/withdrawal regression. Not general causal reasoning. |
| Experience extraction | Not the expanded candidate | Active-run proposal + terminal `ExtractExperience`/validation | `TestCompletionLearningThroughOrdinaryTools`; no self-assigned execution/evidence. |
| Independent adoption/application | Not the expanded candidate | `adopt_experience`, `apply_experience`, normal dispatch | Exact origin action sequence, fresh conditions, permission/hook/budget and SQLite receipt. No parameterized replay. |
| Feedback and negative learning | Not the expanded candidate | Complete execution identity + independent check, `needs_revision` | Unrelated/denied/unknown cases and true failed-tool counterexamples; outcome is not causal benefit. |
| Belief/dependency revision | Partial foundations | Native CAS revisions, `NodeRef`, source lineage | Changed premise, invalidation history, source withdrawal/deletion tests. Read-time rather than autonomous watchers. |
| Logical retention/consolidation | Not unified | `MemoryPolicy`, `GovernorApply`, `MaintainCognitiveMemory` | Protected counterexamples, equal-evidence aliases, atomic rejection and pressure. No physical tiers or lossy fact merging. |
| Read-only Governor | Existing | Normal Go SQLite Analyze/Plan/Validate retained | Native graph/no-mutation tests. Model Curator still fails closed before a request. |
| Authorized Governor Apply | Not available | Host-only owned lifecycle batch | Archive/tier/canonicalization only; branch/promote/epoch remain proposals, not executable truth changes. |
| Recovery | Existing run/persistence recovery | Memory-finalization recovery in `Engine.Open` | Real SQLite injected settlement faults, no repeated actions; pending becomes unknown. |
| Consumer privacy | Existing source/fence boundary | Reader-private Uses, decisions/applications/access cleanup | Public API, actual tools, historical reads and reopen cases; trusted local host only. |
| Measured frequency and utility | Existing recency ranking | Reader-local access versus independent use/outcomes | Recall never increments successful reuse. Causal utility explicitly unmeasured. |
| General task transfer / sustained learning gain | Not established | Not claimed | Deterministic fixture proves the mechanism, not improved real-task success or human-level cognition. |

## SDK availability versus production activation

“Automatic” below means framework execution after explicit enablement/prerequisites, not autonomous selection of every cognitive action.

| Capability | Public host / SDK path | Normal production trigger | v1.0 limit |
|---|---|---|---|
| Working / episodic memory | Session, Task State, Notes, executions and checks | Existing Agent loop and durable terminal state | Does not retain private reasoning. |
| Semantic candidate maintenance | `EnableCognition`, `MaintainCognition`, `CognitionOptions.Auto` | Explicit pass, or opt-in bounded pass after top-level completion | `cognition_candidates` is not automatically converted into native graph. Auto may call the configured model; it was not enabled for paid testing here. |
| Candidate Start attention | `ReadKnowledge` diagnostics | Top-level `knowledgeIndexRow` and `BeginRun(Notices, NoticeSources)` | Up to 6 locators / 2048 bytes; excludes Experience. |
| Experience discovery | `RecallExperiences`, `ReadExperience` | Model-selected `recall_experiences` or `read_thread` | No automatic opening index or guaranteed model selection. |
| Experience proposal / extraction | `ExtractExperience`, `ValidateExperience` | Model `propose_experience`; framework finalizes after any eligible run, including children | Complete observed origin plus approved checks required; not prose-only learning. |
| Adoption / actual reuse | `AdoptExperience` | Model `adopt_experience`, then `apply_experience` through normal callback | Exact actions/arguments; no host replay API; eight lifetime Uses and byte budget. |
| Outcome / recovery | `RecordExperienceFeedback`, `AgentResult.Cognition` | Check at application end, terminal settlement and `Engine.Open` recovery | No later-state attribution or repeated tools; utility remains `causal_benefit_unmeasured`. |
| Native belief / negative / abstraction | `PutCognitiveNode`, `VerifyCognitiveNode`, graph APIs | Explicit host calls; no automatic Agent writer | Model/host proposal is not check evidence. |
| Evidence-based assessment | `AssessCognition` | Model-selected `assess_cognition`; immutable decision receipt | Query mode searches belief only; explicit IDs and dependency/peer expansion are bounded to 16. |
| Experience revision | `ReviseExperience` | Explicit origin-host revision after feedback | Counterexample stops reuse; no automatic definition rewrite or self-approval. |
| Governor analysis / plan / validation | `GovernorAnalyze`, `GovernorPlan(ctx, nil, …)`, `GovernorValidate` | Explicit host request | No model curator or autonomous Epoch. |
| Governor Apply / retention | `GovernorApply`, `SetCognitiveLifecycle`, `MaintainCognitiveMemory` | Explicit host-owned CAS transaction | Not a scheduled background optimizer; protects counterexamples and safety records. |
| Source withdrawal / privacy deletion | Existing `RevokeSource`, `DeleteSession` | Explicit host operation; framework propagates fences and purge/recovery | V-D1 can withdraw the whole shared Experience after one consumer deletion. |

## Accepted v1.0 limits

- Two record families share scope/check/provenance/privacy authority but not one lifecycle or cross-family conflict view. This architectural boundary is documented, not declared fixed.
- V-D1 shared-owner availability, V-002 tier invalidation/concurrent-adoption CAS, and V-003 advisory prevalidation differences remain accepted. They do not authorize weaker transaction/fence checks.
- Experience is a **Verified Procedural Macro**, not parameterized transfer, general semantic learning, autonomous belief formation or causal gain measurement.
- Long-term usefulness, larger-scale recall quality and physical storage tiers are unproven/unimplemented. [The roadmap](roadmap.md) requires independent future scope and evidence.

## Interpretation

- **Implemented candidate** means real Go code and a reachable path, not merged main, a product release or independent PASS.
- Ordinary tests, Race, migration checks, offline demo and platform compilation/runtime results are reported separately. SKIP and unrun platforms are not PASS.
- Model-proposed causes, confidence and unverified summaries do not become verified facts. Unknown/conflict/expired knowledge remains explicit.
- Historical Node code and historical reports are reference inputs, not production implementation or current test results.
