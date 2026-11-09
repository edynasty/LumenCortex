# Unified runtime lifecycle hooks

Core owns the lifecycle dispatcher (`internal/verify.Registry`, protocol `2.0`). `Engine.RegisterLifecycleHook` registers trusted, concurrent-safe host code. `RegisterCommandHook` uses that same dispatcher with a sandboxed external process; no second registry, discovery daemon or automatic installation exists.

## Events and enforcement

| Boundary | Events | Behavior |
|---|---|---|
| Agent run | SessionStart, SessionEnd | Start can deny before model/tool work; End observes terminal state, including cancellation and panic. A resumed turn is a new run. |
| Provider attempt | BeforeModelCall, AfterModelCall | Before can deny; After observes a completed/failed attempt. No reasoning is passed. |
| Tool | BeforeToolCall, AfterToolCall, ToolFailure | Registry/schema/permission checks remain authoritative. Nested Eval callbacks use the same before-tool validator. |
| Child run | BeforeSubagentStart, AfterSubagentStart, BeforeSubagentEnd, AfterSubagentEnd | Host admission covers direct and tool-driven child starts; end brackets owned cleanup/settlement. It does not delay a child on the parent's debugger. |
| Eval | BeforeEval, AfterEval | Agent and direct host execution use the same dispatcher; neither can bypass a security denial. |
| DAP | BeforeDebugStart, AfterDebugStop | Start still needs current grants. Stop observes completed manager cleanup, not merely cancellation. |
| Workflow | BeforeWorkflowTransition, AfterWorkflowTransition | Before receives a proposed transition, never a mutable live Workflow. Denial preserves the original state. Human gate approval is included even if the action does not change. After follows persistence. |
| Verification | BeforeVerification, AfterVerification | Independent verifier results remain authoritative; a hook cannot turn a failed check into a pass. |
| Delivery | BeforeDelivery, AfterDelivery | Before applies to the Agent completion boundary and independent delivery attempt; After follows a persisted delivery result. These are distinct boundaries and can share an event name. |
| Request window | BeforeCompact, AfterCompact | Applies when whole-group window construction drops transcript groups; no summarizer or hidden-reasoning persistence. |

Existing Stop, Completion, OnFailure, OnCancel and OnTimeout events remain available. A hook registered for an event is audited with ID/version/decision/timing. Durable event payloads follow the existing content-free projection, not arbitrary hook input/output.

## Decisions and authority

Only `continue`, `deny`, `modify_allowed_fields` are accepted. The former `allow` decision and Pre/Post names have no compatibility aliases.

Only an explicitly mutable, non-security BeforeToolCall hook may return argument patches, and only alongside `modify_allowed_fields` and a host-supplied schema validator. The resulting arguments are revalidated and safety hooks see the final values. Session/run identity, tool identity, permission, workflow facts and verification results cannot be patched. The tool's existing sandbox/permission policy still applies.

Security hooks always fail closed and their denials are never converted to permission by the ordinary completion-repair limit. Ordinary observers report errors/denials without changing completed work or suppressing later observers. Ordinary Stop/completion corrections retain finite existing budgets; those budgets never override independent verification failure.

Each hook has an independent deadline (5 s default, 30 s maximum); nesting is limited to four dispatcher calls on the inherited context. Audit overflow denies admission rather than skipping security hooks. Trusted Go hooks cannot be forcibly killed: they must honor context and must not retain/mutate shared objects. Third-party hooks should use the external process API; in-process host code is not an untrusted security boundary.

## External process hooks

The host explicitly supplies an absolute executable, arguments, optional read paths, events and sandbox configuration. Each invocation receives one JSON LifecycleRequest on stdin and returns exactly one JSON LifecycleResult on stdout. Input/output are limited to 64 KiB; unknown response fields, trailing JSON and invalid decisions fail. stderr is drained, not retained as potentially sensitive data.

A fresh disposable writable workspace replaces the real Session workspace. There is no inherited environment, network permission, debugger grant, Engine handle or control socket. Existing `ProgramCommand` and `shell.StartManaged` provide sandbox admission, timeout, owned-process tree cleanup and crash supervision. Output pipes have a finite drain deadline. The command deadline reserves time for cleanup.

macOS sandbox execution is exercised. Linux uses the existing verified caller-isolation requirement and refuses an unconfigured host; Linux command-hook execution is not claimed by the macOS evidence. Windows remains subject to the platform's sandbox availability. No production installation is performed.

## Evidence

Workspace evidence: `bench/evidence/cognitive-agent-execution-platform-20261108/raw/hooks-runtime-confirmed.*`, `hooks-public-smoke.*`. The former exercises real Engine writes, no-request model denial, schema/security/recursion/observation policy, same-action workflow approval, real sandboxed process protocol and timeout. The latter runs a standalone public API program: sandboxed process denial and exactly one durable hook event. All providers are deterministic fixtures; no external model requests.

Earlier failure receipt `hooks-runtime-integration.*` is preserved: same-action gate approval initially bypassed transition hooks; the corrected approval path and regression now pass. Full assembled-platform regression and independent review are separate gates.
