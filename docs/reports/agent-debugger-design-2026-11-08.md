# LumenCortex Agent Debugger — detailed design and prototype report

- Date: 2026-11-08
- Core baseline: `937e161` (`main`, clean before this work)
- Desktop baseline: `main` ahead 1; pre-existing untracked/deleted files present. Desktop is not modified.
- Status: design in progress; DAP host availability verified; runnable prototype and agent-value experiment pending.
- Scope: Core design + isolated experiment only. No production Runtime, Agent Loop, Toolset, Desktop, database schema, benchmark or production configuration changes.

## 1. Executive decision

Build an **Agent Debugger** as a separate, opt-in execution capability. It is not a richer terminal transcript. The system must expose two distinct but related debugging surfaces:

1. **Program debugger (DAP):** real runtime state of a program the Agent launches or attaches to: breakpoints, pause/resume, step over/into/out, threads, stack frames, scopes, variables and expression evaluation.
2. **Agent execution trace:** observable LumenCortex harness state: model request boundaries, tool calls, tool output, run/job lifecycle, file changes and explicit task-state transitions. This is not a program call stack and must never expose hidden model reasoning.

A shared debug session allows the Agent and human operator to inspect/control the same stopped process. The user remains able to take over; the Agent does not silently resume after a human pause or takeover.

**Recommended protocol:** Debug Adapter Protocol (DAP) over stdio, with one adapter process per debug session. Core owns lifecycle, process I/O, request sequencing, state, bounds and permissions. Desktop is a thin client over typed Core APIs and renders an IDE-style debugger. The first integration should support one language adapter end-to-end before adding more.

## 2. Evidence from current repositories

### Core

- Baseline `937e161` is clean on `main`.
- `internal/lsp` already implements a language-server process manager/client; it is a useful structural reference, but LSP is not DAP and should not be merged into one abstraction.
- `internal/toolset` owns Agent-visible tools and execution policy. `internal/toolset/shell.go` runs foreground commands with bounded output, timeout and cancellation.
- `runtime.Engine` owns the workspace, session store, event bus, LSP managers and Agent supervisor. `runtime/events.go` provides bounded, non-blocking event delivery.
- The Agent loop invokes tools synchronously and records observable tool results. A DAP session must be separately owned and must not hold the Agent loop goroutine while waiting for a human.
- No DAP client, breakpoint manager, stack/variable API or debugger session manager was found in Core.

### Desktop

- Desktop is a React + TypeScript + Wails application and must remain a thin adapter over Core.
- Existing inspector components include `TerminalPanel.tsx`, `Inspector.tsx`, and `LSPSettingsPanel.tsx`; these are useful UI integration references, not a reason to embed debugger logic in React.
- Desktop's working tree contains pre-existing changes/untracked artifacts. They were not changed or staged.

### Local adapter availability

- macOS Xcode provides `lldb-dap` at the path returned by `xcrun --find lldb-dap`.
- `clang` and Go are available.
- Python 3.11 is available, but `debugpy` is not installed.
- `dlv` was not found on PATH or in the inspected Go module cache.
- This makes C + LLDB-DAP the lowest-friction real-adapter protocol experiment on this host. It does not establish Go/Delve or Python/debugpy support.

## 3. Product semantics

### 3.1 What is being debugged?

| Surface | Examples | Mechanism | Durable? |
|---|---|---|---|
| Program runtime | source location, threads, frames, locals, watches | DAP adapter | Bounded session metadata and user-approved artifacts only |
| Agent harness | tool start/end, command duration, output summary, file changes, run state | Core event bus + existing journal | Existing observable event/journal rules |
| Model-private reasoning | hidden chain-of-thought, private scratchpad | Not available | Never persisted, replayed or exposed |

The UI must label these separately. A DAP stack frame is a real program frame. An Agent tool trace is an orchestration event. Do not synthesize a fake stack by nesting tool calls.

### 3.2 Primary user journeys

