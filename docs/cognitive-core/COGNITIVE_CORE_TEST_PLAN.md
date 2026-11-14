# Cognitive Core — Test Plan and Coverage (T5a and Go P0)

Current evidence: workspace `bench/evidence/cognitive-core-final-completion-20261113/`, initially bound to code candidate `2151f17b33a84912892353e69c9b00f6b348a239`. [Completion report](../reports/cognitive-core-final-completion-2026-11-13.md) gives subsequent fixes, exact commands and PASS/FAIL/SKIP. Deterministic in-process providers drive real Engine/Agent Loop/tools/SQLite/check processes; no paid or real-model request. Historical results below remain bound to their own commits. Self-verification is not independent acceptance; cross-compilation is not native execution.

Review regressions, 2026-11-14: the full review added 22 test files with 34 top-level tests (`*_review_test.go` in `runtime` and `internal/knowledge`, plus `internal/knowledge/application_test.go`). They hold the regressions of findings FFR-001..017, every finding red on the unfixed candidate, and one test that pins the host Governor path on the normal SQLite store, which was already green there. Commands, results and receipts: [review report](../reports/cognitive-core-fable-full-review-2026-11-14.md) §8 and workspace `bench/evidence/cognitive-core-fable-full-review-20261114/`.

## 1. Historical T5a commands

```sh
go build ./...
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./internal/session ./internal/knowledge ./internal/migrate ./internal/agent ./internal/toolset ./internal/evaluator
go test -race -count=1 ./runtime -run 'TestCognition|TestIndependent|TestAudit|TestCrossFinding|TestRealEngine|TestResumeOnFreshEngine|TestSessionPinning'
```

The completion validation runs full-module build/vet/plain tests plus affected cognitive Race and platform checks. A failure is retained, diagnosed, corrected and rerun only on its affected boundary; a source-bound union is explicitly distinguished from a single clean full-suite command. Historical unchanged verification may be cited as history, never counted as a fresh execution.

## Completion scenarios

- `TestCompletionLearningThroughOrdinaryTools`: origin proposes after real actions; the next run recalls, adopts and explicitly applies; independent feedback, actual artifact and private Uses are checked.
- `TestCompletionFeedbackRequiresActualExperienceApplication` and application guards: unrelated read, changed conditions, substituted actions, ordinary policy and missing receipts cannot become successful reuse.
- `TestCompletionExecutedToolFailureBecomesCounterexample` and negative-origin cases distinguish observed failure from denial/interruption/partial execution.
- Lifecycle/dependency/assessment tests check protected retention, immutable history, source withdrawal, unknown/conflict handling and atomic canonicalization.
- Recovery tests inject real SQLite settlement failures and reopen the Engine; no action replay, no false success, reader-local access only.
- `TestCognitiveCompletion*` checks migration upgrade, rollback, hot-WAL missing-table refusal and downgrade semantic-loss guards.
- `go run ./cmd/lcx-experience-demo` exercises the public Engine with local tools/checks and an offline provider; it is mechanism evidence, not a model-benefit benchmark.

### Independent-review remediation

Fixed code: `eb47a7d4aa9aa1429bf5fb50d6b2483445faf07c`. `review-final-targeted` records 64 PASS / 0 FAIL / 0 SKIP test events across Runtime/Agent/Session. The original `ilr-workflow-identity-lineage-red`, `ilr-background-red`, `cc-gov-red-behavior`, intermediate missing-warning failures and compile-fixture failure remain separate.

