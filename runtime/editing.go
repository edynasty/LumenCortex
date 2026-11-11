package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/internal/editing"
)

// WriteLease is a shared-work-tree write lease: a path or directory prefix one holder may write for a
// bounded time. The collaboration layer takes one before an agent edits a path another agent could also
// edit; a commit by a different session to a leased path is refused with lease_conflict rather than
// silently overwriting it. The default execution model gives each writing agent its own work tree, so a
// lease is only needed when a work tree is really shared.
type WriteLease = editing.Lease

// WriteLeaseRequest names the lease to take, renew or release. Root defaults to the engine's workspace;
// Path is a file or, when it is a directory or ends with a separator, a whole subtree. Holder is the
// actor holding it, normally the session id that will do the writing.
type WriteLeaseRequest struct {
	Root       string `json:"root,omitempty"`
	Path       string `json:"path"`
	Holder     string `json:"holder"`
	Prefix     bool   `json:"prefix,omitempty"`
	TTLSeconds int    `json:"ttlSeconds,omitempty"`
}

// AcquireWriteLease takes or renews the lease on a path for a holder. An overlapping lease held by
// another holder is a conflict naming that holder.
func (e *Engine) AcquireWriteLease(ctx context.Context, req WriteLeaseRequest) (WriteLease, error) {
	if err := ctx.Err(); err != nil {
		return WriteLease{}, err
	}
	root, target, prefix, err := e.resolveLease(req)
	if err != nil {
		return WriteLease{}, err
	}
	return e.editing.Leases().Acquire(root, target, req.Holder, prefix, leaseTTL(req))
}

// RenewWriteLease extends a lease the holder already has.
func (e *Engine) RenewWriteLease(ctx context.Context, req WriteLeaseRequest) (WriteLease, error) {
	if err := ctx.Err(); err != nil {
		return WriteLease{}, err
	}
	if _, target, _, err := e.resolveLease(req); err != nil {
		return WriteLease{}, err
	} else {
		return e.editing.Leases().Renew(target, req.Holder, leaseTTL(req))
	}
}

// ReleaseWriteLease drops a lease the holder has. Releasing a lease nobody holds is not an error: a run
// that ends twice must not fail its second cleanup.
func (e *Engine) ReleaseWriteLease(ctx context.Context, req WriteLeaseRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, target, _, err := e.resolveLease(req)
	if err != nil {
		return err
	}
	e.editing.Leases().Release(target, req.Holder)
	return nil
}

// WriteLeases lists the active leases, expired ones already dropped.
func (e *Engine) WriteLeases(ctx context.Context) ([]WriteLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return e.editing.Leases().List(), nil
}

// resolveLease turns a request into the canonical target the engine checks commits against.
//
// A path is inside the workspace when it is lexically under Root, or when its canonical form is under
// Root's canonical form. The second case is the Target a listed lease carries: for a workspace opened
// through a symlink, Root keeps the workspace as it was spelled while Target names the real path, and
// a host renews or releases a lease by passing that Target back. A path outside the workspace's real
// directory is still refused, and the returned Root keeps its spelling.
func (e *Engine) resolveLease(req WriteLeaseRequest) (root, target string, prefix bool, err error) {
	if strings.TrimSpace(req.Path) == "" {
		return "", "", false, errors.New("a write lease needs a path")
	}
	if strings.TrimSpace(req.Holder) == "" {
		return "", "", false, errors.New("a write lease needs a holder")
	}
	root = req.Root
	if root == "" {
		root = e.workspace
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", "", false, err
	}
	target = req.Path
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	target = filepath.Clean(target)
	if !leaseContained(root, target) && !leaseContained(editing.CanonicalPath(root), editing.CanonicalPath(target)) {
		return "", "", false, fmt.Errorf("write lease path escapes the workspace: %s", req.Path)
	}
	prefix = req.Prefix || strings.HasSuffix(req.Path, string(filepath.Separator))
	if !prefix {
		if info, statErr := os.Stat(target); statErr == nil && info.IsDir() {
			prefix = true
		}
	}
	return root, editing.CanonicalPath(target), prefix, nil
}

// leaseContained reports whether target is root or a path under it.
func leaseContained(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func leaseTTL(req WriteLeaseRequest) time.Duration {
	if req.TTLSeconds <= 0 {
		return 0
	}
	return time.Duration(req.TTLSeconds) * time.Second
}

// FileRevision returns the editing revision (the rev read_file and edit events name) of a workspace
// file. A missing file has revision "" and no error; a path escaping the workspace is refused.
func (e *Engine) FileRevision(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	rev, _, err := e.editing.FileRevision(nil, path)
	return rev, err
}

// EditDiff returns the unified diff of one workspace file between two revisions that commits produced
// or replaced. An empty afterRev means the current content on disk; an empty beforeRev means the file
// before it existed. Only files a commit wrote are diffable, from a bounded recent history; a revision
// it no longer holds is refused with an editing error of code unknown_rev, never approximated.
func (e *Engine) EditDiff(ctx context.Context, path, beforeRev, afterRev string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return e.editing.Diff(nil, path, beforeRev, afterRev)
}
