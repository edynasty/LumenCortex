# EDITING-PRE-01 / PRE-02: Fail-Closed Policy (L1 Option B)

- **Date:** 2026-11-23
- **Status:** `FIXED_AND_SELF_VERIFIED_PENDING_INDEPENDENT_REVIEW` (independent review: GPT-6.1 Sol; final technical verification: Gemini 4 Carbon)
- **Design input:** `PRE-01-PRE-02-FINAL-DESIGN-DECISION.md`. That document is marked "PROPOSED — PENDING OWNER APPROVAL". The Owner delivered it to the implementer, and this branch implements its recommended Option B. Merging still requires Owner approval and independent review.
- **Branch:** `fix/editing-pre01-pre02-opus-remediation-20261123`, on top of `2c950cf` (Opus remediation), which is on top of `aa9281f` (Carbon). `main` (`7aeb160`) is unchanged.
- **Evidence:** workspace `bench/evidence/editing-pre01-pre02-opus-20261123/failclosed/` (outside Git)

## 1. Policy implemented

"Unsupported rather than unsafe." An existing text file is only ever replaced by a staged atomic swap: private same-directory stage, then write, then copy metadata, then `fsync`, then readback, then baseline recheck, then `RENAME_SWAP` / `RENAME_EXCHANGE` (plain `rename` where no exchange exists), then post-publication verification. The truncate-and-rewrite path (`writeInPlace`, its capacity probe and `updateInPlacePreservingIdentity`) is **removed**.

| Case | Behavior | Where refused |
|---|---|---|
| Update of a file with more than one hard link | `unsupported_filesystem` / `PublishUnsupported`; the whole plan writes nothing | `prepare` preflight (`engine.go`), re-checked at publication (`updateAtomicSwap`) in case a link appears in between |
| Metadata a swap cannot reproduce (macOS ACL, foreign owner, unsettable xattr) | `unsupported_filesystem`; private stage removed; target inode, bytes and metadata untouched | `updateAtomicSwap`, before the swap |
| Host without private sibling staging | `unsupported_filesystem` | `writeCAS` |
| Delete without a no-replace move (`RENAME_EXCL` / `RENAME_NOREPLACE` unsupported) | `unsupported_filesystem`; file untouched. This replaces the former check-then-`os.Remove` fallback, which had the original PRE-02 delete window. | `deleteCAS` |
| Delete whose path another writer recreates after the move aside | `conflict`; the recreated file is untouched; the original is kept at the quarantine path named in the error | `deleteCAS`, before quarantine removal |
| Deleting one name of a hard-linked file | Supported; the other names keep the data | — |
| New file | Exclusive staged no-replace publication (unchanged) | — |

- **Same-process LC writers:** already serialized per canonical path by `Engine.stripes`. No new lock was added.
- **Error code:** reuses `CodeUnsupportedFilesystem`. Its documentation and agent hint now cover hard-linked and ACL files as well as filesystems.
- **Hook comments:** `PublishFaultHooks.AfterTruncate` and `afterTruncate` remain as the last pre-publication fault boundary of an update; their comments were updated.
- **`PublishPartial`:** stays only so persisted receipts still decode; it is no longer produced.

## 2. RED → GREEN

RED was run against `2c950cf` with this branch's test files overlaid. GREEN is this branch.

| Test | RED (`2c950cf`) | GREEN |
|---|---|---|
| `TestOpusPRE01HardLinkedUpdateRefusedBeforeAnyWrite/batch_preflight_refuses_everything` | FAIL: committed; the in-place write went through | PASS |
| `…/link_created_after_preflight` | FAIL: committed | PASS |
| `…/delete_one_name_keeps_other_links` | PASS (behavior unchanged) | PASS |
| `TestOpusPRE01ACLFileRefusedIntact` (allow and deny-delete ACE) | FAIL: committed in place | PASS |
| `TestR7NonStagingHostRefusesUpdate` | FAIL: in-place write committed | PASS |
| `TestOpusPRE02DeleteWithoutNoReplaceMoveRefused` | FAIL: deleted via check-then-remove | PASS |
| `TestOpusPRE02DeleteRecreatedPathKeepsQuarantinedOriginal` | FAIL: original destroyed, conflict reported only after the fact | PASS |
| `TestPRE01UpdatePreservesPermissionsSymlinksAndHardlinks` (hard-link section, contract changed) | FAIL: committed | PASS |

