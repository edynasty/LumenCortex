package evalruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/internal/streambuf"
	"github.com/edynasty/LumenCortex/protocol"
)

// Wire types of the JSON-RPC control protocol (SPI 1.0). They are deliberately
// private: the public contract is evalapi, the wire is an implementation detail a
// plugin may reimplement in any language.
type wireInit struct {
	SPIVersion      string         `json:"spiVersion"`
	ProtocolVersion string         `json:"protocolVersion"`
	SessionID       string         `json:"sessionId"`
	Language        string         `json:"language"`
	Workspace       string         `json:"workspace"`
	ArtifactDir     string         `json:"artifactDir"`
	Limits          evalapi.Budget `json:"limits"`
}

type wireInitResult struct {
	SPIVersion     string   `json:"spiVersion"`
	RuntimeVersion string   `json:"runtimeVersion"`
	Language       string   `json:"language"`
	PID            int      `json:"pid"`
	Capabilities   []string `json:"capabilities"`
}

type wireExecute struct {
	RequestID      string              `json:"requestId"`
	CellID         string              `json:"cellId"`
	Code           string              `json:"code"`
	DeadlineUnixMS int64               `json:"deadlineUnixMs"`
	Budget         evalapi.Budget      `json:"budget"`
	Tools          []protocol.ToolSpec `json:"tools,omitempty"`
	RunID          string              `json:"runId,omitempty"`
	TrialID        string              `json:"trialId,omitempty"`
}

type wireDisplay struct {
	MIME     string `json:"mime"`
	Encoding string `json:"encoding,omitempty"`
	Text     string `json:"text,omitempty"`
	Size     int64  `json:"size,omitempty"`
}

type wireArtifact struct {
	Name string `json:"name"`
	MIME string `json:"mime,omitempty"`
	Size int64  `json:"size,omitempty"`
}

type wireResult struct {
	Status          string         `json:"status"`
	Stdout          string         `json:"stdout"`
	Stderr          string         `json:"stderr"`
	Error           string         `json:"error"`
	Traceback       string         `json:"traceback"`
	StateStatus     string         `json:"stateStatus"`
	ExitCode        int            `json:"exitCode"`
	Displays        []wireDisplay  `json:"displays"`
	Artifacts       []wireArtifact `json:"artifacts"`
	DurationMS      int64          `json:"durationMs"`
	OutputTruncated bool           `json:"outputTruncated"`
}

type wireHealth struct {
	Status     string `json:"status"`
	Generation uint64 `json:"generation"`
	PID        int    `json:"pid"`
}

