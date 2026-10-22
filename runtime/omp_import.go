package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/ompsession"
	"github.com/edynasty/LumenCortex/internal/session"
)

// OMP session import (ADR-0011). An import copies the visible history of an OMP session file, and
// of its subagent sessions, into new native sessions of this workspace. It never writes the source,
// never runs a tool, and never copies hidden reasoning. The new sessions are ordinary idle sessions:
// a Start continues one with this workspace's agent, model and tools.

// ImportSourceOMP is the source name recorded in an imported session's provenance.
const ImportSourceOMP = "omp"

// ompImporterVersion identifies the conversion rules that produced an imported transcript.
const ompImporterVersion = 1

// ompMaybeRunningWindow is how recently a session without OMP's exit marker must have been written
// for the import to warn that OMP may still be running it.
const ompMaybeRunningWindow = 10 * time.Minute

// Warning codes of an OMP import preview.
const (
	OMPWarnWorkspaceMismatch = "workspace_mismatch"
	OMPWarnMaybeRunning      = "maybe_running"
	OMPWarnOpenToolCalls     = "open_tool_calls"
	OMPWarnChildrenCapped    = "children_capped"
	OMPWarnMalformedRecords  = "malformed_records"
	OMPWarnSourceChanged     = "source_changed"
)

// OMPImportStats counts what a conversion copied, changed and left out.
type OMPImportStats = ompsession.Stats

// OMPWarning is one finding of a preview: a stable code and an English explanation.
type OMPWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// OMPImportRef is the native session an OMP session was imported into.
type OMPImportRef struct {
	SessionID  string    `json:"sessionId"`
	ImportedAt time.Time `json:"importedAt"`
	// SourceChanged is true when the source file differs from the bytes that were imported.
	SourceChanged bool `json:"sourceChanged"`
}

