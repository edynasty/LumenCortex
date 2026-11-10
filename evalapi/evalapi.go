// Package evalapi is the language-neutral, versioned execution SPI shared by the
// LumenCortex host and every execution runtime plugin.
//
// It has no dependency on the agent loop, the runtime package or the toolset: a
// provider implements the wire protocol in any language, and the host consumes
// only these types. Everything a plugin declares here is a claim the host must
// verify; nothing in a manifest grants authority. Grants, workspaces, budgets and
// tool authority are host decisions.
package evalapi

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

// SPIVersion is the version of this contract. A runtime whose manifest declares a
// different major version is rejected; the wire protocol negotiates the same value
// during initialize.
const SPIVersion = "1.0"

// ProtocolVersion is the JSON-RPC 2.0 wire protocol revision spoken over the
// worker's dedicated control pipe. It is separate from SPIVersion so the framing
// can evolve without changing these Go types.
const ProtocolVersion = "1.0"

// Worker control channel file descriptors. The protocol never shares stdout or
// stderr with user code: fd 3 is the host-to-worker pipe, fd 4 the worker-to-host
// pipe, so a cell that writes to fd 1 cannot corrupt the framing.
const (
	WorkerControlReadFD  = 3
	WorkerControlWriteFD = 4
)

// Result status values. A result is "completed" only when the runtime reported
// the cell finished and the worker survived it.
const (
	ResultStatusCompleted = "completed"
	ResultStatusFailed    = "failed"
	ResultStatusTimeout   = "timeout"
	ResultStatusCancelled = "cancelled"
	ResultStatusCrashed   = "crashed"
	ResultStatusRejected  = "rejected"
)

// StateStatus says whether the kernel is still usable after the cell. It is not a
// rollback claim: a failed cell may have changed files, variables or the network.
const (
	// StateStatusOK: the worker survived; variables from earlier cells remain.
	StateStatusOK = "ok"
	// StateStatusLost: the worker is gone (crash, timeout, cancel, OOM); the next
	// execution starts from a new generation and nothing is replayed.
	StateStatusLost = "lost"
	// StateStatusUnknown: the worker did not answer, so its state cannot be claimed.
	StateStatusUnknown = "unknown"
)

// Session lifecycle states.
const (
	StateCreated    = "CREATED"
	StateStarting   = "STARTING"
	StateReady      = "READY"
	StateBusy       = "BUSY"
	StateIdle       = "IDLE"
	StateResetting  = "RESETTING"
	StateCancelling = "CANCELLING"
	StateStopping   = "STOPPING"
	StateStopped    = "STOPPED"
	StateFailed     = "FAILED"
	StateDisposed   = "DISPOSED"
)

// Sharing roles. OWNER is immutable; no operation transfers it.
const (
	RoleOwner    = "OWNER"
	RoleObserver = "OBSERVER"
)

// Capability names a manifest may declare. Registration does not imply a
// capability: Discover reports only what the runtime declared and the host
// verified during the initialize handshake.
const (
	CapPersistent   = "persistent_execution"
	CapModules      = "modules"
	CapFileIO       = "file_io"
	CapScientific   = "scientific"
	CapPlotting     = "plotting"
	CapToolCallback = "tool_callback"
	CapNotebook     = "notebook"
	CapArtifact     = "artifact"
	CapInterrupt    = "interrupt"
	CapDisplay      = "display"
	CapSubprocess   = "subprocess"
	CapStreaming    = "streaming"
)

// Event types published through Options.OnEvent. They mirror the SPI lifecycle.
const (
	EventSessionCreated       = "session_created"
	EventWorkerStarted        = "worker_started"
	EventExecutionStarted     = "execution_started"
	EventStdout               = "stdout"
	EventStderr               = "stderr"
	EventDisplay              = "display"
	EventExecutionCompleted   = "execution_completed"
	EventExecutionFailed      = "execution_failed"
	EventExecutionCancelled   = "execution_cancelled"
	EventTimeout              = "timeout"
	EventWorkerCrashed        = "worker_crashed"
	EventSessionReset         = "session_reset"
	EventSessionClosed        = "session_closed"
	EventArtifactCreated      = "artifact_created"
	EventToolCall             = "tool_call"
	EventToolDenied           = "tool_denied"
	EventResourceLimit        = "resource_limit"
	EventCleanupFailed        = "cleanup_failed"
	EventGrant                = "grant"
	EventRevoke               = "revoke"
	EventStateChanged         = "state_changed"
	EventSessionExpired       = "session_expired"
	EventCallbackUnauthorized = "callback_unauthorized"
)

