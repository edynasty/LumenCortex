# ADR-0017: Approved checks and closed episodes (CHECKS-001)

- **Status:** Proposed (approval part implemented by T4-S1, closed episodes and the set-level gate by
  T4-S2, constraint use by T4-S3). Changing the default for every user needs owner decision O-4 before
  it reaches `main`.
- **Date:** 2026-10-26
- **Owners:** project maintainers
- **Supersedes:** none
- **Superseded by:** none

## Context

A configured check is a command or schema in `lumencortex.json`. The completion gate, work units, a
person's acceptance and the evidence rule (ADR-0006 D2) all accept a passed runtime result "at the current
fingerprint". The fingerprint covers the tree and part of the definition; it says that *this* definition
gave *this* result on *this* tree, not that anybody agreed to the definition.

A run can rewrite the definition. The Opus review reproduced it (`opus-review/final/run.json`, P3): a run
changed `exit 1` into `true`, and the next run on the same thread passed the completion gate with
`passed/runtime`. Emptying the set, deleting the only required check or making it optional passes the gate
the same way, since the gate enumerates required checks from whatever the configuration says now.

## Decision

### Approval is host state, per project, by epoch

`check_approvals` (schema v6, ADR-0018) holds the approvals. `project_id` is the canonical workspace the
state is bound to (ADR-0016). Nothing is read from the repository or an imported database: an approval
does not travel with a clone.

- **Digests.** A definition's `specDigest` is `sha256:` over the canonical JSON of id, kind, command,
  schema, files, timeout (ms), required and source (`evaluator.SpecDigest`). The set's `checkSetDigest`
  covers every definition sorted by id; the empty set has its own digest (`evaluator.SetDigest`). The
  older 8-byte fingerprint hash is not reused.
- **Approving.** `Engine.ApproveChecks` takes the set digest the user was shown and fails with
  `ErrApprovalStale` when the configuration no longer has it. In one transaction it revokes the active set
  and its definitions, then writes one `check_set` row and one `check_spec` row per definition under a new
  epoch: one above every epoch the project ever had, revoked ones included. Approvers are `user`,
  `trust-flow` (only a flow that showed the whole set) and `experiment-freeze`.
- **Approved means exact.** The current set runs under epoch *E* only when the newest set approval is
  active, has the current set digest, and its definition rows are exactly the current definitions. Then
  every definition carries `ApprovalEpoch = E`; otherwise every one carries 0. Deleting a check, making
  it optional, emptying the set, or changing the trust that drops workspace checks changes the set digest
  and leaves the whole set unapproved.
- **configScope and executionScope.** The definitions and their approval are always read from the Engine
  workspace's effective configuration (`runtime.approvedChecks`), never from a session worktree; the
  evaluator runs them in the session's workspace or worktree. The run's frozen `plan.checks` only picks
  the ids the gate asks for: `runChecks.Decide` re-reads definitions and approval before deciding, and
  again when the episode closes (below).

### What an unapproved check can do

Nothing that counts. The evaluator does not run it (verdict `spec_unapproved`, nothing stored), a person
cannot accept it (`ErrSpecUnapproved`), and no stored result satisfies it. Every stored result now
carries the `spec_digest` and `approval_epoch` it was decided or accepted under; a result counts only
for the same definition under the same epoch (`evaluator.Satisfies`, `Store.DecidingEvalResult`). Rows
from before approvals have neither and never count: after upgrading, approved checks run once again.

### A run that writes a definition voids the approval

`Store.CommitToolResult` revokes the project's active check approval in the same transaction that stores
a derived file edge (T1-S4/S5) naming `lumencortex.json`, from any session or worktree of the project and
whatever the case of the path. Reverting the file later does not bring the approval back: the revoked
rows stay revoked and a new approval takes a new epoch. `CheckApprovalState.LastWrite` names the newest
such write (session, run, execution) since the newest approval, so a host can show where a change came
from.

### A pass counts only from a closed episode, and only in its own run

