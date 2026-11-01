# ADR-0019: Execution runtime — tool timeouts, run deadlines, background jobs, timers, diagnostics, run budgets (schema v8)

- **Status:** Proposed (implemented on `rt/execution-runtime`)
- **Date:** 2026-11-01
- **Owners:** project maintainers
- **Supersedes:** none
- **Superseded by:** none

## Context

A run executes its tool calls one at a time, and the `shell` tool waits for its command to exit with no
timeout of its own (`internal/shell.Runner.Run` stops only on cancellation). Nothing tells a run how much
time it has: in TB2 W13 runs spent their budget inside one command or in exploration and were killed by
the harness (`AgentTimeoutError`) without delivering. There is no way to start a long build or test and keep
working, no durable record of commands that outlive one tool call, no timers, and the only trace is the
SQLite `journal` table (content-free by projection) plus the lossy in-process event bus.

This ADR adds the smallest set of runtime capabilities that closes those gaps, reusing what exists: the
shell runner and its process groups, the session store and its migration runner, the event bus, the
journal's content policy, `RunSupervisor`, and the configuration loader.

## Decision

### Overview

```mermaid
flowchart LR
  Loop[agent.Loop] -- tool call + ToolDeadline --> Toolset
  Toolset -- foreground --> Shell[shell.Runner\nprocess group, timeout]
  Toolset -- mode=background / job_* --> Jobs[jobs.Manager\nworkers, bounded queue]
  Scheduler[schedule.Scheduler\none timer] -- shell kind --> Jobs
  Scheduler -- agent kind --> Supervisor[RunSupervisor.Start]
  Jobs -- rows --> DB[(SQLite: jobs, scheduled_tasks)]
  Scheduler -- rows --> DB
  Jobs -- output files --> Files[(state/jobs/ID/*)]
  Jobs & Scheduler & Loop -- events --> Bus[event bus]
  Bus --> Diag[diag.Log\nredacting, async JSONL, rotation]
  Jobs & Scheduler -- slog --> Diag
  Desktop -- Engine API --> Jobs & Scheduler & Diag
```

Packages: `internal/jobs` (queue and workers), `internal/schedule` (timers), `internal/diag` (diagnostics
log). SQL stays in `internal/session` (`jobs.go`, `schedules.go`), as for every other table. The runtime
binds them in `runtime/jobs.go`, `runtime/schedules.go`, `runtime/diagnostics.go`. Limits come from the
global configuration's `execution` object (`config.Execution`, global file only, like `concurrency`).

### 0. The command tool is `bash`; the agent has no Git tools

The model-visible command tool is `bash` (`protocol.ToolBash`), and it really runs bash: `/bin/bash -lc`,
else `bash -lc` from `PATH`; on Windows Git Bash (`%ProgramFiles%\Git\bin\bash.exe`), else a `bash.exe` on
`PATH` that is not the WSL launcher (`%SystemRoot%\System32\bash.exe` runs in a Linux distribution, not the
workspace). Without bash it falls back to `/bin/sh -lc` (`cmd.exe /C` on Windows) and the tool description
says so instead of claiming bash (`shell.Bash`, resolved once per process; the description names the exact
invocation). Configured checks and the host's `SessionHandle.RunShell` keep `/bin/sh -lc` (`shell.System`): an approved
check keeps the meaning it was approved with. The toolset runs the bash tool on a copy of the host's
runner, so the shared runner's interpreter never changes.

The tool was called `shell`. `protocol.CanonicalToolName` maps the old name for every host-written list:
allowlists and denylists (`agent.Loop`, the prompt report), and workflow `allowedTools` and `tool`
conditions (`internal/workflow`), so a denylist or workflow that names `shell` keeps its meaning without an
edit. A model call to `shell` (a resumed history still shows it) runs nothing; its result names `bash` when
the run offers it, else it is the plain refusal.

`git_status` and `git_diff` are no longer agent tools: a run with `bash` runs `git` itself, with the full
command line instead of two fixed views. The host Git API (`repository.Service`, `Engine.GitStatus`/
`GitDiff`, Desktop's Git panel) is unchanged. Consequence: a run without `bash` (`read-only` and
`workspace` policy, the explore child) cannot read Git state; it reads the working tree as it is, and a host
or parent that wants a change reviewed states it in the goal.

### 1. Foreground tool timeout (RUNTIME-001)

