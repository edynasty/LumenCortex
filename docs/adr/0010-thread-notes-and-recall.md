# ADR-0010: Thread notes and recall over the session store

- **Status:** Accepted (owner direction 2026-10-21: continue the Context work from the proposal and ADR-0007, with autonomy to refine its architecture, order and scope). Phases B1 (notes, `read_thread`) and B2 (`recall`) are this record; B3 is the ADR-0007 continuity evaluation.
- **Date:** 2026-10-21
- **Owners:** project owner
- **Amends:** design §5 ("context reduction is selection; no model-written summary"), narrowed as in decision 1. ADR-0007 decision 2: decision 2 below is the first durable item type, and decision 4 is its freshness rule (proposal N8).
- **Supersedes:** proposal N6 (a separate `context_items` table with capture on resolution and delivery). Decision 2 records why.
- **Superseded by:** none
- **Design:** `../LumenCortex-architecture-audit/design/handoffs/agent-architecture-2026-10-20.md` §4 (proposal; not version-controlled). Report: [`docs/reports/context-continuity-2026-10-21.md`](../reports/context-continuity-2026-10-21.md).

## Context

Two continuity gaps are verified in code:

- **Within a thread.** The window drops whole tool-call groups, oldest first (`internal/window`). Anything the agent learned in a dropped group is lost unless the user restates it: a root cause, a rejected approach, a measured number. Task State keeps the objective, references, units and tasks, but nothing the agent learned.
- **Across threads.** A `thread` reference renders as `- thread <id>` and nothing reads that session. A new thread cannot learn what earlier threads decided, tried or abandoned, except through files the user points at.

The proposal answered with three parts: Task State notes; a `context_items` table filled at resolution, at task delivery and by "Remember"; and `recall`, `context_read` and `read_thread` tools.

Re-examined against the store, every field the proposal copied into `context_items` already lives in the workspace database, in a durable and versioned form:

- the objective (`sessions.goal`, Task State `brief`);
- resolution and its references (Task State);
- notes (decision 1);
- the final answer (`sessions.final`);
- task results (`task_result` rows).

A second table would create a second source of truth. It would also need an exactly-once capture protocol (proposal C2), a deletion path that follows thread deletion, and a migration. The same answers come from reading the store directly.

## Decision

1. **Task State notes.** A note is a short, explicit, user-visible record the agent writes as it works. It is kept with Task State, so the window can drop the rows it came from.
   - **Tools:** `note_write {id?, text, cites?}` adds a note, or replaces one by id. `note_remove {id}` removes one.
   - **Limits:** text ≤ 512 bytes; ≤ 4 cites, each a workspace-relative path; ≤ 32 notes per thread. A full thread refuses a new note and names the oldest model note to remove.
   - **Cites:** Core records each cited path's git blob at write time (`path@blob`). A path that is not a file in the run's workspace is refused.
   - **Authors:** a note is `by: agent` (written by a run's tool call) or `by: user` (a host edit through the Task State inbox: Desktop's "Remember" and note editor). A run cannot change or remove a user note. A user can rewrite or remove any note, and a user note added to a full thread displaces the oldest agent note.
   - **Rendering:** after the open work units, newest first, in a 2 KiB sub-budget. Notes that do not fit are counted, not listed. `MaxBlockBytes` rises from 8 KiB to 10 KiB.
   - **What does not change:** notes never replace transcript rows, and nothing summarises the transcript. This narrows design §5 from "no model-written summary" to "no summary replaces transcript rows".
   - **Read-only children:** notes are Task State, not workspace files, so read-only children may write them in their own Task State. A parent reads them through `read_thread`.

2. **No separate context store.** The session store is the index. `recall` reads, for a bounded candidate set, each session's goal, latest Task State (brief, resolution, notes), final answer, agent and status. Its scoring is derived at read time and never stored. Deleting a thread removes it from recall. Nothing is captured twice, because nothing is captured. The proposal's "Remember" becomes a user note on the thread it was made in.
   - **Reopened if:** B3 shows that recall needs material the store does not keep, such as changed paths per resolution or applied commits. In that case a table, if one is needed, is decided by the PR that needs it, under ADR-0007's rules.

