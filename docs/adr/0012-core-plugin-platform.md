# ADR-0012: Core plugin platform and session import sources

- **Status:** Accepted (in-process plugin platform; first capability: session import)
- **Date:** 2026-10-22
- **Owners:** project owner
- **Supersedes:** ADR-0011 D12 (the OMP-specific Engine and CLI surfaces)
- **Superseded by:** none

## Context

LumenCortex needs to extend Core without adding a branch to the Core main flow for each
integration. Session import is the concrete driver: OMP import exists (ADR-0011), and Codex and Pi
sessions are the next sources. Each source has its own file format; the rules that keep imported
history safe are the same for all of them: never write the source, never execute a historical
tool call, never import hidden reasoning, derive stable ids, refuse duplicates, write the tree
atomically, mark the result as imported history.

Core also needs explicit observation (listeners on Core events) and controlled intervention
(hooks at a defined boundary). The existing `Engine.Events` bus is bounded and non-blocking: it is
suitable for observation, not for durable work.

## Decisions

### D1 — A public contract package, a Core-owned registry

`pluginapi` (module path `github.com/edynasty/LumenCortex/pluginapi`) is the contract: `Manifest`,
`Plugin` (`Manifest`, `Start(ctx, Host)`, `Stop(ctx)`), `Host`, `Event`, hook types and the
capability interfaces. It imports only `protocol`. Built-in plugins live beside the code they wrap
(the OMP source is `internal/ompsession.Plugin`) and hosts supply their own through
`runtime.Options.Plugins`. `runtime.PluginRegistry` owns registration, lifecycle, enablement,
capability lookup and diagnostics.

### D2 — Versioning and capability negotiation

`pluginapi.APIVersion` is `1`. A manifest must name the exact version; a contract change that
breaks a plugin increments it. A manifest declares capabilities from the set this Core knows
(`session-import`, `events.subscribe`, `hooks.before-session-import`); an unknown capability is
refused at registration (`ErrPluginCapability`), so a plugin built for a newer contract fails
loudly instead of half-working. A declared capability must be implemented: a manifest declaring
`session-import` whose value does not implement `pluginapi.SessionImportSource` is refused. Host
methods check the capability they need at call time.

### D3 — Lifecycle, enablement and isolation

Plugins register before the registry starts (built-ins first), start in id order and stop in
reverse order on `Engine.Close`. Enablement is a global preference stored in
`<GlobalDir>/plugins.json` (`{"version":1,"disabled":[ids]}`); everything else about a plugin is
code. `Engine.SetPluginEnabled` writes the file atomically and applies the change to the calling
Engine at once (stop → `disabled`, start → `running` or `failed`); other open Engines apply it on
their next Open. An unreadable file does not block Open: every plugin counts as enabled, the
catalog reports `stateError`, and changing enablement is refused until the file is fixed, so the
file is never silently overwritten.

One broken extension never stops Core: a `Start` error or panic marks that plugin `failed` and the
others still start; `Open` succeeds. Core recovers a panic in every plugin call (manifest, start,
stop, hook, capability method), marks the plugin `failed`, closes its event subscriptions, removes
its hooks and publishes `plugin.failed`. A failed plugin's capabilities are no longer offered.
`Engine.Plugins()` returns the catalog: manifest, built-in flag, enablement, state, last error,
failure count and time, and the import source a plugin provides.

### D4 — Plugins reach Core only through `Host`; Core keeps data and transactions

`Host` offers exactly `SubscribeEvents` and `RegisterBeforeSessionImportHook`. A plugin never
receives the Engine, the session store, a transaction, a credential or a tool runner. Capabilities
return data; Core decides what to do with it. New host methods require a concrete consumer and
are added with the capability that gates them.

### D5 — Session import source capability

`pluginapi.SessionImportSource` has `ImportSource()` (stable source id, name, converter version,
default root), `ListSessions` (top-level sessions, header data only) and `LoadSession` (one
session tree as an `ImportBundle` of `ImportTranscript`s: visible rows, subagent tree by key,
source hashes and sizes, conversion counts, source-specific warnings). The source confines paths
to its root and reads only.

Core owns the rest, for every source (`runtime/session_import.go`):

- **Validation.** Roles `user`/`assistant`/`tool` only; origins limited to `imported_notice`,
  `imported_marker` (user rows) and `tool_interrupted` (tool rows), so no row can pose as one the
  runtime wrote; every call answered exactly once (`window.Groups`); valid tool-call arguments;
  rows at most 1 MiB; at most 100 children, depth at most 4, parents before children, unique keys
  and source ids; at most 32 warnings with stable codes. An invalid bundle is
  `ErrInvalidImportBundle` and writes nothing.
