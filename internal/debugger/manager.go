package debugger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edynasty/LumenCortex/evalruntime"
	"github.com/edynasty/LumenCortex/internal/debugger/dap"
	"github.com/edynasty/LumenCortex/internal/diag"
	"github.com/edynasty/LumenCortex/internal/mcp"
	"github.com/edynasty/LumenCortex/internal/shell"
)

var (
	ErrDenied   = errors.New("debug operation denied")
	ErrStale    = errors.New("debug control epoch is stale")
	ErrNotFound = errors.New("debug session not found")
	ErrState    = errors.New("debug operation unavailable in current state")
)

type Adapter struct {
	ID        string   `json:"id"`
	Language  string   `json:"language"`
	Command   string   `json:"command"`
	Args      []string `json:"args,omitempty"`
	ReadPaths []string `json:"readPaths,omitempty"`
	// Launch is host-owned adapter-specific configuration. User tool input cannot override it.
	Launch         map[string]any `json:"launch,omitempty"`
	Loopback       bool           `json:"loopback,omitempty"`
	Transport      string         `json:"transport,omitempty"`
	Attach         map[string]any `json:"attach,omitempty"`
	AttachPIDField string         `json:"attachPidField,omitempty"`
	// MCPServer reuses an explicitly configured, dedicated IntelliJ MCP endpoint.
	MCPServer string `json:"mcpServer,omitempty"`
	// Discovery status, reported by Adapters(); these never affect Start.
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Preset    bool   `json:"preset,omitempty"`
}
type Options struct {
	Adapters    []Adapter
	Sandbox     evalruntime.SandboxOptions
	MaxSessions int
	Lifetime    time.Duration
	MCP         *mcp.Registry
	// Discover probes the built-in adapter presets and registers the ones this
	// host can actually run. It never installs an adapter.
	Discover bool
	OnEvent  func(sessionID, event string, data map[string]any)
}
type StartOptions struct {
	SessionID        string   `json:"sessionId"`
	RunID            string   `json:"runId,omitempty"`
	AdapterID        string   `json:"adapterId"`
	Program          string   `json:"program"`
	Args             []string `json:"args,omitempty"`
	Workspace        string   `json:"-"`
	Actor            string   `json:"-"`
	AllowEvaluate    bool     `json:"-"`
	PrepareForAttach bool     `json:"prepareForAttach,omitempty"`
	AllowAttach      bool     `json:"-"`
}
type Breakpoint struct {
	Line         int    `json:"line"`
	Condition    string `json:"condition,omitempty"`
	HitCondition string `json:"hitCondition,omitempty"`
}
type Control struct {
	SessionID string `json:"sessionId"`
	DebugID   string `json:"debugId"`
	Epoch     uint64 `json:"epoch"`
	Actor     string `json:"-"`
	Action    string `json:"action"`
	ThreadID  int    `json:"threadId,omitempty"`
}
type Snapshot struct {
	ID            string    `json:"id"`
	SessionID     string    `json:"sessionId"`
	RunID         string    `json:"runId,omitempty"`
	AdapterID     string    `json:"adapterId"`
	State         string    `json:"state"`
	Owner         string    `json:"owner"`
	Epoch         uint64    `json:"epoch"`
	ThreadID      int       `json:"threadId,omitempty"`
	StartedAt     time.Time `json:"startedAt"`
	Deadline      time.Time `json:"deadline"`
	DroppedOutput uint64    `json:"droppedOutput"`
	Error         string    `json:"error,omitempty"`
	TargetPID     int       `json:"targetPid,omitempty"`
	AdapterPID    int       `json:"adapterPid,omitempty"`
	StopEpoch     uint64    `json:"stopEpoch,omitempty"`
	StopReason    string    `json:"stopReason,omitempty"`
	// ExceptionFilters are the adapter's exception breakpoint filters.
	ExceptionFilters []string `json:"exceptionFilters,omitempty"`
}
type session struct {
	snapshot       Snapshot
	opts           StartOptions
	adapter        Adapter
	process        *dap.Process
	cancel         context.CancelFunc
	dir            string
	initialized    chan struct{}
	initOnce       sync.Once
	op             chan struct{}
	done           chan struct{}
	closeOnce      sync.Once
	closeErr       error
	client         *dap.Client
	children       []*dap.Client
	breakpoints    map[string][]Breakpoint
	socket         string
	life           context.Context
	ideSession     string
	ideBreakpoints map[string][]string
	ideEntry       int
	ideThreads     map[int]string
	ideUncertain   bool
	// Protected by Manager.mu, as are snapshot and the manager's registry.
	capabilities map[string]bool
	// stop tracking: stopEpoch increments on every stopped/continued; refEpoch
	// maps each reference the Manager handed out to the epoch that minted it.
	stopEpoch        uint64
	stopReady        chan struct{}
	lastStop         *StopInfo
	refEpoch         map[int]uint64
	exceptionFilters []string
	closing          bool
}
type Manager struct {
	mu       sync.Mutex
	opts     Options
	adapters map[string]Adapter
	presets  []Adapter
	prober   PresetProber
	sessions map[string]*session
	closed   bool
}