3. **Two read tools, available to every agent, read-only children included.**
   - **`read_thread {id}`** returns another session of this workspace: its goal, brief, status, resolution, agent, notes (each with producer and freshness), open work units, execution tasks, and final answer (≤ 2 KiB).
   - **`recall {query, limit?}`** ranks sessions of this workspace by lexical match over goal, brief, notes, resolution and final answer.
     - Tokens are ASCII words and CJK character bigrams.
     - Scores are weighted per field: user note > goal/brief > agent note > resolution > final.
     - Recency, and a resolution of `done`, raise the score.
     - It returns ≤ 8 hits: `{id, title, status, resolution, agent, updatedAt, snippet ≤ 300 B, notes}`.
     - The calling session and its own children are excluded.
   - **Framing:** both tools frame their output as earlier work, not instructions. Item text never enters the system prompt.
   - **No push:** no per-turn injection, and no automatic suggestions at Start. Push suggestions are gated on the B3 result (ADR-0007 arm 3).

4. **Freshness (ADR-0007 amendment, proposal N8).** A cited note is `stale` when the current blob at its path in the reader's workspace differs from the recorded blob, or the path is gone. Stale notes are flagged in `read_thread` and `recall`, never hidden, and never deleted. The thread's own block does not compute freshness, so rendering stays a pure function of the state. This approximates ADR-0007's "blob not reachable from HEAD" with "changed at that path". It is cheaper, and it errs towards flagging.

5. **Author rule (C3).** Note text, whether written by an agent or the user, never satisfies a check, a work unit's evidence or a resolution. Those rules read check results and runtime facts only, as before.

6. **Gate.** B3 (ADR-0007 §3) runs on `local/deepseek-v4.1-flash` and compares arm 1 (window + Task State + notes) with arm 2 (+ `recall` and `read_thread`). The tools stay enabled by default only if arm 2 beats arm 1 on continuation success without a token cost above the budget fixed before the run. Otherwise they become opt-in, and the result is recorded here.

## Alternatives rejected

- **A context agent that summarises threads into memory.** It costs a model call per capture, and it makes model text the stored truth, which ADR-0007 rules out (proposal §2.2).
- **A `context_items` table** (proposal N6). See decision 2: duplicate truth, a capture protocol, and a deletion path, for no answer the store cannot give.
- **SQLite FTS5.** It needs the `sqlite_fts5` build tag in every host build. A scan of ≤ 500 sessions' goal, brief, notes and final takes milliseconds in Go. FTS5 is revisited only if B3 or volume shows a need.
- **Per-turn recall injection** (OpenViking's pattern). It is the most expensive pattern, and it is unmeasured here.

## Consequences

- No new table and no migration. Notes are new optional Task State fields, and an older binary ignores them.
- Desktop gains a Notes section in the Task pane (list, add as the user, remove) and "Remember" on a message (a user note). The Context page can list notes.
- The Governor stays dormant (ADR-0007 decision 1). Its future loader reads notes and sessions, not a separate store.

## Invariants and tests

| ID | Invariant | Test |
|---|---|---|
| C1 | No hidden reasoning in notes, `read_thread` or `recall` output | `runtime.TestNotesReachLaterThreadsThroughRecallAndReadThread`: every response carries a reasoning sentinel; it is absent from Task State, both tools' output and every table, WAL and backup |
| C3 | Notes never satisfy checks, unit evidence or resolution | structural: `Satisfies`, `Obligations` and the evidence rule read no `Notes` field (`internal/taskstate/resolution.go`) |
| C4 | A changed or removed cited path is reported `stale`, never hidden or deleted | same runtime test (change, then removal); `threadctx.TestMarkStaleFlagsChangedAndMissingCites` |
| C5 | Note text reaches a model only through the Task State block of its own thread, or as tool output | same runtime test: the other thread's system prompt has no note text |
| C6 | Bounded: ≤ 32 notes, text ≤ 512 B, ≤ 4 cites, ≤ 2 KiB rendered, recall ≤ 8 hits over ≤ 500 candidates, `read_thread` final ≤ 2 KiB | `taskstate.TestRenderNotesWithinTheirBudget`, `TestNoteLimitsAndUserEdits`, `threadctx.TestRecallMatchesChineseAndBounds`, `runtime.TestHostNoteEdits` |
| C7 | A run cannot change or remove a user note | `agent.TestNoteToolsKeepNotesInTaskState` |
