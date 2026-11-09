# Agent Runtime Debugger

This debugger controls the Agent loop, not the program being developed. Program debugging uses DAP separately.

## Ownership and commands

`AgentOptions.Debug` sets initial breakpoints and `pauseOnStart`. Every prepared live run has a controller attached to its existing tracked lifetime; no second session registry, durable process snapshot, or hidden reasoning state exists. `Engine.AgentDebugSnapshot` returns the current run, revision, safe boundary and breakpoints. Inactive runs cannot resume; Core restart requires an ordinary new run.

`Engine.ControlAgentDebug` is a host-only API, not an Agent tool. Commands include `pause`, `continue`, `step_agent`, `step_model`, `step_tool`, `step_turn`, `breakpoints`, `cancel` and `close`. A command names the Session, Run and current revision; delayed controls cannot silently address a later pause/run. Breakpoint replacement is bounded to 64 and copied, never caller-owned mutable state. Cancellation and close end the original run through existing cleanup.

## Boundaries and steps

- Pause waits for the next safe execution boundary. In-flight Shell/MCP/Eval operations finish or are cancelled under their own deadlines; no goroutine or operating-system process is suspended.
- Step Agent advances one observable boundary.
- Step Model advances one provider request/response, including a failed request, and pauses before another request. The successful answer is stored before pausing.
- Step Tool advances one tool result. That result and its execution provenance are committed before pausing, so cancellation/resume does not replay it.
- Step Turn advances the remaining model/tool group and pauses before the next provider request, or before delivery when the response completes the turn.
- Continue honors enabled breakpoints; once breakpoints disable when hit. Conditions are exact string matches on `step`, `tool`, `agent`, `failed`, `denied`, not executable code or private model content.

Supported breakpoint events: BeforeModelCall, AfterModelCall, BeforeToolCall, AfterToolCall, ToolFailure, PermissionDenied, BeforeSubagentStart, AfterSubagentEnd, WorkflowTransition, BeforeDelivery.

## Safety

Session existence, fences and persistence health are rechecked before and after a paused boundary, and on every host control request. Tool policy, tool allow/deny lists, workflow admission, lifecycle hooks and execution provenance remain authoritative after stepping. Debugger cannot supply replacement arguments, change facts, fabricate verification, or broaden policy. Delivery pause precedes verification so resuming does not reuse a verdict from before human intervention.

`BeforeSubagentStart` runs only for actual spawn/task-start tools. `AfterSubagentEnd` is observed when the child run has finished cleanup; it latches a matching breakpoint on the parent and pauses at the parent's next safe boundary. A child never blocks its parent waiting for a debug callback.

Pause consumes real wall-clock time: deadlines/cancellation/revocation end it. Breakpoints, snapshots and live controllers are not persisted as recoverable execution state. State changes enter the existing bounded Runtime Event Bus with metadata only. The Desktop renders snapshots and forwards typed commands; it never decides whether a step may execute.

Revocation observed at any model/tool/delivery debug boundary retains the terminal
`fenced` cause, rather than being relabelled as user cancellation. The same
cross-process admission fence and bystander isolation still apply.
