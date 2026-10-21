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

## B3 continuity evaluation

**Pre-registered before any run (2026-10-21).** Harness: `internal/realmodel/continuity_test.go` (build tag `realmodel`), model `local/deepseek-v4.1-flash` only.

- **History** (built once per repeat on the real model): 7 interactive threads, 10 turns in total. In each one the user states facts or decisions that never reach the repository: the staging host and token variable, a sync.Map decision after a mutex deadlock, a counter naming convention, the release branch naming (an old convention, later superseded), plus two distractors. The history is copied once per (task, arm), so both arms read the same store.
- **Tasks (thread B):** `deploy`, `cache`, `metrics` and `release` need the history. `control` does not.
- **Arms:**
  - arm 1 `window`: window, Task State and notes, with `recall`/`read_thread` denied through `ToolDenylist`;
  - arm 2 `recall`: the same, plus both tools.
- **Grading:** deterministic checks of files and of `go build`/`go test` output, never a model.
- **Gate:** the tools stay on by default only if all three hold. Otherwise they become opt-in.
  1. Arm 2's success over the four history tasks exceeds arm 1's by at least 2 runs across all repeats.
  2. Arm 2's `control` success is not lower than arm 1's.
  3. Arm 2's mean prompt tokens per B run are ≤ 1.5× arm 1's (the token budget).
- **Secondary, not gating:**
  - recall relevant@1 and @3;
  - whether the history threads wrote notes carrying the key fact, which measures notes alone;
  - whether `release` picks the superseded convention.
- **Repeats:** 3. A pilot run that only checks the harness mechanics is reported separately and does not count.

**Pilots (mechanics only; not counted).** Both ran `deploy` and `control` with one repeat.

1. Pilot 1 surfaced two harness defects, which are fixed:
   - `MessagePage(…, 0, …)` reads seq < 0, so the recorded tool list was empty. The harness now passes `-1`.
   - The default policy is read-only, so thread B could not write. B threads now run with `Policy: "full"` in their private fixture.
2. Pilot 2 confirmed the mechanics: `control` passed in both arms, and writes and tools were recorded. It also showed two behaviours:
   - History notes captured the key fact for `deploy`, `cache` and `metrics`. For `release-old` the thread wrote no note. For `release-new` the note recorded the check but not the new convention.
   - In the `recall` arm the model never called `recall`, so `deploy` failed in both arms. The model guessed the host.

   Before the counted run, the `recall` tool description gained one sentence: facts from earlier conversations are often not in the repository, so call `recall` before guessing one or when a task may continue earlier work. A denied tool is not offered at all, so arm 1 is unaffected. This is the only change between the pilots and the counted run.

## Open

- B3 continuity evaluation (ADR-0010 D6) decides whether `recall`/`read_thread` stay on by default.
- `docs/agent-runtime.md` still describes the Node-era subagent and graph tools. It is outside this change and unchanged.
