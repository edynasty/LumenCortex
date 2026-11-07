package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

func (e *Engine) forgetEvalOwner(ctx context.Context, sessionID string) error {
	e.evalMu.Lock()
	manager := e.evalManager
	e.evalMu.Unlock()
	if manager != nil {
		if err := manager.ForgetOwner(ctx, sessionID); err != nil {
			return err
		}
	}
	if sessionID == "" || filepath.Base(sessionID) != sessionID || sessionID == "." || sessionID == ".." {
		return errors.New("invalid execution owner identity")
	}
	// This subtree is created only by CreateEvalSession, never selected by model input.
	return os.RemoveAll(filepath.Join(e.repoDir, "eval-workspaces", sessionID))
}
