package jobs

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
)

type astraCancelBarrier struct {
	Store
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (s *astraCancelBarrier) UpdateJob(ctx context.Context, id, from string, change session.JobChange) (bool, error) {
	if from == session.JobQueued && change.State == session.JobCancelled {
		s.once.Do(func() { close(s.entered); <-s.release })
	}
	return s.Store.UpdateJob(ctx, id, from, change)
}

func TestAstraCancelSurvivesConcurrentClaim(t *testing.T) {
	for _, mode := range []string{"job", "run", "session"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			stateDir := t.TempDir()
			store, err := session.Open(filepath.Join(stateDir, "state"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := store.Create(ctx, session.Session{ID: "astra_session", Goal: "cancel race"}); err != nil {
				t.Fatal(err)
			}
			barrier := &astraCancelBarrier{Store: store, entered: make(chan struct{}), release: make(chan struct{})}
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(barrier.release) }) }
			defer unblock()
			manager, err := New(Options{Store: barrier, StateDir: stateDir})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				unblock()
				_ = manager.Close()
			}()
			job, err := manager.Submit(ctx, Spec{Workspace: t.TempDir(), Command: "sleep 30", RunID: "astra_run", SessionID: "astra_session"})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				var err error
				switch mode {
				case "job":
					_, err = manager.Cancel(ctx, job.ID)
				case "run":
					_, err = manager.CancelRun(ctx, "astra_run", "run_ended")
				case "session":
					_, err = manager.CancelSession(ctx, "astra_session")
				}
				done <- err
			}()
			select {
			case <-barrier.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("cancel never read queued")
			}
			if err := manager.Start(ctx); err != nil {
				t.Fatal(err)
			}
			until := time.Now().Add(5 * time.Second)
			for {
				current, err := manager.Get(ctx, job.ID)
				if err != nil {
					t.Fatal(err)
				}
				if current.PID > 0 {
					break
				}
				if time.Now().After(until) {
					t.Fatal("worker did not start process")
				}
				time.Sleep(10 * time.Millisecond)
			}
			unblock()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			current, err := manager.Get(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.State != session.JobCancelled {
				t.Errorf("cancel returned nil but job state=%s pid=%d", current.State, current.PID)
			}
			if processAlive(current.PID) {
				t.Errorf("cancel returned while process %d was alive", current.PID)
			}
		})
	}
}
