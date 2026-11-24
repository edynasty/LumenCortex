**Verdict: PASS_WITH_LIMITATIONS.** Both P3 fixes hold up under my own checks. Nothing in production code changed. I found one new issue, a P3 evidence gap that doesn't block the merge. I modified nothing in the repositories or in `bench/evidence`. My scratch files are in `/tmp/lc-ov01-indep.91wlpA`.

**Model:** Claude Opus 5.5 (the harness reports `local-anthropic/combo/claude-opus-5-5`).

## Identity and sync
| Item | Result |
|---|---|
| HEAD | `51316603974d916295bc6f0560d46e7772f304e5`. The worktree is clean (`git status --porcelain` printed nothing) |
| Commits in range | `93a285593bd80820e2aaaf951ab4318a3783c3f1` (tests) and `5131660` (docs) |
| Remote | `git ls-remote origin fix/editing-sol-ov01-final-p3-20261123` returns `5131660…`, the same as local |
| Merge-base with `9ad5f2aa` | `9ad5f2aa1c4ace22829be574b562ea41b1dfe127` |
| Ancestry | `7aeb160` is an ancestor of HEAD (exit 0). Local and origin `main` are both `7aeb1607ffa588727c6fe04873f7b58a1dc16f15` |
| Changed files | `concurrency_test.go`, `engine_test.go`, `remediation_r2_01_test.go`, `review_regression_test.go`, and two reports: `editing-sol-ov01-final-p3-2026-11-23.md` (new) and `editing-opus-v01-platform-scope-2026-11-23.md` |
| Production Go changes | None. No changed `.go` file lacks the `_test.go` suffix |
| Same bytes as `b03c91e` | `publish_file.go`, `publish_darwin.go`, `publish_other_unix.go`, `fileid_windows.go`, `engine.go` and `store.go` all show no diff |

## Verdict by area
| Area | Verdict |
|---|---|
| SOL-OV01-01 | PASS |
| SOL-OV01-02 | PASS, with the P3 evidence gap below |
| Production integrity | PASS |
| Regression preservation | PASS |
| Platform docs | PASS |
| Evidence integrity | PASS_WITH_LIMITATIONS: the raw evidence is local only, and one Linux claim is not in its raw log |

**SOL-OV01-01 (test gating).** Each gate now sits at the smallest correct scope, and no original assertion was removed or weakened.
- **`TestWriteLeaseRefusesOtherHolder`:** these checks run before the gated `update_after_release` child:
  - the `lease_conflict` code and the holder
  - original bytes unchanged
  - no Committed, DirtyFailure, Published, Wrote, Verified, NewRev or NewSHA256
  - `CommitStatePrePublishFailure`
  - `Leases().Check` returns nil after release

  The new assertions are correct. The lease check (`engine.go:502-510`) runs before `prepare` and before any write, and `refuse` settles with `CommitStatePrePublishFailure` (`engine.go:471`).
- **`TestReviewEvictedReadRequiresFreshRevision`:** `unknown_rev` is the right code. It comes from `engine.go:753-757` during `prepare`, so it doesn't depend on the platform. The old check (`err == nil` with `t.Error`) became an exact-code `t.Fatalf`, which is stricter.
- **`TestReviewLeaseAcquisitionWaitsForCommit`:** the `update` subtest has the same setup and plan as the original (`Create:false`). The new `create` subtest exercises the same admission-lock assertions.
- **Large-file and four hashline tests:** the gate now comes after the read, anchor and op-resolution steps.
- **R201 stress:** the allowed-outcome sets and `roundsPerStep = 10` are unchanged. Native Darwin logs show `rounds=10` for all 13 steps.
- **Atomic stress `before_commit`:** this step is newly skipped without exchange. It was ungated at baseline and failed by chance. The implementer's `base-noexchange.jsonl` shows an `unsupported_filesystem` failure there, while my 6 baseline repeats all passed. Skipping it is justified because the allowed-outcome set must stay unchanged.
- **The 11 unchanged whole-test gates:** each starts with, or depends on, an existing-file update before any independent assertion. None hides a concrete independent check.

**Platform docs.** The report matrix matches the build-tagged code:
- Darwin exchanges with `renameatx_np(RENAME_SWAP)` and Linux with `renameat2(RENAME_EXCHANGE)`.
- Windows `renameSwapForPublish` returns ENOSYS (`fileid_windows.go:36-37`). FreeBSD and other Unix return ENOSYS too (`publish_other_unix.go:14`).
- An unsupported exchange is refused before publication (`publish_file.go:419-423`).
- There is no overwrite-rename fallback: `MoveFileExW` uses `dwFlags=0` and there is no `os.Rename` in production code.

## Gate recount (Go AST counter on both trees, `go run` exit 0)
| Counting level | Baseline `9ad5f2aa` | Final | Report |
|---|---:|---:|---|
| Helper call sites | 23 | 24 | 23 / 24 ✔ |
| Immediate top-level whole-test gates | 19 | 11 | 19 / 11 ✔ |
| Later top-level gates | 0 | 5 | 0 / 5 ✔ |
| Nested call sites | 4 | 8 | 4 / 8 ✔ |
| Nested gated cases (runtime subtests) | 5 | 13 | 5 / 13 ✔ |
| Enclosing top-level functions | 22 | 22 | 22 / 22 ✔ |

