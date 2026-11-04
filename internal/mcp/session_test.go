package mcp

import (
	"context"
	"strings"
	"testing"
)

// recordingSession is a session that only records the tools called on it.
type recordingSession struct {
	called []string
	closed bool
}

func (s *recordingSession) Request(context.Context, string, map[string]any, any) error { return nil }
func (s *recordingSession) Notify(string, map[string]any) error                        { return nil }
func (s *recordingSession) ListTools(context.Context) ([]Tool, error)                  { return nil, nil }
func (s *recordingSession) CallTool(_ context.Context, name string, _ map[string]any) (CallToolResult, error) {
	s.called = append(s.called, name)
	return CallToolResult{}, nil
}
func (s *recordingSession) Tools() []Tool  { return nil }
func (s *recordingSession) Status() Status { return Status{} }
func (s *recordingSession) Close() error {
	s.closed = true
	return nil
}

// ADR-0021 D5: closing a client whose binding's stop tool is outside the include list calls no tool,
// closes the client, and records on the server that the sessions it opened were left open. A host write
// cannot configure such a binding; this is the backstop for one that reached a running server anyway.
func TestBoundSessionStopOutsideIncludeIsReported(t *testing.T) {
	cfg := Config{
		ID: "computer", Command: "computer", Include: []string{"computer_start"},
		Session: &SessionBinding{
			Argument: "session_id", StartTool: "computer_start", SessionIDField: "computer_session_id",
			StopTool: "computer_stop", StopArgument: "computer_session_id",
		},
	}
	srv := newServer(nil, instanceKey("/workspace", cfg.ID), "/workspace", cfg, ScopeGlobal)
	client := &recordingSession{}
	srv.mu.Lock()
	srv.current = client
	srv.boundSessions = []string{"cmp_1", "cmp_2"}
	srv.mu.Unlock()
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
	if len(client.called) != 0 || !client.closed {
		t.Fatalf("called=%v closed=%v, want no tool called and the client closed", client.called, client.closed)
	}
	if text := srv.lastError(); !strings.Contains(text, "computer_stop") || !strings.Contains(text, "2") {
		t.Fatalf("lastError = %q, want the skipped stop tool and the 2 sessions left open", text)
	}
}
