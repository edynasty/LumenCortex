package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/window"
	"github.com/edynasty/LumenCortex/pluginapi"
	"github.com/edynasty/LumenCortex/protocol"
)

// Session import (ADR-0011, ADR-0012). A source plugin reads another runtime's session and returns
// an ImportBundle; everything after that is Core's: bundle validation and bounds, native ids,
// idempotency, provenance, the trailing imported row and the one transaction that writes the tree.
// An import never writes the source, never runs a tool and never copies hidden reasoning. The new
// sessions are ordinary idle sessions: a Start continues one with this workspace's agent, model,
// tools and permissions.

// Warning codes of an import preview. Sources may add their own codes.
const (
	ImportWarnWorkspaceMismatch = "workspace_mismatch"
	ImportWarnMaybeRunning      = "maybe_running"
	ImportWarnOpenToolCalls     = "open_tool_calls"
	ImportWarnChildrenCapped    = "children_capped"
	ImportWarnMalformedRecords  = "malformed_records"
	ImportWarnSourceChanged     = "source_changed"
)

// Import bounds Core enforces on every bundle, whatever the source.
const (
	// MaxImportChildren bounds the subagent sessions imported with one session.
	MaxImportChildren = 100
	// MaxImportRowBytes bounds the content of one imported row.
	MaxImportRowBytes = 1 << 20
	maxImportIDBytes  = 512
	maxImportWarnings = 32
)

var (
	ErrImportSourceNotFound = errors.New("import source is not available")
	ErrInvalidImportBundle  = errors.New("import source returned an invalid bundle")
)

// ImportSource describes one running session import source.
type ImportSource = pluginapi.ImportSourceInfo

// ImportStats counts what a conversion copied, changed and left out.
type ImportStats = pluginapi.ImportStats

// ImportWarning is one finding of a preview: a stable code and an English explanation.
type ImportWarning = pluginapi.ImportWarning

// ImportRef is the native session a source session was imported into.
type ImportRef struct {
	SessionID  string    `json:"sessionId"`
	ImportedAt time.Time `json:"importedAt"`
	// SourceChanged is true when the source differs from the bytes that were imported.
	SourceChanged bool `json:"sourceChanged"`
}

