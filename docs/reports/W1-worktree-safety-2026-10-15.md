# W1 — Session worktree safety (2026-10-15)

- **Status:** implemented and validated locally; committed, not pushed.
- **Baseline:** Core `38387cf`. Go 1.26.5, git 2.54.0, macOS arm64.
- **Contract:** [ADR-0005](../adr/0005-session-worktree-ownership-and-deletion.md) (owner Decisions 2–4, 2026-10-15); [ADR-0006](../adr/0006-task-context-resolution-and-conversational-execution.md) aligned (execution-child shell scope, retained tasks block parent deletion).
- **Origin:** probes P1, P2, P4, P5 of the product architecture review (`../LumenCortex-architecture-audit/design/handoffs/product-architecture-review-2026-10-15.md`, outside version control).

## Root causes fixed

| Probe | Before | Cause | Now |
|---|---|---|---|
| P1 | `DeleteSession` left the worktree directory and branch orphaned | deletion only removed SQLite rows | deletion runs the ADR-0005 §5 cleanup first; clean worktree → directory, branch, rows and lock files removed |
| P2 | a worktree could be removed under a running session | `RemoveSessionWorktree` took no run lock | remove, apply, attach and delete take the run lock (`ErrRunHeld`) |
| P4 | a read-only subagent could remove its parent's worktree | the binding had no owner; any bound session could remove | `SessionRuntime.Owner`; borrowers only unbind (`ErrWorktreeBorrowed`) |
| P5 | after the parent's run ended, its still-running child was unprotected | the run lock is per session; nothing tracked use of the directory | cross-process worktree lease: shared for runs and host writes, exclusive for remove/discard/apply (`ErrWorktreeBusy`) |

## Guarantees

- **Ownership.** The creator owns the worktree. Subagent bindings record the owner. Older rows are classified by path (`<worktreeRoot>/<owner id>`) and registered branch; anything that does not match is `uncertain` and is never removed or discarded, only retained with `KeepBranch`.
- **Locking.** The run lock is taken first, then the lease. Both use `flock` lock files in the run directory, so `kill -9` frees them. Leases are non-blocking: under sustained shared use an exclusive caller gets `ErrWorktreeBusy` and retries. There is no queueing.
- **Destructive defaults refuse.** Dirty → `ErrWorktreeDirty`; unapplied commits → `ErrWorktreeUnapplied`. The refusal carries a `WorktreeInspection` (dirty paths, commits). `KeepBranch` keeps the branch. `Discard` must repeat the HEAD and status digest from `InspectSessionWorktree`; any change since then → `ErrDiscardStale`.
- **Descendants.** Finished read-only subagents without their own worktree are deleted with the parent in the same transaction. Running children, children owning or holding an uncertain worktree, and independent (non-subagent) children → `ErrSessionHasDependents` with the list. Nothing is touched.
- **Partial failure.** Git, filesystem and SQLite are not one transaction. The order is preflight → disk (LSP/MCP stop, `git worktree remove`/`prune`, branch delete) → one DB transaction → journal and lock-file removal. A crash after the disk step leaves the session bound to a missing worktree. The retry inspects it as missing and converges. A stale `Discard` is refused, so the user must re-authorize against the fresh inspection.
- **Missing directory at Start.** `ErrWorktreeMissing` (`*WorktreeMissingError` includes the branch and whether it still exists) is returned before any row is written.
- **Not a sandbox.** Leases coordinate LumenCortex processes only. `git worktree remove` also deletes ignored files.

## Changed API (public contract)

- `RemoveSessionWorktree(ctx, id, WorktreeCleanup) (WorktreeCleanupResult, error)`, replacing `force bool`.
- `DeleteSession(ctx, id, WorktreeCleanup) (DeleteSessionResult, error)`, replacing `error` only.
- New: `InspectSessionWorktree`, error sentinels `ErrWorktree*`, `ErrDiscardStale`, `ErrSessionHasDependents`, `*WorktreeRefusal`, `*DependentsError`, `*WorktreeMissingError`.
- CLI: `worktree inspect`; `worktree remove` and `session-delete` take `[--keep-branch | --discard <head> <digest>]`. `--force` is removed.
- `session.Store.Delete` deletes several ids in one transaction.
- No schema change.

