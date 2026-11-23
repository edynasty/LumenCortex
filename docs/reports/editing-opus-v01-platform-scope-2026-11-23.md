# OPUS-V-01: platform scope of existing-file updates after the fail-closed exchange policy

- Finding: OPUS-V-01, P3. Owner: Claude Opus 5.5, who found it during the independent verification of the Sol L1 remediation.
- Base: `b03c91e5db6969f0345a2034ecb172cf826bc792` (`fix/editing-pre01-pre02-sol-l1-remediation-20261123`). Branch: `fix/editing-opus-v01-platform-scope-20261123`.
- Status: `FIXED_BY_IMPLEMENTER / SELF_VERIFIED / PENDING_INDEPENDENT_REVIEW`.
- Scope: tests and reports only. Production code is unchanged: no file publication algorithm, `Engine.stripes`, SQLite, SnapshotStore, receipt or MutationEvidence change. No `os.Rename` overwrite fallback is reintroduced.

## Finding

Sol `4c79bc0` refuses an update when the atomic exchange is unsupported (`publish_file.go:419-423`, `isSwapRenameUnsupportedErr`). On these platforms the exchange primitive always returns `ENOSYS`:

- `renameSwapForPublish` on Windows (`fileid_windows.go:36`). `isSwapRenameUnsupportedErr` maps `ENOSYS`/`EINVAL` to unsupported.
- `renameSwapPlatform` in `publish_other_unix.go`, which covers FreeBSD and every Unix other than darwin and linux.

So on these platforms every update of an existing file returns `unsupported_filesystem`, and nothing is written. Before `4c79bc0` they used the overwrite-style staged rename. This is the intended Option B behavior, but three things still described the old behavior:

1. `remediation_r6_windows_test.go` expected a verified update and a published update prefix.
2. Untagged tests that also build on Windows and FreeBSD expected successful updates.
3. `editing-pre01-pre02-failclosed-2026-11-23.md` said "Updates use staged rename".

CI runs only Ubuntu, so nothing caught it.

No product contract requires native Windows editing:
- Desktop releases are macOS-only (`LumenCortex-Desktop/docs/release-process.md`).
- Windows NTFS and native FreeBSD acceptance are deferred by the Owner (`TODO.md` `PLATFORM-WIN-NTFS-001`, 2026-11-23).

This is therefore not a release blocker.

## Platform matrix at this branch

Source-derived. Only the "Native evidence" column reports execution.

| Platform | Create new file | Update existing file | Delete | Native evidence |
|---|---|---|---|---|
| macOS (APFS) | private stage, then `link`, or `renameatx_np(RENAME_EXCL)` | `renameatx_np(RENAME_SWAP)`. Refused: several hard links, metadata or ACL that cannot be reproduced, unsupported exchange | quarantine by `renameatx_np(RENAME_EXCL)` | executed on macOS 27.0.1 arm64 |
| Linux (local filesystem) | private stage, then `link`, or `renameat2(RENAME_NOREPLACE)` | `renameat2(RENAME_EXCHANGE)`, same refusals | quarantine by `renameat2(RENAME_NOREPLACE)` | Docker Desktop linuxkit VM only. Native ext4/xfs/btrfs host not run |
| Windows | private stage, then no-replace `MoveFileExW` | **refused: `unsupported_filesystem`, nothing written** | quarantine by no-replace `MoveFileExW` | **UNVERIFIED**: cross `vet` and `test -c` only |
| FreeBSD and other Unix | private stage, then `link`. Refused if links are unsupported | **refused: `unsupported_filesystem`, nothing written** | **refused** (`renameNoReplacePlatform` is `ENOSYS`). Behavior predates `4c79bc0` | **UNVERIFIED**: cross `vet` and `test -c` only |

## Changes

- `platform_exchange_swap_test.go` (darwin, linux) and `platform_exchange_none_test.go` (all other platforms) declare `platformExchangesExistingFiles`, which matches the build-tagged `renameSwapPlatform`.
- `platform_exchange_test.go`: `requireExistingFileUpdate(t)` skips, with a message naming GOOS and OPUS-V-01, a test whose subject requires replacing an existing file. On darwin and linux it never skips, so their coverage is unchanged.
- `platform_exchange_none_test.go` adds `TestOpusV01ExistingFileUpdateRefusedWithoutExchange`, which uses the platform's real primitive:
  - The update is refused with `unsupported_filesystem` and "nothing was written".
  - `failed_before_publication`: not committed, not dirty.
  - `PublishUnsupported`: no published, verified, wrote or rolled-back flag set, no NewRev or NewSHA256, no snapshot of the refused content.
  - The original keeps the same file identity (`os.SameFile`) and bytes, and no `.lcx-*` entry is left.
  - In a batch, the next file is `not_attempted` and unchanged.
