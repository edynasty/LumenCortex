# Harness evolution — Eval / managed-process slice (2026-11-10)

Status: implemented and validated on macOS arm64 and in a privileged Ubuntu 24.04
arm64 container. Branch `feat/harness-evo-eval`, worktree
`wt/core-harness-evo-eval`, baseline Core commit `696eb74`
(`feat/agent-debugger-execution-platform`).

Scope: the Eval + managed-process slice of the agent-harness capability roadmap
(`bench/evidence/agent-harness-capability-roadmap-20261110/`), spec
`DAP_AND_PERSISTENT_EVAL_DESIGN.md` §3–§4. No DAP files were edited.

## 1. Baseline (before this slice)

- `go test ./evalruntime ./evalapi` on macOS: 150 pass / 3 skip
  (`raw/e2-evalruntime-package-tests.jsonl`).
- Python/JS persistent kernels worked (create/execute/reset/timeout/interrupt/
  close), with vars, functions, imported modules and exception-keeps-state.
- Gaps confirmed by inventory:
  - **No inspection API**: the host could not ask which names a kernel holds.
  - **Reset was the only lifecycle operation**, and it replaced the process *and*
    wiped the workspace; there was no way to restart the process and keep files.
  - **Python module-level await was rejected**: only a trailing `await expr`
    worked, through `asyncio.run()` (a fresh loop per cell, so an asyncio task
    could not cross a cell).
  - **macOS memory was reported as "monitored"** but the report was a free-form
    note; no result field said which bound fired or how it was enforced, and no
    process census existed. On macOS there is no hard memory limit (measured, see
    §3), so the label was the only thing preventing a supervised kill from being
    read as a kernel guarantee.

## 2. What changed

### 2.1 `internal/procrt` (new; shared managed-process base)

`Owner{SessionID,RunID,Component,ObjectID}`, `Registry.Register/List/Count`,
`CloseOwner` with an interrupt→terminate→kill `Ladder`, `Enforcement` with
`hard|runtime|supervised|none`, `Status` census rows, a `Ring` that wraps the
existing `internal/streambuf` bound, and `CmdHandle`/`RegisterCommand` for
launchers that own a plain `*exec.Cmd`.

Extraction, not duplication: the supervision itself (process groups, descendant
tracking with start times, cgroup membership, cleanup window, output rings) still
lives where it was — `evalruntime/proc.go` and `internal/shell`. `procrt` owns the
census, the escalation order and the enforcement vocabulary.

`CloseOwner` unregisters every row it acted on, including a row whose stop was not
clean, and returns `ErrCleanupFailed` with the surviving pids. It never reports a
stop it cannot certify.

**DAP hook (one line, for the integrator; DAP files were not edited):** the DAP
adapter launcher can call

```go
unregister := procrt.RegisterCommand(procrt.Owner{SessionID: sid, Component: procrt.ComponentDAP, ObjectID: debugID}, cmd)
defer unregister()
```

after `cmd.Start()`, and `handle.Reaped()` from its existing wait goroutine. A
richer launcher implements `procrt.Handle` and calls `procrt.Default().Register`.

### 2.2 Enforcement truthfulness (`evalapi`, `evalruntime`)

- `evalapi.EnforcementRuntime = "runtime"` and
  `evalapi.EnforcementSupervised = "supervised"`; `EnforcementMonitored` is kept
  as the legacy spelling of supervised so older declarations still parse.
- New `evalapi.EnforcementView` (`memory/cpu/disk`, `memoryLimitBytes`,
  `memoryRuntimeCapBytes`, `memoryPollMs`, notes) on `evalapi.Session`.
- `evalapi.Result` gained `limit` (`memory|cpu|disk`) and `enforcement`
  (`hard|runtime|supervised`) next to `stateStatus=lost`.
- Attribution is recorded when the bound fires, not guessed afterwards:
  cgroup OOM → memory/hard; RSS monitor → memory/supervised; V8 heap abort
  (stderr "Reached heap limit"/"JavaScript heap out of memory") →
  memory/runtime; SIGXCPU → cpu/hard.
- The supervised reason carries the observed RSS, the limit, the poll interval and
  the measured overshoot, e.g.
  `memory limit exceeded: observed RSS 190496768 bytes > 134217728 byte limit (supervised, polled every 200ms; overshoot 56279040 bytes)`.
- `Usage.MemoryPeakBytes` is now the highest sampled RSS on hosts without
  cgroups (0 still means "not measured", never "zero resident").

