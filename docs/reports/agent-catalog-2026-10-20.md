# Agent catalog and model lists — phases A1, A2 Core and A3 (2026-10-20)

- **Scope:** ADR-0009 phase A1, Core only: the `agents` configuration key, built-ins, validation, trust merge, the merged catalog view, and the retirement of `subagentModel`. No run consumes thread agents, ceilings or per-candidate effort yet (A2), and children cannot choose an agent yet (A3).
- **Baseline:** Core `efe9b66` (ADR-0008 concurrency); A1 committed as `bfed40c`.
- **Status:** A1 and A2 Core implemented and validated locally; not pushed. A2 Core committed as `8998c25`; A2 Desktop (Agents page and composer on the Core catalog, `agents.json` import, `subagentModel` migration) committed as Desktop `c82a039`, report in that repo at `docs/reports/agent-catalog-desktop-2026-10-20.md`. `lcx-go agents` committed as `67ad2ca`. A3 implemented and validated (section below).
- **Design:** [ADR-0009](../adr/0009-agent-catalog-and-model-lists.md); handoff `../LumenCortex-architecture-audit/design/handoffs/agent-architecture-2026-10-20.md` (not version-controlled).

## Changes

| Area | Change | Files |
|---|---|---|
| Schema | `agents` (id pattern `^[a-z][a-z0-9-]{0,39}$`, ≤ 20 entries), `$defs/agent`, `$defs/agentModel`; `subagentModel` removed; `trustedWorkspaces` text names agents | `docs/lumencortex.schema.json` |
| Types | `Agent`, `AgentModel`, `AgentUse`, `AgentInfo` (`ForConversation`, `ForDelegation`, `SpawnEligible`, `TaskEligible`), built-in ids, bounds, policy/delegation value constants | `config/agents.go`, `config/types.go` (`File.Agents`; `File.SubagentModel` removed) |
| Per-file rules | built-in fixed fields refused; delegation agents need a read-only/workspace policy and a description and no delegation; ≤ 16 custom; ≤ 280-byte description; ≤ 16 KiB UTF-8 instructions; no duplicate model in a list — in `Parse` and `Validate` (so `Save` too) | `config/agents.go` `validateAgents`, `config/schema.go` |
| Merge | trusted workspace: custom agents replaced whole, built-ins field by field; untrusted: only `models`/`effort` of an existing agent, to global-provider models, else `untrusted_override` on `agents.<id>` | `config/agents.go` `mergeAgents`, `config/merge.go` |
| Catalog | `Effective.Agents` (built-ins first, then custom by id) with defaults, `Source`, `Digest`; `Effective.Agent(id)`; every model in the merged catalog, first one calls tools, names unique case-insensitively | `config/agents.go` `agentCatalog`, `config/load.go` |
| Retirement | `subagentModel` → `ErrSubagentModelRetired` naming the replacement; `MigrateSubagentModel(path)` rewrites through `Save` with the read digest | `config/agents.go`, `config/schema.go` |
| Runtime | a read-only child's model is `explore`'s first model, an execution task's is `worker`'s, else the parent's pin | `runtime/tasks.go` `childModelRef`, `runtime/subagents.go` |
| Docs | configuration section, child model rule | `docs/go-provider.md`, `docs/adr/0009-…`, `docs/adr/README.md` |

## Tests

New, `config/agents_test.go`:

- `TestBuiltinAgentsWithoutConfiguration` — catalog order, ceilings, eligibility with no file.
- `TestBuiltinAgentFixedFieldsAreRejected` — each fixed field of a built-in is refused with its path.
- `TestDelegationAgentRules` — delegation ceiling/description/no-delegation, blank name, duplicate model, byte bound, 17 custom agents; `Save` refuses an invalid typed document.
- `TestSubagentModelIsRetiredWithItsReplacementNamed`, `TestMigrateSubagentModel` — load error before, rewrite keeps unrelated keys and an existing `worker` list, second run is a no-op.
- `TestAgentCatalogDefaultsAndReferences` — custom defaults, unknown model, first model without tool calling, name colliding with a built-in (case-insensitive); a later non-tool model is accepted.
- `TestUntrustedWorkspaceAgents` — models/effort of existing agents applied; instructions, workspace-provider models and new agents dropped with warnings.
- `TestTrustedWorkspaceAgents` — built-in field-by-field override, custom whole replacement, workspace provider models, digest changes.

Changed: config merge tests and fixtures moved from `subagentModel` to `agents.explore`; four new invalid schema fixtures (`agent-id-pattern`, `agent-models-empty`, `agent-use-unknown`, `agent-unknown-field`); the valid `global.json` fixture carries built-in overrides and a custom delegation agent (typed round trip); `runtime/models_test.go` `TestChildUsesTheExploreAgentModelWhenConfigured`.

Run on this change: `go test -count=1 ./...` (all packages ok), `go build ./...` and `go vet ./...` (clean).

## Compatibility