`shell` takes `timeout_sec` (1…86400). The effective timeout is `timeout_sec` when the call gives one, else
`execution.shellTimeoutSeconds` (absent: none, the behaviour before this ADR), and in both cases at most the
time left before the call's tool deadline (§2). At the timeout the runner sends SIGTERM to the command's
process group, waits a 2 s grace, then SIGKILL; the pipes are closed after the existing 2 s wait delay, so
no orphaned process keeps the call open. The result keeps the partial stdout/stderr (head and tail) and
reports `timedOut: true`, `timeoutSec`, exit code -1, and an error text telling the model that the group
was terminated and the command's effects are unknown. A timeout is a tool result (`ok: false`,
`ToolResult.Interrupted`), not a run exit; cancellation of the run stays a run exit (`cancelled`), so the
two are distinguishable. The execution record of the call (ADR-0018) ends `outcome_unknown` for a
timed-out command: it may have changed the workspace. A call whose tool deadline has already passed is
not started.

No default timeout is shipped: the commands an agent runs range from a second to a long build, and a short
built-in default would break the long ones. A deployment that wants one sets
`execution.shellTimeoutSeconds`; a run with a deadline is bounded by it either way.

### 2. Run deadline (RUNTIME-001)

`AgentOptions.Deadline` (absolute time; CLI `LCX_DEADLINE` as a Go duration from process start or RFC 3339)
and `AgentOptions.DeadlineReserveSeconds` (default: a tenth of the budget, clamped to 30 s…120 s, at most
half the budget). The loop anchors the deadline on the monotonic clock when the run starts, so the time
left only decreases. It:

- passes every tool call `protocol.WithToolCallID` and `protocol.WithToolDeadline(ctx, deadline −
  margin)`, where the answer margin is 1.5× the longest model request of the run, at least 10 s, at most
  the reserve: tools bound their own waits by it (foreground shell, `job_wait`), so a reply still fits
  after the last tool result. A read-only subagent spawned by a tool call inherits that time as its own
  deadline;
- adds a `deadline` member to each tool result (a trailing runtime line when the result is not a JSON
  object): `remainingSec`, `reserveSec` and `usedSec {model, tools, wait}` — time in model requests
  (retries included), in tools, and in `job_wait`. Results are new transcript rows, so the cached prefix is
  not disturbed: no dynamic text enters the fixed system prompt. The stall detector reads the result
  before the report is added;
- appends one user row (origin `deadline`) when a run with a deadline starts, stating its budget, and one
  when the remaining time first falls within the reserve, asking the model to deliver. Neither stops
  work: within the reserve the model may still run short commands, bounded by the margin;
- ends the run `interrupted` with kind `deadline` when the deadline passes (a request or tool in flight is
  cancelled). External cancellation keeps kind `cancelled`.

No deadline: behaviour is unchanged.

### 3. Background jobs and the durable queue (RUNTIME-002/003)

`shell` takes `mode: "foreground" | "background"`. A background call submits a job and returns at once with
its id. Tools `job_status`, `job_wait`, `job_cancel`, `job_list` act on the jobs of the run's session. All
are `exec` permission (registered only where `shell` is). `job_wait` blocks without model calls until one of
the named jobs (or any active job of the run) ends or its timeout / the tool deadline passes.

```mermaid
stateDiagram-v2
  [*] --> queued: Submit (row committed first)
  queued --> running: worker claims (UPDATE … WHERE state='queued')
  queued --> cancelled: Cancel / run ended / Engine closed
  running --> completed: exit 0
  running --> failed: exit ≠ 0 or start error
  running --> timed_out: job timeout
  running --> cancelled: Cancel (group killed)
  running --> outcome_unknown: owner process died (found at next open)
  completed --> [*]
  failed --> [*]
  timed_out --> [*]
  cancelled --> [*]
  outcome_unknown --> [*]
```

- Every transition is one `UPDATE … WHERE job_id = ? AND state = ?` (compare-and-set): two writers never
  both move a job.
- `runner_id`/`runner_pid` name the owning manager. At open, a manager adopts queued jobs of runners whose
  process is gone and marks their running jobs `outcome_unknown`; such a job is never re-run automatically.
- Workers: `execution.maxConcurrentJobs` (default 2). Queue: `execution.maxQueuedJobs` (default 32); a
  submission beyond it is refused with a clear error, never dropped. At most 8 active jobs per run.
- Output: per stream at most `execution.jobOutputBytes` on disk (head + tail, middle cut) under
  `<state>/jobs/<job_id>/`, bounded memory per job, readable while the job runs. The database holds byte
  counts only. Retention: finished jobs older than `execution.jobRetentionDays` are deleted with their
  files; files whose row is gone (a deleted session) are removed.