type wireToolCall struct {
	CallID    string         `json:"callId"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	// RequestID is the execution the worker believes it is serving. It is
	// optional for third-party runtimes; when present the host refuses a
	// callback that names a different execution.
	RequestID string `json:"requestId,omitempty"`
}

// worker is one live runtime process and its control connection.
type worker struct {
	s          *session
	m          *Manager
	generation uint64
	runtime    string
	idStr      string

	proc *process
	conn *rpcConn

	active atomic.Pointer[activeCall]
	dead   atomic.Bool
	mu     sync.Mutex
}

func (w *worker) id() string {
	if w == nil {
		return ""
	}
	return w.idStr
}

func (w *worker) alive() bool {
	return w != nil && w.proc != nil && !w.dead.Load()
}

func (w *worker) start(ctx context.Context) error {
	s := w.s
	spec := launchSpec{
		exe:             s.manifest.Executable,
		args:            s.manifest.Args,
		dir:             s.workDir,
		env:             s.workerEnv(),
		kernelDir:       s.kernel,
		profilePath:     s.profilePath,
		workersDir:      w.m.workersDir,
		cpuSeconds:      s.budget.CPUSeconds,
		fileBytes:       s.budget.DiskBytes,
		network:         w.m.opts.Sandbox.AllowNetwork,
		allowSubprocess: subprocessSupported(),
		// Host-owned interpreter installation roots plus explicit host extras.
		// Never derived from a cell or from an arbitrary registration.
		extraRead: append(append([]string(nil), w.m.opts.Sandbox.ExtraReadPaths...), s.readPaths...),
		identity:  w.m.opts.identity,
	}
	cmd, err := w.m.launcher.command(spec)
	if err != nil {
		return err
	}
	launchFiles, releaseLaunch, err := prepareBrokerLaunch(w.m.opts.identity)
	if err != nil {
		return err
	}
	defer releaseLaunch()
	// Linux: the worker's cgroup exists, with its limits written, before the
	// worker runs; startProcess admits the gated process into it before release.
	// No cgroup means no worker.
	var leaf *cgroupLeaf
	if cgroupRequired {
		if w.m.cgroups == nil {
			return fmt.Errorf("%w: %s", evalapi.ErrSandboxUnsupported, firstNonEmpty(w.m.isolationReason, "resource isolation unavailable"))
		}
		leaf, err = w.m.cgroups.newLeaf(s.budget.MemoryBytes)
		if err != nil {
			return fmt.Errorf("%w: resource isolation unavailable: %v", evalapi.ErrSandboxUnsupported, err)
		}
	}
	p, err := startProcess(procSpec{
		cmd:         cmd,
		dir:         spec.dir,
		env:         spec.env,
		memoryBytes: s.budget.MemoryBytes,
		diskBytes:   s.budget.DiskBytes,
		diskRoot:    s.kernel,
		onStdout:    w.onStdout,
		onStderr:    w.onStderr,
		onLimit:     w.onLimit,
		cgroup:      leaf,
		launchFiles: launchFiles,
	})
	if err != nil {
		return fmt.Errorf("%w: %v", evalapi.ErrWorker, err)
	}
	w.proc = p
	if leaf != nil {
		// The sandbox's own session must exist and be identifiable before the
		// worker is used: interrupts are delivered there and nowhere else.
		groupErr := injectStartFault("group")
		if groupErr == nil {
			_, groupErr = p.workerGroup()
		}
		if groupErr != nil {
			return w.abortStart(fmt.Errorf("%w: %v", evalapi.ErrWorker, groupErr))
		}
	}
	w.idStr = fmt.Sprintf("%s#%d", s.manifest.ID, p.pid)
	w.conn = newRPCConn(p.protoOut, p.protoIn, w.handleRequest, w.handleNotification)
	w.conn.start()
	go func() {
		<-p.waitCh
		w.dead.Store(true)
		w.conn.close(errConnClosed)
		w.s.onWorkerExit(w)
	}()

	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := []byte(nil), injectStartFault("initialize")
	if err == nil {
		raw, err = w.conn.call(initCtx, "initialize", wireInit{
			SPIVersion:      evalapi.SPIVersion,
			ProtocolVersion: evalapi.ProtocolVersion,
			SessionID:       s.id,
			Language:        s.manifest.Language,
			Workspace:       s.workDir,
			ArtifactDir:     s.pendingDir,
			Limits:          s.budget,
		})
	}
	if err != nil {
		detail := ""
		if p.stderrText() != "" {
			detail = ": " + p.stderrText()
		}
		return w.abortStart(fmt.Errorf("%w: initialize: %v%s", evalapi.ErrWorker, err, detail))
	}
	var init wireInitResult
	if err := json.Unmarshal(raw, &init); err != nil {
		return w.abortStart(fmt.Errorf("%w: initialize result: %v", evalapi.ErrWorker, err))
	}
	if err := injectStartFault("handshake"); err != nil {
		init.SPIVersion = err.Error()
	}
	if major(init.SPIVersion) != major(evalapi.SPIVersion) {
		return w.abortStart(fmt.Errorf("%w: runtime %s negotiated SPI %q, host speaks %q",
			evalapi.ErrUnsupportedCapability, s.manifest.ID, init.SPIVersion, evalapi.SPIVersion))
	}
	w.runtime = init.RuntimeVersion
	return nil
}

// abortStart stops a worker whose process started but which failed before it
// was handed to the session: the tree is terminated and its cgroup removed now,
// not at Dispose. Anything that could not be cleaned up is added to cause.
func (w *worker) abortStart(cause error) error {
	residual := w.shutdown(context.Background())
	if len(residual) > 0 {
		cause = errors.Join(cause, fmt.Errorf("%w: processes still running after the failed start: %v", evalapi.ErrWorker, residual))
	}
	if w.proc != nil {
		cause = errors.Join(cause, w.proc.close())
	}
	return cause
}

func (w *worker) onStdout(chunk []byte) {
	if ac := w.active.Load(); ac != nil && ac.out != nil {
		_, _ = ac.out.Write(chunk)
	}
}

func (w *worker) onStderr(chunk []byte) {
	if ac := w.active.Load(); ac != nil && ac.err != nil {
		_, _ = ac.err.Write(chunk)
	}
}

func (w *worker) onLimit(reason string) {
	w.s.m.emit(evalapi.Event{Type: evalapi.EventResourceLimit, SessionID: w.s.id, OwnerID: w.s.ownerID,
		AgentID: w.s.agentID, Generation: w.generation, Data: map[string]string{"reason": reason}})
}

func (w *worker) execute(ctx context.Context, params wireExecute) (wireResult, error) {
	raw, err := w.conn.call(ctx, "execute", params)
	if err != nil {
		return wireResult{}, err
	}
	var result wireResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return wireResult{}, fmt.Errorf("%w: execute result: %v", evalapi.ErrWorker, err)
	}
	return result, nil
}

// interrupt asks the worker to stop whatever it runs, as the first step of a
// reset, close or dispose that terminates it anyway.
func (w *worker) interrupt(reason string) {
	w.notifyInterrupt(reason, "")
	w.signalInterrupt()
}

// notifyInterrupt tells a cell blocked on the control channel (a tool
// callback) that it is interrupted. requestID names the execution the request
// is for ("" when it is not tied to one); the worker ignores a notification
// that names another execution.
func (w *worker) notifyInterrupt(reason, requestID string) {
	if w.conn != nil {
		_ = w.conn.notifyPeer("interrupt", map[string]any{"reason": reason, "requestId": requestID})
	}
}

// signalInterrupt sends SIGINT to the worker's process group.
func (w *worker) signalInterrupt() {
	if w.proc != nil {
		_ = w.proc.interrupt()
	}
}

func (w *worker) kill() []int {
	if w.proc == nil {
		return nil
	}
	return w.proc.killTree()
}

func (w *worker) health(ctx context.Context) (wireHealth, error) {
	raw, err := w.conn.call(ctx, "health", map[string]any{})
	if err != nil {
		return wireHealth{}, err
	}
	var health wireHealth
	if err := json.Unmarshal(raw, &health); err != nil {
		return wireHealth{}, err
	}
	return health, nil
}

func (w *worker) resetState(ctx context.Context) (wireHealth, error) {
	raw, err := w.conn.call(ctx, "reset", map[string]any{})
	if err != nil {
		return wireHealth{}, err
	}
	var health wireHealth
	if err := json.Unmarshal(raw, &health); err != nil {
		return wireHealth{}, err
	}
	return health, nil
}

// shutdown closes the worker: a polite close, then group and descendant
// termination. The returned pids are descendants that survived the cleanup
// window; a non-empty list means the stop was not clean.
func (w *worker) shutdown(ctx context.Context) []int {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	conn, proc := w.conn, w.proc
	w.mu.Unlock()
	if conn != nil {
		closeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, _ = conn.call(closeCtx, "close", map[string]any{})
		cancel()
	}
	var residual []int
	if proc != nil {
		residual = proc.terminate(ctx)
		proc.close()
	}
	if conn != nil {
		conn.close(errConnClosed)
	}
	w.dead.Store(true)
	return residual
}

func (w *worker) handleRequest(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "tool/call":
		call := w.active.Load()
		if call == nil {
			return nil, &rpcError{Code: -32001, Message: "no execution is active; tool callbacks are closed"}
		}
		var req wireToolCall
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid tool/call params"}
		}
		// A worker that names its execution must name this one: a callback from
		// a superseded cell never borrows the capability of the current cell.
		if req.RequestID != "" && req.RequestID != call.requestID {
			w.s.m.emit(evalapi.Event{
				Type: evalapi.EventCallbackUnauthorized, SessionID: w.s.id, OwnerID: w.s.ownerID, AgentID: call.executor,
				RequestID: req.RequestID, CellID: call.cellID, Generation: call.generation,
				Data: map[string]any{"tool": req.Name, "callId": req.CallID,
					"reason": fmt.Sprintf("callback names execution %q, but execution %q is active", req.RequestID, call.requestID)},
			})
			return map[string]any{"ok": false, "denied": true,
				"content": fmt.Sprintf("callback names execution %q, but execution %q is active", req.RequestID, call.requestID)}, nil
		}
		return call.invoke(req)
	}
	return nil, &rpcError{Code: -32601, Message: "unsupported method " + method}
}

func (w *worker) handleNotification(method string, params json.RawMessage) {
	if method != "event" {
		return
	}
	var event struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(params, &event); err != nil {
		return
	}
	if event.Type == "started" {
		// The worker entered the cell's code: from now on a SIGINT is
		// attributable to this request (see Manager.Interrupt).
		var started struct {
			RequestID string `json:"requestId"`
		}
		if json.Unmarshal(event.Data, &started) == nil {
			w.s.markStarted(w, started.RequestID)
		}
		return
	}
	if event.Type != "display" {
		return
	}
	var display wireDisplay
	if err := json.Unmarshal(event.Data, &display); err != nil {
		return
	}
	w.s.m.emit(evalapi.Event{Type: evalapi.EventDisplay, SessionID: w.s.id, OwnerID: w.s.ownerID,
		AgentID: w.s.agentID, Generation: w.generation,
		Data: map[string]any{"mime": display.MIME, "size": display.Size}})
}

// boundedText builds the streaming sink for one execution's raw stdout/stderr.
func boundedText(limit int64) *streambuf.HeadTail {
	head := limit / 2
	tail := limit - head
	if head < 1024 {
		head = 1024
	}
	if tail < 1024 {
		tail = 1024
	}
	return streambuf.New(int(head), int(tail))
}

func snapshotText(buf *streambuf.HeadTail) (string, bool) {
	if buf == nil {
		return "", false
	}
	snap := buf.Snapshot()
	if snap.Truncated {
		return string(snap.Head) + fmt.Sprintf("\n... [truncated %d bytes] ...\n", snap.Total-int64(len(snap.Head)+len(snap.Tail))) + string(snap.Tail), true
	}
	return string(snap.Head) + string(snap.Tail), false
}

var errNoWorker = errors.New("eval: worker is not running")
