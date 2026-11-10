# Harness evolution — Editing slice (Hashline + AST editing)

Date 2026-11-11. Slice owner: EditingSlice. Branch `feat/harness-evo-editing`, worktree
`wt/core-harness-evo-editing`, baseline Core `696eb74` (main). Design authority:
`bench/evidence/agent-harness-capability-roadmap-20261110/HASHLINE_AND_AST_EDITING_DESIGN.md`, with the
prototypes `prototypes/e3-hashline` (S1–S9) and `prototypes/e4-ast-edit` (8 cases).

## Status

| Capability | Status | Evidence |
|---|---|---|
| Hashline read (`read_file` anchors + revisions) | Validated | `internal/toolset/editing_tools_test.go`, `internal/editing/engine_test.go` |
| `edit_lines` (anchored line ops, stale rejection, relocation) | Validated | E3 S1–S8 ports |
| Single write path (`Commit`) for write_file / replace_in_file / apply_patch / lsp_rename / edit_lines / edit_go | Validated | toolset suite + `TestMultiFileCommitIsAtomic`, `TestMultiFileCommitRollsBackOnWriteFailure` |
| write_file stale-read race (E3 S7b) fixed | Validated | `TestWriteFileRefusesStaleReadThroughToolset`, `TestWriteFileRefusesAStaleReadInTheRunRegistry`, `TestWholeFileWriteRefusesStaleBaseRevision` |
| Shared write leases (engine + runtime host API) | Validated | `TestWriteLeaseRefusesOtherHolder`, `TestWriteLeasePrefixAndExpiry`, `TestWriteLeaseBlocksAnotherSessionsWriteTool` |
| Go AST editing (`edit_go`) | Validated (Go only) | E4 1–8 ports, `TestEditGoThroughToolset` |
| `edit.committed` / `edit.conflict` events | Implemented | published through `e.events.publish`; not asserted by a dedicated test (see limitations) |
| Agent loop end-to-end | Validated | `TestAgentLoopEditsLinesAndRejectsAStaleEdit` (scripted provider) |
| TS/JS/Python structured editing | Not in this slice | remains the existing LSP path (`lsp_rename`, code actions); no AST claim for those languages |

## What existed before

- `read_file` returned the whole file (128 KiB cap) with no line numbers and no revision
  (`internal/toolset/files.go`).
- `write_file` / `replace_in_file` / `apply_patch` / `lsp_rename` ended in `writeChecked`
  (`internal/toolset/path.go`): open without truncating, check protected state on the descriptor,
  truncate, write. No content precondition, no compare-and-swap, no revision binding.
- Consequently an agent editing from an old read silently overwrote a concurrent change (E3 S7b).

## What changed

### 1. `internal/editing` — the one write path

- `doc.go`: `Doc` (raw bytes, lines, per-line terminators, dominant EOL, BOM), `RevOf`
  (base32(SHA-256(raw))[:8], 40 bits), `LineHash` (base32(SHA-256(trimRight(line," \t")))[:3]),
  anchors `N:hhh`, `Render` / `RenderWindows`, `Splice`, `LineOp`, `ApplyLineOps` (descending,
  preserves final-newline state), `ApplySplices`, `NormalizeText`, `ValidateSplices`,
  `MaxFileBytes = 4 MiB`.
- `store.go`: per-session bounded snapshot store (`path, rev → doc + seen lines`), LRU over entries
  with 64 paths / 16 MiB / 8 revisions per path; light `RegisterRev` for plain reads; `LeaseStore`
  with path or directory-prefix leases, TTL, expiry, overlap conflicts, `LeaseConflict`.
- `engine.go`: `Engine.Commit(ctx, Plan) (Result, error)`.
  - resolves every path with the caller's `Host` (the toolset's existing `resolve` + guard),
  - dedupes and sorts canonical paths, takes 256 lock stripes in ascending order,
  - checks write leases (`lease_conflict{holder}`), reads each file through the guarded descriptor,
    enforces `BaseRev` and the session-bound stale-read rule (`RejectStaleRead`),
  - applies `Content` / exact `Edits` / byte `Splices` / hashline `LineOps` (with bounded relocation),
  - validates (Go syntax, Go types), previews or writes,
  - writes with a compare-and-swap on the same open descriptor right before truncate+write, rehashes
    after the write (`concurrent_write_detected`), rolls every file back on any failure,
  - registers the new revision, publishes `edit.committed` per file and `edit.conflict` on refusal.