- A run's active jobs: the completion gate refuses a final answer while the run still has queued or running
  jobs (correction names them: wait, read or cancel), at most twice per run, so a job that never ends cannot
  loop the run; a failed lookup does not block. When the run ends, however it ends (completed after those
  refusals included), its remaining active jobs are cancelled (reason `run_ended`, event `jobs.run_ended`):
  no job outlives the run that wanted it unobserved. Host and schedule jobs are not tied to a run and are
  never cancelled by a run's end. Engine close cancels the jobs it runs.
- Resume: a run whose session has queued, running or `outcome_unknown` jobs gets one runtime user row
  (origin `jobs`, ≤ 600 bytes, ≤ 8 jobs: id, state, command head) written by its Start transaction, next to
  the notebook index (ADR-0020 §5), so it checks the runtime's state instead of its transcript's. A
  session without such jobs gets no row.
- Events: `job.state` `{jobId, sessionId, runId, scheduleId, source, state, exitCode}`.

### 4. Timers and scheduled tasks (RUNTIME-004)

In-run delays use `job_wait` (bounded, cancellable, no model calls). Durable scheduled tasks live in
`scheduled_tasks`: kind `shell` (submits a job, source `schedule`) or `agent` (starts a run on a session with
a prompt and an explicit policy, default read-only, via the host's `RunSupervisor`); trigger `once` (at a
time), `interval` (every N ≥ 60 s) or `cron` (a standard five-field expression, `*`, lists, ranges and
steps, read in an explicit IANA `timezone` — `UTC` unless the task names one; a local time the
spring-forward gap skips does not fire, and a wall time the fall-back hour repeats fires once);
`missed_policy` `skip` (default) or `run_once`, applied when a scheduler starts to the occurrences more
than two minutes overdue; a more recent one may be about to fire in another Engine on the same database,
so it goes through the normal claim. The expression and zone are columns of the row
(`cron_expr`, `timezone`), so a task means the same thing after a restart and in another process.

- One scheduler goroutine per Engine with one `time.Timer` set to the earliest due task; no per-task timers.
- A firing is claimed by `UPDATE … SET next_run_at = <next> WHERE schedule_id = ? AND next_run_at = <read>`:
  two Engines on one database never fire the same occurrence twice.
- Overlap: when the previous firing's job or run is still active, the occurrence is skipped
  (`skipped_overlap`), never run concurrently. An agent task whose session is running (the run lock is
  held, by this process or another) is the same case: the start is refused and the occurrence is recorded
  as a skip. A one-shot task has no later occurrence to wait for, so it is not skipped: it stays pending
  and is tried again a few seconds later, and a wake-up that comes due during the run that scheduled it
  fires once that run ended. A previous firing whose job no longer exists (job retention deleted it) is
  not active.
- An agent task fires only in a process with a `RunSupervisor`; otherwise it stays due.
- A task whose stored trigger yields no further slot (a corrupted expression, an unknown zone) is
  disabled with `last_status = failed` and the reason in `last_error`, so it cannot fail on every tick.
- An agent may schedule its own continuation: the tools `wakeup_schedule {delay_sec, message}` and
  `wakeup_cancel {wakeup_id}` create and remove a one-shot `agent` task on the calling session with the
  calling run's policy as its ceiling, at most three pending per session. Both are `write` tools (they
  schedule future work) and exist only in a thread's run whose runtime bound a controller: a child run's
  lifecycle belongs to its parent, so a subagent or an execution task gets neither tool.
- Events: `schedule.changed`, `schedule.fired`.

### 5. Diagnostics log (OBS-001)

`internal/diag` writes JSON Lines to `<state>/logs/` through `log/slog`: an asynchronous, bounded queue
(a full queue drops the record and counts it; the agent path never waits on log I/O), size rotation (8 MiB
× 5 files) and 14-day retention. Level from `execution.logLevel`. The runtime feeds it the event bus
(lossy by design: durable state stays in SQLite) plus direct records from the job manager and scheduler.

Correlation fields, used where they apply: `session_id`, `run_id`, `step`, `tool_call_id`, `job_id`,
`schedule_id`, `provider`, `model`, `duration_ms`, `error_code`.

Redaction applies at every level, before a record is queued: secret-named keys (authorization, cookie,
api key, token, secret, password, credential, private key) are replaced; secret-shaped values (bearer
tokens, `sk-…`, GitHub/AWS keys, PEM blocks) are masked inside any string; content fields (prompts,
messages, reasoning, tool output, tool arguments, commands) are reduced to their byte length, except that
`debug` keeps a command or argument text truncated to 512 bytes after masking. Reasoning and tool output are
never logged. Export writes a zip of the (already redacted) logs, a manifest and job/schedule summaries
without commands or payloads.

