package evalruntime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/internal/safefs"
)

// maxResults bounds the per-session execution history. Results carry bounded
// output, so the history has a bounded footprint.
const maxResults = 64

// session is one persistent kernel: its private directory tree, its worker, its
// sharing grants, its bounded result and artifact indexes.
type session struct {
	m         *Manager
	id        string
	manifest  evalapi.Manifest
	readPaths []string

	ownerID    string
	agentID    string
	parentID   string
	workspace  string
	permission string
	label      string
	slot       int

	kernel      string
	workDir     string
	artifactDir string
	pendingDir  string
	homeDir     string
	tmpDir      string
	profilePath string
	controlDir  string
	dataFS      *safefs.Root
	controlFS   *safefs.Root
	spaceFS     *safefs.Root
	fsMu        sync.Mutex

	budget evalapi.Budget
	// enforcement is how this kernel's bounds are really applied on this host.
	// It is computed once, from the platform and the runtime, and reported.
	enforcement evalapi.EnforcementView
	leaseUntil  time.Time
	createdAt   time.Time

	mu            sync.Mutex
	state         string
	generation    uint64
	worker        *worker
	lastError     string
	grants        map[string]evalapi.Grant
	results       []evalapi.Result
	artifacts     map[string]evalapi.Artifact
	artifactBytes int64
	closed        bool
	// requestIDs and callbackIDs are lifetime replay guards. They are never
	// evicted: once a request or callback id is used, reusing it is refused
	// (explicitly, with an error) rather than silently re-executed after history
	// trimming. A request's record also says whether it finished, and what an
	// execution-scoped interrupt did to it, so a late interrupt is answered
	// from the record instead of acting on whatever runs now.
	requestIDs  map[string]*requestRecord
	callbackIDs map[string]struct{}

	// execSem serializes cells: one worker, one cell at a time.
	execSem chan struct{}
	// active is the cell currently admitted to this kernel, so a revocation or a
	// grant expiry can cancel it instead of only refusing future calls.
	active *executionRef
	// executing is set while Execute owns the kernel, from the moment the cell
	// is admitted to the moment its result is built. The worker-exit watcher
	// reads it so an exit that Execute is already reporting is not double
	// counted as an idle death.
	executing bool
}

