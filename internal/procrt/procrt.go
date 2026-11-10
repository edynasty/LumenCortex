// Package procrt is the shared managed-process base for every supervised
// child process LumenCortex starts: eval kernels, debug adapters, jobs, hooks,
// bash commands and MCP servers. It owns exactly four things — ownership,
// census, escalation and resource-enforcement reporting — and nothing about the
// protocols those processes speak. evalruntime keeps its kernel protocol,
// internal/debugger keeps DAP, internal/jobs keeps its queue.
//
// The registry is bookkeeping, not containment: a process is bound by whatever
// sandbox and limits its launcher applied. Status.Enforcement says how each
// bound is really applied on this host (hard / runtime / supervised / none), so
// a caller never reads a monitor as a kernel limit.
package procrt

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/edynasty/LumenCortex/internal/streambuf"
)

// Components name the subsystem that owns a supervised process.
const (
	ComponentEval = "eval"
	ComponentDAP  = "dap"
	ComponentJob  = "job"
	ComponentHook = "hook"
	ComponentBash = "bash"
	ComponentMCP  = "mcp"
)

// Enforcement labels state how a bound is actually applied. They are values,
// not promises: a host reports what it verified.
const (
	// Hard is enforced by the OS or kernel: a cgroup v2 controller, an rlimit,
	// or a sandbox that denies the operation outright.
	Hard = "hard"
	// Runtime is enforced by the interpreter itself (for example V8's heap cap).
	// It bounds what the runtime accounts, not the whole resident set.
	Runtime = "runtime"
	// Supervised is a host monitor that kills the process after it observes the
	// overshoot. It never prevents the overshoot.
	Supervised = "supervised"
	// None means the host applies no bound.
	None = "none"
)

// Process states reported by the census.
const (
	StateRunning  = "running"
	StateStopping = "stopping"
	StateExited   = "exited"
	StateFailed   = "failed"
)

// Owner names who a process belongs to. SessionID is the Core session (or run
// owner) whose revocation must stop it; Component says which subsystem started
// it; ObjectID is that subsystem's own identity (eval session, debug session,
// job id) and is never interpreted here. An empty field in a filter matches any
// value.
type Owner struct {
	SessionID string `json:"sessionId,omitempty"`
	RunID     string `json:"runId,omitempty"`
	Component string `json:"component,omitempty"`
	ObjectID  string `json:"objectId,omitempty"`
}

// Matches reports whether o satisfies filter. An empty filter field matches.
func (o Owner) Matches(filter Owner) bool {
	if filter.SessionID != "" && o.SessionID != filter.SessionID {
		return false
	}
	if filter.RunID != "" && o.RunID != filter.RunID {
		return false
	}
	if filter.Component != "" && o.Component != filter.Component {
		return false
	}
	if filter.ObjectID != "" && o.ObjectID != filter.ObjectID {
		return false
	}
	return true
}

// Enforcement reports how each bound that the launcher requested is applied.
// The zero value means "no bound applied", which is reported as None by the
// accessors; a caller must not read an empty label as a guarantee.
type Enforcement struct {
	Memory string `json:"memory"`
	CPU    string `json:"cpu"`
	Disk   string `json:"disk"`
	// MemoryLimitBytes is the bound the launcher asked for (0: none).
	MemoryLimitBytes int64 `json:"memoryLimitBytes,omitempty"`
	// MemoryRuntimeCapBytes is the interpreter-enforced heap cap, when the
	// runtime has one (V8 --max-old-space-size). It is not the RSS bound.
	MemoryRuntimeCapBytes int64 `json:"memoryRuntimeCapBytes,omitempty"`
	// MemoryPollMS is the monitor interval for a supervised memory bound.
	MemoryPollMS int64    `json:"memoryPollMs,omitempty"`
	Notes        []string `json:"notes,omitempty"`
}

// MemoryLimitLabel returns the memory label, never empty.
func (e Enforcement) MemoryLimitLabel() string { return label(e.Memory) }

func label(v string) string {
	switch v {
	case Hard, Runtime, Supervised:
		return v
	default:
		return None
	}
}

// Usage is the census's live view of one process. Fields a host cannot measure
// are zero, never estimated.
type Usage struct {
	CPUMs           int64 `json:"cpuMs,omitempty"`
	RSSBytes        int64 `json:"rssBytes,omitempty"`
	MemoryPeakBytes int64 `json:"memoryPeakBytes,omitempty"`
	OutputBytes     int64 `json:"outputBytes,omitempty"`
}

