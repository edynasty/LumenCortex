# SOL-PRE-L1 independent verification — Claude Opus 5.5

- Date: 2026-11-23. Mode: read-only. No tracked file, branch, commit, push, merge or worktree change.
- Reviewer: Claude Opus 5.5. Author of the earlier Opus implementation `71cce56`; independent of Sol's remediation only.
- Reviewed: `fix/editing-pre01-pre02-sol-l1-remediation-20261123` at `b03c91e5db6969f0345a2034ecb172cf826bc792`, range `71cce56acb06b457dad2a4fec3a2c3db56537fd6..b03c91e5db6969f0345a2034ecb172cf826bc792`.
- This directory is local evidence outside the Core Git repository and is not pushed.

## Verdict

**PASS_WITH_LIMITATIONS.** SOL-PRE-L1-01: **VERIFIED_FIXED**. SOL-PRE-L1-02: **VERIFIED_FIXED**. One new P3 finding (OPUS-V-01, non-blocking for macOS/Linux) and the existing L2/L3 limitations remain open.

## Baseline

| Check | Result |
|---|---|
| Local HEAD / branch | `b03c91e…`, `fix/editing-pre01-pre02-sol-l1-remediation-20261123`, 0 dirty paths |
| Remote | `origin` Sol `b03c91e…`, Opus `71cce56…`, main `7aeb160…`; Carbon branch absent remotely (local `aa9281f…`) |
| Ancestry | `71cce56 → 315dc74 → 4c79bc0 → b03c91e`, linear |
| Protected | Opus and Carbon worktrees clean and unchanged; Owner `AGENTS.md` SHA256 `84bb06cd…d1a0` |
| Exported tree | `git archive b03c91e` identical to the tested tree (`diff -r`) |
| Sol evidence | `SHA256SUMS` = `23cdabe9…8c21c`, 17/17 OK; test input hash equals committed test file (`47e7e2c2…`) |
| Opus evidence | `SHA256SUMS` = `8192f238…121e`, 17/17 OK, unchanged |

## Diff assessment

Production: `publish_file.go` (+31/−75 net with test-only moves), `fileid_unix.go`/`fileid_windows.go` (constant removed). No change to create, delete quarantine, restore, preflight, receipts, `Engine.stripes`, snapshots or SQLite.

- L1-01 (`publish_file.go:409-417`): `captureFileIdentity(origFile)` (fstat of the opened original) after full staging, Sync, readback, host and fault barriers, the pre-swap byte and kqueue check, and the pathname identity check; `linkCount() > 1` → `abortBeforeSwap(errMultiLinkUpdate)`.
- L1-02 (`publish_file.go:419-423`): `isSwapRenameUnsupportedErr` (Unix: ENOTSUP, EOPNOTSUPP, EINVAL, ENOSYS) → `abortBeforeSwap(CodeUnsupportedFilesystem)`. `os.Rename` fallback, `usedSwap` and `closeHandlesBeforeFallbackRename` removed. No `os.Rename(tmpPath, …)` remains in non-test editing code; the only other `renameSwapForPublish` call is the guarded restoration (`publish_file.go:706`).
- `abortBeforeSwap` removes staging only if descriptor, name, mtime, size and bytes still match; otherwise `PublishUncertain`/dirty. Refusal maps to `PublishUnsupported` through `prepared.fail` (`engine.go:959-962`).

## Independent RED → GREEN

macOS 27.0.1 arm64, APFS, Go 1.26.5, `GOWORK=off GOFLAGS= CGO_ENABLED=1`. Exact `git archive` exports in `/tmp/opus-sol-l1-verify.EaQw2M/{base,final}`.

| Test | `71cce56` | `b03c91e` |
|---|---|---|
| Opus probe: late link by child process at `BeforePublish`, after complete staging | FAIL: swap 1×, `committed`, `published_verified`, NewSHA issued, target ino changed, alias split, refused rev registered in SnapshotStore | PASS: swap 0×, `unsupported_filesystem`, `failed_before_publication`, not dirty, target+alias same ino, nlink 2, original bytes, no `.lcx-*`, no snapshot; follow-up commit after alias removal publishes normally (no lock leak) |
| Opus probe: unsupported errno at swap (ENOTSUP, EOPNOTSUPP, EINVAL, ENOSYS) | FAIL all 4 (post-publication path entered, published_verified) | PASS all 4: swap 1× with complete staging, 0 post-publication entries, target ino/bytes/mode 0640 unchanged, no leftovers |
| Opus probe: EACCES / EIO at swap (not unsupported) | PASS | PASS: not converted to unsupported; EACCES → `failed_before_write`, EIO → `uncertain`+dirty |
| Opus probe: Partial Commit First, ENOTSUP on 2nd of 3 | FAIL | PASS: a verified (not rolled back), b unsupported unchanged, c `not_attempted`, `partially_published` |
| Opus probe: real swap + held-FD write + third-party replacement | PASS | PASS: third party kept at target, external bytes kept at named private path, `concurrent_write_detected` |
| Sol `TestSolPREL1UnsupportedSwapRefused` (overlay on base) | FAIL | PASS |
| Sol `…/link_created_after_staging` | FAIL | PASS |
| Sol `…/batch_preflight_refuses_everything` | PASS (preflight already existed) | PASS |
| Mutant: preflight check disabled in `engine.go:711` | — | FAIL: ordinary first file reported `published_verified` |

