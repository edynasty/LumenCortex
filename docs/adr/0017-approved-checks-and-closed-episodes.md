# ADR-0017: Approved checks and closed episodes (CHECKS-001)

- **Status:** Proposed (approval part implemented by T4-S1; the closed-episode part is T4-S2's). Changing
  the default for every user needs owner decision O-4 before it reaches `main`.
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
  again after; when the set or the epoch changed in between, no verdict passes.

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

### ConstraintBinding is approved on its own

`Engine.ApproveConstraintBinding` records that a constraint (ref and digest) may be satisfied by one
configured check definition (id and its current spec digest) in a scope; the binding digest covers all
five. Approvers are `user`, `team-decision` and `experiment-freeze`. Approving checks never approves a
binding and approving a binding never approves the check; T4-S3 requires both at use. No tool or agent
path reaches either API: a maintainer or an executing agent can only propose.

### One entry for every host

`Engine.CheckApprovalState` shows the current set, each definition as unchanged/added/changed against the
newest approval, the removed ones, the previous approval and the bindings. `lcx-go checks status` and
`lcx-go checks approve <set-digest>` print and call exactly these; Desktop will call the same methods
(T7-S1). `Engine.RevokeApproval` revokes the set or one binding.

## Consequences

- With no approved set, a non-interactive completion gate with required checks gets `spec_unapproved`
  verdicts instead of running them, and a person cannot accept them. This is the default change O-4
  decides. The dedicated exit for it, the set-level gate that also covers an emptied set, and the
  interactive behaviour are T4-S2's.
- An approval covers definitions only. It does not prove that `./verify.sh`, the tests or the
  dependencies a check runs were not changed by an agent, and it is no isolation against a same-UID
  process with a full shell (R0 boundary).
- Limits of the write detection: a change and its revert inside one shell command is invisible to the
  pre/post snapshot; a write outside the workspace (the global configuration file) produces no file edge;
  a crash between a tool's write and its commit records no edge. In each case a changed definition still
  changes the digest and stays unapproved; only a write followed by an exact revert escapes revocation.

## Verification

`internal/evaluator` (`TestSatisfies`, `TestSpecAndSetDigests`, `TestCheckVerdict`,
`TestApprovalGatesRunsAcceptanceAndReuse`), `internal/session` approval store tests and `runtime` engine
tests (P3 reproduction, deletion/optional/empty set, write-then-revert, old acceptance, old trust, stale
digest, epoch monotonicity, CLI and Engine through one predicate). Report:
`docs/reports/cognition/t4-s1-20261026.md`.
