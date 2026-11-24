# OPUS-V-01 final independent review — GPT-6.2 Sol

## Verdict

**PASS_WITH_LIMITATIONS**, for macOS APFS and previously verified Linux configurations. OPUS-V-01's incorrect existing-file replacement expectations and platform claims are corrected. Production code is unchanged; no hidden macOS/Linux P2 regression was found. Windows and FreeBSD native acceptance remains **UNVERIFIED**.

Two new non-blocking P3 findings belong to the discovering reviewer, GPT-6.2 Sol. This explicitly read-only mandate does not authorize their remediation. Neither changes production fail-closed behavior nor hides a macOS/Linux P2 regression.

Recommend proceeding to Owner approval and final integration, without another general filesystem audit. This recommendation is not merge authorization or formal approval of Option B.

## Scope, identity and independence

- Repository origin: `https://github.com/edynasty/LumenCortex.git`.
- Range: `b03c91e5db6969f0345a2034ecb172cf826bc792..9ad5f2aa1c4ace22829be574b562ea41b1dfe127`.
- Branch: `fix/editing-opus-v01-platform-scope-20261123`.
- Candidate local HEAD and remote branch: `9ad5f2aa1c4ace22829be574b562ea41b1dfe127`; worktree clean at inspection.
- Ancestry: one commit above `b03c91e`; `merge-base --is-ancestor` exit 0.
- Remote main remains `7aeb1607ffa588727c6fe04873f7b58a1dc16f15`.
- Owner-owned `LumenCortex-Core/AGENTS.md` SHA256: `84bb06cd73eb37997966e2d2e3cb8e8b181b27f97ff367d209f327bee818d1a0`.
- Reviewer: GPT-6.2 Sol, separate from Opus's correction. Earlier Sol P2 work is not being granted a new independent PASS by its author; its previously independent VERIFIED_FIXED status is preserved because affected production blobs are unchanged.
- No repository code, test, report, backlog, branch, commit or historical evidence was changed. No push, merge, reset, cleanup or release. This report and independent run artifacts are only in `/tmp/lc-sol-opusv01-review.r1SGOU/`.
- LSP unavailable: no configured Go server and `gopls` absent. Used source inspection and Go build-tag selection instead.

## Separate assessments

| Area | Assessment |
|---|---|
| OPUS-V-01 remediation correctness | PASS: new expectations match the implemented safe refusal; supported-platform production is unchanged |
| Test gating integrity | PASS_WITH_LIMITATIONS: every gated scenario includes an existing-file replacement dependency, but several whole-test gates also suppress platform-independent assertions; see SOL-OV01-01 |
| Fail-closed assertion coverage | PASS for source and host simulation; native Windows/FreeBSD execution UNVERIFIED |
| Platform matrix | PASS for source-derived primitives and qualified evidence; not native Windows/FreeBSD acceptance |
| Report/evidence integrity | PASS_WITH_LIMITATIONS: all preserved hashes reconcile; whole simulation is incorrectly labelled GREEN and gate count is inaccurate; see SOL-OV01-02 |
| Remaining unsupported platforms | Windows/FreeBSD existing-file update refused. FreeBSD delete refused. Other Unix uses the same stub; untested |
| Merge recommendation | No in-scope P0/P1/P2 blocker found for macOS/Linux. Proceed to Owner decision and final integration; do not merge automatically |

## Production and platform verification

Compared complete Git trees by path, blob/mode identity: 20 changed paths, all documentation or `_test.go`; **zero production blob changes**. This includes `publish_file.go`, Engine/stripes, SnapshotStore, receipts, MutationEvidence and SQLite-related code.

- Darwin: `publish_darwin.go:11-16` uses `renameatx_np(RENAME_EXCL/RENAME_SWAP)`.
- Linux: `publish_linux.go:11-16` uses `renameat2(RENAME_NOREPLACE/RENAME_EXCHANGE)`.
- Windows: `fileid_windows.go:36-41` always returns ENOSYS for exchange, classified unsupported. Create uses `MoveFileExW` without replacement (`:177-202`, `:287-335`); deletion quarantine uses the same no-replace move (`:44-54`).
- FreeBSD/other Unix: `publish_other_unix.go:1-16` returns ENOSYS for exchange and no-replace rename. Create can use `link`; when links and no-replace rename are unavailable it refuses (`fileid_unix.go:209-225`). Delete refuses unsupported quarantine (`publish_file.go:827-833`).
- Existing-file update: unsupported exchange follows the existing owned-staging cleanup path (`publish_file.go:268-282,419-423`), then `prepared.fail` maps clean refusal to PublishUnsupported (`engine.go:946-962`). Concurrent faults can yield conflict/uncertainty instead; the platform matrix describes ordinary update attempts, not every possible earlier error or no-op.
- `publish_file.go:409-417` final link-count refusal and `:419-423` unsupported-exchange refusal remain byte-identical to the independently accepted candidate. No overwrite fallback was reinstated.