func New(opts Options) (*Manager, error) {
	if opts.MaxSessions == 0 {
		opts.MaxSessions = 4
	}
	if opts.MaxSessions < 1 || opts.MaxSessions > 16 {
		return nil, errors.New("debug session limit must be 1..16")
	}
	if opts.Lifetime == 0 {
		opts.Lifetime = 15 * time.Minute
	}
	if opts.Lifetime < time.Second || opts.Lifetime > time.Hour {
		return nil, errors.New("debug lifetime must be 1s..1h")
	}
	m := &Manager{opts: opts, adapters: make(map[string]Adapter), sessions: make(map[string]*session), prober: DefaultProber()}
	for _, a := range opts.Adapters {
		if _, ok := m.adapters[a.ID]; ok {
			return nil, errors.New("duplicate debug adapter")
		}
		prepared, err := prepareAdapter(a, opts.MCP)
		if err != nil {
			return nil, err
		}
		prepared.Available = true
		m.adapters[prepared.ID] = prepared
	}
	if opts.Discover {
		m.discover(context.Background())
	}
	return m, nil
}

// prepareAdapter validates one adapter definition and clones its host-owned
// maps, so a caller cannot mutate a running session's launch configuration.
func prepareAdapter(a Adapter, registry *mcp.Registry) (Adapter, error) {
	if a.ID == "" || len(a.ID) > 128 || (a.MCPServer == "" && !filepath.IsAbs(a.Command)) {
		return Adapter{}, errors.New("adapter needs an ID and absolute host command or MCP server")
	}
	if a.Transport != "" && a.Transport != "stdio" && a.Transport != "unix" {
		return Adapter{}, errors.New("unsupported debug adapter transport")
	}
	if a.AttachPIDField != "" && a.AttachPIDField != "pid" && a.AttachPIDField != "processId" {
		return Adapter{}, errors.New("unsupported attach PID field")
	}
	if a.MCPServer != "" {
		if registry == nil || a.Command != "" || len(a.Args) != 0 || len(a.Launch) != 0 || len(a.Attach) != 0 || a.Transport != "" {
			return Adapter{}, errors.New("IntelliJ adapter needs only a configured MCP server")
		}
		return a, nil
	}
	info, err := os.Stat(a.Command)
	if err != nil {
		return Adapter{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return Adapter{}, errors.New("adapter is not executable")
	}
	a.Args = slices.Clone(a.Args)
	a.ReadPaths = slices.Clone(a.ReadPaths)
	raw, err := json.Marshal(a.Launch)
	if err != nil || len(raw) > 64<<10 {
		return Adapter{}, errors.New("invalid adapter launch options")
	}
	if err = json.Unmarshal(raw, &a.Launch); err != nil {
		return Adapter{}, err
	}
	raw, err = json.Marshal(a.Attach)
	if err != nil || len(raw) > 64<<10 {
		return Adapter{}, errors.New("invalid adapter attach options")
	}
	if err = json.Unmarshal(raw, &a.Attach); err != nil {
		return Adapter{}, err
	}
	return a, nil
}

// Adapters reports the configured adapters plus the built-in preset status.
// Host commands and launch templates stay private to the Manager.
func (m *Manager) Adapters() []Adapter {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Adapter, 0, len(m.adapters)+len(m.presets))
	for _, a := range m.adapters {
		out = append(out, Adapter{ID: a.ID, Language: a.Language, Available: true, Version: a.Version, Preset: a.Preset})
	}
	for _, status := range m.presets {
		if _, configured := m.adapters[status.ID]; configured {
			continue
		}
		out = append(out, Adapter{ID: status.ID, Language: status.Language, Available: status.Available, Version: status.Version, Reason: status.Reason, Preset: true})
	}
	slices.SortFunc(out, func(a, b Adapter) int { return strings.Compare(a.ID, b.ID) })
	return out
}
func confined(root, path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrDenied
	}
	return resolved, nil
}
func (m *Manager) Start(ctx context.Context, opts StartOptions) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if opts.SessionID == "" || (opts.Actor != "human" && opts.Actor != "agent") || len(opts.Args) > 128 {
		return Snapshot{}, ErrDenied
	}
	if opts.PrepareForAttach && !opts.AllowAttach {
		return Snapshot{}, ErrDenied
	}
	root, err := filepath.EvalSymlinks(opts.Workspace)
	if err != nil {
		return Snapshot{}, err
	}
	opts.Workspace = root
	program, err := confined(root, opts.Program)
	if err != nil {
		return Snapshot{}, err
	}
	opts.Program = program
	info, err := os.Stat(program)
	if err != nil || !info.Mode().IsRegular() {
		return Snapshot{}, errors.New("debug target must be a workspace file")
	}
	for _, arg := range opts.Args {
		if len(arg) > 8192 {
			return Snapshot{}, errors.New("debug argument too long")
		}
	}
	if runtime.GOOS == "linux" {
		kind, err := shell.Containment()
		if err != nil || kind != "cgroup" {
			return Snapshot{}, errors.New("debug requires cgroup tree containment")
		}
	}
	token := make([]byte, 16)
	if _, err = rand.Read(token); err != nil {
		return Snapshot{}, err
	}
	id := hex.EncodeToString(token)
	m.mu.Lock()
	a, ok := m.adapters[opts.AdapterID]
	if m.closed || !ok || len(m.sessions) >= m.opts.MaxSessions {
		m.mu.Unlock()
		return Snapshot{}, ErrDenied
	}
	if a.MCPServer != "" {
		for _, active := range m.sessions {
			if active.adapter.MCPServer == a.MCPServer {
				m.mu.Unlock()
				return Snapshot{}, ErrDenied
			}
		}
	}
	life, cancel := context.WithTimeout(ctx, m.opts.Lifetime)
	deadline, _ := life.Deadline()
	s := &session{snapshot: Snapshot{ID: id, SessionID: opts.SessionID, RunID: opts.RunID, AdapterID: a.ID, State: "starting", Owner: opts.Actor, Epoch: 1, StartedAt: time.Now().UTC(), Deadline: deadline}, opts: opts, adapter: a, cancel: cancel, initialized: make(chan struct{}), op: make(chan struct{}, 1), done: make(chan struct{}), stopReady: make(chan struct{}), refEpoch: make(map[int]uint64)}
	s.life = life
	// Hold the operation slot until initial setup completes; Close may cancel it but
	// cannot dispose a process which Start has not yet published.
	s.op <- struct{}{}
	m.sessions[id] = s
	m.mu.Unlock()
	held := true
	defer func() {
		if held {
			<-s.op
		}
	}()
	fail := func(err error) (Snapshot, error) {
		cancel()
		held = false
		<-s.op
		if s.process != nil {
			output := s.process.Stderr.Snapshot()
			if output.Total > 0 {
				err = fmt.Errorf("%w: adapter stderr: %s", err, diag.MaskSecrets(string(output.Head)))
			}
		}
		return Snapshot{}, errors.Join(err, m.stopSession(s))
	}
	if a.MCPServer != "" {
		if opts.PrepareForAttach || len(opts.Args) != 0 {
			return fail(ErrDenied)
		}
		if err := m.startIntelliJ(life, s); err != nil {
			return fail(err)
		}
		go func() {
			select {
			case <-life.Done():
				m.stopSession(s)
			case <-s.done:
			}
		}()
		go m.watchIntelliJ(s)
		m.emit(s, "debug.session.started", map[string]any{"status": "configuring"})
		return m.Snapshot(opts.SessionID, id)
	}
	// Keep Unix socket paths below the platform sockaddr_un limit.
	s.dir, err = os.MkdirTemp("/tmp", "lc-dap-")
	if err != nil {
		return fail(err)
	}
	s.dir, err = filepath.EvalSymlinks(s.dir)
	if err != nil {
		return fail(err)
	}
	args := a.Args
	socket := ""
	if a.Transport == "unix" {
		socket = filepath.Join(s.dir, "dap.sock")
		args = make([]string, len(a.Args))
		for i, arg := range a.Args {
			args[i] = strings.ReplaceAll(arg, "{socket}", socket)
		}
	}
	var identityRead, identityWrite *os.File
	prepare := ""
	if opts.PrepareForAttach {
		prepare = program
		identityRead, identityWrite, err = os.Pipe()
		if err != nil {
			return fail(err)
		}
		defer identityRead.Close()
		defer identityWrite.Close()
	}
	cmd, err := evalruntime.ProgramCommand(life, evalruntime.ProgramOptions{Debug: true, Sandbox: m.opts.Sandbox, Executable: a.Command, Args: args, Workspace: root, PrivateDir: s.dir, ReadPaths: a.ReadPaths, Loopback: a.Loopback, Socket: socket, PrepareTarget: prepare, TargetArgs: opts.Args, TargetIdentity: identityWrite, Env: []string{"PATH=/usr/bin:/bin", "HOME=" + root, "TMPDIR=" + filepath.Join(s.dir, "tmp")}, CPUSeconds: int(m.opts.Lifetime.Seconds()), FileBytes: 64 << 20})
	if err != nil {
		return fail(err)
	}
	var process *dap.Process
	var startErr error
	if a.Transport == "unix" {
		process, startErr = dap.StartUnix(life, cmd, socket)
	} else {
		process, startErr = dap.Start(life, cmd)
	}
	if startErr != nil {
		return fail(startErr)
	}
	m.mu.Lock()
	s.process = process
	s.client = process.Client
	s.socket = socket
	s.snapshot.AdapterPID = process.PID()
	s.breakpoints = make(map[string][]Breakpoint)
	m.mu.Unlock()
	if a.Language == "javascript" && a.Transport == "unix" {
		process.Client.SetReverseHandler(func(ctx context.Context, msg dap.Message) error {
			ctx, cancel := context.WithCancel(ctx)
			stop := context.AfterFunc(life, cancel)
			defer stop()
			defer cancel()
			return m.nodeTarget(ctx, s, msg)
		})
	}
	go m.events(s, process.Client)
	initCtx, stop := context.WithTimeout(life, 20*time.Second)
	defer stop()
	var caps map[string]any
	if err = s.process.Client.Request(initCtx, "initialize", map[string]any{"clientID": "lumencortex", "adapterID": a.ID, "pathFormat": "path", "linesStartAt1": true, "columnsStartAt1": true, "supportsVariableType": true, "supportsVariablePaging": true, "supportsRunInTerminalRequest": false}, &caps); err != nil {
		return fail(err)
	}
	var filters []string
	if items, ok := caps["exceptionBreakpointFilters"].([]any); ok {
		for _, item := range items {
			if entry, ok := item.(map[string]any); ok {
				if name, ok := entry["filter"].(string); ok && name != "" && len(filters) < 32 {
					filters = append(filters, name)
				}
			}
		}
	}
	m.mu.Lock()
	s.capabilities = make(map[string]bool)
	for k, v := range caps {
		if b, ok := v.(bool); ok {
			s.capabilities[k] = b
		}
	}
	s.exceptionFilters = filters
	s.snapshot.ExceptionFilters = slices.Clone(filters)
	m.mu.Unlock()
	if opts.PrepareForAttach {
		_ = identityWrite.Close()
		_ = identityRead.SetReadDeadline(time.Now().Add(5 * time.Second))
		stopRead := context.AfterFunc(life, func() { _ = identityRead.Close() })
		defer stopRead()
		var identity struct {
			PID       int `json:"pid"`
			ParentPID int `json:"parentPID"`
		}
		if err := json.NewDecoder(io.LimitReader(identityRead, 1024)).Decode(&identity); err != nil {
			return fail(err)
		}
		if identity.PID <= 1 || identity.ParentPID <= 1 {
			return fail(ErrDenied)
		}
		m.mu.Lock()
		s.snapshot.TargetPID = identity.PID
		s.snapshot.State = "prepared"
		snapshot := s.snapshot
		m.mu.Unlock()
		go func() {
			select {
			case <-life.Done():
				m.stopSession(s)
			case <-s.done:
			}
		}()
		m.emit(s, "debug.session.started", map[string]any{"status": "prepared"})
		return snapshot, nil
	}
	launch := make(map[string]any, len(a.Launch)+4)
	for k, v := range a.Launch {
		launch[k] = v
	}
	launch["program"] = program
	launch["cwd"] = root
	if len(opts.Args) > 0 {
		launch["args"] = opts.Args
	} else {
		delete(launch, "args")
	}
	launch["console"] = "internalConsole"
	launched := make(chan error, 1)
	go func() { launched <- s.process.Client.Request(life, "launch", launch, nil) }()
	launchDone := false
	select {
	case <-s.initialized:
	case err = <-launched:
		launchDone = true
		if err != nil {
			return fail(err)
		}
		select {
		case <-s.initialized:
		case <-initCtx.Done():
			return fail(initCtx.Err())
		}
	case <-initCtx.Done():
		return fail(initCtx.Err())
	}
	// Adapters such as debugpy defer launch response until configurationDone.
	// Launch waits under the session lifetime, not the shorter initialize context.
	m.mu.Lock()
	s.snapshot.State = "configuring"
	snapshot := s.snapshot
	m.mu.Unlock()
	// The launch completion is drained independently; failure terminates the session.
	go func() {
		if !launchDone {
			select {
			case err := <-launched:
				if err != nil {
					m.stopSession(s)
					return
				}
			case <-life.Done():
				m.stopSession(s)
				return
			}
		}
		select {
		case <-life.Done():
			m.stopSession(s)
		case <-s.done:
		}
	}()
	m.emit(s, "debug.session.started", map[string]any{"status": "configuring"})
	return snapshot, nil
}
func (m *Manager) emit(s *session, event string, data map[string]any) {
	if m.opts.OnEvent != nil {
		data["debugId"] = s.snapshot.ID
		data["runId"] = s.opts.RunID
		m.opts.OnEvent(s.opts.SessionID, event, data)
	}
}
func (m *Manager) events(s *session, client *dap.Client) {
	for event := range client.Events() {
		var body stopEvent
		var exitBody struct {
			ExitCode int `json:"exitCode"`
		}
		_ = json.Unmarshal(event.Body, &body)
		_ = json.Unmarshal(event.Body, &exitBody)
		m.mu.Lock()
		if s.snapshot.State != "closed" && (client == s.client || event.Event == "initialized") {
			switch event.Event {
			case "initialized":
				s.initOnce.Do(func() { close(s.initialized) })
			case "stopped":
				s.stopEpoch++
				s.snapshot.State = "stopped"
				s.snapshot.ThreadID = body.ThreadID
				s.snapshot.StopEpoch = s.stopEpoch
				s.snapshot.StopReason = body.Reason
				epoch := s.stopEpoch
				m.mu.Unlock()
				m.emit(s, "debug."+event.Event, map[string]any{"reason": body.Reason, "threadId": body.ThreadID, "stopEpoch": epoch})
				m.captureStop(s, body)
				continue
			case "continued":
				s.stopEpoch++
				s.snapshot.State = "running"
				s.snapshot.StopEpoch = s.stopEpoch
				s.snapshot.StopReason = ""
			case "terminated", "exited":
				s.stopEpoch++
				s.snapshot.State = "terminated"
				s.snapshot.StopEpoch = s.stopEpoch
				s.snapshot.StopReason = event.Event
				reason := event.Event
				stop := &StopInfo{Reason: reason, ThreadID: s.snapshot.ThreadID, StopEpoch: s.stopEpoch, State: "terminated"}
				s.lastStop = stop
				close(s.stopReady)
				s.stopReady = make(chan struct{})
			}
		}
		m.mu.Unlock()
		// Never persist raw adapter expressions, environment, output or variables.
		m.emit(s, "debug."+event.Event, map[string]any{"reason": body.Reason, "threadId": body.ThreadID, "exitCode": exitBody.ExitCode})
	}
	// The transport ended: a disconnect we initiated is expected; anything else
	// is an adapter crash and must be observable as a lost session.
	m.mu.Lock()
	graceful := s.closing || s.snapshot.State == "terminated" || s.snapshot.State == "closed" || s.snapshot.State == "cleanup_failed"
	lost := false
	if !graceful && s.snapshot.State != "lost" {
		s.stopEpoch++
		s.snapshot.State = "lost"
		s.snapshot.StopEpoch = s.stopEpoch
		s.snapshot.StopReason = "lost"
		if client.Err() != nil {
			s.snapshot.Error = "debug adapter connection lost"
		}
		s.lastStop = &StopInfo{Reason: "lost", ThreadID: s.snapshot.ThreadID, StopEpoch: s.stopEpoch, State: "lost"}
		close(s.stopReady)
		s.stopReady = make(chan struct{})
		lost = true
	}
	m.mu.Unlock()
	if lost {
		m.emit(s, "debug.session.lost", map[string]any{"reason": "adapter exited unexpectedly"})
	}
	go m.stopSession(s)
}

