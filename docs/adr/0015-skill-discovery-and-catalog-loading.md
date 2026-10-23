# ADR-0015: Skill discovery from cross-agent roots and catalog loading

- **Status:** Accepted
- **Date:** 2026-10-23
- **Owners:** project owner
- **Supersedes:** the "whole SKILL.md in the system prompt" behaviour of the skills layer (ADR-0013 keeps the trust rule)
- **Superseded by:** none

## Context

LumenCortex read skills from two directories of its own: `<GlobalDir>/skills` and
`<workspace>/.lumencortex/skills`. Skills the user installed for other agents were invisible:
`npx skills` / skill installers put them in `~/.agents/skills` (often as symbolic links to a
shared checkout), Claude Code uses `~/.claude/skills`, and OMP reads both `.agent(s)/skills`
roots for the user and the project. On the owner's machine `~/.agents/skills` holds nine skills,
two of them links, one link broken; LumenCortex saw none of them.

The skills layer also put every enabled SKILL.md into the system prompt, up to 256 KiB. Real
installed skills are 3–28 KiB each, so the eight readable ones cost about 110 KiB on every request of every
run, whether the task needed them or not. The Agent Skills format is built for progressive
disclosure: name and description up front, the body when the task calls for it.

## Decisions

### D1 — Roots and precedence

Each scope has an ordered list of roots (`skills.DefaultRoots`):

| Scope | Roots, highest first | Writable |
| --- | --- | --- |
| global | `<GlobalDir>/skills`, `~/.agents/skills`, `~/.claude/skills` | the first only |
| project | `<workspace>/.lumencortex/skills`, `<workspace>/.agents/skills`, `<workspace>/.claude/skills` | the first only |

A skill is `<root>/<id>/SKILL.md`, `id` matching `[A-Za-z0-9._-]{1,64}`. Within a scope the first
root that has an id provides it; the others are listed with `shadowedBy` (`"global:lumencortex"`)
and never reach a run. Across scopes the project skill overrides the global one (`overridden`),
as before. A root path that appears twice (home is the workspace) is read once. A missing root has
no skills; an unreadable foreign root hides its skills instead of failing the run.

Project discovery reads the workspace root only; it does not walk up to a repository root as OMP
does. The Engine workspace is normally the repository root, and walking would make the project
scope depend on where the Engine happens to be opened.

### D2 — Foreign skills are read in place

LumenCortex never copies, rewrites or deletes a skill outside its own roots. A skill directory
that is a symbolic link is followed one level and reported (`linked`, `realPath`); a link inside
LumenCortex's own root is also treated as someone else's files. Consequences:

- `Save` writes the scope's LumenCortex root and refuses to write through a link. Saving an id that
  exists in a foreign root creates a LumenCortex copy that shadows it — an explicit override.
- `Delete` removes only a LumenCortex-owned skill; a foreign or linked one is refused with the
  root it lives in.
- Enablement works for every skill: it is stored in the scope's `skills-state.json` beside the
  LumenCortex root, keyed by id, and a project setting still overrides the global one.
- A broken link is listed with an error (`broken link: <target>`) and is never offered.

### D3 — Trust is unchanged

ADR-0013 applies to every root by scope: the user's home roots are the user's own configuration
and are always read; the workspace's roots (`.lumencortex`, `.agents`, `.claude`) are repository
content and are read only in a trusted workspace, the withheld ids reported as before.

### D4 — The prompt carries a catalog; `read_skill` loads a skill

The skills layer is a catalog: a fixed preamble and one line per enabled skill
(`` - `id` (Name): description ``), the description whitespace-collapsed and at most 1 KiB, the
whole catalog at most 32 KiB (skills beyond it are counted, not listed). The body is not in the
prompt.

A run whose catalog lists at least one skill is offered `read_skill(id, path?)` (permission
`read`, so every policy and every child has it). Without `path` it returns SKILL.md; with a
relative path it returns that file (text only, at most 128 KiB, cut on a rune boundary and marked
`truncated`) or a directory listing (at most 200 entries, hidden entries skipped). The path must
resolve, through links, inside the directory the skill resolves to; `..`, absolute paths and
links that leave the skill are refused. `read_skill` reads exactly the skills the run's catalog
lists: the reader is built from the same `PromptResult.Included` as the prompt, so a disabled,
shadowed or withheld skill is not readable. The file is read at call time.

`Engine.EffectivePrompt` builds its toolset from the same catalog, so its report lists
`read_skill` exactly when a run would have it.

## Invariants

- No LumenCortex operation writes, renames or deletes a file outside `<GlobalDir>/skills`,
  `<workspace>/.lumencortex/skills` and their `skills-state.json` files.
- A run's catalog and its `read_skill` agree on the set of skills.
- `read_skill` never returns bytes from outside the resolved skill directory.
- Project roots of any convention are read only in a trusted workspace.

## Alternatives considered

### Copy or import foreign skills into LumenCortex's root

One directory to read. Not selected: copies go stale when the installer updates the original,
links would be duplicated, and the user would have two places to edit.

### Keep injecting full bodies, raise or lower the cap

Simple and the model never has to ask. Not selected: the cost is paid on every request for
skills most tasks do not use, and a cap silently drops whichever skills sort last.

### Let `read_file` reach skill directories

No new tool. Not selected: `read_file` is confined to the workspace on purpose; widening it to
home directories widens every run's read authority, not just skills.

## Consequences

### Positive

- Skills installed for other agents work in LumenCortex without copying.
- Prompt cost is one line per skill: the owner's eight readable skills make a catalog of about 4 KiB instead of about 110 KiB of bodies.
- Conflicts are visible (`shadowedBy`, `overridden`) instead of silent.

### Negative / trade-offs

- The model must decide to load a skill; a poor description means a skill that is never used.
  A real-model smoke (`kimi-k3`) loaded the matching skill on the first step.
- A run with a tool allowlist that omits `read_skill` sees the catalog but cannot load bodies.

## Security and trust impact

`read_skill` adds read access to the resolved directories of the catalog's skills, which the
user's configuration (global) or a trusted workspace (project) put there. It does not add write
authority, does not follow links out of a skill, and refuses binary files.

## Persistence / compatibility impact

No schema change. `Skill` gains `source`, `root`, `linked`, `realPath`, `readOnly` and
`shadowedBy` (additive JSON). `skills.NewRegistry` takes root lists (internal API). Existing
`skills-state.json` files keep their meaning.

## Validation plan

- `internal/skills`: precedence and shadowing across the three conventions, linked and broken
  links, foreign files unchanged after save/delete/disable, saving and deleting through a link
  refused, catalog contains descriptions and no body, catalog bound, untrusted project roots
  withheld, `ReadResource` confinement (`..`, absolute, escaping link, binary, missing) and
  truncation.
- Real data: `lcx-go skills list global` against the owner's home lists the nine
  `~/.agents/skills` entries with the two links and the broken one; a real run loaded
  `company-deploy` through `read_skill` and quoted its first heading.

## Documentation impact

`docs/security-and-trust.md` (project roots), `docs/architecture.md` (skills row).