Every decision of a group of checks (the completion gate, a work unit's completion) is one **check
episode** (`Evaluator.Decide`). It takes a workspace snapshot, stores each fresh result `open` under a
random `episode_id` with its `pre_fingerprint`, and after the last check takes the snapshot again and
re-reads the definitions and approval, both outside any transaction. One short write transaction
(`Store.CloseEpisode`) then sets every row of the episode to `closed` (nothing changed), `scope_changed`
(the workspace changed: tracked files or untracked non-ignored files; ignored build output does not
count) or `spec_unapproved` (the definitions or their approval changed), with its `post_fingerprint`. A
cancelled episode closes `interrupted`; a crash leaves rows `open`. No episode table is added: the four
columns are schema v6's on `eval_results`.

`evaluator.Satisfies` is the one predicate for the gate, the evidence rule (AutoDone), a work unit and a
person's acceptance: a passed row at the current fingerprint under the current approval, from a
`closed` episode whose pre and post fingerprints are the current one. A person's acceptance is an
episode of its own, closed at the fingerprint the person saw, and holds across runs until the
fingerprint or approval changes. A runtime pass additionally counts only in the run that closed its
episode (the run's `Evaluator` remembers it in memory) and only until a side-effect tool completes
(`Checks.Mutated`, called for every admitted execution and every workspace-mutating result). Across runs
a check is decided again (fresh-at-use); the host's own views, built outside a run, count only a
person's acceptance. `Store.DecidingEvalResult` reads only closed rows, so an open or pre-episode row
never shadows an older closed one.

### The gate covers the set, not only its members

The completion gate first asks whether the set itself is approved (`Checks.SetApproved`): it is not when
the project had an approved set and the current one differs (a check removed, made optional, the set
emptied) or the approval was revoked. Then the gate fails with one `spec_unapproved` verdict for the
set, even when nothing is required any more. A project that never approved a set is not gated at the
set level: its required checks are `spec_unapproved` one by one, and a project without required checks
completes as before.

A model cannot clear `spec_unapproved`: a non-interactive run ends `failed` with kind `spec_unapproved`
at the gate instead of receiving a correction. An interactive turn ends with its answer as before (the
checks stay an obligation of the thread).

### ConstraintBinding is approved on its own

`Engine.ApproveConstraintBinding` records that a constraint (ref and digest) may be satisfied by one
configured check definition (id and its current spec digest) in a scope; the binding digest covers all
five. Approvers are `user`, `team-decision` and `experiment-freeze`. Approving checks never approves a
binding and approving a binding never approves the check; T4-S3 requires both at use. No tool or agent
path reaches either API: a maintainer or an executing agent can only propose.

### A constraint is decided by its bound check, fresh, at every Start

The user selects constraints for a thread with a Task State edit (`constraint`: `select`, `remove`, or
`deviation` with a reason); no edit marks one satisfied, and no tool or model output reaches the list.
Each selected constraint is a `ConstraintUse` in `task_states.json`: ref, digest, scope, and the last
decision (status, binding digest, check, spec digest, approval epoch, episode id, result seq, pre/post
fingerprint, run id).

Every Start decides them after the seeds and queued edits and before its Task State version commits,
in the session's workspace: an active binding must match the ref, digest and scope and name a check
whose definition is still the bound one (else `unbound`); the check is decided through the run's own
`runChecks.Decide`, so an unapproved definition is `spec_unapproved` and runs nothing, and only a pass
the check ran for in a closed episode, at a fingerprint the episode held, is `satisfied`. A person's
earlier acceptance or another run's pass decides nothing (`unknown`). A Start runs at most two checks
for its constraints, within 15 s in all; a constraint past either, a check stopped at the budget, an
unclosed episode or a fenced session is `unknown`, and the run starts anyway. A check bound to several
constraints runs once. A deviation the user accepted is never decided over. The pass is registered in
the run like any episode, so the completion gate takes it in that run without running it again.

The list is not rendered into model requests: pushing it is T5a-S2's, under O-5.

### One entry for every host

`Engine.CheckApprovalState` shows the current set, each definition as unchanged/added/changed against the
newest approval, the removed ones, the previous approval and the bindings. `lcx-go checks status` and
`lcx-go checks approve <set-digest>` print and call exactly these; Desktop will call the same methods
(T7-S1). `Engine.RevokeApproval` revokes the set or one binding.

## Consequences

- With no approved set, a non-interactive completion gate with required checks gets `spec_unapproved`
  verdicts instead of running them, a person cannot accept them, and the run ends `failed/spec_unapproved`
  at once. The same holds for an approved set changed since. This is the default change O-4 decides.
- A runtime pass no longer carries over between runs: every run that needs a check decides it again
  (cost: one check execution per run at the gate). Host views (`SessionChecks`, evidence after Apply)
  show a past run's pass as the newest result but not as satisfying; only a person's acceptance does.
- A thread with selected constraints spends up to two check runs and 15 s of each Start on them, before
  the run begins, and every such Start writes a Task State version. A check bound to a constraint is
  decided in the session's workspace like the gate's; a person who wants a constraint met without its
  check records a deviation instead.
- Desktop shows the new exit kind `spec_unapproved` as its raw kind until T7-S1 adds its copy.
- An approval covers definitions only. It does not prove that `./verify.sh`, the tests or the
  dependencies a check runs were not changed by an agent, and it is no isolation against a same-UID
  process with a full shell (R0 boundary).
- Limits of the write detection: a change and its revert inside one shell command is invisible to the
  pre/post snapshot; a write outside the workspace (the global configuration file) produces no file edge;
  a crash between a tool's write and its commit records no edge. In each case a changed definition still
  changes the digest and stays unapproved; only a write followed by an exact revert escapes revocation.

## Verification

`internal/evaluator` (`TestSatisfies`, `TestSpecAndSetDigests`, `TestCheckVerdict`,
`TestApprovalGatesRunsAcceptanceAndReuse`, `TestEpisodeReuseIsInRunOnly`,
`TestEpisodeScopeChangedAndIgnoredOutput`, `TestEpisodeSpecChangedDuringTheRun`,
`TestEpisodeInterruptedOpenAndLegacyRows`, `TestUndecidingRowsDoNotShadowAClosedOne`), `internal/agent`
(`TestCompletionGateEndsOnUnapprovedChecks`, `TestCompletionGateRefusesAChangedSetThatRequiresNothing`,
`TestSideEffectToolsInvalidateInRunEpisodes`), `internal/session` approval store tests and `runtime`
engine tests (P3 reproduction, deletion/optional/empty set with the set-level gate, write-then-revert,
old acceptance, old trust, stale digest, epoch monotonicity, CLI and Engine through one predicate,
`TestRuntimePassIsDecidedAgainInTheNextRun`, `TestUnapprovedRequiredCheckEndsTheRun`,
`TestConstraintIsSatisfiedOnlyThroughAnApprovedBinding`, `TestConstraintIsDecidedFreshByEveryStart`,
`TestConstraintRevalidationIsCapped`, `TestConstraintRevalidationBudgetDegradesToUnknown`,
`TestConstraintEdits`, `TestRewritingTheBoundCheckCannotSatisfyTheConstraint`), `internal/taskstate`
(`TestDecideConstraintKeepsTheUsersDeviation`). Reports:
`docs/reports/cognition/t4-s1-20261026.md`, `docs/reports/cognition/t4-s2-20261026.md`,
`docs/reports/cognition/t4-s3-20261026.md`.