func newSession(m *Manager, p *provider, opts evalapi.CreateOptions, slot int) (*session, error) {
	workspace, err := filepath.Abs(opts.Workspace)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(workspace)
	if err != nil {
		return nil, fmt.Errorf("%w: workspace %q: %v", evalapi.ErrInvalid, workspace, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: workspace %q is not a directory", evalapi.ErrInvalid, workspace)
	}
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		return nil, err
	}
	owner := strings.TrimSpace(opts.OwnerID)
	if owner == "" {
		return nil, fmt.Errorf("%w: owner identity is required", evalapi.ErrInvalid)
	}
	agent := strings.TrimSpace(opts.AgentID)
	if agent == "" {
		agent = owner
	}
	permission := strings.TrimSpace(opts.Permission)
	if permission == "" {
		permission = "workspace"
	}
	id := newID()
	// Control files are never writable by any worker. Unique directories also
	// prevent a closed session's artifacts from becoming a later kernel's data.
	kernel := filepath.Join(workspace, fmt.Sprintf("kernel-%d-%s", slot, id))
	control := filepath.Join(workspace, "control-"+id)
	s := &session{
		m:           m,
		id:          id,
		manifest:    p.manifest,
		readPaths:   append([]string(nil), p.readPaths...),
		ownerID:     owner,
		agentID:     agent,
		parentID:    strings.TrimSpace(opts.ParentAgentID),
		workspace:   workspace,
		permission:  permission,
		label:       opts.Label,
		kernel:      kernel,
		workDir:     filepath.Join(kernel, "work"),
		artifactDir: filepath.Join(control, "artifacts"),
		pendingDir:  filepath.Join(kernel, "pending"),
		homeDir:     filepath.Join(kernel, "home"),
		tmpDir:      filepath.Join(kernel, "tmp"),
		profilePath: filepath.Join(control, "sandbox.sb"),
		controlDir:  control,
		budget:      evalapi.MergeBudget(m.defaultBudget, opts.Budget),
		enforcement: enforcementView(m.sandboxBackend(), p.manifest.Language, evalapi.MergeBudget(m.defaultBudget, opts.Budget), m.memoryEnforcement),
		leaseUntil:  opts.LeaseUntil,
		createdAt:   time.Now().UTC(),
		state:       evalapi.StateCreated,
		grants:      make(map[string]evalapi.Grant),
		artifacts:   make(map[string]evalapi.Artifact),
		requestIDs:  make(map[string]*requestRecord),
		callbackIDs: make(map[string]struct{}),
		execSem:     make(chan struct{}, 1),
	}
	s.spaceFS, err = safefs.Open(workspace)
	if err != nil {
		return nil, err
	}
	if err = s.spaceFS.MkdirAll(filepath.Base(kernel)); err != nil {
		s.spaceFS.Close()
		return nil, err
	}
	if err = s.spaceFS.MkdirAll(filepath.Base(control) + "/artifacts"); err != nil {
		s.spaceFS.Close()
		return nil, err
	}
	s.controlFS, err = safefs.Open(control)
	if err != nil {
		s.spaceFS.Close()
		return nil, err
	}
	s.dataFS, err = safefs.Open(kernel)
	if err != nil {
		s.controlFS.Close()
		s.spaceFS.Close()
		return nil, err
	}
	for _, dir := range []string{"work", "pending", "home", "tmp"} {
		if err = s.dataFS.MkdirAll(dir); err != nil {
			_ = s.removeFiles()
			return nil, err
		}
	}
	if err = s.prepareIdentity(); err != nil {
		_ = s.removeFiles()
		return nil, err
	}
	meta := map[string]any{
		"id":            s.id,
		"runtime":       p.manifest.ID,
		"language":      p.manifest.Language,
		"ownerId":       s.ownerID,
		"agentId":       s.agentID,
		"parentAgentId": s.parentID,
		"workspace":     s.workspace,
		"createdAt":     s.createdAt,
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err == nil {
		err = s.controlFS.WriteFile("meta.json", data, 0600)
	}
	if err != nil {
		_ = s.removeFiles()
		return nil, err
	}
	return s, nil
}

func newID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand failing is fatal for identity; fall back to time-based id.
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

func (s *session) public() evalapi.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.publicLocked()
}

func (s *session) publicLocked() evalapi.Session {
	workerID := ""
	if s.worker != nil {
		workerID = s.worker.id()
	}
	activeRequestID := ""
	if s.active != nil {
		activeRequestID = s.active.requestID
	}
	return evalapi.Session{
		ID:              s.id,
		RuntimeID:       s.manifest.ID,
		Language:        s.manifest.Language,
		OwnerID:         s.ownerID,
		AgentID:         s.agentID,
		ParentAgentID:   s.parentID,
		Workspace:       s.kernel,
		Permission:      s.permission,
		WorkerID:        workerID,
		State:           s.state,
		Generation:      s.generation,
		Budget:          s.budget,
		LeaseUntil:      s.leaseUntil,
		CreatedAt:       s.createdAt,
		UpdatedAt:       time.Now().UTC(),
		LastError:       s.lastError,
		Label:           s.label,
		ActiveRequestID: activeRequestID,
		Enforcement:     s.enforcement,
	}
}

func (s *session) setState(state, reason string) {
	s.mu.Lock()
	s.state = state
	if reason != "" {
		s.lastError = reason
	}
	s.mu.Unlock()
	s.m.emit(evalapi.Event{Type: evalapi.EventStateChanged, SessionID: s.id, OwnerID: s.ownerID, AgentID: s.agentID,
		Generation: s.generationValue(), Data: map[string]string{"state": state, "reason": reason}})
}

