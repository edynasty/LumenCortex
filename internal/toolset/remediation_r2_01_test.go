package toolset

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestR201WriteFileInPlaceForeignWriterRealRace solidifies Opus 5.5's 300-iteration toolset probe:
// real write_file and apply_patch create calls racing against a concurrent writer that opens the
// target with O_WRONLY|O_TRUNC and writes foreign bytes in place. Zero iterations may delete the
// concurrently written file.
func TestR201WriteFileInPlaceForeignWriterRealRace(t *testing.T) {
	foreign := []byte("FOREIGN WRITER CONTENT THAT IS LONGER THAN THE AGENT CONTENT\n")
	agentContent := []byte("agent content\n")

	t.Run("write_file_300_iterations", func(t *testing.T) {
		const iterations = 300
		var lost, refusedLost, kept, okRuns, mixed int
		for i := 0; i < iterations; i++ {
			root := t.TempDir()
			set, err := New(Options{Workspace: root, Policy: PolicyFull, SessionID: "s1"})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(root, "gen"), 0o755); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "gen", "out.txt")
			var wrote, stop atomic.Bool
			var wg sync.WaitGroup
			var once sync.Once
			testHookBeforeOpen = func(path string) {
				if filepath.Base(path) != "out.txt" {
					return
				}
				once.Do(func() {
					started := make(chan struct{})
					wg.Add(1)
					go func() {
						defer wg.Done()
						close(started)
						deadline := time.Now().Add(150 * time.Millisecond)
						for !stop.Load() && time.Now().Before(deadline) {
							f, err := os.OpenFile(target, os.O_WRONLY|os.O_TRUNC, 0)
							if err != nil {
								continue
							}
							_, _ = f.Write(foreign)
							_ = f.Close()
							wrote.Store(true)
							return
						}
					}()
					<-started
				})
			}
			result, execErr := set.Execute(context.Background(), "write_file", map[string]any{
				"path":    "gen/out.txt",
				"content": string(agentContent),
			}, nil)
			stop.Store(true)
			wg.Wait()
			testHookBeforeOpen = nil
			// R7 may report a post-publication cleanup uncertainty (a private temp that cannot be
			// proven ours is left as debris) as a non-OK result with an ordinary error. That is a
			// reported failure, not data loss: the invariant checked below is the destination bytes.
			if execErr != nil && result.OK {
				t.Fatalf("inconsistent outcome: OK result with error: %v", execErr)
			}
			if result.OK {
				okRuns++
			}
			got, readErr := os.ReadFile(target)
			switch {
			case readErr != nil && wrote.Load():
				lost++
				if !result.OK {
					refusedLost++
				}
			case readErr == nil:
				kept++
				if !bytes.Equal(got, foreign) && !bytes.Equal(got, agentContent) {
					mixed++
				}
			}
		}
		t.Logf("R2-01 toolset write_file: iterations=%d okRuns=%d lost=%d (refusedLost=%d) mixed=%d kept=%d",
			iterations, okRuns, lost, refusedLost, mixed, kept)
		if lost > 0 || mixed > 0 {
			t.Fatalf("R2-01 DATA LOSS: write_file lost=%d mixed=%d out of %d iterations", lost, mixed, iterations)
		}
	})
}