// Enforcement values report how a limit is actually applied. They are the
// difference between a kernel-enforced bound, an interpreter-enforced one and a
// host-monitored one; the host never reports a supervised limit as hard.
const (
	// EnforcementHard is enforced by the OS or kernel: a cgroup v2 controller or
	// an rlimit.
	EnforcementHard = "hard"
	// EnforcementRuntime is enforced by the interpreter itself, such as V8's
	// heap cap. It bounds what that runtime accounts for, not the whole RSS.
	EnforcementRuntime = "runtime"
	// EnforcementSupervised is a host monitor that kills the process after it
	// observes the overshoot; the overshoot itself is not prevented.
	EnforcementSupervised = "supervised"
	// EnforcementMonitored is the legacy spelling of EnforcementSupervised,
	// kept so a stored declaration still parses. New reports use supervised.
	EnforcementMonitored = EnforcementSupervised
	EnforcementNone      = "none"
)

// Sandbox backends the host can implement with real containment.
const (
	SandboxSeatbelt   = "macos-seatbelt"
	SandboxBubblewrap = "bubblewrap"
)

// Sentinel errors. Callers classify with errors.Is; the text stays stable enough
// for a tool result.
var (
	ErrNotFound              = errors.New("eval: not found")
	ErrDenied                = errors.New("eval: permission denied")
	ErrClosed                = errors.New("eval: manager is closed")
	ErrBusy                  = errors.New("eval: session is busy")
	ErrExpired               = errors.New("eval: lease or grant expired")
	ErrSandboxUnsupported    = errors.New("eval: no supported sandbox on this platform")
	ErrUnsupportedCapability = errors.New("eval: capability is not supported by this runtime")
	ErrLimit                 = errors.New("eval: resource limit exceeded")
	ErrInvalid               = errors.New("eval: invalid argument")
	ErrWorker                = errors.New("eval: worker failure")
	// ErrAuthorityDomain marks an operation that would cross an authority
	// domain: a kernel executes only for its owner. Another actor shares data
	// through artifacts, never through the owner's live heap.
	ErrAuthorityDomain = errors.New("eval: crosses an authority domain")
)

// ToolCaller is the host-authorized tool surface for exactly one execution. The
// manager validates every call against the admitted policy; a worker cannot widen
// it. It is declared here (not aliased) so this package owns its own contract and
// protocol can declare the same method set for its callback context without an
// import cycle; a protocol.ToolCaller value satisfies this interface structurally.
type ToolCaller interface {
	Tools(ctx context.Context) ([]protocol.ToolSpec, error)
	Call(ctx context.Context, name string, arguments map[string]any) (protocol.ToolResult, error)
}

// Budget bounds one execution and one session. Zero fields inherit the next
// broader default (request -> session -> manager Options.Budget); a negative
// value means unlimited where the host can honour it. WallSeconds always has a
// finite effective value: an execution without a deadline is not offered.
//
// MaxWorkers is a manager-wide bound on live kernels (idle and busy both count),
// because a persistent kernel holds its process while idle. MemoryBytes bounds
// one worker's process group; the aggregate is at most MaxWorkers x MemoryBytes
// and is not enforced as a separate total.
//
// CPUSeconds, MemoryBytes and DiskBytes apply to the kernel's process and take
// effect when it starts (CreateOptions/Options budget). A request that asks for a
// lower value is refused instead of silently running under the weaker session
// bound; WallSeconds, OutputBytes, ArtifactBytes, MaxCallbacks and MaxDepth apply
// per request.
type Budget struct {
	WallSeconds   int   `json:"wallSeconds"`
	CPUSeconds    int   `json:"cpuSeconds"`
	MemoryBytes   int64 `json:"memoryBytes"`
	OutputBytes   int64 `json:"outputBytes"`
	ArtifactBytes int64 `json:"artifactBytes"`
	DiskBytes     int64 `json:"diskBytes"`
	MaxWorkers    int   `json:"maxWorkers"`
	MaxCallbacks  int   `json:"maxCallbacks"`
	MaxDepth      int   `json:"maxDepth"`
}

