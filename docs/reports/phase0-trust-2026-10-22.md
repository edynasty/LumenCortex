# Phase 0 project inputs, workspace trust and the effective prompt — 2026-10-22

- **Status:** implemented and validated locally on branch `p0/trust`; not pushed, not merged.
- **Baseline:** Core `1ac5932` (`v0.1.0`).
- **Decision:** [ADR-0013](../adr/0013-project-inputs-and-workspace-trust.md), with an amendment to
  [ADR-0009](../adr/0009-agent-catalog-and-model-lists.md).
- **Scope:** plan P0-B (skills half), P0-C (project instructions), P0-D (`EffectivePrompt`), P0-E1
  item 1 (Ask text). The MCP half of P0-B and item 2 of P0-E1 are implemented by the parallel MCP
  slice; the ADR states their rule.

## Delivered

| Capability | Where |
| --- | --- |
| Workspace confinement helper for a single known file (symlink-safe, one rule shared with note cites) | `internal/repository/path.go` (`ConfinedFile`), used by `internal/repository/blobs.go` |
| Instruction files: global `AGENTS.md` always, project `AGENTS.md` else `CLAUDE.md` only in a trusted workspace, 64 KiB cap with truncation, symlink escape refused, missing/refused reported | `internal/instructions/` |
| Skills trust gate: `Prompt(PromptOptions{IncludeProject})` returns the prompt plus the included and the withheld project skills | `internal/skills/registry.go`, `internal/skills/types.go` |
| One tool-list function for the loop and the report: `StepSpecs` (toolset, task tools, `start_task`, denylist) and `StepTools` (the same list plus every tool a step could offer, with the withholding filter) | `internal/agent/stepspecs.go`; the loop calls it in `internal/agent/loop.go` |
| Tool origin (`builtin`, `lsp`, `mcp:<serverID>`) without changing registration call sites | `internal/toolset/toolset.go` (`Set.Catalog`) |
| Layered system prefix: base prompt → global instructions → project instructions → skills → agent instructions, each labelled with its source | `internal/agent/types.go` (`SystemLayer`), `internal/agent/loop.go` (`systemMessages`), `runtime/prompt.go` (`assemblePrompt`) |
| `Engine.EffectivePrompt` with layers, tools, trust, Task State block, warnings; no side effects | `runtime/prompt.go` |
| Trust read per run from the Start's own configuration load, carried into the prompt assembly | `runtime/models.go` (`modelPlan.trusted/globalDir/agentSource`), `runtime/agent.go` (`prepareRun`) |
| `skills.loaded` records the withheld project skill ids and the reason | `runtime/agent.go` |
| Ask text: description and read-only authority say what a read-only run can do | `config/agents.go`, `runtime/agent.go` (`policyAuthority`) |
| Documentation | `docs/adr/0013-*`, `docs/adr/README.md`, `docs/adr/0009-*` (amendment), `docs/security-and-trust.md` (Workspace trust) |

## Contract for Desktop

`Engine.EffectivePrompt(ctx, EffectivePromptRequest) (EffectivePromptResult, error)`.

```go
type EffectivePromptRequest struct {
	SessionID     string   `json:"sessionId,omitempty"`     // optional: with it, the session's pin, Task State, worktree, thread tools
	Agent         string   `json:"agent,omitempty"`         // catalog agent; "" = the session's pin, else "agent"
	Policy        string   `json:"policy,omitempty"`        // host grant; the agent's ceiling narrows it
	Delegation    string   `json:"delegation,omitempty"`    // none | propose | isolated
	ToolAllowlist []string `json:"toolAllowlist,omitempty"`
	ToolDenylist  []string `json:"toolDenylist,omitempty"`
	Interactive   bool     `json:"interactive,omitempty"`   // conversation turn vs task run
	Subagents     bool     `json:"subagents,omitempty"`     // started through RunSupervisor.Start
}

type EffectivePromptResult struct {
	Workspace  string        `json:"workspace"`
	Trusted    bool          `json:"trusted"`
	Agent      string        `json:"agent"`
	Policy     string        `json:"policy"`
	Delegation string        `json:"delegation"`
	Layers     []PromptLayer `json:"layers"`
	Tools      []PromptTool  `json:"tools"`
	Warnings   []string      `json:"warnings,omitempty"`
}

type PromptLayer struct {
	Kind      string `json:"kind"`                // builtin | global-instructions | project-instructions | skills | agent-instructions | task-state
	Source    string `json:"source,omitempty"`    // file path, directory list, "agent:<id> (<source>)", "session:<id>"
	Scope     string `json:"scope,omitempty"`     // core | global | project | agent | session
	Included  bool   `json:"included"`
	Reason    string `json:"reason,omitempty"`    // missing | untrusted workspace | resolves outside the workspace | not a regular file | unreadable | no enabled skills | none
	Bytes     int    `json:"bytes"`               // len(Text)
	Text      string `json:"text,omitempty"`      // exactly what the layer contributes
	Truncated bool   `json:"truncated,omitempty"`
	Warning   string `json:"warning,omitempty"`
}

type PromptTool struct {
	Name       string `json:"name"`
	Permission string `json:"permission"` // read | write | exec | delegate
	Source     string `json:"source"`     // builtin | task | lsp | mcp:<serverID> | delegation
	Included   bool   `json:"included"`
	Reason     string `json:"reason,omitempty"` // policy | allowlist | denylist | delegation
}
```

