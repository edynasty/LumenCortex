# Core PRE-01/PRE-02 final integration — 2026-11-24

## Owner scope and acceptance

Option B formally approved in `docs/adr/governance/pre01-pre02-owner-approval-2026-11-24.md`. Development main only; no production release, tag or TB2 authorization.

PRE-01: VERIFIED_WITHIN_SUPPORTED_SCOPE. PRE-02: VERIFIED_WITH_LIMITATIONS. L2 old-FD writes, L3 SIGKILL residue, late hard-link race remain OPEN. Binary export retains separate inherited boundary. Native ext4, Windows and FreeBSD remain UNVERIFIED; Windows/FreeBSD updates and FreeBSD delete unsupported. NFS/FUSE excluded.

## Git publication

- Original main: 7aeb1607ffa588727c6fe04873f7b58a1dc16f15.
- Reviewed candidate: 51316603974d916295bc6f0560d46e7772f304e5.
- Archive/governance commit: 5298e85a56a8c7845a2264904f9240573a049780.
- Evidence module-isolation commit: d3332c60a21f0916065a18b3738a93b6ed84eb4e. Historical Go probes isolated from production package discovery without changing their bytes.
- PR: https://github.com/edynasty/LumenCortex/pull/11, normal merge, no squash/rebase/force.
- Merge: ef10ca7c8e8031e75fdf27f449924b8254f0e2bb; parents 7aeb1607ffa588727c6fe04873f7b58a1dc16f15 and d3332c60a21f0916065a18b3738a93b6ed84eb4e.
- Live origin/main verified at merge SHA; merge tree equals integration head exactly; reviewed candidate ancestry preserved.
- GitHub reports no branch protection or rulesets; merge used standard PR workflow, not admin bypass. Hosted checks were in progress when GitHub allowed normal merge; no hosted CI PASS claimed.

## Evidence preservation

`bench/evidence/core-final-integration-20261124/INDEX.md` and its 525-entry manifest preserve history and provenance. 443 historical files copied and byte-compared, unchanged historical manifests retained: 15 SHA256SUMS plus one ARCHIVE_SHA256SUMS all verify. R7 ORIGINALS paths resolve from private-r6-originals; all 22 pass at correct root, initial wrong-root failure preserved.

Required 26-entry Opus platform, 17-entry Sol L1 and 40-entry final Sol P3 sets retained. Original Carbon/Opus/Sol reports are archived or referenced through immutable commit paths and hashes. Final fresh-context Opus review originally existed only as a native session response: exact assistant text extracted, source path recorded; private reasoning not archived. Temporary raw test outputs and mutant sources retained. Worktrees with ignored databases/history will not be deleted.

SOL-OV01-EVIDENCE-01 remains OPEN P3: Linux raw log did not record UID or actual commands; no missing evidence reconstructed. Stress failure was implementer's baseline focused simulation, not historical full simulation. Historical full simulation remains FAIL/exit 1 (152 PASS/6 FAIL/24 SKIP).

## Merged-SHA regression — FAILED, acceptance stopped

Clean detached worktree `wt/core-final-merged-verification-20261124`, exact ef10ca7 SHA, macOS ARM64, Go 1.26.5, CGO_ENABLED=1, GOFLAGS empty, GOWORK=off; GOMODCACHE plain absolute `/Users/tangxingpeng/go/pkg/mod`.

Run each once, sequentially; raw stdout/stderr, command, start/end UTC and exit in workspace `bench/evidence/core-final-integration-postmerge-20261124/`. Stop acceptance and cleanup at any failure. Commands: gofmt editing; go build ./...; go vet ./...; go test -count=1 -timeout=45m ./...; four-package Race; selected runtime Race `^(TestT3B_|TestT4_|TestT11_)`. Exact test names confirmed in current source.

Observed results: gofmt exit 0 (empty); build exit 0; vet exit 0; full suite exit 1, 42 packages `ok`, one failing package (`runtime`), six packages without tests. Runtime failure: `TestT3B_CausalAuthorshipConcurrencyAndIgnoredFilesMatrix/RC34_F02_T6_dirty_and_clean_write_failures`, context_space_t3_test.go:512. T6-3 expected OutcomeUnknown but observed OutcomeFailed; mutation evidence remained unknown, post-ref `cm=unknown`, file edges zero, repair floor unchanged (min=1).

Source attribution: T6-3 describes failure after destructive f.Truncate(0), but publish_file.go:375-377 invokes AfterTruncate before swap and abortBeforeSwap:277-279 returns clean PublishFailedBeforeWrite on successful staging cleanup. This is a stale Runtime contract assertion introduced into incompatibility by the PRE publication change, not an observed environment fault. No production data loss established. Finding INT-PRE-REG-01, P1 acceptance blocker, Owner Sol, OPEN; minimum repair is a separate scoped test-contract correction retaining original-byte/no-publication assertions and independent verification. No fix or silent retry performed during this stopped acceptance run.

The full command ran 2026-11-24T00:58:14Z–01:12:49Z. Both remaining Race gates NOT_RUN because acceptance stopped. Test-level skips cannot be counted from the required non-verbose output; package-level results are preserved, no zero-skip claim. No historical branch/worktree deletion is authorized to proceed after this failure.

## Cleanup and independent acceptance

No historical deletion yet. Preflight finds 21 relevant historical worktrees, all commits reachable from merged main. One Opus worktree contains 12 ignored Owner/local state files and must remain untouched. Other candidates require final gates PASS, immediate status/occupancy/PR recheck and archive verification before removal.

Owner AGENTS.md SHA256 remains 84bb06cd73eb37997966e2d2e3cb8e8b181b27f97ff367d209f327bee818d1a0. Original main worktree will not be removed, reset, restored, stashed or cleaned. Desktop unrelated pre-existing changes preserved.

Current status: INTEGRATION_BLOCKED (already merged/pushed ef10ca7; regression acceptance failed). Post-merge independent acceptance not issued. All historical worktrees and local/remote branches retained; zero cleanup deletions. No release readiness or cross-platform native PASS claimed.
