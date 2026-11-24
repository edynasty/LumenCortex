# ADR-0024 R3 remediation evidence (2026-11-19)

Workspace evidence directory (not in any Git repository). Integrity: `SHA256SUMS` (`shasum -a 256 -c SHA256SUMS`).

- **Branch:** `fix/adr-0024-r3-remediation-20261119` in `wt/core-adr-0024-r3-20261119`, created from `2ba79ae4aedd35d597604168894c3d65167e9cde`.
- **Code candidate:** `0155df659341dfa5e92904d461da669f69b3f305`. The 7-character prefix `0155df6` is ambiguous in this repository (a tree object shares it); use at least `0155df65`.
- **Fix commits:** `87a3166711277c0ea311374f0eecaeb03d0d4b7d` (R3-01), `b7830e4c4737bab130cc0bc3638c771c54c97639` (R3-04 notice wording), `0155df659341dfa5e92904d461da669f69b3f305` (R3-03).
- **R3 review evidence** (input to this round): `../adr-0024-r3-review-20261119/`.

## Authoritative logs (record the exact commit they ran against)

| File | What ran | Result |
|---|---|---|
| `logs/verify-at-0155df6.log` | At HEAD `0155df65…` with a clean tree: `go vet` of editing/toolset/agent at the intermediate commits `87a3166` and `b7830e4`; `go test -run '^(TestR304\|TestF02)'` at `b7830e4`; `gofmt -l` on the changed files; `go build ./...` + `go vet ./internal/... ./runtime/...`; targeted `TestR301/TestR303/TestR304/TestF02`; `go test -race -count=1 ./internal/agent ./internal/editing ./internal/toolset`; `go build ./...` + `go vet` for linux/amd64, linux/arm64, windows/amd64, freebsd/amd64; windows/amd64 `go test -c` for editing, toolset and agent | the 11 steps before `go test -c` each record `exit=0`; race `ok` 47.951 s / 5.698 s / 29.869 s. The `go test -c` step records no exit code; the log lists the three `.test.exe` files it produced (20,100,096 / 5,907,968 / 17,033,728 bytes) |
| `logs/red-green-at-commits.log` (`scripts/red-green-at-commits.sh`) | each new regression test against its parent commit's production code (RED) and its own fix commit (GREEN), from `git archive` snapshots | R3-03 RED: 9 failures (8 `TestR303*` + `TestF02MultiToolCallAttributionAndUserResetOnRealSQLite`), GREEN `ok`. R3-01 and R3-04 RED are **build failures** (the tests use symbols the fixes add), GREEN `ok` |
| `logs/post-commit-fs-linux.log` (`scripts/post-commit-fs-linux.sh`) | `git archive` of `0155df65…`: real macOS exFAT image (probe tests in `probes/r3-01/`); Docker `lc-ubuntu-go:24.04` (Go 1.27.1, linux/arm64, uid 1000, no network) running `editing.test` (all) and `toolset.test -run 'Create\|R201\|WriteFile'` with TMPDIR on overlayfs and on tmpfs | exFAT: create returns `unsupported_filesystem`, existing-file edit succeeds, 0 leftover `.lcx-create-*`; Linux: editing and toolset `exit=0` on both TMPDIRs |
| `logs/resume-parity-at-commit.log` | the archived R3 Resume parity probe (`../adr-0024-r3-review-20261119/probes/head/internal/agent/zz_probe_r3_resume_test.go`, unchanged) at `0155df65…` | `PROVENANCE MISMATCH` 0 (was 3 on `2ba79ae`, `../adr-0024-r3-review-20261119/logs/resume-parity-fresh.log`) |

## Behavioural RED logs

- `logs/r3-01-exfat-real.log`: the exFAT probe against `2ba79ae` (create fails with a bare `operation not supported`, empty error code), then against the uncommitted fix tree. The committed-code rerun is `logs/post-commit-fs-linux.log`.
- `logs/r3-04-red.log`: `TestR304*` against `2ba79ae` production code, with `probes/r3-04-red-overlay/zz_red_overlay_test.go` adapting the new helper and signatures. The notice claims the omitted latest step (seq 13) is preserved.

## Pre-commit logs (`logs/pre-commit/`, superseded)

Ran on `2ba79ae` plus the then-uncommitted R3 changes; no content hash was recorded, so they are kept as history only.

- `build-vet-cross-linux.log`: its Docker toolset run is **invalid**. `toolset.test`'s `TestMain` could not build `../lsp/testdata/fakeserver` (source tree not mounted) and exited before running any test, and the script recorded no exit code. The committed rerun mounts the snapshot and records `toolset exit=0`.
- The other files are the RED/GREEN iterations made during the fix.

## Not covered

- Native Windows execution.
- macOS SMB and Windows exFAT behaviour.
- Linux block-device filesystems (ext4, xfs, btrfs).
- The full `./runtime` and `./evalruntime` suites; this round does not change those packages.
