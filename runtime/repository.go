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
