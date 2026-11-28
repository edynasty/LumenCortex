# Context Space and Cognitive Core

Load this when a change touches context construction (window, projection, Task State, compaction, receipts, notes recall, skills snapshot) or the Cognitive Core (knowledge, maintenance, governor, experience, run posture, admission, settlement). Both are persistence + lifecycle areas: the reliability rules (`reliability.md`) apply too.

## 1. Context Space (ADR-0024; ADR-0010 for notes)

Invariants and where they are tested:

| Invariant | Test |
|---|---|
| Window build is deterministic and bounded; groups stay paired; overflow and capacity retry behave | `internal/window/window_test.go` `TestBuildProperties`, `TestE5Regression`, `TestGroupsUnpaired`, `TestOverflow`, `TestCapacityRetryHalvesTranscript`, `TestEstimateDeterministic`; `internal/window/estimate_test.go` `TestReserveTokensForBytesGoldenAndMonotonicity` |
| Candidate projection, isolation, governance; skills snapshot lifecycle | `runtime/context_space_stage1_test.go` (`TestT5_…`, `TestT10_SkillSnapshotLifecyclePaginationAndConcurrency`), `runtime/context_space_t3_test.go` `TestT11_CandidateProfileIsolationAndGovernanceInvariants`, `TestT3B_CausalAuthorshipConcurrencyAndIgnoredFilesMatrix` |
| Crash/veto/reconcile/resume keep a monotonic chain | `runtime/context_space_t4_test.go` `TestT4_MultiCallVetoCrashReconcileResumeAndMonotonicChain` |
| Agent-loop stages (budget guards, 8K clamp, receipts) | `internal/agent/context_space_stage2_test.go`, `internal/agent/context_space_stage3_test.go` |
| Isolation between sessions/children: one run's notes, state or files never appear in another's context unless delegated | the T11 isolation matrix; add a case for any new context source |
| Hidden reasoning never enters durable context or recovery | `internal/migrate/migrate_test.go` `TestNoReasoningAtRest`, `runtime/models_test.go` `TestHiddenReasoningIsNeverPersisted`; Core `AGENTS.md` requires a regression for every change to persistence, compaction, handoff, context construction or recovery |
| Prefix stability of what the model sees | `prompt-tools-cache.md` §4 |
| Report equals run (`EffectivePrompt`) | `runtime/prompt_test.go` `TestEffectivePromptMatchesARealRun`; known gap: the report does not reflect compaction/receipts/relaxation (F-05 / EVO-09) — do not cite it as proof of those modes |

Minimum for a Context Space change: L2 = changed package + `internal/agent` + `internal/session` + `runtime` group *Context* (`test-matrix.md` §3), plus `-race` on the T3B/T4/T11 selection when concurrency or lifecycle code changed:

```bash
env -u GOFLAGS go test -race -count=1 -timeout=45m -run '^(TestT3B_|TestT4_|TestT11_)' ./runtime
```

A new context source (a new message origin, slot or projection mode) must be registered with the receipt boundary (EVO-11) and get an isolation case and a prefix case.

## 2. Cognitive Core (ADR-0023; `docs/cognitive-core/`)

Canonical scenario list: `docs/cognitive-core/COGNITIVE_CORE_TEST_PLAN.md` (vertical slice `TestCognitionVerticalSlice`, §10.2 negative tests, completion and review regressions). Reuse it; do not invent a parallel matrix.

Invariants that always apply to a Cognitive Core change:

1. **Persistence is real**: assert after close/reopen of the Engine on the same state; inject a real SQLite failure inside settlement and assert nothing partial is visible (`TestCallbackStateAndExecutionCommitTogether` pattern).
2. **Permissions only narrow**: posture, admission and resume never grant more than the run's contract and the current durable state. Known conflict: candidate test `runtime/cognition_posture_test.go` `TestCognitionPostureResumeReAdmitsCurrentDurableState` *(candidate)* asserts resume may widen off→on, against frozen decision D-06E4 (EVO-04, Owner decision EV-16 pending). Do not "fix" it by editing the assertion alone; it changes only with that decision.
3. **State transitions**: every status (`candidate_only`, `current`, `unknown`, `stale`, withdrawn) has a test for entry and exit; the cursor cannot jump; dirty revision and terminal status are atomic (`TestCursorCannotJump`, `TestTerminalStatusAndDirtyMarkAreAtomic`).
4. **Untrusted proposals are rejected**: forged sources, check ids, self-promotion, recursion (`TestCognitionRejectsUntrustedProposals`, `TestAuditFalseClaimPassingUnrelatedCheck`).
5. **Lease/generation fencing across processes**: an old worker cannot commit after takeover (`TestLeaseTakeoverRefusesTheOldGeneration`, `TestMaintenanceLeaseAcrossProcesses`).
6. **Recovery does not replay actions** and does not convert unknown into success.
7. **Migrations** of cognition tables round-trip and refuse partial states (`internal/migrate/cognition_test.go` `TestCognitionMigrationRoundTrip`, `TestCognitionMigrationFailureRollsBack`); see `reliability.md` §1.

Minimum for a Cognitive Core change: L3 = changed package + `internal/session` + `internal/knowledge` + `internal/migrate` in full, `runtime` group *Cognitive*, and the historical cognitive race set:

```bash
env -u GOFLAGS go test -race -count=1 -timeout=20m ./internal/session ./internal/knowledge ./internal/migrate ./internal/agent ./internal/toolset ./internal/evaluator
env -u GOFLAGS go test -race -count=1 -timeout=45m -run 'TestCognition|TestIndependent|TestAudit|TestCrossFinding|TestRealEngine|TestResumeOnFreshEngine|TestSessionPinning' ./runtime
```

Candidate-only coverage *(candidate `7c032ae`)*: `runtime/cognition_posture*_test.go` (incl. two-process `cognition_posture_process_test.go`), `internal/session/run_posture_test.go`.

`go run ./cmd/lcx-experience-demo` is mechanism evidence with an offline provider, not a model-benefit benchmark. Model-benefit claims need BENCH with Owner authorization.