- `TestReviewExperienceActionsObserveLiveWorkflow`: real two-action procedure; permitted transition continues, restricted tool/human gate/denied transition prevents the second effect; the next action sees committed Task State. A same-response tool cannot cross the human gate.
- `TestReviewIdenticalExperienceActionsHaveDistinctReceipts`: identical arguments produce distinct IDs; missing second read settles unknown and receives neither success nor counterexample credit.
- `TestReviewExperienceShellMustFinishInForeground`: actual background mode is refused before job creation; explicit/default foreground controls pass.
- `TestReviewNestedThreadWarningRetainsWithdrawal`: warning created after the procedure was observed is actually returned by `read_thread`; nested recipient is reached by reporter revoke/delete, with private reporter identity absent.
- `TestExperienceApplicationSettlementRequiresOrderedExactExecutions`: duplicate/reversed IDs, wrong run/arguments/outcome and false completed-unknown receipts are rejected transactionally; genuine unknown receipt can be retained without credit.
- `TestCallbackStateAndExecutionCommitTogether`: Task State CAS failure rolls back execution settlement; valid retry commits state and execution together without an unmatched tool row.
- `TestReviewGenericNodeCannotForgeCanonicalBinding` / `TestReviewCanonicalAliasCanBeRevokedAfterTargetChanges`: invalid canonical writes, existing-alias generic rewrite, target evolution, history preservation and irreversible withdrawal.
- Existing consumer privacy, original failure-counterexample, source reach, restart/recovery and Context Graph tests retain their assertions. Final build/vet/full suite/Race/platform receipts are listed in the completion report rather than inferred from this focused result.

中文验证口径：上述测试通过真实 Engine、工具、检查进程和 SQLite 证明机制与权限边界；脚本 Provider 不产生付费模型请求。原失败与各次修复结果分别保留，独立审查不冒充亲自运行测试，跨编译不冒充原生运行。

The ILR-001 closure additionally requires explicit-empty next-stage `allowedTools:[]` with nil host allowlist. `TestEffectiveAllowlistPreservesDenyAll` distinguishes nil, empty, disjoint and intersecting permissions; the real procedure asserts a durable transition, absent second effect, unknown receipt and no successful feedback. `ilr001-empty-allowlist-red` retains the two helper failures and executed forbidden second action before the `slices.Clone` correction.

## 2. §10.1 vertical slice — `runtime.TestCognitionVerticalSlice`

| Step | Assertion |
|---|---|
| 1 Agent A does a task | A's scripted run reads `config.env` and `defaults.env` and answers the port; dirty=1, cursor=0 |
| 2–3 Candidate via the Agent Loop | `MaintainCognition` runs the maintainer as an Agent Run offered only `read_thread`; the batch carries A's answer |
| 4–5 Host check → qualified observation | the host binds `port_value(service)=8080` to check `port`; the maintainer proposes that assertion (no check id); Core runs the bound check in a closed episode; one `adoption` observation `passed`; cursor=1; a second pass does nothing (no recursion) |
| 6–8 Agent B inherits and uses it | B's first request carries the index row with the locator and no claim text; B calls `read_knowledge`, gets `current`, writes `client.env` = 8080 **without** re-deriving from files |
| — | B's write moves the fingerprint, and a host read never reuses a process (`run`-class) check → host view `unknown`; the next Start reruns the check for its run → `current`; a `revalidation` observation is appended, the adoption stays |
| 9–10 Indirect dependency change | `defaults.env` (not cited, read by the check) changes → `unknown`, cited path still `unchanged` |
| 11 Rerun | Agent C's Start reruns `port`, it fails → `stale`; C ignores it and derives 9090 from the files; observations = failed, passed, passed (history kept) |
| 12 Correction | maintenance of C's run yields a corrected candidate (assertion 9090, bound by the host to check `port9090`); Agent D gets it `current` ahead of the `stale` one and writes 9090 without re-deriving |

The worker's behaviour is a pure function of what Core returned (index row, `read_knowledge` view, files), so the different `client.env` outcomes are caused by inheritance, not scripted per agent.

## 3. §10.2 negative tests

