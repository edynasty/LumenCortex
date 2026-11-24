# Final-P3 verification commands and results

All worktree Go commands: `GOWORK=off GOFLAGS=`, host Race `CGO_ENABLED=1`. Baseline export `/tmp/lc-sol-finalp3.seqJnO/base` is `git archive 9ad5f2aa`; candidate is the isolated remediation worktree. Overlays and unchanged host-primitive simulation source are preserved under `probes/`; absolute paths can be relocated without changing source. Temporary holder mutant never entered the candidate tree.

## Coverage and red/green

```sh
go test -overlay BASE_OR_CANDIDATE_OVERLAY -count=1 -json \
  -run '^(TestWriteLeaseRefusesOtherHolder|TestReviewEvictedReadRequiresFreshRevision|TestReviewLeaseAcquisitionWaitsForCommit|TestR201TwoOSProcessesCreateRace)$' ./internal/editing
python3 probes/check_coverage.py logs/base-noexchange.jsonl
python3 probes/check_coverage.py logs/candidate-noexchange-final.jsonl
```

Baseline simulation exited 1 (an atomic-create stress round took an unsupported existing-update path). Original independent parents SKIP; coverage checker exit 1. Final simulation subset exit 0; coverage checker exit 0 and all seven required PASS actions observed. Baseline lease-only run exit 0/SKIP, not a passing lease assertion.

```sh
go test -overlay HOLDER_MUTANT_OVERLAY -count=1 -json \
  -run '^TestWriteLeaseRefusesOtherHolder$' ./internal/editing
```

Baseline holder-mutant: exit 0/SKIP. Final holder-mutant: exit 1, exact wrong-holder assertion fails. Mutation differs from production by one Error.Holder assignment; `probes/holder-mutant.diff` records it.

## Focused refusal and full simulation

```sh
go test -overlay refusal-overlay.json -count=1 -json \
  -run 'TestOpusV01ExistingFileUpdateRefusedWithoutExchange|TestR7WindowsNativeTimestampAndNoCompensation|TestR301|TestWriteLeaseRefusesOtherHolder|TestReviewEvictedReadRequiresFreshRevision|TestReviewLeaseAcquisitionWaitsForCommit|TestR201TwoOSProcessesCreateRace|TestLargeFileAnchorEditIsBounded|TestHashline' ./internal/editing
go test -overlay refusal-overlay.json -count=1 -json ./internal/editing
```

Focused exit 0, 33 PASS / 0 FAIL / 16 SKIP. Complete simulation exit 1, 161 PASS / 6 FAIL / 29 SKIP, top-level 81/4/16. The same four Darwin/Linux-only tests fail; no full-simulation GREEN claim. Both are Darwin host fault injection, not native Windows/FreeBSD acceptance.

## Required macOS checks

```sh
gofmt -l internal/editing
go test -count=1 -json ./internal/editing
go test -race -count=1 -json ./internal/editing
go test -race -count=1 ./internal/editing ./internal/toolset ./internal/agent ./internal/session
```

Each exit 0; gofmt output empty. Editing normal and Race each 189 PASS including subtests (98 top-level), no FAIL/SKIP. Four packages `ok`. JSON is used only to record actual test actions; normal/Race semantics match requested commands. No complete Core suite executed.

## Linux VM

Docker image `golang:1.23` frozen local digest in ENVIRONMENT.txt; uid 1000, HOME=/tmp, GOCACHE=/tmp/gocache, GOPROXY=off, GOFLAGS=-mod=mod, GOWORK=off, read-only mounted module cache. Exact test source copied into export, then into container `/tmp/w` on overlayfs.

```sh
go test -count=1 -v -run 'TestWriteLeaseRefusesOtherHolder|TestReviewEvictedReadRequiresFreshRevision|TestReviewLeaseAcquisitionWaitsForCommit|TestR201TwoOSProcessesCreateRace|TestLargeFileAnchorEditIsBounded|TestHashline' ./internal/editing
CGO_ENABLED=1 go test -race -count=1 -v -run 'TestWriteLeaseRefusesOtherHolder|TestReviewEvictedReadRequiresFreshRevision|TestReviewLeaseAcquisitionWaitsForCommit|TestR201TwoOSProcessesCreateRace|TestLargeFileAnchorEditIsBounded|TestHashline' ./internal/editing
```

Each exit 0, each 39 PASS / 0 FAIL / 0 SKIP including subtests. Linux 7.0.14-linuxkit arm64, Go 1.23.12. Not native ext4 verification.

## Compile-only platform checks

```sh
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c -o SCRATCH/editing-windows.test ./internal/editing
GOOS=freebsd GOARCH=amd64 CGO_ENABLED=0 go test -c -o SCRATCH/editing-freebsd.test ./internal/editing
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go list -f '{{range .TestGoFiles}}{{println .}}{{end}}' ./internal/editing
GOOS=freebsd GOARCH=amd64 CGO_ENABLED=0 go list -f '{{range .TestGoFiles}}{{println .}}{{end}}' ./internal/editing
```

Each exit 0. `platform_exchange_none_test.go` selected; Windows test only in Windows binary; Darwin/Linux-only tests excluded. Native Windows/FreeBSD remains UNVERIFIED.
