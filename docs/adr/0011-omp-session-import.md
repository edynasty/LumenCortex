# ADR-0011: OMP session import

- **Status:** Accepted (owner direction 2026-10-22: import OMP sessions as native sessions; history completeness, idempotence and data safety first)
- **Date:** 2026-10-22
- **Owners:** project owner
- **Supersedes:** none
- **Superseded by:** ADR-0012 D5 for the surfaces (D12) only

## Context

Users move to LumenCortex from OMP (oh-my-pi, `@oh-my-pi/pi-coding-agent`, observed at 18.12.6). Their
history lives in OMP session files, `<agentDir>/sessions/<project>/<timestamp>_<id>.jsonl`
(`agentDir` = `PI_CODING_AGENT_DIR` or `~/.omp/agent`). A survey of the 1,189 local session files (2.4 GB; counts below from 200–400-file
samples) established the format that matters:

- Line 1 is a fixed-width `title` slot, line 2 the `session` header (`version` 3, `id`, `cwd`,
  `timestamp`, optional `parentSession`). Every other record has `id`/`parentId` and forms a tree;
  the file keeps every branch. OMP resolves the active branch from the last record: its
  `activeLeafId` when present (late `model_usage` markers carry it; `null` clears the branch), a
  captured leaf that no longer exists falls back to the newest self-selecting record, then walks
  parents to the root (`resolveSessionLeafId`). In a 400-file sample 171 of 176,023 records were
  branch points and 162 records sat off the active branch, in 7 files.
- `message` records carry `user`, `assistant`, `toolResult`, `developer` and `custom` roles.
  Assistant content mixes `text`, `toolCall` (`id`, `name`, `arguments` object) and `thinking`
  blocks (with signatures); a 400-file sample held 29,486 thinking blocks against 44,501 assistant
  messages. In a 200-file sample 19,337 of 44,296 tool call ids (OpenAI Responses `call_…|fc_…`) do not match the id pattern providers accept.
  Assistant turns can be replaced by a retry (`retryRecovery`) or hold only thinking.
- `compaction` (468 seen) and `branch_summary` (18) records carry model-written summaries that may
  be derived from hidden reasoning. `custom_message` records are harness notices; `display` says
  whether the user saw them. `custom` records (`session_exit`, todo state, tool start markers) are
  not conversation.
- Images are `image` blocks whose data is usually a `blob:sha256:<hash>` reference into
  `<agentDir>/blobs`.
- Subagent transcripts live in `<file stem>/<AgentId>.jsonl`, nested the same way, with a
  `session_init` record (agent name, task, system prompt). Killed ones have a `.tombstone`; advisor
  transcripts (`__advisor*.jsonl`), backups, `.lock.os` sidecars and artifacts share the directory.
- OMP writes while it runs: a file may end in an unterminated record.

Core sessions are SQLite rows with a text-only `protocol.Message` transcript (ADR-0004), contiguous
seqs, every assistant call answered by exactly one directly following tool row
(`window.Groups`), hidden reasoning only in run memory, and provenance in `metadata_json`. There was
no import path and no external id.

## Decision

**D1 — Scope.** Core imports OMP history as new native sessions of the open workspace. It supports
three things and states the rest:

| Capability | Support |
|---|---|
| History migration (visible conversation, calls, results, subagents) | yes |
| Continue the imported conversation in LumenCortex | yes: an imported session is an ordinary idle session; a Start uses this workspace's agent, model list, tools and policy |
| Take over a task OMP is still running | no: the import is a snapshot of the complete records; a recently written file without `session_exit`, or one with a partial tail, gets the `maybe_running` warning |
| Resume OMP's in-flight state (pending tool calls, model/thinking selection, todo/goal state, IRC jobs, worktrees) | no: runtime state does not transfer; unanswered calls become interrupted results |

