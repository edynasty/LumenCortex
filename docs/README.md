# Documentation index

LumenCortex is a Go-based cognitive coding-agent system. **Go is the sole production Runtime.** Node.js source and package metadata remain as historical reference only; they are not a second production runtime or a feature-parity target. Node.js test results are not Go production acceptance evidence.

## Start here

- [System overview (English)](system-overview.md) · [中文](system-overview.zh-CN.md)
- [Architecture (English)](architecture.md) · [中文](architecture.zh-CN.md)
- [Architecture diagrams (English)](architecture-diagrams.md) · [中文](architecture-diagrams.zh-CN.md)
- [Agent execution flow (English)](execution-flow.md) · [中文](execution-flow.zh-CN.md)
- [Agent runtime (English)](agent-runtime.md) · [中文](agent-runtime.zh-CN.md)
- [Go runtime migration (English)](go-runtime-migration.md) · [中文](go-runtime-migration.zh-CN.md)

## Agent Harness

- [Harness overview (English)](harness/README.md) · [中文](harness/README.zh-CN.md)
- [Harness capability matrix (English)](harness/capabilities.md) · [中文](harness/capabilities.zh-CN.md)
- [Harness roadmap (English)](harness/roadmap.md) · [中文](harness/roadmap.zh-CN.md)
- [Hashline and AST editing](reports/harness-evolution-editing-20261110.md)
- [DAP debugger (English)](dap-debugger.md) · [中文](dap-debugger.zh-CN.md)
- [Persistent Eval (English)](persistent-eval.md) · [中文](persistent-eval.zh-CN.md)
- [Agent Hub (English)](agent-hub.md) · [中文](agent-hub.zh-CN.md)
- [Agent debugger (English)](agent-debugger.md) · [中文](agent-debugger.zh-CN.md)
- [Workflow Contract (English)](workflow-contract.md) · [中文](workflow-contract.zh-CN.md)
- [Security and trust (English)](security-and-trust.md) · [中文](security-and-trust.zh-CN.md)
- [Host boundary (English)](host-boundary.md) · [中文](host-boundary.zh-CN.md)

## Cognitive architecture

- [Cognitive Core overview (English)](cognitive-core/README.md) · [中文](cognitive-core/README.zh-CN.md)
- [Unified cognitive architecture (English)](cognitive-core/architecture.md) · [中文](cognitive-core/architecture.zh-CN.md)
- [Cognitive capability matrix (English)](cognitive-core/capabilities.md) · [中文](cognitive-core/capabilities.zh-CN.md)
- [Memory lifecycle (English)](cognitive-core/memory-lifecycle.md) · [中文](cognitive-core/memory-lifecycle.zh-CN.md)
- [Cognitive roadmap (English)](cognitive-core/roadmap.md) · [中文](cognitive-core/roadmap.zh-CN.md)
- [Cognitive control plane (English)](cognitive-control-plane.md) · [中文](cognitive-control-plane.zh-CN.md) — historical design record; current Go capabilities are defined by Go source and tests.
- [Attention Light (English)](attention-light-algorithm.md) · [中文](attention-light-algorithm.zh-CN.md)
- [Context and Evidence (English)](context-evidence.md) · [中文](context-evidence.zh-CN.md)
- [Data model (English)](data-model.md) · [中文](data-model.zh-CN.md)
- [Architecture Decision Records](adr/README.md)

## Development and validation

- [Standalone readiness (English)](standalone-readiness.md) · [中文](standalone-readiness.zh-CN.md)
- [Validation (English)](validation.md) · [中文](validation.zh-CN.md)
- [Go provider configuration (English)](go-provider.md) · [中文](go-provider.zh-CN.md)
- [Plugin and MCP templates (English)](plugins-mcp-templates.md) · [中文](plugins-mcp-templates.zh-CN.md)
- [Runtime event contract (English)](runtime-event-contract.md) · [中文](runtime-event-contract.zh-CN.md)
- [Runtime hooks (English)](runtime-hooks.md) · [中文](runtime-hooks.zh-CN.md)
- [Documentation guide (English)](documentation-guide.md) · [中文](documentation-guide.zh-CN.md)

## Current major engineering gaps

The [Cognitive Core capability matrix](cognitive-core/capabilities.md) and [Harness capability matrix](harness/capabilities.md) distinguish implemented Go main behavior from unmerged candidates and design targets.

The authoritative implementation and evidence matrix is [Standalone readiness](standalone-readiness.md). Current limitations include:

1. **Local-session rollback** — arbitrary workspace mutations made by shell commands do not have whole-run rollback. Go session Worktrees can be isolated and safely discarded or handed off.
2. **Mandatory isolation** — Worktree execution is available as an option; it is not mandatory for every Agent run.
3. **Vision and browser automation** — not implemented as built-in runtime capabilities.
4. **TUI interaction depth** — editor-grade navigation and interaction remain UX work; the current Go CLI is a source-run preview surface.
5. **Model calibration** — live Jev/Laya validation and broad multi-model routing calibration remain open.
6. **Graph governance** — Go main exposes read-only Analyze / Plan / Validate. Model Curator is disabled before provider invocation until request-envelope admission exists; Apply and Cortex Epoch execution are not claimed in Go main.
7. **Physical storage tiers and GC** — hot/warm/cold metadata exists, but separate physical stores and destructive cognitive garbage collection are not claimed.
8. **Linux and Desktop release gates** — real Linux DAP, host/VM cgroup and UID Broker, and native interactive Wails acceptance require their own platform evidence.
9. **Expanded Cognitive Core** — Scope / Time / Evidence and Experimental Experience have been developed and reviewed in local candidates, but those candidate commits are not part of Core `main`. See the [Cognitive Core capability matrix](cognitive-core/capabilities.md) and the finalization evidence before making merge or release claims.

Historical Node.js capabilities may be described in historical design documents, but they must not be presented as current Go production capabilities.
