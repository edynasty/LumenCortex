# ADR-0024 R3 independent review: archived probes and logs (2026-11-19)

Durable copy of the R3 review workspace `/tmp/lc-adr0024-r3/` (sources and logs only; compiled
test binaries, disk images and source snapshots were not copied and can be rebuilt from the commits below).

| Label | Commit | Meaning |
|---|---|---|
| main | `c73d17359f5d9dd16ef88b0de4525862593fddaa` | original baseline |
| prev | `a8b4786` | previously reviewed ADR-0024 head |
| r202 | `1a682261bd1267c271124d578a93c94e5edc8e6e` | R2-02 commit (R2-04 "before") |
| head | `2ba79ae4aedd35d597604168894c3d65167e9cde` | R2 remediation head (production code `7d064d0`) |

- `probes/<label>/...`: throwaway `zz_probe_r3_*_test.go` probes exactly as compiled into each snapshot
  (`git archive <commit>` + probe files). They are review instruments, not repository tests.
  `probes/head/internal/editing/zz_probe_r3_publish_test.go` `TestZZR3MultiProcessPublishRace`
  over-counts legitimate later CAS overwrites as anomalies; its FAIL in `logs/r201-macos.log` is a probe
  defect (REJECTED as a code defect), superseded by `TestZZR3FSMatrix`.
- `scripts/`: runners used for the macOS R2-01 matrix and the agent probes.
- `logs/r201-macos.log`: APFS/HFS+/FAT32/exFAT/8 MiB-ENOSPC matrix (R3-01 exFAT evidence at lines 78-79, 137, 140).
- `logs/r201-linux.log`: Linux ARM64 container (overlayfs, tmpfs, fuseblk bind mount).
- `logs/resume-parity-fresh.log`, `logs/resume-parity-all.log`: SIGKILL cut-point Resume parity (R3-03 evidence: `PROVENANCE MISMATCH`).
- `logs/r202-agent.log`, `logs/r202-giant.log`: oversized newest group, live vs cold Resume (R3-04 evidence).
- `logs/r204-profile.log`: R2-04 before/after measurement (r202 vs head, same probe source).
- `logs/race-editing-toolset.log`: `go test -race` of `internal/editing` and `internal/toolset` at head.
- `logs/cross-vet-*.err`: cross-GOOS `go vet` stderr (`solaris` fails in pre-existing `internal/runlock`, not ADR-0024).
- `checks/`: R2 evidence SHA256 claimed-vs-actual and report identifier extraction.
- `diffs/`: R2-02 / R2-04 commit diffs used during review.
- `scope/r3-review-commission.txt`: the R3 review commission text.

Integrity: `SHA256SUMS` covers every file in this directory except itself.
