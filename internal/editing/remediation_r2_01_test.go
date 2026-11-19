package editing

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// inPlaceForeignHost wraps LocalHost (implementing only Host.{Resolve,Open,Event}) and injects a
// foreign in-place mutation on the newly O_EXCL-created path before writeCAS reads or writes.
type inPlaceForeignHost struct {
	LocalHost
	mutate func(path string) error
	fired  bool
}

func (h *inPlaceForeignHost) Open(path string, create bool) (*os.File, error) {
	f, err := h.LocalHost.Open(path, create)
	if err != nil || h.fired || !create {
		return f, err
	}
	if info, statErr := f.Stat(); statErr == nil && info.Size() == 0 {
		h.fired = true
		if merr := h.mutate(path); merr != nil {
			_ = f.Close()
			return nil, merr
		}
	}
	return f, nil
}

// postWriteForeignHost injects a foreign in-place overwrite during writeCAS right before the
// post-write verification read by hooking afterTruncate / Write sequence via a custom Host + hook.
func TestR201InPlaceForeignMutationsNeverDeleted(t *testing.T) {
	t.Run("cas_conflict_after_inplace_foreign_write", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "gen"), 0o755); err != nil {
			t.Fatal(err)
		}
		lh, err := NewLocalHost(root)
		if err != nil {
			t.Fatal(err)
		}
		foreign := []byte("FOREIGN EDITOR SAVE\n")
		host := &inPlaceForeignHost{
			LocalHost: lh,
			mutate: func(path string) error {
				return os.WriteFile(path, foreign, 0o644)
			},
		}
		e, err := New(Options{Root: root, Host: host})
		if err != nil {
			t.Fatal(err)
		}
		var receipt CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { receipt = r })
		content := []byte("agent content\n")
		_, commitErr := e.Commit(ctx, Plan{
			Actor:   Actor{Session: "s1"},
			Tool:    "write_file",
			Host:    host,
			Changes: []FileChange{{Path: "gen/out.txt", Content: &content, AllowCreate: true}},
		})
		if !host.fired {
			t.Fatalf("expected inPlaceForeignHost to fire")
		}
		if CodeOf(commitErr) != CodeConflict {
			t.Fatalf("expected CodeConflict, got %v", commitErr)
		}
		if receipt.Committed || receipt.DirtyFailure {
			t.Fatalf("expected Committed=false DirtyFailure=false on pre-write CAS refusal, got %+v", receipt)
		}
		got, readErr := os.ReadFile(filepath.Join(root, "gen", "out.txt"))
		if readErr != nil {
			t.Fatalf("R2-01 DATA LOSS: foreign in-place content DELETED after refusal: %v", readErr)
		}
		if !bytes.Equal(got, foreign) {
			t.Fatalf("foreign content corrupted: got %q, want %q", got, foreign)
		}
	})

	t.Run("inplace_foreign_append_preserved", func(t *testing.T) {
		root := t.TempDir()
		lh, err := NewLocalHost(root)
		if err != nil {
			t.Fatal(err)
		}
		appended := []byte("FOREIGN APPENDED LINE\n")
		host := &inPlaceForeignHost{
			LocalHost: lh,
			mutate: func(path string) error {
				af, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
				if err != nil {
					return err
				}
				_, werr := af.Write(appended)
				return errors.Join(werr, af.Close())
			},
		}
		e, err := New(Options{Root: root, Host: host})
		if err != nil {
			t.Fatal(err)
		}
		content := []byte("agent content\n")
		_, commitErr := e.Commit(context.Background(), Plan{
			Actor:   Actor{Session: "s1"},
			Tool:    "write_file",
			Host:    host,
			Changes: []FileChange{{Path: "gen/append.txt", Content: &content, AllowCreate: true}},
		})
		if CodeOf(commitErr) != CodeConflict {
			t.Fatalf("expected CodeConflict, got %v", commitErr)
		}
		got, readErr := os.ReadFile(filepath.Join(root, "gen", "append.txt"))
		if readErr != nil || !bytes.Equal(got, appended) {
			t.Fatalf("foreign appended content lost: err=%v got=%q", readErr, got)
		}
	})

	t.Run("inplace_foreign_oversized_write_preserved", func(t *testing.T) {
		root := t.TempDir()
		lh, err := NewLocalHost(root)
		if err != nil {
			t.Fatal(err)
		}
		oversized := bytes.Repeat([]byte("Z"), MaxFileBytes+64)
		host := &inPlaceForeignHost{
			LocalHost: lh,
			mutate: func(path string) error {
				return os.WriteFile(path, oversized, 0o644)
			},
		}
		e, err := New(Options{Root: root, Host: host})
		if err != nil {
			t.Fatal(err)
		}
		content := []byte("agent small\n")
		_, commitErr := e.Commit(context.Background(), Plan{
			Actor:   Actor{Session: "s1"},
			Tool:    "write_file",
			Host:    host,
			Changes: []FileChange{{Path: "gen/huge.bin", Content: &content, AllowCreate: true}},
		})
		if commitErr == nil {
			t.Fatalf("expected error when foreign oversized content landed in created inode")
		}
		got, readErr := os.ReadFile(filepath.Join(root, "gen", "huge.bin"))
		if readErr != nil || len(got) != len(oversized) || !bytes.Equal(got, oversized) {
			t.Fatalf("foreign oversized content lost/corrupted: err=%v len=%d want=%d", readErr, len(got), len(oversized))
		}
	})

	t.Run("inplace_foreign_partial_overwrite_during_write_preserved", func(t *testing.T) {
		root := t.TempDir()
		lh, err := NewLocalHost(root)
		if err != nil {
			t.Fatal(err)
		}
		// Custom host forces the Host.Open fallback path; afterTruncate writes foreign bytes into the
		// same inode before returning nil, and then another foreign write alters a prefix!
		host := &inPlaceForeignHost{
			LocalHost: lh,
			mutate:    func(string) error { return nil },
		}
		e, err := New(Options{Root: root, Host: host})
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, "gen", "partial.txt")
		e.afterTruncate = func(path string) error {
			// Simulate external process writing into path right after truncate when our write also fails:
			if werr := os.WriteFile(path, []byte("FOREIGN PARTIAL OVERWRITE\n"), 0o644); werr != nil {
				return werr
			}
			return errors.New("simulated write failure after foreign in-place write")
		}
		var receipt CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { receipt = r })
		content := []byte("agent content\n")
		_, commitErr := e.Commit(ctx, Plan{
			Actor:   Actor{Session: "s1"},
			Tool:    "write_file",
			Host:    host,
			Changes: []FileChange{{Path: "gen/partial.txt", Content: &content, AllowCreate: true}},
		})
		if commitErr == nil {
			t.Fatalf("expected simulated write failure")
		}
		if !receipt.DirtyFailure {
			t.Fatalf("expected DirtyFailure=true after post-truncate failure")
		}
		got, readErr := os.ReadFile(target)
		if readErr != nil {
			t.Fatalf("foreign partial overwrite deleted on failDirty: %v", readErr)
		}
		if string(got) != "FOREIGN PARTIAL OVERWRITE\n" {
			t.Fatalf("got %q, want FOREIGN PARTIAL OVERWRITE", got)
		}
		_ = target
	})

	t.Run("rollback_preserves_file_modified_and_restored_by_foreign_process", func(t *testing.T) {
		root := t.TempDir()
		existingPath := filepath.Join(root, "b_existing.txt")
		if err := os.WriteFile(existingPath, []byte("old-b\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		e, err := New(Options{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		createdPath := filepath.Join(root, "a_created.txt")
		aContent := []byte("agent-a\n")
		bContent := []byte("agent-b\n")
		e.beforeWrite = func(path string) {
			if filepath.Base(path) == "b_existing.txt" {
				// Foreign writer mutates a_created.txt in place (even appending/changing mtime) and causes b_existing conflict!
				f, err := os.OpenFile(createdPath, os.O_WRONLY|os.O_APPEND, 0)
				if err == nil {
					_, _ = f.Write([]byte("foreign-tail\n"))
					_ = f.Close()
				}
				_ = os.WriteFile(existingPath, []byte("concurrent-b\n"), 0o644)
			}
		}
		_, commitErr := e.Commit(context.Background(), Plan{
			Actor: Actor{Session: "s1"},
			Tool:  "apply_patch",
			Changes: []FileChange{
				{Path: "a_created.txt", Content: &aContent, Create: true},
				{Path: "b_existing.txt", Content: &bContent},
			},
		})
		if commitErr == nil {
			t.Fatalf("expected conflict on b_existing.txt")
		}
		got, readErr := os.ReadFile(createdPath)
		if readErr != nil {
			t.Fatalf("rollback deleted file after foreign append: %v", readErr)
		}
		if string(got) != "agent-a\nforeign-tail\n" {
			t.Fatalf("unexpected content after rollback: %q", got)
		}
	})

	t.Run("rollback_preserves_file_touched_and_restored_to_same_bytes_by_foreign_process", func(t *testing.T) {
		root := t.TempDir()
		existingPath := filepath.Join(root, "b_existing.txt")
		if err := os.WriteFile(existingPath, []byte("old-b\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		e, err := New(Options{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		createdPath := filepath.Join(root, "a_created.txt")
		aContent := []byte("agent-a\n")
		bContent := []byte("agent-b\n")
		e.beforeWrite = func(path string) {
			if filepath.Base(path) == "b_existing.txt" {
				// Foreign process temporarily rewrites a_created.txt then restores identical bytes with newer mtime!
				_ = os.WriteFile(createdPath, []byte("temporary foreign edit\n"), 0o644)
				future := time.Now().Add(2 * time.Second)
				_ = os.WriteFile(createdPath, aContent, 0o644)
				_ = os.Chtimes(createdPath, future, future)
				_ = os.WriteFile(existingPath, []byte("concurrent-b\n"), 0o644)
			}
		}
		_, commitErr := e.Commit(context.Background(), Plan{
			Actor: Actor{Session: "s1"},
			Tool:  "apply_patch",
			Changes: []FileChange{
				{Path: "a_created.txt", Content: &aContent, Create: true},
				{Path: "b_existing.txt", Content: &bContent},
			},
		})
		if commitErr == nil {
			t.Fatalf("expected conflict on b_existing.txt")
		}
		if _, readErr := os.ReadFile(createdPath); readErr != nil {
			t.Fatalf("rollback deleted file that had been modified and restored by foreign process: %v", readErr)
		}
	})
}

// TestR201InPlaceForeignWriterRealRace solidifies Opus 5.5's 400-iteration concurrent race probe.
func TestR201InPlaceForeignWriterRealRace(t *testing.T) {
	const iterations = 400
	foreign := []byte("FOREIGN WRITER CONTENT THAT IS LONGER THAN THE AGENT CONTENT\n")
	var lost, foreignKept, agentKept, mixed, other int
	for i := 0; i < iterations; i++ {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "gen"), 0o755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, "gen", "out.txt")
		e, err := New(Options{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		var wrote atomic.Bool
		var stop atomic.Bool
		var wg sync.WaitGroup
		started := make(chan struct{})
		e.beforeWrite = func(string) {
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
		}
		content := []byte("agent content\n")
		_, _ = e.Commit(context.Background(), Plan{
			Actor:   Actor{Session: "s1"},
			Tool:    "write_file",
			Changes: []FileChange{{Path: "gen/out.txt", Content: &content, AllowCreate: true}},
		})
		stop.Store(true)
		wg.Wait()
		got, readErr := os.ReadFile(target)
		switch {
		case readErr != nil && wrote.Load():
			lost++
		case readErr != nil:
			other++
		case bytes.Equal(got, foreign):
			foreignKept++
		case bytes.Equal(got, content):
			agentKept++
		default:
			mixed++
		}
	}
	t.Logf("R2-01 real race: iterations=%d lost=%d foreignKept=%d agentKept=%d mixed=%d other=%d",
		iterations, lost, foreignKept, agentKept, mixed, other)
	if lost > 0 || mixed > 0 {
		t.Fatalf("R2-01 DATA LOSS/CORRUPTION: lost=%d mixed=%d out of %d iterations", lost, mixed, iterations)
	}
}

// TestR201TwoOSProcessesCreateRace uses two independent OS processes (parent Engine.Commit racing
// against a spawned native helper executable) on a real filesystem across both exclusive-create and
// in-place overwrite modes, verifying zero data loss and exact byte preservation.
func TestR201TwoOSProcessesCreateRace(t *testing.T) {
	foreign := []byte("FOREIGN OS PROCESS PAYLOAD 0123456789ABCDEF\n")
	agentBytes := []byte("agent os process content\n")

	helperDir := t.TempDir()
	helperSrc := filepath.Join(helperDir, "main.go")
	helperBin := filepath.Join(helperDir, "foreign_writer")
	src := `package main
import (
	"errors"
	"os"
	"time"
)
func main() {
	target := os.Args[1]
	mode := os.Args[2]
	ready := os.Args[3]
	payload := []byte("FOREIGN OS PROCESS PAYLOAD 0123456789ABCDEF\n")
	_ = os.WriteFile(ready, []byte("1"), 0o644)
	deadline := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(deadline) {
		flags := os.O_WRONLY | os.O_TRUNC
		if mode == "create" {
			flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
		}
		f, err := os.OpenFile(target, flags, 0o644)
		if err != nil {
			if mode == "create" && errors.Is(err, os.ErrExist) {
				os.Exit(1)
			}
			continue
		}
		_, _ = f.Write(payload)
		_ = f.Close()
		os.Exit(0)
	}
	os.Exit(2)
}
`
	if err := os.WriteFile(helperSrc, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	buildCmd := exec.Command("go", "build", "-o", helperBin, helperSrc)
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build helper binary failed: %v\n%s", err, out)
	}

	for _, mode := range []string{"create", "inplace"} {
		t.Run(mode, func(t *testing.T) {
			const rounds = 50
			var lost, mixed, childWins, agentWins int
			for i := 0; i < rounds; i++ {
				root := t.TempDir()
				target := filepath.Join(root, "shared.txt")
				ready := filepath.Join(root, ".ready")
				e, err := New(Options{Root: root})
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(helperBin, target, mode, ready)
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				waitDeadline := time.Now().Add(time.Second)
				for time.Now().Before(waitDeadline) {
					if _, err := os.Stat(ready); err == nil {
						break
					}
					time.Sleep(100 * time.Microsecond)
				}
				_, commitErr := e.Commit(context.Background(), Plan{
					Actor:   Actor{Session: "s-proc"},
					Tool:    "write_file",
					Changes: []FileChange{{Path: "shared.txt", Content: &agentBytes, AllowCreate: true}},
				})
				childErr := cmd.Wait()
				childWrote := childErr == nil

				got, readErr := os.ReadFile(target)
				if readErr != nil {
					if childWrote || commitErr == nil {
						lost++
					}
					continue
				}
				switch {
				case bytes.Equal(got, foreign):
					childWins++
				case bytes.Equal(got, agentBytes):
					agentWins++
				default:
					mixed++
				}
			}
			t.Logf("two-OS-process %s race: rounds=%d childWins=%d agentWins=%d lost=%d mixed=%d",
				mode, rounds, childWins, agentWins, lost, mixed)
			if lost > 0 || mixed > 0 {
				t.Fatalf("two-OS-process %s race: lost=%d mixed=%d in %d rounds", mode, lost, mixed, rounds)
			}
		})
	}
}
