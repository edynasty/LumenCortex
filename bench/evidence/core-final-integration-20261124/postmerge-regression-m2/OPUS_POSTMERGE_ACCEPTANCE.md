# Post-Merge Independent Acceptance — Core `M2` (`d0cb171d0319bd0d523bbc19241343b9d1c3e60c`)

- **Reviewer:** `opus-final-accept` (fresh-context swarm worker, model `claude-opus-5-5-high`, read-only)
- **Reviewed Commit:** `d0cb171d0319bd0d523bbc19241343b9d1c3e60c` (`origin/main` after PR #12 and PR #13)
- **Recorded Timestamp (UTC):** `2026-11-24T03:02:52Z` (`request_id: d369c241-dd18-4338-bec9-bbbc749a6b02`, swarm board seq `#2`)
- **Scratch Log Created Outside Repo by Reviewer:** `/tmp/opus-final-accept-g6.txt`

## Verbatim Reviewer Report

```text
@coordinator [done] Read-only post-merge acceptance of Core M2 d0cb171. I edited nothing in the repo. The only file I wrote was the scratch log /tmp/opus-final-accept-g6.txt.
1 PASS: ls-remote origin main = d0cb171. Parents are cfef043 and 2852b56. gh shows PR#12 MERGED (cfef043, head 35409ca) and PR#13 MERGED (d0cb171, head 2852b56).
2 PASS: 35409ca, 316e99b, 2852b56, cfef043 and ef10ca7 are all ancestors of M2. Both merges are clean. 2852b56..M2 contains only the postmerge evidence (36 files) plus the report. cfef043..M2 contains only the carbon evidence (12 files) plus runtime/context_space_t3_test.go.
3 PASS: ef10ca7..M2 changes exactly one .go file, runtime/context_space_t3_test.go, which is a test. Zero production .go files changed.
4 PASS: Gate 4 exit 0. Packages: 43 ok, 6 [no test files], 0 FAIL, which matches go list (49). Environment: go1.26.5 darwin/arm64, GOFLAGS=[], CGO_ENABLED=1, GOWORK=off, tested_sha=M2. Run time 02:43:27–02:56:26Z. Limitation: the run was non-verbose, so test-level skips were not counted.
5 PASS: Gate 5 exit 0 on editing, toolset, agent and session.
6 PASS: Gate 6 exit 0 with 18 RUN / 18 PASS (3 top-level tests plus 15 subtests), 0 SKIP, 0 FAIL. My own rerun on the clean detached REG_WT (HEAD=M2) gave the same result: gofmt on the changed files was empty, build 0, vet 0, targeted -race 0 with 18/18 PASS, 0 skip. The worktree is still clean afterwards.
7 PASS: build, vet and 01a are all exit 0 with empty output. 01b (legacy audit) lists 16 files. None of them was touched between 7aeb160 and M2, and the count is still 16 today, so these were already unformatted before this work.
8 PASS: the Option B approval ADR is present on M2 (it already existed at ef10ca7).
9 PASS: the ADR and report give PRE-01 VERIFIED_WITHIN_SUPPORTED_SCOPE and PRE-02 VERIFIED_WITH_LIMITATIONS. The Opus REPORT.md gives PASS_WITH_LIMITATIONS → INT_PRE_REG_01_INDEPENDENTLY_VERIFIED for 2852b56, scoped to the focused fix only.
10 PASS: L2, L3 (P3) and the late hard-link race are still OPEN in the ADR and report.
11 PASS: platform table checks out. APFS supported. Linux supported where atomic exchange works; overlayfs/tmpfs verified, ext4 UNVERIFIED. Windows and FreeBSD unsupported, fail closed, UNVERIFIED. NFS/FUSE excluded. Binary export boundary kept. No release, TB2 or hosted-CI PASS claimed.
12 PASS: manifests verify. core-final-integration 525/525, postmerge 35/35, carbon 11/11, m2-regression 43/43, Opus logs 13/13, and no unlisted files in any of them. Nested manifests: 6 SHA256SUMS verify in place (26/17/26/9/1/1) and historical-manifest.sha256 1/1. R7 ORIGINALS.sha256 verifies 22/22 only from the private-r6-originals root; the wrong-root failure is preserved, as the docs say. main-agents-*.sha256 are hashes of stdin, so they cannot be checked as manifests.
13 PASS: postmerge 04-test-all.stdout = eda4550c… and still shows the runtime T6 FAIL (exit 1). The report = 00eec92b…. Each was added once in 35409ca and never changed after.
14 PASS: Core AGENTS.md is ` M` with SHA 84bb06cd…, unchanged.
Open limitations:
- SOL-OV01-EVIDENCE-01 (P3, OPEN).
- OPUS-INTPRE-EV-01 (P3, OPEN): docs/reports/runtime-int-pre-reg-01… is still missing.
- 16 legacy gofmt files (pre-existing, P3).
- Inherited platform and L2/L3 limits.
New observations (P3, non-blocking; not fixed because this review was read-only):
- (a) The INDEPENDENTLY_VERIFIED verdict exists only in volatile /private/tmp logs. On M2 itself, the carbon README still says PENDING_INDEPENDENT_REVIEW and the report still says INT-PRE-REG-01 OPEN (those are historical files kept as-is). The verdict should be archived somewhere durable.
- (b) The 01a command ends in `test ! -s /dev/null`, which is always true, so the gate does not actually fail on gofmt output. The PASS rests on stdout being 0 bytes and on my rerun.
- (c) The local Core main checkout is still at ef10ca7 and has not been fast-forwarded. Harmless.
Verdict: PASS_WITH_LIMITATIONS
```