**Agent-led debugging**
1. Agent writes or identifies a program and a reproducible failing command.
2. Agent asks Core to start a debug session for a declared executable, arguments and working directory.
3. Agent sets source breakpoints and continues.
4. Adapter emits `stopped`; Core publishes a bounded event with thread and reason.
5. Agent reads stack, scopes and variables through typed debug tools.
6. Agent may step or evaluate a bounded, explicitly permitted expression.
7. Agent stops the session, changes code and runs normal tests.

**Human-led debugging**
1. User opens the Debugger view for the current project/session.
2. User selects an existing Agent-owned debug session.
3. User adds/removes breakpoints, inspects stack/variables and controls execution.
4. Human takeover transfers control ownership. While held by the human, Agent control calls are refused.
5. User explicitly resumes Agent control or terminates the session.

**Passive execution observation**
- Shell and other tools continue to stream bounded output and lifecycle events.
- No DAP process is started unless the user or Agent explicitly requests debugging.
- The normal path must retain today's low overhead.

## 4. Architecture

```text
Agent Loop / Human UI
  | typed tools / typed Wails bridge
  v
Core runtime.Engine
  | session + project ownership checks
  v
internal/debugger.Manager
  | session state, control lease, quotas, redaction, bounded events
  v
internal/debugger/dap.Client
  | Content-Length framed JSON messages
  v
Adapter process (stdio, one per debug session)
  |-- Go: dlv dap
  |-- Python: debugpy adapter
  |-- JS/TS: js-debug
  |-- C/C++: lldb-dap
  v
Target process (inside the same approved execution boundary)
```

### 4.1 Ownership and lifecycle

Core owns the DAP adapter process and target process lifecycle. Desktop never launches an adapter directly.

A debug session is bound to:
- Core workspace / Project root;
- parent Agent session and, when applicable, Run ID;
- adapter kind and executable resolved by Core configuration;
- explicit target executable, args, cwd and source mappings;
- controller lease: `agent`, `human` or `none`.

Suggested state machine:

```text
created -> starting -> configuring -> running <-> stopped
                       |                 |          |
                       +-> failed        +-> exited +-> terminated
human takeover: running/stopped -> human_control -> agent_control
any non-terminal state -> stopping -> terminated
```

Do not reuse a DAP session across unrelated Agent Runs. A stopped target is not a durable checkpoint: process memory and OS resources are ephemeral. After Core restart, mark the session `lost`/terminated and require a new launch; never pretend the target can be resumed from a stored stack/variable snapshot.

### 4.2 Core package boundaries

Proposed packages (not implemented in production yet):

- `internal/debugger/types.go`: public-to-Core value types and enums.
- `internal/debugger/manager.go`: per-workspace/session registry, state machine, control lease and cleanup.
- `internal/debugger/dap/client.go`: DAP framing, request correlation, event dispatch, timeouts.
- `internal/debugger/dap/codec.go`: Content-Length parser/writer with strict size limits.
- `internal/debugger/adapters/catalog.go`: allowlisted adapter definitions and language capabilities.
- `internal/debugger/manager_test.go`: lifecycle, lease, cancellation and ownership tests.
- `internal/debugger/dap/client_test.go`: framing, concurrent requests, malformed frames and adapter exit tests.

Do not put DAP process logic into `internal/lsp`: LSP and DAP have different request lifecycles, capabilities, state machines and process ownership.

### 4.3 DAP transport

DAP uses JSON messages framed by ASCII headers, usually:

```text
Content-Length: <UTF-8 byte count>\r\n
\r\n
<JSON payload>
```

A minimal Go framing primitive:

```go
func writeDAP(w io.Writer, payload any) error {
    body, err := json.Marshal(payload)
    if err != nil {
        return err
    }
    if len(body) > maxDAPMessageBytes {
        return ErrMessageTooLarge
    }
    if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
        return err
    }
    _, err = w.Write(body)
    return err
}

func readDAP(r *bufio.Reader) (json.RawMessage, error) {
    length := -1
    for {
        line, err := r.ReadString('\n')
        if err != nil {
            return nil, err
        }
        line = strings.TrimRight(line, "\r\n")
        if line == "" {
            break
        }
        key, value, ok := strings.Cut(line, ":")
        if !ok {
            return nil, ErrMalformedDAPHeader
        }
        if strings.EqualFold(strings.TrimSpace(key), "Content-Length") {
            if length != -1 {
                return nil, ErrMalformedDAPHeader
            }
            n, err := strconv.Atoi(strings.TrimSpace(value))
            if err != nil || n < 0 || n > maxDAPMessageBytes {
                return nil, ErrMessageTooLarge
            }
            length = n
        }
    }
    if length < 0 {
        return nil, ErrMalformedDAPHeader
    }
    body := make([]byte, length)
    if _, err := io.ReadFull(r, body); err != nil {
        return nil, err
    }
    return json.RawMessage(body), nil
}
```