| Required case | Test |
|---|---|
| Wrong candidate | `TestCognitionRejectsUntrustedProposals` (bound check contradicts → `candidate_only` + failed observation), `TestAuditFalseClaimPassingUnrelatedCheck` (false claim beside a passing unrelated check) |
| No source | same (`sourceRefs: []` → rejected) |
| Forged source | same (zero session; forged digest of a real session) |
| Stale candidate | `TestCognitionVerticalSlice`, `TestCognitionSourceMissingAndCitedChange` |
| Source revoked | `TestCognitionConflictAndWithdrawal`, `TestCognitionSourceMissingAndCitedChange` (Engine delete fences) |
| Source missing | `TestCognitionSourceMissingAndCitedChange` (cited file deleted, session row gone) |
| Unrelated knowledge | `TestCognitionUnrelatedKnowledgeStaysOutOfTheWay` (16 qualified candidates → 6 locators, no claim/subject text) |
| Conflicting knowledge | `TestCognitionConflictAndWithdrawal`; conflict resolved by rerun in `TestCognitionVerticalSlice` |
| Forged check id | `TestCognitionRejectsUntrustedProposals`, `TestAuditFalseClaimPassingUnrelatedCheck` (any `checkIds` field → rejected) |
| Unapproved check | `TestCognitionRejectsUntrustedProposals` (bound check configured after approval, `touch` side effect absent, no observation); `TestAuditFalseClaimPassingUnrelatedCheck` (unapproved set, older definition) |
| Model self-promotion | same (`"status":"qualified"` → rejected) |
| Self-generated recursion | `TestCognitionVerticalSlice` (second pass does nothing), `TestCognitionMaintenanceSessionIsolation` (dirty stays 1), `TestTerminalStatusAdvancesDirtyRevisionForSourcesOnly` |
| Old worker commits after lease takeover | `TestLeaseTakeoverRefusesTheOldGeneration`, `TestCognitionLeaseTakeoverAcrossEngines`, `TestMaintenanceLeaseAcrossProcesses` (separate OS processes) |
| Dirty revision lost | `TestTerminalStatusAndDirtyMarkAreAtomic` |
| Cursor jump | `TestCursorCannotJump` |
| Maintenance session leaks project write | `TestCognitionMaintainerToolBoundary`, `TestCognitionMaintenanceSessionIsolation` |
| Maintainer calls DAP / MCP / Shell | `TestCognitionMaintainerToolBoundary` (bash, write_file, debug_start, mcp__…, eval_create, spawn_subagent, read_knowledge all refused; workspace unchanged) |
| Main agent blocked by maintenance failure | `TestCognitionBackgroundMaintenance` (main run completes while the maintainer is blocked; failing passes leave `failed`, later runs unaffected), `TestCognitionFailedPassKeepsSourcesDirty` |

Additional: `TestCommitIsAllOrNothing`, `TestConcurrentWorkersCommitEachSourceOnce`, `TestDuplicateClaimsAreNotInsertedTwice`, `TestObservationsAreDerivedFromQualifiedResultsOnly`, `TestCognitionOffByDefault`; `TestCognitionMigration*` / `TestCognitionSupportMigration*` / `TestCognitiveGraph*` / `TestCanonicalSequence*` for migration, rollback and wrong-branch refusal; `internal/knowledge` identity, dedupe and precedence tests.

## 3a. Remediation R1 regression tests (T5a independent review findings)

| Finding | Test (subtests) | What fails on `23bad7c` |
|---|---|---|
| P1-01 | `TestIndependentFreshAtUse` (9): another reader, new run revalidates, external change at the same fingerprint, unfinished revalidation, same-run reuse, mutation/window ends reuse, other scope, ≤2 revalidations, 15 s budget | an old cross-run PASS is `current` |
| P1-02 | `TestIndependentPrivacyDeletion` (3): every derived copy removed (no SQL row holds the claim), WAL reader keeps it pending, interrupted deletion recovered by another Engine | candidate text survives the deletion |
| P1-03 | `TestIndependentSourceVersion` (3): new run makes the old candidate stale, change during the maintainer's read refuses the commit, delayed maintainer of the old version cannot commit; `TestCommitRefusesASourceChangedAfterItsBatchWasRead` | a correction leaves the old candidate `current` |
| P1-04 | `TestIndependentDedupeAfterWithdrawal`; `TestSupportIdentityDecidesDuplicates` | a withdrawn claim blocks independent support |
| P1-05 | `TestAuditFalseClaimPassingUnrelatedCheck` (4 + the 11-entry main case): revoked binding, unapproved set, older definition, fingerprint change | a passing unrelated check qualifies a false claim |
| P2-06 | `TestIndependentBatchCap` (6): byte-exact cap, the review's 4×8179 B case, split without loss, failed pass after a cut, catalog with no room, two Engines draining concurrently | 33 173 B batch sent |
| combined | `TestCrossFindingRegression` (6) | — |