func (s *session) generationValue() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation
}

func (s *session) bumpGeneration() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.generation++
	return s.generation
}

// roleFor resolves an agent's role on this kernel. Expired grants are dropped
// when they are seen, so an expired share cannot be revived by waiting.
func (s *session) roleFor(agentID string) (string, bool) {
	if agentID != "" && agentID == s.ownerID {
		return evalapi.RoleOwner, true
	}
	if agentID == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	grant, ok := s.grants[agentID]
	if !ok {
		return "", false
	}
	if !grant.ExpiresAt.IsZero() && time.Now().After(grant.ExpiresAt) {
		delete(s.grants, agentID)
		return "", false
	}
	return grant.Role, true
}

// canExecute reports whether agentID may run a cell in this kernel. A kernel is
// one actor's authority domain: only its immutable owner executes in it. A grant
// shares read access to results and artifacts (the explicit data path), never the
// owner's live heap.
func (s *session) canExecute(agentID string) bool {
	if agentID == "" {
		return false
	}
	return agentID == s.ownerID
}

func (s *session) canRead(agentID string) bool {
	_, ok := s.roleFor(agentID)
	return ok
}

// canInterrupt reports whether agentID may stop this kernel's running cell. A
// cross-domain interrupt would let another actor control the owner's process.
func (s *session) canInterrupt(agentID string) bool {
	return agentID != "" && agentID == s.ownerID
}

func (s *session) canControl(agentID string) bool {
	role, ok := s.roleFor(agentID)
	return ok && role == evalapi.RoleOwner
}

func (s *session) checkLease() error {
	if !s.leaseUntil.IsZero() && time.Now().After(s.leaseUntil) {
		return fmt.Errorf("%w: session %s lease ended at %s", evalapi.ErrExpired, s.id, s.leaseUntil.Format(time.RFC3339))
	}
	return nil
}

// workerEnv is the complete worker environment: an explicit allowlist built from
// scratch. No host variable is inherited, so credentials, SSH agents and proxy
// settings cannot leak through the environment.
func (s *session) workerEnv() []string {
	exeDir := filepath.Dir(s.manifest.Executable)
	env := []string{
		"PATH=" + exeDir + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + s.homeDir,
		"TMPDIR=" + s.tmpDir,
		"TMP=" + s.tmpDir,
		"TEMP=" + s.tmpDir,
		"LANG=en_US.UTF-8",
		"PYTHONNOUSERSITE=1",
		"PYTHONDONTWRITEBYTECODE=1",
		"PYTHONUNBUFFERED=1",
		"PYTHONIOENCODING=utf-8",
		"MPLCONFIGDIR=" + filepath.Join(s.homeDir, ".matplotlib"),
		"MPLBACKEND=Agg",
		"NODE_NO_WARNINGS=1",
		"LC_EVAL_SESSION=" + s.id,
		"LC_EVAL_WORKSPACE=" + s.workDir,
		"LC_EVAL_ARTIFACTS=" + s.pendingDir,
	}
	for _, name := range s.m.opts.Sandbox.EnvAllow {
		name = strings.TrimSpace(name)
		if name == "" || strings.ContainsAny(name, "= \t\n") {
			continue
		}
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

func (s *session) addResult(r evalapi.Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results = append(s.results, r)
	if len(s.results) > maxResults {
		s.results = append([]evalapi.Result(nil), s.results[len(s.results)-maxResults:]...)
	}
}

func (s *session) snapshotResults() []evalapi.Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]evalapi.Result(nil), s.results...)
}

func (s *session) artifactsSnapshot() []evalapi.Artifact {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]evalapi.Artifact, 0, len(s.artifacts))
	for _, a := range s.artifacts {
		out = append(out, a)
	}
	return out
}

