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
	Context    string        `json:"context"`
	ID         string        `json:"id"`
	Kind       string        `json:"kind,omitempty"`
	ParentID   string        `json:"parentId,omitempty"`
	Status     string        `json:"status"`
	Agent      string        `json:"agent,omitempty"`
	Goal       string        `json:"goal"`
	Brief      string        `json:"brief,omitempty"`
	Resolution string        `json:"resolution,omitempty"`
	UpdatedAt  time.Time     `json:"updatedAt"`
	Notes      []ThreadNote  `json:"notes,omitempty"`
	OpenUnits  []threadUnit  `json:"openUnits,omitempty"`
	Tasks      []threadTask  `json:"tasks,omitempty"`
	Children   []threadChild `json:"children,omitempty"`
	Final      string        `json:"final,omitempty"`
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
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
	Agent  string `json:"agent,omitempty"`
	Goal   string `json:"goal"`
}

// maxThreadChildren bounds the children read_thread lists.
const maxThreadChildren = 20

// ReadThread returns one session of this workspace as read_thread shows it, with note freshness
// checked against the main checkout (ADR-0010 D3, D4).
func (e *Engine) ReadThread(ctx context.Context, id string) (ThreadView, error) {
	return e.readThread(ctx, id, e.workspace)
}

// Recall ranks this workspace's sessions against query (ADR-0010 D3), with note freshness checked
// against the main checkout. exclude names a session whose own subtree is left out ("" for none).
func (e *Engine) Recall(ctx context.Context, query string, limit int, exclude string) ([]RecallHit, error) {
	return e.recall(ctx, query, limit, exclude, e.workspace)
}

func (e *Engine) readThread(ctx context.Context, id, workspace string) (ThreadView, error) {
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
		child, err := e.store.Get(ctx, rel.ChildSessionID)
		if err != nil {
			continue
		}
		view.Children = append(view.Children, threadChild{ID: child.ID, Kind: rel.Kind, Status: child.Status, Agent: child.AgentID, Goal: threadctx.Cut(child.Goal, 256)})
	}
	view.Notes = make([]ThreadNote, 0, len(thread.Notes))
	for _, n := range thread.Notes {
		view.Notes = append(view.Notes, threadctx.NoteView(n))
	}
	if err := markNotes(ctx, workspace, thread.Notes, view.Notes); err != nil {
		return ThreadView{}, err
	}
	return view, nil
}

func (e *Engine) recall(ctx context.Context, query string, limit int, exclude, workspace string) ([]RecallHit, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(query) > threadctx.MaxQueryBytes {
		return nil, fmt.Errorf("recall query must be 1-%d bytes", threadctx.MaxQueryBytes)
	}
	rows, err := e.store.ContextRows(ctx, threadctx.MaxCandidates, threadctx.MaxFinalBytes)
	if err != nil {
		return nil, err
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
		if excluded[row.ID] {
			continue
		}
		// A notes value that does not decode (a foreign writer) leaves the thread recallable by its goal.
		t, _ := threadOf(row)
		threads = append(threads, t)
		byID[t.ID] = t
	}
	hits := threadctx.Recall(query, threads, time.Now().UTC(), limit)
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
		if err := markNotes(ctx, workspace, notes, hits[i].Notes); err != nil {
			return nil, err
		}
	}
	if hits == nil {
		hits = []RecallHit{}
	}
	return hits, nil
}

// markNotes sets each view's freshness from the files in workspace. One git process hashes every
// cited path; nothing is hashed when no note cites one.
func markNotes(ctx context.Context, workspace string, notes []taskstate.Note, views []ThreadNote) error {
	var paths []string
	for _, n := range notes {
		for _, c := range n.Cites {
			paths = append(paths, c.Path)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	repo, err := repository.New(workspace)
	if err != nil {
		return err
	}
	current, err := repo.FileBlobs(ctx, paths)
	if err != nil {
		return err
	}
	for i, n := range notes {
		threadctx.MarkStale(&views[i], n, current)
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

func (r threadReader) ReadThread(ctx context.Context, id string) (any, error) {
	return r.engine.readThread(ctx, id, r.workspace)
}

func (r threadReader) Recall(ctx context.Context, query string, limit int) (any, error) {
	hits, err := r.engine.recall(ctx, query, limit, r.sessionID, r.workspace)
	if err != nil {
		return nil, err
	}
	return map[string]any{"context": threadctx.Framing, "hits": hits}, nil
}
