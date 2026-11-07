package evalruntime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
)

// maxResults bounds the per-session execution history. Results carry bounded
// output, so the history has a bounded footprint.
const maxResults = 64

// session is one persistent kernel: its private directory tree, its worker, its
// sharing grants, its bounded result and artifact indexes.
type session struct {
	m        *Manager
	id       string
	manifest evalapi.Manifest

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

	budget     evalapi.Budget
	leaseUntil time.Time
	createdAt  time.Time

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
	// trimming.
	requestIDs  map[string]struct{}
	callbackIDs map[string]struct{}

	// execSem serializes cells: one worker, one cell at a time.
	execSem chan struct{}
	// active is the cell currently admitted to this kernel, so a revocation or a
	// grant expiry can cancel it instead of only refusing future calls.
	active *executionRef
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
	// A kernel always gets its own pre-allocated slot inside the host-provided
	// namespace: sharing a heap never shares the owner's whole private tree, and
	// every profile knows the other slots by name (see Manager.denyPathsFor).
	kernel := filepath.Join(workspace, fmt.Sprintf("kernel-%d", slot))
	s := &session{
		m:           m,
		id:          id,
		manifest:    p.manifest,
		ownerID:     owner,
		agentID:     agent,
		parentID:    strings.TrimSpace(opts.ParentAgentID),
		workspace:   workspace,
		permission:  permission,
		label:       opts.Label,
		kernel:      kernel,
		workDir:     filepath.Join(kernel, "work"),
		artifactDir: filepath.Join(kernel, "artifacts"),
		pendingDir:  filepath.Join(kernel, "artifacts", "pending"),
		homeDir:     filepath.Join(kernel, "home"),
		tmpDir:      filepath.Join(kernel, "tmp"),
		profilePath: filepath.Join(kernel, "sandbox.sb"),
		budget:      evalapi.MergeBudget(m.defaultBudget, opts.Budget),
		leaseUntil:  opts.LeaseUntil,
		createdAt:   time.Now().UTC(),
		state:       evalapi.StateCreated,
		grants:      make(map[string]evalapi.Grant),
		artifacts:   make(map[string]evalapi.Artifact),
		requestIDs:  make(map[string]struct{}),
		callbackIDs: make(map[string]struct{}),
		execSem:     make(chan struct{}, 1),
	}
	for _, dir := range []string{s.workDir, s.artifactDir, s.pendingDir, s.homeDir, s.tmpDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			_ = os.RemoveAll(kernel)
			return nil, err
		}
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
		_ = os.WriteFile(filepath.Join(kernel, "meta.json"), data, 0o600)
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
	return evalapi.Session{
		ID:            s.id,
		RuntimeID:     s.manifest.ID,
		Language:      s.manifest.Language,
		OwnerID:       s.ownerID,
		AgentID:       s.agentID,
		ParentAgentID: s.parentID,
		Workspace:     s.kernel,
		Permission:    s.permission,
		WorkerID:      workerID,
		State:         s.state,
		Generation:    s.generation,
		Budget:        s.budget,
		LeaseUntil:    s.leaseUntil,
		CreatedAt:     s.createdAt,
		UpdatedAt:     time.Now().UTC(),
		LastError:     s.lastError,
		Label:         s.label,
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

func (s *session) canExecute(agentID string) bool {
	role, ok := s.roleFor(agentID)
	return ok && (role == evalapi.RoleOwner || role == evalapi.RoleExecutor)
}

func (s *session) canRead(agentID string) bool {
	_, ok := s.roleFor(agentID)
	return ok
}

func (s *session) canInterrupt(agentID string) bool {
	return s.canExecute(agentID)
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
	if int64(len(data)) > s.budget.ArtifactBytes {
		return evalapi.Artifact{}, fmt.Errorf("%w: artifact %q is %d bytes, limit is %d", evalapi.ErrLimit, name, len(data), s.budget.ArtifactBytes)
	}
	s.mu.Lock()
	if s.artifactBytes+int64(len(data)) > s.budget.ArtifactBytes {
		s.mu.Unlock()
		return evalapi.Artifact{}, fmt.Errorf("%w: artifact quota %d bytes exhausted", evalapi.ErrLimit, s.budget.ArtifactBytes)
	}
	s.mu.Unlock()

	id := newID()
	sum := sha256.Sum256(data)
	artifact := evalapi.Artifact{
		ID:        id,
		SessionID: s.id,
		CellID:    cellID,
		RuntimeID: s.manifest.ID,
		Name:      safeArtifactName(name),
		MIME:      mimeForName(name, mimeType),
		SHA256:    hex.EncodeToString(sum[:]),
		Size:      int64(len(data)),
		CreatedAt: time.Now().UTC(),
	}
	path := filepath.Join(s.artifactDir, id)
	if err := os.WriteFile(path, data, 0o400); err != nil {
		return evalapi.Artifact{}, err
	}
	sidecar, err := json.Marshal(artifact)
	if err == nil {
		_ = os.WriteFile(path+".json", sidecar, 0o400)
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
	s.mu.Lock()
	artifact, ok := s.artifacts[id]
	s.mu.Unlock()
	if !ok {
		return evalapi.Artifact{}, nil, fmt.Errorf("%w: artifact %s", evalapi.ErrNotFound, id)
	}
	file, err := os.Open(filepath.Join(s.artifactDir, artifact.ID))
	if err != nil {
		return evalapi.Artifact{}, nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, artifact.Size+1))
	if err != nil {
		return evalapi.Artifact{}, nil, err
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

// adoptPendingArtifacts moves files a worker declared from its staging directory
// into the immutable store. Symlinks and directories are refused.
func (s *session) adoptPendingArtifacts(cellID string, declared []wireArtifact) ([]evalapi.Artifact, error) {
	seen := make(map[string]bool, len(declared))
	names := make([]wireArtifact, 0, len(declared))
	for _, item := range declared {
		name := safeArtifactName(item.Name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, wireArtifact{Name: name, MIME: item.MIME})
	}
	var out []evalapi.Artifact
	for _, item := range names {
		path := filepath.Join(s.pendingDir, item.Name)
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		if !info.Mode().IsRegular() {
			_ = os.Remove(path)
			continue
		}
		if info.Size() > s.budget.ArtifactBytes {
			_ = os.Remove(path)
			return out, fmt.Errorf("%w: artifact %q is %d bytes, limit is %d", evalapi.ErrLimit, item.Name, info.Size(), s.budget.ArtifactBytes)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		artifact, err := s.storeArtifact(cellID, item.Name, item.MIME, data)
		if err != nil {
			_ = os.Remove(path)
			return out, err
		}
		_ = os.Remove(path)
		out = append(out, artifact)
	}
	return out, nil
}

func (s *session) resetWorkspace() error {
	entries, err := os.ReadDir(s.workDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(s.workDir, entry.Name())); err != nil {
			return err
		}
	}
	entries, err = os.ReadDir(s.pendingDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		_ = os.RemoveAll(filepath.Join(s.pendingDir, entry.Name()))
	}
	return nil
}

// executionRef is one admitted cell: who runs it and how to stop it.
type executionRef struct {
	agentID    string
	generation uint64
	cancel     context.CancelFunc
}

func (s *session) setActive(ref *executionRef) {
	s.mu.Lock()
	s.active = ref
	s.mu.Unlock()
}

func (s *session) clearActive() {
	s.mu.Lock()
	s.active = nil
	s.mu.Unlock()
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

// admitRequestID reserves a request id for the session's lifetime.
func (s *session) admitRequestID(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.requestIDs[id]; exists {
		return fmt.Errorf("%w: request id %q was already used in session %s", evalapi.ErrInvalid, id, s.id)
	}
	if len(s.requestIDs) >= maxTrackedRequests {
		return fmt.Errorf("%w: session %s reached its request id limit; start a new kernel", evalapi.ErrLimit, s.id)
	}
	s.requestIDs[id] = struct{}{}
	return nil
}

// trackCallbackID records a worker callback id for the kernel's lifetime.
func (s *session) trackCallbackID(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.callbackIDs[id]; exists {
		return fmt.Errorf("duplicate tool callback id %q", id)
	}
	if len(s.callbackIDs) >= maxTrackedCallbacks {
		return fmt.Errorf("callback id limit reached for session %s", s.id)
	}
	s.callbackIDs[id] = struct{}{}
	return nil
}

var errSessionClosed = errors.New("eval: session is closed")
