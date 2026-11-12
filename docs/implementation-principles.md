# Implementation Principles

This guide explains how LumenCortex turns architecture decisions into production behavior. It is an implementation contract, not a claim that every planned capability is complete.

## 1. One production runtime and one authority

Go is the only production Runtime. There is one Agent execution authority, one Session store, one permission system and one lifecycle owner. Modules may have separate responsibilities, but must not create competing sources of truth. Historical Node.js code is reference material only.

**Why:** two runtimes or duplicate registries drift in permission, state, recovery and behavior.

## 2. Separate proposal, authorization, execution and evidence

A model may propose an action or interpretation. It cannot authorize itself, declare a tool successful, or turn its own text into verified evidence.

1. Model proposes.
2. Core checks identity, scope, policy and current state.
3. Host performs the authorized operation.
4. Core records the actual result and provenance.
5. A verifier checks only the claim it is designed to check.
6. Core commits the state transition.

**Why:** model output is untrusted input; authority and durable truth belong to deterministic code and real observations.

## 3. Make ownership explicit

Every durable or mutable object needs an owner and defined readers/writers: Session, Run, Worker, task, workspace, artifact, knowledge item or experience. Cross-session sharing must be explicit and source-bound. Consumer feedback must not leak to another consumer. Revoking a source invalidates dependent uses; deleting one consumer must not delete unrelated owners' data.

**Why:** implicit sharing causes privacy leaks, lease loss and cross-session corruption.

## 4. Check versions at every write boundary

For concurrent state, use expected revisions, CAS, leases or transactions. Re-read after a conflict. Never overwrite newer content from a stale snapshot. All file writes—including Eval artifact export, AST edits and LSP edits—must use the guarded Editing Engine path.

**Why:** a successful read does not grant the right to overwrite a later version.

## 5. Bound every resource

Set explicit limits for model context, tool output, concurrent Agents, child tasks, background jobs, process lifetime, memory, CPU, event queues, artifact size and retention. Enforce limits before acquiring resources; a UI-only or documentation-only limit is not enforcement.

**Why:** long tasks must not cause unbounded memory, process or cost growth.

## 6. Cancellation and cleanup are correctness

Cancellation reaches owned provider requests, tools, child runs and processes. Cleanup finishes before terminal state is published. If an external operation's result cannot be known, record outcome_unknown; do not guess success or blindly retry a non-idempotent action.

**Why:** stopping the caller is not the same as stopping the work it owns.

## 7. Persist facts, not unbounded live state

Durable history belongs in SQLite or an explicit Artifact store. In-memory structures are bounded projections for the current operation. Events are observations, not permissions or completion proofs. Rebuildable indexes are not authoritative state.

**Why:** restart must recover committed facts without pretending ephemeral processes or heaps survived.

## 8. Treat paths, projects and external inputs as untrusted

Canonicalize workspace identity, enforce path confinement, inspect symlinks component by component, and gate project configuration by explicit trust. Secrets are references resolved at runtime, never copied into logs or model context.

**Why:** repository content and tool arguments are user-controlled inputs, not trusted policy.

## 9. Fail closed when authority or evidence is missing

If scope, source, identity, permission, resource isolation or verification is unavailable, refuse the operation or return an explicit unknown state. Do not silently downgrade security or turn missing evidence into success.

**Why:** safe refusal is preferable to an unprovable state transition.

## 10. Tie tests to real contracts

A regression test should fail against the old or deliberately wrong behavior, then pass against the fix. Test real public call paths where possible. Keep PASS, FAIL and SKIP separate; state OS, architecture, commit and exact command. Cross-compilation is not native runtime validation.

**Why:** tests prove only the behavior they actually exercise.

## 11. Separate architecture from implementation status

Label each capability as implemented in main, partial, candidate-only, planned or unverified. A diagram, Node reference implementation, local branch or isolated test does not establish a released Go capability.

**Why:** documentation must match what users can actually run.

## 12. Cognitive Core rules

- Preserve original episodes and evidence; summaries are derived views.
- Distinguish working, episodic, semantic and procedural memory.
- Recency, frequency, utility, confidence and evidence strength are different signals.
- Reuse experience only when its preconditions are satisfied.
- Model-generated experience is a candidate until validated.
- Unknown, conflicting and stale knowledge must remain explicit.
- Storage, recall and summarization are not learning.

## 13. Documentation is part of delivery

When behavior changes, update the canonical component document, Chinese counterpart, evidence and readiness status. Keep README short. ADRs record durable decisions; reports record what happened in a particular implementation or review.

## Review checklist

- Who owns this state? Who may read and mutate it?
- What proves the operation was authorized?
- What happens if the process crashes at each boundary?
- Can a stale writer overwrite newer state?
- Are resource limits enforced before launch?
- Can the operation be cancelled and cleaned up?
- What evidence proves the claim?
- Is the feature in Go main or only a candidate?
- Have English and Chinese docs been updated together?
