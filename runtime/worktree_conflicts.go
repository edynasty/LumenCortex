package runtime

import (
	"context"
	"os"
	"sort"

	"github.com/edynasty/LumenCortex/internal/repository"
)

type RuntimeOwner struct {
	SessionID string `json:"sessionId,omitempty"`
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	Branch    string `json:"branch,omitempty"`
}

type WorktreeConflict struct {
	Path   string         `json:"path"`
	Owners []RuntimeOwner `json:"owners"`
}

func (e *Engine) WorktreeConflicts(ctx context.Context) ([]WorktreeConflict, error) {
	ownersByPath := map[string][]RuntimeOwner{}

	mainStatus, err := e.repo.GitStatus(ctx)
	if err != nil {
		return nil, err
	}
	mainOwner := RuntimeOwner{Kind: RuntimeLocal, Path: e.workspace}
	for _, file := range mainStatus.Files {
		ownersByPath[file.Path] = appendOwner(ownersByPath[file.Path], mainOwner)
	}

	sessions, err := e.store.List(ctx, 500, 0)
	if err != nil {
		return nil, err
	}
	for _, item := range sessions {
		identity, err := e.SessionRuntime(ctx, item.ID)
		if err != nil || identity.Kind != RuntimeWorktree {
			continue
		}
		if _, err := os.Stat(identity.Path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		repoService, err := repository.New(identity.Path)
		if err != nil {
			return nil, err
		}
		status, err := repoService.GitStatus(ctx)
		if err != nil {
			return nil, err
		}
		owner := RuntimeOwner{
			SessionID: item.ID,
			Kind:      RuntimeWorktree,
			Path:      identity.Path,
			Branch:    identity.Branch,
		}
		for _, file := range status.Files {
			ownersByPath[file.Path] = appendOwner(ownersByPath[file.Path], owner)
		}
	}

	conflicts := make([]WorktreeConflict, 0)
	for path, owners := range ownersByPath {
		if len(owners) < 2 {
			continue
		}
		sort.Slice(owners, func(i, j int) bool {
			if owners[i].Kind == owners[j].Kind {
				return owners[i].Path < owners[j].Path
			}
			return owners[i].Kind < owners[j].Kind
		})
		conflicts = append(conflicts, WorktreeConflict{Path: path, Owners: owners})
	}
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].Path < conflicts[j].Path })
	return conflicts, nil
}

func appendOwner(owners []RuntimeOwner, owner RuntimeOwner) []RuntimeOwner {
	for _, existing := range owners {
		if existing.Kind == owner.Kind && existing.Path == owner.Path {
			return owners
		}
	}
	return append(owners, owner)
}