### 2.3 macOS: the strongest real mechanism per resource

Measured on this host (macOS 27.0.1, arm64), see §3 for the raw evidence:

- `setrlimit(RLIMIT_AS)` and `setrlimit(RLIMIT_DATA)` are **rejected with EINVAL**;
  `ulimit -v` cannot be lowered. There is no hard per-process memory limit.
- Seatbelt (`sandbox-exec`) has no memory operation; it cannot bound RSS.
- V8 `--max-old-space-size` **is** enforced by the interpreter: a heap-exhausting
  cell aborts the process with exit 134 and
  `FATAL ERROR: Reached heap limit`. It bounds the JS heap only — `Buffer`
  allocations are outside it.
- `RLIMIT_CPU` (`ulimit -t`) is enforced by the kernel (SIGXCPU).

Implementation: JS kernels are started with
`--max-old-space-size=3/5·MemoryBytes` and `--max-semi-space-size=min(16, heap/4)`,
reported as `memoryRuntimeCapBytes`. RSS remains supervised and is what stops
external memory. Python has no interpreter heap cap, so its memory bound stays
supervised. Linux keeps the cgroup hard limits unchanged.

### 2.4 Kernel inspection, restart, census (gap fills)

- Worker RPC `inspect` (Python and JS) lists user-visible names and their types
  **without rendering values** — a `__repr__` with side effects cannot run during
  an inspection. The runtimes declare `evalapi.CapInspect`.
- `Manager.Inspect` / `Engine.EvalInspect` / tool `eval_inspect` (permission read;
  auto-denied inside eval callbacks like the other `eval_*` tools). A kernel
  without a live worker reports `alive=false` and no entries; a runtime that does
  not implement it returns `ErrUnsupportedCapability`, never an empty state.
- `Manager.Restart` / `Engine.EvalRestart` / `eval_control{action:"restart"}`
  replace the kernel process and discard live state, **keeping workspace files**.
  `Reset` is unchanged (process replacement + workspace wipe), so existing
  guarantees and tests hold.
- `Engine.Processes(ctx, sessionID)` returns the procrt census for one Core
  session (`owner/component/pid/treePids/state/usage/enforcement/notes`), additive
  host API for the Hub.

### 2.5 Python module-level await

Cells are parsed with
`compile(..., PyCF_ONLY_AST | PyCF_ALLOW_TOP_LEVEL_AWAIT)`, the trailing
expression is split off as before, and the body/final expression are compiled with
`PyCF_ALLOW_TOP_LEVEL_AWAIT` and driven through **one persistent event loop**.
`x = await f()` now works anywhere in a cell, and an asyncio task created in one
cell completes in the next. Previously the cell raised
`SyntaxError: 'await' outside function` and each cell got a fresh loop through
`asyncio.run`, which broke cross-cell tasks and clients.

## 3. Measured enforcement matrix

| Resource | Linux (cgroup v2) | macOS arm64 | Mechanism |
|---|---|---|---|
| memory | **hard** `memory.max` + `swap.max=0` + `oom.group=1`, applied before the worker exists | **supervised** RSS poll (200 ms) for Python; **supervised** RSS + **runtime** V8 heap cap for JS | see below |
| cpu | **hard** `RLIMIT_CPU` + `cpu.max` | **hard** `RLIMIT_CPU` (`ulimit -t`) | SIGXCPU attributed to `limit=cpu` |
| disk | **hard** per-file `RLIMIT_FSIZE`; workspace total supervised (400 ms) | **hard** per-file `RLIMIT_FSIZE`; workspace total supervised | artifact quota separate |
| wall | supervised host timer + kill | supervised host timer + kill | timeout loses state |
| process tree | hard: cgroup leaf + `cgroup.kill` + `pids.max=512` | fork denied by Seatbelt; process-group + descendant tracking otherwise (observation, not containment) | |

Raw empirical checks on this macOS host:

- `python: resource.setrlimit(RLIMIT_AS, 256MiB)` → `ValueError: current limit
  exceeds maximum limit`; C `setrlimit` → `EINVAL`; `getrlimit` still reports
  infinity. Same for `RLIMIT_DATA`.
- `node --max-old-space-size=64` on an object-allocating loop → exit 134,
  `FATAL ERROR: Reached heap limit Allocation failed - JavaScript heap out of memory`
  (peak RSS ≈ 122 MB for a 64 MiB heap cap).
