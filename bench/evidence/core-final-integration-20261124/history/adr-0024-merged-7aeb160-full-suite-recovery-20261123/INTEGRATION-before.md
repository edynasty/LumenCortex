# ADR-0024 Core final integration

Status: `INTEGRATION_BLOCKED` at the post-merge regression gate. Merge recorded 2026-11-22 14:42:43 UTC; the merge remains on remote main.

## Git and authorization

- Core: `edynasty/LumenCortex`; [PR #10](https://github.com/edynasty/LumenCortex/pull/10), merged by GitHub with a two-parent merge commit. Live remote main: `7aeb1607ffa588727c6fe04873f7b58a1dc16f15`.
- Original main and first parent: `c73d17359f5d9dd16ef88b0de4525862593fddaa`. Frozen candidate: `d0f114be20ab578388e2333f2c6c71392f509de7` (59 linear commits over the original main, 48 changed production Go files). Original evidence: `83993065af7362ab27ab3c226d2a8b113e28cab8`, a direct child of the candidate. Published evidence head and second parent: `daa5cb8edb7e729d017a0f1588c4bc97d387f005` (normal non-force push). Only five documentation files were added after the production candidate.
- Merge tree `15ad21e5e6128e6a5f96c5621da2f40bc8115e58` equals the evidence head and the preflight merge tree. No Desktop or submodule pointer changes, module/migration diffs, or unresolved merge markers outside archived evidence. Public GitHub reported main unprotected with no active rules; the merge did not use an admin override or delete the evidence branch.
- Original Core `main` worktree remains at the original SHA, with its sole unstaged `M AGENTS.md`. Its before/after file digest is `84bb06cd73eb37997966e2d2e3cb8e8b181b27f97ff367d209f327bee818d1a0`. No checkout, stash or reset was used there. Pre-existing unrelated worktree and Desktop changes were left untouched.

## Architecture and open risks

- [Owner approval](../../../wt/core-adr-0024-merged-verification-20261122/docs/adr/governance/0024-owner-approval-2026-11-22.md) records the exact rc3.8 draft 2 bytes, SHA256 `4a56762c9b74b5ad2065082f8dc12ad9fd12ca93ab70b56ee510461b0971da9b`. The approved document is retained unchanged. Its historical status line is superseded by the separate ledger.
- [Carbon's review](../../../wt/core-adr-0024-merged-verification-20261122/docs/reports/evidence/adr-0024-final-merge/REVIEW-carbon-ov-final.md) is `PASS_WITH_LIMITATIONS` for the frozen candidate, not independent post-merge acceptance. SHA256 `b0670902243c297695a3ccf9183c01869ca9b765188420da017bb5628140e434` matched the local evidence, the published GitHub blob and the merged tree. The complete archived evidence subtree matches `83993065`.
- Fresh hash verification covered all 304 entries across the nine candidate `SHA256SUMS` files (22 refer to superseded blobs and matched the exact manifest commits), 2 final-evidence entries and 26 entries in the nested SOL review manifest checked against its original archive. The workspace R3/R4/R5/R6 manifests were checked separately. Historical evidence was not rewritten.
- The [canonical PRE ledger](../../../wt/core-adr-0024-merged-verification-20261122/docs/reports/evidence/adr-0024-final-merge/README.md#canonical-open-p2-ledger) matches the original R3 probes: PRE-01 is destructive truncate/write failure; PRE-02 is loss of external modifications between comparison and mutation. Both remain `OPEN P2`. Temporary development-main restrictions: backups including untracked data, disk/quota headroom, no non-cooperative concurrent writers, authoritative reread on uncertain or dirty state, and no blind replay of a partially committed batch.
- Historical P3 limitations and OV-CARBON-01/02 remain, as does the accepted R3-01 macOS exFAT limitation. Native Windows/Linux/FreeBSD are UNVERIFIED for acceptance. No production release, GA, stable certification, unrestricted irreplaceable-file use, or TB2 authorization.

## Exact-tree regression and cleanup

Regression ran on `wt/core-adr-0024-merged-verification-20261122`, clean and detached at the remote merge SHA before and after. Toolchain: Go 1.26.5, macOS arm64, `CGO_ENABLED=1`; modules verified and SQLite CGO files were selected.

| Gate | UTC start/end | Exit | Result |
|---|---|---:|---|
| `go build ./...` | 14:43:30-14:43:41 | 0 | Passed |
| `go vet ./...` | 14:43:45-14:43:48 | 0 | Passed |
| `go test -count=1 -timeout=45m ./...` | 14:43:52-14:57:41 | 1 | 42 packages passed, 1 failed, 6 had no tests; 46 named skips |
| `go test -race -count=1 -timeout=45m -v ./internal/editing ./internal/toolset ./internal/agent ./internal/session` | 14:57:57-14:59:41 | 0 | 4 packages passed; 2 named skips |
| `go test -race -count=1 -timeout=45m -v ./runtime -run '^(TestT3B_\|TestT4_\|TestT11_)'` | 14:59:45-15:00:09 | 0 | 3 top-level tests passed, no skips |

The ordinary suite failed in `cmd/lcx-go` at `TestDBCommandDispatchesBeforeOpen`. My logging configuration (`GOFLAGS=-json`) changed the unchanged fixture's subprocess `go env GOMODCACHE` from plain text to JSON; its `TestMain` assigns that output to `GOMODCACHE`, and the nested build rejects it as a relative path. This is a verification-environment failure, not an observed integration-induced production failure. The original run is preserved without retry. The race gates used command-line `-v`, with `GOFLAGS` unset, to record skips without altering subprocess Go behavior.

Raw stdout/stderr, exact invocation metadata, package results, and skip reasons are indexed in [the execution summary](verification-summary.md). [SHA256SUMS](SHA256SUMS) covers 60 execution artifacts; its SHA256 is `90aa13efc277ffee66feada9cafb38841791473b13959771bea1f654a158259f`. These local workspace files are outside the Core Git repository. The [merged-SHA GitHub Go workflow](https://github.com/edynasty/LumenCortex/actions/runs/45034301120) also passed on Ubuntu, but it is a separate CI command set, not a replacement for the failed required gate or a native-filesystem acceptance matrix.

Post-merge independent acceptance and the separate stabilization handoff have not started because the required regression gate failed. Resume only with separately authorized verification; preserve this run and keep its failure distinct. For a future verification run, do not export an instrumenting `GOFLAGS` into the test subprocesses.

The 13 historical worktree tips matched the fetched/live branch baselines and were all reachable from the merge with zero unique commits. Preliminary status showed no tracked, untracked or ignored changes in those worktrees. No worktree or branch has been removed. Cleanup is blocked by the failed regression and would also require a fresh activity check. Retain the final candidate, evidence and original main worktrees.
