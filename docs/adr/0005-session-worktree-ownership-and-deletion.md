# ADR-0005: Session worktree ownership, leases and deletion

- **Status:** Accepted (owner decisions D5 and Decision 3, 2026-10-15); implemented by W1, amended by the W1 follow-up (ignored files, discard digest, target lease for Apply)
- **Date:** 2026-10-15
- **Owners:** project owner
- **Supersedes:** none. Amends design §4.2, §4.3, §7 and §8.2 (adds W1/W2 ahead of M3.1)
- **Superseded by:** none

## Context

The architecture review (`../../../LumenCortex-architecture-audit/design/handoffs/product-architecture-review-2026-10-15.md`) verified the following against the public `runtime` API at `2c5fe7b`. The probes are in `../../../LumenCortex-architecture-audit/design/verify/product-review-2026-10-15/`.

- **P1:** `DeleteSession` leaves the worktree directory and its `lumencortex/<id>` branch behind.
- **P2:** `RemoveSessionWorktree` deletes the worktree of a running session. Only Desktop guards against this, and only for runs in its own process.
- **P4:** a read-only subagent copies its parent's `runtime` binding. `RemoveSessionWorktree(child)` then deletes the parent's worktree, and the parent stays bound to a missing path.
- **P5:** a parent run ends while its child keeps running in the parent's worktree. The parent's run lock no longer protects that worktree.

## Decision

### 1. Owner, borrower, uncertain

The `runtime` core key gains `owner`. A session's role for its bound worktree is:

- **owner**: `owner` equals the session id. For rows written before W1 (no `owner`), the worktree directory name equals the session id.
- **borrower**: `owner` names another session. For older rows, the directory name is the id of another session bound to the same path.
- **uncertain**: anything else.

Rules by role:

- `AttachWorktree` writes `owner = session`.
- A read-only subagent copies its parent's binding with the parent's owner.
- Only the owner may remove, discard or apply. A borrower may only unbind (`ErrWorktreeBorrowed` for discard and apply).

Ownership is re-checked against Git under the exclusive lease, before anything destructive. If the directory exists, `git worktree list` must register it with the bound branch. Otherwise the worktree is **uncertain**: unregistered, a different branch, or a detached HEAD.

### 2. Worktree lease

- **The lock.** A `runlock` file in the run directory, id `wt_<worktree directory name>`. The directory name is a session id, because every managed worktree is `<worktreeRoot>/<owner id>`.
- **Shared holders**, for the whole operation:
  - every agent run whose workspace is a worktree: owner or borrower, `Start` or `RunAgent`;
  - host operations that write in the worktree: `RunShell`, and the session Git stage/unstage/revert/commit operations.
- **Exclusive holders**: remove, discard, apply, and the deletion of the owner.
- A held lease → `ErrWorktreeBusy`; nothing has been written.
- **Lock order:** `db.lock` → session run lock(s) → lease. Every acquisition is non-blocking, so no deadlock is possible.
- The binding is re-read and the directory is checked **after** the lease is acquired. A holder therefore never acts on a worktree that was removed while it was waiting.

**Lease file lifecycle**

- The file is created on first use.
- It is kept across remove and re-attach.
- It is deleted only after the owner session is deleted. A later process that opens a stale descriptor finds no directory and no session, and fails.
- flock locks belong to the open file description and are released on process death, `kill -9` included (`internal/runlock`).

**Target checkout lease.** Apply writes the main checkout, which no worktree lease covers. Apply therefore also takes an exclusive `runlock` with id `target_checkout`, from before the plan until after the cherry-pick. A held lease → `ErrWorktreeBusy` before any git command runs.

Without it, concurrent Applies from independent sessions collided on git's `index.lock` or sequencer: 36 of 40 probe pairs failed with raw git errors. The loser's failure path ran `git cherry-pick --abort`, which can abort the winner's sequence.

`GitCherryPick` refuses when the checkout already has a cherry-pick, revert, merge or rebase in progress, so its abort only ever undoes a pick it started.

Runs in the main workspace (local runtime) do not take this lease. Apply's target-dirty check is their only guard.