- **Identity and idempotency.** Native id = `session_` + first 12 bytes of
  SHA-256(`source:sourceSessionID`) — for OMP exactly the ids ADR-0011 imports already have. An
  existing id is `alreadyImported` (with `sourceChanged` from size/hash); nothing is replaced.
- **Persistence.** Core appends the `imported` row, writes `metadata.import` provenance (source,
  plugin id, converter version, source ids, path, hash, size, counts) and the `subagent` relations
  (mode `imported`) in one `ImportSessions` transaction, then publishes and journals
  `session.imported`.
- **Warnings.** Core computes `workspace_mismatch`, `maybe_running`, `open_tool_calls`,
  `children_capped`, `malformed_records` and `source_changed` from the bundle, then appends the
  source's own.

Surfaces: `Engine.ImportSources`, `ListImportSessions(source, …)`, `PreviewImport(source, root,
path)`, `ImportSession(source, root, path)`; CLI `lcx-go import sources|list|preview|run` and
`lcx-go plugins [list|enable|disable]`. They replace the OMP-specific `Engine.OMPSessions`,
`PreviewOMPSession`, `ImportOMPSession` and `lcx-go omp …` (clean cutover; Desktop moved with it).
Adding a Codex or Pi source is a new `SessionImportSource`; no Core flow, Desktop binding or CLI
command changes.

### D6 — Listeners observe, hooks intervene

Listeners (`events.subscribe`) receive Core's bounded, best-effort event stream; it closes when
the plugin stops. Events are notifications, not authoritative state; event loss is visible in the
bus counters. Work that must not be lost needs a durable outbox, which is out of scope.

`BeforeSessionImport` is the one hook point. It runs before the source is read on an import (not
on preview), receives an immutable request (source, workspace, root, path) and may allow (nil) or
refuse (error). Order: ascending priority, plugin id, registration order. Timeout: default 2 s,
at most 30 s. Policy: `fail-closed` turns an error into a refusal; an error returned in time is
the hook's decision, not a plugin failure. A timeout or panic is a failure: counted, published as
`plugin.hook.failed`, and under `fail-open` the import continues. A panic also fails the plugin.
Caller cancellation always aborts. A handler that ignores its context keeps running in its
goroutine: in-process code cannot be killed.

New hook points need a stable lifecycle boundary, typed input, defined mutability, ordering,
timeout and failure semantics, and a consumer. Hooks never wrap every method and never receive
mutable internal objects.

### D7 — Not a sandbox, not a marketplace

In-process plugins run with Core's authority and are for trusted built-in and host-integrated
code. Core does not scan plugin directories, download or install binaries, or claim isolation.
Third-party distribution needs a process protocol with an explicit authority boundary
(TODO PLUGIN-002). Go's `plugin` package is not used.

### D8 — Desktop

Desktop shows the catalog (Settings → Plugins): name, id, version, API version, capabilities,
built-in flag, state, last error and failure count, and an enable switch that calls
`SetPluginEnabled`. The import sheet lists the running import sources and works for any of them.

## Invariants

- Core opens even when every non-essential plugin fails.
- A failed or disabled plugin offers no capability, hook or subscription.
- Every imported session, whatever its source, satisfies `window.Groups`, ends with Core's
  `imported` row, carries Core-written provenance, and was written in one transaction.
- A plugin never holds a store handle or writes Core data directly.

## Alternatives considered

### Keep OMP-specific Engine methods and wrap them as a plugin

The earlier draft registered a plugin whose methods called back into OMP-specific Engine methods.
Not selected: a second source would have needed new Engine methods, Desktop bindings and CLI
commands, which is the coupling this decision removes.

### Let sources write sessions themselves

Simpler for a source. Not selected: id derivation, deduplication, transactions and the
history-only guarantees would be re-implemented, and possibly weakened, per source.

### External process plugins now

Real isolation. Not selected yet: no third-party plugin exists, and the authority, packaging,
update and signing questions deserve their own decision (PLUGIN-002).

## Consequences

- Positive: new import sources are additive; plugin failures are contained and visible; listener
  and hook semantics are explicit.
- Negative: in-process plugins are trusted code; event delivery is best-effort; enablement is
  per-user, not per-workspace.

## Validation

`runtime/plugins_test.go`: manifest validation (API version, unknown/duplicate capability,
undeclared implementation, duplicate source id), start order and reverse stop, start failure and
panic isolation with `plugin.failed` events, persisted enablement applied live and across opens,
invalid state file reported and never overwritten, hook order, refusal vs timeout vs panic
semantics, capability gating of host methods, listener stream closing on disable.
`runtime/session_import_test.go`: the OMP source end to end (idempotency, source untouched, ids
unchanged from ADR-0011, hook refusal writes nothing, continuation), a second fake source through
the same pipeline with source-scoped ids, ten invalid-bundle cases that write nothing, adapter
panic isolation, and disabling/re-enabling the OMP plugin.
