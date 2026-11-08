package evalruntime

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/protocol"
)

// inlineDisplayLimit is the largest rich payload kept inline in a result. Larger
// images are stored as artifacts and referenced, so a result frame stays bounded.
const inlineDisplayLimit = 64 << 10

// maxDisplays bounds the displays one cell may return.
const maxDisplays = 16

// maxCellArtifacts bounds the artifacts one cell may declare. It matches the
// workers' own collection bound and the store's adoption bound, so a flood is
// refused before the host walks an unbounded list.
const maxCellArtifacts = 256

// maxCodeBytes bounds one cell's source. The control channel frame limit is
// maxFrame; a code body larger than this could never be delivered, so it is an
// admission error instead of a frame failure that kills a healthy kernel.
const maxCodeBytes = maxFrame - (256 << 10)

// Options configures the host. Root holds only immutable worker scripts and host
// plugin manifests; all session content lives in the per-session private
// directory the host passes to Create. Executables are explicit or discovered on
// PATH: nothing is ever installed.
type Options struct {
	Root       string
	Python     string
	JavaScript string
	Budget     evalapi.Budget
	OnEvent    func(evalapi.Event)
	Sandbox    SandboxOptions
}

// DefaultBudget is the host fallback when neither Options.Budget nor a request
// sets a field. MaxWorkers is 2 so a mixed-language host can hold a Python and a
// JavaScript kernel at once; a kernel counts while it is idle too.
func DefaultBudget() evalapi.Budget {
	return evalapi.Budget{
		WallSeconds:   300,
		CPUSeconds:    120,
		MemoryBytes:   2 << 30,
		OutputBytes:   256 << 10,
		ArtifactBytes: 8 << 20,
		DiskBytes:     64 << 20,
		MaxWorkers:    2,
		MaxCallbacks:  64,
		MaxDepth:      2,
	}
}

// Manager owns the runtime registry and every kernel of one host process.
type Manager struct {
	opts       Options
	root       string
	workersDir string
	launcher   *launcher
	registry   *registry
	// cgroups is the delegated cgroup subtree that bounds every worker (Linux).
	// isolationReason says why it is missing; no worker starts without it.
	cgroups         *cgroupPool
	isolationReason string
	defaultBudget   evalapi.Budget
	// maxKernels bounds live kernels (idle or busy) across this manager. It is a
	// host decision; a session always owns exactly one worker.
	maxKernels int
	createMu   sync.Mutex

	mu       sync.RWMutex
	closed   bool
	sessions map[string]*session
	byOwner  map[string]map[string]struct{}
}

// New builds a manager. It fails closed when the platform has no real sandbox:
// running cells in a bare cwd is never the fallback.
func New(opts Options) (*Manager, error) {
	if strings.TrimSpace(opts.Root) == "" {
		return nil, fmt.Errorf("%w: Root is required", evalapi.ErrInvalid)
	}
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return nil, err
	}
	for _, dir := range []string{root, filepath.Join(root, "plugins")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	sandbox, err := newLauncher(opts.Sandbox)
	if err != nil {
		return nil, err
	}
	workersDir, err := materializeWorkers(root)
	if err != nil {
		return nil, err
	}
	defaultBudget := opts.Budget.WithDefaults(DefaultBudget())
	m := &Manager{
		opts:          opts,
		root:          root,
		workersDir:    workersDir,
		launcher:      sandbox,
		registry:      newRegistry(),
		defaultBudget: defaultBudget,
		maxKernels:    defaultBudget.MaxWorkers,
		sessions:      make(map[string]*session),
		byOwner:       make(map[string]map[string]struct{}),
	}

	subprocessAllowed := subprocessSupported()
	// Linux bounds every worker with its own cgroup. Without one there is no
	// memory limit and no atomic tree stop, so no worker is started at all.
	memoryEnforcement := evalapi.EnforcementMonitored
	isolationReason := ""
	if cgroupRequired {
		memoryEnforcement = evalapi.EnforcementHard
		pool, poolErr := newCgroupPool()
		if poolErr != nil {
			memoryEnforcement = evalapi.EnforcementNone
			isolationReason = "resource isolation unavailable: " + poolErr.Error()
		} else {
			m.cgroups = pool
		}
	}
	m.isolationReason = isolationReason
	python, pythonReason := resolveExecutable(opts.Python, "python3")
	pythonAvailable := python != ""
	if pythonAvailable {
		resolved, resolveErr := pythonRuntimeExecutable(python)
		if resolveErr != nil {
			pythonAvailable = false
			pythonReason = resolveErr.Error()
		} else {
			python = resolved
		}
	}
	if !pythonAvailable {
		python = strings.TrimSpace(opts.Python)
		if python == "" {
			python = "python3"
		}
	}
	caps := map[string]bool{}
	if pythonAvailable {
		caps = pythonCapabilities(python)
	}
	if pythonAvailable && isolationReason != "" {
		pythonAvailable, pythonReason = false, isolationReason
	}
	if err := m.registry.register(pythonManifest(python, filepath.Join(workersDir, pythonWorkerName), caps, platformLimits(sandbox.backend, m.defaultBudget, subprocessAllowed, memoryEnforcement), subprocessAllowed), true, pythonAvailable, pythonReason); err != nil {
		return nil, m.abandon(err)
	}
	if p, ok := m.registry.get("python"); ok && p.available {
		p.readPaths, err = pythonReadPaths(p.manifest.Executable)
		if err == nil {
			err = m.probeBuiltin(p)
		}
		if err != nil {
			p.available = false
			p.reason = err.Error()
		}
	}

	js, jsReason := resolveExecutable(opts.JavaScript, "node", "nodejs")
	jsAvailable := js != ""
	if jsAvailable && isolationReason != "" {
		jsAvailable, jsReason = false, isolationReason
	}
	if !jsAvailable {
		js = strings.TrimSpace(opts.JavaScript)
		if js == "" {
			js = "node"
		}
	}
	if err := m.registry.register(javascriptManifest(js, filepath.Join(workersDir, javascriptWorkerName), platformLimits(sandbox.backend, m.defaultBudget, subprocessAllowed, memoryEnforcement), subprocessAllowed), true, jsAvailable, jsReason); err != nil {
		return nil, m.abandon(err)
	}
	if p, ok := m.registry.get("javascript"); ok && p.available {
		if probeErr := m.probeBuiltin(p); probeErr != nil {
			p.available = false
			p.reason = probeErr.Error()
		}
	}
	if err := m.registry.loadPluginDir(filepath.Join(root, "plugins")); err != nil {
		return nil, m.abandon(err)
	}
	return m, nil
}

// abandon releases what a failed New already acquired.
func (m *Manager) abandon(err error) error {
	if m.cgroups != nil {
		_ = m.cgroups.close()
		m.cgroups = nil
	}
	return err
}

// subprocessSupported reports whether this host can clean up an arbitrary
// descendant tree. Linux runs the worker in its own cgroup inside a private PID
// namespace, so cgroup.kill stops every descendant. macOS has no
// equivalent, so fork is denied and the capability is declared unsupported.
func subprocessSupported() bool { return runtime.GOOS == "linux" }

