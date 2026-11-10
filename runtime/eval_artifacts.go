package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"mime"
	"path/filepath"

	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/internal/toolset"
)

// TransferEvalArtifact is an explicit host operation. Workspace I/O goes through
// Core's existing protected-path resolver rather than accepting a plugin host path.
func (e *Engine) TransferEvalArtifact(ctx context.Context, sessionID, action, evalID, artifactID, path, targetID string) (evalapi.Artifact, error) {
	if err := e.evalSessionAllowed(ctx, sessionID); err != nil {
		return evalapi.Artifact{}, err
	}
	workspace, err := e.agentWorkspace(ctx, sessionID)
	if err != nil {
		return evalapi.Artifact{}, err
	}
	files, err := toolset.New(toolset.Options{Workspace: workspace, Policy: toolset.PolicyWorkspace, Protected: e.protected, SessionID: sessionID,
		Editing: e.editing,
		Events:  func(kind, sid string, payload map[string]any) { e.events.publish(kind, sid, payload) }})
	if err != nil {
		return evalapi.Artifact{}, err
	}
	return e.transferEvalArtifact(ctx, sessionID, action, evalID, artifactID, path, targetID, files)
}
func (a runEval) Transfer(ctx context.Context, action, evalID, artifactID, path, targetID string) (evalapi.Artifact, error) {
	if a.files == nil {
		return evalapi.Artifact{}, errors.New("runtime file authority unavailable")
	}
	return a.engine.transferEvalArtifact(ctx, a.sessionID, action, evalID, artifactID, path, targetID, a.files)
}
func (e *Engine) transferEvalArtifact(ctx context.Context, sessionID, action, evalID, artifactID, path, targetID string, files *toolset.Set) (evalapi.Artifact, error) {
	ctx, done, err := e.trackEval(ctx, sessionID)
	if err != nil {
		return evalapi.Artifact{}, err
	}
	defer done()
	manager, err := e.evalHost(ctx)
	if err != nil {
		return evalapi.Artifact{}, err
	}
	if _, err := manager.Session(ctx, evalID, sessionID); err != nil {
		return evalapi.Artifact{}, err
	}
	var result evalapi.Artifact
	var source string
	switch action {
	case "import":
		if path == "" {
			return result, errors.New("import requires a workspace path")
		}
		data, err := files.ReadArtifact(ctx, path, 4<<20)
		if err != nil {
			return result, err
		}
		sum := sha256.Sum256(data)
		source = "workspace-sha256:" + hex.EncodeToString(sum[:])
		kind := mime.TypeByExtension(filepath.Ext(path))
		if kind == "" {
			kind = "application/octet-stream"
		}
		result, err = manager.PutArtifact(ctx, evalID, sessionID, filepath.Base(path), kind, data)
		if err != nil {
			return result, err
		}
	case "export", "copy":
		if artifactID == "" {
			return result, errors.New("export/copy requires artifact_id")
		}
		artifact, data, err := manager.Artifact(ctx, evalID, sessionID, artifactID)
		if err != nil {
			return result, err
		}
		source = artifact.ID
		result = artifact
		if action == "export" {
			if path == "" {
				return result, errors.New("export requires a workspace path")
			}
			if err := files.WriteArtifact(ctx, path, data); err != nil {
				return result, err
			}
		} else {
			if targetID == "" {
				return result, errors.New("copy requires target_session_id")
			}
			name := filepath.Base(path)
			if path == "" {
				name = artifact.Name
			}
			result, err = manager.PutArtifact(ctx, targetID, sessionID, name, artifact.MIME, data)
			if err != nil {
				return result, err
			}
		}
	default:
		return result, errors.New("artifact action must be import, export or copy")
	}
	event := map[string]any{"sessionId": sessionID, "action": action, "evalSessionId": evalID, "targetSessionId": targetID, "source": source, "artifactId": result.ID, "sha256": result.SHA256, "bytes": result.Size}
	if err := e.store.Journal(ctx, "eval.artifact_transfer", event); err != nil {
		return result, err
	}
	e.events.publish("eval.artifact_transfer", sessionID, event)
	return result, nil
}