- **Breaking:** a `lumencortex.json` that sets `subagentModel` no longer loads. The error names the replacement and `config.MigrateSubagentModel` fixes it in one call. The owner's own global file does not set the key (checked: 0 occurrences).
- Desktop at `af4fcf9` builds against `efe9b66` and still references `config.Effective.SubagentModel` / `File.SubagentModel`; it moves to this Core together with the A2 Desktop change (pin, `MigrateSubagentModel` prompt, Agents page on the catalog).

## A2 Core — thread agents, candidate lists, ceilings

- **Scope:** a session runs and pins a catalog agent; the agent's list is its failover chain with per-entry effort; its ceilings cap the host's grant; `AgentOptions.Instructions` removed. Children still cannot choose an agent (A3).

| Area | Change | Files |
|---|---|---|
| Persistence | migration 0005: `sessions.agent_id TEXT` with the id-pattern `CHECK`, no backfill (NULL = default agent on the next Start, model pin unchanged); down migration restores the table | `internal/migrate/sql/0005_agent.sql`, `internal/migrate/sql/down/0005_agent.down.sql`, `internal/session/store.go`, `internal/session/start.go` (`RunStart.AgentID`, written in the BeginRun transaction) |
| Agent rule | pure `AgentDecision(requested, stored, default, grant, lookup)`: rules `agent.user` / `agent.session` / `agent.default`; grant = min(host grant, agent ceilings); one `agent` decision with grant, ceilings, catalog digest; replayed by `ReplayDecision` | `internal/agent/agents.go`, `internal/agent/policy.go` |
| Model rule | `ResolveModelRef`/`ModelDecision` take the agent's first model and whether the agent switched: new rule `model.agent`; an explicit `ModelRef` still wins (`model.user`); old records replay unchanged | `internal/agent/policy.go` |
| Per-candidate effort | `agent.Options.CandidateEfforts`; the loop applies a candidate's own default and auto ceiling, the per-run override still wins over all | `internal/agent/types.go`, `internal/agent/send.go` |
| Start | `AgentOptions.Agent`; `planModels(startRequest)` resolves agent → model → candidates `[pin] + list` (child without a list: parent agent's list, else `failover`) → efforts; auto is resolved once when any candidate needs it; `ErrUnknownAgent` (also a removed pinned agent — never a silent fallback), `ErrAgentNotForConversation`; `SessionInfo.Agent` | `runtime/agent.go`, `runtime/models.go`, `runtime/engine.go` |
| Children | default agent `explore` (subagent) / `worker` (execution task, also on host resume); no longer inherit the thread's effort override; delegation stays none | `runtime/subagents.go`, `runtime/tasks.go` |
| Instructions | `AgentOptions.Instructions`, `InstructionsError` and sentinels removed; the agent's instructions are appended and logged as digest + length (`instructions.appended`) | `runtime/instructions.go` |
| CLI / docs | `LCX_AGENT`; `lcx-go agents` prints the merged catalog (instructions by size); run semantics paragraph | `cmd/lcx-go/main.go`, `docs/go-provider.md` |

Tests (new, `runtime/agents_test.go`, renamed from `instructions_test.go`):

- `TestAgentInstructionsAppendedAfterDefaultPrompt` — last system message; switching to `ask` drops them.
- `TestAgentInstructionsEvidenceIsDigestOnly` — digest/length only; text absent from step records, transcript and events; trace replays.
- `TestAgentCeilingsCapTheGrant` — `ask` under a host grant of `full` + `isolated` exposes no shell/write/`start_task`; logged grant is read-only/none.
- `TestAgentRefusalsWriteNothing` — unknown, delegation-only and `explore` refused with no row and no pin; a removed pinned agent is refused until the host names one.
- `TestSessionAgentPinning` — default pin, switch re-pins model (`model.agent`), stay, explicit model wins over a switch, switch to an agent without a list re-pins `model`, pre-0005 session keeps its model.
- `TestAgentListFailoverWithPerEntryEffort` — a@high fails (availability), b answers at its own max, pin stays a; a user model outside the list is tried first at the agent's effort; per-run override wins; a list edit changes fallbacks, not the pin.
- `TestChildInheritsParentListButNotEffortOverride` — explore child falls back through the parent agent's list; no `effort.override` (mutation-checked: re-adding the inheritance fails the test).
- `internal/agent/policy_test.go` `TestResolveModelRefPrecedence` (agent cases, every row replays), `TestResolveAgentAndCeilings`.
- `internal/session` / `internal/migrate`: `BeginRun` writes `agent_id` atomically and a failed BeginRun leaves it; downgrade chain 5→4→3→2 and re-upgrade; `Latest() == 5`.

Changed: exact decision traces in `runtime/models_test.go` gain the leading `agent.default` decision, and model-rule assertions select by kind instead of position.

Hidden reasoning: `agent_id` stores a catalog id only; nothing model-written enters it. The existing reasoning-echo regression tests (`runtime/echo_test.go`) pass unchanged in the full run.

Run on this change: `go test -count=1 ./...` (all 24 packages ok; `runtime` re-run after the missing-parent fix, ok), `go build ./...` and `go vet ./...` clean.

Compatibility: **breaking for hosts** — `AgentOptions.Instructions` no longer exists (Desktop is the only caller and moves in the paired A2 Desktop change). Databases upgrade to user_version 5; `Downgrade` to 4 drops the column. A new thread's behaviour without `Agent` is the built-in `agent` with no ceiling, i.e. the previous behaviour.

## A3 — agent choice in the delegation tools

- `internal/toolset/subagents.go`: `AgentCatalogLines`, `AgentIDs`, `AgentParameter`; `Options.SubagentAgents` builds `spawn_subagent`'s optional `agent` enum (default `explore`) and description lines; an id outside the enum errors before `SubagentController.Spawn(ctx, goal, agentID)` is called.
- `internal/agent/delegation.go`: `Options.TaskAgents` does the same for `start_task` (default `worker`); `TaskStarter.StartTask(ctx, objective, agentID)`. Under `propose`, the proposal records the agent and `taskstate.TaskBounds` (max steps, tool calls per step, max tokens, denylist) of the proposing run, which fixes A-F8 (a user-started proposal ran without the parent's bounds).
- `config.Effective.DelegationAgents(eligible)`; `modelPlan.spawnAgents/taskAgents` read once per Start with the rest of the configuration.
- `runtime`: `Engine.delegationAgent` re-checks the agent against the configuration of that moment in `subagentController.Spawn`, `startExecutionTask` and `resumeUnstartedTask`, before any session, relation or worktree exists (`ErrUnknownAgent`, new `ErrAgentUseMismatch`). The launcher passes the agent as the child's default agent; the child's first Start pins it, and later Starts (host resume, Reconcile) run the pin. **Bug found by the new test and fixed:** `constrainExecutionChild` overwrote the launcher's agent with `worker` on the first Start; it now keeps a launcher-set value (unexported, never host-supplied).
- Checkpoints `subagent.spawn`, `task.started`, event `subagent.spawned` and relation metadata carry `agent`; `SubagentNode.Agent`, `TaskResult.Agent` (delivered to the parent model in the `task_result` row) report it.
- `ErrUnknownAgent` is now `agent.ErrAgentNotInCatalog` (one sentinel; message `agent is not in the catalog: "id"`), committed with `lcx-go agents` in `67ad2ca`.

Tests (new): `runtime/delegation_agents_test.go`:
- `TestDelegationToolsOfferOnlyEligibleAgents`: acceptance 1.
- `TestSpawnRunsTheChosenAgentAndResumeKeepsIt`: acceptance 2 and 5. The child pins `audit`, runs its model, and gets its instructions after the launch prompt with no write tool. Ineligible and unknown ids start nothing. After a crash, Reconcile and a host resume asking `full`, the child still runs `audit` read-only.
- `TestStartTaskRefusesAnIneligibleAgent`: acceptance 3. A tool error lists the valid ids; no session, worktree or Task State entry is created.
- `TestProposalRecordsAgentAndParentBounds`: acceptance 4. The child gets `max_tokens` 500 and the denylist. The tree and `task_result` name `fixer`. A proposal whose agent became read-only is refused at Start without creating a worktree.
- `TestChildRunsItsAgentEffortNotTheThreadOverride`: the handoff's A3 extra. The thread asks `max`; the child of `explore {effort: low}` sends `low`.

`internal/toolset/subagents_test.go`:
- `TestSpawnSubagentRunsOnlyEligibleAgents`.
- `TestAgentCatalogLinesAreBounded`: acceptance 6. A full catalog of 16 maximal read-only delegation agents measures exactly the bound, 16 × (5 + 40 + 280) bytes plus the `explore` line, which is under 6 KiB.

`internal/agent/schema_test.go` now validates both enums' JSON Schema.

Not run: the advisory selection probe on a real model (acceptance "plus one advisory selection probe") needs an owner-approved model and cost; not executed.

Hidden reasoning: the new Task State fields hold a catalog id and numeric/denylist bounds copied from run options; nothing model-written beyond the enum-checked id. `TestProposedTaskRunsConfinedAndDeliversItsResultOnce` (child reasoning never in the database) passes unchanged.

Run on this change: `go test -count=1 ./...` (all packages ok), `go build ./...`, `go vet ./...` clean. `gofmt -l` lists only files not touched here (pre-existing).

Compatibility: Go API — `toolset.SubagentController.Spawn` and `agent.TaskStarter.StartTask` gain an `agentID` parameter (in-repo implementations only). Wire — tool calls without `agent` behave as before; Task State and read-model fields are additive and omitted when empty; proposals recorded before A3 start `worker` without bounds, as before.

## Open

- Neither repo is pushed; Desktop `c82a039` pins Core `8998c25`, so Core must be pushed first for a clean Desktop module download. N3 (auth-error failover) stays a separate owner decision.
