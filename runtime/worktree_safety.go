package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/runlock"
	"github.com/edynasty/LumenCortex/internal/session"
)

// Worktree ownership, leases and cleanup (ADR-0005).

var (
	ErrWorktreeBusy         = errors.New("session worktree is in use")
	ErrWorktreeDirty        = errors.New("session worktree has uncommitted changes")
	ErrWorktreeUnapplied    = errors.New("session worktree has commits that are not applied")
	ErrWorktreeOwnership    = errors.New("session worktree ownership is uncertain")
	ErrWorktreeBorrowed     = errors.New("session borrows its worktree from another session")
	ErrWorktreeMissing      = errors.New("session worktree directory is missing")
	ErrDiscardStale         = errors.New("session worktree changed since the discard was authorized")
	ErrSessionHasDependents = errors.New("session has descendant sessions that need explicit handling")
	ErrWorktreeIgnored      = errors.New("session worktree has ignored files that removal would delete")
)

// Worktree roles of a bound session.
const (
	WorktreeOwner     = "owner"
	WorktreeBorrower  = "borrower"
	WorktreeUncertain = "uncertain"
)

// WorktreeMissingError reports a binding whose directory does not exist. The host
// can detach the session with RemoveSessionWorktree, whose cleanup matrix protects a
// branch that still holds unapplied commits.
type WorktreeMissingError struct {
	SessionID    string `json:"sessionId"`
	Path         string `json:"path"`
	Branch       string `json:"branch,omitempty"`
	BranchExists bool   `json:"branchExists"`
}

func (e *WorktreeMissingError) Error() string {
	return fmt.Sprintf("%v: %s (branch %q exists: %v); detach it with RemoveSessionWorktree", ErrWorktreeMissing, e.Path, e.Branch, e.BranchExists)
}

func (e *WorktreeMissingError) Unwrap() error { return ErrWorktreeMissing }

// WorktreeInspection is what a cleanup would act on. Hosts show it before asking for
// KeepBranch or Discard; a Discard repeats its Head and StatusDigest.
type WorktreeInspection struct {
	SessionID string `json:"sessionId"`
	// Role is "" for a session without a worktree.
	Role   string `json:"role,omitempty"`
	Owner  string `json:"owner,omitempty"`
	Path   string `json:"path,omitempty"`
	Branch string `json:"branch,omitempty"`
	// Exists: the directory exists. Registered: git lists it (with the bound branch
	// when it exists).
	Exists       bool `json:"exists"`
	Registered   bool `json:"registered"`
	BranchExists bool `json:"branchExists"`
	// Head is the worktree's HEAD, or the branch tip when the directory is missing.
	Head string `json:"head,omitempty"`
	// TargetHead is the main checkout's HEAD that UnappliedCommits is measured against.
	TargetHead       string   `json:"targetHead,omitempty"`
	DirtyFiles []string `json:"dirtyFiles,omitempty"`
	// IgnoredFiles are ignored entries ("dir/" for a fully ignored directory) that git
	// reports clean but removing the directory deletes: build output, dependencies,
	// local configuration such as .env. IgnoredTruncated: the list is a prefix.
	IgnoredFiles     []string `json:"ignoredFiles,omitempty"`
	IgnoredTruncated bool     `json:"ignoredTruncated,omitempty"`
	// StatusDigest covers the dirty and ignored listings and the size, mode and mtime
	// of every listed path.
	StatusDigest     string   `json:"statusDigest,omitempty"`
	UnappliedCommits []string `json:"unappliedCommits,omitempty"`
	// Uncertain says why ownership could not be established.
	Uncertain string `json:"uncertain,omitempty"`
}

// WorktreeCleanup selects what a removal may do beyond the safe default.
type WorktreeCleanup struct {
	// KeepBranch removes the worktree directory, including its ignored files, when it
	// has no uncommitted changes, and keeps its branch; for an uncertain binding it
	// leaves the disk untouched.
	KeepBranch bool `json:"keepBranch,omitempty"`
	// Discard authorizes destroying uncommitted changes, ignored files and unapplied
	// commits. It is honoured only while the worktree still has the inspected HEAD and
	// status digest.
	Discard *WorktreeDiscard `json:"discard,omitempty"`
}

type WorktreeDiscard struct {
	ExpectHead         string `json:"expectHead"`
	ExpectStatusDigest string `json:"expectStatusDigest"`
}

