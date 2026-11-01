# Working notebook (ADR-0020) — slice report

- **Branch:** `rt/notebook` (worktree `wt/core-rt-notebook`), base `ba0f7a5`
- **Scope:** Core only. Persistent agent working notebook: schema v9, `internal/session` SQL,
  `internal/notebook` Markdown helpers, `notebook_list`/`notebook_read`/`notebook_update` tools, the
  runtime Engine API, the run-start context index, ADR-0020 and the runtime documentation section.
- **Non-goals:** no todo tool, no second Task State, no vector store, no Desktop code, no new
  dependency, no new process or database.

## Status

Implemented and validated in this worktree (see "Tests" for the exact commands and results).

## Design summary

- `notebooks(id, session_id, title, body, bytes, headings_json, version, created_at, updated_at,
  updated_by)` with `UNIQUE(session_id, title)`; `notebook_revisions(notebook_id, version, body, op,
  updated_by, created_at)` with `PRIMARY KEY(notebook_id, version)`.
- `bytes` and `headings_json` are derived from the body at every write, so a list and the run-start
  index never load a body.
- Limits: body 256 KiB, title 1…120 characters, 32 notebooks per session, 50 revisions kept per
  notebook (older pruned in the same transaction).
- Writes are compare-and-set inside one short write transaction; a mismatch returns
  `*session.NotebookConflict` carrying the current version and writes nothing. `append` and `section`
  read and write in the same transaction, so an update without `expected_version` still applies to
  exactly the version it extended.
- Permission `state`: admitted by every policy, read-only included; never `mutatesWorkspace`.
- The run-start index is one user row (origin `notebook`), at most 8 notebooks, at most 600 bytes,
  written by the Start transaction; no body, no row when the session has no notebooks.
- A run reads its own notebooks and its direct children's (`session_relations`), and always writes
  its own session.

## Files

| File | Change |
|---|---|
| `internal/migrate/sql/0009_notebooks.sql` | new tables and index |
| `internal/migrate/sql/down/0009_notebooks.down.sql` | reversible drop |
| `internal/migrate/migrate_test.go` | latest schema assertion 8 → 9 |
| `internal/session/downgrade_test.go` | v9 round trip and 9→6 downgrade |
| `internal/session/notebooks.go` | create/list/get/update/revisions/delete, limits, conflicts |
| `internal/session/notebooks_test.go` | store tests incl. concurrency, pruning, purge, storage bytes |
| `internal/session/purge.go` | notebooks and revisions in `purgeTables`, `purgeWhere` |
| `internal/session/relations.go` | `IsDirectChild` |
| `internal/session/start.go` | `RunStart.Notices` |
| `internal/notebook/notebook.go` | Markdown helpers (sections, append, checklist, headings) |
| `internal/notebook/notebook_test.go` | table tests incl. fences, CRLF, duplicates |
| `internal/toolset/notebook.go` | the three tools, `PermissionState`, `NotebookStore` |
| `internal/toolset/toolset.go` | option, field, registration, policy admission |
| `internal/toolset/toolset_test.go` | read-only test allows `state`, still refuses `write_file` and `bash` |
| `protocol/agent.go` | `OriginNotebook` |
| `runtime/notebooks.go` | Engine API, notebook access for a run, index row |
| `runtime/agent.go` | bind the run's notebook, append the index row in the Start |
| `runtime/prompt.go` | report binds the notebook for a session run |
| `runtime/notebooks_tool_test.go` | tool tests through `Set.Execute`, spec sizes |
| `runtime/notebooks_e2e_test.go` | reopen, context reset, index row, conflicts, events |
| `docs/adr/0020-agent-notebook.md`, `docs/adr/README.md` | ADR and index entry |
| `docs/agent-runtime.md` | "Working notebooks" section and the `state` permission |

## Measurements

| Figure | Value | How |
|---|---|---|
| `notebook_list` spec | 422 bytes JSON, 432 bytes as sent | `TestNotebookToolSpecsAreSmall` |
| `notebook_read` spec | 557 bytes JSON, 567 bytes as sent | same |
| `notebook_update` spec | 928 bytes JSON, 938 bytes as sent | same |
| three specs together | 1,907 bytes JSON, 1,937 bytes as sent | same |
| run-start index row, 8 notebooks | 585 bytes | `TestNotebookIndexRowCarriesNoBody` |
| 32 notebooks × 50 revisions × 8 KiB bodies | 14,536,704 bytes on disk (12.5 MiB of bodies) | `TestNotebookStorageBytes` |

## Limits and risks

- A notebook is state in the state database (ADR-0016); a user who wants a copy uses
  `ExportNotebook` or the Desktop export. There is no import.
- Revision bodies are kept, so a notebook's cost is up to `50 × body`; the limits above bound it.
- The index row is appended at every Start of a session that has notebooks; a session that starts
  runs very frequently adds one short row per run (bounded at 600 bytes).
- Notebook tools are not part of the workflow contract's tool observations; they do not affect
  gate decisions by design.

## Tests (run 2026-11-01 in this worktree)

| Command | Result |
|---|---|
| `gofmt -l <changed files>` | no output |
| `go build ./...` | ok |
| `go vet ./...` | ok |
| `go test -count=1 ./internal/notebook/ ./internal/session/ ./internal/toolset/ ./internal/migrate/ ./runtime/` | all ok |
| `go test ./... -count=1` | all packages ok, `cmd/lcx-go` included |
| `go test -race -count=1 -run 'TestNotebook...' ./internal/session/ ./runtime/` | ok |

Measured in `TestNotebookToolSpecsAreSmall`, `TestNotebookIndexRowCarriesNoBody` and
`TestNotebookStorageBytes`; the figures are in the table above.
