# EDITING-PRE-01 / PRE-02 — Independent Audit and Remediation (Opus)

- **Date:** 2026-11-23
- **Status:** `FIXED_BY_IMPLEMENTER / SELF_VERIFIED / PENDING_INDEPENDENT_REVIEW` (reviewer: Gemini 4 Carbon)
- **Branch:** `fix/editing-pre01-pre02-opus-remediation-20261123`, based on Carbon `aa9281fe1618acd87ffb322559d4ccd643b37e8a`
- **Untouched:** Carbon branch/worktree (`aa9281f`, clean), `main` (`7aeb160`), Carbon evidence `bench/evidence/editing-pre01-pre02-macos-20261123`
- **Evidence:** workspace `bench/evidence/editing-pre01-pre02-opus-20261123/` (logs, probes, `SHA256SUMS`; outside Git)
- **Verdict on Carbon `aa9281f`:** `REVISE`. **Verdict on this branch:** `PASS_WITH_LIMITATIONS`, pending independent review and an Owner decision on L1.

## 1. Carbon assessment

Carbon closes the original single-link reproductions. Single-link updates now stage privately and are exchanged with `renameatx_np(RENAME_SWAP)`. Deletes are quarantined through `RENAME_EXCL`. ENOSPC on an 8 MiB `hdiutil` image, `RLIMIT_FSIZE` 0/3, and 8/25/44-byte external writes no longer destroy data on a real macOS host (`TestPRE01RealDiskImageENOSPC` ran natively and passed here).

The audit confirmed six defects in that implementation:

| ID | Sev | Class | Defect (Carbon `aa9281f`) | Data impact |
|---|---|---|---|---|
| OPUS-F1 | P2 | new regression (PRE-01 fix) | The swap publishes a new inode and drops the group, every xattr (`com.apple.quarantine`, `com.apple.ResourceFork`, user tags), BSD flags (`UF_HIDDEN`), birth time and ACLs. With an `everyone deny delete` ACL, an editable file now fails with `permission denied`. | Silent metadata loss; a quarantine-flag drop is a Gatekeeper downgrade |
| OPUS-F2 | P2 | PRE-02 recovery | When `RENAME_SWAP` is unsupported, the plain-rename fallback "restored" external bytes with `os.Rename(rPath, targetAbs)`. That overwrote a third party's replacement at the target. | Third-party file destroyed |
| OPUS-F3 | P2 | PRE-02 delete | The final check on the unlinked delete target polls kqueue after the read. A write that lands between them is detected, but the stale pre-write bytes are restored. When the path was recreated, `restoreUnlinkedExternalBytes` treated `ErrExist` as success and discarded the bytes. | External write lost, reported as "restored" |
| OPUS-F4 | P2 | PRE-02 update | On the uncertain post-swap path (identity or close error), `removeDisplacedOrig` deleted the displaced original even when it held external writes. | External bytes deleted under `PublishUncertain` |
| OPUS-F5 | P1 (gate) | integration | The requested four-package Race gate failed on Carbon. `toolset` `TestR7R2New01*` and `agent` `TestR7PartialCommitAndDiagnosticsColdResume` rely on an `AfterTruncate` fault to produce a partial publication, which the single-link swap no longer does. The delete stress oracle counted held-FD writes made after the final check as losses (1/80 flake). The same tests pass on `main`. | Gate RED; no data loss |
| L1 | P2 open | PRE-02 residual | Files with more than one link still take the in-place truncate-and-write path, so the original check-to-truncate silent loss still reproduces for them (`TestOpusProbeHardlinkPRE02`). It fails identically on `main` and on this branch; this is not a new regression. | 8-byte external write overwritten while reported `published_verified` |

## 2. Fixes (this branch)

- **F1:** `replicateSwapMetadata` (new files `publish_metadata_{unix,darwin,linux,other}.go`) runs on the empty staged file before the swap.
  - **darwin:** copies and verifies uid/gid, every xattr, BSD flags (except `UF_COMPRESSED`) and birth time (`setattrlist ATTR_CMN_CRTIME`). Kernel-added `com.apple.provenance` is the only extra xattr it tolerates. A file with an ACL (detected via `fgetattrlist ATTR_CMN_EXTENDED_SECURITY`) is refused.
  - **linux:** owner, group and xattrs, including `system.posix_acl_*`. Other metadata it cannot reproduce exactly is refused.
  - **Fallback:** any refusal aborts the stage (the staged file is removed) and falls back to `updateInPlacePreservingIdentity` (capacity probe, then in-place write), the same path multi-link files use. Metadata is never dropped silently. The trade-off is that these files keep the L1 window.
- **F2:** Without the exchange primitive, recovery never renames over the target. `preserveExternalBytes` publishes the bytes with no-replace semantics to the target if it is free, otherwise to `<name>.lc-conflict-<nonce>`, and names that path in the error. The swap restore swaps back only if the target still has the identity this run published; otherwise it keeps the original at its private name and reports it.
- **F3:** The final check on the unlinked delete target re-reads after detection and preserves the bytes through `preserveExternalBytes`. An occupied path yields a conflict copy, never a discard. If a quarantine restore fails, the error names the kept quarantine path.
- **F4:** On the uncertain post-swap path the displaced original is kept, and the error names its path.
- **F5:** The three integration fixtures hard-link the faulted file outside the workspace, which keeps it on the in-place path where a partial publication is still producible. The delete stress oracle now counts only writes completed before the final check (new test hook `BeforeFinalExternalCheck`). Writes after that point fall under limitation L2.
- New test-only hooks: `PublishFaultHooks.BeforeFinalExternalCheck` and `AfterFinalExternalRead`.

