package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/evalruntime"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/protocol"
)

// EvalOptions is host-owned. Plugins never choose executable paths, grants or budgets.
// A nil Options.Eval disables all execution plugins and preserves the legacy tools.
type EvalOptions struct {
	Python     string         `json:"python,omitempty"`
	JavaScript string         `json:"javascript,omitempty"`
	Budget     evalapi.Budget `json:"budget"`
	// Bubblewrap is the root-owned bwrap the Linux sandbox runs; empty means
	// "bwrap" on PATH. See evalruntime.SandboxOptions.BubblewrapPath.
	Bubblewrap string `json:"bubblewrap,omitempty"`
	// RequireCallerIsolation makes the Linux sandbox fail closed unless the
	// user-namespace grant behind Bubblewrap cannot be borrowed by other callers.
	// See evalruntime.SandboxOptions.RequireCallerIsolation.
	RequireCallerIsolation bool   `json:"requireCallerIsolation,omitempty"`
	RunID                  string `json:"runId,omitempty"`
	TrialID                string `json:"trialId,omitempty"`
}

var ErrEvalDisabled = errors.New("execution runtimes are disabled by the host")

func cloneEvalOptions(in *EvalOptions) *EvalOptions {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func (e *Engine) evalHost(ctx context.Context) (*evalruntime.Manager, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.evalMu.Lock()
	defer e.evalMu.Unlock()
	e.runsMu.Lock()
	closing := e.closing
	e.runsMu.Unlock()
	if closing {
		return nil, errors.New("engine is closing")
	}
	if e.evalOptions == nil {
		return nil, ErrEvalDisabled
	}
	if e.evalManager != nil {
		return e.evalManager, nil
	}
	manager, err := evalruntime.New(evalruntime.Options{Root: filepath.Join(e.repoDir, "eval"), Python: e.evalOptions.Python, JavaScript: e.evalOptions.JavaScript, Budget: e.evalOptions.Budget, Sandbox: evalruntime.SandboxOptions{BubblewrapPath: e.evalOptions.Bubblewrap, RequireCallerIsolation: e.evalOptions.RequireCallerIsolation}, OnEvent: func(event evalapi.Event) {
		e.events.publish("eval."+event.Type, event.OwnerID, event)
	}})
	if err != nil {
		return nil, err
	}
	e.evalManager = manager
	return manager, nil
}

func (e *Engine) closeEval() error {
	e.evalMu.Lock()
	manager := e.evalManager
	e.evalManager = nil
	e.evalMu.Unlock()
	if manager == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return manager.Dispose(ctx)
}

func (e *Engine) closeEvalOwner(sessionID string) error {
	e.evalMu.Lock()
	manager := e.evalManager
	e.evalMu.Unlock()
	if manager == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return manager.CloseOwner(ctx, sessionID)
}

func (e *Engine) evalSessionAllowed(ctx context.Context, sessionID string) error {
	if _, err := e.store.Get(ctx, sessionID); err != nil {
		return err
	}
	withdrawn, err := e.store.SessionWithdrawn(ctx, sessionID)
	if err != nil {
		return err
	}
	if withdrawn {
		return ErrNotebookWithdrawn
	}
	return nil
}

func (e *Engine) EvalRuntimes(ctx context.Context) ([]evalapi.Manifest, error) {
	manager, err := e.evalHost(ctx)
	if err != nil {
		return nil, err
	}
	return manager.Discover(ctx)
}

// RegisterEvalRuntime accepts a host-approved manifest; it does not grant authority.
func (e *Engine) RegisterEvalRuntime(ctx context.Context, manifest evalapi.Manifest) error {
	manager, err := e.evalHost(ctx)
	if err != nil {
		return err
	}
	return manager.Register(manifest)
}

func (e *Engine) EvalSessions(ctx context.Context, sessionID string) ([]evalapi.Session, error) {
	if err := e.evalSessionAllowed(ctx, sessionID); err != nil {
		return nil, err
	}
	manager, err := e.evalHost(ctx)
	if err != nil {
		return nil, err
	}
	listed, err := manager.Sessions(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	// A kernel is one actor's authority domain. The manager lists kernels this
	// agent owns *or was granted read access to*; the host-facing view is the
	// agent's own kernels only, so a listing can never drive a cross-domain
	// reset, close or interrupt.
	owned := make([]evalapi.Session, 0, len(listed))
	for _, session := range listed {
		if session.OwnerID != "" && session.OwnerID != sessionID {
			continue
		}
		owned = append(owned, session)
	}
	return owned, nil
}

// CreateEvalSession creates a private calculation workspace, never the host's state directory
// or another session's workspace. Import source data using an explicitly authorized tool.
func (e *Engine) CreateEvalSession(ctx context.Context, sessionID, runtimeID string) (evalapi.Session, error) {
	return e.createEvalSession(ctx, sessionID, runtimeID, "")
}

func (e *Engine) createEvalSession(ctx context.Context, sessionID, runtimeID, parentID string) (evalapi.Session, error) {
	ctx, done, err := e.trackEval(ctx, sessionID)
	if err != nil {
		return evalapi.Session{}, err
	}
	defer done()
	manager, err := e.evalHost(ctx)
	if err != nil {
		return evalapi.Session{}, err
	}
	// LC identifiers are stored identities, but path construction still rejects separators.
	if sessionID == "" || filepath.Base(sessionID) != sessionID || sessionID == "." || sessionID == ".." {
		return evalapi.Session{}, errors.New("invalid session identity")
	}
	workspace := filepath.Join(e.repoDir, "eval-workspaces", sessionID)
	if err := os.MkdirAll(workspace, 0700); err != nil {
		return evalapi.Session{}, err
	}
	return manager.Create(ctx, evalapi.CreateOptions{RuntimeID: runtimeID, OwnerID: sessionID, AgentID: sessionID, ParentAgentID: parentID, Workspace: workspace, Permission: toolset.PolicyWorkspace, Budget: e.evalOptions.Budget})
}

// ExecuteEval is the host/UI entry. It grants no callbacks implicitly. Agent executions
// instead receive the loop's admitted callback dispatcher through executeEvalWith.
func (e *Engine) ExecuteEval(ctx context.Context, sessionID string, req evalapi.Request) (evalapi.Result, error) {
	return e.executeEvalWith(ctx, sessionID, req, nil)
}

func (e *Engine) executeEvalWith(ctx context.Context, sessionID string, req evalapi.Request, caller evalapi.ToolCaller) (evalapi.Result, error) {
	ctx, done, err := e.trackEval(ctx, sessionID)
	if err != nil {
		return evalapi.Result{}, err
	}
	defer done()
	manager, err := e.evalHost(ctx)
	if err != nil {
		return evalapi.Result{}, err
	}
	if req.AgentID != "" && req.AgentID != sessionID {
		return evalapi.Result{}, errors.New("request cannot impersonate another agent")
	}
	req.AgentID = sessionID
	if e.evalOptions != nil {
		if e.evalOptions.RunID != "" {
			req.RunID = e.evalOptions.RunID
		}
		req.TrialID = e.evalOptions.TrialID
	}
	if req.ID == "" {
		id, err := newEvalRequestID()
		if err != nil {
			return evalapi.Result{}, err
		}
		req.ID = id
	}
	// Session access is checked by the manager again at execution admission.
	if _, err := manager.Session(ctx, req.SessionID, sessionID); err != nil {
		return evalapi.Result{}, err
	}
	// Host lifecycle hooks run on every path, the UI's ExecuteEval included: a configured PreEval
	// safety hook can refuse an execution, and PostEval observes what it decided. This is the same
	// dispatch the agent loop uses, so the two paths cannot diverge.
	pre, err := e.DispatchExecutionHook(ctx, LifecycleRequest{Event: LifecyclePreEval, SessionID: sessionID, RunID: req.RunID, ToolCallID: req.ID, ToolName: "eval", Detail: map[string]any{"evalSessionId": req.SessionID, "cellId": req.CellID, "language": req.Language}})
	if err != nil {
		return evalapi.Result{}, err
	}
	if pre.Denied() {
		return evalapi.Result{}, fmt.Errorf("eval execution denied by a lifecycle hook: %s", pre.FeedbackText())
	}
	result, execErr := manager.Execute(ctx, req, caller)
	// PostEval is advisory: the execution already happened, so a deny is recorded by the hook's audit
	// entry and never rewrites the result.
	_, _ = e.DispatchExecutionHook(ctx, LifecycleRequest{Event: LifecyclePostEval, SessionID: sessionID, RunID: req.RunID, ToolCallID: req.ID, ToolName: "eval", Detail: map[string]any{"evalSessionId": req.SessionID, "cellId": req.CellID, "status": result.Status, "stateStatus": result.StateStatus}})
	return result, execErr
}

// EvalInterrupt interrupts exactly the execution target names; see
// evalruntime.Manager.InterruptExecution. A late interrupt whose target already
// finished is answered evalapi.InterruptTargetFinished and never stops the cell
// that runs now.
func (e *Engine) EvalInterrupt(ctx context.Context, sessionID, evalID string, target evalapi.InterruptTarget) (evalapi.InterruptOutcome, error) {
	ctx, done, err := e.trackEval(ctx, sessionID)
	if err != nil {
		return evalapi.InterruptOutcome{}, err
	}
	defer done()
	manager, err := e.evalHost(ctx)
	if err != nil {
		return evalapi.InterruptOutcome{}, err
	}
	return manager.InterruptExecution(ctx, evalID, sessionID, target)
}
func (e *Engine) EvalReset(ctx context.Context, sessionID, evalID string) error {
	return e.evalControl(ctx, sessionID, evalID, "reset")
}
func (e *Engine) EvalClose(ctx context.Context, sessionID, evalID string) error {
	return e.evalControl(ctx, sessionID, evalID, "close")
}
func (e *Engine) evalControl(ctx context.Context, sessionID, evalID, action string) error {
	ctx, done, err := e.trackEval(ctx, sessionID)
	if err != nil {
		return err
	}
	defer done()
	manager, err := e.evalHost(ctx)
	if err != nil {
		return err
	}
	switch action {
	case "reset":
		return manager.Reset(ctx, evalID, sessionID)
	case "close":
		return manager.Close(ctx, evalID, sessionID)
	default:
		return errors.New("unknown eval control action")
	}
}

func (e *Engine) EvalResults(ctx context.Context, sessionID, evalID string) ([]evalapi.Result, error) {
	if err := e.evalSessionAllowed(ctx, sessionID); err != nil {
		return nil, err
	}
	manager, err := e.evalHost(ctx)
	if err != nil {
		return nil, err
	}
	return manager.Results(ctx, evalID, sessionID)
}
func (e *Engine) EvalArtifact(ctx context.Context, sessionID, evalID, artifactID string) (evalapi.Artifact, []byte, error) {
	if err := e.evalSessionAllowed(ctx, sessionID); err != nil {
		return evalapi.Artifact{}, nil, err
	}
	manager, err := e.evalHost(ctx)
	if err != nil {
		return evalapi.Artifact{}, nil, err
	}
	return manager.Artifact(ctx, evalID, sessionID, artifactID)
}

// GrantEvalSession is a host operation. Session identity and private workspace are
// derived from Core, not accepted from the grant payload as new authority.
func (e *Engine) GrantEvalSession(ctx context.Context, ownerID, evalID string, grant evalapi.Grant) error {
	if err := e.evalSessionAllowed(ctx, ownerID); err != nil {
		return err
	}
	if err := e.evalSessionAllowed(ctx, grant.AgentID); err != nil {
		return err
	}
	manager, err := e.evalHost(ctx)
	if err != nil {
		return err
	}
	value, err := manager.Session(ctx, evalID, ownerID)
	if err != nil {
		return err
	}
	if value.OwnerID != ownerID {
		return errors.New("only the kernel owner may grant access")
	}
	// Kernel execution can read its existing heap. Explicit grants authorize that heap
	// but never the recipient's unrelated workspace or a broader tool policy.
	grant.Workspace = value.Workspace
	grant.Permission = value.Permission
	return manager.Grant(ctx, evalID, ownerID, grant)
}
func (e *Engine) RevokeEvalSession(ctx context.Context, ownerID, evalID, agentID string) error {
	if err := e.evalSessionAllowed(ctx, ownerID); err != nil {
		return err
	}
	manager, err := e.evalHost(ctx)
	if err != nil {
		return err
	}
	return manager.Revoke(ctx, evalID, ownerID, agentID)
}

type runEval struct {
	engine                     *Engine
	sessionID, parentID, runID string
	files                      *toolset.Set
}

func (a runEval) Runtimes(ctx context.Context) ([]evalapi.Manifest, error) {
	return a.engine.EvalRuntimes(ctx)
}
func (a runEval) Sessions(ctx context.Context) ([]evalapi.Session, error) {
	return a.engine.EvalSessions(ctx, a.sessionID)
}
func (a runEval) Create(ctx context.Context, id string) (evalapi.Session, error) {
	return a.engine.createEvalSession(ctx, a.sessionID, id, a.parentID)
}
func (a runEval) Execute(ctx context.Context, req evalapi.Request) (evalapi.Result, error) {
	req.RunID = a.runID
	if a.engine.evalOptions != nil {
		req.TrialID = a.engine.evalOptions.TrialID
		if a.engine.evalOptions.RunID != "" {
			req.RunID = a.engine.evalOptions.RunID
		}
	}
	caller, _ := protocol.ToolCallbackFromContext(ctx)
	if caller != nil {
		req.AllowedCapabilities = []string{"*"}
	}
	return a.engine.executeEvalWith(ctx, a.sessionID, req, caller)
}
func (a runEval) Control(ctx context.Context, id, action string) error {
	return a.engine.evalControl(ctx, a.sessionID, id, action)
}
func (a runEval) Interrupt(ctx context.Context, id string, target evalapi.InterruptTarget) (evalapi.InterruptOutcome, error) {
	return a.engine.EvalInterrupt(ctx, a.sessionID, id, target)
}
func (e *Engine) executionTools(ctx context.Context, sessionID string, opts AgentOptions, set *toolset.Set, runIDs ...string) error {
	if !opts.EvalEnabled {
		return nil
	}
	if _, err := e.evalHost(ctx); err != nil {
		return err
	}
	runID := ""
	if len(runIDs) > 0 {
		runID = runIDs[0]
	}
	return set.BindEval(runEval{engine: e, sessionID: sessionID, parentID: opts.parentSessionID, runID: runID, files: set})
}

// Track individual host Eval requests separately from the session's agent run lock:
// one agent may call Eval while running, and distinct language kernels may coexist.
func (e *Engine) trackEval(ctx context.Context, sessionID string) (context.Context, func(), error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, nil, err
	}
	key := "eval:" + sessionID + ":" + hex.EncodeToString(id[:])
	ctx, done := e.trackRun(ctx, key)
	if err := e.evalSessionAllowed(ctx, sessionID); err != nil {
		done()
		return nil, nil, err
	}
	return ctx, done, nil
}

func evalRunOwner(id string) string {
	if strings.HasPrefix(id, "eval:") {
		parts := strings.SplitN(id, ":", 3)
		if len(parts) == 3 {
			return parts[1]
		}
	}
	return id
}

// newEvalRequestID returns a fresh request id: the identity an execution-scoped interrupt names.
func newEvalRequestID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}
