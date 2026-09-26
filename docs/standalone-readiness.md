# Standalone coding-agent readiness

This document is the evidence gate for claiming that LumenCortex is ready to serve as a primary standalone coding agent.

A feature being present in source code is not enough. Readiness claims require an automated or real-model proof that exercises the same execution path.

## Current readiness matrix

| Area | Implementation | Validation | Readiness note |
|---|---|---|---|
| Durable Sessions / resume | Complete | Core CI + WAL cross-process tests | Ready |
| Bounded long-task context | Complete | deterministic 20-tool-round test + real Qwen3 graph-memory VM proof | Ready |
| SQLite WAL / restart persistence | Complete | Core CI, integrity/checkpoint/concurrency tests | Ready |
| Large-repository lexical/symbol retrieval | Complete | 100k-node FTS5/symbol benchmark | Ready for exact/symbol-heavy lookup |
| Semantic/vector retrieval | Complete optional baseline | Core CI: SQLite embedding cache, incremental content-hash sync, exact cosine, RRF Hybrid, pure-semantic recall and CLI path | Ready optional baseline; ANN/HNSW/IVF acceleration remains a scale optimization |
| Workspace read/search/write tools | Complete | Core CI, including bounded batch `read_files` | Ready |
| Atomic multi-file patching | Complete | Batch validation/ambiguity/traversal tests | Ready |
| Shell execution | Complete | async/timeout/process-tree/cancel tests | Ready |
| Live shell stdout/stderr | Complete | pre-completion streaming + Agent/TUI event tests | Ready |
| Agent cancellation / resume | Complete | AbortSignal + TUI + process SIGINT/SIGTERM tests; interrupted Session persists | Ready |
| Permission policy tiers | Complete | dedicated policy tests; unknown policy fails closed | Ready: read-only/workspace/full have distinct scope semantics |
| LSP definition/references/symbols/hover/diagnostics | Complete | isolated LSP harness | Ready for navigation/diagnostics |
| LSP rename/code actions | Complete baseline | isolated Content-Length LSP harness + atomic WorkspaceEdit + Agent-tool tests | Ready for edit-backed rename/quick-fix; command-only actions and LSP resource operations intentionally require separate handling |
| MCP stdio/HTTP | Complete | modern + legacy fallback harness | Ready |
| Focused Subagents | Complete | durable isolated Session tests | Ready |
| Parallel Sessions | Complete | overlap assertion + mutation safety gate | Ready |
| Full-screen TUI | Complete baseline | frame/session/events/cancel/live-output tests | Usable; richer navigation/scrolling remains UX work |
| Provider abstraction | Complete | provider tests + real local endpoint health | Ready |
| Workflow Contract / deterministic gates | Complete baseline | condition/runtime + Agent completion/tool-boundary + CLI human-gate tests | Ready baseline: Facts/Action/Route/Outcome/Gate with durable resume |
| Real local-model coding task | Complete | Qwen3 4B VM run `35549612505`: failing baseline → edits → 2/2 tests pass; independent rerun and unchanged test SHA256; persisted completed Session | Ready baseline |
| Real DeepSeek V4 coding | Adapter complete | Not yet verified with working credentialed route | Evidence gap |
| Cognitive Git | Complete for cognitive graph | Core CI | Ready for cognition history |
| Session ↔ real Git worktree isolation | Complete Go baseline | Go runtime lifecycle, Agent workspace routing, shell isolation, status/diff, overlap-conflict and safe handoff/apply tests; Go CLI lifecycle tests | Ready optional baseline for isolated Go sessions; not the same as Cognitive-Git branch ↔ worktree transactional binding |
| Workspace rollback after arbitrary shell mutation | Partial | Worktree-session isolation/removal tests | Worktree sessions can be discarded safely; local-session arbitrary shell mutation still has no whole-run rollback |
| Skills layer | Complete Node + Go baseline | layered registry tests, Node/Go Agent prompt injection, Node Subagent propagation, Node/Go CLI lifecycle tests | Ready baseline with global/project layering and enable state |
| Vision/browser tools | Not implemented | None | Gap |
| Adaptive cognitive routing / Think mode | Implemented baseline | Core CI cognitive-control tests | Experimental baseline; routing quality still needs workload benchmarks |
| Decision Layer + Category model chains | Implemented baseline | Core CI: algorithm/System-One adapter, ordered-chain fallback, circuit breaker, provider effort mapping, session trace tests | Ready for experimental use; live Jev/Laya validation pending |
| Persistent Work Units | Implemented baseline | dependency/evidence/verification/final-answer gate tests | Ready experimental baseline |
| Hot/warm/cold storage metadata | Implemented baseline | SQLite schema-v3/backfill/access/compaction tests | Logical graph remains intact; physical tier separation is not claimed |
| Graph Governor / Cortex Epochs | Implemented baseline + opt-in scheduler | Core CI: analyzer, semantic Curator, validator, canonicalization/branch/promotion, reversible epoch tests, persistent scheduler debounce/stale-plan/CLI/Agent lifecycle tests | Experimental governance baseline; scheduler can auto-plan after completed Agent runs, but apply remains explicit and physical tier separation remains a gap |
| Go cognitive-control parity | Implemented baseline | Go CI: shared profile, System One HTTP, Category failover/circuit, dynamic Think effort, Work Unit gates | Governor write/apply is intentionally not duplicated |
| Go Graph Governor read path | Implemented baseline | Go CI: shared SQLite snapshot + Analyzer/Curator/Validator + no-mutation assertions | Read-only; Node remains mutation authority |