`layers[].text` of the included layers, in order, is the run's prompt: the `builtin` layer is the
first system message, the instruction/skills layers are the system messages after it, the
`agent-instructions` layer is the last system message, and the `task-state` layer is the first user
message. The contract test asserts exactly this against a real run.

## Tests run

- `go build ./...`, `go vet ./...`, `go test -count=1 ./...` — pass.
- New coverage:
  - `internal/instructions/instructions_test.go`: trust gate, `CLAUDE.md` fallback and `AGENTS.md`
    precedence, symlink out of the workspace refused, truncation at 64 KiB, missing files, the global
    file read through a symlink.
  - `internal/skills/registry_test.go`: the untrusted prompt carries the global scope only and names
    the withheld project ids.
  - `internal/agent/stepspecs_test.go`: included entries are exactly `StepSpecs`; `policy`,
    `allowlist`, `denylist` and `delegation` reasons; `start_task` under propose and under isolated
    without a task starter.
  - `internal/agent/loop_test.go`: the system prefix order (base, global, project, skills, agent
    instructions) and that a blank layer is not sent.
  - `runtime/prompt_test.go`: trust gate at the report level and in a real run (including a child
    run), layer order and labels, the contract test against a recording provider, withheld tools,
    "writes nothing" (table row counts and an input file's mtime, with and without a session), a
    worktree session reading the Engine workspace's instruction file, child authority, child base
    prompt.
  - `runtime/interactive_test.go`: the read-only authority text now says what a read-only run can do.

## Deviations from the plan

1. **`authority` is not a separate layer.** The plan lists `authority` among the layer kinds; the run
   sends the purpose and the authority as one system message, so splitting them in the report would
   make the report disagree with the wire. The base layer carries both, and the contract test pins it.
2. **The Task State block is a layer of its own kind, not a system layer.** It is reported as
   `kind: task-state` (scope `session`) and is the first *user* message, as the loop sends it.
3. **`tools[].reason` covers `policy`, `allowlist`, `denylist` and `delegation`.** MCP tools of a
   project server blocked by trust are absent rather than listed with a reason: a stopped server's
   tool names are unknown, and the MCP listing already reports the server with `blockedByTrust`.
4. **`EffectivePromptRequest` carries `Interactive` and `Subagents`.** Both are `AgentOptions` fields
   that change the prompt and the tool list; without them the report could not match a real run. They
   are additive JSON fields, `false` for a first thread run outside a supervisor.
5. **`EffectivePrompt` describes a thread run unless the session is a child.** For a child session it
   applies the relation's authority, the child's fixed base prompt and no MCP/spawn/thread tools,
   mirroring `constrainChild`; it does not model a host-supplied `AgentOptions.SystemPrompt`.
6. **No new event for prompt warnings.** Truncation and withheld layers are visible in
   `EffectivePromptResult.Warnings` and in the layer text the model reads; a per-run event would
   duplicate instruction text into the journal, which ADR-0009's digest-only evidence rule forbids.
7. **`Engine.Skills(scope)` does not yet report `blockedByTrust`.** The plan lists it; this slice
   leaves it to the host, which can combine `EffectivePrompt().trusted` with each skill's `scope`.
   Listed below as a follow-up.

## Out of scope / follow-ups

- `Engine.Skills(scope)` and the Desktop Skills panel: expose `blockedByTrust` per skill (plan §2.3).
- Subdirectory `AGENTS.md` and `.cursorrules` (CTX-002).
- SECURITY-002: `.lumencortex/lumencortex.db` shipped by a repository is opened without a provenance
  check (P2, ADR-0013 residual risk).
- Desktop: the "effective prompt" tab (P0-D UI) and the trust display in the MCP/Skills/Settings
  panels, both against the DTO above.

## Risks

- The report and the run share `assemblePrompt` and `StepSpecs`, but the report reads configuration
  once while a run may resolve automatic effort and re-pin the session; a host that shows the report
  before a Start describes the *next* run, not the previous one.
- A workspace whose `AGENTS.md` is a symlink inside the workspace is read; one that leaves the
  workspace is refused, which a user with a dotfiles checkout in a trusted workspace may notice
  (the global file is exempt).
