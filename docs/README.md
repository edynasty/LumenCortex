# Documentation index

LumenCortex is a Go-based cognitive coding-agent system. **Go is the sole production Runtime.** Node.js source and package metadata remain as historical reference only; they are not a second production runtime or a feature-parity target. Node.js test results are not Go production acceptance evidence.

## Start here

- [System overview](system-overview.md)
- [Architecture](architecture.md)
- [Architecture diagrams](architecture-diagrams.md)
- [Agent execution flow](execution-flow.md)
- [Agent runtime](agent-runtime.md)
- [Go production runtime and migration history](go-runtime-migration.md)

## Agent Harness

- [Harness overview (English)](harness/README.md) · [中文](harness/README.zh-CN.md)
- [Harness capability matrix (English)](harness/capabilities.md) · [中文](harness/capabilities.zh-CN.md)
- [Harness roadmap (English)](harness/roadmap.md) · [中文](harness/roadmap.zh-CN.md)
- [Hashline and AST editing](reports/harness-evolution-editing-20261110.md)
- [DAP debugger](dap-debugger.md)
- [Persistent Eval](persistent-eval.md)
- [Agent Hub](agent-hub.md)
- [Workflow Contract](workflow-contract.md)
- [Security and trust](security-and-trust.md)

## Cognitive architecture

- [Cognitive Core overview (English)](cognitive-core/README.md) · [中文](cognitive-core/README.zh-CN.md)
- [Unified cognitive architecture (English)](cognitive-core/architecture.md) · [中文](cognitive-core/architecture.zh-CN.md)
- [Cognitive capability matrix (English)](cognitive-core/capabilities.md) · [中文](cognitive-core/capabilities.zh-CN.md)
- [Memory lifecycle (English)](cognitive-core/memory-lifecycle.md) · [中文](cognitive-core/memory-lifecycle.zh-CN.md)
- [Cognitive roadmap (English)](cognitive-core/roadmap.md) · [中文](cognitive-core/roadmap.zh-CN.md)
- [Cognitive control plane](cognitive-control-plane.md) — historical design record; current Go capabilities are defined by Go source and tests.
- [Attention Light algorithm](attention-light-algorithm.md)
- [Data model](data-model.md)
- [Architecture Decision Records](adr/README.md)

## Development and validation

- [Standalone readiness and known gaps](standalone-readiness.md)
- [Validation evidence](validation.md)
- [Go provider configuration](go-provider.md)
- [Plugin and MCP templates](plugins-mcp-templates.md)

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
