# EDITING-PRE-01 & EDITING-PRE-02 macOS Remediation Report

- **Date:** 2026-11-23
- **Status:** `FIXED_AND_SELF_VERIFIED_PENDING_INDEPENDENT_REVIEW` (implementer self-verification complete; independent non-author review required before `PASS` or merge to `main`)
- **Repository / Worktree:** `wt/core-adr-0024-integration-20261122`
- **Branch:** `fix/editing-pre01-pre02-macos-20261123`
- **Base Commit (`main`):** `7aeb1607ffa588727c6fe04873f7b58a1dc16f15`
- **Evidence Bundle:** `bench/evidence/editing-pre01-pre02-macos-20261123/`

## 1. Scope & Problem Statement

Following post-merge acceptance of `ADR-0024` (`7aeb1607ffa588727c6fe04873f7b58a1dc16f15`), two carried-forward `OPEN P2` filesystem mutation hazards remained in `internal/editing` on macOS (the Owner's primary target platform; native Windows NTFS runtime verification is deferred under `PLATFORM-WIN-NTFS-001`):

1. **`EDITING-PRE-01` (Destructive truncate-before-write on existing-file updates):**
   `Engine.writeInPlace` (`internal/editing/publish_file.go`) called `f.Truncate(0)` on the live destination file before allocating and writing `p.next`. When the subsequent write failed under `ENOSPC`, `EDQUOT`, `EFBIG` (`RLIMIT_FSIZE`), or short write, the user's original file was destroyed (`0` bytes or partial prefix bytes).
2. **`EDITING-PRE-02` (Loss or corruption of concurrent non-cooperative external modifications):**
   - *Historical multi-file rollback vector:* Closed earlier in `ADR-0024` R7 (`dcc0589c2b`) by removing automatic multi-file rollback/compensation (`TestR7PrefixExternalAppendBeforeLaterConflict`).
   - *Surviving existing-file update window:* Between reading `p.original` for comparison and mutating the live file in `writeInPlace`, a short external write (e.g., `8 B "FOREIGN\n"`) was overwritten with false `committed / published_verified`, while a longer external write (e.g., `44 B`) left mixed agent+foreign bytes on disk.
   - *Surviving delete window:* Between reading/statting `p.abs` and calling `os.Remove(p.abs)` in `deleteCAS`, a concurrent external append, overwrite, or atomic replacement was permanently unlinked while `Commit` reported `committed / published_verified`.

## 2. Architecture & Fix Summary

### 2.1 Existing-File Staged Atomic Swap (`Engine.updateAtomicSwap`)

- **Private Sibling Pre-Staging (`PRE-01` closure):**
  For `AtomicFileStager` hosts (`*toolset.Set`) and `LocalHost`, updating an existing regular file first stages the complete `p.next` payload into a private sibling file (`.lcx-create-<128-bit-hex>`), preserving the original permission bits (`Chmod(origMode)`), reading back `p.next`, and leaving `FileCommitReceipt.Staged = false` (preserving the create-only `Staged` receipt contract). Any `ENOSPC`, `EFBIG` (`RLIMIT_FSIZE`), `EDQUOT`, or short write aborts via `removeCreatedWhileOpen` before the live destination is ever modified, preserving 100% of original bytes on the original inode with `CommitStatePrePublishFailure`, `DirtyFailure = false`, and zero `.lcx-*` leftovers.
- **Symlink & Multi-Link Preservation:**
  - If `p.abs` is a workspace-confined symlink to an existing regular file (`p.viaSymlink`), `updateAtomicSwap` and `verifyActual` validate workspace/protected-state guards on both the symlink and its resolved target, atomically swapping the target regular file while preserving the symlink entry itself.
  - If the destination regular file has multiple hard links (`Nlink > 1`), `stageAndCleanupCapacityProbe` first proves full block/quota/`RLIMIT_FSIZE` capacity in the target directory and cleans up the probe before updating the shared inode via `writeInPlace`, keeping hard links intact while preventing `ENOSPC`/`EFBIG` truncation.
  - Custom hosts without private sibling staging (`r7PlainHost`) retain `writeInPlace` and its `PublishPartial` receipt semantics.
- **macOS `RENAME_SWAP` + `EVFILT_VNODE` Window Closure (`PRE-02` update closure):**
  - On `darwin` (`publish_darwin.go`), `newVnodeWriteWatch` registers a non-blocking `kqueue` `EVFILT_VNODE` (`NOTE_WRITE | NOTE_EXTEND`) watch on the open original descriptor before reading `p.original`.
  - Immediately prior to swap (after `BeforeAtomicPublish`, `BeforePublish`, and `AfterTruncate` hooks), `updateAtomicSwap` re-verifies `!watch.wrote()`, exact original bytes on `origFile`, and `os.Lstat(targetAbs)` identity/size; any external change aborts cleanly before swapping (`CodeConflict`, `DirtyFailure = false`, external bytes untouched).
  - Publication exchanges `tmpPath` and `targetAbs` atomically via `unix.RenameatxNp(..., unix.RENAME_SWAP)` on macOS and `unix.Renameat2(..., unix.RENAME_EXCHANGE)` on Linux (falling back to `os.Rename` only on filesystems returning `ENOTSUP`/`EINVAL`/`ENOSYS`).
  - While holding `origFile` and the displaced `tmpPath` across post-swap checks, if `watch.wrote()` or re-reading `origFile` detects an overlapping write on an external process's pre-opened descriptor, `updateAtomicSwap` atomically swaps the intact external inode back into `targetAbs` via `RENAME_SWAP`, removes the staged inode, and returns `CodeConcurrentWrite` (`DirtyFailure = true`, `PublishExternalModification`) — eliminating both silent overwrite of short external writes and mixed bytes on long external writes.

### 2.2 Quarantine-Verify-Restore Delete (`Engine.deleteCAS`)

- Instead of unlinking `p.abs` directly after reading `p.original`, `deleteCAS` moves `p.abs` to a private sibling `.lcx-del-<128-bit-hex>` via no-replace rename (`RENAME_EXCL` on macOS, `RENAME_NOREPLACE` on Linux, `MoveFileExW(..., 0)` on Windows).
- `deleteCAS` then inspects the quarantined entry (`os.Lstat`, descriptor identity, `!watch.wrote()`, and exact byte comparison against `p.original`):
  - If an external append, same-length overwrite, or atomic rename replacement occurred in the check-to-move window, `quarantineRestoreForDelete` atomically moves the quarantined file back to `p.abs` without replacing any newer entry and returns `CodeConflict` (`PublishExternalModification`), preserving 100% of external bytes.
  - If unchanged, `os.Remove(quarantinePath)` removes the quarantined entry, followed by one final check on the still-open `origFile` / `EVFILT_VNODE` descriptor before `Close()` (re-publishing intact bytes via `restoreUnlinkedExternalBytes` if an external descriptor wrote concurrently with final unlink).

### 2.3 POSIX VFS Causal Boundary Note

Both `updateAtomicSwap` and `deleteCAS` protect every external path replacement and every external descriptor write that occurs up to `Commit` completion/verification. Under standard POSIX VFS semantics, if an external process opens a file descriptor prior to `Commit`, writes zero bytes throughout `Commit`'s entire execution, and writes to that unlinked old descriptor *after* `Commit` has already verified and returned, that post-return write targets the unlinked open inode (`displaced_open_fd_after_verified` in `remediation_r2_01_test.go`).

## 3. Changed Files

- `internal/editing/publish_darwin.go` — `renameSwapPlatform` (`RENAME_SWAP`) and `vnodeWriteWatch` (`kqueue` `EVFILT_VNODE`).
- `internal/editing/publish_linux.go` — `renameSwapPlatform` (`RENAME_EXCHANGE`) and `vnodeWriteWatch` stub.
- `internal/editing/publish_other_unix.go` — `renameSwapPlatform` (`ENOSYS`) and `vnodeWriteWatch` stub.
- `internal/editing/fileid_unix.go` — `nlink`/`linkCount()`, `renameSwapForPublish`, `renameNoReplaceForQuarantine`, and quarantine helpers.
- `internal/editing/fileid_windows.go` — Windows parity helpers (`nlink`, `matchesOwnedCleanup`, `isCleanPrePublishErr`, quarantine delegators) keeping `GOOS=windows` build/vet/test-compile clean.
- `internal/editing/engine.go` — `BeforeDeleteUnlink` fault hook and `viaSymlink` tracking in `prepared`.
- `internal/editing/publish_file.go` — `updateAtomicSwap`, `stageAndCleanupCapacityProbe`, quarantine-verified `deleteCAS`, and `viaSymlink` target check in `verifyActual`.
- `internal/editing/r7_fault_matrix_unix_test.go` — bind `TestR7InPlaceRLimitFaults` to `r7PlainHost` so fallback partial-write reporting remains covered alongside staged atomic swap.
- `internal/editing/remediation_r2_01_test.go` — exclude `pre02MixedReportedDirty` and `pre02ExternalWriteOverwritten` from valid stress outcomes and assert clean pre-swap refusal on `outcome_b4`.
- `internal/editing/remediation_pre01_pre02_unix_test.go` — dedicated Red/Green regression suite for `EDITING-PRE-01` and `EDITING-PRE-02`.

## 4. Verification Matrix & Honest Environment Disclosure

| Step / Gate | Command | Result | Evidence File |
|---|---|---|---|
| 1. Authentic RED reproduction (on unpatched `7aeb1607`) | `go test -v -count=1 ./internal/editing -run '^(TestPRE01\|TestPRE02)'` | **FAIL (expected RED)**: `EFBIG` `zero_0` & `short_3` destroyed `a.txt`; `PRE-02` `8B` wiped with false `published_verified`, `44B` corrupted into mixed bytes; `deleteCAS` lost `8/80` external appends | `01-red-editing.txt` |
| 2. Targeted `PRE-01` / `PRE-02` GREEN run | `go test -v -count=1 ./internal/editing -run '^(TestPRE01\|TestPRE02)'` | **PASS**: `7 PASS`, `1 SKIP`, `0 FAIL` | `02-green-pre01-pre02.txt` |
| 3. Full `internal/editing` normal suite (macOS ARM64) | `go test -v -count=1 ./internal/editing` | **PASS**: `87 PASS`, `1 SKIP`, `0 FAIL` | `03-full-editing-normal.txt` |
| 4. Full `internal/editing` Race suite (macOS ARM64) | `CGO_ENABLED=1 go test -race -v -count=1 ./internal/editing` | **PASS**: `87 PASS`, `1 SKIP`, `0 FAIL` | `04-full-editing-race.txt` |
| 5. `gofmt`, `go vet`, & Cross-OS `build` + `test -c` (`darwin`, `linux`, `windows`, `freebsd`) | `gofmt -l`, `go build`, `go vet`, `go test -c ./internal/editing` across `darwin/arm64`, `linux/amd64`, `linux/arm64`, `windows/amd64`, `freebsd/amd64` | **PASS**: exit `0` across all 4 OSes (`ALL_GATES_EXIT_0`) | `05-gofmt-vet-crossbuild.txt` |

### Environment Boundaries Disclosed

1. **Seatbelt `hdiutil` virtual block device restriction (`1 SKIP`):**
   Inside the local agent Seatbelt sandbox, `hdiutil create` returns `create failed - 设备未配置` (`ENXIO` on IOKit disk-image attach), causing `TestPRE01RealDiskImageENOSPC` to skip cleanly while `TestPRE01SimulatedBlockQuotaENOSPC` (`0 B` and `320 KiB` partial staging `ENOSPC` on `14,000 B` existing files) and `TestPRE01RLimitEFBIGPreservesExistingFile` (real kernel `RLIMIT_FSIZE` `EFBIG`) execute natively and pass. An unrestricted macOS host shell or independent reviewer should run:
   ```bash
   go test -v -count=1 ./internal/editing -run '^TestPRE01RealDiskImageENOSPC$'
   ```
2. **Sandbox `GOMODCACHE` & root-walk boundary:**
   Only `golang.org/x/sys` is vendored in local `$GOROOT`; packages importing external third-party modules (`go-sqlite3`, `yaml.v3`, `jsonschema-go`) and `internal/safefs` tests opening `/` under Seatbelt were not modified and should be included in the unrestricted host/independent-reviewer `go test -race ./internal/editing ./internal/toolset ./internal/agent ./internal/session` run before merging to `main`.
3. **Cross-platform runtime status:**
   Per Owner priority (`PLATFORM-WIN-NTFS-001`), Windows/Linux/FreeBSD were verified via cross-compilation, `go test -c`, and `go vet` only; native runtime execution outside macOS remains deferred.
