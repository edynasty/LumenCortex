package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/internal/threadctx"
)

// ThreadNoteEdit is the Note part of a TaskStateEdit: the user adds (no ID), rewrites or removes a
// note of the thread (ADR-0010 D1).
type ThreadNoteEdit = taskstate.NoteEdit

// MaxThreadNotes and MaxThreadNoteBytes bound a thread's notes and one note's text, so a host can
// explain a refusal while the user types; Core validates again.
const (
	MaxThreadNotes     = taskstate.MaxNotes
	MaxThreadNoteBytes = taskstate.MaxNoteTextBytes
)

// ErrTooManyNotes: the thread already holds MaxThreadNotes notes written by the user.
var ErrTooManyNotes = taskstate.ErrTooManyNotes

// RecallHit is one recall result; ThreadNote is a note with its freshness (ADR-0010 D3, D4).
type (
	RecallHit  = threadctx.Hit
	ThreadNote = threadctx.NoteHit
)

// ThreadView is what read_thread returns about one session of this workspace.
type ThreadView struct {
	Context  string `json:"context"`
	ID       string `json:"id"`
	Kind     string `json:"kind,omitempty"`
	ParentID string `json:"parentId,omitempty"`
	Status   string `json:"status"`
	// Withdrawn is true when a revocation or privacy fence names the session: nothing of its content
	// is returned.
	Withdrawn  bool          `json:"withdrawn,omitempty"`
	Agent      string        `json:"agent,omitempty"`
	Goal       string        `json:"goal,omitempty"`
	Brief      string        `json:"brief,omitempty"`
	Resolution string        `json:"resolution,omitempty"`
	UpdatedAt  time.Time     `json:"updatedAt"`
	Notes      []ThreadNote  `json:"notes,omitempty"`
	OpenUnits  []threadUnit  `json:"openUnits,omitempty"`
	Tasks      []threadTask  `json:"tasks,omitempty"`
	Children   []threadChild `json:"children,omitempty"`
	Final      string        `json:"final,omitempty"`
	// Transcript is one page of the session's visible messages, when the reader asked for it.
	Transcript *threadTranscript `json:"transcript,omitempty"`
}

type threadUnit struct {
	ID     string `json:"id"`
	Goal   string `json:"goal"`
	Status string `json:"status"`
}

type threadTask struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	Objective string `json:"objective"`
	SessionID string `json:"sessionId,omitempty"`
}

type threadChild struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	Withdrawn bool   `json:"withdrawn,omitempty"`
	Agent     string `json:"agent,omitempty"`
	Goal      string `json:"goal,omitempty"`
}

// maxThreadChildren bounds the children read_thread lists.
const maxThreadChildren = 20

// ReadThread returns one session of this workspace as a host shows it, with note freshness and
// applicability checked against the main checkout (ADR-0010 D3, D4). The text of a note whose cited
// file is gone is kept, labelled source_missing; a fenced session or note shows nothing.
func (e *Engine) ReadThread(ctx context.Context, id string) (ThreadView, error) {
	return e.readThread(ctx, id, e.workspace, false, transcriptRequest{})
}

// Recall ranks this workspace's sessions against query (ADR-0010 D3), with note freshness checked
// against the main checkout, as a host shows it (see ReadThread). exclude names a session whose own
// subtree is left out ("" for none).
func (e *Engine) Recall(ctx context.Context, query string, limit int, exclude string) ([]RecallHit, error) {
	return e.recall(ctx, query, limit, exclude, e.workspace, false)
}

// SourceStatus is the store's provenance of one derived target of a session (plan T2-S1).
type SourceStatus = session.SourceStatus

// SourceStatus reports where a derived target of a session (a Task State entry ref such as
// "ts:note:<id>@sha256:<hex>", a file ref "path@blob", a child session ref) came from: its origin
// class, the root its edges lead to, whether that lineage is complete, and whether the session or a
// source on the way is fenced. It only reads (plan T2-S1, for hosts).
func (e *Engine) SourceStatus(ctx context.Context, sessionID, ref string) (SourceStatus, error) {
	return e.store.SourceStatus(ctx, sessionID, ref)
}

