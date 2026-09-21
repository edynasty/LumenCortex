package runtime

import (
	"context"

	"github.com/edynasty/LumenCortex/internal/repository"
)

type SearchMatch = repository.SearchMatch
type SearchResult = repository.SearchResult
type FileResult = repository.FileResult
type GitFileStatus = repository.GitFileStatus
type GitStatus = repository.GitStatus
type GitDiff = repository.GitDiff
type GitActionResult = repository.GitActionResult

func (e *Engine) SearchText(ctx context.Context, query, path string, limit int) (SearchResult, error) {
	return e.repo.SearchText(ctx, query, path, limit)
}

func (e *Engine) FindFiles(ctx context.Context, pattern, path string, limit int) (FileResult, error) {
	return e.repo.FindFiles(ctx, pattern, path, limit)
}

func (e *Engine) GitStatus(ctx context.Context) (GitStatus, error) {
	return e.repo.GitStatus(ctx)
}

func (e *Engine) GitDiff(ctx context.Context, path string, staged bool) (GitDiff, error) {
	return e.repo.GitDiff(ctx, path, staged)
}


func (e *Engine) GitStage(ctx context.Context, path string) (GitActionResult, error) {
	return e.repo.GitStage(ctx, path)
}

func (e *Engine) GitUnstage(ctx context.Context, path string) (GitActionResult, error) {
	return e.repo.GitUnstage(ctx, path)
}

func (e *Engine) GitRevert(ctx context.Context, path string) (GitActionResult, error) {
	return e.repo.GitRevert(ctx, path)
}

func (e *Engine) GitCommit(ctx context.Context, message string) (GitActionResult, error) {
	return e.repo.GitCommit(ctx, message)
}

func (e *Engine) GitPush(ctx context.Context) (GitActionResult, error) {
	return e.repo.GitPush(ctx)
}


func (e *Engine) sessionRepository(ctx context.Context, sessionID string) (*repository.Service, error) {
	workspace, err := e.agentWorkspace(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if workspace == e.workspace {
		return e.repo, nil
	}
	return repository.New(workspace)
}

func (e *Engine) SessionSearchText(ctx context.Context, sessionID, query, path string, limit int) (SearchResult, error) {
	repoService, err := e.sessionRepository(ctx, sessionID)
	if err != nil {
		return SearchResult{}, err
	}
	return repoService.SearchText(ctx, query, path, limit)
}

func (e *Engine) SessionFindFiles(ctx context.Context, sessionID, pattern, path string, limit int) (FileResult, error) {
	repoService, err := e.sessionRepository(ctx, sessionID)
	if err != nil {
		return FileResult{}, err
	}
	return repoService.FindFiles(ctx, pattern, path, limit)
}

func (e *Engine) SessionGitStatus(ctx context.Context, sessionID string) (GitStatus, error) {
	repoService, err := e.sessionRepository(ctx, sessionID)
	if err != nil {
		return GitStatus{}, err
	}
	return repoService.GitStatus(ctx)
}

func (e *Engine) SessionGitDiff(ctx context.Context, sessionID, path string, staged bool) (GitDiff, error) {
	repoService, err := e.sessionRepository(ctx, sessionID)
	if err != nil {
		return GitDiff{}, err
	}
	return repoService.GitDiff(ctx, path, staged)
}

func (e *Engine) SessionGitStage(ctx context.Context, sessionID, path string) (GitActionResult, error) {
	repoService, err := e.sessionRepository(ctx, sessionID)
	if err != nil {
		return GitActionResult{}, err
	}
	return repoService.GitStage(ctx, path)
}

func (e *Engine) SessionGitUnstage(ctx context.Context, sessionID, path string) (GitActionResult, error) {
	repoService, err := e.sessionRepository(ctx, sessionID)
	if err != nil {
		return GitActionResult{}, err
	}
	return repoService.GitUnstage(ctx, path)
}

func (e *Engine) SessionGitRevert(ctx context.Context, sessionID, path string) (GitActionResult, error) {
	repoService, err := e.sessionRepository(ctx, sessionID)
	if err != nil {
		return GitActionResult{}, err
	}
	return repoService.GitRevert(ctx, path)
}

func (e *Engine) SessionGitCommit(ctx context.Context, sessionID, message string) (GitActionResult, error) {
	repoService, err := e.sessionRepository(ctx, sessionID)
	if err != nil {
		return GitActionResult{}, err
	}
	return repoService.GitCommit(ctx, message)
}

func (e *Engine) SessionGitPush(ctx context.Context, sessionID string) (GitActionResult, error) {
	repoService, err := e.sessionRepository(ctx, sessionID)
	if err != nil {
		return GitActionResult{}, err
	}
	return repoService.GitPush(ctx)
}