- `ops.go`: `ParseOps` (anchor hash + "seen line" checks, overlap rejection), `rebaseLineOps` (every
  edited line and its ±1 neighbours must map contiguously, otherwise `stale`).
- `diff.go`: Myers O((N+M)D) with `maxDiffEdits = 256`, `maxDiffLines = 200 000`; `UnifiedDiff` for
  previews.
- `goval.go`: `GoSyntax()` and `GoTypes()` validators; the type checker uses the source importer for
  the standard library and **degrades to syntax-only** with an explicit `type_check_degraded`
  diagnostic when the package imports anything else (module deps cannot be resolved from source
  here).
- `goedit/`: Go AST editing. `go/parser` + `go/ast` locate, `go/format` only names a receiver type;
  edits are byte splices against the parsed bytes, so comments and formatting outside the target are
  preserved. Scope-aware rename uses `ast.NewPackage` object resolution (cross-file, shadowing-safe,
  capture refused). Ops: find, replace_body, replace_decl, add/remove/rename parameter,
  add/remove/replace struct or interface member, add/remove import, rename (package symbol, method,
  field, param; multi-file within the package).

### 2. Toolset

- `read_file` gains `start_line`, `end_line`, `anchors`. The anchored view is
  `@@ path rev=… lines=N eol=lf|crlf bom=…` followed by `N:hhh|text` rows; the default (no
  `anchors`) output keeps the existing JSON shape and adds `rev`. Plain reads register the revision
  only, so they cannot be edited without an anchored read but still make a stale whole-file write
  detectable.
- New `edit_lines` {path, rev, ops[{op, start, end, text}]} → new rev, `applied`, and fresh anchors
  around each landed edit (`landed` line ops in the result).
- New `edit_go` {path, ops[…], preview} → symbols, notes, diff on preview, `typeCheck` note.
- `write_file`, `replace_in_file`, `apply_patch`, `lsp_rename` now build a plan and commit through
  the engine; `write_file` sets `RejectStaleRead` and accepts `base_rev`.
- `internal/toolset/editing.go` makes `Set` the engine's `Host`: same `resolve`, same
  descriptor-level `denyIfProtected`, same event bus.

### 3. Runtime

- `runtime.Engine` owns one `*editing.Engine` per workspace (`runtime/engine.go`), passed to every
  run's toolset with an event sink (`runtime/agent.go`, `runtime/eval_artifacts.go`).
- `runtime/editing.go`: `AcquireWriteLease`, `RenewWriteLease`, `ReleaseWriteLease`, `WriteLeases`
  with `WriteLeaseRequest{Root, Path, Holder, Prefix, TTLSeconds}`; `WriteLease` aliases
  `editing.Lease`. Paths are canonicalized and confined to the workspace; holder defaults to the
  session by convention.
- Tool classification: `edit_lines` is a `tierFiles` tool and names its path; `edit_go` is
  `tierWorkspace` like `lsp_rename` because a rename can write sibling files the arguments do not
  name. Both are `repo.write` capabilities. `edit_lines` is allowed in nested eval callbacks
  (`edit_go` is not, because its targets are not the named path).

## Public surface added

- Package `internal/editing` (`Engine`, `Plan`, `FileChange`, `Result`, `Snapshot`, `SnapshotStore`,
  `Lease`, `LeaseStore`, `Doc`, `Splice`, `LineOp`, `Anchor`, `Error` with stable codes).
- Package `internal/editing/goedit` (`Plan`, `Op`, `Source`, `Result`, `Symbol`).
- Runtime methods `(*Engine).AcquireWriteLease/RenewWriteLease/ReleaseWriteLease/WriteLeases`, types
  `WriteLease`, `WriteLeaseRequest`.
- Tools `edit_lines`, `edit_go`; `read_file` parameters `start_line`, `end_line`, `anchors`;
  `write_file`/`replace_in_file` parameter `base_rev`.
- Event kinds `edit.committed`, `edit.conflict` (content-free: path, oldRev, newRev, actor, runId,
  tool, created/deleted; conflict adds code, hint, holder, currentRev).