// WorktreeCleanupResult reports what a cleanup did.
type WorktreeCleanupResult struct {
	// Action: "none" (no worktree), "unbound" (borrower), "retained" (uncertain
	// binding, disk untouched) or "removed".
	Action           string `json:"action"`
	Path             string `json:"path,omitempty"`
	Branch           string `json:"branch,omitempty"`
	DirectoryRemoved bool   `json:"directoryRemoved,omitempty"`
	BranchDeleted    bool   `json:"branchDeleted,omitempty"`
	BranchKept       bool   `json:"branchKept,omitempty"`
	Discarded        bool   `json:"discarded,omitempty"`
	// Commits are the unapplied commits that were kept on the branch or discarded.
	Commits []string `json:"commits,omitempty"`
	// IgnoredFiles are the ignored entries deleted with the directory.
	IgnoredFiles []string `json:"ignoredFiles,omitempty"`
}

// WorktreeRefusal is a cleanup the safe default refused. Err is one of the
// ErrWorktree* sentinels; Inspection says what would have been lost.
type WorktreeRefusal struct {
	Err        error
	Inspection WorktreeInspection
}

func (r *WorktreeRefusal) Error() string {
	in := r.Inspection
	switch {
	case errors.Is(r.Err, ErrWorktreeDirty):
		return fmt.Sprintf("%v: %d file(s) in %s, e.g. %s", r.Err, len(in.DirtyFiles), in.Path, strings.Join(firstN(in.DirtyFiles, 5), ", "))
	case errors.Is(r.Err, ErrWorktreeUnapplied):
		return fmt.Sprintf("%v: %d commit(s) on %s; keep the branch or discard explicitly", r.Err, len(in.UnappliedCommits), in.Branch)
	case errors.Is(r.Err, ErrWorktreeIgnored):
		return fmt.Sprintf("%v: %d entr(ies) in %s, e.g. %s; keep the branch or discard explicitly", r.Err, len(in.IgnoredFiles), in.Path, strings.Join(firstN(in.IgnoredFiles, 5), ", "))
	case errors.Is(r.Err, ErrWorktreeOwnership):
		return fmt.Sprintf("%v: %s (%s)", r.Err, in.Uncertain, in.Path)
	default:
		return fmt.Sprintf("%v: %s", r.Err, in.Path)
	}
}

func (r *WorktreeRefusal) Unwrap() error { return r.Err }

// SessionDependent is a descendant that blocks deleting its ancestor.
type SessionDependent struct {
	SessionID string `json:"sessionId"`
	Kind      string `json:"kind"`
	// Reason: "running", "owns_worktree", "uncertain_worktree" or "independent".
	Reason string `json:"reason"`
}

type DependentsError struct {
	SessionID  string
	Dependents []SessionDependent
}

func (e *DependentsError) Error() string {
	parts := make([]string, 0, len(e.Dependents))
	for _, d := range e.Dependents {
		parts = append(parts, d.SessionID+" ("+d.Reason+")")
	}
	return fmt.Sprintf("%v: %s", ErrSessionHasDependents, strings.Join(parts, ", "))
}

func (e *DependentsError) Unwrap() error { return ErrSessionHasDependents }

// DeleteSessionResult reports a deletion: the worktree cleanup and the temporary
// descendants deleted with the session.
type DeleteSessionResult struct {
	SessionID       string                `json:"sessionId"`
	Worktree        WorktreeCleanupResult `json:"worktree"`
	DeletedChildren []string              `json:"deletedChildren,omitempty"`
}

// worktreeRole classifies a session's worktree binding (ADR-0005 §1). Every
// worktree core creates is <worktreeRoot>/<owner id>.
func (e *Engine) worktreeRole(ctx context.Context, sessionID string, identity SessionRuntime) (role, owner, reason string, err error) {
	dir := filepath.Base(identity.Path)
	if filepath.Clean(filepath.Dir(identity.Path)) != filepath.Clean(e.worktreeRoot) {
		return WorktreeUncertain, "", "worktree is not directly under the managed root", nil
	}
	owner = identity.Owner
	if owner == "" {
		owner = dir
		if owner != sessionID {
			other, err := e.SessionRuntime(ctx, owner)
			if errors.Is(err, session.ErrNotFound) {
				return WorktreeUncertain, "", "no session owns the directory", nil
			}
			if err != nil {
				return "", "", "", err
			}
			if other.Kind != RuntimeWorktree || filepath.Clean(other.Path) != filepath.Clean(identity.Path) {
				return WorktreeUncertain, "", "the directory's session is not bound to it", nil
			}
		}
	}
	if owner != dir {
		return WorktreeUncertain, "", "the recorded owner does not match the directory", nil
	}
	if owner == sessionID {
		return WorktreeOwner, owner, "", nil
	}
	return WorktreeBorrower, owner, "", nil
}

