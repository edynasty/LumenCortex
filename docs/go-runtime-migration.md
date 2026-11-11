# Go production runtime and migration history

**Current Owner decision (2026-11-11): Go is the sole production Runtime.**

This document records the historical Node-to-Go transition and the current production boundary. The earlier incremental Node/Go parity plan is superseded. Node.js source, package metadata, and tests remain in the repository as historical reference only; they are not a second production implementation, a compatibility target, or a reason to continue maintaining Node features.

## Current production authority

- The Go `runtime.Engine` is the sole production runtime implementation.
- New runtime behavior, production integrations, and acceptance tests must target Go.
- Desktop and other hosts consume the Go Core contract. Desktop `main` currently pins Core `v0.1.4`; it does not automatically consume the newer Go runtime code baseline at `208ad6b`.
- The Go runtime owns current Session, Workflow, tool execution, permission, persistence, and managed process semantics.
- Go uses its own SQLite schema and migration runner. It does not share the legacy Node database format.
- Legacy Node databases are refused by Go with `ErrLegacyNodeDatabase`; they are not silently converted or partially opened.
- A retained legacy Node reader refuses Go-migrated databases before writing. This is a historical database-safety boundary, not a supported dual-runtime deployment.

## Current Core main baseline

- Go runtime code baseline in `main`: `208ad6b` (2026-11-11).
- Latest Core tag in this checkout: `v0.1.4` at `42ee234`, older than `main`.
- Go Engine identity: `0.1.4-preview`.
- Desktop `main`: `4a6c2f3`, pinned to Core `v0.1.4`.

The Core mainline includes the Go Agent Harness foundations: persistent Sessions, Agent execution, Workflow integration, workspace tools, revision-aware Editing Engine, Hashline operations, Go AST editing, DAP management, persistent Python/JavaScript Eval, Agent Hub, MCP/LSP, Skills, Subagents, and parallel Sessions. Platform acceptance remains bounded by the evidence in the relevant reports: Linux real DAP and host/VM cgroup/UID Broker, plus native interactive Wails, are not inferred from container or mock tests.

## Cognitive Core boundary

The expanded Cognitive Core P0 work (Go Scope / Time / Evidence, read-only Graph Governor validation, and Experimental Experience Learning) remains a separate unmerged candidate as of Core `208ad6b`. It must not be represented as part of current `main` until its inherited baseline, migrations, and final independent review are accepted.

The Go Graph Governor currently claims read-only Analyze / Plan / Validate. The model Curator fails closed before sending a provider request while request-envelope admission is unavailable. Go main does not claim Graph Apply, autonomous graph mutation, or Cortex Epoch execution.

## Go CLI entrypoint

`cmd/lcx-go` is the current source-run CLI/host entrypoint. The program describes its command surface as a preview CLI; that does not change the Go Engine's role as the production runtime implementation, and it does not mean a separately packaged CLI release has been published.

From the Core repository:

```bash
go run ./cmd/lcx-go health
go run ./cmd/lcx-go sessions
go run ./cmd/lcx-go agent "inspect this repository and fix the failing tests"
```

The Agent command requires a configured Go provider/model. See [Go provider configuration](go-provider.md).

## Historical migration notes

The initial Go work began as a bounded-memory vertical slice with a public `runtime.Engine`, SQLite-backed Sessions, paged message access, bounded event subscriptions, bounded shell output, and explicit memory accounting. At that time, Node behavior was used as a migration comparison source.

That comparison phase is historical. The current policy is not to close feature-by-feature parity with Node. Instead:

1. implement production behavior in Go;
2. define the Go contract and ownership explicitly;
3. validate the Go execution path with Go tests and real Engine fixtures;
4. preserve useful Node-era design history without treating Node tests as Go acceptance evidence.

Historical commands and implementation notes may remain in old reports. They do not establish current production behavior.

## Build and verification

The Go toolchain version is declared in `go.mod`. SQLite builds that use `github.com/mattn/go-sqlite3` require CGO and a working C toolchain.

Use Go commands for production validation:

```bash
go test ./...
go vet ./...
```

Node.js 22.13+ is needed only for explicitly running retained historical Node.js reference tests/tools or regenerating Mermaid documentation diagrams. Neither use makes Node.js a production runtime.

## Non-goals

- No new Node.js production implementation.
- No Node/Go feature-parity backlog.
- No shared Node/Go SQLite database format.
- No migration of legacy Node databases by reinterpretation.
- No use of Node test results as evidence that Go behavior is correct.
- No promotion of the experimental Cognitive Core P0 candidate before whole-candidate independent review.