This is a design excerpt, not production-ready code: production implementation must also handle header limits, UTF-8 byte counts, concurrent writes, request sequence correlation, pending-request cancellation, adapter stderr, process exit and close races.

### 4.4 DAP request sequence

Minimum launch sequence:

```text
initialize
launch (or attach)
setBreakpoints (per source)
setExceptionBreakpoints (optional)
configurationDone
wait for initialized/stopped/continued/exited/terminated events
```

Core must correlate responses by request `seq`, not assume responses arrive in request order. Events may arrive between any two responses.

Core API shape (proposed):

```go
type DebugLaunch struct {
    SessionID string
    RunID     string
    Adapter   string // allowlisted adapter id, never an arbitrary command
    Program   string // resolved inside the approved project/worktree
    Args      []string
    CWD       string
    StopOnEntry bool
}

type DebugSession struct {
    ID          string
    SessionID   string
    RunID       string
    State       DebugState
    Adapter     string
    Controller  DebugController
    StartedAt   time.Time
    StoppedAt   *time.Time
    StopReason  string
    ThreadID    int
    Source      *DebugSourceLocation
}

func (e *Engine) DebugLaunch(ctx context.Context, in DebugLaunch) (DebugSession, error)
func (e *Engine) DebugSetBreakpoints(ctx context.Context, id string, in []SourceBreakpoint) ([]Breakpoint, error)
func (e *Engine) DebugContinue(ctx context.Context, id string, threadID int) (DebugRunState, error)
func (e *Engine) DebugPause(ctx context.Context, id string, threadID int) (DebugRunState, error)
func (e *Engine) DebugStep(ctx context.Context, id string, threadID int, kind StepKind) (DebugRunState, error)
func (e *Engine) DebugStackTrace(ctx context.Context, id string, threadID int) ([]StackFrame, error)
func (e *Engine) DebugScopes(ctx context.Context, id string, frameID int) ([]Scope, error)
func (e *Engine) DebugVariables(ctx context.Context, id string, variablesReference int) ([]Variable, error)
func (e *Engine) DebugEvaluate(ctx context.Context, id string, frameID int, expression string) (Evaluation, error)
func (e *Engine) DebugStop(ctx context.Context, id string) error
```

The actual API should avoid exposing raw DAP JSON to Agent tools or Desktop. Typed Core methods normalize adapter-specific differences.

### 4.5 Agent tool surface

Do not expose 28 low-level DAP methods as independent tools. Tool schema bloat and call overhead will hurt ordinary coding tasks. Use a compact tool set:

| Tool | Purpose |
|---|---|
| `debug_start` | Start allowlisted adapter for an executable inside the project |
| `debug_breakpoints` | Set/list/remove source breakpoints |
| `debug_control` | continue, pause, step_over, step_into, step_out, stop |
| `debug_inspect` | stack, scopes, variables, threads |
| `debug_evaluate` | Evaluate one bounded expression, only when enabled by policy |

`debug_inspect` should support a bounded selector rather than return the entire tree. Default response: top 10 frames, 20 variables per scope, 8 KiB per value, maximum depth 2, with explicit truncation metadata. The Agent can request a child variable by reference when necessary.

Suggested tool schema:

```go
protocol.ToolSpec{
    Name: "debug_inspect",
    Description: "Inspect a stopped debuggee: threads, stack frames, scopes or variables. Results are bounded; request a child variablesReference for expansion.",
    Permission: "debug",
    Parameters: obj(map[string]any{
        "debug_session_id": stringParam("Active debug session id"),
        "kind": enumParam("threads", "stack", "scopes", "variables"),
        "thread_id": integerParam("Required for stack/scopes"),
        "frame_id": integerParam("Required for scopes/evaluate"),
        "variables_reference": integerParam("Required for variables"),
    }, "debug_session_id", "kind"),
}
```

The tool should return a structured JSON object containing only observable target state, not model reasoning.

### 4.6 Human control and Agent control

Use a single-writer control lease:

- Initial owner is the Agent when Agent launched the target; otherwise human.
- Human takeover increments a monotonically increasing control epoch.
- Every control request carries the expected epoch. Stale Agent requests are refused.
- Human pause/takeover is sticky. Agent cannot auto-resume.
- Agent handback is an explicit UI action.
- Stop is always available to the human owner; Agent stop is subject to session ownership.

This prevents a race where a model response resumes a process after the user has paused it.

## 5. Language adapters and realistic support

| Language | Adapter | Debug features | Priority |
|---|---|---|---|
| C/C++ | Xcode/LLVM `lldb-dap` | breakpoints, frames, variables, stepping | Prototype |
| Go | `dlv dap` | source breakpoints, goroutines/threads, frames, locals | P1 |
| Python | `debugpy.adapter` | breakpoints, frames, scopes, variables, evaluate | P1 |
| JavaScript/TypeScript | Microsoft `js-debug` | Node targets, source maps, async stacks | P2 |
| Java | Java debug adapter | JVM frames, locals, watches | P3 |

Adapter installation and configuration is explicit. Never download/install an adapter as a side effect of an Agent tool call. Core should resolve an adapter ID to an allowlisted executable and arguments from trusted configuration. The adapter itself is executable code and must run within the same approved execution boundary as the target.

## 6. Desktop debugger workbench

Use the existing React/TypeScript + Wails architecture. Desktop only renders typed Core state and forwards commands.

Proposed layout:

```text
┌ Debug toolbar: Continue | Pause | Step Over | Step Into | Step Out | Stop ┐
├───────────────────────┬──────────────────────────────┬───────────────────┤
│ Breakpoints / Threads │ Source editor                │ Call Stack        │
│                       │ current line highlight       │ Variables         │
│                       │ inline local values           │ Watches           │
├───────────────────────┴──────────────────────────────┴───────────────────┤
│ Debug Console | Agent Tool Trace | Adapter Output | Session Events       │
└──────────────────────────────────────────────────────────────────────────┘
```

Suggested integration points:
- Add a Debugger workspace panel, not a replacement for `TerminalPanel`.
- Reuse the existing code viewer/editor infrastructure where practical; inspect the current editor capability before choosing Monaco or another editor.
- Add a typed bridge module and domain types.
- Subscribe to Core debug events through the existing bounded event stream.
- Keep event/output history bounded and virtualized.
- Localize all copy through the existing i18n catalog and use shared CSS tokens.
- At 560/820 px, collapse side panes into tabs rather than shrinking source text to unusable widths.

## 7. Security, reliability and resource boundaries

1. **Executable allowlist:** Agent supplies an adapter ID, not a shell command or arbitrary adapter path.
2. **Target confinement:** executable, cwd, source paths and file references must resolve under the session's approved workspace/worktree. Explicitly model generated build output paths.
3. **No shell bypass:** DAP launch does not bypass the same run policy, project trust and user approval requirements as equivalent code execution.
4. **Evaluate is code execution:** initially disable `debug_evaluate` by default; allow only for trusted workspaces and explicit policy. Never evaluate against a different process/session.
5. **Resource bounds:** cap adapter count per workspace, target lifetime, message size, stdout/stderr, event buffer, frames, variables, variable depth and value length.
6. **Cancellation:** stop the process group on explicit stop, owner cancellation, deadline, adapter crash or Engine close. Do not orphan debuggee children.
7. **No false persistence:** store metadata and bounded event summaries only. A process cannot survive Core restart as a resumable debugger session.
8. **Redaction:** redact environment values, arguments known to contain secrets, adapter stderr and evaluated values before events/logging. Never return environment dumps by default.
9. **Concurrency:** serialize control operations per debug session; permit safe concurrent reads only when adapter capability/state allows.
10. **Human takeover:** a human-owned control epoch blocks Agent step/continue/evaluate calls until explicit handback.

