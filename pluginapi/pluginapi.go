// Package pluginapi is the contract between LumenCortex Core and its in-process extensions
// (ADR-0012). A plugin declares a versioned Manifest, is started and stopped by Core's registry,
// and reaches Core only through the Host it is given. Capabilities are typed Go interfaces in this
// package; a manifest may declare only capabilities its value implements.
//
// Core keeps ownership of its data: an extension never receives the session store, a transaction
// or the Engine. A session import source, for example, returns an ImportBundle and Core validates,
// derives ids, checks idempotency and writes it.
//
// In-process plugins run with the authority of the Core process. This package is not a sandbox;
// it is for built-in and host-integrated code the host already trusts. In particular:
//
//   - Core recovers a panic in a call it makes into the plugin (Manifest, Start, Stop, a hook, a
//     capability method) and marks the plugin failed. A panic in a goroutine the plugin starts
//     itself is not recovered and ends the process.
//   - Start, Stop and capability methods have no Core-imposed deadline: they get the caller's
//     context and must return promptly. A Start that blocks delays Engine open.
//   - A hook timeout ends Core's wait, not the handler: a handler that ignores its context keeps
//     running in its goroutine, also after its plugin is stopped or disabled. Handlers must honour
//     ctx.Done().
//   - Stopping or disabling a plugin closes its event streams and removes its hooks; a call
//     already in flight finishes. Stop must release anything the plugin started.
package pluginapi

import (
	"context"
	"encoding/json"
	"time"
)

// APIVersion is the contract version this Core implements. A manifest must name it exactly; a
// change that breaks an existing plugin increments it.
const APIVersion = 1

// Capabilities a manifest may declare.
const (
	// CapabilitySessionImport: the plugin implements SessionImportSource.
	CapabilitySessionImport = "session-import"
	// CapabilityEvents: the plugin may call Host.SubscribeEvents.
	CapabilityEvents = "events.subscribe"
	// CapabilityBeforeSessionImport: the plugin may call Host.RegisterBeforeSessionImportHook.
	CapabilityBeforeSessionImport = "hooks.before-session-import"
)

// Manifest is a plugin's declaration. ID is stable across versions ([a-z][a-z0-9._-], at most 64
// bytes); Version is the plugin's own version; APIVersion is the pluginapi version it was built for.
type Manifest struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Description  string   `json:"description,omitempty"`
	APIVersion   int      `json:"apiVersion"`
	Capabilities []string `json:"capabilities"`
}

// Plugin is an in-process extension. Start runs once per enable; Stop releases what Start acquired.
// Both must honor ctx. Core recovers a panic in any plugin call and marks the plugin failed.
type Plugin interface {
	Manifest() Manifest
	Start(ctx context.Context, host Host) error
	Stop(ctx context.Context) error
}

// Host is everything a plugin may ask of Core. Each method checks the capability it requires.
type Host interface {
	// SubscribeEvents streams Core events. The stream is bounded and best-effort: a slow subscriber
	// loses old events. It is closed, and cancel becomes a no-op, when the plugin stops.
	SubscribeEvents(buffer int) (events <-chan Event, cancel func(), err error)
	// RegisterBeforeSessionImportHook adds a hook that may refuse a session import. The hook is
	// removed when the plugin stops.
	RegisterBeforeSessionImportHook(hook BeforeSessionImportHook) error
}

// Event is a Core notification. It is not authoritative state: re-read Core when it matters.
type Event struct {
	Seq       uint64          `json:"seq"`
	At        time.Time       `json:"at"`
	Type      string          `json:"type"`
	SessionID string          `json:"sessionId,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// HookFailurePolicy decides what a hook error or timeout does to the operation.
type HookFailurePolicy string

const (
	// HookFailClosed aborts the operation on an error or timeout.
	HookFailClosed HookFailurePolicy = "fail-closed"
	// HookFailOpen records the failure and lets the operation continue.
	HookFailOpen HookFailurePolicy = "fail-open"
)

// Hook timeouts: the default when Timeout is zero, and the largest accepted.
const (
	DefaultHookTimeout = 2 * time.Second
	MaxHookTimeout     = 30 * time.Second
)

// BeforeSessionImportHook runs before Core reads an import source. Hooks run by ascending
// Priority, then plugin id, then registration order. Handle allows the import by returning nil and
// refuses it with an error; it cannot change the request.
type BeforeSessionImportHook struct {
	Priority int
	Timeout  time.Duration
	Policy   HookFailurePolicy
	Handle   func(context.Context, BeforeSessionImportRequest) error
}

// BeforeSessionImportRequest describes the import about to happen.
type BeforeSessionImportRequest struct {
	Source    string `json:"source"`
	Workspace string `json:"workspace"`
	Root      string `json:"root"`
	Path      string `json:"path"`
}