The baseline matches the prior review's definition (23 call sites, 19 top-level tests, 5 nested cases). Every line in `GATE_INVENTORY.json` matches my AST positions.

## Commands I ran (all with `GOWORK=off GOFLAGS=`)
**Proof that independent assertions now run without exchange (no-exchange overlay, my own `noex.go`, four tests, `-json`):**

| Target | Exit | Result | Detail |
|---|---|---|---|
| Baseline export | 0 | 8 PASS / 6 SKIP | The three lease/read parents and two-syscall stress SKIP |
| Candidate export | 0 | 17 PASS / 11 SKIP | The three parents, `create`, and the four create-only stress steps PASS |

**My own mutants (overlay on `engine.go`, each test run alone):**

| Mutant | Test | Baseline, no exchange | Baseline, native | Candidate, no exchange | Candidate, native |
|---|---|---|---|---|---|
| m1: wrong lease Holder (`engine.go:505`) | `TestWriteLeaseRefusesOtherHolder` | exit 0, SKIP | exit 1, FAIL | exit 1, FAIL at `concurrency_test.go:122` | exit 1, FAIL |
| m2: evicted read returns `conflict` (`engine.go:756`) | `TestReviewEvictedReadRequiresFreshRevision` | exit 0, SKIP | exit 0, PASS (baseline misses it even natively) | exit 1, FAIL at `review_regression_test.go:97` | exit 1, FAIL |
| m3: file written on lease refusal | `TestWriteLeaseRefusesOtherHolder` | exit 0, SKIP | exit 1, FAIL | exit 1, FAIL at `concurrency_test.go:125` | exit 1, FAIL |
| m4: admission released before write | `TestReviewLeaseAcquisitionWaitsForCommit` | exit 0, SKIP | not run | exit 1; `create` FAILs at `review_regression_test.go:163`, `update` SKIPs | not run |

**Other runs:**

| Command | Exit | Result |
|---|---|---|
| Native Darwin, all 29 tests in the four changed files, normal | 0 | 56 PASS / 0 FAIL / 0 SKIP |
| Same tests with `-race` | 0 | 56 PASS / 0 FAIL / 0 SKIP |
| Baseline no-exchange atomic stress, `-count=6` | 0 | 6/6 PASS |
| Windows and FreeBSD amd64 `go test -c` and `go vet` on the candidate | 0 each | compile only |
| `gofmt -l internal/editing` | 0 | empty |
| `shasum -c` of `SHA256SUMS`, final directory | — | 40/40 OK; the only file not listed is `SHA256SUMS` itself |
| `shasum -c` of `SHA256SUMS`, historical directory | — | 26/26 OK; digest `3cac00bc…`. `green-sim-no-exchange.txt` is covered by it |

**Recounts from raw logs (all match the reports):**
- **Historical full simulation:** 152 PASS / 6 FAIL / 24 SKIP, 78/4/19 at top level, ending in `FAIL`. The four failing tests are tagged `darwin` or `darwin || linux`.
- **Final full simulation:** 161/6/29, 81/4/16 at top level, package fail. It has the same four failures.
- **Final focused simulation:** 33/0/16, package pass.
- **macOS normal and Race:** 189 PASS each (98 top-level), exit 0. `TestPRE01RealDiskImageENOSPC` passes in both.
- **Linux:** 39 + 39 PASS with no SKIP/FAIL; `normal_exit=0`, `race_exit=0`.
- **`candidate-noexchange-final.jsonl`:** 17/0/11.

**Labels.** The historical full run is labeled FAIL / exit 1 (scope report line 64). Focused runs are labeled "focused simulation only" (line 65 and final report line 50). Cross-compilation is called compile-only, and the Darwin simulation is not presented as Windows/FreeBSD acceptance. The Core commit holds only the reports and tests; `bench/evidence` is outside any Git repository, local only, as the reports state.

## New findings
- **P3, confidence 0.6: the Linux "uid 1000" and Race-command claims aren't in the raw evidence.** The claims are at `docs/reports/editing-sol-ov01-final-p3-2026-11-23.md:67`, `FINAL_HANDOFF.md:51` and `COMMANDS.md:46`.
  - **Gap:** `logs/linux-focused.log` records the kernel, the Go version, `overlayfs`, the test output and the exit labels. It does not record the uid or the command lines; the macOS logs do record theirs (`command=…`). That Race was used is supported only by the `race_exit` label and the slower 1.81s run time.
  - **Reproducer:** `grep -n 'uid\|command=' logs/linux-focused.log` finds nothing.
  - **Impact:** documentation only. Pass counts and exit codes are correct. I did not re-run Linux.
- **Minor wording note (not counted as a finding):** final report line 18 calls the atomic-stress failure the "prior simulation's". The historical full simulation actually passed R201. The failure was in the implementer's baseline focused run.

## Limitations carried forward
- Native Windows, FreeBSD and ext4 behaviour remains UNVERIFIED.
- FreeBSD delete is unsupported, and L2, L3 and the hard-link-check race are unchanged. None of these was reopened.
- Option B remains PENDING_OWNER_APPROVAL; I am not claiming approval.