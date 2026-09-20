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
| Semantic/vector retrieval | Not implemented | None | Optional gap for fuzzy semantic recall |
| Workspace read/search/write tools | Complete | Core CI, including bounded batch `read_files` | Ready |
| Atomic multi-file patching | Complete | Batch validation/ambiguity/traversal tests | Ready |
| Shell execution | Complete | async/timeout/process-tree/cancel tests | Ready |
| Live shell stdout/stderr | Complete | pre-completion streaming + Agent/TUI event tests | Ready |
| Agent cancellation / resume | Complete | explicit AbortSignal test; interrupted Session persists | Ready |
| LSP definition/references/symbols/hover/diagnostics | Complete | isolated LSP harness | Ready for navigation/diagnostics |
| LSP rename/code actions | Not implemented | None | Gap for IDE-grade refactoring |
| MCP stdio/HTTP | Complete | modern + legacy fallback harness | Ready |
| Focused Subagents | Complete | durable isolated Session tests | Ready |
| Parallel Sessions | Complete | overlap assertion + mutation safety gate | Ready |
| Full-screen TUI | Complete baseline | frame/session/events/cancel/live-output tests | Usable; richer navigation/scrolling remains UX work |
| Provider abstraction | Complete | provider tests + real local endpoint health | Ready |
| Real local-model coding task | Implemented workflow | **Pending current VM proof** | Do not claim complete until independent tests and unchanged test hashes pass |
| Real DeepSeek V4 coding | Adapter complete | Not yet verified with working credentialed route | Evidence gap |
| Cognitive Git | Complete for cognitive graph | Core CI | Ready for cognition history |
| Cognitive branch ↔ real Git worktree binding | Not implemented | None | Gap for transactional workspace isolation |
| Workspace rollback after arbitrary shell mutation | Not implemented | None | Gap; apply_patch itself is atomic, whole Agent runs are not |
| Skills layer | Not implemented | None | Gap |
| Vision/browser tools | Not implemented | None | Gap |

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

- transactional Git worktree isolation for every Agent run,
- rollback of arbitrary workspace changes produced by shell commands,
- IDE-grade rename/code-action refactoring,
- semantic embedding retrieval,
- reusable Skills,
- vision/browser automation,
- polished terminal navigation equivalent to mature editor-grade TUIs.

These are tracked as explicit product gaps rather than being inferred from the existence of generic shell or MCP tools.
