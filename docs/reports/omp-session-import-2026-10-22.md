# OMP session import (ADR-0011)

- **Date:** 2026-10-22
- **Status:** implemented and validated locally; committed to local Core `main`, not pushed.
- **Baseline:** Core `7033dea` (agent presets `ff07185` committed before it).
- **Decision record:** [ADR-0011](../adr/0011-omp-session-import.md).
- **Desktop:** list/preview/import dialog and thread-view notices, see the Desktop report
  `docs/reports/omp-session-import-desktop-2026-10-22.md`.

## What a user can do

| Capability | State |
|---|---|
| List OMP sessions of this workspace (or all) with an "imported" marker | `Engine.ListImportSessions("omp", …)`, `lcx-go import list omp [--all] [--query] [--limit]` (was `Engine.OMPSessions`, `lcx-go omp list` before ADR-0012) |
| Preview what an import writes, with warnings, without writing | `Engine.PreviewImport("omp", …)`, `lcx-go import preview omp <file>` |
| Import a session and its subagent sessions as native sessions | `Engine.ImportSession("omp", …)`, `lcx-go import run omp <file>` |
| Continue an imported conversation | ordinary Start (`RunAgent`, Desktop composer) |
| Re-import safely | same ids; reports `alreadyImported`, and `sourceChanged` when the file changed |

## Not supported (by decision, see ADR-0011 D1)

- Taking over a task OMP is still running, or OMP's in-flight state: pending calls, model/thinking
  selection, todo/goal state, IRC jobs, OMP worktrees. A live file is imported as a snapshot with
  the `maybe_running` warning.
- Hidden reasoning, signatures, provider payloads, compaction and branch summaries, the subagent
  system prompt (never decoded).
- Images and other attachments (placeholders; blobs are not read).
- Off-branch records (counted in `offBranch`).
- Subagent transcripts beyond 100 sessions or depth 4 (counted in `childrenSkipped`).
- Updating an import from a changed source (delete the imported session, then import again).
- Files outside `<OMP agent dir>/sessions/<project>/`, and importing a subagent transcript alone.

## Changes

| Area | Files |
|---|---|
| Transcript origins `imported`, `imported_notice`, `imported_marker`; `ImportedContent` | `protocol/agent.go` |
| Core metadata key `import` | `internal/session/metadata.go`, `runtime/engine.go` (comment) |
| Atomic multi-session insert, `ErrSessionExists` | `internal/session/import.go` |
| OMP reader (snapshot, branch resolution, subagent discovery), converter, listing and path guard | `internal/ompsession/{source,convert,list}.go` |
| Engine API, provenance, warnings, event/journal `session.imported` | `runtime/session_import.go` (generic since ADR-0012); OMP source plugin `internal/ompsession/plugin.go` |
| CLI | `cmd/lcx-go/omp.go`, `cmd/lcx-go/main.go` |
| At-rest scan helper generalized to several texts | `runtime/echo_test.go` |
| Docs | ADR-0011, ADR index, `docs/data-model.md`, `docs/go-provider.md` |

## Format evidence

From the local OMP 18.12.6 installation (`src/session/session-entries.ts`, `session-context.ts`,
`sub-sessions.ts`, `blob-store.ts`, `registry/agent-tombstone.ts`, `advisor/transcript-recorder.ts`)
and a survey of the 1,189 local session files (2.4 GB); figures in ADR-0011 Context. The leaf
resolution mirrors `resolveSessionLeafId`/`lastSelfSelectedLeafId`; child discovery mirrors
`collectSubSessionsFromDir` (sorted names, advisor and dot files skipped, recursion only into real
directories named after a transcript).

One finding changed the converter during validation: OMP marks a retried assistant turn with
`retryRecovery`, but in one real session such a turn carried a call whose result was recorded.
Dropping the turn orphaned that result. A retried turn with calls is now kept (unit test
`TestConvertRetriedTurnWithRecordedCallStays`); after the fix the whole local corpus has 0 orphan
results.

## Tests