- `remediation_r6_windows_test.go`:
  - The timestamp subtest now asserts the refusal through the same helper.
  - The prefix-not-compensated subtest keeps its purpose with new files, which Windows publishes. The prefix `a.txt` must read exactly `new-a\nWIN-FOREIGN\n`; it previously only had to contain the appended text. `b.txt` must keep the foreign bytes, and `c.txt`, an update, must be `not_attempted` and unchanged.
- `remediation_r3_01_unix_test.go` (also built on FreeBSD): the final existing-file edit expects refusal with `keep.txt` unchanged when the platform has no exchange; otherwise it expects the edit as before.
- **SOL-OV01-02 correction:** at `9ad5f2aa`, there were 23 helper call sites, 19 top-level gated tests and five selected nested cases (R7 update, R4 05b, R201 b3, b5 and stress). These are different counting levels, not additive totals. The nested b3/b5 cases share a call site.
- **SOL-OV01-01 correction:** whole-test gating also suppressed independent lease/refusal/read checks on unsupported platforms. The superseding final-P3 report documents their preservation and the recounted final gate scopes; the original “no assertion weakened” statement did not account for this coverage reduction.

## Verification

All commands used `GOWORK=off GOFLAGS=`. Exit codes and logs are in `bench/evidence/editing-opus-v01-platform-scope-20261123/`.

| Check | Result |
|---|---|
| RED: original `remediation_r6_windows_test.go` (tag removed; the darwin-absent `lastWriteTime` log replaced) with `renameSwapForPublish` returning `ENOSYS` | exit 1. Both subtests fail with `unsupported_filesystem` |
| Full-package no-exchange simulation (historical; corrected by SOL-OV01-02) | **FAIL / exit 1: 152 PASS, 6 FAIL, 24 SKIP** including subtests; 78/4/19 at top level. Four top-level failures are in Darwin/Linux-only test files excluded from Windows/FreeBSD builds. This explains failures; it does not make the complete run GREEN |
| Selected assertions in that historical simulation | **PASS — focused simulation only:** refusal test, both Windows subtests and three R301 subtests. Not native Windows/FreeBSD acceptance and not a separate whole-package PASS |
| Mutant: no-exchange tests run where the update succeeds | refusal test, R301 and the Windows timestamp subtest FAIL (`err=<nil>`) |
| macOS 27.0.1 arm64 APFS, Go 1.26.5: `gofmt -l internal/editing` | exit 0, no files |
| `go test -count=1 -v ./internal/editing` | exit 0. 175 PASS lines (including subtests), 0 FAIL, 0 SKIP. Real `hdiutil` 8 MiB ENOSPC test PASS. The set of test names and outcomes is identical to `b03c91e` |
| `CGO_ENABLED=1 go test -race -count=1 -v ./internal/editing` | exit 0. 175 PASS, 0 FAIL, 0 SKIP |
| `go test -race -count=1 ./internal/editing ./internal/toolset ./internal/agent ./internal/session` | exit 0, all `ok` |
| `go build ./...` and `go vet ./internal/editing` | exit 0 |
| `GOOS={windows,freebsd,linux} GOARCH=amd64 CGO_ENABLED=0 go vet` and `go test -c ./internal/editing` | exit 0. Compilation only, not execution |
| Linux container (`golang:1.23`, linuxkit 7.0.14 aarch64, overlayfs, uid 1000): editing `-v`, then editing `-race` | exit 0. 168 PASS, 0 FAIL, 2 pre-existing SKIP (`hdiutil`, case-sensitive `existing_dir_spelling`). No OPUS-V-01 gate skipped |

The simulation keeps darwin's create and delete primitives. It does not reproduce Windows `MoveFileExW` or the FreeBSD primitives.

## Remaining limitations

- Windows and FreeBSD native execution: **UNVERIFIED**.
- FreeBSD delete is refused because no no-replace rename exists. A native FreeBSD run of the editing suite would fail its delete-dependent tests. This predates `4c79bc0` and is outside OPUS-V-01. It is recorded for the deferred `PLATFORM-WIN-NTFS-001` phase.
- Unchanged: L2, writes through an already-open descriptor after the final check. L3, SIGKILL debris. A link created after the final link-count check and before the exchange.
- Option B governance remains `PENDING_OWNER_APPROVAL`.