The Desktop release procedure describes macOS artifacts. Workspace PLATFORM-WIN-NTFS-001 explicitly defers native Windows/FreeBSD compatibility. No current native Windows editing requirement was established from those contracts.

## Every gated scenario

There are **19 top-level tests and five selected nested cases**, implemented by **23 helper callsites**: 19 top-level plus four nested locations, one of which selects both R201 b3 and b5. Historical simulation has 24 SKIP outcomes. The report's “19 tests and three subtests” groups R201 b3/b5/stress together and is not a literal test count.

`platform_exchange_swap_test.go` sets true on darwin/linux; `platform_exchange_none_test.go` sets false elsewhere. `requireExistingFileUpdate` is not a runtime syscall probe and cannot skip a genuine unsupported-exchange regression on darwin/Linux.

| Gated test/case | Required replacement or dependency | Independent assertion limitation |
|---|---|---|
| ConcurrentLineEditsLoseNoUpdate | Four successful edits of existing `f.txt` | Outcome requires publication |
| LargeFileAnchorEditIsBounded | Successful splice of existing large file | Initial read/line-count checks are also skipped |
| WriteLeaseRefusesOtherHolder | Successful update after releasing lease | Earlier lease_conflict/holder/unchanged-file checks need no exchange; SOL-OV01-01 |
| MultiFileCommitStopsAtFirstWriteFailure | Verified existing-file prefix before second-path failure | Requires prefix publication |
| MultiFileCommitIsAtomic | Two successful existing-file updates | Requires publication |
| HashlineFreshEditOnSnapshot | Successful edit and registered new revision | Requires publication |
| HashlineRelocatesAcrossUnrelatedInsertion | Successful relocated edit | Requires publication |
| HashlineAppliesBatchAgainstOneSnapshot | Successful multi-op update | Requires publication |
| HashlinePreservesEncoding | Successful update with byte-exact encoding | Requires publication |
| DiffHistoryIsBounded | Series of successful updates to populate history | Pure history operations remain tested separately |
| R7PrefixExternalAppendBeforeLaterConflict | Published update prefix before external append | Create-based Windows prefix case retains analogous coverage |
| R7PrefixExternalSameByteReplacement | Published update prefix then external replacement | Requires prefix publication |
| R7R2SelfReplayIsFailedBeforeWrite | Initial successful update before replay | Requires published predecessor |
| R7SameInodeExternalOverwriteIsExternal | Initial published update before external overwrite | Requires prefix publication |
| ReviewEvictedReadRequiresFreshRevision | Successful update after fresh reread | Earlier eviction/stale-read refusal checks also skipped; SOL-OV01-01 |
| ReviewLeaseAcquisitionWaitsForCommit | Successful update with blocked concurrent lease acquisition | Admission-lock wait can be exercised with create; whole test skipped |
| ReviewByteEvictionKeepsNewestViewFence | Successful large update establishes newest view | Requires predecessor publication; snapshot budget test remains ungated |
| XReviewConcurrentLineEditAndWholeFileWrite | Exactly one existing-file update wins | Requires publication |
| XReviewHashlineAnchorsAfterOtherSessionCommit | Successful predecessor and relocated edit | Requires predecessor publication; separate stale-anchor tests remain ungated |
| R7VerifiedPrefixUpdateCreateDelete/update | Verified update prefix | Only update case gated; create/delete cases remain |
| R4/05b_inplace_close_failure_is_uncertain | Successful swap reaches post-publication injected close error | Requires publication |
| R201/b3 | Swap occurs before held-FD external write | Requires exchange |
| R201/b5 | Replacement of complete externally created file | Requires exchange |
| R201/two_syscall_allow_create_stress | Several update-capable interleavings | Also skips create-only stress steps; exclusive/atomic create stress and deterministic create cases remain |

Ungated stale-anchor/overlap/cancellation, lease prefix/expiry, snapshot budget and create/delete tests remain. However the precise lease_conflict holder assertion has no equivalent ungated test; the ungated validation-acquired lease test only asserts non-nil error and unchanged bytes. This is a real unsupported-platform coverage reduction, not a macOS/Linux regression.

