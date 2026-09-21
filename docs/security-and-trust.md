# Security and trust model

LumenCortex is an autonomous coding runtime that can read files, edit a workspace, execute shell processes, talk to language servers, and call MCP tools. Safety therefore depends on multiple independent boundaries rather than one prompt instruction.

## Security layers

```text
User / Operator
     │
     ▼
Permission Policy
     │
     ▼
Workflow Action Tool Boundary
     │
     ▼
Tool Schema + Argument Validation
     │
     ▼
Workspace / Host / External Scope
     │
     ▼
Execution-time Checks
     │
     ▼
Tool implementation
     │
     ▼
Evidence + Session persistence
```

No single layer should be treated as sufficient by itself.

## Permission policies

Three policy tiers exist:

| Policy | Workspace read | Workspace write | Host shell | External MCP write |
|---|---:|---:|---:|---:|
| `read-only` | yes | no | no | no |
| `workspace` | yes | yes | no | no |
| `full` | yes | yes | yes | yes |

Unknown policy names fail closed.

For normal autonomous coding with `--yes`, the current CLI default is `full`.

TUI without `--yes` is intentionally read-only.

## Scope model

Tools can declare a scope:

- `workspace`,
- `host`,
- `external`.

Examples:

- file editing tools are workspace-scoped,
- shell is host-scoped,
- MCP write tools are external-scoped.

Permission policy is evaluated against both permission class and scope.

## Workflow tool boundaries

Workflow `allowedTools` is a second independent restriction.

The provider only receives schemas legal for the current Action, and every call is checked again immediately before execution.

This prevents a multi-call assistant turn from continuing to use a tool after an earlier call changed the Workflow Action.

Workflow is therefore runtime enforcement, not advisory prompt text.

## Human Gates

A Human Gate is explicit approval state.

The runtime:

- pauses the Session,
- does not continue consuming LLM requests,
- records the approving actor,
- writes a human-sourced Fact,
- resumes only after explicit approval.

A model cannot approve a Human Gate by claiming approval in text.

## Evidence and trust zones

Cognitive evidence uses provenance-oriented trust zones such as:

- `system_verified`,
- `repo_trusted`,
- `runtime_verified`,
- `user_provided`,
- `external_untrusted`,
- `model_inferred`.

Evidence grade is ordinal provenance, not a probability:

```text
hypothesis < static < tested < runtime < reproduced
```

A model statement does not automatically become evidence.

## Workflow Fact provenance

Workflow Facts store `factSources`.

A tool-derived source can record:

- timestamp,
- Action,
- Outcome ID,
- tool,
- step,
- compact arguments,
- compact parsed result.

Human approval records:

- timestamp,
- actor,
- Action,
- Gate.

This is separate from cognitive Evidence and intentionally simpler.

## Path safety

Workspace tools reject traversal outside the workspace.

Workflow dotted paths reject dangerous segments:

- `__proto__`,
- `prototype`,
- `constructor`.

This blocks prototype-pollution-style Fact paths.

## Shell safety behavior

Shell execution is asynchronous and cancellable.

Current hardening includes:

- process-group termination on Unix-like platforms,
- timeout handling,
- process-tree termination on output overflow,
- bounded captured output,
- live stdout/stderr streaming,
- structured truncation that preserves result metadata,
- cancellation propagation.

However, shell commands with `full` policy remain powerful host-level execution.

## MCP trust boundary

MCP tools are external integrations.

Read-only annotations can reduce permission requirements, but external systems remain outside the LumenCortex workspace trust boundary.

Do not assume an MCP server is safe merely because its schema is valid.

## Workflow fail-closed behavior

The Workflow runtime fails closed on:

- unknown Workflow version,
- invalid/missing Action references,
- unsafe dotted paths,
- definition drift during Session resume,
- unsupported Gate types,
- automatic Route cycles beyond the transition limit.

## Known safety gaps

These are important and should remain explicit in documentation.

### No transactional real Git worktree binding yet

Cognitive Git versions cognitive state, not the real workspace filesystem.

### No whole-run rollback for arbitrary shell mutations

`apply_patch` itself is validated/atomic for its own edit set, but a shell command can make arbitrary changes that are not automatically rolled back.

### External credentials

Provider/MCP credentials are process/environment concerns. The runtime should not be described as a secrets manager.

### Untrusted repository content

Repository text can contain instructions intended to influence the model. The current architecture separates evidence provenance and tool permission, but does not claim complete prompt-injection elimination.

## Recommended operating profiles

### Inspection / audit

```bash
lcx agent "analyze the repository" --policy read-only
```

### Controlled editing without shell

```bash
lcx agent "apply the requested code edits" --policy workspace --yes
```

### Full autonomous coding

```bash
lcx agent "fix and verify the issue" --policy full --yes
```

Use `full` only when host shell execution and external write-capable tools are intended.

## Security documentation rule

Any change that expands:

- tool permission,
- scope,
- shell behavior,
- external integration,
- Workflow completion semantics,
- path handling,
- persistence of sensitive data,

must update this document and add or update validation evidence in the same change.