// resolveExecutable prefers the explicit host path, else the first program found
// on PATH. An explicit path that does not exist is an error reason, not a silent
// fallback to another interpreter.
func resolveExecutable(explicit string, names ...string) (string, string) {
	explicit = strings.TrimSpace(explicit)
	if explicit != "" {
		path := explicit
		if !filepath.IsAbs(path) {
			resolved, err := exec.LookPath(path)
			if err != nil {
				return "", fmt.Sprintf("configured executable %q was not found", explicit)
			}
			path = resolved
		}
		info, err := os.Stat(path)
		if err != nil {
			return "", fmt.Sprintf("configured executable %q is missing", explicit)
		}
		if info.IsDir() {
			return "", fmt.Sprintf("configured executable %q is a directory", explicit)
		}
		return path, ""
	}
	for _, name := range names {
		if path, err := exec.LookPath(name); err == nil {
			return path, ""
		}
	}
	return "", fmt.Sprintf("none of %s is installed; set the executable explicitly", strings.Join(names, ", "))
}

func platformName() string { return runtime.GOOS }

// workspaceOf resolves the host-provided namespace the kernel will live in.
func workspaceOf(workspace string) string {
	if abs, err := filepath.Abs(workspace); err == nil {
		return abs
	}
	return workspace
}

func (m *Manager) ensureOpen() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return evalapi.ErrClosed
	}
	return nil
}