The review's original probes (`bench/evidence/cognitive-core-sol-independent-review-20261109/raw/probes/`) fail 6/6 on `23bad7c` (`raw/prefix/` of the remediation evidence). They do not compile against the fix: they pass a `checkIds` list to the test helper `candidateJSON`, a field P1-05 removed; the tests above restate each probe scenario against the fixed contract.

## 3b. Astra R2 regressions

| Boundary | Additional exercised scenario |
|---|---|
| Freshness | Same-run inherited environment mutation, concurrent readers, process crash, real 15-second bound, unapproved schema dependency and approved local static reuse (`cognition_freshness_r2_test.go`, `schema_r2_test.go`) |
| Source/version | Delivery finalization and host edits requeue the latest terminal source; old version/lease cannot commit (`cognition_source_r2_test.go`) |
| Exact support | Commit-time revoked binding, policy change/reapproval, Start-index revocation, deterministic public prose and all five assertion kinds (`cognition_binding_r2_test.go`, `cognition_support_r2_test.go`) |
| Privacy | Late finalization, cross-process held maintainer, SIGKILL after rows/rebuild/blocked checkpoint, repeated VACUUM failure, inherited paths/events, unrelated data preservation (`cognition_privacy*_test.go`, `cognition_purge*_test.go`) |
| Byte budget | 32768 accepted / 32769 refused with Unicode, metadata, scope and JSON escapes; failed split retries retain cursor/sources (`cognition_batch_r2_test.go`) |
| Combined/state | A correction, B→C independent support, same-version dedupe, external failure, privacy, bounded backlog; immutable observations and forbidden withdrawn/candidate-only promotion (`cognition_cross_r2_test.go`, `cognition_state_r2_test.go`) |

Final macOS ARM64 evidence: build/vet passed; full test run passed 2415 tests with 37 skips; related-package race passed 656 with 4 skips; cognition Runtime race passed 96 with no skips. Counts include subtests. All 34 historical cognition roots and six original equivalent counterexamples passed. The standalone public Engine API smoke also passed with real SQLite, Git and check processes but a scripted provider (no model request). See `TEST_RESULTS.json` and `PLATFORM_RESULTS.json` in the R2 evidence directory for exact commands, skips and initial failures.

Ubuntu ARM64 remains **BLOCKED**: the existing VM's SSH endpoint refused connections and UTM Automation was unavailable. No Linux execution is claimed. Windows AMD64 build and seven affected test-package cross-compiles passed with CGO disabled; this is not Windows runtime acceptance. The SQLite-authorizer VACUUM fault-injection file is CGO-only and was separately exercised with macOS race after adding that guard.


## 3c. Fable review remediation R1 regressions (review of `f98ea7c`)

| ID | Test (subtests) | What fails on `f98ea7c` |
|---|---|---|
| FABLE-01 binding × conflict | `TestFableVerifiedFactIsNotContestedByUnverifiedClaims` (2): a contrary assertion whose own bound check failed, then the fact going stale and its correction being adopted; an unbound contrary value and a free-text claim on the same subject. Start index, run read and host read are asserted; the unsupported claims read `conflict` | the verified fact reads `conflict` for every reader |
| FABLE-02 withdrawal × dedupe × cursor | `TestFableCoBatchedSourceRecoversAfterWithdrawal` (5: a revoked source; a source changed since the batch is maintained at its new version only; a reader of a withdrawn candidate stays withdrawn while its batch neighbour recovers; a privacy deletion; an interrupted privacy deletion queues the source once); `TestFableProcessingKeysFollowFencedPasses` | 4 of 5 subtests: the co-batched source is never maintained again (backlog 0), its knowledge stays withdrawn for good; the changed-source subtest is a control and passes |
| FABLE-03 privacy × inheritance (boundary) | `TestFableRestatingReaderIsIsolatedByPrivacyDeletion` | passes on `f98ea7c`; pins the stated boundary |
| FABLE-04 freshness × concurrent runs | `TestFableConcurrentRunsDoNotOutliveANewerFailure` | run X reads `current` after run Y recorded a failure at the same state |