## 3. RED → GREEN

New tests: `remediation_pre01_pre02_opus_darwin_test.go` and `remediation_pre01_pre02_opus_unix_test.go`.

| Test | Carbon `aa9281f` (+ hook-only overlay) | This branch |
|---|---|---|
| `TestOpusPRE01SwapReplicatesMetadata` | FAIL: gid 12→20, flags 0x8000→0, birth time changed | PASS |
| `TestOpusPRE01ACLFileKeepsInodeAndACL/user:nobody_allow_read` | FAIL: inode changed | PASS |
| `TestOpusPRE01ACLFileKeepsInodeAndACL/everyone_deny_delete` | FAIL: `permission denied` | PASS |
| `TestOpusPRE02DeleteFinalCheckKeepsWriteAfterRead` | FAIL: stale bytes restored | PASS |
| `TestOpusPRE02FallbackRecoveryNeverOverwritesThirdParty` | FAIL: third-party file overwritten | PASS |
| `TestOpusPRE02DeleteRecoveryPreservesBytesWhenPathRecreated` | FAIL: bytes discarded | PASS |
| `TestOpusPRE02UncertainSwapKeepsDisplacedOriginal` | FAIL: displaced original deleted | PASS |

The RED overlay adds only the two hook call sites to Carbon's `publish_file.go`; the diff is in the evidence bundle.

## 4. Gates (this branch, macOS arm64, go1.26.5)

| Command | Result |
|---|---|
| `go test -v -count=1 ./internal/editing` | exit 0, 94 PASS, 0 SKIP (real 8 MiB `hdiutil` ENOSPC ran) |
| `CGO_ENABLED=1 go test -race -v -count=1 ./internal/editing` | exit 0, 94 PASS |
| `go test -race -count=1 ./internal/editing ./internal/toolset ./internal/agent ./internal/session` | exit 0 (Carbon: FAIL editing/toolset/agent) |
| `go test -race -count=30 -run 'TestPRE02…\|TestOpusPRE02…' ./internal/editing` | exit 0 |
| Cross `build ./...`, `vet`, `test -c` for darwin/arm64, linux/amd64, linux/arm64, windows/amd64, freebsd/amd64 | all ok; gofmt clean |
| **Native Linux** (Docker golang:1.23, linuxkit 7.0.14 arm64, uid 1000, overlayfs and tmpfs): `go test -v` + `-race` | exit 0, 90 PASS, 1 SKIP (the hdiutil test is macOS-only) |

Not executed natively: Windows and FreeBSD remain **UNVERIFIED**. Linux ran in a container VM only; ext4/xfs/btrfs, SELinux labels and real POSIX ACLs were not exercised.

## 5. Probes (outside the repository; evidence `probes/`)

- **Hard links:** with 4 links across 3 directories, one of them outside the workspace, an update keeps the inode and every alias reads the new bytes. Two aliases in one batch are refused (`names the same file … through another spelling`).
- **SIGKILL (independent child process):**
  - Before the swap: target intact, the staged new content is left in `.lcx-create-*`.
  - After the swap: the target holds the new content, the original is left in `.lcx-create-*`.
  - Delete before quarantine: target intact.
  - Delete after unlink: the delete completed.
  - No data is lost, but there is **no sweeper**, so these private files remain until manually removed.

## 6. Remaining limitations (support contract)

- **L1 (P2, Owner decision required):** multi-link files, and files whose metadata cannot be reproduced (darwin ACLs, foreign owner, unsettable xattrs), are updated in place. ENOSPC/EFBIG still refuse before truncation (capacity probe, not a reservation), but the PRE-02 check-to-truncate window is **open** for them. Options:
  - (a) Accept and document it (current behavior; preserves alias and ACL semantics).
  - (b) Fail closed for these files with an explicit `unsupported` result. This blocks editing hard-linked or ACL'd files.
  - kqueue cannot tell our own write from a foreign write on the same vnode, so in-place detection is not a viable third option.
- **L2:** kqueue is a notification, not a lock. A process that already holds a descriptor can write after the final check. For an update the write lands in the displaced inode, which is then removed; for a delete it lands in the unlinked inode. Those bytes are lost. macOS offers no non-cooperative mandatory lock, so universal protection against held-descriptor writers is **not** claimed.
- **L3:** private `.lcx-create-*` and `.lcx-del-*` files left after SIGKILL contain recoverable data but are not swept or reported on restart.
- **L4:** on Windows and FreeBSD, `replicateSwapMetadata` is a no-op, so alternate data streams, ACLs and xattrs are not carried across a replacement. Native behavior is unverified. *Superseded at Sol `4c79bc0`:* these platforms no longer replace existing files; updates are refused with `unsupported_filesystem` (OPUS-V-01).
- Set-uid/set-gid bits are cleared on update. `main`'s in-place write clears them as well.
- `com.apple.provenance` on the new inode is kernel-assigned.