**D2 — Only the visible active branch.** The importer resolves the active branch exactly as OMP
does and copies it in order. Off-branch records are counted, not copied. A `branch_summary` and a
`compaction` become an `imported_marker` row saying where the event happened; their summaries are
not copied. The marker states that the rows above are the complete visible history (compaction in
OMP shortened the model's context, not the file).

**D3 — Hidden content never enters Core.** The decoder has no field for `thinking` text,
signatures, `providerPayload`, summaries or `systemPrompt`; thinking-only assistant turns, and retried
turns without calls, are dropped (a retried turn with calls stays, because its calls may have run
and a recorded result must keep its call). `developer` messages, `custom` records and `custom_message`s with
`display: false` (reminders, nudges) are counted and left out. Visible notices become
`imported_notice` rows labelled `[OMP <customType>]`.

**D4 — Tool history is never executed.** Calls and results are copied as transcript rows. An id
that providers reject is replaced by `omp_` + 40 hex of its SHA-256 on both sides. A result that
answers no open call, or a repeated id, is dropped and counted. Calls without a result get the row
reconciliation writes (`tool_interrupted`, `session.InterruptedToolResult`). Notices that arrived
between a call and its results are held until the turn's results are written. Error results start
with `[error]`. A row longer than 256 KiB is cut on a UTF-8 boundary with the count of bytes left
out. The converted transcript must pass `window.Groups` or the import fails.

**D5 — Import notice.** Every imported transcript ends with an `imported` user row
(`protocol.ImportedContent`): the rows above ran in another runtime, against the workspace as it
was then, and nothing was executed by LumenCortex. The next Start sees it before the user's input.

**D6 — Attachments.** Images become text placeholders naming the MIME type (`[image/png attachment
not imported: …]`). Blobs are not read. Core transcripts are text-only (ADR-0004); attachment
support is a separate decision.

**D7 — Subagents.** Child transcripts are listed the way OMP lists them (sorted `*.jsonl`, no
advisor/backup/dot files, recursion only into real directories named after a transcript), bounded
like a subagent tree (100 sessions, depth 4; the rest is counted in `childrenSkipped`). Each becomes
a native session with a `subagent` relation to its imported parent and `subagent.mode` `imported`;
its goal is the OMP agent id and agent name (`Scout1 (scout)`), because OMP tasks open with a
generic preamble. A tombstone sets `aborted` in the preview.

**D8 — Identity, idempotence, atomicity.** A session's id is `session_` + 24 hex of
`SHA-256("omp:" + OMP session id)` (a child without its own id uses `<parent id>/<key>`). The whole
tree is written by `Store.ImportSessions` in one `BEGIN IMMEDIATE` transaction; an existing id
writes nothing and returns `ErrSessionExists`, so a repeated or concurrent import reports
`alreadyImported`. An imported session is never replaced, because it may have been continued:
when the source has changed since (size or SHA-256 of the imported bytes differ) the result and
preview say `sourceChanged`; deleting the session allows a fresh import.

**D9 — Provenance.** `metadata.import` is a new core-owned key (hosts cannot write it): source,
importer version, OMP session id, path, subagent key, SHA-256 and size of the complete records
read, mtime, import time, cwd, title, agent, model, `openToolCalls`, off-branch count and the
conversion counts. The OMP title also seeds the host `ui.title`. Sessions are created
`completed` with no model or agent pin: the first Start pins as for any session (ADR-0009).

**D10 — Source safety.** Files are opened read-only; nothing is locked, renamed or written; blobs
are not read. A preview and an import read one consistent snapshot: one pass indexes and hashes the
complete records, the second reads the branch records from the same descriptor at the indexed
offsets. Only a top-level session file of the sessions root is accepted, after resolving symlinks
(a subagent transcript, a file outside the root and a symlink escaping it are refused). A record
over 64 MiB fails the read; malformed records are counted and skipped.

**D11 — Workspace mapping.** The import goes to the open workspace. The list shows sessions whose
`cwd` is the workspace or below it (real paths) unless `all` is set; a session from another
directory imports with the `workspace_mismatch` warning, because its paths and results describe
that directory.

**D12 — Surfaces.** *Superseded by ADR-0012 D5.* The OMP reader is now the built-in import source
plugin `omp-import` (`internal/ompsession.Plugin`, source id `omp`), and Core's generic pipeline
(`Engine.ImportSources`, `ListImportSessions`, `PreviewImport`, `ImportSession`; `lcx-go import
…`) does the validation, ids, provenance, trailing `imported` row and transaction described here.
Native ids are unchanged. Desktop lists, previews and imports through the generic surface, then
opens the session in its thread view.

## Invariants

- No source byte, lock, sidecar or blob is written or read beyond the session files.
- No thinking text, signature, provider payload, summary or system prompt is stored or sent.
- An imported transcript satisfies `window.Groups` and ends with the `imported` row.
- No imported call is executed; continuation only appends.
- One OMP session maps to at most one native session tree per workspace database.

## Alternatives considered

### Copy compaction summaries

They let the model see what OMP's model saw after a compaction. Not selected: they are model text
that can restate hidden reasoning, and the file keeps the full visible history they summarize.

### Re-import replaces the earlier import

Keeps the copy current. Not selected: the imported session may have been continued here, and
silent replacement loses that work. The owner deletes and re-imports explicitly.

### Store attachments as Core artifacts

Not selected now: transcripts are text-only and no provider path reads artifacts as images.

## Consequences

### Positive

- OMP history becomes browsable, recallable (ADR-0010) and continuable without leaving LumenCortex.
- Repeated imports are safe; failed imports leave nothing behind.

### Negative / trade-offs

- Images, hidden reasoning, summaries, off-branch work and OMP runtime state are not carried over.
- An import is a snapshot; later OMP activity is reported, not merged.

## Security and trust impact

Imported content is untrusted history. The import row tells the model not to rely on it without
re-checking. Paths are confined to the sessions root. A new core-owned metadata key keeps hosts from
forging provenance.

## Persistence / compatibility impact

No schema change. New transcript origins `imported`, `imported_notice`, `imported_marker`; new
core metadata key `import`; relation metadata `{"mode":"imported","ompAgent","key"}`.

## Validation plan

Real-format fixture tests (branch, compaction, retry, thinking, payload, image, notices, unanswered
call, nested/killed/advisor subagents, partial tail, malformed record, symlink escape), store
atomicity, runtime idempotence/source-unchanged/continuation/at-rest scans, and real-session smoke
runs (`docs/reports/omp-session-import-2026-10-22.md`).

## Documentation impact

`docs/data-model.md` (origins, metadata key), this ADR, the report, the ADR index.