### 3. Run lock

`AttachWorktree`, `RemoveSessionWorktree`, `ApplySessionWorktree` and `DeleteSession` take the session's run lock (`ErrRunHeld`) before the lease.

### 4. Inspection and authorization

`InspectSessionWorktree(ctx, id)` is a read without locks. It returns:

- role and owner; path and branch;
- whether the directory exists and is registered;
- HEAD: the worktree's HEAD, or the branch tip when the directory is missing;
- the dirty files, including untracked ones;
- the ignored entries that removing the directory would delete, with a fully ignored directory as one `dir/` entry (`git status --ignored=traditional --untracked-files=normal`);
- a status digest: SHA-256 over both git listings plus the size, mode and mtime of every listed path. Editing a file that was already listed therefore also makes a discard stale;
- the unapplied commits.

**Unapplied commits** are `git cherry <main checkout HEAD> <worktree HEAD>`: commits whose patch is not in the main checkout's current HEAD. This is deliberately conservative: work applied only to another branch counts as unapplied.

**Options**: `WorktreeCleanup{KeepBranch bool; Discard *WorktreeDiscard{ExpectHead, ExpectStatusDigest}}`.

- `KeepBranch` and `Discard` are mutually exclusive.
- `Discard` is performed only if a fresh inspection under the exclusive lease returns the same HEAD and digest; otherwise → `ErrDiscardStale`. A confirmation therefore never destroys work the user was not shown.

### 5. Cleanup matrix

Applies to `RemoveSessionWorktree` (the session stays, unbound) and to `DeleteSession` (the session is deleted).

| Owner's worktree | default | `KeepBranch` | `Discard` (matching) |
|---|---|---|---|
| clean, no unapplied commits, no ignored entries | remove directory, delete branch | remove directory, keep branch | remove directory, delete branch |
| unapplied commits | `ErrWorktreeUnapplied` | remove directory, keep branch | force remove, delete branch |
| ignored entries (`.env`, `node_modules/`, build output) | `ErrWorktreeIgnored` | remove directory with them, keep branch | force remove, delete branch |
| dirty (tracked or untracked) | `ErrWorktreeDirty` | `ErrWorktreeDirty` | force remove, delete branch |
| directory missing, branch absent | prune | prune | prune |
| directory missing, branch has unapplied commits | `ErrWorktreeUnapplied` | prune, keep branch | prune, delete branch |
| directory missing, branch applied | prune, delete branch | prune, keep branch | prune, delete branch |
| uncertain ownership | `ErrWorktreeOwnership` | touch nothing on disk; unbind/delete; report the retained path | `ErrWorktreeOwnership` |
| borrower | unbind only | unbind only | `ErrWorktreeBorrowed` |

Refusals are typed errors (`*WorktreeRefusal`) that carry the inspection (files, ignored entries, commits, HEAD, digest), so a host can show what to do next. When several rows apply, the first refusal in the order dirty → unapplied → ignored is returned, and the inspection lists all of them. Results list the ignored entries that were deleted.

### 6. Child sessions (owner Decision 3)

There is no blanket cascade. Deleting a session classifies its descendants (`session_relations`):

- **Temporary**: deleted together with the session. A descendant is temporary when its relation kind is `subagent`, its run lock is free, and it has no worktree or only borrows one. Read-only subagents cannot hold independent work.
- **Every other descendant**: the deletion is refused with `*DependentsError` (`ErrSessionHasDependents`), listing each session and its reason: `running`, `owns_worktree`, `uncertain_worktree`, or `independent` (another relation kind, such as a future execution task). Nothing has been written.

The user handles a dependent explicitly: finish it, or clean it up or delete it on its own. Retained execution tasks keep their own rows and provenance.

### 7. Execution and partial failure

SQLite, Git and the filesystem cannot be updated atomically, and deletion does not claim to be all-or-nothing. It runs in phases:

1. **Locks:** the run locks of the session and its temporary descendants, then the exclusive lease. Any conflict → error; nothing written.
2. **Preflight:** a fresh inspection, the descendant classification, the matrix and the discard check. Any refusal → typed error; nothing written.
3. **Disk:**
   - stop the LSP and MCP processes for the path;
   - `git worktree remove` (`--force` only for a matching `Discard`), or `git worktree prune` when the directory is missing;
   - delete the branch when the plan says so.
4. **Database:** one transaction that deletes the session and its temporary descendants (`DeleteSession`), or removes the `runtime` key (`RemoveSessionWorktree`).
5. Journal entry; lock files removed (`DeleteSession`); locks released.

**Recovery.** Disk work precedes the database write, so a failure or crash in step 3 or 4 leaves the session row in place, bound to a worktree that is missing or partly cleaned. `Start` reports `ErrWorktreeMissing`. Retrying the same operation converges through the "directory missing" rows of the matrix.

A `Discard` interrupted after the directory was removed needs a fresh authorization: the status digest of a missing directory differs from the one the user confirmed. The retry is shown the branch and its commits before anything else is destroyed.

### 8. Missing worktree

- If a bound directory does not exist, `Start`, `RunAgent` and other uses of the session workspace return `*WorktreeMissingError` (`ErrWorktreeMissing`). The error carries the path, the branch and whether the branch still exists.
- The host can detach with `RemoveSessionWorktree`, whose matrix protects a branch that has unapplied commits.
- Recreating the directory from the branch is not part of W1.

## Invariants

- No Core operation removes a worktree directory or branch that holds uncommitted changes, unapplied commits or ignored files, except under an explicit `KeepBranch` (which keeps the commits on the branch and is refused for uncommitted changes) or a `Discard` whose expected state matches the state found under the exclusive lease. The safe default never destroys anything Git cannot restore.
- At most one Apply writes the main checkout at a time, across sessions and processes.
- No borrower or uncertain binding causes removal of a worktree directory or branch.
- No worktree is removed, discarded or applied while any agent run or worktree-writing host operation, in any process, holds its lease.
- Deletion never removes a running, worktree-owning or non-subagent descendant.

## Limits

- `git worktree remove` deletes **ignored** files without `--force`, although `git status` reports the tree clean. Core therefore lists them and refuses the default. `KeepBranch` and `Discard` remove them, and the list is capped at `MaxStatusBytes` (`IgnoredTruncated`).
- The digest uses mtime and size; an edit within the filesystem's mtime granularity that keeps the size is not detected.
- A worktree is **not an operating-system sandbox**. The lease coordinates LumenCortex processes; it cannot stop an external process or a user editing the directory. Trusted commands such as `shell` and evaluator checks run with the user's filesystem and network access.
- The "unapplied" test compares against the main checkout's current HEAD only.
- flock on network filesystems is unreliable (design R2).

## Alternatives considered

- **Run lock only:** insufficient, because a borrower can run after the owner's run has ended (P5).
- **Keep the branch by default:** rejected by the owner. Retention is the explicit `KeepBranch`.
- **Cascade deletion to all descendants:** rejected by the owner (Decision 3).
- **Reference counts in SQLite:** a crash leaves the counts stale, whereas kernel locks are released on process death.

## Consequences

- `RemoveSessionWorktree` and `DeleteSession` take `WorktreeCleanup` and return results. Desktop W2 is paired: a delete/remove dialog built on `InspectSessionWorktree`, and removal of Desktop's in-process guard.
- New CLI commands: `lcx-go worktree inspect`, `worktree remove [--keep-branch | --discard HEAD DIGEST]` and `session-delete` with the same flags.
- No schema change. `owner` is a field inside the existing `runtime` core key, and older rows are classified by the rule in §1.

## Validation (W1)

Tests use real temporary Git repositories:

- P1, P2 and P4 inverted;
- every cell of the matrix, including the missing directory, uncertain ownership and a stale discard;
- a borrower holding the lease after the owner's run ended → busy;
- a lease held by a second `Engine` and by a separate process → busy, then free after `kill -9`;
- descendant classification;
- retry after a partial cleanup;
- `ErrWorktreeMissing` at Start.

Then `go test -count=1 ./...`, `go build ./...` and `go vet ./...`.
