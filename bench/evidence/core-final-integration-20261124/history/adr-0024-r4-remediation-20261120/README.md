# ADR-0024 R4 Final Remediation Evidence (2026-11-20)

Workspace evidence directory (outside all Git repositories). Integrity is verified via `SHA256SUMS` (`shasum -a 256 -c SHA256SUMS`) and `scripts/verify_r4_evidence.py`.

- **Branch:** `fix/adr-0024-r4-remediation-20261120` in `wt/core-adr-0024-r4-20261120`, branched from `aeb2e5014697fa544ee855c4d6277be4c074fc16` (`fix/adr-0024-r3-remediation-20261119`).
- **Original main baseline:** `c73d17359f5d9dd16ef88b0de4525862593fddaa` (unmerged, untouched).
- **R4 code candidate commit:** `a58aef27759612c99c441062f8b8049d0c8cb1f4`.
- **R4 atomic code commits:**
  1. `4ac972483cb0197179c4a1938a07c2a045cfd886` (`R4-01`: exact `ToolCallID` / `ParentToolCallID` to `ExecutionID` persistence and causal ledger association).
  2. `a58aef27759612c99c441062f8b8049d0c8cb1f4` (`R4-02`: explicit `PublishState` / `CommitState` model and dirty post-publication failure semantics across `internal/editing`, `internal/toolset`, and `internal/agent`).

## Authoritative RED / GREEN and Final Verification Logs

Every log in `logs/` records the exact 40-character commit SHA it ran against and explicit `exit=<rc>` status lines for every command (no unconditional success banners).

| File | Commit SHA | What ran | Result |
|---|---|---|---|
| `logs/r4-01-red.log` | `aeb2e5014697fa544ee855c4d6277be4c074fc16` (zero production diff) | `go test -v -count=1 -run '^TestR401LiveLoopAndColdResumeExactIdentityCases$' ./internal/agent` | Behavioral RED (`exit=1`): Case A (`c1-bad` steals `c2-good` execution), Case B (`c2-top` steals `c1-eval` nested execution), Case C (`c0-read` blamed for nested `write_file`), and legacy no-`ToolCallID` inference all fail at runtime |
| `logs/r4-01-green.log` | `4ac972483cb0197179c4a1938a07c2a045cfd886` (`status=0`) | `go test -race -count=1 -v` on `TestExecutionAdmissionCallIDRoundTripAndConstraints` (`./internal/session`) and `TestR401\|TestR303\|TestR304\|TestF02` (`./internal/agent`) | Both commands `PASS` with `-race` (`exit=0`, `exit=0`) |
| `logs/r4-02-red.log` | `4ac972483cb0197179c4a1938a07c2a045cfd886` (`editing` & `toolset` identical to `aeb2e5014697fa544ee855c4d6277be4c074fc16`) | `go test -count=1 -v -run 'TestR402PostPublishDefectsReproducer' ./internal/editing` (`probes/r4_02_red_repro_test.go`) | Behavioral RED (`exit=1`): post-link error and post-rename error leave `new.txt` published on disk while `CommitReceipt` reports `Committed=false, DirtyFailure=false, Files=[]` and `Result.Files=[]`; multi-file second-file post-publish error skips rollback of `b.txt` |
| `logs/r4-02-green.log` | `a58aef27759612c99c441062f8b8049d0c8cb1f4` (`status=0`) | `go test -race -count=1 -v` on `TestR402\|TestR301` (`./internal/editing`), `TestR402` (`./internal/toolset`), and `TestR402` (`./internal/agent`) | All three commands `PASS` with `-race` (`exit=0`, `exit=0`, `exit=0`) |
| `logs/verify-r4-final.log` | `a58aef27759612c99c441062f8b8049d0c8cb1f4` (`dirty_files=0`) | `scripts/run_final_verify.sh`: `go build ./...`, `go vet ./...`, non-race & `-race` suites on `./internal/agent ./internal/editing ./internal/toolset ./internal/session`, `-race` `TestT3\|TestT4` on `./runtime`, cross-OS `go build ./...` + `go vet` on `linux/arm64`, `linux/amd64`, `windows/amd64`, `freebsd/amd64`, and `windows/amd64` `go test -c` on `editing/toolset/agent/session` | Every step records `exit=0`; `overall_exit=0` |

## Probes and Scripts

- `probes/r4_02_red_repro_test.go`: exact un-hooked `R4-02` behavioral RED reproducer used to produce `logs/r4-02-red.log` against unmodified `internal/editing`.
- `scripts/run_final_verify.sh`: deterministic verification runner recording per-step `exit=<rc>` and `overall_exit=<rc>`.
- `scripts/verify_r4_evidence.py`: automated verifier checking all backticked commit references (`git cat-file -e <sha>^{commit}`), all report SHA-256 rows, and all `SHA256SUMS` manifests (`R3` review, `R3` remediation, `R4` remediation).

## Remaining Unverified Boundaries

- Native Windows runtime execution (`MoveFileExW`, `ByHandleFileInformation`, NTFS/FAT/exFAT/ReFS, SQLite file locking, `-race`).
- macOS SMB and Windows exFAT behavior.
- Linux block-device filesystems (`ext4`, `xfs`, `btrfs`).
- Full `./runtime` and `./evalruntime` package suites (not modified in R4; targeted `TestT3|TestT4` in `./runtime` ran under `-race`).