// Status is one row of the census.
type Status struct {
	Owner       Owner       `json:"owner"`
	PID         int         `json:"pid"`
	TreePIDs    []int       `json:"treePids,omitempty"`
	State       string      `json:"state"`
	StartedAt   time.Time   `json:"startedAt"`
	Usage       Usage       `json:"usage"`
	Enforcement Enforcement `json:"enforcement"`
}

// Handle is what a supervised process must expose to the registry. A launcher
// implements it over the process it started; the registry never touches the
// process directly.
type Handle interface {
	// PID is the process the launcher started.
	PID() int
	// TreePIDs lists the descendants the launcher knows about. It may be empty;
	// an empty list is not a claim that no descendant exists.
	TreePIDs() []int
	// State is one of the State* values.
	State() string
	Usage() Usage
	Enforcement() Enforcement
	// Interrupt asks the process to stop cooperatively.
	Interrupt() error
	// Terminate asks the whole tree to exit and returns the pids that survived
	// its own cleanup window.
	Terminate(ctx context.Context) []int
	// Kill stops the whole tree unconditionally and returns survivors.
	Kill() []int
	// Done is closed once the process has been reaped.
	Done() <-chan struct{}
}

// Ladder is the escalation policy of CloseOwner and of any caller that stops a
// process it does not own the internals of: interrupt, wait, terminate, wait,
// kill, wait. Each wait is bounded so a caller can never block forever.
type Ladder struct {
	InterruptWait time.Duration
	TerminateWait time.Duration
	KillWait      time.Duration
}

// DefaultLadder is the escalation timing used by the Engine: a cooperative
// interrupt gets two seconds (the eval workers repeat their own interrupt on a
// shorter interval), a terminate gets two, and a kill is allowed one second to
// reap before its survivors are reported.
func DefaultLadder() Ladder {
	return Ladder{InterruptWait: 2 * time.Second, TerminateWait: 2 * time.Second, KillWait: time.Second}
}

// Escalation describes what CloseOwner did to one process.
type Escalation struct {
	Owner     Owner    `json:"owner"`
	PID       int      `json:"pid"`
	Steps     []string `json:"steps"`
	Survivors []int    `json:"survivors,omitempty"`
}

type entry struct {
	owner     Owner
	handle    Handle
	startedAt time.Time
}

// Registry is the process census. It is safe for concurrent use and holds no
// process resources: unregistering a handle never stops the process, and
// CloseOwner stops what is registered.
type Registry struct {
	mu      sync.Mutex
	entries map[*entry]struct{}
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{entries: make(map[*entry]struct{})}
}

var defaultRegistry = New()

// Default is the process registry the Engine reports from. One per host process:
// every launcher registers here, so the census is complete across subsystems.
func Default() *Registry { return defaultRegistry }

// Register adds h under o and returns the function that removes it. The
// function is idempotent. A nil handle is refused (no census row is invented).
func (r *Registry) Register(o Owner, h Handle) func() {
	if r == nil || h == nil {
		return func() {}
	}
	e := &entry{owner: o, handle: h, startedAt: time.Now()}
	r.mu.Lock()
	r.entries[e] = struct{}{}
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.entries, e)
			r.mu.Unlock()
		})
	}
}

// List returns the census rows matching filter, ordered by pid. A process that
// already exited but was not unregistered is reported as exited, never hidden.
func (r *Registry) List(filter Owner) []Status {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	entries := make([]*entry, 0, len(r.entries))
	for e := range r.entries {
		if e.owner.Matches(filter) {
			entries = append(entries, e)
		}
	}
	r.mu.Unlock()
	out := make([]Status, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.status())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PID != out[j].PID {
			return out[i].PID < out[j].PID
		}
		return out[i].Owner.ObjectID < out[j].Owner.ObjectID
	})
	return out
}

// Count is the number of registered processes, for census assertions.
func (r *Registry) Count() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

func (e *entry) status() (s Status) {
	s = Status{Owner: e.owner, StartedAt: e.startedAt, State: StateFailed}
	// A handle's own accessors may panic after its process is gone; the census
	// must still answer.
	defer func() {
		if recover() != nil {
			s.State = StateExited
		}
	}()
	s.PID = e.handle.PID()
	s.TreePIDs = append([]int(nil), e.handle.TreePIDs()...)
	s.State = e.handle.State()
	s.Usage = e.handle.Usage()
	s.Enforcement = e.handle.Enforcement()
	return s
}