- `node --max-old-space-size=64` on a `Buffer.alloc` loop → no heap abort, RSS
  reached 2.7 GB: external memory is outside the cap, so it must stay supervised.
- Supervised Python breach (limit 128 MiB): killed after 688 ms wall, sampled peak
  190,496,768 B → **overshoot 56,279,040 B (53.7 MiB)** at ~400 MB/s allocation.
- Supervised JS `Buffer.alloc` breach (limit 192 MiB): observed RSS 572,178,432 B
  → **overshoot 370,851,840 B (353.7 MiB)** at ~1.8 GB/s allocation. The overshoot
  window is one poll interval, so its byte size scales with the allocation rate;
  this is why it is reported as supervised, never hard.

## 4. Tests

All commands run in this worktree; raw output under
`bench/evidence/agent-harness-implementation-20261110/raw/eval/`.

### 4.1 Capability inventory (before → after)

| Capability | Python | JavaScript | Added by this slice |
|---|---|---|---|
| create | yes | yes | — |
| execute | yes | yes | — |
| vars / functions / imported modules persist | yes | yes | — (regression tests kept) |
| async / Promise | trailing `await expr` only → `x = await f()` anywhere, one persistent loop | `await`, `Promise`, dynamic `import()` | Python module-level await + persistent loop; JS ESM/dynamic import covered by test |
| inspect | no → names + types, no value rendering | no → same | worker `inspect`, `eval_inspect`, `Manager.Inspect` |
| reset | process replace + workspace wipe | same | — (semantics unchanged) |
| restart | none → process replace, **workspace kept** | same | `Manager.Restart`, `eval_control restart` |
| cancel / interrupt | SIGINT, escalation, generation guard | synchronous V8 cannot be preempted (declared) | — |
| close | yes | yes | census row removed |
| exception keeps state | yes | yes | — |
| process census / enforcement label | none → `Engine.Processes` | same | procrt |

Agent tool registry: `eval_runtimes`, `eval_sessions`, `eval_create`, `eval`,
`eval_control` (+ `restart`), `eval_artifact`, `eval_inspect` (new). All are
bound only when the host enables eval (`AgentOptions.EvalEnabled`), and all are
denied inside eval callbacks (`isEvalTool` prefix rule).

## 5. Validation (macOS arm64)

Run in this worktree, `-count=1`, raw JSON kept:

| Command | Result | Raw |
|---|---|---|
| `go test -count=1 -json ./evalruntime/... ./evalapi/... ./internal/procrt/...` | **169 pass / 0 fail / 4 skip** (skip: two detached-descendant tests that need `subprocess` enabled, the Linux cgroup test, the Linux socket-isolation test) | `raw/eval/packages-macos.jsonl` |
| `go test -race -count=1 -json ./internal/procrt/` | 7 pass / 0 fail | `raw/eval/procrt-race-macos.jsonl` |
| `go test -race -count=1 -json ./evalruntime/` | 146 pass / 0 fail | `raw/eval/evalruntime-race-macos.jsonl` |
| `go test -race -count=1 -json -run 'TestAgentKeeps\|TestProcessesReportsEvalCensus\|TestEvalInspectAndRestartThroughTheEngine\|TestAgentUsesPersistentRuntime…\|TestAgentEvalCallbacks…' ./runtime/` | 7 pass / 0 fail | `raw/eval/runtime-eval-race-macos.jsonl` |
| `go build ./... && go vet ./...` | clean | — |

The agent-loop tests use a **scripted provider** (`local-eval-persistence-fixture`),
so no model request was made.

## 6. Limitations and unverified claims

- macOS has no hard memory limit for the interpreter; Python memory is supervised
  and can overshoot by one poll interval of allocation (measured above). JS heap
  is runtime-capped, external memory is supervised. This is reported, not hidden.
- The supervised overshoot depends on the cell's allocation rate; the test asserts
  the poll interval is reported and records the measured bytes, but cannot
  promise a byte ceiling for arbitrary allocation speeds.
- `Inspect`, `Restart` and `Processes` are **not available through the Linux UID
  broker** (the broker RPC was deliberately not extended in this slice); the
  Engine returns `ErrUnsupportedCapability` for inspection/restart and an empty
  census there.
- JS synchronous code still cannot be preempted; a timeout loses state, as before.
- The DAP launcher does not yet call `procrt.RegisterCommand`; the hook is
  documented above for the integrator.
