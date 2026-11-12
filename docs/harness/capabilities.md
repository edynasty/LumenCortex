# Agent Harness Capability Matrix

**Baseline:** Core main 883c63e (2026-11-12). Status is based on current Go source and existing project documentation, not on an unmerged worktree or competitor feature list.

| Area | Current Go Core main | Evidence / implementation surface | Boundary |
|---|---|---|---|
| Agent loop and provider adapters | Implemented | runtime/, internal/agent/, provider packages | Model quality and provider-specific behavior remain provider-dependent |
| Durable Session / resume / cancellation | Implemented | Session store, supervisor, lifecycle tests | Does not imply rollback of arbitrary external side effects |
| Work Units / Workflow Contract / delivery checks | Implemented baseline | internal/workunit/, internal/workflow/, delivery verification | Delivery verification is separate from semantic correctness |
| Tool permissions / workspace trust | Implemented | Tool policy, host boundary, security tests | Per-call approvals and mandatory worktree isolation are separate gaps |
| Shell and managed process lifecycle | Implemented | shell.StartManaged, jobs and runtime process ownership | Local-session arbitrary shell mutations are not whole-run reversible |
| MCP stdio / HTTP and Skills | Implemented | MCP registry, host templates, skills registry | Browser/computer use is provided through external integrations, not a built-in browser runtime |
| LSP navigation / diagnostics / guarded rename | Implemented baseline | internal/lsp/, toolset and editing integration | Go Code Actions and LSP resource operations are not claimed |
| DAP program debugger | Implemented in Core main | internal/debugger/, runtime/dap_debugger.go | Real adapter/platform gates remain; native Desktop UI is separate |
| Agent safe-boundary debugger | Implemented in Core main | internal/agent/debug_control.go, runtime host API | Controls Agent execution boundaries; it is distinct from debugging the target program |
| Persistent Python / JavaScript Eval | Implemented in Core main | evalapi, evalruntime, runtime/eval.go | Opt-in; enforcement differs by platform; JS forced interruption may lose state |
| Agent Hub / task DAG / leases / messages / handoff | Implemented baseline in Core main | runtime/hub.go, Hub/session APIs and Work Units | Desktop presentation and exact release integration are separate |
| Unified Editing Engine | Implemented in Core main | internal/editing/, runtime/editing.go | All writes must use guarded revision/CAS path |
| Hashline / anchored line editing | Implemented baseline | Editing toolset, revision snapshots, line anchors | Hashline is a locator/precondition mechanism, not a semantic correctness proof |
| Go AST-aware editing | Implemented, Go only | internal/editing/goedit/ | Type/scope-aware Go operations; not general TS/Python AST editing |
| Multi-language AST editing | Not implemented as a Go production capability | Historical Node/reference research only | Use LSP where supported; do not count Node implementation |
| Browser / vision / computer use | External integration direction | MCP templates / host integrations | Not a native built-in Core runtime claim |
| Retry / handoff / collaboration controls | Core Hub baseline | Hub APIs, leases, durable messages and handoff | Host/Desktop must use the authoritative Core APIs |
| Native Wails UI for every capability | Separate Desktop surface | Desktop repository and release evidence | Core API availability does not prove UI availability |
| Cognitive architecture | Separate cognitive layer | Cognitive Core docs | Expanded Cognitive Core candidate is not part of this main snapshot |

## Harness capability status versus Harness Evolution

The 2026-11-10 Harness Capability Audit compared three different surfaces: Core main, the then-current Desktop product pinned to an older Core tag, and an unmerged execution-platform branch. Its findings and prototypes are historical, branch-specific evidence. Later Go code landed in Core main, so the old matrix must not be copied as if it were a current status table.

The separate Harness Evolution implementation/review work also included candidate-only changes and Desktop integration constraints. A local candidate or a successful isolated test does not mean it has been merged, tagged, pushed, or shipped in Desktop. Check the exact commit, ancestry, release tag and Desktop go.mod dependency before making a release claim.