// OMPSessionSummary is one top-level OMP session found under the sessions directory.
type OMPSessionSummary struct {
	Path      string    `json:"path"`
	ID        string    `json:"id"`
	Title     string    `json:"title,omitempty"`
	Cwd       string    `json:"cwd"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Size      int64     `json:"size"`
	Children  int       `json:"children"`
	// WorkspaceMatch is true when the session ran in this workspace or below it.
	WorkspaceMatch bool          `json:"workspaceMatch"`
	Imported       *OMPImportRef `json:"imported,omitempty"`
}

// OMPListOptions selects sessions to list. Root "" is the OMP sessions directory
// (config.OMPAgentDir()/sessions). All lists sessions of every workspace; Query filters by title,
// id or directory; Limit defaults to 100 and is at most 500.
type OMPListOptions struct {
	Root  string `json:"root,omitempty"`
	All   bool   `json:"all,omitempty"`
	Query string `json:"query,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

// OMPChildPreview is one subagent session imported with its parent.
type OMPChildPreview struct {
	Key       string `json:"key"`
	SessionID string `json:"sessionId"`
	Agent     string `json:"agent,omitempty"`
	Goal      string `json:"goal"`
	Rows      int    `json:"rows"`
	Aborted   bool   `json:"aborted,omitempty"`
}

// OMPImportPreview is what an import of one session would write, computed without writing.
type OMPImportPreview struct {
	Source OMPSessionSummary `json:"source"`
	// SessionID is the native id the session gets (or got): derived from the OMP session id.
	SessionID     string            `json:"sessionId"`
	Goal          string            `json:"goal"`
	Model         string            `json:"model,omitempty"`
	FirstUser     string            `json:"firstUser,omitempty"`
	LastAssistant string            `json:"lastAssistant,omitempty"`
	Stats         OMPImportStats    `json:"stats"`
	OffBranch     int               `json:"offBranch"`
	Malformed     int               `json:"malformed"`
	Children      []OMPChildPreview `json:"children,omitempty"`
	// ChildrenSkipped counts subagent transcripts beyond the import's bounds.
	ChildrenSkipped int          `json:"childrenSkipped,omitempty"`
	SHA256          string       `json:"sha256"`
	Warnings        []OMPWarning `json:"warnings,omitempty"`
}

// OMPImportResult reports an import. AlreadyImported means nothing was written: the session exists
// from an earlier import (SourceChanged tells whether the source has changed since).
type OMPImportResult struct {
	SessionID       string           `json:"sessionId"`
	AlreadyImported bool             `json:"alreadyImported"`
	SourceChanged   bool             `json:"sourceChanged"`
	Sessions        int              `json:"sessions"`
	Preview         OMPImportPreview `json:"preview"`
}

// ompProvenance is the import metadata key of an imported session.
type ompProvenance struct {
	Source        string         `json:"source"`
	Importer      int            `json:"importer"`
	SessionID     string         `json:"sessionId"`
	Path          string         `json:"path"`
	Key           string         `json:"key,omitempty"`
	SHA256        string         `json:"sha256"`
	Size          int64          `json:"size"`
	ModTime       time.Time      `json:"modTime"`
	ImportedAt    time.Time      `json:"importedAt"`
	Cwd           string         `json:"cwd,omitempty"`
	Title         string         `json:"title,omitempty"`
	Agent         string         `json:"agent,omitempty"`
	Model         string         `json:"model,omitempty"`
	ParentSession string         `json:"parentSession,omitempty"`
	OpenToolCalls bool           `json:"openToolCalls,omitempty"`
	OffBranch     int            `json:"offBranch,omitempty"`
	Stats         OMPImportStats `json:"stats"`
}

// ImportedOMPSessionID is the native id of an imported OMP session: derived from the OMP session
// id, so importing the same session again finds the session instead of creating a second one.
func ImportedOMPSessionID(ompSessionID string) string {
	sum := sha256.Sum256([]byte("omp:" + ompSessionID))
	return "session_" + hex.EncodeToString(sum[:12])
}

func ompRoot(root string) (string, error) {
	if root == "" {
		dir, err := config.OMPAgentDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(dir, "sessions")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// OMPSessions lists top-level OMP sessions, newest first, each with whether it was imported here.
func (e *Engine) OMPSessions(ctx context.Context, opts OMPListOptions) ([]OMPSessionSummary, error) {
	root, err := ompRoot(opts.Root)
	if errors.Is(err, os.ErrNotExist) {
		return []OMPSessionSummary{}, nil
	}
	if err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 500)
	found, err := ompsession.List(root)
	if err != nil {
		return nil, err
	}
	query := strings.ToLower(strings.TrimSpace(opts.Query))
	out := []OMPSessionSummary{}
	for _, s := range found {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		match := e.inWorkspace(s.Cwd)
		if !opts.All && !match {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(s.Title+"\n"+s.ID+"\n"+s.Cwd), query) {
			continue
		}
		summary := ompSummary(s, match)
		ref, err := e.ompImportRef(ctx, s.ID, s.Size, "")
		if err != nil {
			return nil, err
		}
		summary.Imported = ref
		out = append(out, summary)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// PreviewOMPSession reads one top-level OMP session (path must be a session file directly in a
// project directory of root) and reports what an import would write. It writes nothing.
func (e *Engine) PreviewOMPSession(ctx context.Context, root, path string) (OMPImportPreview, error) {
	plan, err := e.planOMPImport(ctx, root, path)
	if err != nil {
		return OMPImportPreview{}, err
	}
	return plan.preview, nil
}

// ImportOMPSession imports one top-level OMP session and its subagent sessions in one transaction.
// A session imported before is left as it is (it may have been continued here) and reported as
// AlreadyImported; delete it first to import the source again.
func (e *Engine) ImportOMPSession(ctx context.Context, root, path string) (OMPImportResult, error) {
	plan, err := e.planOMPImport(ctx, root, path)
	if err != nil {
		return OMPImportResult{}, err
	}
	result := OMPImportResult{SessionID: plan.preview.SessionID, Preview: plan.preview}
	if plan.existing != nil {
		result.AlreadyImported = true
		result.SourceChanged = plan.existing.SourceChanged
		return result, nil
	}
	if err := e.store.ImportSessions(ctx, plan.items); err != nil {
		if errors.Is(err, session.ErrSessionExists) {
			result.AlreadyImported = true
			return result, nil
		}
		return OMPImportResult{}, err
	}
	result.Sessions = len(plan.items)
	payload := map[string]any{"source": ImportSourceOMP, "sessionId": result.SessionID, "sessions": result.Sessions, "rows": plan.rows}
	e.events.publish("session.imported", result.SessionID, payload)
	_ = e.store.Journal(ctx, "session.imported", payload)
	return result, nil
}

type ompPlan struct {
	preview  OMPImportPreview
	items    []session.ImportedSession
	existing *OMPImportRef
	rows     int
}

func (e *Engine) planOMPImport(ctx context.Context, root, path string) (ompPlan, error) {
	realRoot, err := ompRoot(root)
	if err != nil {
		return ompPlan{}, err
	}
	real, err := ompsession.ResolvePath(realRoot, path)
	if err != nil {
		return ompPlan{}, err
	}
	src, skipped, err := ompsession.Read(real, true)
	if err != nil {
		return ompPlan{}, err
	}
	if err := ctx.Err(); err != nil {
		return ompPlan{}, err
	}
	now := time.Now().UTC()
	match := e.inWorkspace(src.Header.Cwd)
	plan := ompPlan{}
	p := &plan.preview
	p.Source = ompSummary(ompsession.Summary{
		Path: real, ID: src.Header.ID, Title: src.Header.Title, Cwd: src.Header.Cwd,
		ModTime: src.ModTime.UTC(), Size: src.Size, Children: len(src.Children) + skipped,
	}, match)
	p.SessionID = ImportedOMPSessionID(src.Header.ID)
	p.SHA256 = src.SHA256
	p.OffBranch = src.OffBranch
	p.Malformed = src.Malformed
	p.ChildrenSkipped = skipped

	ref, err := e.ompImportRef(ctx, src.Header.ID, src.Size, src.SHA256)
	if err != nil {
		return ompPlan{}, err
	}
	plan.existing = ref
	p.Source.Imported = ref

	conv, err := ompsession.Convert(src)
	if err != nil {
		return ompPlan{}, err
	}
	p.Source.CreatedAt = conv.CreatedAt
	p.Goal, p.Model, p.FirstUser, p.LastAssistant, p.Stats = conv.Goal, conv.Model, conv.FirstUser, conv.LastAssistant, conv.Stats
	rootMeta := map[string]any{
		session.MetadataKeyImport: ompProvenanceOf(src, conv, now),
	}
	if title := strings.TrimSpace(src.Header.Title); title != "" {
		rootMeta["ui"] = map[string]any{"title": title}
	}
	rootItem, err := ompItem(p.SessionID, conv, rootMeta, nil)
	if err != nil {
		return ompPlan{}, err
	}
	plan.items = append(plan.items, rootItem)
	plan.rows += len(conv.Messages)

	malformed := src.Malformed
	var walk func(parent *ompsession.Session, parentID string) error
	walk = func(parent *ompsession.Session, parentID string) error {
		for _, child := range parent.Children {
			childConv, err := ompsession.Convert(child)
			if err != nil {
				return err
			}
			malformed += child.Malformed
			sourceID := child.Header.ID
			if sourceID == "" || sourceID == src.Header.ID {
				sourceID = src.Header.ID + "/" + child.Key
			}
			id := ImportedOMPSessionID(sourceID)
			meta := map[string]any{
				session.MetadataKeyImport:   ompProvenanceOf(child, childConv, now),
				session.MetadataKeySubagent: map[string]any{"parentSessionId": parentID, "mode": "imported"},
			}
			relationMeta, err := json.Marshal(map[string]any{"mode": "imported", "ompAgent": child.Agent, "key": child.Key})
			if err != nil {
				return err
			}
			item, err := ompItem(id, childConv, meta, &session.Relation{
				ParentSessionID: parentID, ChildSessionID: id, Kind: RelationSubagent,
				CreatedAt: childConv.CreatedAt, Metadata: relationMeta,
			})
			if err != nil {
				return err
			}
			plan.items = append(plan.items, item)
			plan.rows += len(childConv.Messages)
			p.Children = append(p.Children, OMPChildPreview{
				Key: child.Key, SessionID: id, Agent: child.Agent, Goal: childConv.Goal,
				Rows: len(childConv.Messages), Aborted: child.Aborted,
			})
			if err := walk(child, id); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(src, p.SessionID); err != nil {
		return ompPlan{}, err
	}
	p.Malformed = malformed

	if !match {
		p.Warnings = append(p.Warnings, OMPWarning{OMPWarnWorkspaceMismatch,
			fmt.Sprintf("The session ran in %s, not in this workspace; its paths and tool results describe that directory.", src.Header.Cwd)})
	}
	if src.PartialTail || (!src.Exited && now.Sub(src.ModTime) < ompMaybeRunningWindow) {
		p.Warnings = append(p.Warnings, OMPWarning{OMPWarnMaybeRunning,
			"OMP may still be running this session. The import is a snapshot of the records written so far; the running OMP task is not taken over."})
	}
	if conv.OpenToolCalls {
		p.Warnings = append(p.Warnings, OMPWarning{OMPWarnOpenToolCalls,
			"The session ended with tool calls that have no recorded result. They are marked as interrupted and are not run again."})
	}
	if skipped > 0 {
		p.Warnings = append(p.Warnings, OMPWarning{OMPWarnChildrenCapped,
			fmt.Sprintf("%d subagent transcripts exceed the import bounds (%d sessions, depth %d) and are not imported.", skipped, ompsession.MaxDescendants, ompsession.MaxDepth)})
	}
	if malformed > 0 {
		p.Warnings = append(p.Warnings, OMPWarning{OMPWarnMalformedRecords,
			fmt.Sprintf("%d records could not be read and are not imported.", malformed)})
	}
	if ref != nil && ref.SourceChanged {
		p.Warnings = append(p.Warnings, OMPWarning{OMPWarnSourceChanged,
			"The source changed after it was imported. The imported session is kept as it is; delete it to import the current source."})
	}
	return plan, nil
}

func ompItem(id string, conv ompsession.Conversion, metadata map[string]any, relation *session.Relation) (session.ImportedSession, error) {
	raw, err := json.Marshal(metadata)
	if err != nil {
		return session.ImportedSession{}, err
	}
	return session.ImportedSession{
		Session: session.Session{
			ID: id, CreatedAt: conv.CreatedAt, UpdatedAt: conv.UpdatedAt, Status: "completed",
			Goal: conv.Goal, Metadata: raw,
		},
		Messages: conv.Messages,
		Relation: relation,
	}, nil
}

func ompProvenanceOf(s *ompsession.Session, conv ompsession.Conversion, now time.Time) ompProvenance {
	return ompProvenance{
		Source: ImportSourceOMP, Importer: ompImporterVersion, SessionID: s.Header.ID, Path: s.Path, Key: s.Key,
		SHA256: s.SHA256, Size: s.Size, ModTime: s.ModTime.UTC(), ImportedAt: now, Cwd: s.Header.Cwd,
		Title: s.Header.Title, Agent: s.Agent, Model: conv.Model, ParentSession: s.Header.ParentSession,
		OpenToolCalls: conv.OpenToolCalls, OffBranch: s.OffBranch, Stats: conv.Stats,
	}
}

func ompSummary(s ompsession.Summary, match bool) OMPSessionSummary {
	return OMPSessionSummary{
		Path: s.Path, ID: s.ID, Title: s.Title, Cwd: s.Cwd, CreatedAt: s.CreatedAt, UpdatedAt: s.ModTime,
		Size: s.Size, Children: s.Children, WorkspaceMatch: match,
	}
}

// ompImportRef finds the session an OMP session was imported into, nil when there is none. The source
// counts as changed when its size differs from the imported bytes or, when sha is known, its hash.
func (e *Engine) ompImportRef(ctx context.Context, ompSessionID string, size int64, sha string) (*OMPImportRef, error) {
	id := ImportedOMPSessionID(ompSessionID)
	v, err := e.store.Get(ctx, id)
	if errors.Is(err, session.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var meta struct {
		Import *ompProvenance `json:"import"`
	}
	_ = json.Unmarshal(v.Metadata, &meta)
	ref := &OMPImportRef{SessionID: id, ImportedAt: v.CreatedAt}
	if meta.Import != nil {
		ref.ImportedAt = meta.Import.ImportedAt
		ref.SourceChanged = meta.Import.Size != size || (sha != "" && meta.Import.SHA256 != sha)
	}
	return ref, nil
}

// inWorkspace reports whether dir is the workspace or below it, comparing real paths.
func (e *Engine) inWorkspace(dir string) bool {
	if dir == "" {
		return false
	}
	clean := func(p string) string {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return real
		}
		return filepath.Clean(p)
	}
	ws, d := clean(e.workspace), clean(dir)
	rel, err := filepath.Rel(ws, d)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