// MergeBudget returns base with every non-zero field of override applied. A
// negative override field is copied verbatim (unlimited); zero keeps base.
func MergeBudget(base, override Budget) Budget {
	out := base
	if override.WallSeconds != 0 {
		out.WallSeconds = override.WallSeconds
	}
	if override.CPUSeconds != 0 {
		out.CPUSeconds = override.CPUSeconds
	}
	if override.MemoryBytes != 0 {
		out.MemoryBytes = override.MemoryBytes
	}
	if override.OutputBytes != 0 {
		out.OutputBytes = override.OutputBytes
	}
	if override.ArtifactBytes != 0 {
		out.ArtifactBytes = override.ArtifactBytes
	}
	if override.DiskBytes != 0 {
		out.DiskBytes = override.DiskBytes
	}
	if override.MaxWorkers != 0 {
		out.MaxWorkers = override.MaxWorkers
	}
	if override.MaxCallbacks != 0 {
		out.MaxCallbacks = override.MaxCallbacks
	}
	if override.MaxDepth != 0 {
		out.MaxDepth = override.MaxDepth
	}
	return out
}

// WithDefaults fills unset fields from def and forces a finite wall clock.
func (b Budget) WithDefaults(def Budget) Budget {
	out := MergeBudget(def, b)
	if out.WallSeconds <= 0 {
		out.WallSeconds = 300
	}
	if out.MaxWorkers <= 0 {
		out.MaxWorkers = 1
	}
	if out.MaxCallbacks < 0 {
		out.MaxCallbacks = 0
	}
	if out.MaxDepth <= 0 {
		out.MaxDepth = 1
	}
	if out.OutputBytes <= 0 {
		out.OutputBytes = 256 << 10
	}
	if out.ArtifactBytes <= 0 {
		out.ArtifactBytes = 8 << 20
	}
	if out.DiskBytes <= 0 {
		out.DiskBytes = 64 << 20
	}
	return out
}

// Capability is one declared runtime capability and whether it is actually
// available. Notes explain a false or partial capability.
type Capability struct {
	Name      string `json:"name"`
	Supported bool   `json:"supported"`
	Notes     string `json:"notes,omitempty"`
}

// SandboxRequirement declares what containment a runtime needs. The host decides
// whether it can provide it; a manifest cannot opt out of sandboxing.
type SandboxRequirement struct {
	Required bool     `json:"required"`
	Backends []string `json:"backends,omitempty"`
	Network  bool     `json:"network,omitempty"`
	Notes    string   `json:"notes,omitempty"`
}

// Limits is the platform resource boundary a runtime claims, including how each
// limit is enforced. EnforcementNone means the host does not pretend to bound it.
type Limits struct {
	WallSeconds       int      `json:"wallSeconds"`
	CPUSeconds        int      `json:"cpuSeconds"`
	MemoryBytes       int64    `json:"memoryBytes"`
	OutputBytes       int64    `json:"outputBytes"`
	ArtifactBytes     int64    `json:"artifactBytes"`
	DiskBytes         int64    `json:"diskBytes"`
	MemoryEnforcement string   `json:"memoryEnforcement"`
	CPUEnforcement    string   `json:"cpuEnforcement"`
	DiskEnforcement   string   `json:"diskEnforcement"`
	Notes             []string `json:"notes,omitempty"`
}