// safeArtifactName reduces a caller-provided name to one path component.
func safeArtifactName(name string) string {
	base := filepath.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	base = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.', r == '_', r == '-', r == ' ':
			return r
		}
		return -1
	}, base)
	base = strings.ReplaceAll(base, "..", "_")
	base = strings.Trim(base, ". ")
	if base == "" {
		return "artifact"
	}
	if len(base) > 120 {
		base = base[:120]
	}
	return base
}

func mimeForName(name, fallback string) string {
	if fallback != "" {
		return fallback
	}
	if guessed := mime.TypeByExtension(filepath.Ext(name)); guessed != "" {
		return guessed
	}
	return "application/octet-stream"
}

// storeArtifact writes immutable bytes and indexes them with provenance. The
// quota is checked before anything is written.
func (s *session) storeArtifact(cellID, name, mimeType string, data []byte) (evalapi.Artifact, error) {
	s.fsMu.Lock()
	defer s.fsMu.Unlock()
	return s.storeArtifactLocked(cellID, name, mimeType, data, false)
}

func (s *session) storeArtifactLocked(cellID, name, mimeType string, data []byte, materialize bool) (evalapi.Artifact, error) {
	if int64(len(data)) > s.budget.ArtifactBytes {
		return evalapi.Artifact{}, fmt.Errorf("%w: artifact %q is %d bytes, limit is %d", evalapi.ErrLimit, name, len(data), s.budget.ArtifactBytes)
	}
	s.mu.Lock()
	if len(s.artifacts) >= 4096 || s.artifactBytes+int64(len(data)) > s.budget.ArtifactBytes {
		s.mu.Unlock()
		return evalapi.Artifact{}, fmt.Errorf("%w: artifact quota %d bytes exhausted", evalapi.ErrLimit, s.budget.ArtifactBytes)
	}
	s.mu.Unlock()
	if materialize {
		if err := s.writeInputLocked(name, data); err != nil {
			return evalapi.Artifact{}, err
		}
	}

	id := newID()
	sum := sha256.Sum256(data)
	artifact := evalapi.Artifact{
		ID:        id,
		SessionID: s.id,
		CellID:    cellID,
		RuntimeID: s.manifest.ID,
		Name:      safeArtifactName(name),
		Path:      "artifacts/" + id,
		MIME:      mimeForName(name, mimeType),
		SHA256:    hex.EncodeToString(sum[:]),
		Size:      int64(len(data)),
		CreatedAt: time.Now().UTC(),
	}
	path := "artifacts/" + id
	if err := s.controlFS.WriteFile(path, data, 0400); err != nil {
		return evalapi.Artifact{}, err
	}
	sidecar, err := json.Marshal(artifact)
	if err == nil {
		err = s.controlFS.WriteFile(path+".json", sidecar, 0400)
	}
	if err != nil {
		_ = s.controlFS.RemoveAll(path)
		return evalapi.Artifact{}, err
	}
	s.mu.Lock()
	s.artifacts[id] = artifact
	s.artifactBytes += artifact.Size
	s.mu.Unlock()
	s.m.emit(evalapi.Event{Type: evalapi.EventArtifactCreated, SessionID: s.id, OwnerID: s.ownerID, AgentID: s.agentID,
		Generation: s.generationValue(), Data: artifact})
	return artifact, nil
}

