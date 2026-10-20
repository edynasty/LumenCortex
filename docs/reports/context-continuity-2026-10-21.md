# Context continuity: thread notes, read_thread, recall (ADR-0010)

- **Date:** 2026-10-21
- **Scope:** Core: B1 (notes, `read_thread`) and B2 (`recall`) of ADR-0010. Desktop: the Notes section and Core pin, recorded in the Desktop report. B3: the continuity evaluation, recorded below when it has run.
- **Baseline:** Core `82e6b71` (A3 probe), Desktop `1714459`.
- **Status:** B1/B2 Core implemented and validated locally; not pushed.
- **Authority:** [ADR-0010](../adr/0010-thread-notes-and-recall.md), [ADR-0007](../adr/0007-cognitive-layer-status.md). Proposal: `../LumenCortex-architecture-audit/design/handoffs/agent-architecture-2026-10-20.md` §4 (not version-controlled).

## Decisions taken against the proposal

| Proposal | Decision | Why |
|---|---|---|
| N5 Task State notes | kept, as specified (512 B, 32 notes, 2 KiB render, block 10 KiB) | the only way to keep what a dropped window group established without summarising |
| N6 `context_items` table with capture at resolution/delivery/Remember | **replaced**: the session store is the index | every captured field already lives in the store, so a copy adds duplicate truth, an exactly-once capture protocol and a deletion path, for no answer the store cannot give |
| `context_read` tool | dropped | the only item type is a note, and `read_thread` returns them all |
| "Remember" items, producer `human` | a user note on the thread (`by: user`) | same store, same edit path as the Task pane |
| N8 staleness = cited blob changed | kept; checked at read time in the reader's workspace | the thread's own block stays a pure function of its state |
| N9 Go ranking, FTS5 later | kept | ≤ 500 candidates, fields extracted with `json_extract`, so no workflow snapshot is loaded |

## Changes

- `internal/taskstate/notes.go`: `Note`, `Cite`, `NoteEdit`, limits, `WriteNote`/`RemoveNote` (run) and `setNote` (user, through `Apply`), plus a monotonic `NoteSeq` so ids are never reused. `render.go`: a notes section, newest first, in 2 KiB; `MaxBlockBytes` 8 → 10 KiB; included thread references point at `read_thread`.
- `internal/agent/tasktools.go`: `note_write` and `note_remove` loop tools, and a `Loop.Cite` hook. The system prompts gained one sentence that asks for notes.
- `internal/repository/blobs.go`: `FileBlobs` hashes regular files inside the workspace in one `git hash-object` process.
- `internal/session/context.go`: `ContextRows`/`ContextRow`, one query with the newest Task State's brief, resolution state and notes.
- `internal/threadctx`: a pure tokenizer (words, camelCase parts, plurals, CJK bigrams), the ranker, snippets and freshness marking.
- `internal/toolset/threads.go`: `recall` and `read_thread`, registered when a `ThreadReader` is given; the runtime always gives one.
- `runtime/threads.go`: `Engine.ReadThread`, `Engine.Recall`, and the per-run reader, which checks freshness in the run's own workspace and leaves out the run's own subtree from recall. `runtime/taskstate.go`: a user note add is refused up front when 32 user notes exist.
- Docs: ADR-0010; `docs/go-provider.md` "Thread notes and recall".

## Tests run

- New:
  - `internal/threadctx` (tokens, ranking order by weight, rarity and recency, CJK, bounds, snippet, staleness);
  - `internal/taskstate/notes_test.go` (render budget, limits, user edits, cite paths);
  - `internal/agent/notes_test.go` (tool behaviour, C7, cite hook);
  - `runtime/threads_test.go`: an end-to-end run through the Engine and SQLite; recall and read_thread across threads; stale after a change and after removal; C1 sentinel absent at rest; C5 checked in the system prompt; host edits and limit.
- `go test -count=1 ./...`, `go build ./...` and `go vet ./...`: pass.

## Open

- B3 continuity evaluation (ADR-0010 D6) decides whether `recall`/`read_thread` stay on by default.
- `docs/agent-runtime.md` still describes the Node-era subagent and graph tools. It is outside this change and unchanged.
