package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/edynasty/LumenCortex/internal/editing"
)

type r7r2Payload struct {
	Error string `json:"error"`
	Path  string `json:"path"`
	Files []struct {
		Path         string `json:"path"`
		PublishState string `json:"publishState"`
		Verified     bool   `json:"verified"`
	} `json:"files"`
	FilesOmitted        int            `json:"filesOmitted"`
	FilesOmittedByState map[string]int `json:"filesOmittedByState"`
	Truncated           bool           `json:"truncated"`
}

// r7r2DeepBatch runs apply_patch over n files that share a deep package prefix; the file at failAt
// fails after its write with failErr, which names no path.
func r7r2DeepBatch(t *testing.T, n, failAt int, failErr error) (string, []string) {
	t.Helper()
	const pkg = "src/main/kotlin/com/example/platform/payments/gateway/adapters/stripe/"
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(filepath.Join(ws, pkg), 0o755); err != nil {
		t.Fatal(err)
	}
	rels := make([]string, n)
	var patches []any
	for i := range rels {
		rels[i] = fmt.Sprintf("%sStripeAdapterComponent%02d.kt", pkg, i)
		if err := os.WriteFile(filepath.Join(ws, rels[i]), []byte(fmt.Sprintf("old-%03d\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		patches = append(patches, map[string]any{"path": rels[i], "operation": "update",
			"edits": []any{map[string]any{"old_text": fmt.Sprintf("old-%03d", i), "new_text": fmt.Sprintf("new-%03d", i)}}})
	}
	eng, err := editing.New(editing.Options{Root: ws})
	if err != nil {
		t.Fatal(err)
	}
	set, err := New(Options{Workspace: ws, Policy: PolicyFull, SessionID: "s-r7r2", Editing: eng})
	if err != nil {
		t.Fatal(err)
	}
	failName := filepath.Base(rels[failAt])
	eng.SetPublishFaultHooks(editing.PublishFaultHooks{AfterInPlaceWriteClose: func(dst string) error {
		if filepath.Base(dst) == failName {
			return failErr
		}
		return nil
	}})
	res, _ := set.Execute(context.Background(), "apply_patch", map[string]any{"patches": patches}, nil)
	return res.Content, rels
}

// TestR7R2DeepPathRowsStayIdentifiable reproduces R7R2-Results-01: with many deep paths the shrink
// loop cut every path to one shared prefix before it dropped rows, so the failed file vanished.
func TestR7R2DeepPathRowsStayIdentifiable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		n      int
		failAt int
		err    error
	}{
		{"eio_n16", 16, 8, syscall.EIO},
		{"eio_n20", 20, 10, syscall.EIO},
		{"eio_n32_last", 32, 31, syscall.EIO},
		{"cancelled_n20", 20, 10, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content, rels := r7r2DeepBatch(t, tc.n, tc.failAt, tc.err)
			if !json.Valid([]byte(content)) {
				t.Fatalf("invalid JSON: %s", content)
			}
			if len(content) > maxPartialPayloadBytes {
				t.Fatalf("payload %d bytes exceeds %d", len(content), maxPartialPayloadBytes)
			}
			var p r7r2Payload
			if err := json.Unmarshal([]byte(content), &p); err != nil {
				t.Fatal(err)
			}
			failed := rels[tc.failAt]
			if len(p.Files) == 0 || p.Files[0].Path != failed || p.Files[0].Verified {
				t.Fatalf("failed row is not first with its exact path %q: %+v", failed, p.Files)
			}
			known := map[string]bool{}
			for _, r := range rels {
				known[r] = true
			}
			seen := map[string]bool{}
			for _, f := range p.Files {
				if !known[f.Path] {
					t.Fatalf("row path %q is not an exact requested path", f.Path)
				}
				if seen[f.Path] {
					t.Fatalf("row path %q repeated", f.Path)
				}
				seen[f.Path] = true
			}
			if len(p.Files)+p.FilesOmitted != tc.n {
				t.Fatalf("rows %d + omitted %d != %d", len(p.Files), p.FilesOmitted, tc.n)
			}
			byState := 0
			for _, c := range p.FilesOmittedByState {
				byState += c
			}
			if byState != p.FilesOmitted {
				t.Fatalf("filesOmittedByState %v does not account for %d omitted rows", p.FilesOmittedByState, p.FilesOmitted)
			}
			if p.FilesOmitted > 0 && !p.Truncated {
				t.Fatal("omitted rows without truncated:true")
			}
		})
	}
}

// TestR7R2OversizedFailedPathKeepsFilename: a failed path too long for the envelope keeps its
// filename; the shortened form is the path's tail, never a shared prefix.
func TestR7R2OversizedFailedPathKeepsFilename(t *testing.T) {
	root := t.TempDir()
	set, err := New(Options{Workspace: root, Policy: PolicyWorkspace, SessionID: "s-r7r2-long"})
	if err != nil {
		t.Fatal(err)
	}
	deep := strings.Repeat("very-long-directory-name-segment/", 120)
	failed := deep + "FailedTarget.kt"
	files := []editing.FileCommitReceipt{
		{Path: failed, PublishState: editing.PublishSucceededWithPostError, Published: true, Wrote: true, Error: "input/output error"},
	}
	results := []editing.FileResult{
		{Path: failed, PublishState: editing.PublishSucceededWithPostError, Published: true, Error: "input/output error"},
	}
	for i := 0; i < 24; i++ {
		rel := fmt.Sprintf("%sSibling%02d.kt", deep, i)
		files = append(files, editing.FileCommitReceipt{Path: rel, PublishState: editing.PublishNotAttempted})
		results = append(results, editing.FileResult{Path: rel, PublishState: editing.PublishNotAttempted})
	}
	receipt := editing.CommitReceipt{DirtyFailure: true, State: editing.CommitStatePublishedWithPostError, Files: files}
	result := editing.Result{CommitState: editing.CommitStatePublishedWithPostError, Files: results}
	res, _ := set.commitOutcome(result, receipt, errors.New("input/output error"))
	if !json.Valid([]byte(res.Content)) || len(res.Content) > maxPartialPayloadBytes {
		t.Fatalf("payload invalid or %d bytes over budget", len(res.Content))
	}
	var p r7r2Payload
	if err := json.Unmarshal([]byte(res.Content), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Files) == 0 || !strings.HasSuffix(p.Files[0].Path, "/FailedTarget.kt") {
		t.Fatalf("failed row lost its filename: %+v", p.Files)
	}
	if p.Files[0].PublishState != string(editing.PublishSucceededWithPostError) {
		t.Fatalf("failed row state: %+v", p.Files[0])
	}
	for _, f := range p.Files[1:] {
		if !strings.Contains(f.Path, "Sibling") {
			t.Fatalf("row %q lost its filename", f.Path)
		}
	}
	if !p.Truncated {
		t.Fatal("shortened failed path without truncated:true")
	}
}