// InspectSessionWorktree reads, without locks, what removing the session's worktree
// would affect.
func (e *Engine) InspectSessionWorktree(ctx context.Context, sessionID string) (WorktreeInspection, error) {
	identity, err := e.SessionRuntime(ctx, sessionID)
	if err != nil {
		return WorktreeInspection{}, err
	}
	return e.inspectWorktree(ctx, sessionID, identity)
}

func (e *Engine) inspectWorktree(ctx context.Context, sessionID string, identity SessionRuntime) (WorktreeInspection, error) {
	out := WorktreeInspection{SessionID: sessionID}
	if identity.Kind != RuntimeWorktree {
		return out, nil
	}
	out.Path, out.Branch = identity.Path, identity.Branch
	role, owner, reason, err := e.worktreeRole(ctx, sessionID, identity)
	if err != nil {
		return out, err
	}
	out.Role, out.Owner, out.Uncertain = role, owner, reason
	if role == WorktreeBorrower {
		return out, nil // a borrower never touches the disk
	}

	info, statErr := os.Stat(identity.Path)
	switch {
	case statErr == nil && info.IsDir():
		out.Exists = true
	case statErr == nil:
		return markUncertain(out, "the bound path is not a directory"), nil
	case !errors.Is(statErr, os.ErrNotExist):
		return out, statErr
	}
	worktrees, err := e.repo.GitWorktrees(ctx)
	if err != nil {
		return out, err
	}
	var registered *repository.GitWorktree
	for i := range worktrees {
		if samePath(worktrees[i].Path, identity.Path) {
			registered = &worktrees[i]
			break
		}
	}
	branchHead := ""
	if identity.Branch != "" {
		if branchHead, err = e.repo.GitBranchHead(ctx, identity.Branch); err != nil {
			return out, err
		}
	}
	out.BranchExists = branchHead != ""
	if out.TargetHead, err = e.repo.GitHead(ctx); err != nil {
		return out, err
	}

	if out.Exists {
		switch {
		case registered == nil:
			return markUncertain(out, "the directory is not a registered git worktree"), nil
		case registered.Detached || registered.Branch != identity.Branch:
			return markUncertain(out, fmt.Sprintf("the worktree is on %q, the binding names %q", describeBranch(*registered), identity.Branch)), nil
		}
		out.Registered = true
		out.Head = registered.Head
		worktreeRepo, err := repository.New(identity.Path)
		if err != nil {
			return out, err
		}
		status, err := worktreeRepo.GitWorktreeStatus(ctx)
		if err != nil {
			return out, err
		}
		out.DirtyFiles, out.StatusDigest = status.Files, status.Digest
		out.IgnoredFiles, out.IgnoredTruncated = status.Ignored, status.IgnoredTruncated
	} else {
		out.Registered = registered != nil
		out.Head = branchHead
	}
	if role == WorktreeUncertain {
		return out, nil
	}
	if out.Head != "" {
		if out.UnappliedCommits, err = e.repo.GitUnappliedCommits(ctx, out.TargetHead, out.Head, ""); err != nil {
			return out, err
		}
	}
	return out, nil
}

func markUncertain(in WorktreeInspection, reason string) WorktreeInspection {
	in.Role, in.Owner, in.Uncertain = WorktreeUncertain, "", reason
	return in
}