## Safe-refusal assertions

`platform_exchange_none_test.go:19-46,52-96` checks:
- unsupported_filesystem and a nothing-written explanation;
- failed_before_publication in result and receipt; not committed, not dirty;
- PublishUnsupported for result and receipt; no changed/published/wrote/verified/rolled-back effect;
- empty NewRev and NewSHA256;
- same file identity via os.SameFile and exact original bytes;
- no `.lcx-*` staging entry;
- second batch file not_attempted and unchanged;
- single-update LatestRev is not the rejected content's revision.

The snapshot assertion excludes a trusted latest rejected revision; it does not enumerate every SnapshotStore entry. Source control flow corroborates no registration of the refused content. Do not overstate this as exhaustive snapshot-store inspection.

Neither this new test nor its helper replaces the syscall when executed natively. The Windows timestamp test uses the same helper. Its prefix test now creates A and B, preserves exact appended A bytes and foreign B bytes, and checks C not_attempted. It no longer relies on a Windows existing-file update succeeding.

## Evidence integrity and simulation classification

Original evidence is accessible at `bench/evidence/editing-opus-v01-platform-scope-20261123/`, outside Core Git. Core commit contains the reports/tests, not this evidence directory.

- SHA256SUMS digest independently recomputed: `3cac00bc0211c5cbabacfcf8ca62237ba302ea8b313337f7a31b89d11028f0d1`.
- 26/26 entries verified; no missing or extra payload files.
- `sim_none.go` exactly equals committed no-exchange source after removal of the build tag.
- Old/new simulated Windows source equals the corresponding committed body, except removed build tag and replacement of the Darwin-unavailable Windows timestamp log with `_, _ = preClose, postClose`.
- Archived overlays refer to removed `/tmp/opus-v01-sim.jQalOW` paths. Replayed with relocated paths in this review's isolated export, without modifying originals.

Original whole-package simulation: **FAIL, not GREEN**. Counts are 152 PASS, 6 FAIL, 24 SKIP including subtests; top-level 78 PASS, 4 FAIL, 19 SKIP. Four failures map to:
- TestOpusPRE01SwapReplicatesMetadata — darwin only;
- TestOpusPRE02UncertainSwapKeepsDisplacedOriginal — darwin || linux;
- TestPRE01UpdatePreservesPermissionsSymlinksAndHardlinks — darwin || linux;
- TestPRE02PostSwapDisplacedOpenFDWriteRestored — darwin || linux (two failing nested cases).

Verified `go list` build selection: none of those files appears in Windows/FreeBSD TestGoFiles. Their exclusion explains the four failures but does not turn the complete simulation into a passing run. The relevant positive assertions did pass. Simulation retains Darwin create/delete primitives and is not native platform acceptance.

## Independent commands and observed results

Runs used `/tmp/lc-sol-opusv01-review.r1SGOU/tree`, exported by `git archive 9ad5f2a`. No tests ran against a modified repository worktree. GOWORK=off, GOFLAGS empty.

1. `GOOS={darwin,linux,windows,freebsd} GOARCH=amd64 CGO_ENABLED=0 go list -f '{{range .TestGoFiles}}{{println .}}{{end}}' ./internal/editing` — build-tag selections confirmed.
2. `go test -overlay /tmp/lc-sol-opusv01-review.r1SGOU/overlay-{old,new}.json -count=1 -v -run '^(TestOpusV01ExistingFileUpdateRefusedWithoutExchange|TestR7WindowsNativeTimestampAndNoCompensation|TestR301CreateWithoutNoReplacePublishIsRefusedClearly|TestWriteLeaseRefusesOtherHolder)$' ./internal/editing` — old Windows expectations exit 1; corrected focused subset exit 0. Logs `focused-old.log`, `focused-new.log`. Lease test explicitly skipped under simulated no-exchange.
3. `go test -race -count=1 -v -run 'TestSolPREL1|TestOpusPRE01HardLinkedUpdateRefusedBeforeAnyWrite|TestR7WindowsNativeTimestampAndNoCompensation|TestConcurrentLineEditsLoseNoUpdate|TestLargeFileAnchorEditIsBounded|TestWriteLeaseRefusesOtherHolder|TestMultiFileCommit(IsAtomic|StopsAtFirstWriteFailure)|TestHashline(FreshEditOnSnapshot|RelocatesAcrossUnrelatedInsertion|AppliesBatchAgainstOneSnapshot|PreservesEncoding)|TestDiffHistoryIsBounded|TestR7(PrefixExternalAppendBeforeLaterConflict|PrefixExternalSameByteReplacement|VerifiedPrefixUpdateCreateDelete|R2SelfReplayIsFailedBeforeWrite|ForwardPublicationFaultMatrix|SameInodeExternalOverwriteIsExternal)|TestR201TwoOSProcessesCreateRace|TestR301|TestReview(EvictedReadRequiresFreshRevision|LeaseAcquisitionWaitsForCommit|ByteEvictionKeepsNewestViewFence)|TestXReview(ConcurrentLineEditAndWholeFileWrite|HashlineAnchorsAfterOtherSessionCommit)' ./internal/editing` — exit 0, 57 PASS outcomes, zero FAIL/SKIP. Log `native-targeted-race.log`. Windows-tagged test is absent in this native Darwin run, not natively passed.
4. `GOOS={windows,freebsd} GOARCH=amd64 CGO_ENABLED=0 go test -c -o /tmp/lc-sol-opusv01-review.r1SGOU/editing-{windows,freebsd}.test ./internal/editing` — each exit 0. Compilation only.

