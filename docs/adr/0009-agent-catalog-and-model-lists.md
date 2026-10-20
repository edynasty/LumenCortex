# ADR-0009: Agent catalog and per-agent model lists

- **Status:** Accepted (owner direction 2026-10-20: one Core-owned agent system, built-in and user-defined agents, per-agent ordered model candidates with effort and fallback, no separate model roles). Phases A1 (catalog, config only), A2 (thread agents, lists, ceilings; Desktop on the catalog) and A3 (agent choice in the delegation tools) are implemented.
- **Date:** 2026-10-20
- **Owners:** project owner
- **Supersedes:** the `subagentModel` configuration key; Desktop's `agents.json` profiles (retired when Desktop moves onto the Core catalog in A2)
- **Superseded by:** none
- **Design:** `../LumenCortex-architecture-audit/design/handoffs/agent-architecture-2026-10-20.md` (revision 2), decisions D1–D9 and N1–N2. N3 (authentication-error failover) is a separate decision and not part of this record.

## Context

Core had no agent concept. Five unrelated mechanisms described what a run may do (`Policy`, the `Delegation` grant, relation kind, metadata `mode`, `subagentModel`), and Desktop kept named profiles in its own `agents.json` that Core never saw: only their instructions and permission base reached a Start. The consequences (handoff §1): the model cannot choose a specialist, a thread does not remember its agent, the CLI cannot reproduce a Desktop thread, defaults are stored three times, and `subagentModel` is one hidden model shared by read-only explorers and file-changing workers.

The owner also requires that each agent has its own ordered model candidates with reasoning effort and a reliable fallback, without a second "model roles" system that duplicates agent configuration.

## Decision

1. **Core owns the catalog** as the `agents` key of `lumencortex.json`. An agent is a named starting point for a run: `instructions`, an optional ordered `models` list (`{model, effort?}`, ≤ 8), a default `effort`, and authority **ceilings** (`policy`, `delegation`). `use` says where it runs: `conversation`, `delegation` or `both`.

2. **Four built-ins** with fixed identity and ceilings; a file may set only their `instructions`, `effort` and `models`:

   | id | use | policy ceiling | delegation ceiling | replaces |
   |---|---|---|---|---|
   | `ask` | conversation | read-only | none | Desktop Ask profile |
   | `agent` | conversation | full (no cap) | isolated (no cap) | Desktop Agent profile; the default |
   | `explore` | delegation | read-only | none | `spawn_subagent`'s fixed child |
   | `worker` | delegation | workspace | none | `start_task`'s fixed child |

   Up to 16 custom agents. A delegation agent must cap `policy` at `read-only` (eligible for `spawn_subagent`) or `workspace` (eligible for `start_task`) and must have a `description`, because the model chooses delegation agents by that text. Delegation agents never hold a delegation grant. No Planner or Reviewer built-in: neither has distinct behaviour today (a reviewer needs read access to a task's worktree first).

3. **One list concept.** An agent's `models` list *is* its failover chain. Top-level `model` + `failover` remain the default for an agent without its own list: `model` pins a new session and `failover` follows the pinned model, exactly as before agents had lists (so a session pinned to another model never needs the default model's credentials). Children without their own list inherit the parent's effective list. No model roles.

4. **Trust.** A trusted workspace may add or replace custom agents (whole) and override built-ins field by field. An untrusted workspace may set only `models` and `effort` of an agent that already exists, and only to models of global providers; any other agent entry is dropped with `untrusted_override`. This is the rule `model` and `failover` already follow.

5. **Validation.** The schema bounds every field. Per file: built-in fixed fields are refused; delegation ceilings and description are required; instructions ≤ 16 KiB valid UTF-8; description ≤ 280 bytes; no model twice in one list; ≤ 16 custom agents. After the merge: every listed model is in the merged catalog, an agent's first model calls tools (later ones may not: the step filter skips them), and names are unique regardless of case. Each merged agent carries a `Digest` of its definition and the `Source` it came from.

6. **`subagentModel` is retired.** A file that sets it fails to load with `ErrSubagentModelRetired`, which names the replacement; `config.MigrateSubagentModel(path)` rewrites the file (the model becomes the `explore` and `worker` list unless the file already gives them one) through `Save` with the read digest. A child now starts on the first model of `explore` (read-only) or `worker` (execution), else on its parent's pin.

7. **Run semantics (A2, A3).** A session pins the agent it runs, as it pins its model; switching agent re-pins the model to the new agent's first model unless the same Start names one. Candidates are `[pin] + list`, deduplicated, then the existing filters; editing a list changes fallbacks immediately but never moves a pin. Each candidate's effort is its own entry, else the agent's, else `effort.default`; a thread's per-run override still wins, but children do not inherit it. Effective authority is the lower of the host's grant and the agent's ceiling, further narrowed by the launch kind. `AgentOptions.Instructions` is replaced by naming an agent. `spawn_subagent`/`start_task` gain an optional `agent` parameter offered as a closed, described list of eligible agents; omitting it runs `explore`/`worker`, i.e. today's behaviour.

## Invariants

- An agent's text (instructions, description) can never widen tools, policy, delegation or permissions: ceilings only narrow, and the launch kind fixes a child's authority whatever agent it runs.
- A workspace cannot introduce an agent, instructions or a model outside the global providers unless the user trusts it.
- The catalog is configuration: nothing model-written is stored in it, and no agent is created at run time.
- Fail-fast preflight stays: an unknown model or one without tool calling in first position is a configuration error, never a silently skipped fallback.

## Consequences

- Positive: one place defines agents for Desktop, CLI and any host; specialists become possible without new harness machinery (they are ordinary runs with a preset); `subagentModel`'s two roles split into `explore` and `worker`.
- Negative: a breaking configuration change for files that set `subagentModel` (load error with a one-call migration); Desktop's `agents.json` must be imported once in A2.
- Deferred: auto-generated or self-saved agents, agent composition, per-agent tool allowlists, per-spawn model/effort choice, subagent depth > 1, a Reviewer built-in.

## Phases

- **A1 (implemented):** schema, types, built-ins, validation, trust merge, `Effective.Agents`/`Agent`, `subagentModel` retirement and migration, children's model from `explore`/`worker`. Report: [`docs/reports/agent-catalog-2026-10-20.md`](../reports/agent-catalog-2026-10-20.md).
- **A2:** thread agents, candidate lists with per-entry effort, ceilings, `agent` decision, `Instructions` removed (Core implemented, same report); Desktop on the Core catalog (implemented, Desktop `c82a039`, report `docs/reports/agent-catalog-desktop-2026-10-20.md`).
- **A3 (implemented):** `spawn_subagent {goal, agent?}` and `start_task {objective, agent?}` carry a closed enum of the eligible agents (`SpawnEligible`: delegation use, read-only ceiling; `TaskEligible`: delegation use, workspace ceiling) and list them as `- id: description` lines. An id outside the enum is a tool error that starts nothing. The launcher checks the agent again against the configuration of that moment (`ErrUnknownAgent`, `ErrAgentUseMismatch`) before any session or worktree exists. The child pins the agent on its first Start; a resume runs the pin. A proposal records `tasks[].agent` and the proposing run's step, output and denylist bounds (`tasks[].bounds`), which `StartProposedTask` applies. `SubagentNode.agent` and `TaskResult.agent` report it. Same report; Desktop `1714459` shows the agent on task cards and the fallback notice.