// ImportSessionSummary is one top-level session of a source.
type ImportSessionSummary struct {
	Source    string    `json:"source"`
	Path      string    `json:"path"`
	ID        string    `json:"id"`
	Title     string    `json:"title,omitempty"`
	Cwd       string    `json:"cwd"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Size      int64     `json:"size"`
	Children  int       `json:"children"`
	// WorkspaceMatch is true when the session ran in this workspace or below it.
	WorkspaceMatch bool       `json:"workspaceMatch"`
	Imported       *ImportRef `json:"imported,omitempty"`
}

// ImportListOptions selects sessions to list. Root "" is the source's default directory. All lists
// sessions of every workspace; Query filters by title, id or directory; Limit defaults to 100 and is
// at most 500.
type ImportListOptions struct {
	Root  string `json:"root,omitempty"`
	All   bool   `json:"all,omitempty"`
	Query string `json:"query,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

// ImportChildPreview is one subagent session imported with its parent.
type ImportChildPreview struct {
	Key       string `json:"key"`
	SessionID string `json:"sessionId"`
	Agent     string `json:"agent,omitempty"`
	Goal      string `json:"goal"`
	Rows      int    `json:"rows"`
	Aborted   bool   `json:"aborted,omitempty"`
}

// ImportPreview is what an import of one session would write, computed without writing.
type ImportPreview struct {
	Source ImportSessionSummary `json:"source"`
	// SessionID is the native id the session gets (or got): derived from the source and its id.
	SessionID       string               `json:"sessionId"`
	Goal            string               `json:"goal"`
	Model           string               `json:"model,omitempty"`
	FirstUser       string               `json:"firstUser,omitempty"`
	LastAssistant   string               `json:"lastAssistant,omitempty"`
	Stats           ImportStats          `json:"stats"`
	OffBranch       int                  `json:"offBranch"`
	Malformed       int                  `json:"malformed"`
	Children        []ImportChildPreview `json:"children,omitempty"`
	ChildrenSkipped int                  `json:"childrenSkipped,omitempty"`
	SHA256          string               `json:"sha256"`
	Warnings        []ImportWarning      `json:"warnings,omitempty"`
}

// ImportResult reports an import. AlreadyImported means nothing was written: the session exists from
// an earlier import (SourceChanged tells whether the source has changed since).
type ImportResult struct {
	SessionID       string        `json:"sessionId"`
	AlreadyImported bool          `json:"alreadyImported"`
	SourceChanged   bool          `json:"sourceChanged"`
	Sessions        int           `json:"sessions"`
	Preview         ImportPreview `json:"preview"`
}

// importProvenance is the import metadata key of an imported session.
type importProvenance struct {
	Source        string      `json:"source"`
	Plugin        string      `json:"plugin,omitempty"`
	Importer      int         `json:"importer"`
	SessionID     string      `json:"sessionId"`
	Path          string      `json:"path"`
	Key           string      `json:"key,omitempty"`
	SHA256        string      `json:"sha256"`
	Size          int64       `json:"size"`
	ModTime       time.Time   `json:"modTime"`
	ImportedAt    time.Time   `json:"importedAt"`
	Cwd           string      `json:"cwd,omitempty"`
	Title         string      `json:"title,omitempty"`
	Agent         string      `json:"agent,omitempty"`
	Model         string      `json:"model,omitempty"`
	ParentSession string      `json:"parentSession,omitempty"`
	OpenToolCalls bool        `json:"openToolCalls,omitempty"`
	OffBranch     int         `json:"offBranch,omitempty"`
	Stats         ImportStats `json:"stats"`
}

// ImportedSessionID is the native id of an imported source session: derived from the source id and
// the session's source id, so importing the same session again finds it instead of creating a
// second one. ("omp", id) gives the ids ADR-0011 imports have always had.
func ImportedSessionID(source, sourceSessionID string) string {
	sum := sha256.Sum256([]byte(source + ":" + sourceSessionID))
	return "session_" + hex.EncodeToString(sum[:12])
}

// ImportSources lists the running session import sources by id.
func (e *Engine) ImportSources() []ImportSource {
	out := []ImportSource{}
	for _, s := range e.plugins.importSources() {
		out = append(out, s.info)
	}
	return out
}

func (e *Engine) importSource(id string) (registeredSource, error) {
	for _, s := range e.plugins.importSources() {
		if s.info.ID == id {
			return s, nil
		}
	}
	return registeredSource{}, fmt.Errorf("%w: %q", ErrImportSourceNotFound, id)
}

// ListImportSessions lists a source's top-level sessions, newest first, each with whether it was
// imported here.
func (e *Engine) ListImportSessions(ctx context.Context, source string, opts ImportListOptions) ([]ImportSessionSummary, error) {
	src, err := e.importSource(source)
	if err != nil {
		return nil, err
	}
	var found []pluginapi.ImportSourceSession
	if err := e.plugins.call(src.pluginID, func() error {
		var err error
		found, err = src.source.ListSessions(ctx, pluginapi.ImportListRequest{Root: opts.Root})
		return err
	}); err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 500)
	query := strings.ToLower(strings.TrimSpace(opts.Query))
	out := []ImportSessionSummary{}
	for _, s := range found {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		match := e.inWorkspace(s.Cwd)
		if (!opts.All && !match) || s.ID == "" {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(s.Title+"\n"+s.ID+"\n"+s.Cwd), query) {
			continue
		}
		summary := importSummary(source, s, match)
		if summary.Imported, err = e.importRef(ctx, source, s.ID, s.Size, ""); err != nil {
			return nil, err
		}
		out = append(out, summary)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// PreviewImport reads one top-level session through its source and reports what an import would
// write. It writes nothing and runs no hook.
func (e *Engine) PreviewImport(ctx context.Context, source, root, path string) (ImportPreview, error) {
	plan, err := e.planImport(ctx, source, root, path)
	if err != nil {
		return ImportPreview{}, err
	}
	return plan.preview, nil
}

// ImportSession imports one top-level session and its subagent sessions in one transaction. The
// before-session-import hooks run first and may refuse it. A session imported before is left as it
// is (it may have been continued here) and reported as AlreadyImported; delete it first to import
// the source again.
func (e *Engine) ImportSession(ctx context.Context, source, root, path string) (ImportResult, error) {
	if _, err := e.importSource(source); err != nil {
		return ImportResult{}, err
	}
	if err := e.hooks.beforeSessionImport(ctx, pluginapi.BeforeSessionImportRequest{Source: source, Workspace: e.workspace, Root: root, Path: path}); err != nil {
		return ImportResult{}, err
	}
	plan, err := e.planImport(ctx, source, root, path)
	if err != nil {
		return ImportResult{}, err
	}
	result := ImportResult{SessionID: plan.preview.SessionID, Preview: plan.preview}
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
		return ImportResult{}, err
	}
	result.Sessions = len(plan.items)
	payload := map[string]any{"source": source, "plugin": plan.pluginID, "sessionId": result.SessionID, "sessions": result.Sessions, "rows": plan.rows}
	e.events.publish("session.imported", result.SessionID, payload)
	_ = e.store.Journal(ctx, "session.imported", payload)
	return result, nil
}

type importPlan struct {
	pluginID string
	preview  ImportPreview
	items    []session.ImportedSession
	existing *ImportRef
	rows     int
}

func (e *Engine) planImport(ctx context.Context, source, root, path string) (importPlan, error) {
	src, err := e.importSource(source)
	if err != nil {
		return importPlan{}, err
	}
	var bundle *pluginapi.ImportBundle
	if err := e.plugins.call(src.pluginID, func() error {
		var err error
		bundle, err = src.source.LoadSession(ctx, pluginapi.ImportLoadRequest{Root: root, Path: path})
		return err
	}); err != nil {
		return importPlan{}, err
	}
	if err := ctx.Err(); err != nil {
		return importPlan{}, err
	}
	if bundle == nil {
		return importPlan{}, fmt.Errorf("%w: %s returned no bundle", ErrInvalidImportBundle, src.pluginID)
	}
	if err := validateImportBundle(bundle); err != nil {
		return importPlan{}, fmt.Errorf("%w from %s: %v", ErrInvalidImportBundle, src.pluginID, err)
	}

	now := time.Now().UTC()
	r := &bundle.Root
	match := e.inWorkspace(r.Cwd)
	plan := importPlan{pluginID: src.pluginID}
	p := &plan.preview
	p.Source = importSummary(source, pluginapi.ImportSourceSession{
		Path: r.Path, ID: r.SourceID, Title: r.Title, Cwd: r.Cwd, CreatedAt: r.CreatedAt,
		UpdatedAt: r.ModTime, Size: r.Size, Children: len(bundle.Children) + bundle.ChildrenSkipped,
	}, match)
	p.SessionID = ImportedSessionID(source, r.SourceID)
	p.SHA256 = r.SHA256
	p.OffBranch = r.OffBranch
	p.ChildrenSkipped = bundle.ChildrenSkipped
	if plan.existing, err = e.importRef(ctx, source, r.SourceID, r.Size, r.SHA256); err != nil {
		return importPlan{}, err
	}
	p.Source.Imported = plan.existing
	p.Goal, p.Model, p.FirstUser, p.LastAssistant = r.Goal, r.Model, r.FirstUser, r.LastAssistant

	provenance := func(t *pluginapi.ImportTranscript, stats ImportStats) importProvenance {
		return importProvenance{
			Source: source, Plugin: src.pluginID, Importer: src.info.Converter, SessionID: t.SourceID,
			Path: t.Path, Key: t.Key, SHA256: t.SHA256, Size: t.Size, ModTime: t.ModTime.UTC(),
			ImportedAt: now, Cwd: t.Cwd, Title: t.Title, Agent: t.Agent, Model: t.Model,
			ParentSession: t.ParentSession, OpenToolCalls: t.OpenToolCalls, OffBranch: t.OffBranch, Stats: stats,
		}
	}
	rootMessages, rootStats := importedRows(r)
	p.Stats = rootStats
	rootMeta := map[string]any{session.MetadataKeyImport: provenance(r, rootStats)}
	if title := strings.TrimSpace(r.Title); title != "" {
		rootMeta["ui"] = map[string]any{"title": title}
	}
	item, err := importItem(p.SessionID, r, rootMessages, rootMeta, nil)
	if err != nil {
		return importPlan{}, err
	}
	plan.items = append(plan.items, item)
	plan.rows += len(rootMessages)

	malformed := r.Malformed
	ids := map[string]string{"": p.SessionID}
	for i := range bundle.Children {
		c := &bundle.Children[i]
		id := ImportedSessionID(source, c.SourceID)
		parentID := ids[c.ParentKey]
		ids[c.Key] = id
		malformed += c.Malformed
		messages, stats := importedRows(c)
		meta := map[string]any{
			session.MetadataKeyImport:   provenance(c, stats),
			session.MetadataKeySubagent: map[string]any{"parentSessionId": parentID, "mode": "imported"},
		}
		relationMeta, err := json.Marshal(map[string]any{"mode": "imported", "source": source, "sourceAgent": c.Agent, "key": c.Key})
		if err != nil {
			return importPlan{}, err
		}
		item, err := importItem(id, c, messages, meta, &session.Relation{
			ParentSessionID: parentID, ChildSessionID: id, Kind: RelationSubagent,
			CreatedAt: c.CreatedAt, Metadata: relationMeta,
		})
		if err != nil {
			return importPlan{}, err
		}
		plan.items = append(plan.items, item)
		plan.rows += len(messages)
		p.Children = append(p.Children, ImportChildPreview{
			Key: c.Key, SessionID: id, Agent: c.Agent, Goal: c.Goal, Rows: len(messages), Aborted: c.Aborted,
		})
	}
	p.Malformed = malformed

	warn := func(code, message string) {
		p.Warnings = append(p.Warnings, ImportWarning{Code: code, Message: message})
	}
	if !match {
		warn(ImportWarnWorkspaceMismatch, fmt.Sprintf("The session ran in %s, not in this workspace; its paths and tool results describe that directory.", r.Cwd))
	}
	if bundle.MaybeRunning {
		warn(ImportWarnMaybeRunning, "The source may still be running this session. The import is a snapshot of the records written so far; the running task is not taken over.")
	}
	if r.OpenToolCalls {
		warn(ImportWarnOpenToolCalls, "The session ended with tool calls that have no recorded result. They are marked as interrupted and are not run again.")
	}
	if bundle.ChildrenSkipped > 0 {
		warn(ImportWarnChildrenCapped, fmt.Sprintf("%d subagent transcripts exceed the import bounds (%d sessions, depth %d) and are not imported.", bundle.ChildrenSkipped, MaxImportChildren, MaxSubagentTreeDepth))
	}
	if malformed > 0 {
		warn(ImportWarnMalformedRecords, fmt.Sprintf("%d records could not be read and are not imported.", malformed))
	}
	if plan.existing != nil && plan.existing.SourceChanged {
		warn(ImportWarnSourceChanged, "The source changed after it was imported. The imported session is kept as it is; delete it to import the current source.")
	}
	p.Warnings = append(p.Warnings, bundle.Warnings...)
	return plan, nil
}

// importedRows is a transcript's rows followed by Core's imported row, with Rows counting both.
func importedRows(t *pluginapi.ImportTranscript) ([]protocol.Message, ImportStats) {
	messages := make([]protocol.Message, 0, len(t.Messages)+1)
	messages = append(messages, t.Messages...)
	messages = append(messages, protocol.Message{Role: "user", Content: protocol.ImportedContent, Origin: protocol.OriginImported})
	stats := t.Stats
	stats.Rows = len(messages)
	return messages, stats
}

// validateImportBundle checks what Core relies on, whatever the source: ids, tree shape and bounds,
// row roles and origins, and that every call is answered exactly once (window.Groups), so a
// continued session sends a well-formed history and no row can pose as one the runtime wrote.
func validateImportBundle(b *pluginapi.ImportBundle) error {
	if len(b.Children) > MaxImportChildren {
		return fmt.Errorf("%d subagent sessions exceed the bound of %d", len(b.Children), MaxImportChildren)
	}
	if b.ChildrenSkipped < 0 || len(b.Warnings) > maxImportWarnings {
		return errors.New("invalid skipped count or too many warnings")
	}
	for _, w := range b.Warnings {
		if !pluginIDPattern.MatchString(w.Code) || w.Message == "" || len(w.Message) > 2000 || !utf8.ValidString(w.Message) {
			return fmt.Errorf("invalid warning %q", w.Code)
		}
	}
	if err := validateImportTranscript(&b.Root); err != nil {
		return fmt.Errorf("session %q: %w", b.Root.SourceID, err)
	}
	if b.Root.Key != "" || b.Root.ParentKey != "" {
		return errors.New("the root transcript must have no key and no parent")
	}
	depth := map[string]int{"": 0}
	sourceIDs := map[string]bool{b.Root.SourceID: true}
	for i := range b.Children {
		c := &b.Children[i]
		if err := validateImportTranscript(c); err != nil {
			return fmt.Errorf("subagent %q: %w", c.Key, err)
		}
		if c.Key == "" || len(c.Key) > maxImportIDBytes {
			return fmt.Errorf("subagent %d has an invalid key", i)
		}
		if _, dup := depth[c.Key]; dup {
			return fmt.Errorf("duplicate subagent key %q", c.Key)
		}
		parentDepth, ok := depth[c.ParentKey]
		if !ok {
			return fmt.Errorf("subagent %q names parent %q, which does not come before it", c.Key, c.ParentKey)
		}
		if parentDepth+1 > MaxSubagentTreeDepth {
			return fmt.Errorf("subagent %q is deeper than %d", c.Key, MaxSubagentTreeDepth)
		}
		depth[c.Key] = parentDepth + 1
		if sourceIDs[c.SourceID] {
			return fmt.Errorf("duplicate source session id %q", c.SourceID)
		}
		sourceIDs[c.SourceID] = true
	}
	return nil
}

var importOrigins = map[string]bool{
	"": true, protocol.OriginImportedNotice: true, protocol.OriginImportedMarker: true, protocol.OriginToolInterrupted: true,
}

// importOriginRole is the role each non-empty importable origin must be on.
var importOriginRole = map[string]string{
	protocol.OriginImportedNotice: "user", protocol.OriginImportedMarker: "user", protocol.OriginToolInterrupted: "tool",
}

func validateImportTranscript(t *pluginapi.ImportTranscript) error {
	if t.SourceID == "" || len(t.SourceID) > maxImportIDBytes || !utf8.ValidString(t.SourceID) || strings.ContainsRune(t.SourceID, 0) {
		return errors.New("invalid source session id")
	}
	entries := make([]window.Entry, 0, len(t.Messages)+1)
	for i, m := range t.Messages {
		switch m.Role {
		case "user", "assistant", "tool":
		default:
			return fmt.Errorf("row %d: role %q is not importable", i, m.Role)
		}
		if !importOrigins[m.Origin] {
			return fmt.Errorf("row %d: origin %q is not importable", i, m.Origin)
		}
		if want := importOriginRole[m.Origin]; want != "" && m.Role != want {
			return fmt.Errorf("row %d: origin %q belongs on a %s row", i, m.Origin, want)
		}
		if len(m.Content) > MaxImportRowBytes || !utf8.ValidString(m.Content) {
			return fmt.Errorf("row %d: content is not valid UTF-8 or exceeds %d bytes", i, MaxImportRowBytes)
		}
		for _, call := range m.ToolCalls {
			if call.ID == "" || call.Name == "" || (len(call.Arguments) > 0 && !json.Valid(call.Arguments)) {
				return fmt.Errorf("row %d: invalid tool call", i)
			}
		}
		entries = append(entries, window.Entry{Seq: int64(i), Msg: m})
	}
	entries = append(entries, window.Entry{Seq: int64(len(t.Messages)), Msg: protocol.Message{Role: "user", Content: protocol.ImportedContent, Origin: protocol.OriginImported}})
	if _, err := window.Groups(entries); err != nil {
		return err
	}
	return nil
}

func importItem(id string, t *pluginapi.ImportTranscript, messages []protocol.Message, metadata map[string]any, relation *session.Relation) (session.ImportedSession, error) {
	raw, err := json.Marshal(metadata)
	if err != nil {
		return session.ImportedSession{}, err
	}
	goal := strings.TrimSpace(t.Goal)
	if goal == "" {
		goal = "Imported session"
	}
	created, updated := t.CreatedAt.UTC(), t.UpdatedAt.UTC()
	if created.IsZero() {
		created = t.ModTime.UTC()
	}
	if updated.Before(created) {
		updated = created
	}
	return session.ImportedSession{
		Session: session.Session{
			ID: id, CreatedAt: created, UpdatedAt: updated, Status: "completed", Goal: goal, Metadata: raw,
		},
		Messages: messages,
		Relation: relation,
	}, nil
}

func importSummary(source string, s pluginapi.ImportSourceSession, match bool) ImportSessionSummary {
	return ImportSessionSummary{
		Source: source, Path: s.Path, ID: s.ID, Title: s.Title, Cwd: s.Cwd, CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt, Size: s.Size, Children: s.Children, WorkspaceMatch: match,
	}
}

// importRef finds the session a source session was imported into, nil when there is none. The
// source counts as changed when its size differs from the imported bytes or, when sha is known, its
// hash.
func (e *Engine) importRef(ctx context.Context, source, sourceSessionID string, size int64, sha string) (*ImportRef, error) {
	id := ImportedSessionID(source, sourceSessionID)
	v, err := e.store.Get(ctx, id)
	if errors.Is(err, session.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var meta struct {
		Import *importProvenance `json:"import"`
	}
	_ = json.Unmarshal(v.Metadata, &meta)
	ref := &ImportRef{SessionID: id, ImportedAt: v.CreatedAt}
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