- Error codes: `invalid`, `unknown_rev`, `stale`, `conflict`, `lease_conflict`, `not_utf8`,
  `too_large`, `concurrent_write_detected`, `syntax`, `type`, `overlap`, `anchor_mismatch`,
  `not_seen`, `not_found`, `exists`, `unknown_symbol`, `ambiguous`, `type_check_degraded`.

## Tests actually run

```
go test -count=1 ./internal/editing/...            ok  (engine 0.5s, goedit 2.7s)
go test -race -count=1 ./internal/editing/... ./internal/toolset/...   ok  (31.6s)
go test -count=1 ./internal/toolset/               ok  (23–25s, legacy guard/apply-patch/replace tests included)
go test -count=1 ./runtime/ -run 'TestAgentLoopEditsLinesAndRejectsAStaleEdit|TestWriteLeaseBlocksAnotherSessionsWriteTool|TestWriteFileRefusesAStaleReadInTheRunRegistry|TestCapabilities'  ok
go test -count=1 ./runtime/                        see note below
go build ./...                                     ok
go vet ./...                                       ok
```

Raw evidence (workspace-level, not version controlled): `bench/evidence/agent-harness-implementation-20261110/raw/editing/`
— `editing-tests.json`, `race-editing-toolset.json`, `runtime-editing-tests.json`, `scenarios.md`.

Ported scenarios: E3 S1, S2, S3, S4, S5, S5b, S6, S7, S7b, S8, plus anchor-hash/seen-line and UTF-8
refusals; E4 A1–A8 plus method rename, struct members and preview. S9 is a corpus measurement, not an
invariant, and is not a Go test.

## Limitations and unverified claims

- Snapshot store is in memory: after a process restart a read snapshot is gone and an edit is refused
  as `unknown_rev` until the file is read again. This is the design's stated behaviour.
- Type validation covers packages whose imports are all standard library. A package importing module
  paths degrades to syntax-only and says so in the tool result (`typeCheck` field, `type_check_degraded`
  diagnostic). Full module-aware type checking (`golang.org/x/tools/go/packages`) was not added: the
  slice must add no module dependency.
- Method/field rename is selector-name based within one package, with a refusal when another type in
  the package declares the same name. Renaming a symbol used from another package is out of scope
  (`lsp_rename` covers cross-package renames).
- `edit_go` operates on the files of one directory's package; it does not follow build tags.
- The cross-process window (a user editor saving concurrently with our write) is closed by the
  post-write rehash only in the sense that it is *reported* (`concurrent_write_detected`); the other
  writer's bytes may already have replaced ours. An OS-level lock would be needed to prevent it.
- Leases are in-memory: they do not coordinate two Core processes on one work tree.
- No dedicated assertion on the `edit.committed` payload: events are published through the same bus as
  every other runtime event and the payload is built in `Engine.Commit`; a subscriber-level test is
  deferred (P3) because the event bus already has coverage and the fields are part of the payload map
  only.
- TS/JS/Python: no AST editing, and none is claimed. Their structured edits stay with the language
  server through `lsp_rename` and the existing LSP tools.
- No database migration was needed or added.

## Decisions

- The engine takes its path guard per commit (`Plan.Host`) rather than per engine: the runtime shares
  one engine (and therefore one snapshot and lease store) between every toolset of every workspace it
  serves, while each toolset keeps its own guard instance. This avoids duplicating the guard and
  avoids a second, weaker path check in `internal/editing`.
- `read_file` anchors are opt-in so existing callers and tests keep their output; the revision is
  still recorded by a plain read, which is what makes the write_file race detectable.
- Validation is syntax-only for plain text edits of `.go` files (a rename in one call and its call
  sites in another must not be blocked), and syntax + types for `edit_go`, whose whole point is a
  semantically correct structural edit.
- Rollback restores the previous bytes through the same guarded descriptor; a concurrent write that
  happens during the rollback itself is not detected, matching the pre-existing `apply_patch` policy.

## Next actions (deferred, not blockers)

- P2: assert `edit.committed`/`edit.conflict` payloads from a subscriber test.
- P2: LSP diagnostics attached to the next tool result after a commit (design §4.2 step 5).
- P3: persist snapshot revisions if session continuation across restarts should support anchored
  edits without a re-read.
