package ompsession

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/pluginapi"
)

// SourceID is the import source id of OMP sessions. It seeds native session ids and is recorded in
// provenance, so it never changes.
const SourceID = "omp"

// converterVersion identifies the conversion rules that produced an imported transcript.
const converterVersion = 1

// maybeRunningWindow is how recently a session without OMP's exit marker must have been written for
// the import to warn that OMP may still be running it.
const maybeRunningWindow = 10 * time.Minute

// Plugin is the built-in OMP session import source (ADR-0011, ADR-0012). It reads OMP's JSONL
// session files read-only and converts their visible history; Core writes the result.
type Plugin struct{}

var _ pluginapi.SessionImportSource = Plugin{}

func (Plugin) Manifest() pluginapi.Manifest {
	return pluginapi.Manifest{
		ID: "omp-import", Name: "OMP session import", Version: "1.0.0",
		Description:  "Reads oh-my-pi (OMP) JSONL sessions read-only and imports their visible history, including subagent sessions.",
		Icon:         "import",
		APIVersion:   pluginapi.APIVersion,
		Capabilities: []string{pluginapi.CapabilitySessionImport},
	}
}

func (Plugin) Start(context.Context, pluginapi.Host) error { return nil }
func (Plugin) Stop(context.Context) error                  { return nil }

func (Plugin) ImportSource() pluginapi.ImportSourceInfo {
	root, _ := DefaultRoot()
	return pluginapi.ImportSourceInfo{ID: SourceID, Name: "OMP", Converter: converterVersion, DefaultRoot: root}
}

// DefaultRoot is OMP's sessions directory: <config.OMPAgentDir()>/sessions.
func DefaultRoot() (string, error) {
	dir, err := config.OMPAgentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sessions"), nil
}

func resolveRoot(root string) (string, error) {
	if root == "" {
		var err error
		if root, err = DefaultRoot(); err != nil {
			return "", err
		}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func (Plugin) ListSessions(ctx context.Context, req pluginapi.ImportListRequest) ([]pluginapi.ImportSourceSession, error) {
	root, err := resolveRoot(req.Root)
	if errors.Is(err, os.ErrNotExist) {
		return []pluginapi.ImportSourceSession{}, nil
	}
	if err != nil {
		return nil, err
	}
	found, err := List(root)
	if err != nil {
		return nil, err
	}
	out := make([]pluginapi.ImportSourceSession, 0, len(found))
	for _, s := range found {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out = append(out, pluginapi.ImportSourceSession{
			Path: s.Path, ID: s.ID, Title: s.Title, Cwd: s.Cwd, CreatedAt: s.CreatedAt,
			UpdatedAt: s.ModTime, Size: s.Size, Children: s.Children,
		})
	}
	return out, nil
}

func (Plugin) LoadSession(ctx context.Context, req pluginapi.ImportLoadRequest) (*pluginapi.ImportBundle, error) {
	root, err := resolveRoot(req.Root)
	if err != nil {
		return nil, err
	}
	real, err := ResolvePath(root, req.Path)
	if err != nil {
		return nil, err
	}
	src, skipped, err := Read(real, true)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rootTranscript, err := transcript(src, src.Header.ID, "")
	if err != nil {
		return nil, err
	}
	bundle := &pluginapi.ImportBundle{
		Root:            rootTranscript,
		ChildrenSkipped: skipped,
		MaybeRunning:    src.PartialTail || (!src.Exited && time.Since(src.ModTime) < maybeRunningWindow),
	}
	var walk func(parent *Session, parentKey string) error
	walk = func(parent *Session, parentKey string) error {
		for _, child := range parent.Children {
			if err := ctx.Err(); err != nil {
				return err
			}
			sourceID := child.Header.ID
			if sourceID == "" || sourceID == src.Header.ID {
				sourceID = src.Header.ID + "/" + child.Key
			}
			t, err := transcript(child, sourceID, parentKey)
			if err != nil {
				return err
			}
			bundle.Children = append(bundle.Children, t)
			if err := walk(child, child.Key); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(src, ""); err != nil {
		return nil, err
	}
	return bundle, nil
}

func transcript(s *Session, sourceID, parentKey string) (pluginapi.ImportTranscript, error) {
	conv, err := Convert(s)
	if err != nil {
		return pluginapi.ImportTranscript{}, err
	}
	return pluginapi.ImportTranscript{
		SourceID: sourceID, Key: s.Key, ParentKey: parentKey, Path: s.Path, Title: s.Header.Title,
		Cwd: s.Header.Cwd, Agent: s.Agent, ParentSession: s.Header.ParentSession,
		SHA256: s.SHA256, Size: s.Size, ModTime: s.ModTime.UTC(), Aborted: s.Aborted, Exited: s.Exited,
		Goal: conv.Goal, Model: conv.Model, FirstUser: conv.FirstUser, LastAssistant: conv.LastAssistant,
		Messages: conv.Messages, Stats: conv.Stats, OpenToolCalls: conv.OpenToolCalls,
		OffBranch: s.OffBranch, Malformed: s.Malformed, CreatedAt: conv.CreatedAt, UpdatedAt: conv.UpdatedAt,
	}, nil
}
