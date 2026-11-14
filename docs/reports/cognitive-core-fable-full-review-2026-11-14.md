# Cognitive Core — Independent Full Review and Continuous Remediation (2026-11-14)

Status: **REVIEWED; 17 FINDINGS FIXED AND SELF-VERIFIED; FROZEN LOCAL REVIEW CANDIDATE.** Not merged, pushed, tagged or released.

Reviewer and fix owner: Fable 5.2 (`local-anthropic/claude-fable-5-2`), one instance, no subagent, no other model. Nobody other than the author of the fixes has verified them, so this report does not grant an independent PASS (workspace rule: a finding owner cannot grant one). Engineering completeness is not evidence of long-term autonomous-learning benefit; that benefit is unmeasured.

中文版：[cognitive-core-fable-full-review-2026-11-14.zh-CN.md](cognitive-core-fable-full-review-2026-11-14.zh-CN.md). Finding ledger: [finding-ledger.md](../cognitive-core/finding-ledger.md#fable-full-review-2026-11-14-ffr).

## 1. Baseline and commit relationship

| Ref | SHA | Role |
|---|---|---|
| Core `main` | `00344b5d6a67673fea9fcbc90e0cab16de17dc57` | protected baseline; not modified |
| candidate code baseline | `949b2c6533dcf66f1401d0b72c5d2c44cb560354` | last production/test commit of the candidate |
| frozen candidate | `c1d322938942c9bb4a82f184d1624103b92a5bf4` | tip of `feat/cognitive-core-final-completion-20261113` (documentation freeze over `949b2c6`) |
| review, final code | `c6bdfaf066f9c9fab7be824523471fa1b0bdcf92` | last Go change of this review; every suite in §8 ran on it |
| review, frozen tip | see `final/GIT_STATE.txt` in the evidence | this report and the ledger on top of `c6bdfaf`; no Go file differs |

`main` is the merge base of the candidate, which is five commits ahead (`2151f17`, `445e667`, `eb47a7d`, `949b2c6`, `c1d3229`). The review branch `review/cognitive-core-fable-full-review-20261114` (worktree `wt/core-cognitive-core-fable-full-review-20261114`) starts at `c1d3229` and adds 18 commits (17 fixes, one test-only) and the documentation. No history was rewritten; `main`, the candidate branch and every other worktree are untouched.

Size: candidate over `main`, 190 files, +24353/−1073, of which production Go 63 files, +9955/−469. Review over the candidate (`c1d3229..c6bdfaf`): production Go 19 files, +418/−44; 22 new test files; one existing test file adapted (+16/−3: a changed Store signature and one added case). No test was deleted or skipped. No migration was added or changed.

## 2. Scope and method

- The complete production diff `main..c1d3229` was read end to end, not the latest commit, together with the inherited code it calls into: session store, fences and privacy purge, evaluator, agent loop and callback dispatcher, migrations 0014–0017 and their down scripts.
- For every suspected defect a concrete counterexample was written as a Go test that drives the public `Engine` API (or the Store boundary) on the real CGO SQLite store with a scripted provider. It was run red on the unfixed code (a pristine `git archive` of `c1d3229` for every finding, and for 15 of the 17 also the review head before the fix), the root cause was fixed, the test was run green and the affected packages were rerun. A suspicion without a counterexample is not a finding.
- No earlier verdict was taken as a conclusion: not Astra's reports, not other reviewers' closures, not this reviewer's own earlier PASS. The ten historical defect classes were re-derived with new probes (§4).
- No paid model request, no production database, no Desktop change, no merge, push, tag or release.

## 3. Findings

All 17 were reproducible on pristine `c1d3229`. Detail, root cause, regression and receipts per finding are in the ledger.

| ID | Sev | Domain | Defect on the candidate | Fix |
|---|---|---|---|---|
| FFR-001 | P1 | Learning, Verification | A failed consumer action plus a pre-existing artifact was recorded as a successful reuse. | `cee114a` |
| FFR-002 | P2 | Learning | An adoption whose run ended unsettled could never be closed. | `22601ed` |
| FFR-003 | P1 | Learning, Belief revision | After the origin thread continued, a consumer's outcome (counterexamples included) could never be recorded. | `5fb8bfb` |
| FFR-004 | P2 | Memory, Governance | A counterexample could not settle on an archived record; an archived counterexample could not be managed. | `c27f7ad` |
| FFR-005 | P2 | Reasoning, Belief revision | A record that is not yet valid was assessed as stale. | `24e7038` |
| FFR-006 | P3 | Recovery | Every Engine open reported a recovery failure for an adoption that was merely not due. | `6337bef` |
| FFR-007 | P2 | Learning, Concurrency | A concurrent revision of the shared record lost a consumer's checked outcome. | `e443e8f` |
| FFR-008 | P1 | Learning, Verification | A procedure whose origin action failed was validated as a positive lesson. | `129c2e5` |
| FFR-009 | P2 | Learning, Concurrency | Under contention on a full record the settlement gave up and the use stayed open. | `9e491f8` |
| FFR-010 | P3 | Recovery | Every Engine open reported a recovery failure for a fenced consumer. | `7b75581` |
| FFR-011 | P3 | Attention, Memory | A record whose uses are spent was still recalled as adoptable. | `c73e8a8` |
| FFR-012 | P2 | Privacy, Belief revision | Deleting a consumer let an invalidated identity be extracted again as a fresh record. | `b8bd7ba` |
| FFR-013 | P3 | Attention | A substituted tool was refused with the message for an unreproducible origin. | `b27823b` |
| FFR-014 | P2 | Privacy, Belief revision | Deleting the record's owner session let an invalidated identity be extracted again. | `e0c9319` |
| FFR-015 | P2 | Learning | A child run's applied use, counterexample and proposal were not settled when it ended. | `a9250c3` |
| FFR-016 | P1 | Learning, Verification | The outcome of a use was checked when the run ended or later, so later changes were credited to, or held against, the procedure. | `d4fde6d` |
| FFR-017 | P2 | Learning, Memory | A record admitted uses whose feedback its 64 KiB could not hold: an applied use could never record its outcome, and a record its uses had filled could not be revoked. | `c6bdfaf` |

Four P1, nine P2, four P3. Sixteen of the seventeen concern Experience records (all but FFR-005), and twelve of those lie between an adoption and its feedback. The candidate's tests covered that loop for one consumer of a small record on a quiescent top-level run. The defects are where that assumption ends: several consumers of one record (FFR-007, FFR-009), an origin that keeps working (FFR-003), host lifecycle operations between adoption and settlement (FFR-004), child runs (FFR-015), recovery (FFR-002, FFR-006, FFR-010), deletion against identity (FFR-012, FFR-014), a record that fills up (FFR-011, FFR-017), and what a passing check is evidence of (FFR-001, FFR-008, FFR-016).

## 4. The ten historical defect classes, re-derived

Probes are throwaway tests kept as text in the evidence (`probes/`); the ones that found a defect became the regressions above.

| # | Class | Result on the candidate | Now |
|---|---|---|---|
| 1 | An Experience that was not really executed is recorded as a successful use | Reproducible three ways (FFR-001, FFR-008, FFR-016) | fixed |
| 2 | An unauthorized write or a permission denial counted as success | Held: a denied action leaves an `unknown` receipt and the use is `unknown`, `used=false` | held |
| 3 | `read_file` and other real reads misidentified | Held: reads are traced origin steps the consumer must reproduce; only the refusal text was wrong (FFR-013) | held, text fixed |
| 4 | Future-effective knowledge reported as expired | Reproducible in `assess_cognition` (FFR-005) | fixed |
| 5 | A caller can choose the invalidation time | Held: `InvalidatedAt` and `VerifiedAt` are refused, `RecordedAt` is set by Core, an invalidation cannot be cleared, backdated or reactivated | held |
| 6 | Old knowledge readable after a privacy deletion | Content, copied tool rows and derived records are erased and unreadable. The identity could be revived after a deletion (FFR-012, FFR-014) | fixed |
| 7 | Feedback or ownership leaks between consumers | Held: another reader sees state and confidence only, no uses, and cannot write a foreign feedback | held |
| 8 | An old request reads or writes after a source revocation | Held: the in-flight consumer run fails `fenced`; eight later write paths are refused. Recovery noise only (FFR-010) | held, noise fixed |
| 9 | Fence, CAS and concurrent commit races | Lost settlements under contention were reproducible (FFR-007, FFR-009) | fixed |
| 10 | Governor interface exists but the call path is unusable | Held on the candidate: Analyze, Plan, Validate, Apply, archive, restore, revoke and deletion work on the normal SQLite store through the public API (pinned by `runtime/governor_owned_lifecycle_review_test.go`, `fde3bcd`). On `main` the class is still present, see §10.7 | held |

## 5. The seven domains

**Memory.** Working memory is Task State and `task` records; episodic memory is the origin trace (every execution of the run, at most 16, otherwise the trace is truncated and cannot be validated), check results and `evidence`/`negative` records; semantic memory is typed assertions; procedural memory is Experience records. The transition from an episode to long-term memory happens at the run's terminal boundary (`propose_experience` → extraction from real run records → validation by host-bound checks) and in the bounded maintainer pass (`MaintainCognition`). Compression cannot lose evidence: a lifecycle revision that differs from its predecessor in anything but status, memory policy, record time and reason is refused (`CommitCognitiveLifecycle`); consolidation is accepted only between records with the same assertion, evidence, sources, dependencies, body and validity window; versions are immutable; counterexamples, failed evidence and safety records are protected from archiving. Access count and last access are stored per reader, apart from outcomes and from evidence, and no archive decision reads them: `MaintainCognitiveMemory` archives only records already marked stale or dormant that are unprotected, unreferenced and owned, lowest importance first, and reports pressure instead of deleting to reach a number; the Engine gives the Governor's analysis no access telemetry (a host may pass its own, which then weighs at most 10% of a value that only orders records already stale or dormant, unprotected and with at most one edge). Capacity is a refusal, never an eviction: every node keeps the room of the host's lifecycle decision and an Experience record the room of the feedback of its open uses, so a full record can still be revoked and an applied use can still settle (FFR-017). Defects found: FFR-004, FFR-011, FFR-012, FFR-014, FFR-017.

**Attention.** Recall returns at most eight scoped locators with an applicability that is explicitly not correctness; `read_thread` carries bounded conditional locators and contested warnings; a top-level Start gets one bounded index row of locators after a bounded revalidation, never a claim's text. Relevance is lexical. A warning that does not match the task's words is not shown, which is an attention miss and not a safety hole: adoption and application do not depend on attention, and a contested record cannot be adopted however it was found. Defects found: FFR-011, FFR-013.

**Reasoning.** Records carry an epistemic class (fact, inference, hypothesis, decision, unknown, experience). Only framework verification makes a fact; a host or model proposal with evidence, a verification time or the class `fact` is refused. `assess_cognition` returns current, unknown, stale and conflict per exact record version with the reason, and an action among continue, gather_evidence, ask, defer and stop; an unsupported `continue` stays `gather_evidence`. The decision is stored with the exact versions it read. The model reasons; the framework constrains state and authority. Defect found: FFR-005.

**Learning.** The loop is: episode → `propose_experience` → extraction from the run's real records → validation by host-bound condition and outcome checks → recall → adoption after the consumer's own condition checks → `apply_experience` through the ordinary tool dispatcher → outcome observed when the last action has settled → feedback when the run ends → `needs_revision` on a counterexample → revised definition, validation reset, uses kept → next consumer. After this review a success requires all of: a complete settled receipt with dispatcher-issued execution identities, every action settling as its origin step did (FFR-001), an origin whose own actions did not fail (FFR-008), an outcome observed at the application boundary by the host-bound check (FFR-016), and a completed consumer run. A counterexample is recorded under contention (FFR-007, FFR-009), after the origin continued (FFR-003), on an archived record (FFR-004), for child runs (FFR-015), on a record its uses have filled (FFR-017) and after a restart, without replaying any action. Saving, recalling or summarizing is nowhere counted as learning: recall and adoption give no credit. Limits: reuse is exact-action only, and a record accepts at most eight uses in its lifetime, fewer when it is large.

**Belief revision.** Nothing is overwritten: every change is a new immutable version under CAS, and history stays readable within the reader's scope. A changed source makes a record stale; a not-yet-valid one is unknown (FFR-005); an invalidation is immutable and an invalid identity cannot be restored, also not by deleting a consumer or the owner and extracting again (FFR-012, FFR-014). Dependencies pin exact versions, so a newer premise invalidates applicability instead of replacing the premise silently. Conflicts between records with the same assertion subject are reported, not resolved by order.

**Governance.** On the candidate the Go Governor is usable on the normal SQLite store through the public `Engine` API: Analyze, Plan, Validate and host Apply (tiers, archive of owned unprotected records, consolidation of equivalent evidence), plus `SetCognitiveLifecycle` (archive, restore, revoke) and `DeleteSession`. The model or curator proposes; only the host applies, with exact expected versions and all-or-nothing batches. Branch, promote and epoch proposals stay advisory because they introduce new claims. No model tool reaches a lifecycle operation.

**Verification.** Evidence is a qualified result of a host-approved check bound to the exact assertion (kind, target, key and value): it records the executing session and run, the result sequence, the check definition digest, the approval epoch, the workspace fingerprint, scope and times, and it stops standing when the binding or the definition approval is revoked or a privacy epoch passes it. A check that did not run is never a success; model confidence is a separate field and never evidence. The three P1 defects in this domain were not forged evidence but a real passing check taken as evidence of something it did not show (FFR-001, FFR-008, FFR-016).

## 6. Unified architecture

The domains share one set of contracts in `internal/knowledge`, enforced in one Store: `Scope` (visibility and containment), `Times` (observed, valid window, recorded, verified, invalidated), `Evidence`, `SourceRef` with source versions, `NodeRef` dependencies, ownership by session, the lifecycle status, source fences with privacy epochs, and immutable versions under CAS. I found no contradictory state between two authorities, no public API that bypasses governance (`PutCognitiveNode` refuses Experience records, evidence, verification times and the class `fact`), and no interface without a reachable call path on the candidate.

One structural limitation remains (FFR-D2). There are two record families: maintainer candidates (`cognition_candidates`, read through `read_knowledge` and the Start index) and native records (`cognition_nodes`, used by `assess_cognition`, the Governor and Experience). They share the assertion type, host bindings, check evidence, fences and the privacy purge, but not one lifecycle, conflict detection or read path. Each fact lives in exactly one family and neither can present a claim as current without a passing host-bound check at the reader's present state, so they cannot disagree about a verified claim unless the host approved contradictory checks. A contradiction between unverified claims of the two families is not reported as a conflict, and the Governor does not manage maintainer candidates. Merging them is a schema and API change and is not a review fix; the smallest step would be a read-only projection of candidates into the assessment, without a second write path.

Complexity: the candidate reuses one Engine, Agent loop, Store, evaluator and maintenance lease and adds no service, scheduler or second agent. The review added 418 production lines and no new abstraction: one receipt field, one room computation on the node and a few small predicates.

## 7. Engineering review

- **Real SQLite path.** Every regression runs through `Engine` on the CGO `mattn/go-sqlite3` store in a temporary state directory; nothing is mocked below the provider.
- **Migrations.** 0014–0017 and their down scripts were read; the 0017 down guard refuses a downgrade that would orphan completion-era data. The review changes no schema. FFR-016 adds one optional JSON field to the application receipt; a receipt written without it settles as `unknown`. FFR-017 changes no stored format: it narrows what a content revision may fill (60 of a node's 64 KiB) and when a record admits a use. Records the candidate itself wrote are not converted, and two fixes do not hold for them (FFR-D4).
- **Transactions, CAS, lease, fence.** Admission, settlement, lifecycle batches and privacy deletion are single write transactions that recheck versions and fences at commit. The settlement of a shared record is recomputed after a benign conflict, bounded by the number of writers of a full record (FFR-007, FFR-009). The inherited cross-process fence guards (F05) are byte-identical to their historical fix and still detect the defect: with the fence read that `44bcda9` removed restored in a scratch copy, the deterministic guard fails (`probes/f05-mutant-c6bdfaf.*`).
- **Consumer isolation.** Reads and mutation replies project only the reader's own uses; a foreign feedback is refused.
- **Privacy deletion and source revocation.** Origin content, copied tool rows, derived records and receipts are erased; in-flight consumers fail `fenced`; an identity stays withdrawn after any deletion (FFR-012, FFR-014; for a record the candidate wrote, see FFR-D4).
- **Restart and recovery.** The recovery pass settles under the session's run lock, marks pending applications unknown and never replays an action; it no longer reports uses that are not due or sessions that are fenced (FFR-006, FFR-010), and it cites the outcome observed at the application, not the workspace as it is found (FFR-016).
- **Cancellation and errors.** Settlement after a cancelled run uses a bounded detached context; memory errors are reported in `AgentResult.Cognition` and never change the task result.
- **Resource limits.** Eight uses and three conditions per record, 16 traced steps, eight checks, 64 KiB per node (of which every content revision leaves 4 KiB for a lifecycle revision and the feedback bytes of each open use, FFR-017), 32 KiB per receipt, 512 records per lifecycle batch.

## 8. Validation

Everything in this table was run by this reviewer in this task on the stated commit. PASS, FAIL and SKIP are test events of `go test -json`.

| Check | Commit | Result |
|---|---|---|
| `go build ./...` | `c1d3229` | exit 0 |
| `go vet ./...` | `c1d3229` | exit 0 |
| `go test -count=1 ./...` | `c1d3229` | 2930 PASS / 0 FAIL / 46 SKIP; 43 packages pass, 6 have no tests |
| `go test -count=1 ./...` and `go test -race -count=1 ./...` | `d4fde6d` (the head before FFR-017; superseded) | 2978 PASS / 0 FAIL / 46 SKIP each, exit 0 (`final/superseded-d4fde6d/`) |
| `go build ./...` | `c6bdfaf` | exit 0 |
| `go vet ./...` | `c6bdfaf` | exit 0 |
| `gofmt -l` on every Go file the review changed | `c6bdfaf` | clean |
| `go test -count=1 ./...` | `c6bdfaf` | 2985 PASS / 0 FAIL / 46 SKIP; 43 packages pass, 6 have no tests; exit 0 |
| `go test -race -count=1 ./...` | `c6bdfaf` | 2985 PASS / 0 FAIL / 46 SKIP; 43 packages pass, 6 have no tests; exit 0; no data race reported |
| Migration tests (package `internal/migrate`) | `c6bdfaf` | 150 PASS / 0 FAIL / 2 SKIP, normal and race |
| Privacy and source isolation tests (subset of the full run, by name) | `c6bdfaf` | 213 PASS / 0 FAIL / 4 SKIP, normal and race |
| Restart and recovery tests (subset of the full run, by name) | `c6bdfaf` | 106 PASS / 0 FAIL / 6 SKIP, normal and race |
| Review regressions, 22 files | `c6bdfaf` | 34 top-level tests, 54 with subtests: all PASS, normal and race |
| Red runs of each regression | pristine `c1d3229` for all 17; for 15 also the review head before the fix | exit 1 every time (`findings/FFR-0NN/`) |
| Negative control of the inherited fence guards (F05) | `c6bdfaf` with the pre-`44bcda9` fence read restored in a scratch copy | the deterministic guard fails, exit 1 (`probes/f05-mutant-c6bdfaf.*`); green on the unchanged code |
| `CGO_ENABLED=0 go vet ./...` for linux/arm64, linux/amd64, windows/amd64 | `c6bdfaf` | exit 0 each: a cross type-check of every package and test file, not a native run |

The 46 skipped tests are the same set in the baseline run of `c1d3229` and in both final runs. None is a cognition test, and the review added no skip: 15 are entry points of helper processes, 19 need a real debugger adapter or interpreter named by an environment variable, 4 need Linux (PID namespace, cgroup, bubblewrap), 7 are opt-in probes, replays, measurements, experiments and tooling, and 1 is a case that does not apply (a non-streaming response has no `[DONE]`). Reasons and counts: `final/full-test-c6bdfaf.summary.txt`.

Not run by this reviewer, quoted from the candidate's own report only: the Linux ARM64 runs of earlier commits and the historical review closures. They say nothing about `c6bdfaf`.

## 9. Platform limits

Native runs: macOS 27.0.1, Apple M1 Pro (arm64), Go 1.26.5, CGO enabled, `go-sqlite3` v1.14.49. Linux and Windows were not run natively on `c6bdfaf`. A cross type-check of every package and test file passes for linux/arm64, linux/amd64 and windows/amd64 (§8); nothing was linked or run there, and it is not a native run. The Windows worker sandbox stays unsupported (`evalruntime/signal_windows.go`). No real model was called: providers are scripted, so model-side behavior (whether a model chooses to recall, adopt and apply) is not measured.

## 10. Answers

1. **Do the seven domains work together?** Yes for native records and Experience: one scope, time, evidence, provenance, lifecycle, privacy and revision contract is enforced by one Store, and a use passes through attention, verification, learning, belief revision and governance on the same record. With one limitation: maintainer candidates are a parallel family under the same evidence and privacy authority (FFR-D2).
2. **Is the experience-learning loop correct?** On the candidate, no: false successful uses, a wrongly validated lesson, wrong counterexamples and lost counterexamples were reproducible (four P1 and eight P2 on Experience records). On the review tip the loop is correct for every case I could construct. Remaining, documented: an outcome that held before a faithful application still counts (FFR-D1), exact-action reuse, at most eight uses per record.
3. **Is memory management unified?** Yes for native records: one lifecycle, one protection rule, one capacity pass, per-reader access apart from outcomes. Maintainer candidates have their own applicability lifecycle (FFR-D2).
4. **Is the Governor usable?** On the candidate and the review tip, yes, on the normal SQLite store through the public API, pinned by a test. On `main`, no (10.7).
5. **Is there an architecture-level problem?** No blocking one. One structural limitation (two record families, FFR-D2) and one scale bound (uses stored inside the record, FFR-D3), both recorded for an Owner decision.
6. **Are there unfixed P0, P1 or P2?** None known. Seventeen findings are fixed and self-verified; four P3 observations are recorded with the decision they need.
7. **What is already in `main`?** Harness, Task State, sessions and notes, context graph and thread recall, check approvals and bindings (ADR-0017), source fences and privacy deletion (ADR-0018), and the Governor's Analyze/Plan/Validate entry points. Those entry points do not work on `main`: `CognitiveGraphSnapshot` requires the tables `graph_nodes` and `graph_edges`, which no Go migration creates and no Go code writes (the migration layer lists them as legacy Node-runtime tables). Probed on a `git archive` of `00344b5` (`probes/main-governor-00344b5.stdout`): on a store that Go alone created, all three return `cognitive graph tables are not initialized in shared SQLite store`; `main`'s own Governor test passes because it creates those tables by hand.
8. **What exists only in the candidate?** `internal/knowledge`; migrations 0014–0017; the maintainer and `read_knowledge`; the native cognitive graph; Experience extraction, validation, recall, adoption, application, feedback and revision; `assess_cognition`; Governor Apply and the memory lifecycle; the five cognitive tools; and everything this review fixed.
9. **Is it ready for final integration?** It is ready for the next gate, not self-certified. Conditions: (a) a reviewer who did not write them verifies the 18 review commits: the 17 fixes, together 418 added production lines, each with a red and a green receipt, and one test-only commit; (b) the Owner decides on merge; (c) a native Linux run of `c6bdfaf` if Linux is a release platform; (d) a development store the candidate wrote is recreated before it is used with the review tip, unless the Owner asks for its conversion (FFR-D4). The base to integrate is the review tip, not `c1d3229`: the candidate as frozen has four P1 defects.

## 11. Recorded, not fixed

- **FFR-D1** (P3, needs a product decision). An outcome that already held before a faithfully reproduced application is recorded as a success. Requiring a not-held → held transition would exclude guard-type outcomes such as "the tests still pass". Statistics are informational and marked `causal_benefit_unmeasured`.
- **FFR-D2** (P3, architecture). Two record families, see §6.
- **FFR-D3** (P3, scale). At most eight lifetime uses per record, fewer for a large record, and 64 KiB per node, by design (ADR-0023). FFR-017 makes the byte bound safe and does not lift it; lifting it means moving uses to their own table, a schema change.
- **FFR-D4** (P3, compatibility). A store the unmerged candidate wrote is not converted. A record created there can be extracted again after a deletion erased it. A record the candidate validated over a failed origin action makes recall, Governor analysis and memory maintenance of its project fail as corrupt until the origin session of that record is deleted. Both were probed (`probes/candidate-store-d5542d4.*`). Such a store exists only in development; a store migrated from `main` is not affected.
- `gofmt -l .` lists 17 files (`final/gofmt-whole-tree.txt`). All 17 are byte-identical to `main`: the drift is inherited, not the candidate's or the review's, and is left as found.
- A check for an outcome that appears with a delay must wait for it itself; the outcome is observed once, when the last action has settled.

## 12. Evidence

Workspace `bench/evidence/cognitive-core-fable-full-review-20261114/` (outside Core Git, not version-controlled):

- `baseline/` — build, vet and full test on `c1d3229`.
- `findings/FFR-0NN/` — red and green runs per finding; `commits/` — one record per fix commit.
- `affected/` — affected-package suites during the review.
- `probes/` — the class probes as text and their outputs.
- `final/` — environment, commands, build, vet, full test, full race, category counts and Git state on the frozen tip; `final/superseded-d4fde6d/` — the same validation of the head before FFR-017, kept as run.
- `SHA256SUMS` — digests of every file above.