func (m *Manager) emit(event evalapi.Event) {
	if m.opts.OnEvent == nil {
		return
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	func() {
		defer func() { _ = recover() }()
		m.opts.OnEvent(event)
	}()
}

// Register accepts a host-approved manifest. It grants no authority: executable
// paths, workspaces, budgets and tool policy remain host decisions.
func (m *Manager) Register(manifest evalapi.Manifest) error {
	if err := m.ensureOpen(); err != nil {
		return err
	}
	available, reason := pluginAvailable(manifest)
	return m.registry.register(manifest, false, available, reason)
}

// Discover lists the runtimes this host can actually run, with the capabilities
// each one declared and the host verified.
func (m *Manager) Discover(ctx context.Context) ([]evalapi.Manifest, error) {
	if err := m.ensureOpen(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return m.registry.discover(), nil
}

// Create makes one isolated kernel in a private slot of the host-provided
// workspace and starts its worker, so a broken interpreter or sandbox fails here
// rather than on the first cell.
func (m *Manager) Create(ctx context.Context, opts evalapi.CreateOptions) (evalapi.Session, error) {
	if err := m.ensureOpen(); err != nil {
		return evalapi.Session{}, err
	}
	if err := ctx.Err(); err != nil {
		return evalapi.Session{}, err
	}
	runtimeID := strings.TrimSpace(opts.RuntimeID)
	p, ok := m.registry.get(runtimeID)
	if !ok {
		return evalapi.Session{}, fmt.Errorf("%w: runtime %q is not registered", evalapi.ErrNotFound, runtimeID)
	}
	if !p.available {
		return evalapi.Session{}, fmt.Errorf("%w: runtime %q is unavailable: %s", evalapi.ErrNotFound, runtimeID, p.reason)
	}
	if !p.manifest.SupportsPlatform(platformName()) {
		return evalapi.Session{}, fmt.Errorf("%w: runtime %q does not support %s", evalapi.ErrUnsupportedCapability, runtimeID, platformName())
	}
	// The kernel cap counts idle and busy workers: a persistent kernel holds its
	// process (and its memory) whether or not a cell is running. The slot and
	// the session record are reserved under createMu, so two concurrent Creates
	// cannot pick the same slot; the worker is started *outside* createMu so a
	// hung interpreter or plugin cannot stall an unrelated Create.
	m.createMu.Lock()
	m.mu.RLock()
	live := m.liveKernelsLocked()
	m.mu.RUnlock()
	if m.maxKernels > 0 && live >= m.maxKernels {
		m.createMu.Unlock()
		return evalapi.Session{}, fmt.Errorf("%w: this host allows %d live execution kernel(s); close one first", evalapi.ErrLimit, m.maxKernels)
	}
	workspace := workspaceOf(opts.Workspace)
	slot, ok := m.allocateSlot(workspace)
	if !ok {
		m.createMu.Unlock()
		return evalapi.Session{}, fmt.Errorf("%w: no free kernel slot in this workspace", evalapi.ErrLimit)
	}
	s, err := newSession(m, p, opts, slot)
	if err != nil {
		m.createMu.Unlock()
		return evalapi.Session{}, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.createMu.Unlock()
		_ = s.removeFiles()
		return evalapi.Session{}, evalapi.ErrClosed
	}
	m.sessions[s.id] = s
	if m.byOwner[s.ownerID] == nil {
		m.byOwner[s.ownerID] = make(map[string]struct{})
	}
	m.byOwner[s.ownerID][s.id] = struct{}{}
	m.mu.Unlock()
	m.createMu.Unlock()

	m.emit(evalapi.Event{Type: evalapi.EventSessionCreated, SessionID: s.id, OwnerID: s.ownerID, AgentID: s.agentID,
		Generation: s.generationValue(), Data: map[string]string{"runtimeId": s.manifest.ID, "language": s.manifest.Language}})

	s.setState(evalapi.StateStarting, "")
	if _, err := s.startWorker(ctx); err != nil {
		s.setState(evalapi.StateFailed, err.Error())
		m.mu.Lock()
		delete(m.sessions, s.id)
		delete(m.byOwner[s.ownerID], s.id)
		m.mu.Unlock()
		_ = s.removeFiles()
		return evalapi.Session{}, err
	}
	return s.public(), nil
}

// liveKernelsLocked counts sessions that still hold a kernel slot. A session
// whose worker died keeps its slot: it can restart at any moment, so counting it
// only while a process happens to be alive would let the cap be exceeded by a
// restart. The caller holds m.mu (read or write).
func (m *Manager) liveKernelsLocked() int {
	live := 0
	for _, s := range m.sessions {
		s.mu.Lock()
		holds := !s.closed && s.state != evalapi.StateStopped && s.state != evalapi.StateDisposed
		s.mu.Unlock()
		if holds {
			live++
		}
	}
	return live
}

// allocateSlot picks the lowest free kernel slot in the workspace. Slots are
// pre-allocated so a kernel created later never collides with a running profile.
func (m *Manager) allocateSlot(workspace string) (int, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	used := make(map[int]bool, len(m.sessions))
	for _, other := range m.sessions {
		if other.workspace != workspace {
			continue
		}
		other.mu.Lock()
		live := !other.closed && other.state != evalapi.StateStopped && other.state != evalapi.StateDisposed
		other.mu.Unlock()
		if live {
			used[other.slot] = true
		}
	}
	for slot := 0; slot < m.maxKernels; slot++ {
		if !used[slot] {
			return slot, true
		}
	}
	return 0, false
}

// reportResidual turns surviving descendants into a hard error and an event: a
// stop is never reported as clean when something outlived the cleanup window.
func (m *Manager) reportResidual(s *session, residual []int, action string) error {
	if len(residual) == 0 {
		return nil
	}
	m.emit(evalapi.Event{Type: evalapi.EventCleanupFailed, SessionID: s.id, OwnerID: s.ownerID, AgentID: s.agentID,
		Generation: s.generationValue(), Data: map[string]any{"action": action, "pids": residual}})
	return fmt.Errorf("%w: %d descendant process(es) survived %s: %v", evalapi.ErrWorker, len(residual), action, residual)
}

func (m *Manager) session(id string) (*session, error) {
	m.mu.RLock()
	s, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: session %s", evalapi.ErrNotFound, id)
	}
	return s, nil
}

// Sessions lists the kernels agentID owns or was granted, oldest first.
func (m *Manager) Sessions(ctx context.Context, agentID string) ([]evalapi.Session, error) {
	if err := m.ensureOpen(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	all := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.RUnlock()
	out := make([]evalapi.Session, 0, len(all))
	for _, s := range all {
		if !s.canRead(agentID) {
			continue
		}
		out = append(out, s.public())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *Manager) Session(ctx context.Context, sessionID, agentID string) (evalapi.Session, error) {
	if err := m.ensureOpen(); err != nil {
		return evalapi.Session{}, err
	}
	s, err := m.session(sessionID)
	if err != nil {
		return evalapi.Session{}, err
	}
	if !s.canRead(agentID) {
		return evalapi.Session{}, fmt.Errorf("%w: agent %q has no access to session %s", evalapi.ErrDenied, agentID, sessionID)
	}
	return s.public(), nil
}

// Health probes a kernel.
func (m *Manager) Health(ctx context.Context, sessionID, agentID string) (evalapi.Health, error) {
	if err := m.ensureOpen(); err != nil {
		return evalapi.Health{}, err
	}
	s, err := m.session(sessionID)
	if err != nil {
		return evalapi.Health{}, err
	}
	if !s.canRead(agentID) {
		return evalapi.Health{}, fmt.Errorf("%w: agent %q has no access to session %s", evalapi.ErrDenied, agentID, sessionID)
	}
	s.mu.Lock()
	w := s.worker
	state := s.state
	generation := s.generation
	lastErr := s.lastError
	s.mu.Unlock()
	health := evalapi.Health{
		SessionID:  s.id,
		RuntimeID:  s.manifest.ID,
		State:      state,
		Generation: generation,
		CheckedAt:  time.Now().UTC(),
		LastError:  lastErr,
	}
	if w != nil && w.alive() {
		health.WorkerAlive = true
		if state == evalapi.StateBusy || state == evalapi.StateCancelling {
			// The worker's control loop is executing the cell, so a probe would
			// block until it finishes. Liveness comes from the process itself;
			// the runtime version is whatever the handshake reported.
			health.RuntimeVersion = w.runtime
			return health, nil
		}
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		res, probeErr := w.health(probeCtx)
		cancel()
		if probeErr == nil && res.Status != "" {
			health.RuntimeVersion = w.runtime
		}
	}
	return health, nil
}

// Execute runs one cell. Admission is checked here again: a grant authorizes the
// heap, never a broader workspace, permission or tool policy.
func (m *Manager) Execute(ctx context.Context, req evalapi.Request, caller evalapi.ToolCaller) (evalapi.Result, error) {
	if err := m.ensureOpen(); err != nil {
		return evalapi.Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return evalapi.Result{}, err
	}
	s, err := m.session(req.SessionID)
	if err != nil {
		return evalapi.Result{}, err
	}
	agent := strings.TrimSpace(req.AgentID)
	if agent == "" {
		agent = s.ownerID
	}
	if !s.canExecute(agent) {
		// Both sentinels: ErrDenied is the long-standing classification, and
		// ErrAuthorityDomain names the reason this particular refusal exists.
		return evalapi.Result{}, errors.Join(evalapi.ErrDenied, evalapi.ErrAuthorityDomain,
			fmt.Errorf("agent %q may not execute in session %s owned by %q; each actor runs cells in its own kernel and shares data through artifacts", agent, s.id, s.ownerID))
	}
	if err := s.checkLease(); err != nil {
		return evalapi.Result{}, err
	}
	if strings.TrimSpace(req.ID) == "" {
		return evalapi.Result{}, fmt.Errorf("%w: request id is required", evalapi.ErrInvalid)
	}
	if strings.TrimSpace(req.Code) == "" {
		return evalapi.Result{}, fmt.Errorf("%w: code is required", evalapi.ErrInvalid)
	}
	if req.Language != "" && !strings.EqualFold(req.Language, s.manifest.Language) {
		return evalapi.Result{}, fmt.Errorf("%w: session %s runs %s, not %s", evalapi.ErrInvalid, s.id, s.manifest.Language, req.Language)
	}
	// A frame the control channel cannot carry is refused here, before the
	// kernel is touched: an oversized cell is an admission error, never a
	// reason to kill a healthy worker and discard its heap.
	if len(req.Code) > maxCodeBytes {
		return evalapi.Result{}, fmt.Errorf("%w: code is %d bytes, the limit is %d", evalapi.ErrLimit, len(req.Code), maxCodeBytes)
	}
	// A request id is single-use for the kernel's lifetime, so a retry cannot
	// silently re-execute a cell whose result was trimmed from the history.
	if err := s.admitRequestID(req.ID); err != nil {
		return evalapi.Result{}, err
	}
	budget := evalapi.MergeBudget(s.budget, req.Budget).WithDefaults(m.defaultBudget)
	// CPU, memory and file-size bounds belong to the kernel's process, which is
	// already running: a lower per-request bound cannot be applied without
	// restarting it (and losing the heap). Refuse rather than silently weaken.
	if req.Budget.CPUSeconds > 0 && req.Budget.CPUSeconds < s.budget.CPUSeconds {
		return evalapi.Result{}, fmt.Errorf("%w: the CPU limit is fixed when the kernel starts (session %d s, request %d s)", evalapi.ErrUnsupportedCapability, s.budget.CPUSeconds, req.Budget.CPUSeconds)
	}
	if req.Budget.DiskBytes > 0 && req.Budget.DiskBytes < s.budget.DiskBytes {
		return evalapi.Result{}, fmt.Errorf("%w: the file-size limit is fixed when the kernel starts (session %d bytes, request %d bytes)", evalapi.ErrUnsupportedCapability, s.budget.DiskBytes, req.Budget.DiskBytes)
	}
	if req.Budget.MemoryBytes > 0 && req.Budget.MemoryBytes < s.budget.MemoryBytes {
		return evalapi.Result{}, fmt.Errorf("%w: the memory bound is fixed when the kernel starts (session %d bytes, request %d bytes)", evalapi.ErrUnsupportedCapability, s.budget.MemoryBytes, req.Budget.MemoryBytes)
	}
	if depth := evalapi.CallDepth(ctx); budget.MaxDepth > 0 && depth > budget.MaxDepth {
		return evalapi.Result{}, fmt.Errorf("%w: callback recursion depth %d exceeds the limit of %d", evalapi.ErrDenied, depth, budget.MaxDepth)
	}

	_, grantExpiry, _ := s.roleForExpiry(agent)
	deadline := admissionDeadline(req, budget, ctx, s.leaseUntil, grantExpiry)
	if !deadline.After(time.Now()) {
		return evalapi.Result{}, fmt.Errorf("%w: deadline has already passed", evalapi.ErrExpired)
	}

	// Waiting for the kernel honors the same deadline: a queued cell cannot
	// outlive its own budget while another cell runs.
	waitCtx, cancelWait := context.WithDeadline(ctx, deadline)
	defer cancelWait()
	select {
	case s.execSem <- struct{}{}:
	case <-waitCtx.Done():
		if ctx.Err() != nil {
			return evalapi.Result{}, ctx.Err()
		}
		return evalapi.Result{}, fmt.Errorf("%w: waited past the deadline for the kernel", evalapi.ErrExpired)
	}
	defer func() { <-s.execSem }()

	// Re-check at actual admission: while this cell waited, its grant may have
	// been revoked or expired, the lease may have ended, or the kernel closed.
	if !s.canExecute(agent) {
		return evalapi.Result{}, fmt.Errorf("%w: agent %q lost access to session %s while waiting", evalapi.ErrDenied, agent, s.id)
	}
	if err := s.checkLease(); err != nil {
		return evalapi.Result{}, err
	}
	_, grantExpiry, _ = s.roleForExpiry(agent)
	deadline = admissionDeadline(req, budget, ctx, s.leaseUntil, grantExpiry)
	if !deadline.After(time.Now()) {
		return evalapi.Result{}, fmt.Errorf("%w: deadline has already passed", evalapi.ErrExpired)
	}
	s.mu.Lock()
	closed := s.closed
	state := s.state
	s.mu.Unlock()
	if closed {
		return evalapi.Result{}, errSessionClosed
	}
	switch state {
	case evalapi.StateResetting, evalapi.StateStopping, evalapi.StateDisposed:
		// The kernel is being replaced or stopped; a cell admitted now would run
		// on a heap the lifecycle operation is about to discard.
		return evalapi.Result{}, fmt.Errorf("%w: session %s is %s", evalapi.ErrClosed, s.id, state)
	}

	// The execution's own watch context exists before the worker starts, so a
	// revocation or grant expiry that lands during startup cancels this cell
	// instead of racing it. The control call itself is never cancelled by the
	// caller's context or by our own deadline: terminating a cell is done by
	// signalling the worker, and the worker's answer must still be readable (a
	// cancelled call would drop a perfectly good "cancelled" result and report a
	// lost kernel instead).
	watchCtx, cancelWatch := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWatch()
	if !grantExpiry.IsZero() {
		// An execution admitted under a share stops when the share ends.
		expiryTimer := time.AfterFunc(time.Until(grantExpiry), cancelWatch)
		defer expiryTimer.Stop()
	}
	ref := &executionRef{agentID: agent, cancel: cancelWatch, done: make(chan struct{})}
	s.mu.Lock()
	ref.generation = s.generation
	s.executing = true
	s.active = ref
	s.mu.Unlock()
	// The ref is cleared and closed on every return path: an interrupt or a
	// revocation waits on done to know that this execution is really over.
	defer func() {
		s.mu.Lock()
		s.executing = false
		if s.active == ref {
			s.active = nil
		}
		s.mu.Unlock()
		close(ref.done)
	}()

	w, err := s.startWorker(ctx)
	if err != nil {
		s.setState(evalapi.StateFailed, err.Error())
		return evalapi.Result{}, err
	}
	// A restart bumps the generation; the ref follows the worker that will
	// actually run the cell, so interrupt escalation and revocation compare the
	// generation of this execution and not of the kernel it replaced.
	s.mu.Lock()
	ref.generation = w.generation
	s.mu.Unlock()
	if watchCtx.Err() != nil {
		// The share ended while the worker was starting: never run the cell.
		return evalapi.Result{}, fmt.Errorf("%w: agent %q lost access to session %s before the cell started", evalapi.ErrDenied, agent, s.id)
	}
	if err := s.materializeInputs(agent, req.Inputs); err != nil {
		return evalapi.Result{}, err
	}

	call := newActiveCall(watchCtx, s, caller, req, budget, deadline)
	call.bindExecution(agent, w)
	var tools []protocol.ToolSpec
	if caller != nil && s.manifest.Supports(evalapi.CapToolCallback) {
		loaded, loadErr := call.loadTools(ctx)
		if loadErr != nil {
			return evalapi.Result{}, loadErr
		}
		tools = loaded
	} else {
		call.caller = nil
	}

	cellID := strings.TrimSpace(req.CellID)
	if cellID == "" {
		cellID = req.ID
	}
	wire := wireExecute{
		RequestID:      req.ID,
		CellID:         cellID,
		Code:           req.Code,
		DeadlineUnixMS: deadline.UnixMilli(),
		Budget:         budget,
		Tools:          tools,
		RunID:          req.RunID,
		TrialID:        req.TrialID,
	}
	s.setState(evalapi.StateBusy, "")
	started := time.Now().UTC()
	w.active.Store(call)
	m.emit(evalapi.Event{Type: evalapi.EventExecutionStarted, SessionID: s.id, OwnerID: s.ownerID, AgentID: agent,
		RequestID: req.ID, CellID: cellID, RunID: req.RunID, TrialID: req.TrialID,
		Generation: w.generation, Data: map[string]any{"language": s.manifest.Language}})

	execCtx := context.WithoutCancel(watchCtx)
	resCh := make(chan execOutcome, 1)
	go func() {
		res, execErr := w.execute(execCtx, wire)
		resCh <- execOutcome{result: res, err: execErr}
	}()

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	var outcome execOutcome
	select {
	case outcome = <-resCh:
	case <-timer.C:
		// A timeout is not a cooperative request: the caller's budget is spent, so
		// the whole owned tree is terminated and the kernel is lost (Owner §27).
		outcome = m.terminate(w, resCh, evalapi.ResultStatusTimeout)
	case <-ctx.Done():
		outcome = m.terminate(w, resCh, evalapi.ResultStatusCancelled)
	case <-watchCtx.Done():
		// Revoked, or the grant expired while the cell ran.
		outcome = m.terminate(w, resCh, evalapi.ResultStatusCancelled)
	}
	w.active.Store(nil)
	// Any tool callback still in flight belongs to an execution that is over:
	// cancel it now instead of letting it run to its own timeout.
	cancelWatch()

	// An interrupt the worker did not survive is reported as cancelled, not as a
	// spontaneous crash: the host asked for the stop.
	if outcome.err != nil && w.interrupting.Load() && outcome.reason == "" {
		outcome.reason = evalapi.ResultStatusCancelled
		outcome.lost = true
	}
	result, outputErr := s.buildResult(req, cellID, outcome, call, started, budget)
	if outputErr != nil {
		// Declared artifacts could not be adopted (a special file, a count or
		// size limit, a vanished entry). The cell's side effects are unknown, so
		// the result says so instead of masking the error as a mere event.
		result.Status = evalapi.ResultStatusFailed
		result.StateStatus = evalapi.StateStatusUnknown
		result.Error = firstNonEmpty(boundText(outputErr.Error(), 4<<10), result.Error)
		result.PartialEffects = true
	}
	if w.proc != nil {
		// A crashed worker never returned a result, so its exit code comes from the
		// process itself. The control channel closes as soon as the process exits,
		// which can be before cmd.Wait records the status, so wait for it briefly.
		if outcome.err != nil {
			waitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = w.proc.wait(waitCtx)
			cancel()
			result.ExitCode = w.proc.exitCode()
		}
		result.Usage.CPUMs = w.proc.cpuMillis()
		result.Usage.MemoryPeakBytes = w.proc.memoryPeak()
		if reason := w.proc.limitReason(); reason != "" && result.Status != evalapi.ResultStatusCompleted {
			// The worker was stopped by its resource bound, not by a bug.
			result.Error = firstNonEmpty(reason+"; "+result.Error, reason)
			result.StateStatus = evalapi.StateStatusLost
		}
	}
	s.addResult(result)
	if result.StateStatus == evalapi.StateStatusLost {
		_ = w.kill()
		residual := w.shutdown(context.Background())
		_ = m.reportResidual(s, residual, "worker termination")
		s.mu.Lock()
		if s.worker == w {
			s.worker = nil
		}
		// The heap is gone: the generation advances even before the next worker
		// starts, so a client that polls right after a timeout sees the loss
		// instead of the dead generation.
		s.generation++
		s.state = evalapi.StateFailed
		s.lastError = firstNonEmpty(result.Error, "worker lost; kernel state discarded")
		s.mu.Unlock()
	} else {
		s.setState(evalapi.StateIdle, "")
	}
	m.emitResultEvent(s, result)
	return result, nil
}

// terminate force-stops a cell whose budget or authority ended: the process group
// and every recorded descendant are killed, and the result reports a lost kernel.
// It is deliberately not cooperative — only an explicit Interrupt asks the worker
// to abort and keep its state.
func (m *Manager) terminate(w *worker, resCh <-chan execOutcome, reason string) execOutcome {
	_ = w.kill()
	select {
	case outcome := <-resCh:
		// A result may race the kill; the kernel is still gone.
		outcome.reason = reason
		outcome.lost = true
		return outcome
	case <-time.After(termGrace):
		return execOutcome{err: errNoWorker, reason: reason, lost: true}
	}
}

// admissionDeadline is the earliest of the wall budget, the request deadline, the
// caller's context, the session lease and the caller's grant expiry.
func admissionDeadline(req evalapi.Request, budget evalapi.Budget, ctx context.Context, lease, grantExpiry time.Time) time.Time {
	deadline := time.Now().Add(time.Duration(budget.WallSeconds) * time.Second)
	if !req.Deadline.IsZero() && req.Deadline.Before(deadline) {
		deadline = req.Deadline
	}
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if !lease.IsZero() && lease.Before(deadline) {
		deadline = lease
	}
	if !grantExpiry.IsZero() && grantExpiry.Before(deadline) {
		deadline = grantExpiry
	}
	return deadline
}

type execOutcome struct {
	result wireResult
	err    error
	reason string
	lost   bool
}

func (s *session) buildResult(req evalapi.Request, cellID string, outcome execOutcome, call *activeCall, started time.Time, budget evalapi.Budget) (evalapi.Result, error) {
	result := evalapi.Result{
		RequestID:   req.ID,
		SessionID:   s.id,
		CellID:      cellID,
		RuntimeID:   s.manifest.ID,
		Language:    s.manifest.Language,
		StartedAt:   started,
		CompletedAt: time.Now().UTC(),
		Generation:  s.generationValue(),
	}
	truncated := false
	if outcome.err != nil {
		result.Status = evalapi.ResultStatusCrashed
		if outcome.reason != "" {
			result.Status = outcome.reason
		}
		result.Error = outcome.err.Error()
		result.StateStatus = evalapi.StateStatusLost
		result.PartialEffects = true
	} else {
		wr := outcome.result
		result.Status = wr.Status
		if result.Status == "" {
			result.Status = evalapi.ResultStatusFailed
		}
		result.Stdout = wr.Stdout
		result.Stderr = wr.Stderr
		result.Error = wr.Error
		result.Traceback = wr.Traceback
		result.ExitCode = wr.ExitCode
		result.StateStatus = wr.StateStatus
		truncated = wr.OutputTruncated
		if result.StateStatus == "" {
			result.StateStatus = evalapi.StateStatusOK
		}
		if outcome.reason != "" {
			result.Status = outcome.reason
		}
		if outcome.lost {
			result.StateStatus = evalapi.StateStatusLost
		}
		if result.Status != evalapi.ResultStatusCompleted {
			result.PartialEffects = true
		}
	}
	// Output the cell wrote to fd 1/2 directly is attributed to this execution.
	if raw, rawTruncated := snapshotText(call.out); raw != "" {
		if result.Stdout != "" {
			result.Stdout += "\n"
		}
		result.Stdout += raw
		truncated = truncated || rawTruncated
	}
	if raw, rawTruncated := snapshotText(call.err); raw != "" {
		if result.Stderr != "" {
			result.Stderr += "\n"
		}
		result.Stderr += raw
		truncated = truncated || rawTruncated
	}
	limit := budget.OutputBytes
	if bounded := boundText(result.Stdout, limit); bounded != result.Stdout {
		result.Stdout = bounded
		truncated = true
	}
	if bounded := boundText(result.Stderr, limit); bounded != result.Stderr {
		result.Stderr = bounded
		truncated = true
	}
	result.Traceback = boundText(result.Traceback, maxToolContent)
	result.Error = boundText(result.Error, 8<<10)
	displays, artifacts, outputErr := s.convertOutputs(cellID, call.executor, outcome.result)
	result.Displays, result.Artifacts = displays, artifacts
	result.DurationMS = time.Since(started).Milliseconds()
	result.Usage = evalapi.Usage{
		WallMS:          result.DurationMS,
		OutputBytes:     int64(len(result.Stdout) + len(result.Stderr)),
		OutputTruncated: truncated,
		Callbacks:       int(call.calls.Load()),
		Depth:           evalapi.CallDepth(call.ctx),
	}
	for _, artifact := range result.Artifacts {
		result.Usage.ArtifactBytes += artifact.Size
	}
	if result.Status == evalapi.ResultStatusCompleted {
		result.PartialEffects = false
	}
	return result, outputErr
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (m *Manager) emitResultEvent(s *session, result evalapi.Result) {
	eventType := evalapi.EventExecutionFailed
	switch result.Status {
	case evalapi.ResultStatusCompleted:
		eventType = evalapi.EventExecutionCompleted
	case evalapi.ResultStatusTimeout:
		eventType = evalapi.EventTimeout
	case evalapi.ResultStatusCancelled:
		eventType = evalapi.EventExecutionCancelled
	case evalapi.ResultStatusCrashed:
		eventType = evalapi.EventWorkerCrashed
	}
	m.emit(evalapi.Event{
		Type: eventType, SessionID: s.id, OwnerID: s.ownerID, AgentID: s.agentID,
		RequestID: result.RequestID, CellID: result.CellID, Generation: result.Generation,
		Data: map[string]any{"status": result.Status, "stateStatus": result.StateStatus,
			"durationMs": result.DurationMS, "outputBytes": result.Usage.OutputBytes},
	})
}

// Interrupt asks a running cell to stop cooperatively. When the worker
// acknowledges, its kernel and variables survive; when it does not answer within
// the grace period the process group is terminated and the kernel is lost.
//
// The escalation is tied to the execution it was requested for: it fires while
// that exact execution (same agent, same generation, same worker) is still
// running, and it waits for the kill to finish so an interrupt that reports
// success never leaves an owned pid behind.
func (m *Manager) Interrupt(ctx context.Context, sessionID, agentID string) error {
	if err := m.ensureOpen(); err != nil {
		return err
	}
	s, err := m.session(sessionID)
	if err != nil {
		return err
	}
	if !s.canInterrupt(agentID) {
		return fmt.Errorf("%w: agent %q may not interrupt session %s", evalapi.ErrDenied, agentID, sessionID)
	}
	s.mu.Lock()
	w := s.worker
	ref := s.active
	s.mu.Unlock()
	if w == nil || ref == nil {
		// Nothing is running: an interrupt is a no-op, not an error.
		return nil
	}
	s.setState(evalapi.StateCancelling, "")
	w.interrupt("interrupt")
	// Give the cell its chance to abort cooperatively and keep its kernel.
	timer := time.NewTimer(interruptGrace)
	defer timer.Stop()
	select {
	case <-ref.done:
		return nil
	case <-timer.C:
	}
	if !s.isActive(ref) {
		return nil
	}
	// The cell ignored the request. Escalate for this execution only, and do not
	// return before the owned tree and the cell's own goroutine are gone.
	residual := w.kill()
	select {
	case <-ref.done:
	case <-time.After(interruptGrace + cleanupWindow):
		return fmt.Errorf("%w: the interrupted execution did not stop after it was killed", evalapi.ErrWorker)
	}
	return m.reportResidual(s, residual, "interrupt")
}

// isActive reports whether ref is still the kernel's running execution in the
// generation it was admitted in.
func (s *session) isActive(ref *executionRef) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active == ref && s.generation == ref.generation && !s.closed
}

// Reset discards the kernel state: the worker is stopped and a new generation is
// started. Variables, modules and in-memory objects do not survive.
func (m *Manager) Reset(ctx context.Context, sessionID, agentID string) error {
	if err := m.ensureOpen(); err != nil {
		return err
	}
	s, err := m.session(sessionID)
	if err != nil {
		return err
	}
	if !s.canControl(agentID) {
		return fmt.Errorf("%w: only the kernel owner may reset session %s", evalapi.ErrDenied, sessionID)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errSessionClosed
	}
	w := s.worker
	s.state = evalapi.StateResetting
	s.mu.Unlock()
	if w != nil {
		w.interrupt("reset")
		_ = w.kill()
		residual := w.shutdown(ctx)
		if err := m.reportResidual(s, residual, "reset"); err != nil {
			s.setState(evalapi.StateFailed, err.Error())
			return err
		}
	}
	// Wait for the running cell to observe the dead worker and release the
	// kernel. RESETTING makes a cell that wins this race fail admission instead
	// of starting a worker the reset would have to throw away.
	select {
	case s.execSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.execSem }()

	// Stop any worker this reset did not see (a cell that started one before the
	// state change): never set s.worker to nil while a process is still owned.
	s.mu.Lock()
	w = s.worker
	s.worker = nil
	s.lastError = ""
	s.mu.Unlock()
	if w != nil {
		w.interrupt("reset")
		_ = w.kill()
		residual := w.shutdown(ctx)
		if err := m.reportResidual(s, residual, "reset"); err != nil {
			s.setState(evalapi.StateFailed, err.Error())
			return err
		}
	}
	if err := s.resetWorkspace(); err != nil {
		s.setState(evalapi.StateFailed, err.Error())
		return err
	}
	if _, err := s.startWorker(ctx); err != nil {
		s.setState(evalapi.StateFailed, err.Error())
		return err
	}
	m.emit(evalapi.Event{Type: evalapi.EventSessionReset, SessionID: s.id, OwnerID: s.ownerID, AgentID: agentID,
		Generation: s.generationValue()})
	return nil
}

// Close stops the kernel and keeps its artifacts readable. Only the owner may
// close it; an executor cannot destroy the owner's heap.
func (m *Manager) Close(ctx context.Context, sessionID, agentID string) error {
	if err := m.ensureOpen(); err != nil {
		return err
	}
	s, err := m.session(sessionID)
	if err != nil {
		return err
	}
	if !s.canControl(agentID) {
		return fmt.Errorf("%w: only the kernel owner may close session %s", evalapi.ErrDenied, sessionID)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.state = evalapi.StateStopping
	w := s.worker
	s.mu.Unlock()
	var cleanupErr error
	if w != nil {
		w.interrupt("close")
		cleanupErr = m.reportResidual(s, w.shutdown(ctx), "close")
	}
	// The running cell observes the closed session and returns; a cell that won
	// the semaphore race started its worker before `closed` was set, so the
	// worker is stopped again below instead of being orphaned.
	select {
	case s.execSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.execSem }()
	s.mu.Lock()
	w = s.worker
	s.worker = nil
	if cleanupErr != nil {
		s.state = evalapi.StateFailed
		s.lastError = cleanupErr.Error()
	} else {
		s.state = evalapi.StateStopped
	}
	s.mu.Unlock()
	if w != nil {
		w.interrupt("close")
		if err := m.reportResidual(s, w.shutdown(ctx), "close"); err != nil && cleanupErr == nil {
			cleanupErr = err
			s.mu.Lock()
			s.state = evalapi.StateFailed
			s.lastError = err.Error()
			s.mu.Unlock()
		}
	}
	// Artifact bytes live under the control root, which stays readable after a
	// close; only the worker-owned data root is released here.
	m.emit(evalapi.Event{Type: evalapi.EventSessionClosed, SessionID: s.id, OwnerID: s.ownerID, AgentID: agentID,
		Generation: s.generationValue()})
	return cleanupErr
}

// Grant adds an explicit, expiring share of one kernel. Ownership is immutable,
// and a grant can never name a different workspace or a broader permission than
// the kernel itself.
func (m *Manager) Grant(ctx context.Context, sessionID, ownerID string, grant evalapi.Grant) error {
	if err := m.ensureOpen(); err != nil {
		return err
	}
	s, err := m.session(sessionID)
	if err != nil {
		return err
	}
	if ownerID != s.ownerID {
		return fmt.Errorf("%w: only the immutable owner may grant access to session %s", evalapi.ErrDenied, sessionID)
	}
	if !evalapi.ValidRole(grant.Role) {
		return fmt.Errorf("%w: unknown role %q", evalapi.ErrInvalid, grant.Role)
	}
	if grant.Role == evalapi.RoleOwner {
		return fmt.Errorf("%w: ownership cannot be transferred or duplicated", evalapi.ErrDenied)
	}
	agentID := strings.TrimSpace(grant.AgentID)
	if agentID == "" || agentID == s.ownerID {
		return fmt.Errorf("%w: grant needs a distinct agent id", evalapi.ErrInvalid)
	}
	if grant.Workspace != "" && grant.Workspace != s.kernel {
		return fmt.Errorf("%w: a grant cannot widen the kernel workspace", evalapi.ErrDenied)
	}
	if grant.Permission != "" && grant.Permission != s.permission {
		return fmt.Errorf("%w: a grant cannot widen the kernel permission", evalapi.ErrDenied)
	}
	grant.AgentID = agentID
	grant.Workspace = s.kernel
	grant.Permission = s.permission
	s.mu.Lock()
	s.grants[agentID] = grant
	s.mu.Unlock()
	m.emit(evalapi.Event{Type: evalapi.EventGrant, SessionID: s.id, OwnerID: s.ownerID, AgentID: agentID,
		Generation: s.generationValue(), Data: map[string]any{"role": grant.Role, "expiresAt": grant.ExpiresAt}})
	return nil
}

// Revoke removes an actor's read-only share immediately.
func (m *Manager) Revoke(ctx context.Context, sessionID, ownerID, agentID string) error {
	if err := m.ensureOpen(); err != nil {
		return err
	}
	s, err := m.session(sessionID)
	if err != nil {
		return err
	}
	if ownerID != s.ownerID {
		return fmt.Errorf("%w: only the immutable owner may revoke access to session %s", evalapi.ErrDenied, sessionID)
	}
	s.mu.Lock()
	_, existed := s.grants[agentID]
	delete(s.grants, agentID)
	s.mu.Unlock()
	// A revocation cancels the grantee's running cell, not only future ones.
	s.cancelActive(agentID)
	if !existed {
		return fmt.Errorf("%w: no grant for agent %q on session %s", evalapi.ErrNotFound, agentID, sessionID)
	}
	m.emit(evalapi.Event{Type: evalapi.EventRevoke, SessionID: s.id, OwnerID: s.ownerID, AgentID: agentID,
		Generation: s.generationValue()})
	return nil
}

// Results returns the bounded execution history the agent may read.
func (m *Manager) Results(ctx context.Context, sessionID, agentID string) ([]evalapi.Result, error) {
	if err := m.ensureOpen(); err != nil {
		return nil, err
	}
	s, err := m.session(sessionID)
	if err != nil {
		return nil, err
	}
	if !s.canRead(agentID) {
		return nil, fmt.Errorf("%w: agent %q has no access to session %s", evalapi.ErrDenied, agentID, sessionID)
	}
	return s.snapshotResults(), nil
}

// Artifact returns one artifact and its bytes after digest verification.
func (m *Manager) Artifact(ctx context.Context, sessionID, agentID, artifactID string) (evalapi.Artifact, []byte, error) {
	if err := m.ensureOpen(); err != nil {
		return evalapi.Artifact{}, nil, err
	}
	s, err := m.session(sessionID)
	if err != nil {
		return evalapi.Artifact{}, nil, err
	}
	if !s.canRead(agentID) {
		return evalapi.Artifact{}, nil, fmt.Errorf("%w: agent %q has no access to session %s", evalapi.ErrDenied, agentID, sessionID)
	}
	return s.readArtifact(artifactID)
}

// PutArtifact imports host-owned bytes into a kernel: the artifact is indexed
// with provenance and also written under the kernel workspace so the runtime can
// read it by name. This is how an authorized cross-language transfer lands.
func (m *Manager) PutArtifact(ctx context.Context, sessionID, agentID, name, mimeType string, data []byte) (evalapi.Artifact, error) {
	if err := m.ensureOpen(); err != nil {
		return evalapi.Artifact{}, err
	}
	s, err := m.session(sessionID)
	if err != nil {
		return evalapi.Artifact{}, err
	}
	if !s.canExecute(agentID) {
		return evalapi.Artifact{}, fmt.Errorf("%w: agent %q may not add artifacts to session %s", evalapi.ErrDenied, agentID, sessionID)
	}
	if err := s.checkLease(); err != nil {
		return evalapi.Artifact{}, err
	}
	return s.importArtifact(ctx, agentID, name, mimeType, data)
}

// CloseOwner stops every kernel the owner holds. It is the normal session-exit
// path; artifacts stay readable until the host purges the owner.
func (m *Manager) CloseOwner(ctx context.Context, ownerID string) error {
	if err := m.ensureOpen(); err != nil {
		return err
	}
	sessions := m.ownerSessions(ownerID)
	var firstErr error
	for _, s := range sessions {
		if err := m.Close(ctx, s.id, ownerID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ForgetOwner stops the owner's kernels and deletes their private directories and
// metadata. It is the privacy purge path: no artifact, workspace file or worker
// output survives it.
func (m *Manager) ForgetOwner(ctx context.Context, ownerID string) error {
	if err := m.ensureOpen(); err != nil {
		return err
	}
	sessions := m.ownerSessions(ownerID)
	var firstErr error
	for _, s := range sessions {
		if err := m.Close(ctx, s.id, ownerID); err != nil {
			firstErr = err
			continue
		}
		if err := s.removeFiles(); err != nil {
			firstErr = err
			continue
		}
		m.mu.Lock()
		delete(m.sessions, s.id)
		m.mu.Unlock()
	}
	m.mu.Lock()
	delete(m.byOwner, ownerID)
	m.mu.Unlock()
	return firstErr
}

func (m *Manager) ownerSessions(ownerID string) []*session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := m.byOwner[ownerID]
	out := make([]*session, 0, len(ids))
	for id := range ids {
		if s, ok := m.sessions[id]; ok {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].createdAt.Before(out[j].createdAt) })
	return out
}

// Dispose stops every worker and releases the manager.
func (m *Manager) Dispose(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	sessions := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.sessions = make(map[string]*session)
	m.byOwner = make(map[string]map[string]struct{})
	m.mu.Unlock()

	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	record := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}
	for _, s := range sessions {
		s.mu.Lock()
		s.closed = true
		s.state = evalapi.StateDisposed
		w := s.worker
		s.worker = nil
		s.mu.Unlock()
		wg.Add(1)
		go func(w *worker, s *session) {
			defer wg.Done()
			if w != nil {
				w.interrupt("dispose")
				record(m.reportResidual(s, w.shutdown(ctx), "dispose"))
			}
			// The worker is fully shut down; now wait for the cell goroutine to
			// release the kernel, so no reader is left when the session's file
			// descriptors are closed.
			select {
			case s.execSem <- struct{}{}:
				<-s.execSem
			case <-time.After(termGrace + cleanupWindow):
				record(fmt.Errorf("%w: session %s did not quiesce before dispose", evalapi.ErrWorker, s.id))
			}
			// Only now are the session's file descriptors released, so nothing is
			// still writing through a handle the manager closes.
			record(s.closeFileHandles())
		}(w, s)
	}
	wg.Wait()
	if m.cgroups != nil {
		record(m.cgroups.close())
	}
	return firstErr
}

// startWorker starts a new generation when the current one is missing or dead.
// It is called with the kernel's execSem held (Execute, Reset) or on a brand new
// session (Create), so two cells can never start two workers.
//
// A worker that started while the session was being closed is shut down again
// instead of being installed: Close/Dispose never return while a worker they did
// not observe is alive.
func (s *session) startWorker(ctx context.Context) (*worker, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errSessionClosed
	}
	if s.worker != nil && s.worker.alive() {
		w := s.worker
		s.mu.Unlock()
		return w, nil
	}
	generation := s.generation + 1
	s.mu.Unlock()

	w := &worker{s: s, m: s.m, generation: generation}
	if err := w.start(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = w.shutdown(context.Background())
		return nil, errSessionClosed
	}
	s.worker = w
	s.generation = generation
	s.state = evalapi.StateReady
	s.lastError = ""
	// Callback ids belong to one worker process: a new generation starts a fresh
	// numbering, so the replay guard is reset with it (it stays bounded).
	s.callbackIDs = make(map[string]struct{})
	s.mu.Unlock()
	s.m.emit(evalapi.Event{Type: evalapi.EventWorkerStarted, SessionID: s.id, OwnerID: s.ownerID, AgentID: s.agentID,
		Generation: generation, Data: map[string]any{"pid": w.proc.pid, "runtimeVersion": w.runtime}})
	return w, nil
}

// onWorkerExit records an unplanned worker death between cells. Without this the
// session would keep reporting IDLE and alive after its process died, and the
// next cell would run against a heap that no longer exists without telling the
// agent that state was lost.
func (s *session) onWorkerExit(w *worker) {
	s.mu.Lock()
	if s.closed || s.worker != w || s.executing || s.active != nil {
		s.mu.Unlock()
		return
	}
	switch s.state {
	case evalapi.StateIdle, evalapi.StateReady:
	default:
		// Resetting, stopping, disposing: the exit is ours, not a loss.
		s.mu.Unlock()
		return
	}
	s.worker = nil
	s.state = evalapi.StateFailed
	exitCode := -1
	if w.proc != nil {
		exitCode = w.proc.exitCode()
	}
	s.lastError = fmt.Sprintf("worker exited unexpectedly with code %d; kernel state discarded", exitCode)
	generation := s.generation
	s.mu.Unlock()
	// Release what the dead worker still holds (its cgroup leaf, pipes).
	_ = w.shutdown(context.Background())
	s.m.emit(evalapi.Event{Type: evalapi.EventWorkerCrashed, SessionID: s.id, OwnerID: s.ownerID, AgentID: s.agentID,
		Generation: generation,
		Data:       map[string]any{"stateStatus": evalapi.StateStatusLost, "exitCode": exitCode, "unplanned": true}})
}

// materializeInputs copies authorized artifacts into the kernel workspace before
// a cell runs. The source session's read grant is checked for the executing agent.
func (s *session) materializeInputs(agentID string, inputs []evalapi.Input) error {
	var total int64
	for _, input := range inputs {
		source, err := s.m.session(input.SessionID)
		if err != nil {
			return err
		}
		if !source.canRead(agentID) {
			return fmt.Errorf("%w: agent %q may not read artifacts of session %s", evalapi.ErrDenied, agentID, input.SessionID)
		}
		artifact, data, err := source.readArtifact(input.ArtifactID)
		if err != nil {
			return err
		}
		total += int64(len(data))
		if total > s.budget.ArtifactBytes {
			return fmt.Errorf("%w: inputs exceed the %d byte artifact budget", evalapi.ErrLimit, s.budget.ArtifactBytes)
		}
		name := input.Name
		if name == "" {
			name = artifact.Name
		}
		if err := s.writeInput(name, data); err != nil {
			return err
		}
	}
	return nil
}

// convertOutputs adopts declared artifacts and turns large inline displays into
// artifact references. An adoption failure is returned to the caller: a cell
// whose declared output could not be taken over is not reported as a clean
// success.
func (s *session) convertOutputs(cellID, agentID string, outcome wireResult) ([]evalapi.Display, []evalapi.Artifact, error) {
	// The declared list is bounded before it is walked: a worker cannot make the
	// host adopt an unbounded number of files by naming them.
	declared := outcome.Artifacts
	if len(declared) > maxCellArtifacts {
		s.m.emit(evalapi.Event{Type: evalapi.EventResourceLimit, SessionID: s.id, OwnerID: s.ownerID,
			AgentID: agentID, Generation: s.generationValue(),
			Data: map[string]any{"reason": fmt.Sprintf("cell declared %d artifacts, the limit is %d", len(declared), maxCellArtifacts)}})
		return nil, nil, fmt.Errorf("%w: cell declared %d artifacts, the limit is %d", evalapi.ErrLimit, len(declared), maxCellArtifacts)
	}
	artifacts, err := s.adoptPendingArtifacts(cellID, declared)
	if err != nil {
		s.m.emit(evalapi.Event{Type: evalapi.EventResourceLimit, SessionID: s.id, OwnerID: s.ownerID,
			AgentID: agentID, Generation: s.generationValue(), Data: map[string]string{"reason": err.Error()}})
		return nil, artifacts, err
	}
	displays := make([]evalapi.Display, 0, len(outcome.Displays))
	for i, item := range outcome.Displays {
		if i >= maxDisplays {
			break
		}
		display := evalapi.Display{MIME: item.MIME, Encoding: item.Encoding, Text: item.Text, Size: item.Size}
		if item.Size > inlineDisplayLimit && item.Encoding == "base64" {
			if data, decodeErr := base64.StdEncoding.DecodeString(item.Text); decodeErr == nil {
				if artifact, storeErr := s.storeArtifact(cellID, "display-"+newID()+extensionFor(item.MIME), item.MIME, data); storeErr == nil {
					display.ArtifactID = artifact.ID
					display.Text = ""
					display.Size = artifact.Size
					artifacts = append(artifacts, artifact)
				}
			}
		}
		displays = append(displays, display)
	}
	return displays, artifacts, nil
}

func extensionFor(mimeType string) string {
	switch mimeType {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/svg+xml":
		return ".svg"
	case "text/html":
		return ".html"
	case "application/json":
		return ".json"
	}
	return ".bin"
}

// RevokeAgent drops every share held by agentID. It is the exit path for a
// grantee: a later agent that reuses the identity gets no implicit access.
func (m *Manager) RevokeAgent(ctx context.Context, agentID string) error {
	if err := m.ensureOpen(); err != nil {
		return err
	}
	m.mu.RLock()
	all := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.RUnlock()
	for _, s := range all {
		s.mu.Lock()
		delete(s.grants, agentID)
		s.mu.Unlock()
		s.cancelActive(agentID)
	}
	return nil
}