// readThread reads one session for a host or, when agent is true, for an agent's read_thread, which
// also loses the text of notes whose source is gone (threadctx.Apply).
func (e *Engine) readThread(ctx context.Context, id, workspace string, agent bool, page transcriptRequest) (ThreadView, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return ThreadView{}, errors.New("thread id is required")
	}
	row, err := e.store.ContextRow(ctx, id, threadctx.MaxFinalBytes)
	if errors.Is(err, session.ErrNotFound) {
		return ThreadView{}, fmt.Errorf("no thread %q in this workspace", id)
	}
	if err != nil {
		return ThreadView{}, err
	}
	thread, err := threadOf(row)
	if err != nil {
		return ThreadView{}, err
	}
	if row.Fenced {
		return ThreadView{Context: threadctx.Framing, ID: thread.ID, Kind: thread.Kind, ParentID: thread.ParentID,
			Status: thread.Status, Withdrawn: true, UpdatedAt: thread.UpdatedAt}, nil
	}
	state, err := e.loadTaskState(ctx, id)
	if err != nil {
		return ThreadView{}, err
	}
	view := ThreadView{
		Context: threadctx.Framing, ID: thread.ID, Kind: thread.Kind, ParentID: thread.ParentID, Status: thread.Status,
		Agent: thread.Agent, Goal: threadctx.Cut(thread.Goal, taskstate.MaxBriefBytes), Brief: thread.Brief,
		Resolution: thread.Resolution, UpdatedAt: thread.UpdatedAt, Final: threadctx.Cut(thread.Final, threadctx.MaxFinalBytes),
	}
	for _, u := range state.Units().List() {
		if u.Status != "completed" && u.Status != "cancelled" {
			view.OpenUnits = append(view.OpenUnits, threadUnit{ID: u.ID, Goal: threadctx.Cut(u.Goal, 256), Status: string(u.Status)})
		}
	}
	for _, t := range state.Tasks {
		view.Tasks = append(view.Tasks, threadTask{ID: t.ID, State: t.State, Objective: threadctx.Cut(t.Objective, 256), SessionID: t.SessionID})
	}
	relations, err := e.store.ChildRelations(ctx, id, maxThreadChildren)
	if err != nil {
		return ThreadView{}, err
	}
	for _, rel := range relations {
		child, err := e.store.ContextRow(ctx, rel.ChildSessionID, 0)
		if err != nil {
			continue
		}
		if child.Fenced {
			view.Children = append(view.Children, threadChild{ID: child.ID, Kind: rel.Kind, Status: child.Status, Withdrawn: true})
			continue
		}
		view.Children = append(view.Children, threadChild{ID: child.ID, Kind: rel.Kind, Status: child.Status, Agent: child.AgentID, Goal: threadctx.Cut(child.Goal, 256)})
	}
	view.Notes = make([]ThreadNote, 0, len(thread.Notes))
	for _, n := range thread.Notes {
		view.Notes = append(view.Notes, threadctx.NoteView(n))
	}
	if err := e.applyNotes(ctx, workspace, id, agent, thread.Notes, view.Notes); err != nil {
		return ThreadView{}, err
	}
	if page.want {
		tr, err := e.transcriptPage(ctx, id, page.before)
		if err != nil {
			return ThreadView{}, err
		}
		view.Transcript = &tr
	}
	return view, nil
}

func (e *Engine) recall(ctx context.Context, query string, limit int, exclude, workspace string, agent bool) ([]RecallHit, error) {
	hits, byID, err := e.rankThreads(ctx, query, limit, exclude)
	if err != nil {
		return nil, err
	}
	for i := range hits {
		t := byID[hits[i].ID]
		notes := make([]taskstate.Note, 0, len(hits[i].Notes))
		for _, hn := range hits[i].Notes {
			for _, n := range t.Notes {
				if n.ID == hn.ID {
					notes = append(notes, n)
				}
			}
		}
		if err := e.applyNotes(ctx, workspace, hits[i].ID, agent, notes, hits[i].Notes); err != nil {
			return nil, err
		}
	}
	if hits == nil {
		hits = []RecallHit{}
	}
	return hits, nil
}

