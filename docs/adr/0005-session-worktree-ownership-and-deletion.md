# ADR-0005: Session worktree ownership, leases and deletion

- **Status:** Accepted (owner decision D5, 2026-10-15); not yet implemented
- **Date:** 2026-10-15
- **Owners:** project owner
- **Supersedes:** none. Amends design §4.2, §4.3 and §8.2 (adds PRs W1/W2 ahead of M3.1)
- **Superseded by:** none

## Context

The architecture review (`../../../LumenCortex-architecture-audit/design/handoffs/product-architecture-review-2026-10-15.md`) verified four defects against the public `runtime` API at `2c5fe7b`. The probes are in `../../../LumenCortex-architecture-audit/design/verify/product-review-2026-10-15/`.

- **P1:** `DeleteSession` leaves the worktree directory and its `lumencortex/<id>` branch behind.
- **P2:** `RemoveSessionWorktree` deletes the worktree of a running session. Only Desktop guards this, and only for runs in its own process.
- **P4:** a read-only subagent copies its parent's `runtime` binding (`runtime/subagents.go` L84-86). `RemoveSessionWorktree(child)` then deletes the **parent's** worktree, and the parent stays bound to a missing path.
- **P5:** a parent run can end while its child still runs in the parent's worktree. The parent's run lock is then free, so the run lock alone cannot protect a worktree that someone else is using.

## Decision

1. **Owner and borrower.**
   - The `runtime` core key gains `owner` (a session id).
   - `AttachWorktree` writes `owner = session`. A child that shares its parent's worktree writes `owner = parent`; it is a **borrower**.
   - A row without `owner` is the owner's only if its path is `<worktreeRoot>/<session id>`, `git worktree list` registers that path, and the branch matches.
   - Any other row has **uncertain ownership**.
   - Only the owner may remove, apply or discard. A borrower may only unbind.

2. **Worktree lease.**
   - One `runlock` file per owned worktree, with id `wt_<owner id>`.
   - Every run whose workspace is a worktree holds it **shared** from Start to exit. That covers the owner and every borrower.
   - Destructive or applying operations hold it **exclusive**: remove, apply, discard, and deleting the owner.
   - A held lease → `ErrWorktreeBusy`; nothing is written.
   - Lock order: `db.lock` → run lock → lease, all non-blocking.
   - flock semantics are already tested (`internal/runlock`), so the lease works across processes and is released after `kill -9`.

3. **Run lock.** `AttachWorktree`, `RemoveSessionWorktree`, `ApplySessionWorktree` and `DeleteSession` take the session's run lock (`ErrRunHeld`) before the lease. Desktop's in-process guard is deleted.

4. **Deletion**: `DeleteSession(ctx, id, DeleteOptions)`. The zero value is the safe default.

   | Worktree state (owner) | default | `KeepBranch` | `Discard{ExpectHead, ExpectStatusDigest}` |
   |---|---|---|---|
   | none, or borrower | delete the session | same | same |
   | clean, every commit applied (`git cherry` empty) | remove directory and branch, delete the session | same | same |
   | unapplied commits | refuse with `ErrWorktreeUnapplied` (commits listed) | remove the directory, **keep the branch**, delete the session; result and journal name the branch | force remove; delete the branch |
   | uncommitted or untracked changes | refuse with `ErrWorktreeDirty` (files listed) | refuse (a branch cannot hold uncommitted work) | force remove; delete the branch |
   | directory missing | refuse if the branch has unapplied commits; otherwise prune and delete | prune, keep the branch, delete | prune and delete the branch |
   | uncertain ownership | refuse with `ErrWorktreeOwnership` | delete the session, touch nothing on disk, report the path | refuse |

   - **Discard authorization.** `Discard` is a separate, explicit option. Core performs it only if the worktree's HEAD and a digest of its `git status --porcelain` still equal what the host showed the user, so a confirmation never destroys work the user did not see.
   - **Subtree deletion.** Deleting a session deletes its child sessions (`subagent`, later `execution`) in the same operation. Each child obeys this matrix; if any child refuses, nothing is deleted. Today the relation rows cascade and the child sessions are orphaned.

5. **Missing worktree at Start.** A bound path that does not exist → `ErrWorktreeMissing` (path, branch). The host offers to recreate it from the branch, or to detach.

6. **Independence.** Resolution, archival and worktree cleanup stay separate operations. Nothing is cleaned up automatically.

## Invariants

- No Core operation removes a worktree directory or branch that holds uncommitted changes or unapplied commits without `Discard`, and only when the state still matches the expected state.
- No borrower can remove, apply or discard.
- No worktree is removed, applied or discarded while any run, in any process, uses it.

## Alternatives considered

- **Run lock only:** insufficient, because of P5 (a borrower runs after the owner's run has ended).
- **Keep the branch by default:** rejected by the owner; the default refuses, and keeping the branch is an explicit choice.
- **Reference counts in SQLite:** a crash leaves counts stale. Kernel locks release on process death.

## Consequences

- `DeleteSession` changes signature; Desktop W2 is paired.
- The CLI gains `--keep-branch` and `--discard` (the latter needs a confirmation that shows the state).
- Two lock files per worktree session. `runlock.Remove` deletes both on session deletion.

## Persistence / compatibility impact

- No schema change. `owner` is a new field inside the existing `runtime` core metadata key.
- Rows without it are classified by the derivation rule in Decision 1.

## Validation plan (W1 must pass)

Probes P1, P2 and P4 inverted. Then:

- the full deletion matrix, each cell, including missing directory and uncertain ownership;
- a borrower running after the owner ended → owner remove and delete return `ErrWorktreeBusy`;
- a lease held by a second `Engine` and by a subprocess → busy; after that subprocess is killed with `kill -9` → free;
- `Discard` with a stale expected HEAD or status → refused;
- subtree deletion where one child refuses → nothing deleted;
- `ErrWorktreeMissing` at Start.

Then `go test -count=1 ./...`, `go build ./...`, `go vet ./...`.

## Documentation impact

Design §4.2/§4.3/§7 and the §8.2 rows W1/W2 (amended by reference in the design header); `docs/agent-runtime.md` worktree section in W1.
