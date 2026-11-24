# ADR-0024 R5 Remediation Evidence (`2026-11-20`)

- **Branch:** `fix/adr-0024-r5-rollback-safety-20261120`
- **Base (R4 HEAD):** `0b3b8e721163890ebb664abb4b5c9fc3ef39f4f1` (R4 production candidate `a58aef27759612c99c441062f8b8049d0c8cb1f4`)
- **R5 Commit 1 (Transaction & Rollback Safety — `R4-02-C`, `R4-02-A`, `R4-02-B`):** `7872bba1f701db0c85417dfb220ff3cbd062b043`
- **R5 Commit 2 (Result Propagation & Per-File State — `R4-02-B`, `R4-02-D`):** `b1fb4324da96d066a360a90ca792d81ac2a37d68`
- **Status:** `R5_REMEDIATION_COMPLETE_PENDING_INDEPENDENT_REVIEW` (implementer self-verified only; never claims independent PASS)

## Evidence Layout

- **Behavioral RED logs** (all executed on clean unmodified `0b3b8e721163890ebb664abb4b5c9fc3ef39f4f1`, `worktree_dirty=0`, using `go test -overlay` with the archived probe files in `probes/`, each recording full command and `exit=1`):
  - `logs/r5-r402a-red.log` (`probes/r5_r402ab_red_probe_test.go`, `probes/overlay_ab.json`) — reproduces `R4-02-A` (`RLIMIT_FSIZE=0` during rollback truncates `b.txt`, swallows `EFBIG`, claims `published_verified`, retains stale `NewRev` and `SnapshotStore`).
  - `logs/r5-r402b-red.log` (`probes/r5_r402ab_red_probe_test.go`, `probes/overlay_ab.json`) — reproduces `R4-02-B` (externally modified created file retains `published_verified` + stale `NewRev`/`SnapshotStore`; fully rolled-back multi-file commit misreported as `partially_published`).
  - `logs/r5-r402c-red.log` (`probes/r5_r402c_red_probe_test.go`, `probes/r5_r402c_engine_overlay.go`, `probes/overlay_c.json`) — reproduces `R4-02-C` (multi-file existing `b.txt` receives external `"FOREIGN\n"` before `Close()` fails with `EIO`; R4 rollback overwrote `b.txt` with `"old-b\n"`, destroying `"FOREIGN\n"`).
  - `logs/r5-r402d-red.log` (`probes/r5_r402d_red_probe_test.go`, `probes/overlay_d.json`) — reproduces `R4-02-D` (both `internal/agent/loop.go:707` and `internal/agent/callback.go:314` overwrite partial-publication JSON with `{"error":"input/output error"}`).
- **Commit-level GREEN logs** (clean working tree `dirty_files=0`, full 40-char commit SHA, explicit `exit=0`):
  - `logs/r5-commit1-green.log` (`7872bba1f701db0c85417dfb220ff3cbd062b043`) — `go vet` on macOS, Windows AMD64, Linux ARM64, plus `-race` tests on `internal/editing`.
  - `logs/r5-commit2-green.log` (`b1fb4324da96d066a360a90ca792d81ac2a37d68`) — `-race` tests on `TestR5|TestR402` across `internal/toolset` and `internal/agent`.
- **Final Verification Log** (frozen Commit 2 candidate `b1fb4324da96d066a360a90ca792d81ac2a37d68`, `dirty_files=0`, 17/17 commands `exit=0`, `overall_exit=0`):
  - `logs/verify-r5-final.log` produced by `scripts/run_r5_verify.sh`: `go build ./...`, `go vet ./...`, non-`-race` and `-race` tests on `./internal/editing ./internal/toolset ./internal/agent ./internal/session`, `-race` `TestT3|TestT4` on `./runtime`, 4-target cross-build + cross-vet (`linux/arm64`, `linux/amd64`, `windows/amd64`, `freebsd/amd64`), and `windows/amd64` `go test -c` on all 4 packages.
- **Verification Scripts:**
  - `scripts/run_r5_verify.sh`
  - `scripts/verify_r5_evidence.py`

## Explicitly `UNVERIFIED` Boundaries

- Native Windows runtime execution (`MoveFileExW`, `ByHandleFileInformation`, NTFS/FAT/exFAT/ReFS, Windows file locking, `-race` on Windows).
- macOS SMB, Windows exFAT, Linux block filesystems (`ext4`, `xfs`, `btrfs`), and full `./runtime` / `./evalruntime` long suites (only `TestT3|TestT4` `./runtime` was re-run for R5).
- Residual OS-level check-then-write TOCTOU window against uncooperative external writers during in-place overwrite/restore (`PRE-02` remains `OPEN P2`, alongside `PRE-01`).
- Crash-power-loss durability (`fsync` is intentionally not called, matching baseline `main`).
