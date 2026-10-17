# C1 Interactive turns (2026-10-18)

Status: **implemented and validated locally; not pushed.** Core and Desktop are committed to local `main`. Desktop pins the unpublished Core C1 commit.

## Scope and authority

- Item: C1, ADR-0006 D3 ([ADR-0006](../adr/0006-task-context-resolution-and-conversational-execution.md)); design §4.1 and §4.7 now state it (`../LumenCortex-architecture-audit/design/README.md`, a directory not under version control).
- Baseline: Core `8a2be25`, Desktop `28bcf85` (M3/M4 done locally, [M3-M4 report](M3-M4-runtime-2026-10-15.md)).
- Out of scope, not started: C2 (`start_task`, `Delegation`, `task_result` rows), C3 (Desktop proposals, task cards, subtree controls), M5.1.

## What the code did before C1

The loop's completion gate (`internal/agent/loop.go`, the `len(calls) == 0` branch) treated every text answer as a completion proposal: workflow `CanFinish` → no incomplete work units → every required check decided `passed`. Anything else appended a `completion_blocked` row and asked the model again until `MaxSteps`. A Desktop thread with an open work unit therefore could not answer a question: each answer was rejected and the run ended `interrupted/max_steps`. The default system prompt told every run, read-only included, to work "until the requested outcome is implemented and verified".

## Implemented behaviour

| Case | Run end | Work units / obligations | Correction row | Checks decided |
|---|---|---|---|---|
| Interactive, no host workflow, question turn | `completed` with the answer | unchanged; resolution unchanged | none | none |
| Interactive, no host workflow, the run made a successful workspace-mutating tool call | `completed` with the answer | unchanged | none | required checks, once, before the run ends |
| Interactive, host workflow active | unchanged gate: `CanFinish` → units → required checks | unchanged gate | as before | as before |
| Non-interactive (default) | unchanged | unchanged | as before | as before |

- **Evidence rule:** it runs at every completed run end, as before. In an interactive turn it sees only stored results (`checkEvidence` reads, never runs), so an answer cannot create acceptance. It latches done only when no obligation is left and positive evidence exists: a stored `runtime`/`human:*` pass at the current fingerprint, a workflow that can finish, or units completed with passing verdicts. This is also how a Desktop "Accept" latches at the next turn end. A user's `done`/`dropped` is never overridden, and a question turn reopens nothing (a read-only tool is not a mutation).
- **Unchanged in every mode:** approval gates (`waiting_gate`), workflow tool boundaries, work-unit verdicts (`work_unit_update` still decides unit checks through the evaluator).
- **Subagents:** Core forces `Interactive` off for every child run (`opts.parentSessionID != ""`), whatever the options say. Execution tasks (C2) will be children too.
- **System prompt:** `agent.SystemPrompt(interactive, authority)` is the one default. It states the run's purpose: a task run works until the outcome is achieved and verified; a conversation turn answers, changes the workspace only when asked, and says what is left. The runtime appends the authority of the run's `Policy` (read-only / workspace / full). A host `SystemPrompt` still replaces it. The task wording changed from "implemented" to "achieved", and every run now gets the authority sentence.

### Implementation-level decision for owner review

**Required checks after an interactive turn that changed the workspace.**

- **Rule:** an interactive turn decides the required checks before it ends if one of its tool calls succeeded with `MutatesWorkspace` (the same condition that reopens a resolved thread). It never appends a correction.
- **Not in D3:** D3 says only that no continuation is appended. Deciding checks follows from "preserve M4": Desktop sends no host workflow and has no "run checks" operation, so without this every Desktop thread would stop producing check results unless the model completes a unit.
- **Why not decide at every turn:** `Decide` reuses only passing results (`internal/evaluator/run.go`). Deciding at every turn end would re-run a failing suite after each question.
- **Cost:** `shell` is `MutatesWorkspace` for any command, so a turn that ran a shell command also triggers the checks.
- **Alternative:** decide nothing at interactive turn ends. Then checks run only through unit completion or a future host "run checks" operation.

## Changes

| File | Change |
|---|---|
| `internal/agent/types.go` | `Options.Interactive` |
| `internal/agent/loop.go` | `SystemPrompt`; interactive case in the completion gate; `run.mutated` |
| `runtime/agent.go` | `AgentOptions.Interactive` (`json:"interactive,omitempty"`), forced off for subagents; default prompt with `policyAuthority` |
| `internal/agent/interactive_test.go`, `runtime/interactive_test.go` | C1 suite (below) |
| `docs/adr/0006-…md`, design §4.1/§4.7 | status and full text |
| Desktop `internal/backend/runtime.go` | conversation threads start `Interactive: true` (see the Desktop report) |

