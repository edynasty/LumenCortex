# Final P3 remediation — independent-review handoff

Status: **SOL_OV01_FINAL_REMEDIATION_COMPLETE_PENDING_INDEPENDENT_REVIEW**

Finding Owner / implementer: GPT-6.2 Sol. Requested independent reviewer: Claude Opus 5.5 — High. No independent reviewer was launched in this remediation task; no Opus execution or PASS claimed.

## Published candidate

- Repository: https://github.com/edynasty/LumenCortex
- Branch: `fix/editing-sol-ov01-final-p3-20261123`
- Base: `9ad5f2aa1c4ace22829be574b562ea41b1dfe127`
- Test fix: `93a285593bd80820e2aaaf951ab4318a3783c3f1`
- Report correction / final HEAD: `51316603974d916295bc6f0560d46e7772f304e5`
- Remote candidate HEAD verified equal to final committed SHA; candidate worktree clean.
- Scope: four test files, two reports; production diff zero. No main merge, PR creation, tag or release.
- Versioned report: `docs/reports/editing-sol-ov01-final-p3-2026-11-23.md`.
- Local worktree: `wt/core-editing-sol-ov01-final-p3-20261123`.
- Raw evidence and SHA256SUMS: `bench/evidence/editing-sol-ov01-final-p3-20261123/`, workspace-root artifacts **not committed in Core**.

## Finding ledger

### SOL-OV01-01 — P3

- Owner: GPT-6.2 Sol.
- Affected baseline: `9ad5f2aa`; `internal/editing/concurrency_test.go`, `engine_test.go`, `review_regression_test.go`, `remediation_r2_01_test.go`.
- Trigger/impact: no-exchange capability skips compound tests, hiding independent lease rejection, holder identity, unchanged bytes, evicted-read rejection and create-capable assertions.
- Root cause: update capability gate precedes unrelated checks or encloses mixed stress steps.
- Fix: narrow gates; successful lease/reread update children only; keep `unknown_rev` rejection and unchanged external bytes; add create admission-lock case; run create-only stress steps while retaining update cases, allowed outcomes and all ten original rounds per step.
- Reproducer: same no-exchange overlay against baseline and candidate. Coverage checker exits 1 against skipped baseline, 0 against candidate's actual PASS actions.
- Mutant: wrong lease-conflict Holder; baseline test SKIP/exit 0, candidate exact holder assertion FAIL/exit 1. Never written into production worktree.
- Fix commit: `93a285593bd80820e2aaaf951ab4318a3783c3f1`.
- Self-regression evidence: macOS normal/Race and four-package Race; Linux VM overlayfs focused normal/Race; no-exchange coverage and holder mutant.
- Closure: **FIXED_BY_IMPLEMENTER / SELF_VERIFIED / PENDING_INDEPENDENT_REVIEW**.

### SOL-OV01-02 — P3

- Owner: GPT-6.2 Sol.
- Affected baseline: `9ad5f2aa`; `docs/reports/editing-opus-v01-platform-scope-2026-11-23.md` and prior independent review qualifications.
- Trigger/impact: report calls complete simulation GREEN despite exit 1; conflates call sites, top-level tests and nested cases; risks false platform acceptance.
- Root cause: selected passing assertions conflated with entire run; incompatible counting levels aggregated.
- Fix: explicit superseding correction separates historical complete FAIL, focused simulation PASS, compilation and native execution; recount final source scopes and actual JSON actions. Historical logs/manifests unchanged (26/26 checksums OK).
- Reproducer/evidence: original raw full-simulation 152 PASS / 6 FAIL / 24 SKIP, exit 1; corrected report retains that FAIL. Final full simulation also FAIL/exit 1 (161/6/29); final focused simulation PASS/exit 0 (33/0/16).
- Final inventory: 24 helper call sites, 11 immediate whole-test gates, five later top-level gates, eight nested call sites representing 13 nested gated cases. These levels are not interchangeable.
- Fix commit: `51316603974d916295bc6f0560d46e7772f304e5`.
- Closure: **FIXED_BY_IMPLEMENTER / SELF_VERIFIED / PENDING_INDEPENDENT_REVIEW**.

## Verification already performed

- macOS APFS Go 1.26.5 arm64 editing normal and Race: each exit 0, 189 PASS including subtests, 98 top-level, no FAIL/SKIP. Real hdiutil ENOSPC PASS in both.
- macOS editing/toolset/agent/session Race: exit 0, four packages ok. gofmt output empty.
- Linux VM: Linux 7.0.14-linuxkit arm64, Go 1.23.12, uid 1000, overlayfs. Focused normal and Race each exit 0, 39 PASS / 0 FAIL / 0 SKIP.
- Windows/FreeBSD amd64 test binaries cross-compiled: exit 0; no-exchange files selected, Darwin/Linux-only tests excluded. Native behavior UNVERIFIED.
- Exact commands, selected regexes and proof semantics: COMMANDS.md; raw JSON/logs and source fixtures retained. Temporary export/test binaries removed; no branches/worktrees deleted.

## Review contract

Review **only** `9ad5f2aa..5131660397`, both P3 fixes and affected test/evidence boundaries. Check that independent assertions run without exchange, update coverage and round counts are preserved, supported-platform behavior is unchanged, the holder mutant is detected, and every evidence label/count matches raw output. Inspect the four test files and two reports; no broad architecture/filesystem audit needed.

Do not grant independent closure based solely on this implementer's tests. Report your own verdict and limitations. No production modification, main integration or Option B approval is authorized by this handoff.

## Preservation and unchanged limits

- Local/remote main remains `7aeb1607ffa588727c6fe04873f7b58a1dc16f15`.
- Carbon local branch remains `aa9281fe1618acd87ffb322559d4ccd643b37e8a`; Carbon worktree clean and untouched.
- Protected Opus/Sol branches remain `71cce56acb06b457dad2a4fec3a2c3db56537fd6`, `b03c91e5db6969f0345a2034ecb172cf826bc792`, `9ad5f2aa1c4ace22829be574b562ea41b1dfe127`.
- Owner AGENTS.md modification preserved: SHA256 `84bb06cd73eb37997966e2d2e3cb8e8b181b27f97ff367d209f327bee818d1a0`; main tree still reports only ` M AGENTS.md`.
- L2 old-FD writes, L3 SIGKILL debris, final hard-link check/exchange gap, FreeBSD unsupported delete unchanged.
- Native ext4, Windows and FreeBSD acceptance remains UNVERIFIED. Option B remains PENDING_OWNER_APPROVAL.
- No deferred improvement was pulled into this task. Next step: focused independent review, not merge or another general audit.
