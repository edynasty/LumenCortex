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
	"time"

	"github.com/edynasty/LumenCortex/internal/editing"
)

// TestR7FailurePayloadEveryReceiptState checks the R7 failure envelope: files exists for all receipt
// states, the failed path and verified prefix are ordered first, publishedFiles lists only paths LC
// really published, verified rows carry their actual revision/hash, and a clean external conflict
// without an LC write is not dirty.
func TestR7FailurePayloadEveryReceiptState(t *testing.T) {
	root := t.TempDir()
	set, err := New(Options{Workspace: root, Policy: PolicyWorkspace, SessionID: "s-r7-payload"})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("partial_failed_path_first_verified_effect_and_recovery", func(t *testing.T) {
		receipt := editing.CommitReceipt{
			Committed:    false,
			DirtyFailure: true,
			State:        editing.CommitStatePartiallyPublished,
			Files: []editing.FileCommitReceipt{
				{Path: "a.txt", Published: true, Verified: true, Wrote: true,
					PublishState: editing.PublishSucceededAndVerified, NewSHA256: "sha256:aaa"},
				{Path: "b.txt", PublishState: editing.PublishFailedBeforeWrite, Error: "read-only file system"},
				{Path: "c.txt", PublishState: editing.PublishNotAttempted},
			},
		}
		result := editing.Result{
			CommitState: editing.CommitStatePartiallyPublished,
			Files: []editing.FileResult{
				{Path: "a.txt", Published: true, Verified: true, Changed: true, PublishState: editing.PublishSucceededAndVerified, NewRev: "rev-a"},
				{Path: "b.txt", PublishState: editing.PublishFailedBeforeWrite, Error: "read-only file system"},
				{Path: "c.txt", PublishState: editing.PublishNotAttempted},
			},
		}
		res, outErr := set.commitOutcome(result, receipt, errors.New("write b.txt: read-only file system"))
		if outErr == nil || res.OK || !res.MutatesWorkspace {
			t.Fatalf("unexpected outcome: res=%+v err=%v", res, outErr)
		}
		var payload struct {
			CommitState        string `json:"commitState"`
			DirtyFailure       bool   `json:"dirtyFailure"`
			PartialPublication bool   `json:"partialPublication"`
			Recovery           string `json:"recovery"`
			Files              []struct {
				Path         string `json:"path"`
				PublishState string `json:"publishState"`
				Published    bool   `json:"published"`
				Verified     bool   `json:"verified"`
				NewRev       string `json:"newRev"`
				SHA256       string `json:"sha256"`
				Error        string `json:"error"`
			} `json:"files"`
			PublishedFiles []editing.FileResult `json:"publishedFiles"`
		}
		if err := json.Unmarshal([]byte(res.Content), &payload); err != nil {
			t.Fatalf("invalid payload: %v (%s)", err, res.Content)
		}
		if payload.CommitState != string(editing.CommitStatePartiallyPublished) || !payload.DirtyFailure || !payload.PartialPublication {
			t.Fatalf("state fields lost: %+v", payload)
		}
		if payload.Recovery == "" || !strings.Contains(strings.ToLower(payload.Recovery), "re-read") {
			t.Fatalf("missing explicit recovery guidance: %+v", payload)
		}
		if len(payload.Files) != 3 {
			t.Fatalf("files must cover every receipt state, got %+v", payload.Files)
		}
		if payload.Files[0].Path != "b.txt" || payload.Files[0].PublishState != string(editing.PublishFailedBeforeWrite) {
			t.Fatalf("failed path first: %+v", payload.Files)
		}
		if payload.Files[1].Path != "a.txt" || !payload.Files[1].Verified || payload.Files[1].NewRev != "rev-a" || payload.Files[1].SHA256 != "sha256:aaa" {
			t.Fatalf("verified prefix effect: %+v", payload.Files[1])
		}
		if payload.Files[2].Path != "c.txt" || payload.Files[2].PublishState != string(editing.PublishNotAttempted) {
			t.Fatalf("not-attempted suffix: %+v", payload.Files[2])
		}
		if len(payload.PublishedFiles) != 1 || payload.PublishedFiles[0].Path != "a.txt" {
			t.Fatalf("publishedFiles must list only genuinely published paths: %+v", payload.PublishedFiles)
		}
		if strings.Contains(res.Content, root) {
			t.Fatalf("host root leaked: %s", res.Content)
		}
	})

	t.Run("clean_external_conflict_is_not_dirty_without_an_effect", func(t *testing.T) {
		receipt := editing.CommitReceipt{
			Committed: false,
			State:     editing.CommitStatePrePublishFailure,
			Files: []editing.FileCommitReceipt{
				{Path: "b.txt", PublishState: editing.PublishExternalModification, Error: "changed after validation"},
			},
		}
		result := editing.Result{
			CommitState: editing.CommitStatePrePublishFailure,
			Files: []editing.FileResult{
				{Path: "b.txt", PublishState: editing.PublishExternalModification, Error: "changed after validation"},
			},
		}
		res, outErr := set.commitOutcome(result, receipt, &editing.Error{Code: editing.CodeConflict, Path: "b.txt", Detail: "the file changed; nothing was written"})
		if outErr != nil || res.OK || res.MutatesWorkspace {
			t.Fatalf("clean conflict must not mutate: res=%+v err=%v", res, outErr)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(res.Content), &payload); err != nil {
			t.Fatalf("invalid payload: %v (%s)", err, res.Content)
		}
		if payload["dirtyFailure"] != false || payload["partialPublication"] != false {
			t.Fatalf("clean conflict classified dirty: %+v", payload)
		}
		files, ok := payload["files"].([]any)
		if !ok || len(files) != 1 {
			t.Fatalf("files must exist for a clean receipt state: %+v", payload)
		}
		if _, present := payload["publishedFiles"]; present {
			t.Fatalf("publishedFiles listed a path LC never published: %+v", payload)
		}
	})

	t.Run("truncation_keeps_the_failed_path", func(t *testing.T) {
		receipt := editing.CommitReceipt{
			DirtyFailure: true,
			State:        editing.CommitStatePartiallyPublished,
		}
		for i := range 40 {
			receipt.Files = append(receipt.Files, editing.FileCommitReceipt{
				Path:         fmt.Sprintf("aaa/not_attempted_%02d.txt", i),
				PublishState: editing.PublishNotAttempted,
			})
		}
		receipt.Files = append(receipt.Files, editing.FileCommitReceipt{
			Path:         "zzz/failed.txt",
			PublishState: editing.PublishFailedBeforeWrite,
			Error:        "read-only file system",
		})
		out := set.formatCommitFailurePayload(editing.Result{CommitState: editing.CommitStatePartiallyPublished}, receipt, syscall.EIO, nil)
		if len(out) > maxPartialPayloadBytes {
			t.Fatalf("payload %d exceeds %d", len(out), maxPartialPayloadBytes)
		}
		var payload struct {
			Files []struct {
				Path string `json:"path"`
			} `json:"files"`
			FilesOmitted int `json:"filesOmitted"`
		}
		if err := json.Unmarshal([]byte(out), &payload); err != nil {
			t.Fatalf("invalid payload: %v (%s)", err, out)
		}
		if len(payload.Files) == 0 || payload.Files[0].Path != "zzz/failed.txt" {
			t.Fatalf("failed path was not prioritized: %+v", payload.Files)
		}
		if payload.FilesOmitted <= 0 {
			t.Fatalf("omission total missing: %+v", payload)
		}
	})
}

