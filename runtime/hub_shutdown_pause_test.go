package runtime

import (
	"context"
	"fmt"
	"testing"
)

// TestHubCancelThenCloseClearsThePauseOnEveryShutdownPath pins RV-03 on both shutdown orders a host may
// use: a paused run the host cancelled ends cancelled, so no reopened Engine reports interrupted(paused),
// whether the host closes the supervisor before the Engine or lets Engine.Close close it.
func TestHubCancelThenCloseClearsThePauseOnEveryShutdownPath(t *testing.T) {
	useGlobalConfig(t, withConcurrency(4, 3))
	for _, viaSupervisor := range []bool{true, false} {
		t.Run(fmt.Sprintf("supervisorCloseFirst=%v", viaSupervisor), func(t *testing.T) {
			for i := range 5 {
				dir := t.TempDir()
				e, err := Open(Options{Workspace: dir})
				if err != nil {
					t.Fatal(err)
				}
				sup, err := NewRunSupervisor(e)
				if err != nil {
					t.Fatal(err)
				}
				id := newParentSession(t, e, &hubScriptProvider{})
				ctx := context.Background()
				if _, err := sup.Start(ctx, id, AgentOptions{Policy: "read-only", Interactive: true, Debug: &AgentDebugOptions{PauseOnStart: true}}); err != nil {
					t.Fatal(err)
				}
				snap := awaitPause(t, e, id, 0)
				if _, err := sup.Command(ctx, HubCommand{SessionID: id, CommandID: "cancel-paused", ExpectedRevision: snap.Revision, Verb: HubVerbCancel}); err != nil {
					t.Fatal(err)
				}
				if viaSupervisor {
					sup.Close()
				}
				if err := e.Close(); err != nil {
					t.Fatal(err)
				}
				e, err = Open(Options{Workspace: dir})
				if err != nil {
					t.Fatal(err)
				}
				sup, err = NewRunSupervisor(e)
				if err != nil {
					t.Fatal(err)
				}
				view, err := sup.View(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if a := view.Agents[0]; a.Paused != nil || a.State == HubStateInterruptedStop {
					t.Errorf("attempt %d: a run the host cancelled while paused reopened as %s paused=%+v", i, a.State, a.Paused)
				}
				sup.Close()
				if err := e.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
