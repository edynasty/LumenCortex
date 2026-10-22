# ADR-0013: Project inputs and workspace trust

- **Status:** Accepted
- **Date:** 2026-10-22
- **Owners:** project owner
- **Supersedes:** none
- **Superseded by:** none
- **Design:** `../LumenCortex-architecture-audit/design/README.md` A8 (extended here from providers and checks to every project input), `handoffs/phase0-and-workbench-plan-2026-10-22.md` §2.3, §2.4, §2.5 and §2.6.

## Context

Core already refuses project-scope providers, checks and agents from an untrusted workspace: the global file's `trustedWorkspaces` names the directories whose `lumencortex.json` may define them, and `config.Load` drops everything else with an `untrusted_override` warning. Three kinds of project input were outside that rule:

- **Skills.** `runtime/engine.go` built the skill registry from `<workspace>/.lumencortex/skills` without looking at trust, and `prepareRun` injected the resulting text into the system prompt of every run, including child runs. A cloned repository could therefore put arbitrary instructions in front of the model by committing `.lumencortex/skills/<id>/SKILL.md`.
- **MCP servers.** `<workspace>/.lumencortex/mcp.json` was loaded and its servers could be started from the MCP panel. The server chooses the command that runs; nothing said the workspace had to be trusted first. A server's `readOnlyHint` annotation, which decides whether a read-only run may call its tools, is declared by the server itself.
- **Project instructions.** Core read no `AGENTS.md` at all, so the one project input every other agent harness honours was missing; adding it without a rule would have introduced the same hole.

The workspace is not one trust domain. The user's own inputs — `<GlobalDir>/AGENTS.md`, `<GlobalDir>/skills`, the global `mcp.json` — are configuration the user wrote. A workspace's inputs are repository content: whoever can commit to the repository controls them, and a cloned third-party repository is exactly the case the boundary exists for.

## Decision

1. **One trust decision, evaluated per run and per call.** Trust is `config.Effective.Trusted`: the Engine workspace, canonicalized (`filepath.EvalSymlinks` + `Abs`), compared for **equality** against the global file's `trustedWorkspaces`. It is never inherited from a parent directory, never granted by a workspace file, and never cached on the `Engine`: `prepareRun` reads it from the same `config.Load` that resolves the run's models, so a change in the global file applies to the next run and to the next MCP call.

2. **Project inputs are gated; global inputs are not.**

   | Input | Path | Untrusted workspace |
   |---|---|---|
   | Skills | `<workspace>/.lumencortex/skills/*` | not read; the run's `skills.loaded` event names the withheld ids |
   | MCP servers | `<workspace>/.lumencortex/mcp.json` | ignored entirely; a project-only server is refused with `ErrUntrustedProject` and nothing is spawned |
   | Project instructions | `<workspace>/AGENTS.md`, else `CLAUDE.md` | not read; the effective prompt reports it as withheld |
   | Providers, checks, agents | `<workspace>/lumencortex.json` | already gated (ADR-0009 D4 and its predecessors) |
   | Global instructions, skills, MCP | `<GlobalDir>/**` | always read: the user's own configuration |

3. **Project inputs are read from the Engine workspace, never from a session worktree.** The configuration, the instruction files and the skills all come from the directory the Engine was opened on. A session that owns a worktree runs with that worktree as its tool workspace, but its instructions are the ones the user sees. Without this rule an agent could edit `AGENTS.md` in its own worktree and have the edit govern the same run.

4. **Instruction files.** `<GlobalDir>/AGENTS.md` is read always. The project file is `<workspace>/AGENTS.md`, or `<workspace>/CLAUDE.md` when there is no `AGENTS.md`; one file, never both, because two sets of project instructions would be a second source of truth. Subdirectory `AGENTS.md` and `.cursorrules` are not read (deferred, CTX-002). Each file is capped at 64 KiB: a longer file contributes its beginning and the layer carries a truncation marker and a warning, because an instruction file that is too long should degrade, not fail the run. A project file that is a symlink resolving outside the workspace, or that is not a regular file, is refused and reported; the confinement rule is `repository.ConfinedFile`, the same one note cites use. The global file is not confined: the user may point it at a dotfiles checkout.

5. **Prompt order.** The system prefix is `SystemPrompt` (purpose and authority, or a host prompt) → global instructions → project instructions → skills → agent instructions. Every layer but the base prompt and the agent's instructions is a system message of its own, labelled with its source (`## Project instructions (from /path/AGENTS.md)`), so the model can attribute what it reads. The agent's instructions stay unlabelled and last (ADR-0009).

6. **Children inherit the layers.** A child run goes through the same `prepareRun` assembly, so it gets the same global instructions, project instructions and skills, gated by the same trust of the Engine workspace. A child is not a way to reach a project input the parent could not.

