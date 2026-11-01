package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/internal/notebook"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/protocol"
)

// The agent's working notebook (ADR-0020). A notebook is a Markdown document the agent maintains for
// itself - Plan, Checklist, Findings, Experiments, Decisions, Failures, Verification, Handoff - and
// the host may read and edit. It is external working memory, not Task State: no runtime decision
// depends on it, and the context carries only a short index of it.
type (
	// NotebookInfo is one notebook without its body.
	NotebookInfo = session.NotebookInfo
	// NotebookDoc is one notebook with its Markdown body.
	NotebookDoc = session.NotebookDoc
	// NotebookRevision is one kept version of a notebook; Body is empty in a list.
	NotebookRevision = session.NotebookRevision
	// NotebookConflict is a lost compare-and-set: the notebook is at CurrentVersion and nothing was
	// written. errors.Is(err, ErrNotebookConflict) reports the conflict.
	NotebookConflict = session.NotebookConflict
)

var (
	// ErrNotebookConflict reports that a notebook write expected a version the notebook no longer has.
	ErrNotebookConflict = session.ErrNotebookVersionConflict
	// ErrNotebookNotFound reports that the session holds no notebook with that id or title.
	ErrNotebookNotFound = session.ErrNotebookNotFound
)

// Notebook limits, as the store enforces them (ADR-0020).
const (
	MaxNotebookBytes       = session.MaxNotebookBytes
	MaxNotebookTitleChars  = session.MaxNotebookTitleChars
	MaxNotebooksPerSession = session.MaxNotebooksPerSession
	MaxNotebookRevisions   = session.MaxNotebookRevisions
)

// Notebooks lists the working notebooks of a session, newest first, without their bodies.
func (e *Engine) Notebooks(ctx context.Context, sessionID string) ([]NotebookInfo, error) {
	if _, err := e.store.Get(ctx, sessionID); err != nil {
		return nil, err
	}
	return e.store.ListNotebooks(ctx, sessionID)
}

// Notebook returns one notebook of a session by id or title, with its body.
func (e *Engine) Notebook(ctx context.Context, sessionID, id string) (NotebookDoc, error) {
	return e.store.GetNotebook(ctx, sessionID, id)
}

// CreateNotebook creates a notebook of the session with body as version 1, attributed to the host.
func (e *Engine) CreateNotebook(ctx context.Context, sessionID, title, body string) (NotebookDoc, error) {
	doc, err := e.store.CreateNotebook(ctx, sessionID, title, body, "host")
	if err != nil {
		return NotebookDoc{}, err
	}
	e.notebookUpdated(ctx, sessionID, doc)
	return doc, nil
}

// SaveNotebook replaces the body of one notebook of the session, attributed to the host. The write
// applies only to expectedVersion; a notebook at another version is refused with a *NotebookConflict
// (errors.Is(err, ErrNotebookConflict)) that carries the current version, and nothing is written.
func (e *Engine) SaveNotebook(ctx context.Context, sessionID, id, body string, expectedVersion int64) (NotebookDoc, error) {
	doc, err := e.store.UpdateNotebook(ctx, sessionID, id, "host", "replace", expectedVersion,
		func(string) (string, error) { return body, nil })
	if err != nil {
		return NotebookDoc{}, err
	}
	e.notebookUpdated(ctx, sessionID, doc)
	return doc, nil
}

// NotebookRevisions lists the kept versions of one notebook, newest first, without their bodies.
func (e *Engine) NotebookRevisions(ctx context.Context, sessionID, id string) ([]NotebookRevision, error) {
	return e.store.NotebookRevisions(ctx, sessionID, id)
}

// NotebookRevision returns one kept version of a notebook, with its body.
func (e *Engine) NotebookRevision(ctx context.Context, sessionID, id string, version int64) (NotebookRevision, error) {
	return e.store.NotebookRevision(ctx, sessionID, id, version)
}

// DeleteNotebook deletes one notebook of the session and every kept revision of it.
func (e *Engine) DeleteNotebook(ctx context.Context, sessionID, id string) error {
	deleted, err := e.store.DeleteNotebook(ctx, sessionID, id)
	if err != nil {
		return err
	}
	e.events.publish("notebook.deleted", sessionID, map[string]any{
		"notebookId": deleted.ID, "version": deleted.Version, "bytes": deleted.Bytes,
	})
	_ = e.store.Journal(context.WithoutCancel(ctx), "notebook.deleted", map[string]any{
		"sessionId": sessionID, "id": deleted.ID, "version": deleted.Version, "bytes": deleted.Bytes,
	})
	return nil
}