### 6. Run budget

`config.Budget` bounds one run: `maxToolCalls`, `maxModelRequests`, `tokenBudget`, `costWarningUsd`,
`costLimitUsd`; the run's deadline (§2) is its runtime limit. The global file's `execution.budget` bounds
every run (child runs included); a run's `AgentOptions.Budget` (CLI: `LCX_MAX_TOOL_CALLS`,
`LCX_MAX_MODEL_REQUESTS`, `LCX_TOKEN_BUDGET`, `LCX_COST_WARNING_USD`, `LCX_COST_LIMIT_USD`) overrides it field
by field. Prices are the model's `cost` (`{input, output}` USD per million tokens, list price; cache
discounts are not modelled).

The runtime controls admission, never the model:

- Before every request attempt (failover and capacity retries included), before its envelope exists:
  the request count, `spent tokens + the request's estimated input` against `tokenBudget`, and with a cost
  limit `spent cost + estimated input cost`, and a price for the model at all. A refused request is not
  sent; the run ends `interrupted` with `budget:model_requests`, `budget:tokens` or `budget:cost`.
- Tool calls: a reply's calls past the remaining allowance are neither stored nor run (event
  `tools.deferred`, reason `budget:tool_calls`), so the transcript stays paired; once the allowance is used,
  the run ends `budget:tool_calls` before its next request (a reply could only ask for tools it may not run).

Spend is settled per sent request and split three ways (`agent.Spend`, returned in `AgentResult.spend`,
event `budget.spend` at every exit, `budget.warning` once):

| | source |
|---|---|
| known | the provider's usage, priced at the model's cost |
| estimated | the request's own token estimate when a response carried no usage, or a request failed after it was sent (it may be billed); priced at the model's cost |
| unknown | a request to a model with no cost: its tokens are counted, its dollars are not, and `unknownCostRequests` counts it. A cost limit refuses such a model outright; a cost warning reports it |

Overshoot: a request already sent is settled whatever its reply costs, so the last admitted request may
take spend past `tokenBudget` or `costLimitUsd` by its output (at most the request's `max_tokens`), and a
provider-side retry inside one attempt is not visible to the runtime. The limits bound what is admitted, not
what a provider bills for a request in flight. Budgets are per run and in memory: a resumed run starts a new
allowance.

### 7. Agent strategy

The default task prompt (`agent.SystemPrompt`, a host prompt replaces it) states a general working
strategy, never task-specific rules: name the deliverable and produce a working version early; keep a
plan and handoff in a notebook for multi-step work and read it, with the runtime rows, when continuing;
bounded experiments; deliver a verified result before the deadline reserve; long commands as background
jobs; change approach after a repeated failure; check files, commands and jobs before answering, and say
what is not done. The runtime enforces what can be enforced (deadline, budget, the jobs gate, the
repeated-failure hint of the Task State, `max_steps`); the prompt carries the rest. No gate loops a run
without bound: the jobs gate stops after two refusals, and the workflow, work-unit and check corrections
are bounded by `max_steps`, the deadline and the budget.

### Engine API (consumed by Desktop)

`Jobs`, `Job`, `JobOutput`, `SubmitJob`, `CancelJob`; `Schedules`, `CreateSchedule`, `UpdateSchedule`,
`DeleteSchedule`; `DiagnosticLogs`, `DiagnosticStats`, `ExportDiagnostics`. JSON field names are camelCase.

## Consequences

- Schema v8 (`0007_jobs`, `0008_schedules`, both reversible). A v6 binary refuses a v8 database
  (`ErrSchemaTooNew`); `lcx-go db downgrade --to 6` drops jobs and schedules after a backup.
- Session deletion removes the session's jobs and scheduled tasks (`purgeTables`, cascade); the job manager
  removes the orphaned output files.
- Background commands run concurrently with the run's foreground tools in the same workspace. The loop still
  executes tool calls one at a time; conflicting background commands are the model's responsibility, bounded
  by the per-run job limit.
- No new process, service or database. Memory: bounded queue and output buffers per job, one scheduler
  timer, one log writer goroutine.

## Alternatives considered

- A separate jobs database: avoids touching the session schema but splits deletion and backup; rejected.
- Logging through the journal table: synchronous SQLite writes on the agent path and no rotation; the
  journal stays the content-free durable trace and the diagnostics log is the bounded, lossy companion.
- A cost limit enforced by asking the model to stop: the model decides when to stop, so it cannot bound
  spend; rejected in favour of admission (§6).