// rankThreads ranks this workspace's sessions against query: the newest MaxCandidates, then older ones
// whose stored text holds a query term (plan T2-S1), less exclude's subtree and every fenced session.
// It returns the hits and the ranked threads by id.
func (e *Engine) rankThreads(ctx context.Context, query string, limit int, exclude string) ([]RecallHit, map[string]threadctx.Thread, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(query) > threadctx.MaxQueryBytes {
		return nil, nil, fmt.Errorf("recall query must be 1-%d bytes", threadctx.MaxQueryBytes)
	}
	rows, err := e.store.ContextRows(ctx, threadctx.MaxCandidates, threadctx.MaxFinalBytes)
	if err != nil {
		return nil, nil, err
	}
	if len(rows) == threadctx.MaxCandidates {
		older, err := e.store.ContextRowsMatching(ctx, threadctx.Tokens(query), threadctx.MaxCandidates, threadctx.MaxCandidates, threadctx.MaxFinalBytes)
		if err != nil {
			return nil, nil, err
		}
		rows = append(rows, older...)
	}
	excluded := map[string]bool{}
	if exclude != "" {
		excluded[exclude] = true
		// Children were started by the excluded session; their results already reached it.
		for _, row := range rows {
			if row.ParentID == exclude {
				excluded[row.ID] = true
			}
		}
	}
	threads := make([]threadctx.Thread, 0, len(rows))
	byID := map[string]threadctx.Thread{}
	for _, row := range rows {
		// A fenced session is withdrawn: it is not a candidate, so nothing of it ranks or shows.
		if excluded[row.ID] || row.Fenced {
			continue
		}
		// A notes value that does not decode (a foreign writer) leaves the thread recallable by its goal.
		t, _ := threadOf(row)
		threads = append(threads, t)
		byID[t.ID] = t
	}
	return threadctx.Recall(query, threads, time.Now().UTC(), limit), byID, nil
}

// applyNotes sets each view's freshness from the files in workspace and its applicability from the
// store's provenance of the note's version in sessionID (threadctx.Apply, for an agent reader when
// agent is true). One git process hashes every cited path; nothing is hashed when no note cites one.
func (e *Engine) applyNotes(ctx context.Context, workspace, sessionID string, agent bool, notes []taskstate.Note, views []ThreadNote) error {
	var paths []string
	for _, n := range notes {
		for _, c := range n.Sources() {
			paths = append(paths, c.Path)
		}
	}
	current := map[string]string{}
	if len(paths) > 0 {
		repo, err := repository.New(workspace)
		if err != nil {
			return err
		}
		if current, err = repo.FileBlobs(ctx, paths); err != nil {
			return err
		}
	}
	for i, n := range notes {
		st, err := e.store.SourceStatus(ctx, sessionID, taskstate.EntryRef(taskstate.EntryNote, n.ID, n))
		if err != nil {
			return err
		}
		threadctx.Apply(&views[i], n, current, threadctx.SourceFacts{Origin: st.Origin, LineageComplete: st.LineageComplete, Withdrawn: st.Withdrawn}, agent)
	}
	return nil
}

func threadOf(row session.ContextRow) (threadctx.Thread, error) {
	t := threadctx.Thread{
		ID: row.ID, ParentID: row.ParentID, Kind: row.Kind, Status: row.Status, Agent: row.AgentID, Goal: row.Goal,
		Brief: row.Brief, Resolution: row.Resolution, Final: row.Final, UpdatedAt: row.UpdatedAt,
	}
	if len(row.Notes) > 0 {
		if err := json.Unmarshal(row.Notes, &t.Notes); err != nil {
			return threadctx.Thread{ID: row.ID, Goal: row.Goal}, fmt.Errorf("decode notes of %s: %w", row.ID, err)
		}
	}
	return t, nil
}

// threadReader is the read_thread/recall back end of one run: freshness is checked in the run's own
// workspace (its worktree when it owns one), and recall leaves out the run's session and children.
type threadReader struct {
	engine    *Engine
	sessionID string
	workspace string
}

func (r threadReader) ReadThread(ctx context.Context, id string, transcript bool, before int64) (any, error) {
	return r.engine.readThread(ctx, id, r.workspace, true, transcriptRequest{want: transcript, before: before})
}

func (r threadReader) Recall(ctx context.Context, query string, limit int) (any, error) {
	hits, err := r.engine.recall(ctx, query, limit, r.sessionID, r.workspace, true)
	if err != nil {
		return nil, err
	}
	return map[string]any{"context": threadctx.Framing, "hits": hits}, nil
}
