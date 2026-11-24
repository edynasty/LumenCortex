# SOL-OV01-01 / SOL-OV01-02 — final P3 remediation

- Owner/implementer: GPT-6.2 Sol, discovering reviewer of both findings.
- Base: `9ad5f2aa1c4ace22829be574b562ea41b1dfe127`; branch `fix/editing-sol-ov01-final-p3-20261123`.
- Scope: test gating, directly related assertions, report corrections and new evidence only. No production/API/locking/schema changes.
- Status: `SOL_OV01_FINAL_REMEDIATION_COMPLETE_PENDING_INDEPENDENT_REVIEW`. Both findings: `FIXED_BY_IMPLEMENTER / SELF_VERIFIED / PENDING_INDEPENDENT_REVIEW`; no independent closure claimed.
- Test correction commit: `93a285593bd80820e2aaaf951ab4318a3783c3f1`. Next observable milestone: publish the scoped report correction and deliver focused review handoff to Claude Opus 5.5 — High. No general PRE-01/PRE-02 audit and no merge authorized.

## SOL-OV01-01 — excessive whole-test gating (P3)

Original gates hid lease_conflict/holder/unchanged-file checks, stale-read rejection and create-capable concurrency/stress assertions. The independent report is preserved as `bench/evidence/editing-sol-ov01-final-p3-20261123/PRIOR_INDEPENDENT_REVIEW.md`.

Changes:
- `TestWriteLeaseRefusesOtherHolder`: lease rejection and holder identity, original bytes, clean receipt, no file publication/revision/hash and successful lease release all execute before a gated `update_after_release` child. The successful update and exact final bytes remain asserted on supported platforms.
- `TestReviewEvictedReadRequiresFreshRevision`: evicted-read rejection now explicitly requires `unknown_rev` and unchanged external bytes on every platform; only `update_after_reread` requires exchange.
- `TestReviewLeaseAcquisitionWaitsForCommit`: retain the original update case and add a create case using the same admission-lock concurrency assertions. Only update is gated.
- Large-file and four hashline tests: perform existing read/anchor/ParseOps assertions before gating the actual successful replacement.
- R201 stress: each original step keeps its allowed outcomes and 10 rounds. In two-syscall stress, after-prepare/before-publish create-only steps run without exchange. In atomic-allow-create stress, those two steps also run; only before-commit (which may replace an existing external file) is gated. This also removes the prior simulation's chance-dependent unsupported-update failure without changing its successful-update contract.
- All other gates remain where the observable subject requires a successful replacement or a previously published replacement. No assertion or stress round was deleted; no production behavior changed.

### Final gate inventory (not additive counting levels)

| Counting level | `9ad5f2aa` | Final test source |
|---|---:|---:|
| Helper call sites | 23 | 24 |
| Immediate top-level whole-test gates | 19 | 11 |
| Later gates within top-level bodies (read/parse first) | 0 | 5 |
| Nested helper call sites | 4 | 8 |
| Selected nested gated cases | 5 | 13 |
| Enclosing top-level test functions containing gates | 22 | 22 |

The final nested cases are: lease update-after-release; reread update; admission-lock update; R7 update-prefix; R4 05b; R201 b3/b5; atomic stress before-commit; five update-capable two-syscall steps. Unsupported-platform full simulation has 16 top-level plus 13 nested SKIP outcomes; the three lease/read parent tests now PASS and the four create-only stress steps PASS. Darwin/Linux capability constant stays true; no gate skips them.

### Red/green and mutation proof

- Baseline `9ad5f2aa`, no-exchange overlay: three independent lease/read parents SKIP; coverage checker returns 1. Lease-only `go test` itself exits 0 because SKIP is not FAIL; this is why coverage actions must be checked.
- Final test source, same no-exchange boundary: parents PASS; create-admission case and all four create-only stress steps PASS; coverage checker returns 0.
- Holder mutant changes only the initial lease-conflict Error.Holder to `WRONG-HOLDER`, in a temporary engine overlay. Baseline still SKIP/exit 0; final corrected lease test fails the exact holder assertion/exit 1. Production mutation was never written into the worktree.
- `probes/check_coverage.py` checks real JSON test actions; it does not inspect test source text or accept a non-throw assertion.

## SOL-OV01-02 — incorrect labels/counts (P3)

The committed Opus scope report is explicitly corrected; its raw logs and manifest remain untouched.