// captureStop bounds the stop into a StopInfo after the state transition. A
// stop superseded by a newer transition is dropped from lastStop but still
// wakes waiters, which re-read the live state.
func (m *Manager) captureStop(s *session, body stopEvent) {
	stop := StopInfo{Reason: body.Reason, ThreadID: body.ThreadID, StopEpoch: m.stopEpochOf(s), State: "stopped"}
	ctx, cancel := context.WithTimeout(s.life, 5*time.Second)
	defer cancel()
	var stack struct {
		Frames []struct {
			ID     int    `json:"id"`
			Name   string `json:"name"`
			Line   int    `json:"line"`
			Source struct {
				Path string `json:"path"`
			} `json:"source"`
		} `json:"stackFrames"`
	}
	if s.client != nil && body.ThreadID > 0 {
		if err := s.client.Request(ctx, "stackTrace", map[string]any{"threadId": body.ThreadID, "startFrame": 0, "levels": 5}, &stack); err == nil {
			for _, frame := range stack.Frames {
				if len(stop.TopFrames) == 5 {
					break
				}
				stop.TopFrames = append(stop.TopFrames, FrameSummary{ID: frame.ID, Name: clip(frame.Name, 512), Line: frame.Line, Path: clip(frame.Source.Path, 1024)})
			}
		}
	}
	if body.Reason == "exception" {
		info := &ExceptionInfo{Description: clip(body.Description, 1024)}
		if body.Text != "" {
			info.Message = clip(body.Text, 1024)
		}
		var details exceptionBody
		if s.client != nil && body.ThreadID > 0 {
			if err := s.client.Request(ctx, "exceptionInfo", map[string]any{"threadId": body.ThreadID}, &details); err == nil {
				info.ID = clip(details.ExceptionID, 256)
				info.BreakMode = clip(details.BreakMode, 64)
				if details.Description != "" {
					info.Description = clip(details.Description, 1024)
				}
				if details.Details.Message != "" {
					info.Message = clip(details.Details.Message, 1024)
				}
			}
		}
		stop.Exception = info
	}
	m.mu.Lock()
	if s.stopEpoch == stop.StopEpoch {
		s.lastStop = &stop
	}
	ready := s.stopReady
	s.stopReady = make(chan struct{})
	m.mu.Unlock()
	close(ready)
}
func (m *Manager) lookup(sessionID, id string) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil || s.opts.SessionID != sessionID {
		return nil, ErrNotFound
	}
	return s, nil
}
func (m *Manager) Snapshot(sessionID, id string) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil || s.opts.SessionID != sessionID {
		return Snapshot{}, ErrNotFound
	}
	out := s.snapshot
	if s.process != nil {
		out.DroppedOutput = s.process.Client.DroppedOutput()
	}
	return out, nil
}
func (m *Manager) List(sessionID string) []Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Snapshot{}
	for _, s := range m.sessions {
		if s.opts.SessionID == sessionID {
			out = append(out, s.snapshot)
		}
	}
	slices.SortFunc(out, func(a, b Snapshot) int { return a.StartedAt.Compare(b.StartedAt) })
	return out
}
func (m *Manager) lock(ctx context.Context, c Control) (*session, func(), error) {
	s, err := m.lookup(c.SessionID, c.DebugID)
	if err != nil {
		return nil, nil, err
	}
	select {
	case s.op <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-s.done:
		return nil, nil, ErrState
	}
	unlock := func() { <-s.op }
	m.mu.Lock()
	valid := s.snapshot.Epoch == c.Epoch && s.snapshot.Owner == c.Actor && s.snapshot.State != "closed"
	m.mu.Unlock()
	if !valid {
		unlock()
		return nil, nil, ErrStale
	}
	return s, unlock, nil
}
func (m *Manager) request(ctx context.Context, s *session, command string, args any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if s.adapter.MCPServer != "" {
		return m.requestIntelliJ(ctx, s, command, args)
	}
	var out json.RawMessage
	err := s.client.Request(ctx, command, args, &out)
	return out, err
}
func (m *Manager) Control(ctx context.Context, c Control) (Snapshot, error) {
	if c.Action == "stop" {
		snapshot, _, err := m.Disconnect(ctx, c, true)
		return snapshot, err
	}
	s, unlock, err := m.lock(ctx, c)
	if err != nil {
		return Snapshot{}, err
	}
	defer unlock()
	command := ""
	args := map[string]any{"threadId": c.ThreadID}
	switch c.Action {
	case "configure":
		command = "configurationDone"
	case "continue":
		command = "continue"
	case "pause":
		command = "pause"
	case "step_over":
		command = "next"
	case "step_into":
		command = "stepIn"
	case "step_out":
		command = "stepOut"
	default:
		return Snapshot{}, ErrDenied
	}
	if _, err = m.request(ctx, s, command, args); err != nil {
		return Snapshot{}, err
	}
	return m.Snapshot(c.SessionID, c.DebugID)
}
func (m *Manager) Transfer(ctx context.Context, c Control, owner string) (Snapshot, error) {
	if c.Actor != "human" || (owner != "agent" && owner != "human") {
		return Snapshot{}, ErrDenied
	}
	s, err := m.lookup(c.SessionID, c.DebugID)
	if err != nil {
		return Snapshot{}, err
	}
	select {
	case s.op <- struct{}{}:
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	case <-s.done:
		return Snapshot{}, ErrState
	}
	defer func() { <-s.op }()
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.snapshot.Epoch != c.Epoch {
		return Snapshot{}, ErrStale
	}
	s.snapshot.Owner = owner
	s.snapshot.Epoch++
	return s.snapshot, nil
}
func (m *Manager) Breakpoints(ctx context.Context, c Control, path string, points []Breakpoint) (json.RawMessage, error) {
	if len(points) > 128 {
		return nil, ErrDenied
	}
	s, unlock, err := m.lock(ctx, c)
	if err != nil {
		return nil, err
	}
	defer unlock()
	// Validate the resolved vnode, but preserve the spelling recorded by the compiler
	// (notably macOS /var versus /private/var) for adapter source matching.
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.opts.Workspace, path)
	}
	_, err = confined(s.opts.Workspace, path)
	if err != nil {
		return nil, err
	}
	for _, p := range points {
		if p.Line < 1 || len(p.Condition) > 1024 || len(p.HitCondition) > 64 {
			return nil, ErrDenied
		}
		if (p.Condition != "" || p.HitCondition != "") && !s.opts.AllowEvaluate {
			return nil, ErrDenied
		}
	}
	out, err := m.request(ctx, s, "setBreakpoints", map[string]any{"source": map[string]string{"path": path}, "breakpoints": points, "sourceModified": false})
	if err != nil {
		return nil, err
	}
	s.breakpoints[path] = slices.Clone(points)
	// The caller reads the adapter's verification (verified/line/message), never
	// an echo of the request.
	m.mu.Lock()
	supported := slicesClone(s.exceptionFilters)
	m.mu.Unlock()
	if len(out) == 0 {
		out = json.RawMessage(`{}`)
	}
	var body map[string]any
	if err = json.Unmarshal(out, &body); err != nil {
		return nil, err
	}
	if raw, ok := body["breakpoints"].([]any); ok {
		for _, item := range raw {
			if entry, ok := item.(map[string]any); ok {
				if _, ok := entry["verified"]; !ok {
					entry["verified"] = false
				}
			}
		}
	}
	body["exceptionFilters"] = supported
	return boundJSON(mustMarshal(body), m.stopEpochOf(s)), nil
}
func (m *Manager) Inspect(ctx context.Context, c Control, kind string, reference, start, count int) (json.RawMessage, error) {
	s, unlock, err := m.lock(ctx, c)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if start < 0 || count < 0 || count > 100 || reference < 0 {
		return nil, ErrDenied
	}
	if count == 0 {
		count = 20
	}
	m.mu.Lock()
	state := s.snapshot.State
	m.mu.Unlock()
	if kind != "threads" && state != "stopped" {
		return nil, fmt.Errorf("%w: target is %s; stack, scopes and variables need a stopped target", ErrState, state)
	}
	switch kind {
	case "threads":
		raw, err := m.request(ctx, s, kind, map[string]any{})
		if err != nil {
			return nil, err
		}
		return boundJSON(raw, m.stopEpochOf(s)), nil
	case "stackTrace":
		args := map[string]any{"threadId": reference, "startFrame": start, "levels": min(count, 20)}
		raw, err := m.request(ctx, s, kind, args)
		if err != nil {
			return nil, err
		}
		m.recordFrames(s, raw)
		return boundJSON(raw, m.stopEpochOf(s)), nil
	case "scopes", "variables":
		if err = m.staleReference(s, reference); err != nil {
			return nil, err
		}
		args := map[string]any{"frameId": reference}
		if kind == "variables" {
			args = map[string]any{"variablesReference": reference, "start": start, "count": count}
		}
		raw, err := m.request(ctx, s, kind, args)
		if err != nil {
			return nil, err
		}
		m.recordChildReferences(s, kind, raw)
		return boundJSON(raw, m.stopEpochOf(s)), nil
	default:
		return nil, ErrDenied
	}
}

