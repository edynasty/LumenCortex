# Core plugin platform and generic session import — 2026-10-22

- **Status:** implemented and validated locally; not pushed.
- **Baseline:** Core `d2eaa01` (ADR-0011 OMP import, OMP-specific Engine methods).
- **Decision:** [ADR-0012](../adr/0012-core-plugin-platform.md).

## Delivered

| Capability | Where |
| --- | --- |
| Public contract: manifest, `Plugin`, `Host`, `Event`, hook types, `SessionImportSource`, bundle types | `pluginapi/` |
| Registry: validation (API version, known/duplicate/implemented capabilities, unique source ids), id-ordered start, reverse stop, panic recovery on every plugin call, failure isolation, persisted enablement applied live, catalog diagnostics | `runtime/plugins.go` |
| Hook dispatcher: priority/id/registration order, per-hook timeout (default 2 s, max 30 s), fail-open/fail-closed, refusal vs failure, panic fails the plugin, caller cancellation | `runtime/plugins.go` |
| Generic import pipeline: bundle validation, source-scoped ids (OMP ids unchanged), dedupe, Core-owned provenance and imported row, one transaction, warnings | `runtime/session_import.go` |
| OMP reader as the built-in source plugin `omp-import` | `internal/ompsession/plugin.go` |
| CLI `lcx-go import sources|list|preview|run`, `lcx-go plugins [list|enable|disable]` (replaces `lcx-go omp`) | `cmd/lcx-go/import.go` |

Removed: `runtime/omp_import.go` (`Engine.OMPSessions`, `PreviewOMPSession`, `ImportOMPSession`,
`ImportedOMPSessionID`, `OMP*` types), `cmd/lcx-go/omp.go`. ADR-0011 D12 is marked superseded.

## Tests run

- `go build ./... && go vet ./...` — pass.
- `go test -count=1 ./...` — pass (all packages).
- `go test -count=1 -race ./runtime -run 'Plugin|Hook|Import|OMP'` — pass. The first race run
  found a data race in the hook-order test's own recorder (a timed-out hook keeps running in its
  goroutine, by design); the recorder now locks.
- `go test -count=1 ./internal/ompsession` — pass (two expectations updated: the converter no
  longer emits the trailing imported row; Core appends it).

New coverage: `runtime/plugins_test.go` (manifest validation, lifecycle order, start error and
panic isolation, enablement persistence and live apply, invalid state file never overwritten, hook
order/refusal/timeout/panic, capability gating, listener stream closing) and
`runtime/session_import_test.go` (OMP end to end, a second fake source through the same pipeline,
ten invalid bundles that write nothing, adapter panic isolation, disable/re-enable of OMP, the
ADR-0011 id literal).

## Smoke (real data, temporary workspace and config dir)

`/tmp/lcx-plug` built from the working tree; `XDG_CONFIG_HOME` and the workspace were temporary.

- `import sources` → `omp` (converter 1, default root `~/.omp/agent/sessions`); `plugins` →
  `omp-import` built-in, enabled, running.
- `import list omp --all --limit 3` → three real sessions, newest first.
- `import preview omp <sub2api session>` → `session_95a2063abe7e8e82367cb6e8`, 71 rows,
  `workspace_mismatch`; `import run` → 1 session written; second `import run` →
  `alreadyImported: true`. The id equals the ADR-0011 formula for that OMP id.
- SHA-256 of the source file and its subagent directory identical before and after.
- `plugins disable omp-import` → `disabled`, `plugins.json` lists it, `import sources` → `[]`,
  `import preview omp …` → `import source is not available: "omp"`; `plugins enable` → `running`.

## Limitations

- In-process plugins are trusted code with Core's full authority; there is no process isolation
  (ADR-0012 D9). A hook that ignores its context keeps running after its timeout and after its
  plugin is disabled (measured: control returned at 11 ms, handler ran its full 200 ms). `Start`,
  `Stop` and source calls have no Core deadline (a 300 ms `Start` delayed `StartAll` by 300 ms).
  Lifecycle/source deadlines: TODO PLUGIN-003; external process plugins: TODO PLUGIN-002.
- Enablement is per user (global dir), applied to other open Engines on their next open.
- Event delivery stays best-effort; no durable outbox.
- Codex and Pi sources are not implemented; the contract and the fake-source test show they need
  no Core change.
