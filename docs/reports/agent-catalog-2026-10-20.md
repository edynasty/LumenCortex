# Agent catalog and model lists — phase A1 (2026-10-20)

- **Scope:** ADR-0009 phase A1, Core only: the `agents` configuration key, built-ins, validation, trust merge, the merged catalog view, and the retirement of `subagentModel`. No run consumes thread agents, ceilings or per-candidate effort yet (A2), and children cannot choose an agent yet (A3).
- **Baseline:** Core `efe9b66` (ADR-0008 concurrency).
- **Status:** implemented and validated locally; not pushed.
- **Design:** [ADR-0009](../adr/0009-agent-catalog-and-model-lists.md); handoff `../LumenCortex-architecture-audit/design/handoffs/agent-architecture-2026-10-20.md` (not version-controlled).

## Changes

| Area | Change | Files |
|---|---|---|
| Schema | `agents` (id pattern `^[a-z][a-z0-9-]{0,39}$`, ≤ 20 entries), `$defs/agent`, `$defs/agentModel`; `subagentModel` removed; `trustedWorkspaces` text names agents | `docs/lumencortex.schema.json` |
| Types | `Agent`, `AgentModel`, `AgentUse`, `AgentInfo` (`ForConversation`, `ForDelegation`, `SpawnEligible`, `TaskEligible`), built-in ids, bounds, policy/delegation value constants | `config/agents.go`, `config/types.go` (`File.Agents`; `File.SubagentModel` removed) |
| Per-file rules | built-in fixed fields refused; delegation agents need a read-only/workspace policy and a description and no delegation; ≤ 16 custom; ≤ 280-byte description; ≤ 16 KiB UTF-8 instructions; no duplicate model in a list — in `Parse` and `Validate` (so `Save` too) | `config/agents.go` `validateAgents`, `config/schema.go` |
| Merge | trusted workspace: custom agents replaced whole, built-ins field by field; untrusted: only `models`/`effort` of an existing agent, to global-provider models, else `untrusted_override` on `agents.<id>` | `config/agents.go` `mergeAgents`, `config/merge.go` |
| Catalog | `Effective.Agents` (built-ins first, then custom by id) with defaults, `Source`, `Digest`; `Effective.Agent(id)`; `Effective.DefaultModels()` = `model` + `failover`; every model in the merged catalog, first one calls tools, names unique case-insensitively | `config/agents.go` `agentCatalog`, `config/load.go` |
| Retirement | `subagentModel` → `ErrSubagentModelRetired` naming the replacement; `MigrateSubagentModel(path)` rewrites through `Save` with the read digest | `config/agents.go`, `config/schema.go` |
| Runtime | a read-only child's model is `explore`'s first model, an execution task's is `worker`'s, else the parent's pin | `runtime/tasks.go` `childModelRef`, `runtime/subagents.go` |
| Docs | configuration section, child model rule | `docs/go-provider.md`, `docs/adr/0009-…`, `docs/adr/README.md` |

## Tests

New, `config/agents_test.go`:

- `TestBuiltinAgentsWithoutConfiguration` — catalog order, ceilings, eligibility with no file.
- `TestBuiltinAgentFixedFieldsAreRejected` — each fixed field of a built-in is refused with its path.
- `TestDelegationAgentRules` — delegation ceiling/description/no-delegation, blank name, duplicate model, byte bound, 17 custom agents; `Save` refuses an invalid typed document.
- `TestSubagentModelIsRetiredWithItsReplacementNamed`, `TestMigrateSubagentModel` — load error before, rewrite keeps unrelated keys and an existing `worker` list, second run is a no-op.
- `TestAgentCatalogDefaultsAndReferences` — custom defaults, default list, unknown model, first model without tool calling, name colliding with a built-in (case-insensitive); a later non-tool model is accepted.
- `TestUntrustedWorkspaceAgents` — models/effort of existing agents applied; instructions, workspace-provider models and new agents dropped with warnings.
- `TestTrustedWorkspaceAgents` — built-in field-by-field override, custom whole replacement, workspace provider models, digest changes.

Changed: config merge tests and fixtures moved from `subagentModel` to `agents.explore`; four new invalid schema fixtures (`agent-id-pattern`, `agent-models-empty`, `agent-use-unknown`, `agent-unknown-field`); the valid `global.json` fixture carries built-in overrides and a custom delegation agent (typed round trip); `runtime/models_test.go` `TestChildUsesTheExploreAgentModelWhenConfigured`.

Run on this change: `go test -count=1 ./...` (all packages ok), `go build ./...` and `go vet ./...` (clean).

## Compatibility

- **Breaking:** a `lumencortex.json` that sets `subagentModel` no longer loads. The error names the replacement and `config.MigrateSubagentModel` fixes it in one call. The owner's own global file does not set the key (checked: 0 occurrences).
- Desktop at `af4fcf9` builds against `efe9b66` and still references `config.Effective.SubagentModel` / `File.SubagentModel`; it moves to this Core together with the A2 Desktop change (pin, `MigrateSubagentModel` prompt, Agents page on the catalog).

## Open

- A2/A3 per ADR-0009. N3 (auth-error failover) stays a separate owner decision.