| Test | Covers |
|---|---|
| `internal/ompsession` `TestReadFollowsActiveBranchAndSubagentLayout` | real-format fixture: title slot, header, abandoned branch, usage marker off-branch, nested/killed/advisor/lock/artifact children |
| `TestConvertKeepsVisibleHistoryOnly` | exact row sequence; 13 sentinels (thinking, signatures, payload, summaries, hidden reminders, system prompt, abandoned branch, advisor) absent; id normalization; error and image results; notice held until results; compaction and branch markers; retried and thinking-only turns; interrupted call; import row |
| `TestConvertSubagentUsesItsAgentID`, `TestConvertRetriedTurnWithRecordedCallStays` | child goal; retried turn with a call |
| `TestReadExcludesPartialTail`, `TestReadMalformedAndHeaderless` | live file snapshot and hash; malformed record; not a session |
| `TestActivePathLeafResolution` | activeLeafId, null clear, stale leaf fallback, parent cycle |
| `TestNormalizeToolCallID`, `TestTruncateKeepsUTF8`, `TestListAndResolvePath` | id pattern; UTF-8 cut; listing; subagent file, outside file and symlink escape refused |
| `internal/session` `TestImportSessionsIsAllOrNothing` | rollback on a later failure, running status refused, existing id writes nothing, relation and next seq |
| `runtime` `TestOMPImportIsIdempotentAndLeavesSourceUntouched` | preview writes nothing; 4 sessions; subagent tree; provenance and `ui.title`; second import `alreadyImported`; source tree digest (names, modes, mtimes, bytes) unchanged; list marker; workspace filter; changed source reported, transcript unchanged |
| `TestOMPImportRefusesPathsOutsideTopLevel` | subagent transcript alone and outside file refused |
| `TestImportedOMPSessionContinues` | continuation request = Task State + imported rows in order + import row + input; no tool row written; sentinels absent from the request and from every table, WAL, shm and backup |
| `TestImportedOMPSessionMetadataAndDelete` | host cannot set or overwrite `import`; delete removes the 3 imported children; re-import after delete |

Commands run (all pass): `go test -count=1 ./...`, `go vet ./...`, `gofmt -l` on the changed files.

## Real-session smoke runs (throwaway workspaces under `/tmp`)

| Run | Result |
|---|---|
| `omp preview` on all 258 local top-level sessions (821 subagent sessions converted, 112 beyond bounds) | 258/258 ok in 44 s; 219,078 rows; 115,822 calls / 115,817 results (5 interrupted); 65,085 thinking blocks and 456 compactions left out; 0 malformed, 0 orphan results; warnings: `maybe_running` 1 (the session running this work), `open_tool_calls` 1, `children_capped` 2 |
| Import of a 13 MB session with 15 subagents (20 sessions) twice | 1.07 s; second run `alreadyImported`; SHA-256 of the file and of the subagent tree identical before and after; 645 thinking texts, 9 summaries and 653 signatures searched in the database: 0 found |
| Preview of the largest session (152 MB, 114 subagents) | 3.8 s, 278 MB peak RSS, 18,735 rows, 100 children + 14 skipped |
| `lcx-go resume` of an imported small session and of the 2,494-row session with a real model (read-only, 1–2 steps) | the run windowed the long transcript, called only LumenCortex tools (`list_dir`, `git_status`, `recall`) and re-checked the workspace as the import row asks; no historical call ran. Before the import row named the tool-name difference the model once tried OMP's `bash` (refused as not in the working set); the row now says so |

## Risks

- Peak memory of a preview grows with the file (278 MB for 152 MB plus 100 children): the branch
  entries and converted rows are held in memory at once.
- Continuation sees OMP tool names in history; the import row tells the model to use only the
  current tools, but a model can still try one (refused, harmless).
- An OMP format change (version > 3, new roles or block types) imports what it recognizes: unknown
  roles count as hidden, unknown blocks are dropped. The importer version in provenance identifies
  the rules used.
- Recall (ADR-0010) finds imported sessions; their content describes the source workspace when the
  `workspace_mismatch` warning was shown.
