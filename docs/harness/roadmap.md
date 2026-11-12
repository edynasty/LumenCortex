# Agent Harness Roadmap

This roadmap is a product direction, not a claim that every item is missing or authorized for immediate implementation.

## Preserve the existing architecture

- Keep Go as the only production runtime.
- Keep one Agent Loop and one execution authority.
- Keep editing, DAP, Eval, Hub and process lifecycle as distinct capabilities with shared ownership and permission boundaries.
- Use MCP/Plugins for external systems such as browser, computer-use and provider-specific integrations where appropriate.
- Do not copy OMP or Claude Code features without proving they support LumenCortex's own cognitive coding-agent goals.

## Current strengths

Core main already has durable Agent execution, Workflow Contracts, Work Units, permission checks, MCP/LSP, managed Shell, DAP, persistent Eval, Hub task/control primitives, and a shared revision-aware Editing Engine. These capabilities are documented in the capability matrix.

## Remaining directions and gates

| Direction | Why it matters | Current boundary / next decision |
|---|---|---|
| Exact Core release and Desktop integration | Core main capabilities must reach the actual Desktop product | Upgrade Desktop to an exact published Core SemVer only after Core release gates and native Wails acceptance |
| Linux DAP and execution isolation | Debugger and worker security depends on real caller isolation | Verify real adapters, cgroup/UID broker and bwrap paths on supported Linux hosts |
| Native Desktop Hub / Debugger / Eval UX | Host APIs alone do not provide usable interactive controls | Validate actual Wails interactions against a released Core version |
| Multi-language structural editing | AST semantics differ by language | Continue LSP-backed operations first; do not claim general AST support from Go AST |
| Shell rollback / workspace recovery | Arbitrary shell side effects exceed file-edit transactions | Treat as a separate recovery design; do not promise whole-run rollback without side-effect coverage |
| Observability and cost accounting | Long runs need explainable resource and token attribution | Extend per-run observability only with clear source and accounting semantics |
| Browser / vision / computer use | These are external environment capabilities | Prefer MCP/Plugins and host-controlled lifecycle rather than embedding a new browser stack |
| Resource/process unification | DAP, Eval, Jobs and Hooks share process concerns | Reuse managed process primitives; do not collapse the modules into one feature |
| Editing safety | Concurrent users/Agents must not overwrite newer content | Keep revision, lease, CAS, validation and post-write checks as mandatory invariants |

## Non-goals unless separately authorized

- A public plugin marketplace.
- A second production runtime in Node.js.
- A second Agent Loop or background cognitive coordinator.
- General-purpose AST editing for every language without a language-specific semantic implementation.
- Automatic merging of worker worktrees into main.
- Treating competitor parity as the product roadmap.
