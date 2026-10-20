# Configurable agent concurrency — Core (2026-10-20)

Status: **implemented and validated locally** (Core). Desktop Settings: see the Desktop report `docs/reports/agent-concurrency-desktop-2026-10-20.md`.
Decision record: [ADR-0008](../adr/0008-configurable-agent-concurrency.md). Baseline: `f7ffc18`.

## Findings that shaped the design

| # | Finding (verified in code before the change) | Effect |
|---|---|---|
| F1 | `resource.New` defaulted `MaxAgents` to 3; no host set `Options.Budget.MaxAgents` | Desktop ran at most 3 agents per process |
| F2 | `MaxConcurrentSubagentsPerParent = 4` checked in `subagentController.Spawn` and `startExecutionTask` | unreachable: parent + 4 children needs 5 slots, so a thread ran ≤ 2 children |
| F3 | `AcquireAgent` fails fast; no queue anywhere; a refused `Start` writes nothing | kept: no deadlock-prone waiting, no new lifecycle state |
| F4 | `resource.Manager.Reserve` has no caller in the run path | memory is not an admission signal today; agent count stays the unit |
| F5 | `prepareRun` already loads the configuration per Start (`planModels`, checks) | reading the limits per Start costs one more bounded read, no new cache |

## Changes

- `config`: `File.Concurrency` (`maxAgents` 1–16 default 4, `maxChildrenPerParent` 1–8 default 3), `Concurrency.Limits()`, `Effective.Concurrency`; workspace value ignored with new warning code `global_only`; schema object in `docs/lumencortex.schema.json`.
- `internal/resource`: the agent limit is an atomic (`SetMaxAgents`, `MaxAgents`); `Budget()` reports the live limit; refusal carries `(<n> running, limit <m>)`.
- `runtime`: `Engine.concurrency()` loads the limits and applies `maxAgents` unless the host fixed `Budget.MaxAgents`; `RunSupervisor.Start` applies it before `AcquireAgent`; `checkChildLimit` replaces the two constant checks and tells the model how to proceed; `Health()` refreshes the limit; `MaxConcurrentSubagentsPerParent` removed.
- Docs: ADR-0008, ADR-0006 amendment, `docs/go-provider.md` (run ownership, configuration), ADR index.

## Tests run

| Command | Result |
|---|---|
| `go test -count=1 ./runtime -run 'ConfiguredChildLimit\|ConfiguredMaxAgents\|ShareTheParentLimit' -v` | 3 PASS |
| `go test -count=1 ./config ./internal/resource` | ok |
| `go build ./... && go vet ./... && go test -count=1 ./...` | all packages ok |

New/changed regression tests: `TestConcurrencyIsReadFromTheGlobalFileOnly`, `TestConcurrencyOutsideItsBoundsIsInvalid`, `TestSubagentControllerEnforcesConfiguredChildLimit` (replaces the fixed-4 test; adds the live raise), `TestRunSupervisorAppliesConfiguredMaxAgentsAtEveryStart` (live lower keeps running agents, refused start leaves the session `created`, host override wins), `TestExecutionChildrenAndSubagentsShareTheParentLimit` (now on the default limit).

## Limits and open points

- Host resume of an interrupted child is bounded by `maxAgents` only, as before.
- Limits are per process (per Engine). Two LumenCortex processes each get their own `maxAgents`; children running in another process still count toward `maxChildrenPerParent`.
- Hidden reasoning: not touched; no persistence, compaction or recovery path changed.
