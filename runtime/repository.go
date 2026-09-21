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
