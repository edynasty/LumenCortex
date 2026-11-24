# ADR-0024 R6 Remediation Evidence Directory (2026-11-20)

- **Repository:** `LumenCortex-Core`
- **Branch:** `fix/adr-0024-r6-safety-20261120` (isolated worktree `/Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-r6-20261120`)
- **R5 Input Baseline:** `fix/adr-0024-r5-rollback-safety-20261120@bfc493634d819bc3be7d30d669bbb2fbef30212e` (frozen R5 code candidate `b1fb4324da96d066a360a90ca792d81ac2a37d68`)
- **R6 Code Commits:**
  - Commit 1 (`internal/editing` — `R5-01` & `R5-06` safe Windows handling/tests): `4341d3bddeb78c0a052416cd1cd4194e7cf09233`
  - Commit 2 (`internal/toolset`, `internal/agent` — `R5-02`..`R5-05` structured JSON bounds, provenance preservation, path safety, SQLite `Resume` & revocation cascade): `aa7b77a63c86d1951d36bfc12234cb17495cd871`
- **Authoritative Architecture Specification:** `LumenCortex-architecture-audit/design/handoffs/context-space-architecture-master.zh-CN.md` (`v0.4-rc3.7`, SHA256 `5c9edaeb6b917d7cf508677036f17ab1911dd6b5f1a78ee25d8e0082f6cbb89f`)

## Contents

1. `independent-r5-probes/` (`ARCHIVE_SHA256SUMS` — 52 archived files):
   - Verbatim read-only archive of the three independent reviewer probe directories (`/tmp/lc-r5-final-audit.OHWLHa/`, `/tmp/lc-r5-statefault-probe.iVTt3s/`, `/tmp/lc-r5-r402d-probe.20509/`) captured before any R6 code changes; original `/tmp` directories remain completely untouched.
2. `logs/`:
   - `r6-r501-red.log`: Deterministic two-process `R5-01` reproduction (`TestIndependentR5AppendAfterRestoreWrite` with `LC_R5_EXPECT_FOREIGN_LOST=false`) comparing `main@c73d1735` (`main_exit=0`, `disk="old-a\nFOREIGN\n"`), `R4@0b3b8e72` (`r4_exit=0`, `disk="old-a\nFOREIGN\n"`), and unmodified `R5@bfc49363` (`dirty_files=0`, `exit=1`, `disk="old-a\n"`, `foreign_lost=true`).
   - `r6-r502-05-red.log`: Deterministic behavioral RED on unmodified `R5@bfc49363` (`dirty_files=0`, `exit=1`) reproducing `R5-02` (`valid_json=false`, `consumer_reached=false`), `R5-03` (`len=26031 > 4096`), `R5-04` (`contains_root=true`), and `R5-05` (`corrupted_sibling=true`, `.-sibling/secret.txt`).
   - `r6-commit1-green.log`: Commit 1 (`4341d3bddeb78c0a052416cd1cd4194e7cf09233`, `dirty_files=0`) `-race` verification of `TestIndependentR5AppendAfterRestoreWrite` (all 4 length regimes), `TestR6RollbackRaceAndFaultMatrix` (all 14 subtests + 12-iteration cross-process stress), `TestR6PartialRLimitFSizeRollback`, and full `./internal/editing` (`exit=0`).
   - `r6-commit2-green.log`: Commit 2 (`aa7b77a63c86d1951d36bfc12234cb17495cd871`, `dirty_files=0`) `-race` verification of `R5-02`..`R5-05` + SQLite cold `Resume` + `RevokeSession` cascade (`exit=0`).
   - `verify-r6-final.log`: Full §14 verification matrix on frozen code candidate `aa7b77a63c86d1951d36bfc12234cb17495cd871` (`dirty_files=0`): `go build ./...`, `go vet ./...`, non-race & `-race` on `editing`/`toolset`/`agent`/`session`, `-race` `TestT3|TestT4` on `./runtime`, cross-platform build/vet for `linux/arm64`, `linux/amd64`, `windows/amd64`, `freebsd/amd64`, and 4 `windows/amd64` `.test.exe` compilations (`exit=0`).
3. `scripts/`:
   - `run_r6_red.sh`: Reproducible Phase A RED + `main`/`R4` comparison runner (uses temporary `go test -overlay`, leaving worktrees clean).
   - `run_r6_verify.sh`: Reproducible Commit 2 + full §14 candidate verification runner.
   - `verify_r6_evidence.py`: Automated verifier checking every backticked commit SHA in docs/ADRs, every SHA256 row in `§11`, all five `SHA256SUMS` manifests (`R3` review, `R3` rem, `R4` rem, `R5` rem, `R6` rem), and `ARCHIVE_SHA256SUMS`.

## Explicit `UNVERIFIED` Platform and Filesystem Boundaries

- **Windows native execution (`R5-06`, `UNVERIFIED — Potential P2`):** Cross-compilation (`GOOS=windows GOARCH=amd64 go build/vet` and `go test -c` producing `editing-r6-win.test.exe`, `toolset-r6-win.test.exe`, `agent-r6-win.test.exe`, `session-r6-win.test.exe`) succeeded, and `internal/editing/remediation_r6_windows_test.go` provides a native-ready test (`go test -v -run TestR6WindowsRollbackTimestampAndIdentity ./internal/editing`), but no native Windows host is available in this environment. Native NTFS/ReFS/exFAT/FAT `CloseHandle` (`IRP_MJ_CLEANUP`) timestamp flush behavior, `MoveFileExW` runtime semantics, and Windows `-race` remain `UNVERIFIED`.
- **Historical SQLite content (`R5-04`):** Path sanitization applies to newly produced tool results and diagnostics; zero historical SQLite rows or migrations were rewritten.
- **Residual OS TOCTOU (`PRE-02`, `OPEN P2`, pre-existing):** `R5-01` (unconditional `Write -> Truncate` append deletion) is eliminated and pre-shrink / post-readback / post-close divergence checks abort into conservative partial-commit (`PublishExternalModification`), but POSIX and Windows lack a kernel-atomic content-CAS write or shrink primitive against an uncooperative writer racing inside the sub-microsecond kernel window immediately before `write(2)` or `ftruncate(2)`.
- **Crash durability:** No file or directory `fsync` is claimed.
