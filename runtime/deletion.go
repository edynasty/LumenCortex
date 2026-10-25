package runtime

import (
	"context"
	"os"
	"path/filepath"

	"github.com/edynasty/LumenCortex/internal/session"
)

// DeletionPreview is what DeleteSession of a session would do, read before anything is written
// (plan T3-S4): the sessions it deletes, the descendants that block it, the rows and copies it removes,
// the sessions it fences, and the copies it cannot reach. Nothing in it is content.
type DeletionPreview struct {
	SessionID string `json:"sessionId"`
	// Deleted are the session and the read-only subagent descendants deleted with it. A descendant
	// running or owning a worktree when DeleteSession runs becomes a dependent instead.
	Deleted    []string           `json:"deleted"`
	Dependents []SessionDependent `json:"dependents,omitempty"`
	session.DeletionPreview
	// Outside are the copies of the content this database does not control.
	Outside []OutsideCopy `json:"outside"`
}

// OutsideCopy is one place a deletion cannot remove the content from.
type OutsideCopy struct {
	// Kind: "backup" (a pre-migration copy of the database, Path), "worktree" (the session's worktree,
	// removed or kept by WorktreeCleanup, Path), "workspace" (files and Git history the session's tools
	// wrote), "provider" (requests already sent to a model provider).
	Kind string `json:"kind"`
	Path string `json:"path,omitempty"`
}

// DeletionPreview reads what DeleteSession of sessionID would remove; it takes no lock and writes
// nothing.
func (e *Engine) DeletionPreview(ctx context.Context, sessionID string) (DeletionPreview, error) {
	if _, err := e.store.Get(ctx, sessionID); err != nil {
		return DeletionPreview{}, err
	}
	p := DeletionPreview{SessionID: sessionID, Deleted: []string{sessionID}}
	for queue := []string{sessionID}; len(queue) > 0; queue = queue[1:] {
		relations, err := e.store.ChildRelations(ctx, queue[0], MaxSubagentTreeNodes)
		if err != nil {
			return DeletionPreview{}, err
		}
		for _, r := range relations {
			if r.Kind != RelationSubagent {
				p.Dependents = append(p.Dependents, SessionDependent{SessionID: r.ChildSessionID, Kind: r.Kind, Reason: "independent"})
				continue
			}
			p.Deleted = append(p.Deleted, r.ChildSessionID)
			queue = append(queue, r.ChildSessionID)
		}
	}
	counts, err := e.store.DeletionPreview(ctx, p.Deleted)
	if err != nil {
		return DeletionPreview{}, err
	}
	p.DeletionPreview = counts
	p.Outside = []OutsideCopy{}
	backups, _ := os.ReadDir(filepath.Join(e.repoDir, "backups"))
	for _, b := range backups {
		p.Outside = append(p.Outside, OutsideCopy{Kind: "backup", Path: filepath.Join(e.repoDir, "backups", b.Name())})
	}
	for _, id := range p.Deleted {
		if identity, err := e.SessionRuntime(ctx, id); err == nil && identity.Kind == RuntimeWorktree {
			p.Outside = append(p.Outside, OutsideCopy{Kind: "worktree", Path: identity.Path})
		}
	}
	p.Outside = append(p.Outside, OutsideCopy{Kind: "workspace"}, OutsideCopy{Kind: "provider"})
	return p, nil
}
