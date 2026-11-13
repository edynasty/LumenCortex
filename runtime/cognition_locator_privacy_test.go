package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// Locators carry workspace paths even when the reader never opens a claim.
// Source withdrawal must isolate a reader that restated that path too.
func locatorPrivacyFixture(t *testing.T, qualified bool) (*cognitionFixture, string, string, string) {
	t.Helper()
	f := newCognitionFixture(t, true)
	path := "CF-BASE-001-private-project-path.txt"
	writeFile(t, filepath.Join(f.root, path), "8080")
	source := f.run(t, "inspect project", cogAnswer("configuration inspected"))
	f.propose(func(refs []string) []string {
		ref := refOf(t, refs, source)
		if qualified {
			return []string{factJSON("service port", []string{ref}, []string{path}, portFact("8080"))}
		}
		return []string{candidateJSON("procedure", "project procedure", "inspect configuration", ref, []string{path})}
	})
	report := f.maintain(t)
	if len(report.Inserted) != 1 || (qualified && report.Qualified != 1) {
		t.Fatalf("locator fixture adoption: %+v", report)
	}
	return f, source, report.Inserted[0], path
}

func TestCognitionLocatorPrivacyIsolatesRestatingReaders(t *testing.T) {
	for _, exit := range []string{"index", "list"} {
		t.Run(exit, func(t *testing.T) {
			f, source, _, path := locatorPrivacyFixture(t, exit == "index")
			p := &cognitionProvider{respond: func(n int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
				if exit == "list" && n == 0 {
					if strings.Contains(indexRow(req), path) {
						t.Error("candidate-only fixture unexpectedly inherited an index")
					}
					return cogCall("read_knowledge", `{}`), nil
				}
				visible := indexRow(req)
				if exit == "list" {
					visible = cogToolResult(t, req)
				}
				if !strings.Contains(visible, path) {
					t.Errorf("%s did not expose the locator path: %q", exit, visible)
				}
				return cogFinal("The project path is " + path), nil
			}}
			readerID := f.run(t, "use project locators", p)
			if _, err := f.engine.DeleteSession(context.Background(), source, WorktreeCleanup{}); err != nil {
				t.Fatal(err)
			}
			if withdrawn, err := f.engine.store.SessionWithdrawn(context.Background(), readerID); err != nil || !withdrawn {
				t.Fatalf("%s-only reader can re-export a deleted source path: withdrawn=%v err=%v", exit, withdrawn, err)
			}
			if view, err := f.engine.ReadThread(context.Background(), readerID); err != nil || !view.Withdrawn || strings.Contains(fmt.Sprint(view), path) {
				t.Fatalf("reader exported after deletion: %+v %v", view, err)
			}
			cont := cogAnswer("continued")
			if _, err := f.engine.runAgentWith(context.Background(), readerID, cont, AgentOptions{Input: "continue", MaxSteps: 2}); !errors.Is(err, session.ErrFenced) || len(cont.requests) != 0 {
				t.Fatalf("deleted locator reached a later request: err=%v requests=%d", err, len(cont.requests))
			}
		})
	}
}

func TestCognitionLocatorPrivacyHidesWithdrawnPaths(t *testing.T) {
	f, source, _, path := locatorPrivacyFixture(t, false)
	if _, err := f.engine.store.RevokeSession(context.Background(), source, "test"); err != nil {
		t.Fatal(err)
	}
	list, err := f.engine.ReadKnowledge(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprint(list), path) {
		t.Fatalf("withdrawn locator still exports its private path: %+v", list)
	}
}

func TestCognitionLocatorPrivacyRefusesRevokedStartNotice(t *testing.T) {
	f, source, _, path := locatorPrivacyFixture(t, true)
	h, err := f.engine.NewSession(context.Background(), SessionOptions{Goal: "read project index"})
	if err != nil {
		t.Fatal(err)
	}
	testHookBeforeBeginRun = func() {
		if _, err := f.engine.store.RevokeSession(context.Background(), source, "test"); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { testHookBeforeBeginRun = nil })
	p := cogAnswer("done")
	_, err = f.engine.runAgentWith(context.Background(), h.ID, p, AgentOptions{MaxSteps: 2})
	if !errors.Is(err, session.ErrFenced) || len(p.requests) != 0 {
		t.Fatalf("Start accepted a revoked locator: err=%v requests=%d", err, len(p.requests))
	}
	if n := countRows(t, f.engine, `SELECT count(*) FROM session_messages WHERE session_id=? AND instr(json,?)>0`, h.ID, path); n != 0 {
		t.Fatalf("refused Start persisted %d revoked notices", n)
	}
}

func TestCognitionLocatorPrivacyRefusesRevokedListCommit(t *testing.T) {
	f, source, _, path := locatorPrivacyFixture(t, false)
	h, err := f.engine.NewSession(context.Background(), SessionOptions{Goal: "list project knowledge"})
	if err != nil {
		t.Fatal(err)
	}
	testHookBeforeToolCommit = func() {
		if _, err := f.engine.store.RevokeSession(context.Background(), source, "test"); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { testHookBeforeToolCommit = nil })
	p := scripted(func(protocol.ProviderRequest) protocol.ProviderResponse { return cogCall("read_knowledge", `{}`) })
	_, err = f.engine.runAgentWith(context.Background(), h.ID, p, AgentOptions{MaxSteps: 3})
	if !errors.Is(err, session.ErrFenced) || len(p.requests) != 1 {
		t.Fatalf("list read before revocation committed afterwards: err=%v requests=%d", err, len(p.requests))
	}
	if n := countRows(t, f.engine, `SELECT count(*) FROM session_messages WHERE session_id=? AND role='tool' AND instr(json,?)>0`, h.ID, path); n != 0 {
		t.Fatalf("refused tool commit persisted %d private list results", n)
	}
}