Logs: `logs/probe-{base,final}.txt`, `logs/probe-thirdparty-*.txt`, `logs/sol-tests-on-*.txt`, `logs/mutant-nopreflight.txt`; probe source `probes/`.

## Test integrity

- Late link is created by a separate OS process (`os/exec` re-invoking the test binary); synchronization is the hook boundary plus child exit; no sleeps. The barrier first verifies complete staged bytes; the test checks the alias names the original inode.
- ENOTSUP test reads the full stage inside the injected syscall and asserts exactly one call; it cannot pass by failing earlier.
- Whole-plan test orders `0-plain.txt` first, counts writer entries (0), and asserts `not_attempted` via `r7AssertNotAttempted`. The preflight-removal mutant fails it.
- Removed tests: `TestOpusPRE02FallbackRecoveryNeverOverwritesThirdParty` exercised only the deleted fallback; its third-party guarantee on the remaining swap path is covered (`TestZZOpusVerifySwapRecoveryNeverOverwritesThirdParty` passes; the unchanged delete tests keep `RECREATED-BY-THIRD-PARTY`). The removed quota row never reached the swap. No assertion was weakened for a still-existing path.
- Report correction on the historical external-write probe is accurate and does not repeat the unsupported claim.

## Gates (exact `b03c91e` tree, each once)

| Command | Exit | Detail |
|---|---|---|
| `gofmt -l internal/editing` | 0 | no files |
| `go test -count=1 -v ./internal/editing` | 0 | 98 PASS, 0 FAIL, 0 SKIP (native `hdiutil` 8 MiB ENOSPC ran: PASS 12.08 s) |
| `go test -race -count=1 -v ./internal/editing` | 0 | 98 PASS, 0 FAIL, 0 SKIP |
| `go test -race -count=1 ./internal/editing ./internal/toolset ./internal/agent ./internal/session` | 0 | all `ok` |
| `go build ./...` | 0 | |
| `go vet ./...` | 0 | |
| Cross `vet` + `test -c` editing: windows, linux, freebsd (amd64) | 0 | compilation only |
| Linux container (`golang:1.23`, linuxkit 7.0.14 aarch64, uid 1000; bind mount `fuseblk`, copy on `overlayfs`) | 0 | editing normal 99 PASS (includes the Opus probes) 1 SKIP (`hdiutil`); Race editing+toolset `ok`; Sol and Opus probes PASS on overlayfs |

Linux ran in Docker Desktop's VM; native ext4 on a real host was not run. Windows/FreeBSD not executed.

## New finding

**OPUS-V-01 — P3 — owner: Claude Opus 5.5 (discoverer). Introduced by Sol `4c79bc0` by design. Not authorized to fix in this review.**

- Location: `internal/editing/fileid_windows.go:36-42` (`renameSwapForPublish` always returns `ENOSYS`, classified unsupported); `publish_other_unix.go` (same for FreeBSD and other Unix); `publish_file.go:419-423`.
- Effect: on Windows, FreeBSD and other non-darwin/non-linux Unix, every update of an existing file now returns `unsupported_filesystem` with nothing written. At `71cce56` these platforms used the staged overwrite rename. No data loss; this is the intended fail-closed policy, but it is a total loss of the update capability on those platforms and is not stated in Sol's report.
- Stale tests/docs (source-confirmed, native run UNVERIFIED): `remediation_r6_windows_test.go:38-89` (`committed update` expects `published_verified`) and `:91-135` (expects `a.txt` published) would fail on a native Windows run; CI is Ubuntu-only, so nothing catches it. `docs/reports/editing-pre01-pre02-failclosed-2026-11-23.md:77` ("Updates use staged rename") is now wrong.
- Merge impact: none for macOS/Linux. Minimal remediation once authorized: change those Windows expectations to `unsupported_filesystem` with unchanged bytes and document the platform consequence; or an Owner decision to provide a Windows primitive (out of current scope).

## Remaining limitations

- Link created after the final fstat and before/inside the exchange still splits the aliases and reports `published_verified` (probe `LateLinkAfterFinalCheckBoundary`, both trees). Not atomic; documented by Sol.
- L2 OPEN: writes through an already-open descriptor after the final check land in the displaced or unlinked inode.
- L3 OPEN P3: SIGKILL may leave `.lcx-create-*` / `.lcx-del-*` entries.
- Binary artifact export (`replaceBinary` via safefs) is outside this review and does not inherit these claims.
- Option B: design record still `PROPOSED — PENDING OWNER APPROVAL`; no approval record found. Technical PASS ≠ approval.

## Recommendation

Both P2 findings can be marked independently verified for macOS APFS and Linux (container). The Owner decides on (1) formal Option B approval, (2) whether Windows/FreeBSD update refusal is acceptable (OPUS-V-01), and (3) whether to authorize the small OPUS-V-01 test/doc correction before integration. Merge to `main` is not authorized by this review.