// EnforcementView reports how each bound of one live kernel is actually
// applied on this host. It is a report, not a promise: a field is Hard only
// when the OS or kernel refuses the operation, Runtime when the interpreter
// bounds its own heap, Supervised when a host monitor kills after observing an
// overshoot, and None when nothing bounds it.
type EnforcementView struct {
	Memory string `json:"memory"`
	CPU    string `json:"cpu"`
	Disk   string `json:"disk"`
	// MemoryLimitBytes is the memory bound in force (0: none).
	MemoryLimitBytes int64 `json:"memoryLimitBytes,omitempty"`
	// MemoryRuntimeCapBytes is the interpreter heap cap (V8
	// --max-old-space-size), when the runtime has one. It is not the RSS bound.
	MemoryRuntimeCapBytes int64 `json:"memoryRuntimeCapBytes,omitempty"`
	// MemoryPollMS is the supervision interval for a supervised memory bound;
	// the measured overshoot window is at most this long.
	MemoryPollMS int64    `json:"memoryPollMs,omitempty"`
	Notes        []string `json:"notes,omitempty"`
}

// Zero reports whether nothing is known about the kernel's bounds.
func (v EnforcementView) Zero() bool {
	return v.Memory == "" && v.CPU == "" && v.Disk == ""
}

// Manifest is a runtime plugin's declaration. It is validated by the registry and
// confers no authority: executable paths, workspaces and grants are host-owned.
type Manifest struct {
	ID             string             `json:"id"`
	Version        string             `json:"version"`
	SPIVersion     string             `json:"spiVersion"`
	Language       string             `json:"language"`
	RuntimeVersion string             `json:"runtimeVersion,omitempty"`
	Executable     string             `json:"executable"`
	Args           []string           `json:"args,omitempty"`
	Capabilities   []Capability       `json:"capabilities,omitempty"`
	Dependencies   []string           `json:"dependencies,omitempty"`
	Platforms      []string           `json:"platforms,omitempty"`
	Sandbox        SandboxRequirement `json:"sandbox"`
	Limits         Limits             `json:"limits"`
	ArtifactTypes  []string           `json:"artifactTypes,omitempty"`
	HealthCheck    string             `json:"healthCheck,omitempty"`
	Description    string             `json:"description,omitempty"`
}

// Supports reports whether the manifest declares name as available.
func (m Manifest) Supports(name string) bool {
	for _, c := range m.Capabilities {
		if c.Name == name && c.Supported {
			return true
		}
	}
	return false
}

// SupportsPlatform reports whether the manifest runs on goos. An empty list means
// the plugin declared no platform restriction.
func (m Manifest) SupportsPlatform(goos string) bool {
	if len(m.Platforms) == 0 {
		return true
	}
	for _, p := range m.Platforms {
		if strings.EqualFold(strings.TrimSpace(p), goos) {
			return true
		}
	}
	return false
}

// Budget returns the manifest's declared limits as a budget, for documentation
// and for filling manager defaults. Enforcement metadata is not a promise.
func (m Manifest) Budget() Budget {
	return Budget{
		WallSeconds:   m.Limits.WallSeconds,
		CPUSeconds:    m.Limits.CPUSeconds,
		MemoryBytes:   m.Limits.MemoryBytes,
		OutputBytes:   m.Limits.OutputBytes,
		ArtifactBytes: m.Limits.ArtifactBytes,
		DiskBytes:     m.Limits.DiskBytes,
	}
}

// CapabilityNames returns the supported capability names, sorted.
func (m Manifest) CapabilityNames() []string {
	names := make([]string, 0, len(m.Capabilities))
	for _, c := range m.Capabilities {
		if c.Supported {
			names = append(names, c.Name)
		}
	}
	sort.Strings(names)
	return names
}

