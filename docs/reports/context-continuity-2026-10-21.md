# Context continuity: thread notes, read_thread, recall (ADR-0010)

- **Date:** 2026-10-21
- **Scope:** Core: B1 (notes, `read_thread`) and B2 (`recall`) of ADR-0010, their review fixes, and B3, the continuity evaluation. Desktop, recorded in the Desktop report `docs/reports/context-notes-desktop-2026-10-21.md`:
  - the Task pane Notes section, "Remember" and the Core pin (`7b7f894`);
  - the Context page notes with Core freshness and Forget (`f91df45`), which use the existing `Engine.ReadThread` and `EditTaskState` with no Core change;
  - a native run of the packaged app (`b1beb59`).
- **Baseline:** Core `82e6b71` (A3 probe), Desktop `1714459`. Core after this work: `3e4f418`.
- **Status:** B1/B2 Core implemented, reviewed and validated locally. B3 ran and passed its gate, so `recall`/`read_thread` stay on by default (ADR-0010 D6). Not pushed.
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

### Review fixes (`ec267bd`; harness `3e4f418`)

An independent review of `82e6b71..b908ae6` found five Core defects and two harness defects. All are fixed:

| Finding | Fix | Regression test |
|---|---|---|
| A user note's id was assigned when the inbox applied, so the id a host saw could later name a different note, such as a run's note written in between | `EditTaskState` gives a user add its final id (`u<hex>`, never colliding with a run's `n<seq>`) when it is queued (`NoteEdit.Create`); applying it twice is a no-op | `taskstate.TestQueuedUserNoteIDIsStable`, `runtime.TestHostNoteEdits` (removing the previewed id after a run wrote its own note) |
| The 32-user-note check and the queueing were two steps, so concurrent adds could pass it together | one `Engine.noteMu` section covers both | `runtime.TestHostNoteEdits` (8 concurrent adds at 31 notes: exactly one accepted) |
| `FileBlobs` followed a symlinked directory out of the workspace, and hashed any file regardless of size | each path resolves under the workspace's real path; only regular files ≤ 4 MiB are hashed | `repository.TestFileBlobsOnlyHashFilesInsideTheWorkspace` |
| One unreadable cited file failed every cite in the batch | a failed batch is retried per file; that file alone gets no blob | `repository.TestFileBlobsOneUnreadableFileDoesNotFailTheRest` |
| Four 1 KiB cite paths made a note's line larger than the notes budget, so the newest note was not rendered | the cites part of a line is cut at 256 B | `taskstate.TestRenderShowsTheNewestNoteWithLongCites` (fails before the fix) |
| Harness: the session database sat inside the evaluated workspace, so a B run's shell could read the history | each workspace's state lives at `<workspace>.state` (`Options.RepoDir`); tool calls naming it are counted as `stateAccess` | counted run: `stateAccess` 0 of 30 |
| Harness: graders matched comments, for example `// not sync.Mutex` | the cache and release graders strip Go and shell comments | — |

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

   Before the counted run, the `recall` tool description gained one sentence: facts from earlier conversations are often not in the repository, so call `recall` before guessing one or when a task may continue earlier work. A denied tool is not offered at all, so arm 1 is unaffected. This is the only change to what the model sees between the pilots and the counted run. The harness changed as listed below.

**Attempts.** The counted run is the third attempt. Both earlier attempts are kept (`/tmp/b3-aborted.log`, `/tmp/b3-aborted2.{json,log}`, not version-controlled) and are not counted:

1. Repeat 0 completed. Repeat 1 failed while building its history: each repeat resolved the configuration directories again under the private test home, so the local model's credential was not found. Fixed in `ebe469b`, which resolves them once and saves the records after every run.
2. This attempt was stopped after its first record, because the review found the two harness defects above. Both bias the comparison: the state was reachable from arm 1, and the grading read comments. Fixed in `3e4f418`.

**Counted run (Core `3e4f418`, 3 repeats, 30 B runs, 26 min).** Records: `/tmp/b3-run.json` (not version-controlled).

| Task | Arm 1 `window` | Arm 2 `recall` |
|---|---|---|
| deploy | 0/3 | 3/3 |
| cache | 0/3 | 1/3 |
| metrics | 0/3 | 3/3 |
| release | 0/3 | 3/3 |
| **history tasks** | **0/12** | **10/12** |
| control | 3/3 | 3/3 |
| mean prompt tokens, all runs | 60 602 | 64 700 (1.07×) |

Gate:

1. Arm 2 beats arm 1 by 10 runs (at least 2 needed): **met**.
2. `control` is equal: **met**.
3. Tokens are 1.07× (at most 1.5×): **met**.

So `recall`/`read_thread` stay on by default, recorded in ADR-0010 D6.

Secondary measures:

- **Recall use.** Arm 2 called `recall` in 12 of 12 history runs and in 1 of 3 `control` runs. The relevant thread ranked first in 11 of 12 and within the top 3 in 12 of 12. `read_thread` opened the relevant thread in 6 of 12; in the rest, the snippet was enough.
- **Notes alone.** History threads wrote 4, 6 and 6 notes per repeat. A relevant history note carried the key fact in 10 of 12 (task, repeat) pairs; both misses are in repeat 0 (`cache`, `release`). Arm 1 has no path to other threads' notes (notes are per thread, ADR-0007 arm 1), so these notes reach thread B only through `recall`/`read_thread`.
- **Superseded convention.** `release` chose the retired `release/v…` branch in 0 of 6 runs.
- **Failures.** In arm 2, `cache` failed twice (repeats 1 and 2). Recall ranked the relevant thread first and its note carried the sync.Map decision, but the model kept a mutex. The tool surfaced the fact, and the model did not follow it. In arm 1, one `metrics` run ended `interrupted` at the step limit; it is counted as a failure.

**Reading the result.** Arm 1 cannot reach another thread, so its 0/12 is the expected floor. The run shows two things: continuation across threads needs a cross-thread path, and `recall` provides one at 7% more prompt tokens. It does not compare `recall` with other cross-thread designs, such as per-turn injection. It covers one model only.

## Open

- `cache` arm 2 failures: the decision was retrieved and not applied. Whether to word the recall snippet as a decision, rather than a fact, is a separate change that a new run would have to measure.
- `docs/agent-runtime.md` still describes the Node-era subagent and graph tools. It is outside this change and unchanged.
