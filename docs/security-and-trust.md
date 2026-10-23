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

## Workspace trust

The workspace is not one trust domain. The user's own inputs are configuration the user wrote; a
workspace's inputs are repository content, and whoever can commit to the repository controls them
(ADR-0013).

**The rule.** Trust is `config.Effective.Trusted`: the Engine workspace, canonicalized (symlinks
resolved), compared for **equality** — never as a prefix — against `trustedWorkspaces` in the global
file `<GlobalDir>/lumencortex.json`. Nothing else grants it: a workspace file cannot name itself
trusted (such an entry is dropped with `untrusted_override`), and a subdirectory of a trusted
workspace is not trusted. Trust is read per run and per MCP call from the Engine workspace, never
cached on the Engine, so an edit to the global file applies to the next run.

**What it gates.**

| Input | Path | Untrusted workspace |
|---|---|---|
| Skills | `<workspace>/.lumencortex/skills/*`, `<workspace>/.agents/skills/*`, `<workspace>/.claude/skills/*` | not read; `skills.loaded` names the withheld ids |
| MCP servers | `<workspace>/.lumencortex/mcp.json` | ignored; a project-only server is refused (`ErrUntrustedProject`) and nothing is spawned |
| Project instructions | `<workspace>/AGENTS.md`, else `CLAUDE.md` | not read; the effective prompt reports it as withheld |
| Providers, checks, agents | `<workspace>/lumencortex.json` | dropped with `untrusted_override` |

**What it never gates.** `<GlobalDir>/AGENTS.md`, the global skill roots (`<GlobalDir>/skills`,
`~/.agents/skills`, `~/.claude/skills`) and the global `mcp.json` are the user's own configuration
and are read in every workspace.

**Skills are read in place and loaded on demand (ADR-0015).** Skills in another tool's root, or
behind a symbolic link, are never written, copied or deleted; their enablement lives in
LumenCortex's `skills-state.json`. The prompt carries only a catalog of ids and descriptions;
`read_skill` returns a listed skill's files, confined (also through links) to the directory the
skill resolves to, text only and bounded.

**Read from the Engine workspace, not the worktree.** Configuration, instruction files and skills come
from the directory the Engine was opened on, even when a session runs in its own worktree. An agent
therefore cannot edit the instructions that govern it and have the edit apply to the same run.

**Instruction files.** One global file and, in a trusted workspace, one project file (`AGENTS.md`, or
`CLAUDE.md` when there is no `AGENTS.md`), each capped at 64 KiB: a longer file is truncated with a
visible warning rather than refused. A project file that is a symlink resolving outside the workspace,
or that is not a regular file, is refused. Subdirectory `AGENTS.md` files are not read.

**No self-elevation.** A project can only narrow what it gets:

- its `mcp.json` entries are ignored while the workspace is untrusted, so a project server cannot
  declare its own tools read-only to reach a read-only run;
- an MCP tool permission override (`tools: {"<name>": {"permission": "read"}}`) applies only from the
  global file or from a trusted project; an untrusted project's override is ignored and the server's
  own `readOnlyHint` annotation decides;
- its skills and instructions are not read at all.

**Visible, not inferred.** `Engine.EffectivePrompt` returns the trust flag, every prompt layer with
`included` and the reason it was withheld, and every tool with the filter that withheld it (`policy`,
`allowlist`, `denylist`, `delegation`). A host displays that; it never re-implements the rule.

**Residual risk (SECURITY-002).** A repository can commit `.lumencortex/`, including
`lumencortex.db`. Core opens the state database from that path without checking its provenance, so a
repository that ships a database is trusted to have produced it. Not mitigated yet (P2).

## MCP trust boundary

MCP tools are external integrations.

Read-only annotations can reduce permission requirements, but external systems remain outside the LumenCortex workspace trust boundary. An annotation is declared by the server itself; a project server's tools reach a run only when the workspace is trusted (see Workspace trust).

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

Provider credentials are references in lumencortex.json, never values: `apiKey` is `{env:NAME}`, read from the process environment, or `{secret:NAME}`, resolved when a provider client is built. Literal keys are rejected by the schema.

`{secret:NAME}` values live in Core's file-backed credential store (`config.FileCredentialStore`): one JSON file of name to value at GlobalDir/credentials.json, shared by the CLI and Desktop. The store is not a secrets manager and is not encrypted at rest: it is protected by file permissions (0600 in a 0700 directory, verified fail-closed on every read) and the user account, the same trust level as an environment variable. Writers are serialized by an advisory lock and replace the file atomically (temporary file, fsync, rename). The store is never exported, logged or sent to a frontend, and no API returns every value.

A secret name is the only credential detail that may appear in diagnostics: no error, warning, event, ModelInfo/ProviderInfo JSON or config export ever carries a secret value, and a resolver's own error text is dropped in favor of one that names only the reference. The workspace trust rules treat a secret reference exactly like an environment reference: redirecting a global provider's baseURL or endpoint requires the workspace's own apiKey, of either form, and the merged provider never inherits the global headers.

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