7. **No self-elevation.** A project file can never widen its own reach:
   - it cannot add itself to `trustedWorkspaces` (only the global file is read for trust, and a workspace entry for it is dropped with `untrusted_override`);
   - its MCP server entries are ignored while the workspace is untrusted, so a project server cannot declare its tools read-only to reach a read-only run;
   - a tool permission override in `mcp.json` (`tools: {"<name>": {"permission": "read"}}`, P0-E1) applies only from the global file or from a trusted project; an untrusted project's override is ignored and the server's own `readOnlyHint` annotation decides;
   - its skills and instructions are not read at all.

8. **Trust is reported, not inferred by the host.** `Engine.EffectivePrompt` returns `trusted`, every layer with `included` and `reason`, and every tool with the filter that withheld it. A host shows that; it does not re-implement the rule.

## Invariants

- No prompt a run sends contains text read from an untrusted workspace's project inputs.
- No MCP process named by an untrusted workspace's `mcp.json` is started.
- Trust is read from the global file only, per run and per call, from the Engine workspace; a session worktree's path never grants or revokes trust.
- A project input can only narrow what the project gets, never widen it.
- Global inputs are read in every workspace, trusted or not.

## Alternatives considered

### Alternative A: trust at project open, stored in the session

Rejected: trust would become durable session state that can be stale, and it would be decided once per session instead of per run. The configuration's per-run load is already the single source.

### Alternative B: a project-level allow-list written by the user into the workspace

Rejected: the file that would grant permission lives in the repository, so whoever can commit can grant it. Only the global file may name trusted workspaces.

### Alternative C: read project inputs but label them "untrusted" in the prompt

Rejected: prompt-level labelling is advisory. A model that follows the text has already read it; the boundary has to be before the text enters the prompt.

## Consequences

### Positive

- A cloned repository can no longer inject instructions, skills or an MCP server command into a run by committing `.lumencortex/`.
- The user's global setup keeps working everywhere, so the gate costs nothing in the common case.
- `EffectivePrompt` makes the boundary visible: a host can show exactly what was withheld and why.

### Negative / trade-offs

- A user who works in a new repository sees neither its skills nor its `AGENTS.md` until they trust it, and the only path to trust is the global configuration (the host's "trust this workspace" action writes it).
- Two `config.Load` calls per run are possible where one was (the MCP registry's trust callback loads the configuration itself); both read the same files.
- A workspace `AGENTS.md` larger than 64 KiB is only partly read.

### Residual risk: SECURITY-002

`.lumencortex/` is written into `.git/info/exclude`, but a repository can still commit the directory, including `lumencortex.db`. The state database is opened from that path and its provenance is not checked, so a repository that ships a database is trusted to have produced it. Not addressed here; tracked as SECURITY-002 (P2).

## Persistence / compatibility impact

None. No schema change, no new persisted state: trust is configuration, and the layers are assembled per run. The public shapes that change are additive: `skills.loaded` gains `excluded` and `reason` (both absent when nothing was withheld), and `Engine.EffectivePrompt` is new.

## Validation plan

- `internal/skills`: a prompt without `IncludeProject` carries the global scope only and reports the withheld project ids; with it, both.
- `internal/instructions`: trusted/untrusted, the `CLAUDE.md` fallback, `AGENTS.md` winning over it, a symlink out of the workspace refused, truncation at 64 KiB with a warning, missing files reported, and the global file read through a symlink.
- `runtime`: a real run in an untrusted workspace sends the global inputs and none of the project's, and `skills.loaded` names the withheld ids and the reason; the same holds for a child run; `EffectivePrompt` with a session whose worktree holds a different `AGENTS.md` reports the Engine workspace's file; the contract test compares a real run's system messages and tool specs with `EffectivePrompt`'s report for the same inputs; `EffectivePrompt` writes no row and works without a session.
- MCP (implemented with P0-B's MCP half): a project server is refused while the workspace is untrusted, no process is spawned, and the listing marks it `blockedByTrust`.
- Manual: a repository with `.lumencortex/skills/evil/SKILL.md` and a project `mcp.json`, untrusted, shows neither in the effective prompt and cannot start the server; after trusting, both appear.

## Documentation impact

- `docs/security-and-trust.md` gains a "Workspace trust" section: the rule, the gated inputs, the self-elevation limits and SECURITY-002.
- `docs/adr/0009-agent-catalog-and-model-lists.md` gains an amendment note (the effective prompt is Core-assembled and exposed; the Ask role's text states what a read-only run can do).
- Design A8 is extended in the audit repository from providers and checks to skills, MCP and project instructions.