**Desktop impact:** Desktop pins an older Core and still calls `RemoveSessionWorktree(…, force)` and `DeleteSession(…) error` (`internal/backend/worktree.go`, `app.go`). It breaks at its next Core bump, which is paired work in W2 (dialog built on `InspectSessionWorktree`, removal of Desktop's in-process guard). Desktop was not modified.

## Files

- `runtime/worktree_safety.go` (new): roles, inspection, cleanup matrix, leases, `RemoveSessionWorktree`, `DeleteSession`.
- `runtime/worktree.go`: `Owner`, lock in `AttachWorktree`, missing diagnostics.
- `runtime/supervisor.go`, `runtime/agent.go`, `runtime/engine.go` (shell), `runtime/repository.go` (stage/unstage/revert/commit): shared lease.
- `runtime/worktree_handoff.go`: Apply is owner-only and holds the run lock plus the exclusive lease.
- `runtime/subagents.go`: borrower binding records the owner.
- `internal/repository/{worktree,service,handoff}.go`: status digest, branch head/delete, prune, unbounded `git cherry`.
- `internal/session/reconcile.go`: multi-id transactional delete.
- `cmd/lcx-go/main.go`: CLI.
- `README.md`, `README.zh-CN.md`: CLI docs.
- ADR-0005 and ADR-0006: updated.

## Tests

New tests (`runtime/worktree_safety_test.go`, `runtime/runlock_process_unix_test.go`), all against real temporary git repositories:

- clean delete;
- refusal during a run;
- borrower cannot remove, discard or apply; unbind only;
- borrower lease outlives the owner's run (P5);
- the full matrix: dirty, stale discard, discard, unapplied plus KeepBranch, discard of commits, applied, missing directory with Start refusal, uncertain ownership, exclusive options;
- partial-cleanup retry, including stale re-authorization;
- descendant classification;
- leases across two Engines;
- concurrent shells against deletion;
- cross-process holder killed with `kill -9`, which frees the lease.

Existing tests were migrated to the new signatures. Dirty-worktree cleanups in tests now use an inspected discard. The CLI test checks `--force` rejection and `--discard`.

Results:

- `go test -count=1 ./...` passes (all packages).
- `go build ./...` and `go vet ./...` are clean.
- `go test -race` passes on `runtime`, `internal/session`, `internal/repository` and `cmd/lcx-go`. The W1 tests also passed `-race -count=3`.
- A smoke run of the built `lcx-go` binary covered `session-new --worktree`, a dirty `worktree remove` (refused with the path), `worktree inspect`, and `session-delete --discard` (directory and branch gone, 0 sessions).

## Limits and risks

- Linux/Windows lease behaviour was not run here. Windows uses the existing `runlock` implementation.
- flock is unreliable on network filesystems (design R2).
- Exclusive operations can be starved by continuous shared use (returns `ErrWorktreeBusy`, no fairness).
- The "unapplied" test compares only against the main checkout's current HEAD.
- `gofmt` reports pre-existing drift in about 30 files, including three touched here. They were not restyled.

## Next

W2 (Desktop, paired): implemented on Desktop branch `feat/w2-worktree-integration` (`8537300`), validated against this Core through a local `go.work`; report: Desktop `docs/reports/W2-desktop-worktree-integration-2026-10-15.md`. The Desktop Core pin requires `1d9cc6b` and `156fdff` to be pushed first. Then M3.1 per the review roadmap.

## Follow-up (W2 safety review, 2026-10-15)

Reviewed before the Desktop integration (W2). Separate Core commit on top of `1d9cc6b`.

| Finding | Severity | Evidence | Fix |
|---|---|---|---|
| `git worktree remove` without `--force` deletes ignored files (`.env`, `node_modules/`) of a worktree that `git status --porcelain` reports clean. W1's default cleanup and `KeepBranch` therefore deleted local configuration without listing it. The discard digest did not cover it. | High (silent loss of local config) | Shell experiment: clean status, removal succeeded, `.env` gone | Inspection lists ignored entries (collapsed `dir/`, capped). The default refuses with `ErrWorktreeIgnored`. `KeepBranch`/`Discard` remove them, and the result lists them |
| The discard digest hashed git's listing only, so editing an already-dirty file after inspection did not make the discard stale. This contradicted "destroys only what was shown". | Medium | Code reading; regression test | Digest adds size, mode and mtime of every listed path |
| Concurrent `ApplySessionWorktree` from independent sessions were not serialized on the main checkout. 36 of 40 single-commit pairs failed with raw `index.lock`/sequencer errors. The loser's failure path ran `git cherry-pick --abort`, which can abort the winner's sequence. Plans were computed against a moving target HEAD. | High (lost or aborted applies) | Throwaway probe with 40 rounds (removed) | Exclusive `target_checkout` lease from plan through pick (`ErrWorktreeBusy`). `GitCherryPick` refuses when an operation is already in progress, so its abort only undoes its own pick |

New tests:

- `TestIgnoredFilesStopTheSafeDefault`;
- `TestDiscardIsStaleAfterAListedFileIsEdited`;
- `TestConcurrentAppliesAreSerializedOnTheTarget`: every failure is `ErrWorktreeBusy`, the retry applies, and the checkout ends with exactly 7 commits and clean;
- `TestApplyRefusesWhileTheTargetIsLeased`;
- `TestApplyDoesNotAbortAnotherCherryPick`.

**Residual:**

- Local-runtime runs do not take the target lease; Apply's target-dirty check is their guard.
- The mtime granularity limit in ADR-0005 still applies.