`TestCognitionFiveAssertionSupportBoundaries` asserted the `f98ea7c` conflict rule (the supported assertion reads `conflict` next to an unsupported contrary value); it now asserts the evidence-level rule for both sides: the supported assertion `current`, the contrary value `conflict`. Commands, raw output, exit codes and platform results of this round are in `bench/evidence/cognitive-core-t5a-fable-review-remediation-20261110/`; the Astra R2 statements above are that round's and are left as written.

## 3d. Second Fable review regressions (review of `cbe3a4e`)

| ID | Test (subtests) | What fails on `cbe3a4e` |
|---|---|---|
| REVIEW-F02-01 withdrawal × queue | `TestFableRequeueOfCoBatchedSourceIsIdempotent` (7: fences of one batch queue a waiting source once; a fenced recovery batch queues the source again, once; a new version of a waiting source is queued and maintained; a pass in flight does not consume the row a fence relies on; concurrent fences queue the source once; the waiting row survives a restart and is not doubled after it; a fence that cannot queue the source is not written) | 6 of 7: a waiting source gets one more dirty row per fence (2, 3 and 8 rows observed). In the failed-transaction subtest the refused fence is rolled back on `cbe3a4e` as well; its closing repeated revocation fails like the others. The new-version subtest is a control and passes |
| REVIEW-F01-02 evidence level × window | `TestFableReadConflictIsDecidedWithinTheSubject` | after 200 newer unverified candidates of other subjects, two equally supported contrary facts each read `unknown` by id instead of `conflict` |

Commands, raw output, exit codes and platform results of this round are in `bench/evidence/cognitive-core-t5a-fable-review-and-remediation-20261110/`.

## 3e. Astra final audit regression (review of `e2c69c6`)

`TestExperienceContextCorruptSelection` covers origin-owned, consumer-adopted and unrelated Experience records, with both malformed JSON and missing ownership fields. `ReadThread` and `GetContextGraph` select membership from committed provenance before decoding: unrelated damage stays outside the narrow read; selected damage returns `session.ErrCognitiveCorrupt` and no partial projection. Reads leave the database unchanged. The malformed-JSON fixture disables SQLite CHECK constraints on its injection connection only.

CF-ASTRA-001 failed five of six subcases before the fix. The complete `TestExperienceContext` group passes normally and under Race (13 PASS, 0 SKIP each); receipts are in `bench/evidence/cognitive-core-astra-final-audit-20261112/raw/`. This is implementation-side validation, not independent acceptance.

CF-LINUX-001 additionally covers a valid private HOME below a `TMPDIR` containing a single quote. The earlier `repr()`-string oracle failed on this path despite correct isolation. `TestPythonEnvAllowlistHidesHostSecrets` now decodes the worker's JSON and requires both inherited secret/socket values to be null and HOME to equal the kernel-private path, not host HOME. Normal and Race quoted-path runs pass; no product boundary, skip or assertion is weakened.

CF-OWNER-001 extends the same Unix-only DAP integration boundary to nine tests in `internal/debugger` and `internal/debugger/dap`, including the process-census consumer of the sandbox fixture. The former Runtime-only compilation missed these packages. Their real-process fixtures depend on the existing Unix sandbox/LLDB/signal helpers; protocol and registry tests remain portable. Windows discovery uses the real test executable instead of the Unix shell fixture, without enabling unsupported sandbox launches. The acceptance check now cross-compiles every package's tests with `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -exec=/usr/bin/true -run='^$' ./...`; the executor deliberately does not run Windows binaries. Native Unix test bodies and existing opt-in requirements are unchanged.

CF-ASTRA-002 exercises the existing cancellation, check-definition and concurrent Work Unit freshness regressions with an apostrophe in `TMPDIR`. The check fixtures now quote POSIX path arguments before encoding the complete JSON command; the constraint-start waiter is cancelled and joined during test cleanup instead of calling Fatal from an abandoned goroutine. The unchanged behavioral assertions pass in all 21 selected test/subtest identities, plain and Race. This is a test-only portability correction, not a change to check execution or knowledge promotion.

## 3f. Fable final review regressions (review of `61a1d31`)