Historical macOS normal/Race evidence independently recounts 98 top-level / 175 including-subtest PASS, no FAIL/SKIP. The recorded normal test name/outcome set equals the earlier b03c91e evidence. Four-package Race/build/vet/cross logs all record exit 0. Linux evidence records normal/race exit 0, 168 PASS and two pre-existing skips, but only summary output was retained; did not infer an independently re-executed Linux run from it. No full Core regression rerun was needed or performed.

## New findings ledger

### SOL-OV01-01 — P3 — unsupported-platform independent assertion over-gating

- Owner: GPT-6.2 Sol (discoverer).
- Affected commit: `9ad5f2aa1c4ace22829be574b562ea41b1dfe127`.
- Primary location: `internal/editing/concurrency_test.go:103-132`; related `review_regression_test.go:85-106,128-163` and `remediation_r2_01_test.go:470-480`.
- Trigger: Windows/FreeBSD, or the no-exchange overlay: top-of-test helper skips before lease-conflict/holder/unchanged-file assertions, although those checks run before exchange and remain meaningful. Focused new-overlay log confirms SKIP.
- Root cause: a final successful update dependency gates the whole test rather than just that portion. Similar compound tests suppress independent negative/read/stress assertions.
- Impact: unsupported-platform test coverage reduced; production and all Darwin/Linux assertions remain unchanged. **Non-blocker for selected macOS/Linux integration.**
- Minimum fix when separately authorized: retain platform-independent lease/refusal assertions before gating only update-success portions; use a supported create scenario for lease-admission concurrency where suitable. Keep exact native unsupported-update coverage.
- Status: OPEN, READ_ONLY / REMEDIATION_REQUIRES_SEPARATE_AUTHORIZATION. No fix commit; no red/green remediation claimed.

### SOL-OV01-02 — P3 — simulation verdict and gate-count mislabelling

- Owner: GPT-6.2 Sol (discoverer).
- Affected commit: `9ad5f2aa1c4ace22829be574b562ea41b1dfe127`.
- Locations: scope report `:54-58,67`; local log `logs/green-sim-no-exchange.txt`.
- Trigger/reproducer: compare report's GREEN row and “19 + 3” counts with original final FAIL and 19 top-level plus five nested SKIP outcomes.
- Root cause: successful targeted assertions and grouped callsites were described as a whole-run verdict/literal test count.
- Impact: overstates simulation success and obscures scope accounting; raw evidence retains failures and checksums reconcile. **Non-blocking documentation limitation.**
- Minimum fix when authorized: label whole simulation FAILED / relevant assertions PASS; report 23 callsites, 19 top-level tests and five selected nested cases. Do not edit historical raw evidence.
- Status: OPEN, READ_ONLY / REMEDIATION_REQUIRES_SEPARATE_AUTHORIZATION. No fix commit.

## Remaining boundaries and next decision

- Windows and FreeBSD native test execution remains UNVERIFIED; cross-compilation and Darwin simulation do not close it.
- FreeBSD delete unsupported behavior and resulting delete-dependent test failures predate this correction; deferred platform scope, not reopened here.
- L2 old-FD writes, L3 SIGKILL residue and the post-final-link-count race remain unchanged and open.
- Option B formal approval remains PENDING_OWNER_APPROVAL. No approval was inferred from test success or this task.
- OPUS-V-01 implementation correction is independently accepted with these limitations. Proceed to Owner approval and scoped final integration. Do not launch another general filesystem audit.