// recordFrames registers the frame IDs of a stack trace at the current stop.
func (m *Manager) recordFrames(s *session, raw json.RawMessage) {
	var body struct {
		StackFrames []struct {
			ID int `json:"id"`
		} `json:"stackFrames"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return
	}
	refs := make([]int, 0, len(body.StackFrames))
	for _, frame := range body.StackFrames {
		refs = append(refs, frame.ID)
	}
	m.recordReferences(s, refs...)
}

// recordChildReferences registers the references a scopes/variables response
// hands out so a later stop can reject them.
func (m *Manager) recordChildReferences(s *session, kind string, raw json.RawMessage) {
	refs := []int{}
	switch kind {
	case "scopes":
		var body struct {
			Scopes []struct {
				VariablesReference int `json:"variablesReference"`
			} `json:"scopes"`
		}
		if json.Unmarshal(raw, &body) != nil {
			return
		}
		for _, scope := range body.Scopes {
			refs = append(refs, scope.VariablesReference)
		}
	case "variables":
		var body struct {
			Variables []struct {
				VariablesReference int `json:"variablesReference"`
			} `json:"variables"`
		}
		if json.Unmarshal(raw, &body) != nil {
			return
		}
		for _, variable := range body.Variables {
			refs = append(refs, variable.VariablesReference)
		}
	}
	m.recordReferences(s, refs...)
}

var identifier = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

func (m *Manager) Evaluate(ctx context.Context, c Control, frame int, expression string, sideEffects bool) (json.RawMessage, error) {
	s, unlock, err := m.lock(ctx, c)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if len(expression) > 1024 || frame < 0 {
		return nil, ErrDenied
	}
	m.mu.Lock()
	state := s.snapshot.State
	m.mu.Unlock()
	if state != "stopped" {
		return nil, fmt.Errorf("%w: target is %s; evaluation needs a stopped target", ErrState, state)
	}
	if err = m.staleReference(s, frame); err != nil {
		return nil, err
	}
	if sideEffects {
		if !s.opts.AllowEvaluate {
			return nil, ErrEvaluateNotGranted
		}
		raw, err := m.request(ctx, s, "evaluate", map[string]any{"expression": expression, "frameId": frame, "context": "watch"})
		if err != nil {
			return nil, err
		}
		return boundJSON(raw, m.stopEpochOf(s)), nil
	}
	// Read-only evaluation is variable lookup, not adapter evaluation. Even getters
	// and overloaded operators must not execute target code under a read grant.
	if !identifier.MatchString(expression) {
		return nil, ErrDenied
	}
	raw, err := m.request(ctx, s, "scopes", map[string]int{"frameId": frame})
	if err != nil {
		return nil, err
	}
	var scopes struct {
		Scopes []struct {
			Reference int `json:"variablesReference"`
		} `json:"scopes"`
	}
	if err = json.Unmarshal(raw, &scopes); err != nil {
		return nil, err
	}
	for _, scope := range scopes.Scopes[:min(len(scopes.Scopes), 8)] {
		raw, err = m.request(ctx, s, "variables", map[string]int{"variablesReference": scope.Reference, "start": 0, "count": 100})
		if err != nil {
			return nil, err
		}
		var vars struct {
			Variables []struct {
				Name      string `json:"name"`
				Value     string `json:"value"`
				Type      string `json:"type"`
				Reference int    `json:"variablesReference"`
			} `json:"variables"`
		}
		if err = json.Unmarshal(raw, &vars); err != nil {
			return nil, err
		}
		for _, v := range vars.Variables {
			if v.Name == expression {
				return boundJSON(mustMarshal(map[string]any{"result": v.Value, "type": v.Type, "variablesReference": v.Reference}), m.stopEpochOf(s)), nil
			}
		}
	}
	return nil, fmt.Errorf("variable %q not in bounded visible scopes", expression)
}
func (m *Manager) stopSession(s *session) error {
	s.cancel()
	s.closeOnce.Do(func() {
		s.op <- struct{}{}
		defer func() { <-s.op }()
		if s.adapter.MCPServer != "" {
			s.closeErr = m.closeIntelliJ(s)
		}
		for _, child := range s.children {
			_ = child.Close()
		}
		if s.process != nil {
			s.closeErr = s.process.Close()
		}
		if s.dir != "" {
			s.closeErr = errors.Join(s.closeErr, os.RemoveAll(s.dir))
		}
		m.mu.Lock()
		if s.closeErr != nil {
			s.snapshot.State = "cleanup_failed"
			s.snapshot.Error = s.closeErr.Error()
		} else {
			s.snapshot.State = "closed"
			delete(m.sessions, s.snapshot.ID)
		}
		m.mu.Unlock()
		status := "closed"
		if s.closeErr != nil {
			status = "cleanup_failed"
		}
		m.emit(s, "debug.session.stopped", map[string]any{"status": status, "cleanupFailed": s.closeErr != nil})
		close(s.done)
	})
	<-s.done
	return s.closeErr
}
func (m *Manager) CloseOwner(sessionID string) error {
	m.mu.Lock()
	var all []*session
	for _, s := range m.sessions {
		if s.opts.SessionID == sessionID {
			all = append(all, s)
		}
	}
	m.mu.Unlock()
	var err error
	for _, s := range all {
		err = errors.Join(err, m.stopSession(s))
	}
	return err
}
func (m *Manager) Close() error {
	m.mu.Lock()
	m.closed = true
	all := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	var err error
	for _, s := range all {
		err = errors.Join(err, m.stopSession(s))
	}
	return err
}

func (m *Manager) Done(sessionID, id string) (<-chan struct{}, error) {
	s, err := m.lookup(sessionID, id)
	if err != nil {
		return nil, err
	}
	return s.done, nil
}
func (m *Manager) CloseRun(sessionID, runID string) error {
	m.mu.Lock()
	var all []*session
	for _, s := range m.sessions {
		if s.opts.SessionID == sessionID && s.opts.RunID == runID {
			all = append(all, s)
		}
	}
	m.mu.Unlock()
	var err error
	for _, s := range all {
		err = errors.Join(err, m.stopSession(s))
	}
	return err
}
