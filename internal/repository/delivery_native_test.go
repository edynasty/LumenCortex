package repository

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/taskstate"
)

func TestDeliveryNativeAbsenceAndMismatch(t *testing.T) {
	for _, tc := range []struct {
		name            string
		before, after   string
		absent, changed bool
		status, change  string
	}{
		{"created", "", "new", false, true, taskstate.DeliveryVerified, "created"},
		{"unchanged", "same", "same", false, true, taskstate.DeliveryFailed, "unchanged"},
		{"existing", "same", "same", false, false, taskstate.DeliveryVerified, "unchanged"},
		{"deleted", "old", "", true, true, taskstate.DeliveryVerified, "deleted"},
		{"unexpected-delete", "old", "", false, true, taskstate.DeliveryFailed, "deleted"},
		{"not-deleted", "old", "old", true, false, taskstate.DeliveryFailed, "unchanged"},
		{"missing-baseline", "", "", true, true, taskstate.DeliveryFailed, "unchanged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			root := t.TempDir()
			file := filepath.Join(root, "artifact")
			if tc.before != "" {
				if err := os.WriteFile(file, []byte(tc.before), 0600); err != nil {
					t.Fatal(err)
				}
			}
			repo, err := New(root)
			if err != nil {
				t.Fatal(err)
			}
			d := &taskstate.DeliveryContract{Files: []taskstate.DeliveryFile{{Path: "artifact", Absent: tc.absent, Changed: tc.changed}}}
			if err := repo.CaptureDelivery(context.Background(), d); err != nil {
				t.Fatal(err)
			}
			if tc.after != "" {
				if err := os.WriteFile(file, []byte(tc.after), 0600); err != nil {
					t.Fatal(err)
				}
			} else if tc.before != "" {
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
			}
			r := repo.VerifyDelivery(context.Background(), d)
			if r.Status != tc.status || len(r.Files) != 1 || r.Files[0].Change != tc.change || len(r.Issues) != 0 {
				t.Fatalf("result=%+v", r)
			}
		})
	}
}