// TestR7GenericLSPErrorScrubsHostPaths checks a language-server err.Error() (not a structured
// *os.PathError) loses the host root in both plain and file:// forms.
func TestR7GenericLSPErrorScrubsHostPaths(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "my ws")
	if err := os.MkdirAll(filepath.Join(ws, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(ws, "pkg", "inside.go")
	if err := os.WriteFile(target, []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	set, err := New(Options{Workspace: ws, Policy: PolicyWorkspace, SessionID: "s-r7-lsp"})
	if err != nil {
		t.Fatal(err)
	}

	// A generic preflight error that leaves a file tool through the ordinary error return must not
	// carry the host root, and must keep its errors.Is classification.
	wrapped := set.sanitizeToolError(&os.PathError{Op: "open", Path: target, Err: os.ErrPermission})
	if strings.Contains(wrapped.Error(), ws) || strings.Contains(wrapped.Error(), base) {
		t.Fatalf("preflight error leaked host root: %q", wrapped.Error())
	}
	if !errors.Is(wrapped, os.ErrPermission) || !strings.Contains(wrapped.Error(), "pkg/inside.go") {
		t.Fatalf("preflight error lost meaning: %q", wrapped.Error())
	}

	serverErr := fmt.Errorf("language server failed: open %s: denied (target file://%s)", target, target)
	result := set.lspFailure(context.Background(), "lsp_hover", serverErr, time.Now())
	if result.OK {
		t.Fatalf("expected failure result: %+v", result)
	}
	var payload struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal([]byte(result.Content), &payload); err != nil {
		t.Fatalf("invalid LSP failure JSON: %v (%s)", err, result.Content)
	}
	if strings.Contains(payload.Error, ws) || strings.Contains(payload.Error, base) {
		t.Fatalf("LSP error leaked host root: %q", payload.Error)
	}
	for _, want := range []string{"pkg/inside.go", "file://pkg/inside.go", "denied"} {
		if !strings.Contains(payload.Error, want) {
			t.Fatalf("missing %q in %q", want, payload.Error)
		}
	}
	if payload.Code != "failed" {
		t.Fatalf("classification lost: %+v", payload)
	}
}

// TestR7StructuredPathSpacesAndPlatformMatrix extends the R6 matrix with spaced roots, trailing
// separators, dangling symlinks and fail-closed relative escapes.
func TestR7StructuredPathSpacesAndPlatformMatrix(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "my ws")
	outside := filepath.Join(base, "my ws-sibling")
	if err := os.MkdirAll(filepath.Join(ws, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(ws, "dangling")
	if err := os.Symlink(filepath.Join(outside, "missing"), dangling); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	set, err := New(Options{Workspace: ws, Policy: PolicyWorkspace, SessionID: "s-r7-paths"})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"workspace_root", ws, "."},
		{"workspace_root_trailing_sep", ws + string(filepath.Separator), "."},
		{"real_root", set.realWorkspace, "."},
		{"relative_inside_with_space", "pkg/a b.go", "pkg/a b.go"},
		{"absolute_inside_with_space", filepath.Join(ws, "pkg/a b.go"), "pkg/a b.go"},
		{"nonexistent_ancestors", filepath.Join(ws, "new dir/deeper/x.go"), "new dir/deeper/x.go"},
		{"dangling_symlink_fails_closed", filepath.Join(dangling, "x.go"), "<outside-workspace>/x.go"},
		{"relative_dotdot", "../my ws-sibling/secret.txt", "<outside-workspace>/secret.txt"},
		{"spaced_sibling", filepath.Join(outside, "secret.txt"), "<outside-workspace>/secret.txt"},
		{"windows_outside", `c:\users\alice\secret\token.json`, "<outside-workspace>/token.json"},
		{"unc_outside", `\\srv\share\secret\cred.db`, "<outside-workspace>/cred.db"},
	}
	for _, tc := range cases {
		got := set.sanitizeStructuredPath(tc.in)
		if got != tc.want {
			t.Fatalf("%s: sanitizeStructuredPath(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
		if strings.Contains(got, base) || strings.Contains(got, ".-sibling") {
			t.Fatalf("%s: leaked host base or corrupted sibling: %q", tc.name, got)
		}
	}

	// Windows and UNC workspaces are recognized even on a POSIX test host.
	winSet := &Set{workspace: `C:\Users\Alice\My Ws`, realWorkspace: `C:\Users\Alice\My Ws`}
	if got := winSet.sanitizeStructuredPath(`C:\Users\Alice\My Ws\pkg\a b.go`); got != "pkg/a b.go" {
		t.Fatalf("windows workspace with space: %q", got)
	}
	uncSet := &Set{workspace: `\\srv\share\my ws`, realWorkspace: `\\srv\share\my ws`}
	if got := uncSet.sanitizeStructuredPath(`\\SRV\Share\My Ws\pkg\a.go`); got != "pkg/a.go" {
		t.Fatalf("unc workspace case fold: %q", got)
	}
}

// TestR7EditSpecsDoNotClaimAllOrNone checks the model-facing descriptions after the R7 cutover.
func TestR7EditSpecsDoNotClaimAllOrNone(t *testing.T) {
	root := t.TempDir()
	set, err := New(Options{Workspace: root, Policy: PolicyWorkspace, SessionID: "s-r7-specs"})
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range set.Specs(nil) {
		switch spec.Name {
		case "edit_lines", "edit_go", "apply_patch", "write_file", "replace_in_file", "lsp_rename":
		default:
			continue
		}
		lower := strings.ToLower(spec.Description)
		for _, forbidden := range []string{"all-or-none", "all or none", "atomic batch", "rolls back", "roll back"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("%s description claims %q: %q", spec.Name, forbidden, spec.Description)
			}
		}
	}
	if spec, ok := specOf(set, "apply_patch"); ok && !strings.Contains(spec.Description, "prefix") {
		t.Fatalf("apply_patch must name the preserved prefix: %q", spec.Description)
	}
}