// readArtifact returns the stored bytes after verifying size and digest, so a
// file changed behind the manager's back is reported instead of trusted.
func (s *session) readArtifact(id string) (evalapi.Artifact, []byte, error) {
	s.fsMu.Lock()
	defer s.fsMu.Unlock()
	s.mu.Lock()
	artifact, ok := s.artifacts[id]
	s.mu.Unlock()
	if !ok {
		return evalapi.Artifact{}, nil, fmt.Errorf("%w: artifact %s", evalapi.ErrNotFound, id)
	}
	data, err := s.controlFS.ReadFile("artifacts/"+artifact.ID, artifact.Size)
	if err != nil {
		return evalapi.Artifact{}, nil, fmt.Errorf("%w: artifact %s cannot be verified: %v", evalapi.ErrInvalid, id, err)
	}
	if int64(len(data)) != artifact.Size {
		return evalapi.Artifact{}, nil, fmt.Errorf("%w: artifact %s size changed", evalapi.ErrInvalid, id)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != artifact.SHA256 {
		return evalapi.Artifact{}, nil, fmt.Errorf("%w: artifact %s content changed", evalapi.ErrInvalid, id)
	}
	return artifact, data, nil
}

// adoptPendingArtifacts validates and reads the same nonblocking descriptor.
// No path-based check/read race or silent unsafe-file skipping is permitted.
func (s *session) adoptPendingArtifacts(cellID string, declared []wireArtifact) ([]evalapi.Artifact, error) {
	if len(declared) > 256 {
		return nil, fmt.Errorf("%w: too many pending artifacts", evalapi.ErrLimit)
	}
	seen := make(map[string]bool, len(declared))
	var out []evalapi.Artifact
	for _, item := range declared {
		if err := artifactNameAllowed(item.Name); err != nil {
			return out, err
		}
		if seen[item.Name] {
			continue
		}
		seen[item.Name] = true
		s.fsMu.Lock()
		data, err := s.dataFS.ReadFile("pending/"+item.Name, s.budget.ArtifactBytes)
		if err == nil {
			err = s.dataFS.RemoveAll("pending/" + item.Name)
		}
		s.fsMu.Unlock()
		if err != nil {
			return out, fmt.Errorf("%w: pending artifact %q: %v", evalapi.ErrInvalid, item.Name, err)
		}
		artifact, err := s.storeArtifact(cellID, item.Name, item.MIME, data)
		if err != nil {
			return out, err
		}
		out = append(out, artifact)
	}
	return out, nil
}

func artifactNameAllowed(name string) error {
	if name == "" || name == "." || name == ".." || len(name) > 120 || strings.ContainsAny(name, "/\\\x00") {
		return fmt.Errorf("%w: artifact name must be a single filename", evalapi.ErrInvalid)
	}
	return nil
}

func (s *session) writeInput(name string, data []byte) error {
	if err := artifactNameAllowed(name); err != nil {
		return err
	}
	s.fsMu.Lock()
	defer s.fsMu.Unlock()
	return s.writeInputLocked(name, data)
}

func (s *session) writeInputLocked(name string, data []byte) error {
	mode := os.FileMode(0600)
	if s.m.opts.identity != nil {
		// Kernel is accessible only to its dedicated UID. Keep imported immutable
		// bytes root-owned but readable; the worker can replace them in its work dir.
		mode = 0644
	}
	return s.dataFS.WriteFile("work/"+name, data, mode)
}

func (s *session) prepareIdentity() error {
	identity := s.m.opts.identity
	if identity == nil {
		return nil
	}
	for _, dir := range []string{"", "work", "pending", "home", "tmp"} {
		if err := s.dataFS.ChownDir(dir, identity.uid, identity.gid); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) importArtifact(ctx context.Context, agentID, name, mimeType string, data []byte) (evalapi.Artifact, error) {
	if err := artifactNameAllowed(name); err != nil {
		return evalapi.Artifact{}, err
	}
	s.fsMu.Lock()
	defer s.fsMu.Unlock()
	if err := ctx.Err(); err != nil {
		return evalapi.Artifact{}, err
	}
	if !s.canExecute(agentID) {
		return evalapi.Artifact{}, evalapi.ErrDenied
	}
	s.mu.Lock()
	closed := s.closed || s.state == evalapi.StateStopping || s.state == evalapi.StateResetting
	s.mu.Unlock()
	if closed {
		return evalapi.Artifact{}, fmt.Errorf("%w: session is closing or resetting", evalapi.ErrInvalid)
	}
	return s.storeArtifactLocked("", name, mimeType, data, true)
}

func (s *session) resetWorkspace() error {
	s.fsMu.Lock()
	defer s.fsMu.Unlock()
	if err := s.dataFS.Close(); err != nil {
		return err
	}
	// The lifecycle owner has already waited for the old worker. Reopen only a
	// host-owned root; old generation handles stay closed permanently.
	next, err := safefs.Open(s.kernel)
	if err != nil {
		return err
	}
	s.dataFS = next
	for _, dir := range []string{"work", "pending", "home", "tmp"} {
		if err := next.RemoveAll(dir); err != nil {
			return err
		}
		if err := next.MkdirAll(dir); err != nil {
			return err
		}
	}
	return s.prepareIdentity()
}

func (s *session) closeFileHandles() error {
	s.fsMu.Lock()
	defer s.fsMu.Unlock()
	return errors.Join(s.dataFS.Close(), s.controlFS.Close(), s.spaceFS.Close())
}

func (s *session) removeFiles() error {
	s.fsMu.Lock()
	defer s.fsMu.Unlock()
	err := errors.Join(s.dataFS.Close(), s.controlFS.Close())
	err = errors.Join(err, s.spaceFS.RemoveAll(filepath.Base(s.kernel)), s.spaceFS.RemoveAll(filepath.Base(s.controlDir)))
	return errors.Join(err, s.spaceFS.Close())
}

// executionRef is one admitted cell: who runs it, in which kernel generation, how
// to stop it, and when it is over. The completion channel is what ties interrupt
// escalation and revocation to the identity of *this* execution rather than to a
// session-wide state flag.
//
// The fields below done are guarded by the session mutex. They carry the one
// cooperative interrupt an execution can have: every Interrupt call for the
// same execution joins the first one instead of signalling again, because a
// second SIGINT can land in the worker's own reply path and cost the kernel.
type executionRef struct {
	agentID    string
	generation uint64
	cancel     context.CancelFunc
	done       chan struct{}

	requestID string
	// worker is the worker that runs this cell, bound once it has started.
	worker *worker
	// sent closes once the request is about to be written to the worker; an
	// interrupt raised before that point stops the cell without running it.
	sent chan struct{}
	// started closes when the worker reports that the cell's code is running.
	// A cooperative interrupt is only signalled after it: before, the worker
	// could not attribute the signal to this cell.
	started     chan struct{}
	startedOnce sync.Once
	interrupt   *interruptOp

	// settled and killClaim decide, under the session mutex, which of two
	// racing events owns the end of this execution: Execute settling the cell's
	// outcome, or an interrupt escalation claiming the right to kill its
	// worker. Whichever comes first wins and the other gives way:
	//   - settled first: the outcome stands, the claim is refused, and no
	//     signal or kill for this execution reaches the worker again;
	//   - claimed first: Execute waits until killClaim closes (the kill has
	//     returned) and reports the kernel lost, so the next cell is admitted
	//     only after the old worker is dead and never shares it.
	settled   bool
	killClaim chan struct{}
	// resultStatus and stateStatus are how the execution ended, recorded
	// before done closes.
	resultStatus string
	stateStatus  string
}

// settle marks the outcome of ref as decided. It returns the escalation claim
// that won the race, or nil when the outcome stands.
func (s *session) settle(ref *executionRef) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref.settled = true
	return ref.killClaim
}

// claimKill reserves the right to kill ref's worker. It fails when the outcome
// has been settled, the execution is no longer the running one of its
// generation, the session is closed or no worker runs the cell yet. The caller
// must close the returned channel once its kill has returned.
func (s *session) claimKill(ref *executionRef) (*worker, chan struct{}, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := ref.worker
	if ref.settled || s.active != ref || s.generation != ref.generation || s.closed || w == nil {
		return nil, nil, false
	}
	ref.killClaim = make(chan struct{})
	return w, ref.killClaim, true
}

// interruptOp is the single cooperative interrupt of one execution. Callers
// that arrive while it is in flight wait for it and share its outcome.
type interruptOp struct {
	done chan struct{}
	err  error
}

func newExecutionRef(agentID, requestID string, cancel context.CancelFunc) *executionRef {
	return &executionRef{agentID: agentID, requestID: requestID, cancel: cancel,
		done: make(chan struct{}), sent: make(chan struct{}), started: make(chan struct{})}
}

func (r *executionRef) markStarted() {
	r.startedOnce.Do(func() { close(r.started) })
}

// interruptRequested reports whether an Interrupt has been raised for this
// execution.
func (s *session) interruptRequested(ref *executionRef) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ref.interrupt != nil
}