func TestDeliveryNativeContentHashIgnoresTimestampAndGitSizeLimit(t *testing.T) {
	root := t.TempDir()
	repo, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "large")
	before := make([]byte, MaxBlobFileBytes+1)
	if err := os.WriteFile(file, before, 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	d := &taskstate.DeliveryContract{Files: []taskstate.DeliveryFile{{Path: "large", Changed: true}}}
	t.Setenv("PATH", t.TempDir())
	if err := repo.CaptureDelivery(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	before[len(before)-1] = 1
	if err := os.WriteFile(file, before, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	r := repo.VerifyDelivery(context.Background(), d)
	if r.Status != taskstate.DeliveryVerified || r.Files[0].Change != "modified" || r.Files[0].Baseline.SHA256 == r.Files[0].Current.SHA256 {
		t.Fatalf("content change lost: %+v", r)
	}
}

func TestDeliveryNativeInspectionFailuresAreUnavailable(t *testing.T) {
	root := t.TempDir()
	repo, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "result"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	d := &taskstate.DeliveryContract{Files: []taskstate.DeliveryFile{{Path: "result", Changed: true}}}
	if err := repo.CaptureDelivery(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := repo.VerifyDelivery(ctx, d)
	if r.Status != taskstate.DeliveryUnavailable || len(r.Issues) == 0 || r.Issues[0].Path != "result" {
		t.Fatalf("cancelled hash: %+v", r)
	}
	// A historical Git baseline must not be recaptured as if it were pre-execution.
	d.Files[0].Baseline = nil
	d.Files[0].BaseBlob = "historical-git-blob"
	r = repo.VerifyDelivery(context.Background(), d)
	if r.Status != taskstate.DeliveryUnavailable || r.Issues[0].Phase != "baseline" {
		t.Fatalf("historical baseline: %+v", r)
	}
}

func TestDeliveryNativeConfinementAndFileTypes(t *testing.T) {
	root := t.TempDir()
	repo, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	d := &taskstate.DeliveryContract{Files: []taskstate.DeliveryFile{{Path: "outside/missing", Absent: true}}}
	if err := repo.CaptureDelivery(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	r := repo.VerifyDelivery(context.Background(), d)
	if r.Status != taskstate.DeliveryUnavailable {
		t.Fatalf("outside absence accepted: %+v", r)
	}
	if err := os.Mkdir(filepath.Join(root, "result"), 0700); err != nil {
		t.Fatal(err)
	}
	d = &taskstate.DeliveryContract{Files: []taskstate.DeliveryFile{{Path: "result"}}}
	if err := repo.CaptureDelivery(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	r = repo.VerifyDelivery(context.Background(), d)
	if r.Status != taskstate.DeliveryFailed || r.Files[0].Current.Kind != "directory" {
		t.Fatalf("directory accepted: %+v", r)
	}
}

// A baseline that could not be read only blocks requirements that compare against it.
func TestDeliveryBaselineIssueOnlyAffectsChangeRequirement(t *testing.T) {
	root := t.TempDir()
	repo, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "result")
	if err := os.WriteFile(file, []byte("before"), 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(file, 0600)
	if probe, err := os.Open(file); err == nil {
		probe.Close()
		t.Skip("permission failure needs non-root execution")
	}
	exists := &taskstate.DeliveryContract{Files: []taskstate.DeliveryFile{{Path: "result"}}}
	changed := &taskstate.DeliveryContract{Files: []taskstate.DeliveryFile{{Path: "result", Changed: true}}}
	for _, d := range []*taskstate.DeliveryContract{exists, changed} {
		if err := repo.CaptureDelivery(context.Background(), d); err != nil {
			t.Fatal(err)
		}
		if len(d.BaselineIssues) != 1 || d.Files[0].Baseline != nil {
			t.Fatalf("baseline failure not recorded: %+v", d)
		}
	}
	if err := os.Chmod(file, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("after"), 0600); err != nil {
		t.Fatal(err)
	}
	r := repo.VerifyDelivery(context.Background(), exists)
	if r.Status != taskstate.DeliveryVerified || r.Files[0].Change != "unknown" || len(r.Issues) != 0 {
		t.Fatalf("existence needs no baseline: %+v", r)
	}
	r = repo.VerifyDelivery(context.Background(), changed)
	if r.Status != taskstate.DeliveryUnavailable || len(r.Issues) != 1 {
		t.Fatalf("change without baseline: %+v", r)
	}
	if is := r.Issues[0]; is.Phase != "baseline" || is.Path != "result" || !strings.Contains(is.Reason, "permission denied") {
		t.Fatalf("baseline cause lost: %+v", is)
	}
}

// A regular file where a directory was is proof that nothing below it exists.
func TestDeliveryNativeFileAncestorMeansAbsent(t *testing.T) {
	root := t.TempDir()
	repo, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(root, "pkg")
	if err := os.Mkdir(pkg, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "old.go"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	d := &taskstate.DeliveryContract{Files: []taskstate.DeliveryFile{{Path: "pkg/old.go", Changed: true, Absent: true}, {Path: "pkg/sub/new.go", Absent: true}}}
	if err := repo.CaptureDelivery(context.Background(), d); err != nil || len(d.BaselineIssues) != 0 {
		t.Fatalf("baseline: %+v %v", d, err)
	}
	if err := os.RemoveAll(pkg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pkg, []byte("now a file"), 0600); err != nil {
		t.Fatal(err)
	}
	r := repo.VerifyDelivery(context.Background(), d)
	if r.Status != taskstate.DeliveryVerified || r.Files[0].Change != "deleted" || r.Files[1].Current == nil || r.Files[1].Current.Exists {
		t.Fatalf("file ancestor: %+v", r)
	}
}
