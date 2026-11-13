# Cognitive Core — Security and Trust Boundaries

Status: 2026-11-13 unmerged completion candidate; [current implementation and validation](../reports/cognitive-core-final-completion-2026-11-13.md). Historical test names below retain their provenance. Local single-user Core with an untrusted model; no production/network-authentication threat model is claimed.

## 1. Trust boundary

- **Trusted:** Core's own records (sessions and their versions, terminal statuses, fences, derived origins, eval results), the host's check approvals and claim bindings (ADR-0017), git blobs of the workspace.
- **Untrusted:** every byte the maintainer model writes (claim, subject, rationale, source refs, paths, assertion, any extra field), and every source transcript it reads (prompt injection is data, not instruction).
- **Separation of authority:** execution agents may propose/recall/adopt/apply through the five bounded cognitive tools; none grants check approval or Governor lifecycle authority. The maintainer remains read_thread-only and cannot choose its checks. Only framework commit paths persist qualified observations. `apply_experience` must match observed actions and ordinary permissions/hooks/budget; model text cannot promote itself or widen scope.

## 2. Maintainer permission matrix

| Capability | Main agent run | Maintainer run | Enforcement / test |
|---|---|---|---|
| `read_thread` | policy-dependent | yes (agent view: fenced/withheld content hidden) | allowlist; `TestCognitionVerticalSlice` |
| `read_knowledge` | yes when cognition enabled | **no** | not registered for maintenance; `TestCognitionMaintainerToolBoundary` |
| `bash`, `write_file`, edits | per policy | **no** | read-only policy + allowlist → loop refusal “not allowed”; `TestCognitionMaintainerToolBoundary` |
| DAP (`debug_*`) | per host catalog | **no** | allowlist; same test |
| MCP (`mcp__*`) | per config | **no** (catalog not discovered) | `disableMCP` + allowlist; same test |
| Eval (`eval_*`) | per host admission | **no** | allowlist; same test |
| Subagents | per delegation | **no** | `DelegationNone` + allowlist; same test |
| Run checks / approve checks / bind claims | completion gate / host only | **no**; Core runs the host-bound checks after the run | `runMaintainer` is `Interactive` (no completion gate); `adoptCandidates` |
| Host Start/resume of the maintenance session | — | refused | `prepareRun` purpose binding; `TestCognitionMaintenanceSessionIsolation` |
| Host creating a `purpose` session | — | refused | core-owned metadata key; same test + `TestFailMaintenanceKeepsDirtyAndPurposeIsCoreOwned` |
| Agents reading/recalling the maintenance session | — | refused / excluded | `readThread`, `rankThreads`; `TestCognitionMaintenanceSessionIsolation` |

## 3. Promotion is never self-asserted

| Attack | Result | Test |
|---|---|---|
| Model sets `"status":"qualified"` | entry rejected (unknown field) | `TestCognitionRejectsUntrustedProposals`, `TestAuditFalseClaimPassingUnrelatedCheck` |
| Maintainer names `checkIds` (any value) | entry rejected (unknown field) | both |
| Assertion with an extra field (e.g. `"predicate":"not_equals"`) | entry rejected | `TestAuditFalseClaimPassingUnrelatedCheck` |
| Source ref outside the batch / zero session / forged digest of a real session | rejected | `TestCognitionRejectsUntrustedProposals` |
| No source ref | rejected | same |
| False free-text claim while an approved check passes | no check can be attached; `candidate_only`, never `current` | `TestAuditFalseClaimPassingUnrelatedCheck` |
| Assertion value no binding covers (7070), or a kind with no binding | no binding matches; `candidate_only`, no check runs for it | same |
| Binding approved for another scope | `candidate_only` | same |
| Assertion bound, bound check fails (9090 bound to a failing `port9090`) | `candidate_only` with a `failed` adoption observation | `TestCognitionRejectsUntrustedProposals`, `TestAuditFalseClaimPassingUnrelatedCheck` |
| Binding revoked, superseded, unapproved check set, or check redefined after adoption | reader: `candidate_only` (“no host-approved binding supports … now”) | `TestAuditFalseClaimPassingUnrelatedCheck` subtests, `TestCrossFindingRegression` |
| Bound check configured after the host approved the set | never run; `candidate_only`, no observation | `TestCognitionRejectsUntrustedProposals` |
| Free-text claim (procedure, decision, …) | `candidate_only` | same |
| Path not in the workspace / escaping it | rejected | same + `TestCleanPath` |
| Non-JSON answer | one rejected audit row; sources consumed, not retried forever | `TestCognitionRejectsUntrustedProposals` |
| Oversized answer / too many candidates | whole answer rejected | `TestParseOutputIsStrictPerEntry` |

## 4. Invalidation and withdrawal