## 8. Evaluation: does DAP help the Agent?

A visual debugger is useful only if it improves task outcomes enough to justify tool and token overhead. Evaluate separately from UI polish.

### 8.1 Experiment design

Use a fixed deterministic bug fixture and identical task prompt, source tree, tests, model, temperature, tool budget and timeout for two conditions:

- **A — baseline:** current shell/read/search/test tools only.
- **B — debugger-assisted:** same tools plus DAP debug tools.

Run at least 5 paired tasks before making a product decision. Do not use TB2 or modify its dataset/results. Keep this as a new local diagnostic experiment. If a paid model is used, obtain explicit approval for the bounded number of requests first; the initial protocol experiment is zero-model-request.

### 8.2 Task selection

Start with bugs where runtime state has information that static source inspection and a failing test do not immediately reveal:
- wrong runtime branch/config value;
- incorrect loop/index state;
- mutation or aliasing of an object;
- incorrect state transition across nested calls;
- race/concurrency issue (later phase; DAP alone is not sufficient).

Avoid trivial divide-by-zero or obvious syntax errors as the sole efficacy benchmark: those can make a debugger look helpful without proving incremental value.

### 8.3 Metrics

Primary:
- task success / test pass;
- time to correct root cause;
- total wall-clock time.

Secondary:
- model input/output/total tokens;
- tool calls and DAP requests;
- number of incorrect hypotheses;
- number of code edits/reverts;
- debugger startup and inspection overhead.

For paired runs, report per-task outcomes and median differences. Do not invent a synthetic score. A DAP feature is justified for Agent autonomy if it increases success or reduces root-cause time on runtime-state bugs without disproportionate token/time overhead. If it only helps humans, keep it as a human-facing opt-in feature rather than adding DAP tools to every Agent.

### 8.4 Current experiment limitations

The first local experiment proves DAP protocol interoperability and that an Agent/tool can receive runtime facts. It is not a statistically valid model benchmark. A real model A/B is a later explicit, bounded experiment after the tool integration exists.

## 9. Delivery phases and acceptance

### P0 — protocol proof (this task)

- Real `lldb-dap` process started via stdio.
- initialize / launch / setBreakpoints / configurationDone succeeds.
- Program stops at the intended source line.
- Stack trace and local variables are returned.
- Step Over moves to the next source line.
- Continue/terminate cleans up the target.
- A deterministic report records commands and outputs without private model traces.

### P1 — Core DAP manager

