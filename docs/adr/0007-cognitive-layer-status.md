# ADR-0007: Cognitive layer status and growth path

- **Status:** Accepted (owner decision D6, 2026-10-15)
- **Date:** 2026-10-15
- **Owners:** project owner
- **Supersedes:** none. ADR-0004 D1 stays in force; this records its current reachability. Amends design §8.2 (the T9 gate after M5.1) and audit §12 ("delete from Go")
- **Superseded by:** none

## Context

The Go runtime has no Context Graph, Attention Light, Cognitive Git or search index.

- It never creates the graph tables.
- It refuses databases that contain Node's graph tables (`internal/migrate/migrate.go` L187-205).

As a result, every `lcx-go governor` subcommand fails on every database the Go runtime can open, with "cognitive graph tables are not initialized" (probe P3). The analyzer, validator and opt-in curator are pure functions over `cognition.GraphState`; only the snapshot loader is bound to Node's schema.

Design §8.2 kept the graph only if audit T9 showed value. T9 measures single-session task success, so it cannot answer the product questions: cross-session continuity, relationships, provenance and retrieval quality.

## Decision

1. **Governor dormant, D1 kept.**
   - The deterministic default and the opt-in curator remain the Governor's contract.
   - M5.1 ("Go stops reading graph tables") deletes the Node-schema snapshot loader. From then on the CLI returns `ErrNoContextStore` with that explanation, instead of a setup error the user cannot fix.
   - The Governor is re-enabled only by a loader for the Go context store, with its own tests.

2. **The graph grows from the Context Index.**
   - Thread references, resolved threads and user-confirmed notes (ADR-0006) are the first durable context items.
   - Ranking (Attention Light) and branch scoping (Cognitive Git) are later layers on the same store, added only when the continuity evaluation shows value.
   - No generic graph schema is fixed now. The PR that introduces the first durable item type decides it, within these principles:
     - **Grounded only.** Every item records its producer (runtime check, human, tool observation, model), its source, and a freshness rule. Model statements are never evidence.
     - **No repository copy.** Code is referenced as `path@blob` or as a symbol and resolved live (`rg`, LSP). Bulk file content is never stored as memory.
     - **Invalidation.** An item that cites code is stale when the blob it cites is not reachable from the thread's current HEAD.
     - **Discarded worktrees.** Decisions, rationale and outcomes from discarded worktrees ("tried X, abandoned because Y") remain as history. Code facts that cite blobs reachable only from the discarded branch are invalidated, not deleted.
     - **Table names.** Go tables never reuse Node graph table names, so the legacy-database check stays valid.

3. **The continuity evaluation replaces the T9 gate.** It runs on `local/deepseek-v4.1-flash`.
   - **Corpus:** paired sessions on real repositories, in which session B depends on session A's outcome.
   - **Arms:**
     1. window plus Task State;
     2. plus index and recall;
     3. plus propagation over grounded links.
   - **Metrics:** success on continuation tasks; precision@k against human labels; tokens per request; stale-context errors; user exclusions per suggestion.
   - **Gates:** recall ships if arm 2 beats arm 1. Propagation ships if arm 3 beats arm 2 on precision or stale-context errors within a token budget fixed before the run.

## Consequences

- No cognitive capability is removed from the roadmap. None is ported to Go before it has a grounded store and a measured benefit.
- Desktop and the Demo may present cognitive features as proposals. Only implemented behaviour may be presented as available.

## Validation plan

- **M5.1:** `governor` returns `ErrNoContextStore` on a Go database (test).
- **The continuity evaluation:** its harness and labelled corpus are a separate, later work item, after M4.2.