Probe `TestOpusProbeHardlinkPRE02` (the former L1 silent overwrite): previously `committed=true` with the external bytes lost; now refused, with the external bytes intact.

### Contract-test updates (required by the policy, assertions not weakened)

- **`TestR7InPlaceRLimitFaults`:** exercised the removed truncate path. Replaced by `TestR7NonStagingHostRefusesUpdate`. RLIMIT 0 and 3 on the supported path remain covered by `TestPRE01RLimitEFBIGPreservesExistingFile`.
- **`TestPRE01SimulatedBlockQuotaENOSPC`:** the hard-linked row is removed, because hard-linked updates are now refused before any write (covered above).
- **`TestPRE02DeleteConcurrentExternalWritePreserved/concurrent_stress_80_rounds`:** a private leftover is allowed only when the error names it and it holds exactly the original bytes (`assertOnlyReportedLCXLeftovers`). Any other leftover still fails.
- **toolset `TestR7R2New01*` and agent `TestR7PartialCommitAndDiagnosticsColdResume`:** the mid-batch dirty failure is now a post-publication close fault (`AfterInPlaceWriteClose`, giving `PublishUncertain`) instead of a truncate fault (`PublishPartial`). The tests still check row identity, budget accounting, `partially_published`, the external-append preservation, and cold Resume rows without trusted revisions.

## 3. Gates

Host: macOS arm64, APFS, go1.26.5.

| Command | Result |
|---|---|
| `go test -v -count=1 ./internal/editing` | exit 0, 97 PASS, 0 SKIP (native 8 MiB `hdiutil` ENOSPC ran) |
| `CGO_ENABLED=1 go test -race -v -count=1 ./internal/editing` | exit 0, 97 PASS, 0 SKIP |
| `go test -race -count=1 ./internal/editing ./internal/toolset ./internal/agent ./internal/session` | exit 0 |
| `CGO_ENABLED=1 go test -race -count=30 -run 'TestPRE02\|TestOpusPRE02' ./internal/editing` | exit 0 |
| Cross `build ./...` + `vet` (editing, toolset, agent) + `test -c`: darwin/arm64, linux/amd64, linux/arm64, windows/amd64, freebsd/amd64 | all ok; gofmt clean |
| Linux in Docker (`golang:1.23`, go1.23.12, kernel 7.0.14-linuxkit aarch64, uid 1000, overlayfs and tmpfs): `go test -v ./internal/editing` + `-race ./internal/editing ./internal/toolset` | exit 0 on both; 93 PASS, 1 SKIP (macOS-only `hdiutil`) |

The 4-test difference between macOS and Linux is the darwin-only tests: metadata replication, ACL refusal, the delete final-check race, and the `hdiutil` disk-image test. The full Core suite was not run; per the design document it belongs to the final integration gate.

## 4. Verdict request and remaining limitations

- **PRE-01:** proposed `PRE01_VERIFIED` within the supported scope (macOS APFS, Linux local filesystem). Failed writes preserve the original contents. Required metadata is either preserved and verified (darwin and Linux) or the update is refused. Hard-linked files are refused. No in-place fallback remains.
- **PRE-02:** proposed `PRE02_VERIFIED_WITH_LIMITATIONS`.
  - Same-process LC writers are serialized.
  - External changes detected before publication are refused without writing.
  - External changes detected after the swap are preserved (swap-back, or a named conflict copy).
  - Delete CAS: changes before the move are restored, and a recreated path is preserved.
  - **Limitation L2:** a process holding an already-open descriptor can write after the final check, and those bytes land in the displaced or unlinked inode. They are not protected (kqueue is not a lock, and rename does not revoke descriptors).
- **L3:** private `.lcx-create-*` / `.lcx-del-*` files left by SIGKILL hold recoverable data but are not swept or reported on restart (P3).
- **Windows and FreeBSD (out of scope):** no in-place fallback exists. *Corrected by OPUS-V-01 (`editing-opus-v01-platform-scope-2026-11-23.md`):* since Sol `4c79bc0` these platforms have no exchange primitive, so every update of an existing file is refused with `unsupported_filesystem` and nothing is written; they do not use staged rename. New files are still published (Windows: no-replace `MoveFileExW`; FreeBSD: `link`). FreeBSD delete is refused. Native behavior is unverified.
- **Linux:** acceptance ran in Docker Desktop's Linux VM (linuxkit kernel) on overlayfs and tmpfs. Native ext4 on a real host was not run.
