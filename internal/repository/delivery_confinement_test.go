package repository

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/taskstate"
)

// Absence is proof only for a path that provably stays inside the workspace. Every symlink met on the
// way is followed and confined, including a dangling one whose missing target lies elsewhere.
func TestDeliveryAbsenceRequiresConfinedPath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, root, outside string)
		path  string
		want  string
	}{
		{"missing file", func(*testing.T, string, string) {}, "never/created", taskstate.DeliveryVerified},
		{"file ancestor (ENOTDIR)", func(t *testing.T, root, _ string) { write(t, filepath.Join(root, "pkg"), "file") }, "pkg/sub/out", taskstate.DeliveryVerified},
		{"dangling ancestor inside", func(t *testing.T, root, _ string) { link(t, "missing-dir", filepath.Join(root, "a")) }, "a/out", taskstate.DeliveryVerified},
		{"two-level link inside", func(t *testing.T, root, _ string) {
			link(t, "l2", filepath.Join(root, "l1"))
			link(t, "gone", filepath.Join(root, "l2"))
		}, "l1/out", taskstate.DeliveryVerified},
		{"outside existing target", func(t *testing.T, root, outside string) { link(t, outside, filepath.Join(root, "escape")) }, "escape/out", taskstate.DeliveryUnavailable},
		{"outside dangling target", func(t *testing.T, root, outside string) {
			link(t, filepath.Join(outside, "not-created"), filepath.Join(root, "escape"))
		}, "escape/output", taskstate.DeliveryUnavailable},
		{"outside dangling relative", func(t *testing.T, root, _ string) { link(t, "../not-created", filepath.Join(root, "escape")) }, "escape/output", taskstate.DeliveryUnavailable},
		{"two-level link outside", func(t *testing.T, root, outside string) {
			link(t, "l2", filepath.Join(root, "l1"))
			link(t, filepath.Join(outside, "deep", "gone"), filepath.Join(root, "l2"))
		}, "l1/x/out", taskstate.DeliveryUnavailable},
		{"symlink loop", func(t *testing.T, root, _ string) {
			link(t, "loop2", filepath.Join(root, "loop1"))
			link(t, "loop1", filepath.Join(root, "loop2"))
		}, "loop1/out", taskstate.DeliveryUnavailable},
		// The entry itself exists (a dangling link): it is not absent.
		{"dangling final link", func(t *testing.T, root, _ string) { link(t, "nothing", filepath.Join(root, "out")) }, "out", taskstate.DeliveryFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			repo, err := New(root)
			if err != nil {
				t.Fatal(err)
			}
			tc.setup(t, root, outside)
			d := &taskstate.DeliveryContract{Files: []taskstate.DeliveryFile{{Path: tc.path, Absent: true}}}
			if err := repo.CaptureDelivery(context.Background(), d); err != nil {
				t.Fatal(err)
			}
			r := repo.VerifyDelivery(context.Background(), d)
			if r.Status != tc.want {
				t.Fatalf("%s: got %s, want %s: %+v", tc.path, r.Status, tc.want, r)
			}
			if tc.want == taskstate.DeliveryUnavailable && (len(r.Issues) == 0 || r.Issues[0].Path != tc.path) {
				t.Fatalf("unavailable without a path issue: %+v", r)
			}
		})
	}
}

// A file required to exist behind an in-workspace link is read; behind an outside link it is not.
func TestDeliveryRegularFileBehindLinks(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	repo, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "real"), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "real", "f"), "inside")
	write(t, filepath.Join(outside, "f"), "outside")
	link(t, "real", filepath.Join(root, "in"))
	link(t, outside, filepath.Join(root, "out"))
	d := &taskstate.DeliveryContract{Files: []taskstate.DeliveryFile{{Path: "in/f"}}}
	if r := repo.VerifyDelivery(context.Background(), d); r.Status != taskstate.DeliveryVerified || r.Files[0].Current.Size != int64(len("inside")) {
		t.Fatalf("inside link: %+v", r)
	}
	d = &taskstate.DeliveryContract{Files: []taskstate.DeliveryFile{{Path: "out/f"}}}
	if r := repo.VerifyDelivery(context.Background(), d); r.Status != taskstate.DeliveryUnavailable {
		t.Fatalf("outside link accepted: %+v", r)
	}
}

// A committed deletion is delivered although the file's directory became a regular file.
func TestDeliveryCommittedDeletionBehindFileAncestor(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	if err := os.Mkdir(filepath.Join(root, "pkg"), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "pkg", "old.go"), "old")
	git("add", "-A")
	git("commit", "-qm", "base")
	repo, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	d := &taskstate.DeliveryContract{Files: []taskstate.DeliveryFile{{Path: "pkg/old.go", Absent: true, Changed: true}}, Commit: true}
	if err := repo.CaptureDelivery(context.Background(), d); err != nil || len(d.BaselineIssues) != 0 {
		t.Fatalf("baseline: %+v %v", d, err)
	}
	if err := os.RemoveAll(filepath.Join(root, "pkg")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "pkg"), "now a file")
	if r := repo.VerifyDelivery(context.Background(), d); r.Status != taskstate.DeliveryFailed || !strings.Contains(strings.Join(r.Missing, ";"), "commit") {
		t.Fatalf("uncommitted deletion: %+v", r)
	}
	git("add", "-A")
	git("commit", "-qm", "replace pkg")
	r := repo.VerifyDelivery(context.Background(), d)
	if r.Status != taskstate.DeliveryVerified || r.Files[0].Change != "deleted" {
		t.Fatalf("committed deletion: %+v", r)
	}
}

// One capture failure that no requirement path names is reported once, not per changed file.
func TestDeliveryGeneralBaselineIssueReportedOnce(t *testing.T) {
	repo, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := &taskstate.DeliveryContract{
		Files:          []taskstate.DeliveryFile{{Path: "a", Changed: true}, {Path: "b", Changed: true}},
		BaselineIssues: []taskstate.DeliveryIssue{{Phase: "baseline", Reason: "workspace unavailable"}},
	}
	r := repo.VerifyDelivery(context.Background(), d)
	if r.Status != taskstate.DeliveryUnavailable || len(r.Issues) != 1 || len(r.Files) != 2 || r.Files[0].Change != "unknown" || r.Files[1].Change != "unknown" {
		t.Fatalf("general baseline issue: %+v", r)
	}
}

func link(t *testing.T, target, path string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}
