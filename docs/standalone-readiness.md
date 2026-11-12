# Standalone coding-agent readiness

> **Go-only production policy (Owner decision, 2026-11-11):** Go is the sole production Runtime. This matrix contains historical Node.js evidence from earlier development stages. Node-only results are historical reference, not current Go acceptance evidence or a Node/Go parity target. Use Go source, Go tests, and the current Core release gates for production decisions. The expanded Cognitive Core P0 candidate is not yet part of Core `main`.

This document is the evidence gate for claiming that LumenCortex is ready to serve as a primary standalone coding agent.

A feature being present in source code is not enough. Readiness claims require an automated or real-model proof that exercises the same execution path.

## Current readiness matrix

| Area | Implementation | Validation | Readiness note |
|---|---|---|---|
| Durable Sessions / resume | Complete | Core CI + WAL cross-process tests | Ready |
| Bounded long-task context | Complete | deterministic 20-tool-round test + real Qwen3 graph-memory VM proof | Ready |
| SQLite WAL / restart persistence | Complete | Core CI, integrity/checkpoint/concurrency tests | Ready |
| Large-repository lexical/symbol retrieval | Complete | 100k-node FTS5/symbol benchmark | Ready for exact/symbol-heavy lookup |
| Semantic/vector retrieval | Historical Node.js reference only | Historical Node tests and benchmarks | Not a Go production capability claim; Go semantic retrieval requires separate implementation and validation. |
| Workspace read/search/write tools | Complete | Core CI, including bounded batch `read_files` | Ready |
| Atomic multi-file patching | Complete | Batch validation/ambiguity/traversal tests | Ready |
| Shell execution | Complete | async/timeout/process-tree/cancel tests | Ready |
| Live shell stdout/stderr | Complete | pre-completion streaming + Agent/TUI event tests | Ready |
| Agent cancellation / resume | Complete | AbortSignal + TUI + process SIGINT/SIGTERM tests; interrupted Session persists | Ready |
| Permission policy tiers | Complete | dedicated policy tests; unknown policy fails closed | Ready: read-only/workspace/full have distinct scope semantics |
| LSP definition/references/symbols/hover/diagnostics | Complete | isolated LSP harness | Ready for navigation/diagnostics |
| LSP rename | Complete (Go) | Go `internal/lsp` harness + guarded atomic text WorkspaceEdit + Agent-tool tests | Ready for guarded symbol rename; Go applies text edits only and refuses LSP resource operations |
| LSP Code Actions (`textDocument/codeAction`) | Not implemented (Go) | No Go request handler, Agent tool, or CLI command; implementation and tests exist only in retained Node.js reference source | Not a Go production capability; preserve as a possible Go backlog item, not as completed functionality |
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
| Skills layer | Go baseline | Go layered registry, Agent prompt, and CLI lifecycle tests | Current Go implementation; Node-era evidence is excluded from Go acceptance. |
| Vision/browser tools | Not implemented | None | Gap |
| Adaptive cognitive routing / Think mode | Implemented baseline | Core CI cognitive-control tests + 12-case labeled deterministic routing benchmark + strict CLI gate + captured local `deepseek-v4.1-flash` canonical-prompt calibration scored 12/12 and re-scored in CI | Experimental baseline; one generative model is calibrated, while live Jev/Laya and broader multi-model calibration remain open |
| Decision Layer + Category model chains | Historical Node.js reference; Go capabilities listed separately below | Historical Node test evidence | Do not count Node implementation or tests as current Go production capability. |
| Persistent Work Units | Implemented baseline | dependency/evidence/verification/final-answer gate tests | Ready experimental baseline |
| Hot/warm/cold storage metadata | Implemented baseline | SQLite schema-v3/backfill/access/compaction tests | Logical graph remains intact; physical tier separation is not claimed |
| Graph Governor / Cortex Epochs | Go main: read-only Analyze / Plan / Validate | Go Engine shared-SQLite snapshot, deterministic plan, validation, and no-mutation tests | Model Curator fails closed before provider invocation; Apply and Cortex Epoch execution are not claimed in Go main. Expanded P0 remains unmerged. |
| Go cognitive-control baseline | Implemented baseline | Go CI: shared profile, System One HTTP, bounded generative Decision fallback, retrieval confidence/relation gates, Category failover/circuit, dynamic Think effort, Work Unit gates | This is a Go implementation baseline, not a Node/Go parity commitment. |

## Primary-agent evidence gates

LumenCortex may be described as a primary standalone coding agent only when all mandatory gates below remain green.

1. **Core correctness** — supported Go toolchain CI, Go unit/integration tests, build, and vet pass.
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
independent Go test suite passes
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
- Go LSP Code Actions (`textDocument/codeAction` / `codeAction/resolve` / `workspace/executeCommand`) and LSP file-resource operations are not implemented or claimed; Go main supports guarded rename text edits only,
- semantic/vector retrieval in Go; exact-cosine/RRF evidence belongs to the historical Node.js reference and is not a Go production capability,
- vision/browser automation,
- polished terminal navigation equivalent to mature editor-grade TUIs,
- live Jev/Laya endpoint validation and broader multi-model calibration; the deterministic benchmark and one captured local DeepSeek V4.1 Flash 12/12 routing calibration are implemented,
- Go Curator/semantic Governor application, autonomous graph mutation, or production-calibrated governance policy; current Go main exposes read-only Analyze / Plan / Validate,
- separate physical hot/warm/cold data stores or destructive cognitive GC (not currently claimed),
- broader provider-specific reasoning controls,
- Go Graph Governor Apply, autonomous graph mutation, and Cortex Epoch execution; the current Go mainline Governor is intentionally read-only against the shared SQLite graph.

These are tracked as explicit product gaps rather than being inferred from the existence of generic shell or MCP tools.