// CloseOwner escalates every process matching filter: interrupt, then
// terminate, then kill. It is the one stop path a session revocation, a run
// deletion and an Engine shutdown share, so no subsystem needs its own timing.
// Entries it acted on are unregistered whether or not the stop was clean; a
// non-empty survivors list is returned as an error, because a stop that left
// processes behind must never be reported as success.
func (r *Registry) CloseOwner(ctx context.Context, filter Owner, ladder Ladder) ([]Escalation, error) {
	if r == nil {
		return nil, nil
	}
	if ladder.InterruptWait <= 0 {
		ladder = DefaultLadder()
	}
	r.mu.Lock()
	targets := make([]*entry, 0, len(r.entries))
	for e := range r.entries {
		if e.owner.Matches(filter) {
			targets = append(targets, e)
		}
	}
	r.mu.Unlock()
	sort.Slice(targets, func(i, j int) bool { return targets[i].handle.PID() < targets[j].handle.PID() })

	reports := make([]Escalation, 0, len(targets))
	var survivors []int
	var errs []error
	for _, e := range targets {
		report := Escalation{Owner: e.owner, PID: e.handle.PID()}
		done := e.handle.Done()
		if !exited(done) {
			report.Steps = append(report.Steps, "interrupt")
			if err := e.handle.Interrupt(); err != nil {
				errs = append(errs, fmt.Errorf("interrupt pid %d: %w", report.PID, err))
			}
			waitDone(ctx, done, ladder.InterruptWait)
		}
		if !exited(done) {
			report.Steps = append(report.Steps, "terminate")
			if left := e.handle.Terminate(ctx); len(left) > 0 {
				report.Survivors = mergeInts(report.Survivors, left)
			}
			waitDone(ctx, done, ladder.TerminateWait)
		}
		if !exited(done) || len(report.Survivors) > 0 {
			report.Steps = append(report.Steps, "kill")
			if left := e.handle.Kill(); len(left) > 0 {
				report.Survivors = mergeInts(report.Survivors, left)
			}
			waitDone(ctx, done, ladder.KillWait)
		}
		report.Survivors = livingOnly(report.Survivors)
		if len(report.Survivors) > 0 {
			survivors = mergeInts(survivors, report.Survivors)
			errs = append(errs, fmt.Errorf("%w: owner %+v pid %d left %v running", ErrCleanupFailed, e.owner, report.PID, report.Survivors))
		}
		reports = append(reports, report)
		r.mu.Lock()
		delete(r.entries, e)
		r.mu.Unlock()
	}
	return reports, errors.Join(errs...)
}

// ErrCleanupFailed reports a stop that could not certify that the tree is gone.
var ErrCleanupFailed = errors.New("procrt: process cleanup incomplete")

func exited(done <-chan struct{}) bool {
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func waitDone(ctx context.Context, done <-chan struct{}, d time.Duration) {
	if done == nil {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	case <-ctx.Done():
	}
}

func mergeInts(base, add []int) []int {
	seen := make(map[int]struct{}, len(base)+len(add))
	out := make([]int, 0, len(base)+len(add))
	for _, pid := range append(append([]int(nil), base...), add...) {
		if pid <= 0 {
			continue
		}
		if _, ok := seen[pid]; ok {
			continue
		}
		seen[pid] = struct{}{}
		out = append(out, pid)
	}
	sort.Ints(out)
	return out
}

func livingOnly(pids []int) []int {
	out := make([]int, 0, len(pids))
	for _, pid := range pids {
		if ProcessAlive(pid) {
			out = append(out, pid)
		}
	}
	return out
}

// Ring is the bounded output buffer every supervised process shares: retained
// memory is O(head+tail) whatever the process printed. It wraps streambuf so a
// launcher does not grow a second output policy.
type Ring struct{ *streambuf.HeadTail }

// NewRing builds a ring with the given head and tail bounds.
func NewRing(head, tail int) *Ring { return &Ring{HeadTail: streambuf.New(head, tail)} }

// Snapshot is a copy of the retained bytes.
func (r *Ring) Snapshot() streambuf.Snapshot {
	if r == nil || r.HeadTail == nil {
		return streambuf.Snapshot{}
	}
	return r.HeadTail.Snapshot()
}