// ExportNotebook returns one notebook as a Markdown file and the file name a host should save it as.
// The name is derived from the title and bounded; the body is returned unchanged apart from a final
// newline.
func (e *Engine) ExportNotebook(ctx context.Context, sessionID, id string) (markdown, filename string, err error) {
	doc, err := e.store.GetNotebook(ctx, sessionID, id)
	if err != nil {
		return "", "", err
	}
	body := doc.Body
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return body, notebookFilename(doc.Title), nil
}

// notebookUpdated publishes and journals one notebook write. Neither carries the body: the event is
// what a host refreshes on, the journal row is the content-free durable trace.
func (e *Engine) notebookUpdated(ctx context.Context, sessionID string, doc NotebookDoc) {
	e.events.publish("notebook.updated", sessionID, map[string]any{
		"notebookId": doc.ID, "version": doc.Version, "bytes": doc.Bytes,
	})
	_ = e.store.Journal(context.WithoutCancel(ctx), "notebook.updated", map[string]any{
		"sessionId": sessionID, "id": doc.ID, "version": doc.Version, "bytes": doc.Bytes,
	})
}

// notebookFilename derives an export file name from a notebook title: letters, digits, ".", "-" and
// "_" are kept, a space becomes "-", everything else is dropped; at most 40 characters.
func notebookFilename(title string) string {
	var b strings.Builder
	for _, r := range title {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
		if b.Len() >= 40 {
			break
		}
	}
	name := strings.Trim(b.String(), "-._")
	if name == "" {
		name = "notebook"
	}
	return name + ".md"
}

// notebookAccess is the notebook back end of one run: its session and its run id (the author of a
// write). A run reads its own notebooks and those of its direct children, and writes only its own.
type notebookAccess struct {
	engine    *Engine
	sessionID string
	runID     string
}

// newNotebookAccess binds the notebook tools of a run.
func newNotebookAccess(e *Engine, sessionID, runID string) notebookAccess {
	return notebookAccess{engine: e, sessionID: sessionID, runID: runID}
}

// target resolves the session a read names. Only the run's own session and its direct children are
// reachable; anything else is refused with a reason the model can act on.
func (a notebookAccess) target(ctx context.Context, sessionID string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || sessionID == a.sessionID {
		return a.sessionID, nil
	}
	child, err := a.engine.store.IsDirectChild(ctx, a.sessionID, sessionID)
	if err != nil {
		return "", err
	}
	if !child {
		return "", &toolset.NotebookProblem{Reason: fmt.Sprintf(
			"session %q is not your own session or a direct child subagent of it; a run reads only its own notebooks and those of its children", sessionID)}
	}
	return sessionID, nil
}

