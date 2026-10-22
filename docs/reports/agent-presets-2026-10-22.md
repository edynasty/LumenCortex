# Agent presets and built-in child prompts (ADR-0009 A4)

- **Date:** 2026-10-22
- **Status:** implemented and validated in Core and Desktop (uncommitted; the coordinator commits)
- **Baseline:** Core `7033dea`; Desktop `afd0c51`
- **Scope:** Core built-in explore/worker prompts, compiled-in specialist presets, one-time
  oh-my-pi `modelRoles` conversion, licensing notice, Desktop preset entry point.
- **Non-goals:** no new built-in agent ids, no `lumencortex.json` schema change, no runtime
  "model roles" system, no per-agent tool allowlists, no session-import work.

> Workspace note: the Core worktree also contains another agent's in-progress session-importer
> changes (`internal/session/metadata.go`, `protocol/agent.go`). They are not part of this change
> and were not touched; the file list below is the preset change only.

## Decisions

### Built-in prompts vs. editable presets

Two child roles are harness behavior, not configuration:

- **Explore** (`spawn_subagent`, read-only) and **Worker** (`start_task`, workspace) run with
  authority fixed by the launch kind. Their prompts are compiled in (`runtime/subagents.go`
  `explorePrompt`, `runtime/tasks.go` `workerPrompt`) and cannot be edited, so a workspace cannot
  widen what a child may do and the handoff format the parent relies on cannot drift per project.
- **Specialists** (reviewer, security-reviewer, sonic, thread-historian) are ordinary catalog
  agents. `config.AgentPresets()` returns compiled-in definitions; saving one writes an ordinary
  `agents` entry the user owns and may edit, disable or delete. Nothing is created at run time and
  the catalog stays the single source of truth.

The worker prompt keeps the duties of the former default `taskPrompt` (inspect before editing,
verify with evidence, keep changes scoped, `note_write` durable findings, obey the Workflow
Contract) because setting a child `SystemPrompt` replaces the default prompt that
`runtime/agent.go` would otherwise build. The workspace authority sentence
(`policyAuthority(toolset.PolicyWorkspace)`) is therefore appended to `workerPrompt` in
`executionOptions`. The explore prompt states the read-only invariant itself.

### OMP → LC mapping

| oh-my-pi agent | LC form | use | policy | effort | OMP role |
|---|---|---|---|---|---|
| `scout` | built-in explore prompt | delegation (launch-fixed) | read-only | medium (inferred, default medium) | — |
| `task` | built-in worker prompt | delegation (launch-fixed) | workspace | parent's | — |
| `reviewer` | preset `reviewer` | both | read-only | high | `slow` |
| `security-reviewer` | preset `security-reviewer` | both | read-only | high | — |
| `sonic` | preset `sonic` | delegation | workspace | low | `smol` |
| `seance` | preset `thread-historian` | delegation | read-only | medium | — |

Presets: suggested ids `reviewer`, `security-reviewer`, `sonic`, `thread-historian`; names
Reviewer, Security Reviewer, Sonic, Thread Historian. All descriptions are under 280 bytes and all
instruction bodies under 16 KiB, so each preset is valid when written to a `lumencortex.json`
(covered by a test that puts all four in one file and runs `config.Validate` and `config.Parse`).

### Structured output adaptation

OMP agents use structured `yield` schemas, which LC does not have. Each preset asks for the same
information as markdown:

| OMP | LC markdown |
|---|---|
| `reviewer` yield: `overall_correctness`, `explanation`, `confidence`, `findings[]` | `Verdict: correct/incorrect`; 1–3 sentence explanation; confidence 0.0–1.0; findings each with an imperative heading, `Priority: P0–P3 · Confidence · Location: path:start-end`, one paragraph |
| `security-reviewer` yield: `findings[]`, `reviewed_paths`, `deferred`, `coverage_summary` | findings with rule id, severity, confidence, CWE, locations, evidence, remediation; reviewed paths; deferred with reasons; coverage summary |
| `scout` output: `summary`, `files[]`, `architecture`, optional `report` | Summary; Files as `path:start-end` + contents; Architecture; Report only when the goal asks for one |
| `seance` prose answer over one session | answer citing thread id and message context; gaps stated explicitly |

OMP-specific mechanics were replaced with LC mechanics: `find`/`grep`/`glob`/`ast_grep`/`bash`
become `search_text`/`find_files`/`read_file`/`list_dir`/`git_*`/`lsp_*`; reviewer/security-reviewer
bash is read-only Git inspection only; seance's file tools become `recall` + `read_thread`.

### OMP model-role conversion (one-time, no runtime dependency)

- `config.OMPAgentDir()` returns `PI_CODING_AGENT_DIR` when it is set **and absolute**, else
  `~/.omp/agent`; a relative override is ignored because it would resolve differently per working
  directory. `config.OMPConfigPath()` is `<dir>/config.yml`.
- `config.OMPRoleModels(path, eff)` parses `modelRoles` (string or list of strings), each
  `provider/model[:effort]`, and returns `map[string][]AgentModel` plus warnings.
- Effort map: `auto→auto`, `off|none→none`, `minimal|low→low`, `medium→medium`, `high→high`,
  `xhigh|max→max`; an unknown suffix keeps the model with no effort and warns.
- A trailing `:` only counts as an effort when the part before it is a `provider/model` ref, so a
  model key containing `:` survives.
- Candidates not in `Effective.Catalog` are dropped with one warning each; duplicates are removed;
  the list is capped at 8 (warning); leading candidates that cannot call tools are dropped
  (warning), because an agent's first model must call tools. A role with nothing usable is absent
  from the map.
- A missing file is an empty map and no error; a malformed YAML file is an error. The path is read
  only when the user opens the preset panel, never during a run.