// Session is the host's view of one persistent execution kernel.
type Session struct {
	ID            string `json:"id"`
	RuntimeID     string `json:"runtimeId"`
	Language      string `json:"language"`
	OwnerID       string `json:"ownerId"`
	AgentID       string `json:"agentId"`
	ParentAgentID string `json:"parentAgentId,omitempty"`
	// Workspace is the kernel's own private directory inside the host-provided
	// namespace. Sharing the kernel never shares the owner's wider tree.
	Workspace  string    `json:"workspace"`
	Permission string    `json:"permission"`
	WorkerID   string    `json:"workerId,omitempty"`
	State      string    `json:"state"`
	Generation uint64    `json:"generation"`
	Budget     Budget    `json:"budget"`
	LeaseUntil time.Time `json:"leaseUntil,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	LastError  string    `json:"lastError,omitempty"`
	Label      string    `json:"label,omitempty"`
	// ActiveRequestID is the request whose cell runs right now, if any: the
	// target an interrupt names (InterruptTarget.RequestID).
	ActiveRequestID string `json:"activeRequestId,omitempty"`
	// Enforcement says how this kernel's bounds are actually applied. It lets a
	// Hub show "memory: supervised" instead of implying a hard limit.
	Enforcement EnforcementView `json:"enforcement,omitempty"`
}

// InterruptTarget names the one execution an interrupt is meant for. An
// interrupt never picks "whatever runs now": a request that arrives after its
// target finished must not stop the next cell.
type InterruptTarget struct {
	// RequestID is the target execution's request id (Request.ID). Required.
	RequestID string `json:"requestId"`
	// Generation, when set, is the kernel generation the caller saw the target
	// run in; a target running in another generation is refused.
	Generation uint64 `json:"generation,omitempty"`
}

// Interrupt outcome statuses. Only InterruptInterrupted and
// InterruptCancelledBeforeStart mean the interrupt acted on the target.
const (
	// InterruptInterrupted: the target was running, was signalled (and killed
	// if it ignored the signal), and is over. ResultStatus says how it ended.
	InterruptInterrupted = "interrupted"
	// InterruptCancelledBeforeStart: the target was queued; it will not run.
	InterruptCancelledBeforeStart = "cancelled_before_start"
	// InterruptTargetFinished: the target had already finished; nothing was
	// signalled, whatever runs now was not touched.
	InterruptTargetFinished = "target_finished"
	// InterruptNoActiveExecution: the target is unknown to the session and
	// nothing runs in it.
	InterruptNoActiveExecution = "no_active_execution"
	// InterruptGenerationMismatch: the target runs in another kernel
	// generation than the caller named; nothing was signalled.
	InterruptGenerationMismatch = "generation_mismatch"
)

// InterruptOutcome reports what an execution-scoped interrupt did, so a caller
// can confirm which execution it stopped. Repeating an interrupt for the same
// target returns the same outcome and never signals again.
type InterruptOutcome struct {
	Status     string `json:"status"`
	RequestID  string `json:"requestId"`
	Generation uint64 `json:"generation,omitempty"`
	// ResultStatus and StateStatus are how the target ended (for
	// InterruptInterrupted); empty when its Execute call returned an error
	// instead of a result.
	ResultStatus string `json:"resultStatus,omitempty"`
	StateStatus  string `json:"stateStatus,omitempty"`
	// ActiveRequestID is the execution running when a refusal was decided; it
	// was left alone.
	ActiveRequestID string `json:"activeRequestId,omitempty"`
}

// Alive reports whether the session's kernel can accept another cell without a
// restart. It is a view of State, not a health probe.
func (s Session) Alive() bool {
	switch s.State {
	case StateReady, StateIdle, StateBusy:
		return true
	}
	return false
}

// Grant is an explicit, expiring sharing grant on one kernel. Workspace and
// Permission must equal the kernel's: sharing a heap never widens file or tool
// authority.
type Grant struct {
	AgentID    string    `json:"agentId"`
	Role       string    `json:"role"`
	Workspace  string    `json:"workspace"`
	Permission string    `json:"permission"`
	ExpiresAt  time.Time `json:"expiresAt,omitempty"`
}

// CreateOptions is the host's request to create a kernel. OwnerID and AgentID are
// host-derived identities; Workspace is the host-created private directory the
// kernel may use (the manager derives a per-kernel subdirectory inside it).
type CreateOptions struct {
	RuntimeID     string `json:"runtimeId"`
	OwnerID       string `json:"ownerId"`
	AgentID       string `json:"agentId"`
	ParentAgentID string `json:"parentAgentId,omitempty"`
	// Workspace is the kernel's own private directory inside the host-provided
	// namespace. Sharing the kernel never shares the owner's wider tree.
	Workspace  string    `json:"workspace"`
	Permission string    `json:"permission"`
	Budget     Budget    `json:"budget"`
	LeaseUntil time.Time `json:"leaseUntil,omitempty"`
	Label      string    `json:"label,omitempty"`
}

// Input is an artifact the host materializes into the kernel workspace before an
// execution. The manager checks the caller's read access to the source session;
// the worker sees only a file name in its own workspace.
type Input struct {
	SessionID  string `json:"sessionId"`
	ArtifactID string `json:"artifactId"`
	Name       string `json:"name"`
}

// Request is one cell. AllowedCapabilities nil or empty means no tool callbacks;
// the single entry "*" means every tool the host advertises through ToolCaller.
// Deadline zero means the session's wall budget.
type Request struct {
	ID                  string    `json:"id"`
	SessionID           string    `json:"sessionId"`
	CellID              string    `json:"cellId"`
	AgentID             string    `json:"agentId,omitempty"`
	Language            string    `json:"language,omitempty"`
	RunID               string    `json:"runId,omitempty"`
	TrialID             string    `json:"trialId,omitempty"`
	Code                string    `json:"code"`
	WorkingDirectory    string    `json:"workingDirectory,omitempty"`
	Deadline            time.Time `json:"deadline,omitempty"`
	AllowedCapabilities []string  `json:"allowedCapabilities,omitempty"`
	Budget              Budget    `json:"budget"`
	Inputs              []Input   `json:"inputs,omitempty"`
}

// AllowsAllCapabilities reports whether the request asked for the whole host tool
// surface rather than a named subset.
func (r Request) AllowsAllCapabilities() bool {
	for _, name := range r.AllowedCapabilities {
		if name == "*" {
			return true
		}
	}
	return false
}

// AllowsCapability reports whether name may be called by this request.
func (r Request) AllowsCapability(name string) bool {
	if r.AllowsAllCapabilities() {
		return true
	}
	for _, allowed := range r.AllowedCapabilities {
		if allowed == name {
			return true
		}
	}
	return false
}

// Display is bounded rich output. Encoding is "utf-8" (default) or "base64"; when
// the payload is large it is also stored as an artifact and referenced by
// ArtifactID, keeping the inline frame small.
type Display struct {
	MIME       string `json:"mime"`
	Encoding   string `json:"encoding,omitempty"`
	Text       string `json:"text,omitempty"`
	ArtifactID string `json:"artifactId,omitempty"`
	Size       int64  `json:"size,omitempty"`
}

// Artifact is one bounded output file with provenance. It never exposes an
// arbitrary host path: callers read the bytes through Manager.Artifact.
type Artifact struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	CellID    string `json:"cellId,omitempty"`
	RuntimeID string `json:"runtimeId"`
	Name      string `json:"name,omitempty"`
	// Path is relative to the host-owned artifact store, never a host pathname.
	// Bytes still require the session-authorized Artifact API.
	Path   string `json:"path,omitempty"`
	MIME   string `json:"mime"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	// ParentID and SourceArtifactID record cross-runtime provenance: a host that
	// transfers an artifact from another kernel journals the source id here. The
	// manager never invents them.
	ParentID         string    `json:"parentId,omitempty"`
	SourceArtifactID string    `json:"sourceArtifactId,omitempty"`
	CreatedAt        time.Time `json:"createdAt"`
	ExpiresAt        time.Time `json:"expiresAt,omitempty"`
}