// cancelActive cancels the running cell when it belongs to agentID ("" means any).
func (s *session) cancelActive(agentID string) bool {
	s.mu.Lock()
	ref := s.active
	s.mu.Unlock()
	if ref == nil {
		return false
	}
	if agentID != "" && ref.agentID != agentID {
		return false
	}
	ref.cancel()
	return true
}

// roleForExpiry resolves the role and the expiry of the grant behind it. An
// owner never expires.
func (s *session) roleForExpiry(agentID string) (string, time.Time, bool) {
	role, ok := s.roleFor(agentID)
	if !ok {
		return "", time.Time{}, false
	}
	if role == evalapi.RoleOwner {
		return role, time.Time{}, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	grant, exists := s.grants[agentID]
	if !exists {
		return "", time.Time{}, false
	}
	return role, grant.ExpiresAt, true
}

// Bounds on the lifetime replay guards. Reaching a bound refuses new ids with an
// explicit error: the manager never forgets an id and later replays it.
const (
	maxTrackedRequests  = 100_000
	maxTrackedCallbacks = 500_000
)

// admitRequestID reserves a request id for the session's lifetime and returns
// its record.
func (s *session) admitRequestID(id string) (*requestRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.requestIDs[id]; exists {
		return nil, fmt.Errorf("%w: request id %q was already used in session %s", evalapi.ErrInvalid, id, s.id)
	}
	if len(s.requestIDs) >= maxTrackedRequests {
		return nil, fmt.Errorf("%w: session %s reached its request id limit; start a new kernel", evalapi.ErrLimit, s.id)
	}
	record := &requestRecord{cancel: make(chan struct{})}
	s.requestIDs[id] = record
	return record, nil
}

// requestRecord is what the session remembers of one request id. Guarded by
// the session mutex.
type requestRecord struct {
	// finished is set when the request's Execute call is over, in the same
	// critical section that stops it from being the active execution.
	finished bool
	// cancelled is set by an interrupt that found the request still queued:
	// the request is then refused at admission and never runs. cancel closes
	// at the same time, so a request waiting for the kernel returns at once.
	cancelled bool
	cancel    chan struct{}
	// interrupt is the outcome of the interrupt that acted on the request;
	// repeated interrupts for it return it again. signalled is the execution
	// an interrupt operation was started for, so a repeat that arrives before
	// that operation recorded its outcome joins it instead of reporting the
	// target finished.
	interrupt *evalapi.InterruptOutcome
	signalled *executionRef
}

// finishRequest marks id finished. The caller holds s.mu.
func (s *session) finishRequestLocked(id string) {
	if record := s.requestIDs[id]; record != nil {
		record.finished = true
	}
}

// trackCallbackID records a worker callback id for one kernel generation.
//
// A worker numbers its callbacks from its own counter, which restarts whenever
// the process restarts. The replay guard is therefore scoped to the generation
// that issued the id: a reset, timeout or crash starts a new process whose
// "c1" is a different callback, while a repeated id inside one generation is
// still refused.
func (s *session) trackCallbackID(generation uint64, id string) error {
	key := fmt.Sprintf("%d:%s", generation, id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.callbackIDs[key]; exists {
		return fmt.Errorf("duplicate tool callback id %q in kernel generation %d", id, generation)
	}
	if len(s.callbackIDs) >= maxTrackedCallbacks {
		return fmt.Errorf("callback id limit reached for session %s", s.id)
	}
	s.callbackIDs[key] = struct{}{}
	return nil
}

var errSessionClosed = errors.New("eval: session is closed")