Implementation-side validation by the reviewer who found and fixed the defects; not independent acceptance. Receipts are in `bench/evidence/cognitive-core-fable-final-review-20261112/raw/`.

| ID | Test (subtests) | What fails on `61a1d31` |
|---|---|---|
| FR-001 source run | `TestCognitiveSourceRunIgnoresAnAbortedAutoStart`, `TestMaintenanceSourceRunIgnoresAnAbortedAutoStart`; control `TestExperienceConsumerRunIgnoresAnAbortedAutoStart` | after a cancelled auto-effort Start that wrote nothing, the graph reference of the unchanged source names the judge's run and its validated Experience is no longer current; a requeued co-batched source is queued under that run. The control passes before and after: it pins the definition the fix adopts. |
| FR-003 consumer reach | `TestExperienceConsumerWithdrawalDoesNotFenceCoConsumers` (2: revoke, delete) | withdrawing one consumer fences and reports the co-consumer and the consumer of another Experience; their context graphs return `ErrFenced`. The test also requires that the record of the withdrawn consumer is unreadable for everyone and that withdrawing an origin still reaches its consumers. |
| FR-002 Hub duplicate (test only) | `TestXReviewHubDuplicateCommandIDFromConcurrentCallersRunsTheVerbOnce` | about 1 run in 300: a caller that lost the reservation is answered with the winner's recorded outcome as a duplicate, which `Command` documents and the test did not accept. A mutant that lets every caller run the verb still fails 20 of 20. |


## 3g. Owner-authorized Astra continuation (review of `546692d`)

Owner explicitly authorized Astra after the Gemini route was changed. This is not a Gemini-only review and not independent acceptance of Astra-authored code. Fix `1966cec` changes only the public Experience projection and Context provenance boundary. Evidence: `bench/evidence/cognitive-core-gemini-final-review-20261112/astra-continuation-20261112/` (workspace artifacts, outside this Git repository).

| Finding / invariant | Regression | Evidence |
|---|---|---|
| ACR-001: co-consumer feedback remains private on all public exits | `TestExperienceFeedbackDetailsArePrivateToConsumer` | Unfixed `546692d` fails; checks two consumers, both public node reads, history, feedback replay, duplicate extraction, revalidation, revision and reopen. Durable complete feedback is preserved. |
| ACR-002: origin-only thread read does not subscribe to co-consumers | `TestExperienceOwnThreadReadDoesNotSubscribeToCoConsumers` | Unfixed `546692d` fails for revoke and delete; real `read_thread` execution, surviving consumer context and origin revocation asserted. |
| A consumed counterexample warning retains withdrawal provenance | `TestExperienceCounterexampleWarningRetainsWithdrawal` | Real failed outcome and real `read_thread` ingestion; reporter identity absent from the locator, warning reader reached by reporter withdrawal. |

`raw/acr001-002-red.*` preserves the original failures. Targeted ordinary verification passed 41 tests; later Race passed 46 with zero skips and no race warning. Full and platform receipts bind the committed source separately; cross-compilation is not native Windows execution.

## 4. Performance measurement (§10.3)

Measured once with a throwaway harness (deleted after use) over the same fixture; results in `PERFORMANCE_RESULTS.json` of each evidence directory. Scripted providers make model latency ≈0, so the numbers measure Core overhead (SQLite, git blob/fingerprint, check processes), not real maintenance cost.

Remediation R1 (macOS arm64; run times are medians of 7, the rest single samples): a top-level run takes ≈160 ms with cognition off or with no candidates, ≈530 ms with one qualified candidate bound to a process check, because P1-01 makes every Start rerun such a check for its own run (one `revalidation` observation per run). A maintenance pass over 8 sources ≈435 ms; ten sources with ≈9 KB answers each (`largeBacklog`) drain in 3 passes with serialized inputs of at most 29 987 B (cap 32 768); a privacy deletion with four derived candidates, two derived maintenance sessions and a reader copy ≈32 ms.

## 5. What the gate does not prove

Real-model candidate quality or task-success improvement; unstructured free-text truth; Desktop exposure; production operation; Windows runtime acceptance without a Windows host; an independent Sol review. Cross-builds are not runtime PASS. SQLite cleanup is not physical SSD erasure.