| Event | Applicability | Test |
|---|---|---|
| Unrelated file change | `unknown` until a Start reruns the bound check for its run → `current` for that run | `TestCognitionVerticalSlice` |
| Indirect dependency change (file the check reads, not cited) | `unknown` → rerun fails → `stale`; history keeps both passes | same |
| External environment / network / clock dependency | A process PASS is never reused by another run or a host read. An inherited process-environment change or side-effect tool ends same-run reuse immediately; a newer non-passing result of the same check at the same state, recorded by a concurrent run, ends it too (`unknown`); other unobservable dynamic dependencies retain the bounded 5-minute same-run window | `TestIndependentFreshAtUse`, `TestCognitionRunEnvironmentChangeRevokesPass`, `TestFableConcurrentRunsDoNotOutliveANewerFailure` |
| Source session corrected (new run, new answer) | old candidate `stale` (`sources[].changed`); the new version is maintained again; a batch whose source changed mid-pass is refused (`ErrSourceChanged`) | `TestIndependentSourceVersion`, `TestCommitRefusesASourceChangedAfterItsBatchWasRead` |
| Cited file changed / deleted | `stale` / `source_missing`; restored file → `current` | `TestCognitionSourceMissingAndCitedChange` |
| Source session row gone | `source_missing`, not indexed | same |
| Source deleted via Engine / revoked (`RevokeSource`) | fence reaches maintenance session and every run that read the candidate → `withdrawn`, text no longer served | same + `TestCognitionConflictAndWithdrawal` |
| Two candidates disagree on a subject | Decided by host evidence (`knowledge.EvidenceLevel`): a candidate is `conflict` only against a different claim with at least its own level. Equal levels are both `conflict`, Core does not pick; an unsupported (`candidate_only`) claim reads `conflict` and never moves a host-supported fact off `current`/`unknown` | `TestCognitionConflictAndWithdrawal`, `TestFableVerifiedFactIsNotContestedByUnverifiedClaims`, `TestCognitionFiveAssertionSupportBoundaries` |
| Private (privacy-fenced) source | recorded `sensitivity=private`; `fixBatch` skips it and `Validate` refuses a private ref | unit: `TestValidateKeepsOnlyReferencesCoreIssued` |
| Privacy deletion of a source a maintainer already read | Core cancels derived runs, acquires their cross-process run locks, blanks candidate text/paths/identities, purges maintenance content and runtime events, rewrites inherited copies, VACUUMs and truncates WAL. Pending means not completed; repeated Open resumes safely | `TestIndependentPrivacyDeletion`, `TestCognitionPrivacy*`, `TestCognitionPurgeVacuumFailureRetainsPendingAndRetries` |
| A reader restated, in its own answer, a candidate derived from a session deleted later | The copies Core made (tool results, events) are rewritten. The reader's own model output is not; from the deletion on the reader is withdrawn as a source, is in no maintenance batch, `read_thread`/recall return none of it, its thread cannot be continued and a host page marks every row withheld. Removing that text needs a deletion of the reader | `TestFableRestatingReaderIsIsolatedByPrivacyDeletion` |
| Schema dependency outside the approved regular-file input set | Error, never version-bound support; network/external/symlink dependencies cannot inherit a workspace PASS | `TestCognitionSchemaCannotPromoteExternalUnversionedDependency`, `TestSchemaRejectsDependencyOutsideApprovedInputSet` |
| Binding revoked while a maintainer or revalidation check runs | Commit rechecks active exact support transactionally; Start rebuilds binding authority before producing its index | `TestCognitionCommitRefusesRevokedSupportAtomically`, `TestCognitionStartIndexRechecksRevokedBinding` |

## 5. Availability

Maintenance failure never blocks a main run: the background trigger runs after main-run finalization, so its source version includes delivery Task State. A main run started while the maintainer is blocked completes (`TestCognitionBackgroundMaintenance`). A failed pass leaves `pending_state=failed` and the cursor unchanged. Inheritance errors are published as `cognition.inheritance.failed`; the run starts without the index.

## 6. Residual risks (not mitigated here)

- A bound check proves only what it verifies; the host's binding decides that the check verifies the assertion. A host binding a weak check to an assertion is trusted as given. No semantic verification of free text exists; free text never becomes `current`.
- Structured assertions are displayed only as Core's exact subject/claim rendering, with no model rationale. Free-text `candidate_only` content remains untrusted data, not verified instructions.
- Start indexes are task-relevance ranked and bounded to 6 locators without claim text; selection is not correctness. Large Experience-domain scans remain a performance limitation, not permission to add another database.
- Secrets copied into a claim by the maintainer are not detected (instruction only).
- Same-UID local processes can still write the SQLite file (existing SECURITY-003 boundary, unchanged).
- Privacy guarantees cover Core-managed live/recoverable SQLite files and normal read paths, not SSD forensic erasure, backups or copies outside Core's ownership.
- A maintainer reads the whole batch: revoking or deleting any input conservatively withdraws all its output, because the answer is not separable from the lost source. The transaction that writes those fences queues the batch's other unfenced, unchanged sources again; the next pass maintains them without the lost source (`TestFableCoBatchedSourceRecoversAfterWithdrawal`). Until that pass commits their cognition is absent, never a false `current`. A revocation whose walk expanded to every session requeues nothing.
- A deletion does not rewrite model output of other sessions that restated deleted content in their own words (§4): those sessions are isolated from every later model request, not erased.