// Usage reports what one execution consumed. MemoryPeakBytes is 0 when the host
// cannot measure it; it is never a hard-limit claim.
type Usage struct {
	WallMS          int64 `json:"wallMs"`
	CPUMs           int64 `json:"cpuMs"`
	MemoryPeakBytes int64 `json:"memoryPeakBytes,omitempty"`
	OutputBytes     int64 `json:"outputBytes"`
	OutputTruncated bool  `json:"outputTruncated,omitempty"`
	ArtifactBytes   int64 `json:"artifactBytes"`
	Callbacks       int   `json:"callbacks"`
	ToolCalls       int   `json:"toolCalls"`
	Depth           int   `json:"depth"`
}

// Result is the outcome of one cell. Status says how the execution ended;
// StateStatus says whether the kernel survived it. A failed cell does not roll
// back its side effects.
type Result struct {
	RequestID      string     `json:"requestId"`
	SessionID      string     `json:"sessionId"`
	CellID         string     `json:"cellId,omitempty"`
	RuntimeID      string     `json:"runtimeId,omitempty"`
	Language       string     `json:"language,omitempty"`
	Status         string     `json:"status"`
	Stdout         string     `json:"stdout,omitempty"`
	Stderr         string     `json:"stderr,omitempty"`
	Error          string     `json:"error,omitempty"`
	Traceback      string     `json:"traceback,omitempty"`
	StateStatus    string     `json:"stateStatus"`
	Generation     uint64     `json:"generation"`
	ExitCode       int        `json:"exitCode"`
	PartialEffects bool       `json:"partialEffects"`
	Displays       []Display  `json:"displays,omitempty"`
	Artifacts      []Artifact `json:"artifacts,omitempty"`
	DurationMS     int64      `json:"durationMs"`
	Usage          Usage      `json:"usage"`
	StartedAt      time.Time  `json:"startedAt"`
	CompletedAt    time.Time  `json:"completedAt"`
	// Limit names the resource bound that stopped this execution ("memory",
	// "cpu", "disk"), or is empty when no bound fired.
	Limit string `json:"limit,omitempty"`
	// Enforcement is how that bound was applied (hard, runtime, supervised). It
	// is set together with Limit, so a caller never reads a supervised kill as a
	// kernel guarantee.
	Enforcement string `json:"enforcement,omitempty"`
}