func describeBranch(w repository.GitWorktree) string {
	if w.Detached || w.Branch == "" {
		return "detached HEAD"
	}
	return w.Branch
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// cleanupPlan is the disk work a cleanup will do; it is decided before anything is
// written.
type cleanupPlan struct {
	remove, force, prune, deleteBranch bool
	result                             WorktreeCleanupResult
}

func (p cleanupPlan) touchesDisk() bool { return p.remove || p.prune || p.deleteBranch }

// planCleanup applies the ADR-0005 §5 matrix to an inspection taken under the
// exclusive lease.
func planCleanup(in WorktreeInspection, opts WorktreeCleanup) (cleanupPlan, error) {
	if opts.KeepBranch && opts.Discard != nil {
		return cleanupPlan{}, errors.New("KeepBranch and Discard are mutually exclusive")
	}
	plan := cleanupPlan{result: WorktreeCleanupResult{Path: in.Path, Branch: in.Branch}}
	refuse := func(err error) (cleanupPlan, error) {
		return cleanupPlan{}, &WorktreeRefusal{Err: err, Inspection: in}
	}
	switch in.Role {
	case "":
		plan.result.Action = "none"
		return plan, nil
	case WorktreeBorrower:
		if opts.Discard != nil {
			return refuse(ErrWorktreeBorrowed)
		}
		plan.result.Action = "unbound"
		return plan, nil
	case WorktreeUncertain:
		if !opts.KeepBranch {
			return refuse(ErrWorktreeOwnership)
		}
		plan.result.Action = "retained"
		plan.result.BranchKept = in.BranchExists
		return plan, nil
	}

	plan.result.Action = "removed"
	plan.remove, plan.prune = in.Exists, !in.Exists
	switch {
	case opts.Discard != nil:
		if opts.Discard.ExpectHead != in.Head || opts.Discard.ExpectStatusDigest != in.StatusDigest {
			return refuse(ErrDiscardStale)
		}
		plan.force = in.Exists
		plan.deleteBranch = in.BranchExists
		plan.result.Discarded = true
		plan.result.Commits = in.UnappliedCommits
	case len(in.DirtyFiles) > 0:
		return refuse(ErrWorktreeDirty)
	case len(in.UnappliedCommits) > 0 && !opts.KeepBranch:
		return refuse(ErrWorktreeUnapplied)
	case len(in.IgnoredFiles) > 0 && !opts.KeepBranch:
		// git worktree remove deletes ignored files without --force; the safe default
		// destroys nothing git cannot restore (ADR-0005 §5).
		return refuse(ErrWorktreeIgnored)
	case opts.KeepBranch:
		plan.result.BranchKept = in.BranchExists
		plan.result.Commits = in.UnappliedCommits
	default:
		plan.deleteBranch = in.BranchExists
	}
	if plan.remove {
		plan.result.IgnoredFiles = in.IgnoredFiles
	}
	return plan, nil
}

// executeCleanup does a plan's disk work. Each step is safe to repeat: a failure
// leaves the binding in place, and a retry inspects the directory as missing.
func (e *Engine) executeCleanup(ctx context.Context, plan *cleanupPlan) error {
	if !plan.touchesDisk() {
		return nil
	}
	path := plan.result.Path
	e.stopLSPWorkspace(path)
	if e.mcpRegistry != nil {
		e.mcpRegistry.StopWorkspace(path)
	}
	if plan.remove {
		if err := e.repo.GitRemoveWorktree(ctx, path, plan.force); err != nil {
			return err
		}
		plan.result.DirectoryRemoved = true
	}
	if plan.prune {
		if err := e.repo.GitPruneWorktrees(ctx); err != nil {
			return err
		}
	}
	if plan.deleteBranch {
		if err := e.repo.GitDeleteBranch(ctx, plan.result.Branch); err != nil {
			return err
		}
		plan.result.BranchDeleted = true
	}
	return nil
}

// worktreeLeaseID names the lease of the worktree at path: one lock file per
// worktree directory, beside the run locks.
func worktreeLeaseID(path string) string { return "wt_" + filepath.Base(path) }

// acquireWorktreeLease takes the worktree's lease without blocking: shared for a
// run or a host operation that writes in the worktree, exclusive for remove,
// discard and apply. A held lease → ErrWorktreeBusy.
func (e *Engine) acquireWorktreeLease(path string, exclusive bool) (*runlock.Lock, error) {
	acquire := runlock.TryAcquireShared
	if exclusive {
		acquire = runlock.TryAcquire
	}
	lease, err := acquire(e.runDir(), worktreeLeaseID(path))
	if errors.Is(err, runlock.ErrHeld) {
		return nil, ErrWorktreeBusy
	}
	return lease, err
}

// leaseWorkspace returns the session's workspace held for use. For a worktree that
// is a shared lease, taken before the binding and directory are checked, so the
// directory cannot be removed while the caller uses it. The caller releases the
// lease (nil for the main workspace).
func (e *Engine) leaseWorkspace(ctx context.Context, sessionID string) (string, *runlock.Lock, error) {
	identity, err := e.SessionRuntime(ctx, sessionID)
	if err != nil {
		return "", nil, err
	}
	if identity.Kind != RuntimeWorktree {
		return e.workspace, nil, nil
	}
	lease, err := e.acquireWorktreeLease(identity.Path, false)
	if err != nil {
		return "", nil, err
	}
	workspace, err := e.agentWorkspace(ctx, sessionID)
	if err == nil && filepath.Clean(workspace) != filepath.Clean(identity.Path) {
		err = fmt.Errorf("%w: the binding changed while the lease was taken", ErrWorktreeBusy)
	}
	if err != nil {
		_ = lease.Release()
		return "", nil, err
	}
	return workspace, lease, nil
}

// cleanupOwnedWorktree runs the locked part of a cleanup for a session whose run
// lock the caller holds: role, exclusive lease for an owner, fresh inspection,
// plan, disk work. The returned lease (nil unless an owner) is released by the
// caller after its database write.
func (e *Engine) cleanupOwnedWorktree(ctx context.Context, sessionID string, opts WorktreeCleanup) (cleanupPlan, *runlock.Lock, error) {
	identity, err := e.SessionRuntime(ctx, sessionID)
	if err != nil {
		return cleanupPlan{}, nil, err
	}
	var lease *runlock.Lock
	if identity.Kind == RuntimeWorktree {
		role, _, _, err := e.worktreeRole(ctx, sessionID, identity)
		if err != nil {
			return cleanupPlan{}, nil, err
		}
		if role == WorktreeOwner {
			if lease, err = e.acquireWorktreeLease(identity.Path, true); err != nil {
				return cleanupPlan{}, nil, err
			}
		}
	}
	fail := func(err error) (cleanupPlan, *runlock.Lock, error) {
		_ = lease.Release()
		return cleanupPlan{}, nil, err
	}
	inspection, err := e.inspectWorktree(ctx, sessionID, identity)
	if err != nil {
		return fail(err)
	}
	plan, err := planCleanup(inspection, opts)
	if err != nil {
		return fail(err)
	}
	if err := e.executeCleanup(ctx, &plan); err != nil {
		return fail(err)
	}
	return plan, lease, nil
}

// RemoveSessionWorktree unbinds the session from its worktree and, for the owner,
// removes it under the ADR-0005 §5 matrix. It takes the session's run lock
// (ErrRunHeld) and, for an owner, the worktree's exclusive lease (ErrWorktreeBusy).
func (e *Engine) RemoveSessionWorktree(ctx context.Context, sessionID string, opts WorktreeCleanup) (WorktreeCleanupResult, error) {
	lock, err := e.acquireRun(ctx, sessionID)
	if err != nil {
		return WorktreeCleanupResult{}, err
	}
	defer lock.Release()
	plan, lease, err := e.cleanupOwnedWorktree(ctx, sessionID, opts)
	if err != nil {
		return WorktreeCleanupResult{}, err
	}
	defer lease.Release()
	if plan.result.Action == "none" {
		return plan.result, nil
	}
	if err := e.store.RemoveCoreMetadataKey(ctx, sessionID, session.MetadataKeyRuntime); err != nil {
		return plan.result, err
	}
	e.events.publish("worktree.removed", sessionID, cleanupEventData(plan.result))
	_ = e.store.Journal(ctx, "worktree.removed", map[string]any{"sessionId": sessionID, "result": plan.result})
	return plan.result, nil
}

// DeleteSession deletes a session and its temporary descendants as a privacy deletion (DATA_AND_FAILURES
// §6 N7, plan T3-S4), and cleans up its worktree under the ADR-0005 §5 matrix. Locks, then preflight,
// then the worktree: a failure there leaves the session in place and a retry converges (ADR-0005 §7).
// Then one transaction commits the deletion's fences - from then on no run, read or edge uses the
// session's content, and every session that read it is fenced as by RevokeSource - and short batches
// remove every local copy of the content (session.PurgeStep). A deletion interrupted after its fences
// (a crash, a cancelled ctx, an error) is cleanup_pending: the next DeleteSession of the session, or
// the next Open, resumes it; the content is never used again in between. While another caller holds
// one of its sessions the call fails with ErrRunHeld, and while another reader keeps the WAL from
// being truncated it fails with ErrDeletionPending; either way the deletion stays pending. A nil error
// means every local copy in the database files is gone.
func (e *Engine) DeleteSession(ctx context.Context, sessionID string, opts WorktreeCleanup) (DeleteSessionResult, error) {
	// A pending deletion is resumed first: its last batches remove the session rows, so the root may
	// be gone while the file still owes its rebuild.
	pending, err := e.store.PendingPrivacyDeletion(ctx, sessionID)
	if err != nil {
		return DeleteSessionResult{}, err
	}
	if pending != nil {
		// The fences are committed and the worktree was settled before them: finish the cleanup.
		if err := e.resumeDeletion(ctx, *pending); err != nil {
			return DeleteSessionResult{}, err
		}
		result := DeleteSessionResult{SessionID: sessionID, Worktree: WorktreeCleanupResult{Action: "none"}, DeletedChildren: pending.IDs[1:]}
		_ = e.store.Journal(ctx, "session.delete", map[string]any{"sessionId": sessionID, "deletedChildren": result.DeletedChildren})
		return result, nil
	}
	lock, err := e.acquireRun(ctx, sessionID)
	if err != nil {
		return DeleteSessionResult{}, err
	}
	locks := []*runlock.Lock{lock}
	releaseAll := func() error {
		var errs []error
		for _, held := range locks {
			errs = append(errs, held.Release())
		}
		return errors.Join(errs...)
	}
	children, childLocks, dependents, err := e.temporaryDescendants(ctx, sessionID)
	locks = append(locks, childLocks...)
	if err != nil {
		return DeleteSessionResult{}, errors.Join(err, releaseAll())
	}
	if len(dependents) > 0 {
		return DeleteSessionResult{}, errors.Join(&DependentsError{SessionID: sessionID, Dependents: dependents}, releaseAll())
	}
	plan, lease, err := e.cleanupOwnedWorktree(ctx, sessionID, opts)
	if err != nil {
		return DeleteSessionResult{}, errors.Join(err, releaseAll())
	}
	ids := append([]string{sessionID}, children...)
	deletion, _, err := e.store.BeginPrivacyDeletion(ctx, ids)
	if err == nil {
		err = e.purgeDeletion(ctx, deletion)
	}
	err = errors.Join(err, lease.Release(), releaseAll())
	if err != nil {
		return DeleteSessionResult{}, err
	}
	e.removeRunLocks(ids)
	if plan.result.DirectoryRemoved || (plan.prune && plan.result.Action == "removed") {
		_ = runlock.Remove(e.runDir(), worktreeLeaseID(plan.result.Path))
	}
	result := DeleteSessionResult{SessionID: sessionID, Worktree: plan.result, DeletedChildren: children}
	_ = e.store.Journal(ctx, "session.delete", map[string]any{"sessionId": sessionID, "worktree": plan.result, "deletedChildren": children})
	return result, nil
}

// testHookPurgeStep, when set, runs after each committed PurgeStep (tests kill the process there).
var testHookPurgeStep func()

// ErrDeletionPending is returned by DeleteSession when the deletion is committed and its rows are gone
// but the cleanup could not finish - another reader keeps the WAL from being truncated, so the content
// may still be at rest in the database files. The session is fenced and never used again; the next
// DeleteSession of the session or the next Open finishes the cleanup. It is not a success.
var ErrDeletionPending = session.ErrCleanupPending

// purgeDeletion runs the cleanup of a committed deletion to its end: every batch, the rebuild and the
// WAL truncation (session.PurgeStep). The checkpoint marker is owed before the first batch, so an Open
// after a crash also retries the truncation. A reader that blocks the truncation ends it with
// ErrDeletionPending and the deletion stays cleanup_pending. The caller holds the run lock of every id.
func (e *Engine) purgeDeletion(ctx context.Context, d session.PrivacyDeletion) error {
	if err := e.store.OweCheckpoint(); err != nil {
		return err
	}
	for {
		done, err := e.store.PurgeStep(ctx, d)
		if err != nil {
			return err
		}
		if testHookPurgeStep != nil {
			testHookPurgeStep()
		}
		if done {
			return nil
		}
	}
}

// acquireDeleted takes the run locks of the descendants a pending deletion deletes. A descendant
// already gone needs none.
func (e *Engine) acquireDeleted(ctx context.Context, ids []string) ([]*runlock.Lock, error) {
	var locks []*runlock.Lock
	for _, id := range ids {
		lock, err := e.acquireRun(ctx, id)
		if errors.Is(err, session.ErrNotFound) {
			continue
		}
		if err != nil {
			return locks, err
		}
		locks = append(locks, lock)
	}
	return locks, nil
}

// removeRunLocks removes the lock files of deleted sessions.
func (e *Engine) removeRunLocks(ids []string) {
	for _, id := range ids {
		_ = runlock.Remove(e.runDir(), id)
	}
}

// resumeDeletion finishes a deletion left cleanup_pending, under the run locks of its sessions that
// still exist. A session another caller holds fails it with ErrRunHeld; the deletion stays pending.
func (e *Engine) resumeDeletion(ctx context.Context, d session.PrivacyDeletion) error {
	locks, err := e.acquireDeleted(ctx, d.IDs)
	if err == nil {
		err = e.purgeDeletion(ctx, d)
	}
	for _, lock := range locks {
		err = errors.Join(err, lock.Release())
	}
	if err != nil {
		return err
	}
	e.removeRunLocks(d.IDs)
	return nil
}

// resumeDeletions finishes, at Open, the privacy deletions an earlier process left cleanup_pending. A
// deletion whose sessions another process holds stays pending; the fences keep its content unused.
// An error leaves it pending too and does not fail Open.
func (e *Engine) resumeDeletions(ctx context.Context) {
	pending, err := e.store.PendingPrivacyDeletions(ctx)
	if err != nil {
		return
	}
	for _, d := range pending {
		_ = e.resumeDeletion(ctx, d)
	}
}

// temporaryDescendants walks the session's descendants (ADR-0005 §6). Read-only
// subagents that are not running and own no worktree are temporary: their run
// locks are returned held, for deletion with the session. Every other descendant is
// a dependent that blocks the deletion.
func (e *Engine) temporaryDescendants(ctx context.Context, sessionID string) ([]string, []*runlock.Lock, []SessionDependent, error) {
	var temporary []string
	var locks []*runlock.Lock
	var dependents []SessionDependent
	queue := []string{sessionID}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		relations, err := e.store.ChildRelations(ctx, parent, MaxSubagentTreeNodes)
		if err != nil {
			return temporary, locks, dependents, err
		}
		for _, relation := range relations {
			child := relation.ChildSessionID
			if relation.Kind != RelationSubagent {
				dependents = append(dependents, SessionDependent{SessionID: child, Kind: relation.Kind, Reason: "independent"})
				continue
			}
			childLock, err := e.acquireRun(ctx, child)
			if errors.Is(err, ErrRunHeld) {
				dependents = append(dependents, SessionDependent{SessionID: child, Kind: relation.Kind, Reason: "running"})
				continue
			}
			if err != nil {
				return temporary, locks, dependents, err
			}
			reason, err := e.descendantWorktreeReason(ctx, child)
			if err != nil || reason != "" {
				_ = childLock.Release()
				if err != nil {
					return temporary, locks, dependents, err
				}
				dependents = append(dependents, SessionDependent{SessionID: child, Kind: relation.Kind, Reason: reason})
				continue
			}
			locks = append(locks, childLock)
			temporary = append(temporary, child)
			queue = append(queue, child)
		}
	}
	return temporary, locks, dependents, nil
}

// descendantWorktreeReason is "" when the descendant has no worktree or borrows one.
func (e *Engine) descendantWorktreeReason(ctx context.Context, sessionID string) (string, error) {
	identity, err := e.SessionRuntime(ctx, sessionID)
	if err != nil || identity.Kind != RuntimeWorktree {
		return "", err
	}
	role, _, _, err := e.worktreeRole(ctx, sessionID, identity)
	switch {
	case err != nil:
		return "", err
	case role == WorktreeOwner:
		return "owns_worktree", nil
	case role == WorktreeUncertain:
		return "uncertain_worktree", nil
	}
	return "", nil
}

func cleanupEventData(result WorktreeCleanupResult) map[string]any {
	raw, _ := json.Marshal(result)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

func firstN(items []string, n int) []string {
	if len(items) > n {
		return items[:n]
	}
	return items
}