## Primary-agent evidence gates

LumenCortex may be described as a primary standalone coding agent only when all mandatory gates below remain green.

1. **Core correctness** — Node 22 and Node 24 core CI pass.
2. **Storage durability** — SQLite WAL, restart/reopen, cross-process Session writes and integrity checks pass.
3. **Long-horizon memory** — the real-model graph-memory workflow proves that evidence outside the recent chat window is reactivated from the Context Graph.
4. **Coding mutation** — a real model runs failing tests, edits implementation files, reruns tests and passes an independent verifier without changing the tests.
5. **Observability** — long-running shell commands stream stdout/stderr while still executing.
6. **Interruption safety** — provider and shell work can be cancelled; the Session is persisted as interrupted and remains resumable.
7. **Repository intelligence** — indexed search and LSP harnesses pass.
8. **Extensibility** — MCP and Subagent harnesses pass.

The real coding workflow is intentionally stricter than a model-generated final answer. It requires:

```text
initial tests fail
        ↓
real model uses coding tools
        ↓
implementation files change
        ↓
independent npm test passes
        ↓
test-file SHA256 hashes are unchanged
        ↓
persisted Session contains shell + edit tool evidence
```

## Important non-claims

Passing the readiness gates does not mean LumenCortex has every feature of a mature IDE or terminal coding product.

In particular, the current architecture does not yet claim:

- mandatory Git worktree isolation for every Agent run; the Go runtime supports explicit per-session worktrees, but local sessions remain valid,
- whole-run rollback of arbitrary local-session workspace changes produced by shell commands,
- automatic execution of command-only LSP code actions or LSP create/rename/delete resource operations,
- approximate-nearest-neighbor embedding indexes; the optional exact-cosine/RRF baseline is implemented,
- vision/browser automation,
- polished terminal navigation equivalent to mature editor-grade TUIs,
- live Jev/Laya endpoint validation and calibrated routing benchmarks,
- automatic Governor plan application or production-calibrated governance policy; opt-in post-Agent scheduling/pending-plan persistence is implemented,
- separate physical hot/warm/cold data stores or destructive cognitive GC (not currently claimed),
- broader provider-specific reasoning controls,
- Go Graph Governor mutation/executor parity; the current Go Governor is intentionally read-only against the shared SQLite graph.

These are tracked as explicit product gaps rather than being inferred from the existence of generic shell or MCP tools.