- framing parser and writer with malformed/oversized input tests;
- request correlation, events, deadlines and cancellation;
- adapter allowlist/config;
- lifecycle and controller lease tests;
- typed runtime API + events;
- one language adapter (Go/Delve preferred for LumenCortex's own Go-centric workflow) plus C/LLDB compatibility fixture;
- no DB migration until evidence shows durable metadata is required; initially use in-memory active sessions and bounded events.

### P2 — Agent integration

- compact opt-in debug tool set;
- policy and approval tests;
- stopped-state inspection and expansion;
- Agent prompt guidance: use DAP only after a reproducible runtime failure, not for every coding task;
- controlled A/B evaluation.

### P3 — Desktop workbench

- typed Wails methods and types;
- source editor + breakpoint gutter;
- threads/stack/variables/watches;
- toolbar and debug console;
- human takeover / handback;
- accessibility, localization and responsive acceptance.

### Definition of done for full feature

- Real adapter integration tests on macOS and Linux for supported adapters.
- Agent can launch, break, inspect, step and stop a target through tools.
- Human takeover prevents stale Agent controls.
- No orphan processes after stop, cancellation, timeout, adapter crash or Engine close.
- Tests prove project/session/run ownership, path confinement, bounds and event drops.
- Paired A/B tasks show measurable benefit or feature is scoped as human-only.
- Desktop build and backend tests pass against the exact Core API revision.

## 10. Open decisions

- Whether debug sessions are ephemeral-only for P1 or require a minimal persisted metadata record. Recommendation: ephemeral process state, metadata-only event history; no DB migration in P1.
- Which adapter is first in production. Recommendation: Go/Delve because LumenCortex Core is Go and this is likely the highest-value dogfooding target. C/LLDB is the fastest P0 protocol fixture on this macOS host.
- Whether expression evaluation is Agent-accessible. Recommendation: disabled by default; separate permission from read-only stack/variables.
- Whether the UI source editor is Monaco or existing code viewer. Verify existing editor behavior before adding a heavyweight dependency.
- Whether DAP sessions run in a sandbox/container. Must follow the active execution boundary; do not invent a parallel sandbox system.

## 11. Change and verification ledger

| Item | Status |
|---|---|
| Core/desktop architecture inspection | Done |
| Existing LSP, shell, runtime event and Desktop boundaries inspected | Done |
| Adapter availability | `lldb-dap`, clang, Go available; `dlv` and `debugpy` unavailable |
| DAP real-adapter experiment | Pending |
| Agent benefit A/B | Pending; no model calls authorized in this task |
| Core production implementation | Not started |
| Desktop implementation | Not started |
| Benchmark / TB2 | Not run; no benchmark files changed |

## 12. Next action

Complete the P0 C + LLDB-DAP stdio experiment in `experiments/agent-debugger-dap`. Use its actual event/stack/variable evidence to revise this design. Then decide whether a small Core DAP client prototype is justified before adding production APIs.

## 13. P0 experiment results (2026-11-08)

### Fixture

`experiments/agent-debugger-dap/fixture.c` intentionally uses `count - 3` where the expected multiplier is `count`. The program prints `total=7 expected=43` and exits with code 2. It is deliberately small: the purpose is to prove runtime-state visibility and DAP interoperability, not to claim a representative Agent coding benchmark.

### Real adapter evidence

The experiment used the actual Xcode adapter path from `xcrun --find lldb-dap`, with `clang -g -O0 -fno-omit-frame-pointer`. The stdlib-only Python client completed:

- `initialize` (adapter advertised configuration-done and evaluate-for-hover capability);
- `launch` and `initialized`;
- `setBreakpoints` (source line 4 returned `verified: true`);
- `configurationDone`, then `stopped` with reason `breakpoint`;
- `stackTrace`: `compute_total` → `main` → `start`;
- `scopes` + `variables`: runtime locals included `count=3` and `multiplier=0`;
- two `next` requests, each followed by a `stopped` event and refreshed source location/locals; after the second step, `adjusted=0`;
- `continue`, `exited` with code 2 and `terminated=true`.

The successful run took approximately 1.2 seconds after compilation. Machine-readable output is `experiments/agent-debugger-dap/evidence.json`; executable fixture and dSYM were removed after capture, and the README contains the reproducible build/run commands.

### What this proves

The key Agent-relevant capability is confirmed: an external debugger can return facts that a plain process exit code cannot, including actual local values (`count=3`, `multiplier=0`, `adjusted=0`) and the real call stack at a source breakpoint. This is a viable substrate for an Agent to test a runtime hypothesis rather than infer solely from source and logs.

### What this does not prove

- No LLM/Agent model was invoked; no local model runtime was available on PATH. No paid request was made.
- This simple arithmetic fixture is intentionally easy to diagnose from source, so it is not evidence that DAP improves task success over a capable baseline Agent.
- No A/B result, token reduction, or time-to-root-cause improvement is claimed.
- The prototype is not production-safe: it lacks robust multiplexed request correlation, process-group ownership, full framing/header validation, project confinement and human/Agent control leases.

### Updated decision

Proceed to a small Core-only DAP client/manager spike only after choosing the first production adapter. For LumenCortex's Go-heavy workload, Delve DAP is the recommended first production adapter; C/LLDB remains a proven protocol fixture. Before exposing debug tools to all Agents, run paired baseline-vs-debugger tasks on runtime-state bugs and report success, wall time, root-cause time, tool calls and tokens. Keep debugger tools opt-in until that evidence exists.
