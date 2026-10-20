package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/protocol"
)

func toolReply(store *memoryStore, id string) string {
	for _, m := range store.messages {
		if m.ToolCallID == id {
			return m.Content
		}
	}
	return ""
}

// ADR-0010 D1, C7: a run writes, replaces and removes its own notes, records the blob of each cited
// path, cannot touch the user's note, and the next request's Task State block lists the notes.
func TestNoteToolsKeepNotesInTaskState(t *testing.T) {
	store := newState("notes")
	store.queue(taskstate.Edit{Note: &taskstate.NoteEdit{Text: "Deploys need the staging flag."}})
	var cited []string
	var block string
	provider := &funcProvider{fn: func(_ context.Context, n int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		switch n {
		case 1:
			return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
				call("w1", "note_write", `{"text":"Root cause: retry loop ignores ctx.","cites":["./internal/retry.go"]}`),
				call("w2", "note_write", `{"text":"Tried a mutex; deadlocked."}`),
				call("u1", "note_write", `{"id":"n1","text":"overwrite the user"}`),
				call("u2", "note_remove", `{"id":"n1"}`),
				call("x1", "note_write", `{"text":"bad cite","cites":["missing.go"]}`),
			}}}, nil
		case 2:
			return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
				call("w3", "note_write", `{"id":"n2","text":"Root cause: retry loop ignores ctx cancellation.","cites":["internal/retry.go"]}`),
				call("r1", "note_remove", `{"id":"n3"}`),
			}}}, nil
		default:
			block = taskBlock(req)
			return protocol.ProviderResponse{Message: protocol.Message{Content: "done"}}, nil
		}
	}}
	loop := &Loop{Models: testModels(provider), Store: store, Tools: fakeTools{},
		Cite: func(_ context.Context, paths []string) ([]taskstate.Cite, error) {
			cited = append(cited, paths...)
			out := make([]taskstate.Cite, 0, len(paths))
			for _, p := range paths {
				if p == "missing.go" {
					return nil, errors.New("cited path \"missing.go\" is not a file in this workspace")
				}
				out = append(out, taskstate.Cite{Path: p, Blob: "blob-" + p})
			}
			return out, nil
		}}
	if _, err := loop.Run(context.Background(), "notes", Options{Session: testRef, MaxSteps: 4}); err != nil {
		t.Fatal(err)
	}
	notes := store.ts.Notes
	if len(notes) != 2 {
		t.Fatalf("notes %+v, want the user's and one agent note", notes)
	}
	user, agentNote := notes[0], notes[1]
	if user.ID != "n1" || user.By != taskstate.ByUser || user.Text != "Deploys need the staging flag." {
		t.Fatalf("user note changed: %+v", user)
	}
	if agentNote.ID != "n2" || agentNote.By != taskstate.ByAgent || !strings.Contains(agentNote.Text, "cancellation") ||
		len(agentNote.Cites) != 1 || agentNote.Cites[0] != (taskstate.Cite{Path: "internal/retry.go", Blob: "blob-internal/retry.go"}) {
		t.Fatalf("agent note %+v, want n2 replaced with its cite", agentNote)
	}
	for _, id := range []string{"u1", "u2"} {
		if !strings.Contains(toolReply(store, id), "only the user") {
			t.Fatalf("%s: %s, want the user-note refusal", id, toolReply(store, id))
		}
	}
	if !strings.Contains(toolReply(store, "x1"), "not a file") {
		t.Fatalf("bad cite: %s", toolReply(store, "x1"))
	}
	if !strings.Contains(block, "n2 [agent]: Root cause: retry loop ignores ctx cancellation. (cites internal/retry.go)") ||
		!strings.Contains(block, "n1 [user]: Deploys need the staging flag.") ||
		strings.Index(block, "n2 [agent]") > strings.Index(block, "n1 [user]") {
		t.Fatalf("Task State block does not list the notes newest first:\n%s", block)
	}
	if store.ts.NoteSeq != 3 {
		t.Fatalf("noteSeq %d: ids must not be reused", store.ts.NoteSeq)
	}
}

// A note without cites needs no Cite hook; a run without one refuses cites instead of storing them
// unhashed.
func TestNoteCitesNeedTheCiteHook(t *testing.T) {
	store := newState("nocite")
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
			call("a", "note_write", `{"text":"plain"}`),
			call("b", "note_write", `{"text":"cited","cites":["a.go"]}`),
			call("c", "note_write", `{"text":"x","extra":true}`),
		}}},
		{Message: protocol.Message{Content: "done"}},
	}}
	if _, err := (&Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}).Run(context.Background(), "nocite", Options{Session: testRef, MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	if len(store.ts.Notes) != 1 || store.ts.Notes[0].Text != "plain" {
		t.Fatalf("notes %+v", store.ts.Notes)
	}
	if !strings.Contains(toolReply(store, "b"), "cannot cite") || !strings.Contains(toolReply(store, "c"), "extra") {
		t.Fatalf("b=%s c=%s", toolReply(store, "b"), toolReply(store, "c"))
	}
}