| Evidence kind | Exact classification |
|---|---|
| Historical complete simulation | **FAIL / exit 1**, 152 PASS / 6 FAIL / 24 SKIP, including subtests. Four top-level failures (78 PASS / 4 FAIL / 19 SKIP) come from Darwin/Linux-only test files |
| Selected assertions in historical simulation | **PASS — focused simulation only**. Not an independently passing full package and not native Windows/FreeBSD acceptance |
| Final complete simulation | **FAIL / exit 1**, 161 PASS / 6 FAIL / 29 SKIP; top-level 81 PASS / 4 FAIL / 16 SKIP. The same four excluded-platform failures remain; do not label GREEN |
| Final selected simulation | **PASS / exit 0 — focused simulation only**, 33 PASS / 0 FAIL / 16 SKIP, including relevant refusal, lease/read and create stress cases |
| Cross-compiled test binaries | Compilation only; not native execution |
| Host and Linux-container gates | Actual execution, with filesystem/environment limits stated separately |

Archived overlay paths referred to removed scratch locations; new replay maps copied fixtures to current temporary paths. No original evidence file was relocated or overwritten.

## Completed verification

Commands, exact selected regexes, overlays, JSON actions and counts: workspace evidence `bench/evidence/editing-sol-ov01-final-p3-20261123/COMMANDS.md`, `COUNTS.json`, `GATE_INVENTORY.json`, `logs/`, `probes/`. Historical evidence remains unchanged. Workspace evidence is outside the Core Git repository; the report correction is versioned, the local raw logs are not.

| Check | Observed result |
|---|---|
| macOS APFS editing normal, Go 1.26.5 arm64 | exit 0; 189 PASS including subtests, 98 top-level; 0 FAIL / 0 SKIP |
| macOS APFS editing Race | exit 0; 189 PASS including subtests, 98 top-level; 0 FAIL / 0 SKIP |
| Real 8 MiB `hdiutil` ENOSPC | PASS in both macOS runs |
| macOS four-package Race: editing/toolset/agent/session | exit 0; all four packages `ok` |
| `gofmt -l internal/editing` | exit 0; output empty |
| Linux 7.0.14-linuxkit arm64 VM overlayfs, Go 1.23.12, uid 1000 | focused normal and Race each exit 0; each 39 PASS / 0 FAIL / 0 SKIP including subtests |
| Windows amd64 and FreeBSD amd64 test binary cross-compilation | both exit 0; compile-only |
| Windows/FreeBSD build-tag selection | both exit 0; no-exchange tests selected, Darwin/Linux-only tests excluded |

No complete Core suite, native ext4, native Windows or native FreeBSD run was executed. The required bounded integration check was the four-package Race gate.

## Finding ledger and independent-review handoff

| ID / severity / owner | Trigger and root cause | Fix and regression evidence | Closure |
|---|---|---|---|
| SOL-OV01-01 / P3 / GPT-6.2 Sol | `9ad5f2aa`: unsupported exchange skips whole compound tests, hiding lease holder/read rejection and create-only assertions. Files: concurrency, engine, review regression and R201 test files | `93a285593bd80820e2aaaf951ab4318a3783c3f1`; narrower gates, exact `unknown_rev`, create admission case, per-step stress. Baseline coverage checker exit 1 → corrected exit 0; wrong-holder mutant baseline SKIP/exit 0 → corrected FAIL/exit 1; macOS/Linux normal/Race pass | FIXED_BY_IMPLEMENTER / SELF_VERIFIED / PENDING_INDEPENDENT_REVIEW |
| SOL-OV01-02 / P3 / GPT-6.2 Sol | `9ad5f2aa` scope report calls an exit-1 full simulation GREEN and conflates call sites/top-level/nested counts | Separate historical FAIL, selected PASS, compile-only and native evidence; recount final source and real JSON outcomes; historical raw logs/manifests untouched. Fix commit is the report correction commit following `93a285593b` | FIXED_BY_IMPLEMENTER / SELF_VERIFIED / PENDING_INDEPENDENT_REVIEW |

Review only `9ad5f2aa..final report commit`: both P3 fixes, unchanged update assertions/round counts, baseline-versus-candidate action proof, holder mutant and evidence labels/counts. Expected production diff: zero. Do not repeat a complete filesystem audit, reopen L2/L3/platform scope, infer Option B approval, or merge main. A fresh independent reviewer must issue their own verdict; this implementer cannot grant independent PASS.


## Scope and remaining limitations

- macOS APFS: supported; Linux local filesystems: supported where exchange is available. Linux VM overlayfs is not native ext4 acceptance.
- Windows/FreeBSD existing-file update: unsupported, fail closed. Native Windows/FreeBSD behavior remains UNVERIFIED; cross compilation and host fault injection are not acceptance.
- FreeBSD delete remains unsupported; delete-dependent suite failures predate this change and are outside this task.
- L2 old-FD writes, L3 SIGKILL staging orphans and final hard-link-check/exchange gap remain open and unchanged.
- Option B remains PENDING_OWNER_APPROVAL. This report does not mark either finding independently closed or authorize main integration/release.
