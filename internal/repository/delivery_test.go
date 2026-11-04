package repository

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/taskstate"
)

func TestDeliveryCommitOnlySparseAndLargeIndex(t *testing.T) {
	for _, sparse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sparse=%t", sparse), func(t *testing.T) {
			root := t.TempDir()
			git := func(args ...string) {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = root
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
			}
			put := func(path, text string) {
				t.Helper()
				abs := filepath.Join(root, path)
				if err := os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(abs, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			git("init")
			git("config", "user.email", "fixture@example.invalid")
			git("config", "user.name", "fixture")
			put("keep/result.txt", "before\n")
			if sparse {
				put("outside/absent.txt", "outside\n")
			} else {
				// Ordinary index entries alone exceed the old 512 KiB whole-output cap.
				for i := 0; i < 3000; i++ {
					put(fmt.Sprintf("many/%s%04d", strings.Repeat("p", 180), i), "unchanged\n")
				}
			}
			git("add", ".")
			git("commit", "-m", "baseline")
			if sparse {
				git("sparse-checkout", "init", "--cone")
				git("sparse-checkout", "set", "keep")
			}
			repo, err := New(root)
			if err != nil {
				t.Fatal(err)
			}
			d := &taskstate.DeliveryContract{Commit: true}
			if err := repo.CaptureDelivery(context.Background(), d); err != nil {
				t.Fatal(err)
			}
			put("keep/result.txt", "after\n")
			git("add", "keep/result.txt")
			git("commit", "-m", "delivery")
			check := func(want bool) {
				t.Helper()
				result := repo.VerifyDelivery(context.Background(), d)
				if len(result.Issues) != 0 || (result.Status == taskstate.DeliveryVerified) != want {
					t.Fatalf("satisfied=%t result=%+v", want, result)
				}
			}
			check(true)
			// A materialized skip-worktree path must still be inspected, including in sparse mode.
			git("update-index", "--skip-worktree", "keep/result.txt")
			put("keep/result.txt", "hidden dirty\n")
			check(false)
			put("keep/result.txt", "after\n")
			check(true)
		})
	}
}
