# Agent Harness

**Status snapshot: Core main 883c63e, 2026-11-12.** This guide distinguishes the current Go Core main from unmerged Harness Evolution candidates and Desktop release status.

LumenCortex is a cognitive coding-agent system with its own harness. The harness owns execution, tools, editing, debugging, process lifecycle, collaboration, permissions, persistence and delivery boundaries. Cognitive architecture supplies grounded context and knowledge; it does not replace the Agent runtime.

## Start here

- [Harness capabilities](capabilities.md) — current main capabilities and limits.
- [Harness roadmap](roadmap.md) — capability direction and dependency order.
- [Agent runtime](../agent-runtime.md)
- [Agent Hub](../agent-hub.md)
- [Agent debugger](../agent-debugger.md)
- [DAP program debugger](../dap-debugger.md)
- [Persistent Eval](../persistent-eval.md)
- [Editing evolution](../reports/harness-evolution-editing-20261110.md)
- [Security and trust](../security-and-trust.md)

## Current Core main capabilities

Core main contains Go implementations for multi-turn Agent execution, durable Sessions, tools and permissions, Work Units and Workflow Contracts, MCP/LSP integrations, managed Shell, the Agent debugger, DAP program debugging, persistent Python/JavaScript Eval, Hub task/control primitives, and the revision-aware Editing Engine with Go AST editing.

The presence of a Core API does not prove that every Desktop panel or release package exposes it. Desktop remains a separate versioned product and must be checked against its exact Core dependency and native Wails acceptance.

## Explicit limitations

- Linux real DAP adapters and host/VM cgroup/UID Broker acceptance are platform-specific gates; container or macOS evidence is not a substitute.
- Native interactive Wails UI acceptance is a Desktop gate.
- macOS Eval memory enforcement is supervised, not a hard kernel memory ceiling.
- JavaScript forced interruption can discard the worker and in-memory state.
- Go AST-aware editing is Go-specific. Do not claim general multi-language AST editing.
- LSP navigation and guarded rename do not imply Go LSP Code Actions or resource-operation support.
- Whole-run rollback of arbitrary shell side effects in a local session is not provided.
- Browser/vision/computer-use are extension/MCP integration directions, not built-in Core runtime claims.
- The Harness Evolution candidate and its full independent review are separate historical work; do not claim every proposed follow-up is merged or released.

## Versioning rule

Always state the Core commit/tag and Desktop Core dependency separately. A feature in Core main may not exist in an older published Core tag or the Desktop version currently installed by a user.