No schema change, no new table, event, decision kind or run status.

## Tests (deterministic, real loop; runtime tests use a real engine and SQLite)

| Test | Proves |
|---|---|
| `TestInteractiveTurnEndsWithOpenUnits/interactive` | ADR case 1: `completed` after one request; no correction row; no check decided; no `*.blocked_final` event; units open without verdicts; resolution open; obligations `work_units`, `checks` |
| `TestInteractiveTurnEndsWithOpenUnits/non-interactive_keeps_the_gate` | ADR case 3: the same state blocks each answer and ends `interrupted/max_steps` |
| `TestInteractiveRunKeepsTheWorkflowGate` | ADR case 2: `CanFinish` continuation, tool boundary (denied `apply_patch`), work-unit gate under a finishable workflow, human gate → `waiting_gate` |
| `TestInteractiveTurnResolution` | user's `done`/`dropped` kept through a question with a tool; no work created; answer ≠ acceptance; stored acceptance latches without running a check |
| `TestInteractiveTurnThatChangedTheWorkspaceDecidesChecks` | mutating turn decides once; failure → open, pass → done |
| `TestSystemPromptFollowsTheRunPurpose` | purpose reaches the request |
| `TestInteractiveConversationAcrossTurns` (runtime) | two turns with an open unit and required check: Task State, resolution and obligations carry over; no checks run; the reasoning echo is sent back only within its tool turn and not in the next turn; no sentinel in any table, WAL, backup or event; next turn carries Task State and previous answer; a later non-interactive Start on the same session gets the full gate and the task prompt |
| `TestSubagentRunsAreNeverInteractive` (runtime) | a child with `Interactive: true` is still gated |
| `TestInteractiveTurnThatEditsDecidesChecksWithTheEvaluator` (runtime) | git workspace: `write_file` turn → real evaluator `runtime` pass → done by evidence with the result's ref; next question decides nothing and keeps the resolution |

Mutation checks, each reverted:

| Mutation | Failing tests |
|---|---|
| Interactive case removed | `TestInteractiveTurnEndsWithOpenUnits/interactive` and two resolution subtests |
| Interactive case moved before the workflow case, without the `wf == nil` guard | both workflow-gate subtests |
| Mutating-turn decision disabled | the mutating-turn test |
| Decision run on every turn | the open-units and resolution tests |
| Subagent guard removed | `TestSubagentRunsAreNeverInteractive` |

| Check | Result |
|---|---|
| Core `go build ./...`, `go vet ./...`, `go test -count=1 ./...` (2026-10-18, C1 tree) | pass |
| `go test -race` on `internal/agent` and `runtime` (interactive, prompt, echo, reasoning, completion, work-unit, auto-done, mutation tests) | pass |
| Existing gate, workflow, echo and resume tests, with `Interactive` false | pass unchanged; none was edited |
| Desktop validation | in the [Desktop C1 report](../../../LumenCortex-Desktop/docs/reports/C1-desktop-interactive-2026-10-18.md) |

## Not covered

- **No real provider was run.** The ADR's model probe tests `propose` delegation, which is C2, so it was not run.
- **Resume:** an interrupted interactive run resumes through the unchanged Start/Reconcile path. Resume tests (`TestResumeOnFreshEngineNeverSendsEcho`, checkpoint resume) pass unchanged; none of them sets `Interactive`.
- **`task_result` rows** do not exist until C2. The C1 reasoning regression covers Task State, transcript, check results, events and backups.

## Risks and open items

- **Publication:** Core C1 and Desktop C1 are local. Desktop pins the unpublished Core commit; publish Core first.
- **Desktop status label:** Desktop shows `completed` as the run status. With C1 this means "turn ended", not "task done". The Task tab already shows resolution and obligations (M3.3). Relabelling belongs to C3.
- **Shell cost:** a conversation turn using `shell` triggers the required checks once at its end (see the decision above).

## Next

C2 (execution tasks and delegation) depends on C1 for the never-interactive child rule and the authority prompt. The C2 model probe (`local/deepseek-v4.1-flash`, `propose`) belongs there.