// Succeeded reports whether the cell ran to completion. It is not an acceptance
// of the cell's semantics.
func (r Result) Succeeded() bool {
	return r.Status == ResultStatusCompleted
}

// Interrupted reports whether the result stopped early and left effects that may
// be unknown (the tool layer maps this to protocol.ToolResult.Interrupted).
func (r Result) Interrupted() bool {
	switch r.Status {
	case ResultStatusTimeout, ResultStatusCancelled, ResultStatusCrashed:
		return true
	}
	return r.StateStatus == StateStatusLost
}

// Event is one lifecycle notification. Data holds bounded, typed content; stdout
// and stderr chunks are capped by the manager.
type Event struct {
	Type       string    `json:"type"`
	SessionID  string    `json:"sessionId,omitempty"`
	OwnerID    string    `json:"ownerId,omitempty"`
	AgentID    string    `json:"agentId,omitempty"`
	RunID      string    `json:"runId,omitempty"`
	TrialID    string    `json:"trialId,omitempty"`
	RequestID  string    `json:"requestId,omitempty"`
	CellID     string    `json:"cellId,omitempty"`
	Generation uint64    `json:"generation,omitempty"`
	Data       any       `json:"data,omitempty"`
	At         time.Time `json:"at"`
}

// Health is a live probe of one kernel.
type Health struct {
	SessionID      string    `json:"sessionId"`
	RuntimeID      string    `json:"runtimeId"`
	RuntimeVersion string    `json:"runtimeVersion,omitempty"`
	State          string    `json:"state"`
	Generation     uint64    `json:"generation"`
	WorkerAlive    bool      `json:"workerAlive"`
	CheckedAt      time.Time `json:"checkedAt"`
	LastError      string    `json:"lastError,omitempty"`
}

// callDepthKey carries the tool-callback recursion depth of an execution.
type callDepthKey struct{}

// WithCallDepth returns ctx carrying the callback recursion depth. The bridge
// increments it for every tool call, so an eval -> tool -> eval cycle is bounded.
func WithCallDepth(ctx context.Context, depth int) context.Context {
	return context.WithValue(ctx, callDepthKey{}, depth)
}

// CallDepth is the recursion depth WithCallDepth stored; 0 when absent.
func CallDepth(ctx context.Context) int {
	depth, _ := ctx.Value(callDepthKey{}).(int)
	return depth
}

// ValidRole reports whether role is an ownership or read-only sharing role.
func ValidRole(role string) bool {
	switch role {
	case RoleOwner, RoleObserver:
		return true
	}
	return false
}

// RoleRank orders owner above observer.
func RoleRank(role string) int {
	switch role {
	case RoleOwner:
		return 3
	case RoleObserver:
		return 1
	}
	return 0
}