// List implements toolset.NotebookStore.
func (a notebookAccess) List(ctx context.Context, sessionID string) (any, error) {
	target, err := a.target(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	infos, err := a.engine.store.ListNotebooks(ctx, target)
	if err != nil {
		return nil, err
	}
	entries := make([]notebookListEntry, 0, len(infos))
	for _, info := range infos {
		entries = append(entries, notebookListEntry{
			Title: info.Title, ID: info.ID, Version: info.Version, Bytes: info.Bytes,
			UpdatedAt: info.UpdatedAt.UTC().Format(time.RFC3339), UpdatedBy: info.UpdatedBy,
			Sections: info.Sections,
		})
	}
	return map[string]any{"session": target, "notebooks": entries}, nil
}

// notebookListEntry is one line of notebook_list. Sections are the "## " headings, so the model can
// ask for exactly one of them instead of the whole body.
type notebookListEntry struct {
	Title     string   `json:"title"`
	ID        string   `json:"id"`
	Version   int64    `json:"version"`
	Bytes     int      `json:"bytes"`
	UpdatedAt string   `json:"updatedAt"`
	UpdatedBy string   `json:"updatedBy"`
	Sections  []string `json:"sections"`
}

// Read implements toolset.NotebookStore.
func (a notebookAccess) Read(ctx context.Context, sessionID, ref, section string) (any, error) {
	target, err := a.target(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	doc, err := a.engine.store.GetNotebook(ctx, target, ref)
	if err != nil {
		return nil, notebookProblem(err)
	}
	out := map[string]any{
		"title": doc.Title, "id": doc.ID, "version": doc.Version, "bytes": doc.Bytes,
	}
	if section == "" {
		out["body"] = doc.Body
		return out, nil
	}
	text, ok := notebook.SectionText(doc.Body, section)
	if !ok {
		return nil, &toolset.NotebookProblem{Reason: fmt.Sprintf(
			"notebook %q has no section %q; its sections are: %s", doc.Title, section, strings.Join(doc.Sections, ", "))}
	}
	out["section"] = section
	out["body"] = text
	return out, nil
}

// Update implements toolset.NotebookStore: it writes the run's own notebook only, attributed to
// "agent:<run id>". append and section read the body and write the next version in one transaction,
// so a write without expected_version still applies to exactly the version it extended.
func (a notebookAccess) Update(ctx context.Context, update toolset.NotebookUpdate) (any, error) {
	author := "agent:" + a.runID
	var doc session.NotebookDoc
	var err error
	switch update.Op {
	case "create":
		doc, err = a.engine.store.CreateNotebook(ctx, a.sessionID, update.Notebook, update.Content, author)
	case "replace":
		body := update.Content
		doc, err = a.engine.store.UpdateNotebook(ctx, a.sessionID, update.Notebook, author, "replace", update.Expected,
			func(string) (string, error) { return body, nil })
	case "append":
		doc, err = a.engine.store.UpdateNotebook(ctx, a.sessionID, update.Notebook, author, "append", update.Expected,
			func(current string) (string, error) { return notebook.Append(current, update.Content), nil })
	case "section":
		doc, err = a.engine.store.UpdateNotebook(ctx, a.sessionID, update.Notebook, author, "section", update.Expected,
			func(current string) (string, error) {
				return notebook.UpsertSection(current, update.Section, update.Content), nil
			})
	default:
		return nil, &toolset.NotebookProblem{Reason: "op must be create, replace, append or section"}
	}
	if err != nil {
		return nil, notebookProblem(err)
	}
	a.engine.notebookUpdated(ctx, a.sessionID, doc)
	return map[string]any{
		"title": doc.Title, "id": doc.ID, "version": doc.Version, "bytes": doc.Bytes,
	}, nil
}

// notebookProblem turns a store error into what a model can act on: a version conflict with the
// current version, an unknown notebook, or a refused write. Anything else (a storage failure) is
// returned as it is.
func notebookProblem(err error) error {
	var conflict *session.NotebookConflict
	if errors.As(err, &conflict) {
		return &toolset.NotebookConflict{CurrentVersion: conflict.CurrentVersion}
	}
	if errors.Is(err, session.ErrNotebookNotFound) {
		return &toolset.NotebookProblem{Reason: "no notebook of that title or id in this session; call notebook_list for the titles"}
	}
	if errors.Is(err, session.ErrInvalid) {
		return &toolset.NotebookProblem{Reason: strings.TrimPrefix(err.Error(), session.ErrInvalid.Error()+": ")}
	}
	return err
}

// The run-start index of the working notebooks (ADR-0020). It is one user row appended by the Start
// transaction: titles, versions, update times and section headings, never a body, bounded so that it
// stays a pointer to the work and not a copy of it.
const (
	maxNotebookIndexEntries = 8
	maxNotebookIndexBytes   = 600
	notebookIndexPrefix     = "[runtime] Working notebooks of this session (external working memory, not Task State): "
	notebookIndexTail       = ". Read a notebook's Handoff section with notebook_read before continuing its work; bodies are not repeated here."
)

// notebookIndexRow is the row a run appends when its session has notebooks. ok is false when the
// session has none: such a run gets no row.
func notebookIndexRow(infos []session.NotebookInfo) (protocol.Message, bool) {
	if len(infos) == 0 {
		return protocol.Message{}, false
	}
	entries := make([]string, 0, min(len(infos), maxNotebookIndexEntries))
	for i, info := range infos {
		if i == maxNotebookIndexEntries {
			break
		}
		sections := strings.Join(info.Sections, ", ")
		if sections == "" {
			sections = "none"
		}
		entries = append(entries, fmt.Sprintf("%s (v%d, updated %s, sections: %s)",
			info.Title, info.Version, info.UpdatedAt.UTC().Format(time.RFC3339), sections))
	}
	// The largest prefix of the entries that fits, with a count of those left out: the row is a
	// pointer, so a title it cannot name is one notebook_list call away.
	for n := len(entries); n >= 1; n-- {
		line := notebookIndexPrefix + strings.Join(entries[:n], "; ")
		if remaining := len(infos) - n; remaining > 0 {
			line += fmt.Sprintf("; and %d more (notebook_list)", remaining)
		}
		line += notebookIndexTail
		if len(line) <= maxNotebookIndexBytes {
			return protocol.Message{Role: "user", Origin: protocol.OriginNotebook, Content: line}, true
		}
	}
	line := truncateUTF8(notebookIndexPrefix+entries[0]+notebookIndexTail, maxNotebookIndexBytes)
	return protocol.Message{Role: "user", Origin: protocol.OriginNotebook, Content: line}, true
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