- `config.ResolveAgentPresets(eff, path)` fills `Agent.Models` from the preset's `OMPRole` when the
  conversion is non-empty and leaves it nil otherwise (the saved agent then inherits the default
  list). OMP remains a one-time import, not a second routing system.

`Effective` gained a `Catalog []ModelInfo` field (populated in `merge`) so conversion can check
existence and tool calling against the same merged catalog agents use. This is not a file-schema
change.

### Limits

- 4 presets; no preset duplicates a built-in id or the explore/worker roles.
- Preset descriptions ≤ 280 bytes, instructions ≤ 16 KiB, models ≤ 8 (all Core catalog bounds).
- OMP conversion warnings are non-fatal; a preset without a usable role still works (inherits).
- Desktop respects `MaxCustomAgents` for the preset entry point and dedupes the draft name
  (`Reviewer 2`); the id is derived by Core on Save.

## Changes and affected files

Core (`/Users/tangxingpeng/IdeaProjects/me/LumenCortex/LumenCortex-Core`):

- `config/presets.go` (new) — `AgentPreset`, `AgentPresets()`.
- `config/presets/*.md` (new) — `reviewer.md`, `security-reviewer.md`, `sonic.md`,
  `thread-historian.md`.
- `config/omproles.go` (new) — `OMPAgentDir`, `OMPConfigPath`, `OMPRoleModels`,
  `ResolveAgentPresets`.
- `config/presets_test.go` (new) — catalog validity, fresh copies, role conversion (string/list,
  effort map, unknown model, non-tool first, cap, missing file), resolution, agent dir.
- `config/load.go`, `config/merge.go`, `config/resolve.go` — `Effective.Catalog` and its clone.
- `runtime/subagents.go` — `explorePrompt`; `runtime/tasks.go` — `workerPrompt` + authority.
- `go.mod`, `go.sum` — `gopkg.in/yaml.v3 v3.0.1` only.
- `THIRD_PARTY_NOTICES.md` (new) — oh-my-pi MIT license, copyright lines, derived files.
- `docs/adr/0009-agent-catalog-and-model-lists.md` — A4 phase.
- `docs/agent-runtime.md` — built-in child prompts and presets.

Desktop (`/Users/tangxingpeng/IdeaProjects/me/LumenCortex/LumenCortex-Desktop`):

- `internal/backend/agents.go` — `AgentEntry.Origin`/`OMPRole`, `Runtime.AgentPresets`.
- `internal/backend/agents_test.go` — preset definitions + OMP roles, and the no-OMP case.
- `app.go` — `GetAgentPresets` binding.
- `frontend/src/lib/bridge.ts`, `frontend/src/types.ts` — `GetAgentPresets`, `agentPresets`,
  `source: "preset"`, `origin`/`ompRole`.
- `frontend/src/app/agent-catalog.ts` — `dedupeAgentName`, `presetDraft`.
- `frontend/src/app/hooks/useAgentCatalog.ts` — `presets()` loader.
- `frontend/src/app/hooks/useAgentPresets.ts` (new) — preset panel state, lazy load, dirty confirm.
- `frontend/src/components/agents/AgentPresetPicker.tsx` (new) — preset cards and the OMP-role note.
- `frontend/src/components/agents/AgentsPage.tsx`, `AgentEditor.tsx`, `agents.css` — entry point,
  panel, save of a fresh draft.
- `frontend/src/components/primitives/icons.ts` — `preset` concept (Lucide `LayoutTemplate`).
- `frontend/src/lib/i18n/app-copy.ts` — zh-CN (default) and en copy.
- `frontend/src/visual/fixture-data.ts` — `GetAgentPresets` stub and two presets.
- `frontend/src/components/agents/AgentsPage.test.tsx` — preset draft, dirty confirm, controller
  stub.

Desktop `go.mod`/`go.sum` were **not** changed: Core is still pinned by the existing
pseudo-version. Tests ran against the working tree with a copied modfile
(`/tmp/lcd/go.mod` + `-replace`), never the real one.

## Tests actually run

| Command | Result |
|---|---|
| Core `go test -count=1 ./config/` | pass |
| Core `go test -count=1 ./...` | pass (all packages) |
| Core `go build ./...` | pass |
| Core `go vet ./...` | pass |
| Desktop `go test -modfile=/tmp/lcd/go.mod -count=1 ./internal/backend` | pass |
| Desktop `go vet -modfile=/tmp/lcd/go.mod ./internal/backend` | pass |
| Desktop `cd frontend && npm test` | 65 files, 317 tests pass |
| Desktop `cd frontend && npm run build` | pass |

`go mod tidy` could not run offline (it wants `gopkg.in/check.v1` for yaml's own tests); the
requirement and sums were added with `go get` from the module cache and `go build`/`go test`
resolve cleanly. CI with network access may normalize `go.sum` further.

## Open risks

- The oh-my-pi prompts are adapted prose, not verbatim copies; if upstream changes materially, the
  presets and the two built-in prompts should be reviewed again (recorded in
  `THIRD_PARTY_NOTICES.md` with the source version 18.12.6).
- `OMPRoleModels` treats a trailing `:suffix` as an effort whenever the prefix is a valid
  `provider/model`. A model key that itself ends in a known effort word (`...:high`) would be read
  as an effort; OMP's own syntax has the same ambiguity.
- Preset model lists are a snapshot: editing `~/.omp/agent/config.yml` later changes only the next
  draft, never saved agents.
- The Desktop preset panel reads Core on first open; a Core load failure (invalid
  `lumencortex.json`) is shown inside the panel, not as a page-level warning.
- The Agents page save button is now enabled for a fresh draft (needed so a preset can be saved
  unchanged); pressing it with an empty name shows the existing validation error.
