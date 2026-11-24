# ADR-0024 Core final integration

Status: `MERGED_AND_VERIFIED_PENDING_INDEPENDENT_ACCEPTANCE`. PR #10 merged on 2026-11-22 at 14:42:43 UTC. The required full-suite gate passed in the single Owner-authorized clean-environment run at the same merge SHA.

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
| Historical `go test -count=1 -timeout=45m ./...` (`GOFLAGS=-json`) | 2026-11-22 14:43:52–14:57:41 | 1 | 42 packages passed, 1 failed, 6 had no tests; 46 named skips |
| Required `go test -count=1 -timeout=45m ./...` (`GOFLAGS` unset) | 2026-11-23 00:41:43–01:00:13 | 0 | **PASS**: 43 packages passed, 0 failed, 6 had no test files; individual skips not observable in non-verbose passing output |
| `go test -race -count=1 -timeout=45m -v ./internal/editing ./internal/toolset ./internal/agent ./internal/session` | 14:57:57-14:59:41 | 0 | 4 packages passed; 2 named skips |
| `go test -race -count=1 -timeout=45m -v ./runtime -run '^(TestT3B_\|TestT4_\|TestT11_)'` | 14:59:45-15:00:09 | 0 | 3 top-level tests passed, no skips |

The historical run failed in `cmd/lcx-go` at `TestDBCommandDispatchesBeforeOpen`. Inherited `GOFLAGS=-json` changed the fixture's nested `go env GOMODCACHE` output to JSON; `TestMain` assigned it to `GOMODCACHE`, and the nested build rejected it as a relative path. The corrected preflight verified `GOFLAGS` was absent, effective `go env GOFLAGS` was empty, and `go env GOMODCACHE` returned a plain absolute path. On the unchanged SHA, `cmd/lcx-go` and the full suite passed. The original failure was caused by test-runner environment contamination, not an observed production regression. No persistent Go configuration was changed. Build, Vet, and the two Race gates were not rerun.

Raw stdout/stderr, exact invocation metadata, package results, and skip reasons are indexed in [the execution summary](verification-summary.md). [SHA256SUMS](SHA256SUMS) covers 60 execution artifacts; its SHA256 is `90aa13efc277ffee66feada9cafb38841791473b13959771bea1f654a158259f`. These local workspace files are outside the Core Git repository. The [merged-SHA GitHub Go workflow](https://github.com/edynasty/LumenCortex/actions/runs/45034301120) also passed on Ubuntu, but it is a separate CI command set, not a replacement for the failed required gate or a native-filesystem acceptance matrix.

The one authorized run took 1,110 seconds, produced empty stderr, and left the detached verification worktree clean. The Owner's `AGENTS.md` diff digest matched before and after. [New results and checksums](../adr-0024-merged-7aeb160-full-suite-recovery-20261123/SHA256SUMS) and the [Gemini 4 Carbon handoff](../adr-0024-merged-7aeb160-full-suite-recovery-20261123/CARBON-HANDOFF.md) are recorded separately. The original 60 execution artifacts remain intact. Independent acceptance has not started.

The 13 historical worktree tips matched the fetched/live branch baselines and were all reachable from the merge with zero unique commits. Preliminary status showed no tracked, untracked or ignored changes in those worktrees. No worktree or branch has been removed. Cleanup remains blocked pending independent acceptance and would require a fresh activity check. Retain the final candidate, evidence and original main worktrees.
