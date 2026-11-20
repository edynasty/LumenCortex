package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/internal/editing"
)

func TestR6PayloadByteBoundAndDiagnostics(t *testing.T) {
	t.Run("edit_lines_100_go_type_errors_strictly_bounded_and_sanitized", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "ws")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		origContent := "package p\n\nvar x = 1\n"
		if err := os.WriteFile(filepath.Join(root, "p.go"), []byte(origContent), 0o644); err != nil {
			t.Fatal(err)
		}
		eng, err := editing.New(editing.Options{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		set, err := New(Options{Workspace: root, Policy: PolicyWorkspace, Editing: eng, SessionID: "s-r6-f2"})
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		rd, err := set.Execute(ctx, "read_file", map[string]any{"path": "p.go", "anchors": true}, nil)
		if err != nil {
			t.Fatal(err)
		}
		rev := regexp.MustCompile(`rev=(\S+)`).FindStringSubmatch(rd.Content)[1]
		anchor := regexp.MustCompile(`(?m)^(3:[^|]+)\|`).FindStringSubmatch(rd.Content)[1]

		var b strings.Builder
		for i := 0; i < 100; i++ {
			b.WriteString("var _ int = \"s\"\n")
		}
		res, execErr := set.Execute(ctx, "edit_lines", map[string]any{
			"path": "p.go",
			"rev":  rev,
			"ops":  []map[string]any{{"op": "replace", "start": anchor, "text": b.String()}},
		}, nil)
		if execErr != nil || res.OK || res.MutatesWorkspace {
			t.Fatalf("unexpected edit_lines result: ok=%v mutates=%v err=%v", res.OK, res.MutatesWorkspace, execErr)
		}
		if len(res.Content) > maxPartialPayloadBytes {
			t.Fatalf("R5-03 violation: len=%d > bound=%d", len(res.Content), maxPartialPayloadBytes)
		}
		if !utf8.ValidString(res.Content) {
			t.Fatalf("invalid UTF-8 in payload")
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(res.Content), &obj); err != nil {
			t.Fatalf("invalid JSON payload: %v (%s)", err, res.Content)
		}
		if obj["code"] != editing.CodeType {
			t.Fatalf("expected code=%q, got %v", editing.CodeType, obj["code"])
		}
		if obj["path"] != "p.go" {
			t.Fatalf("expected sanitized path p.go, got %v", obj["path"])
		}
		omitted, ok := obj["diagnosticsOmitted"].(float64)
		if !ok || omitted <= 0 {
			t.Fatalf("expected diagnosticsOmitted > 0, got %+v", obj)
		}
		if obj["truncated"] != true {
			t.Fatalf("expected truncated=true, got %+v", obj)
		}
		if strings.Contains(res.Content, root) || strings.Contains(res.Content, base) {
			t.Fatalf("R5-04 violation: payload leaked host path: %s", res.Content)
		}
		disk, err := os.ReadFile(filepath.Join(root, "p.go"))
		if err != nil || string(disk) != origContent {
			t.Fatalf("disk modified on failed type check: %q", disk)
		}
	})

	t.Run("extreme_json_escaping_and_multibyte_utf8_stay_within_bound", func(t *testing.T) {
		root := t.TempDir()
		set, err := New(Options{Workspace: root, Policy: PolicyWorkspace, SessionID: "s-r6-esc"})
		if err != nil {
			t.Fatal(err)
		}
		escapedPath := strings.Repeat("<>&", 400) + ".go"
		multibyteErr := strings.Repeat("界<", 800)
		editErr := &editing.Error{
			Code:       editing.CodeConflict,
			Path:       escapedPath,
			CurrentRev: strings.Repeat("<", 100),
			Holder:     strings.Repeat("&", 200),
			Detail:     multibyteErr,
		}
		for i := 0; i < 40; i++ {
			editErr.Diagnostics = append(editErr.Diagnostics, editing.Diagnostic{
				Severity: editing.SeverityError,
				Code:     editing.CodeType,
				Path:     filepath.Join(root, escapedPath),
				Line:     i + 1,
				Text:     multibyteErr,
			})
		}
		rec := editing.CommitReceipt{
			Committed:    false,
			DirtyFailure: true,
			State:        editing.CommitStatePartiallyPublished,
		}
		var res editing.Result
		for i := 0; i < 40; i++ {
			p := fmt.Sprintf("sub/%s_%02d.txt", strings.Repeat("<", 40), i)
			rec.Files = append(rec.Files, editing.FileCommitReceipt{
				Path:         p,
				Published:    true,
				Wrote:        true,
				PublishState: editing.PublishRollbackFailed,
				Error:        multibyteErr,
			})
			res.Files = append(res.Files, editing.FileResult{
				Path:         p,
				Published:    true,
				PublishState: editing.PublishRollbackFailed,
				Error:        multibyteErr,
			})
		}
		out := set.formatCommitFailurePayload(res, rec, errors.New(multibyteErr), editErr)
		if len(out) > maxPartialPayloadBytes {
			t.Fatalf("payload %d bytes exceeded %d", len(out), maxPartialPayloadBytes)
		}
		if !utf8.ValidString(out) {
			t.Fatal("invalid UTF-8")
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(out), &parsed); err != nil {
			t.Fatalf("invalid JSON: %v (%s)", err, out)
		}
		if parsed["code"] != editing.CodeConflict ||
			parsed["commitState"] != string(editing.CommitStatePartiallyPublished) ||
			parsed["dirtyFailure"] != true ||
			parsed["partialPublication"] != true ||
			parsed["truncated"] != true {
			t.Fatalf("critical fields lost in bounded payload: %+v", parsed)
		}
		if strings.Contains(out, root) {
			t.Fatalf("host root leaked: %s", out)
		}
	})

	t.Run("multifile_partial_publication_retains_file_entries_by_shrinking_file_errors_first", func(t *testing.T) {
		root := t.TempDir()
		set, err := New(Options{Workspace: root, Policy: PolicyWorkspace, SessionID: "s-r6-files"})
		if err != nil {
			t.Fatal(err)
		}
		rec := editing.CommitReceipt{
			Committed:    false,
			DirtyFailure: true,
			State:        editing.CommitStatePartiallyPublished,
		}
		for i := 0; i < 24; i++ {
			rec.Files = append(rec.Files, editing.FileCommitReceipt{
				Path:         fmt.Sprintf("pkg/file_%02d.go", i),
				Published:    true,
				Wrote:        true,
				PublishState: editing.PublishExternalModification,
				Error:        strings.Repeat("rollback external modification detail ", 8),
			})
		}
		out := set.formatCommitFailurePayload(editing.Result{CommitState: editing.CommitStatePartiallyPublished}, rec, syscall.EIO, nil)
		if len(out) > maxPartialPayloadBytes {
			t.Fatalf("len=%d > %d", len(out), maxPartialPayloadBytes)
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(out), &parsed); err != nil {
			t.Fatal(err)
		}
		files, ok := parsed["files"].([]any)
		if !ok || len(files) != 24 {
			t.Fatalf("expected all 24 file state entries retained after shrinking per-file error text, got len=%d (%+v)", len(files), parsed)
		}
	})
}

func TestR6StructuredPathsAndSanitizeErrorText(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "my ws")
	outsideDir := filepath.Join(base, "my ws-sibling")
	if err := os.MkdirAll(filepath.Join(ws, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "pkg/inside.go"), []byte("package pkg\n\nvar Val = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "pkg/helper.go"), []byte("package pkg\n\nfunc getVal() int { return Val }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideDir, "secret.txt"), []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Symlinks: one pointing inside ws, one escaping outside ws.
	inLink := filepath.Join(ws, "in_link")
	escapeLink := filepath.Join(ws, "escape_link")
	_ = os.Symlink(filepath.Join(ws, "pkg"), inLink)
	_ = os.Symlink(outsideDir, escapeLink)

	eng, err := editing.New(editing.Options{Root: ws})
	if err != nil {
		t.Fatal(err)
	}
	set, err := New(Options{Workspace: ws, Policy: PolicyWorkspace, Editing: eng, SessionID: "s-r6-paths"})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("structured_path_classification_matrix", func(t *testing.T) {
		cases := []struct {
			name string
			in   string
			want string
		}{
			{"empty", "", ""},
			{"dot", ".", "."},
			{"ws_root_abs", ws, "."},
			{"real_ws_root_abs", set.realWorkspace, "."},
			{"inside_rel", "pkg/inside.go", "pkg/inside.go"},
			{"inside_abs", filepath.Join(ws, "pkg/inside.go"), "pkg/inside.go"},
			{"inside_nonexistent", filepath.Join(ws, "pkg/future/new_file.go"), "pkg/future/new_file.go"},
			{"inside_symlink", filepath.Join(ws, "in_link/inside.go"), "in_link/inside.go"},
			{"escaping_symlink", filepath.Join(ws, "escape_link/secret.txt"), "<outside-workspace>/secret.txt"},
			{"escaping_symlink_rel", "escape_link/secret.txt", "<outside-workspace>/secret.txt"},
			{"sibling_prefix_overlap", filepath.Join(outsideDir, "secret.txt"), "<outside-workspace>/secret.txt"},
			{"dotdot_escape", "../../../../Users/alice/.ssh/id_ed25519", "<outside-workspace>/id_ed25519"},
			{"windows_drive_outside", `C:\Users\alice\secrets\token.json`, "<outside-workspace>/token.json"},
			{"unc_outside", `\\corp-fs\private\payroll.csv`, "<outside-workspace>/payroll.csv"},
		}
		for _, tc := range cases {
			got := set.sanitizeStructuredPath(tc.in)
			if got != tc.want {
				t.Fatalf("%s: sanitizeStructuredPath(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
			}
			if strings.Contains(got, base) || strings.Contains(got, ".-sibling") {
				t.Fatalf("%s: leaked base or corrupted sibling in %q", tc.name, got)
			}
		}
	})

	t.Run("unstructured_error_text_boundary_sanitization_r5_05", func(t *testing.T) {
		siblingFile := filepath.Join(outsideDir, "secret.txt")
		insideFile := filepath.Join(ws, "pkg/inside.go")
		rawMsg := fmt.Sprintf("open %s: denied; compiler %s:12:4: ratio 3/4 over HTTP/1.1 at https://example.com/a/b; root=%s; win=C:\\Users\\bob\\ws-sibling\\key.pem; unc=\\\\srv\\share\\secret.db",
			siblingFile, insideFile, ws)

		got := set.sanitizeErrorText(rawMsg)
		if strings.Contains(got, ".-sibling") {
			t.Fatalf("R5-05 regression: sibling path corrupted into .-sibling: %q", got)
		}
		if strings.Contains(got, base) || strings.Contains(got, ws) || strings.Contains(got, set.realWorkspace) {
			t.Fatalf("R5-04 regression: host workspace or temp base leaked: %q", got)
		}
		for _, wantSub := range []string{
			"open <outside-workspace>/secret.txt: denied",
			"compiler pkg/inside.go:12:4:",
			"ratio 3/4 over HTTP/1.1 at https://example.com/a/b;",
			"root=.;",
			"win=<outside-workspace>/key.pem;",
			"unc=<outside-workspace>/secret.db",
		} {
			if !strings.Contains(got, wantSub) {
				t.Fatalf("missing expected sanitized fragment %q in %q", wantSub, got)
			}
		}
	})

	t.Run("windows_drive_and_unc_workspace_boundaries", func(t *testing.T) {
		winSet := &Set{
			workspace:     `C:\Users\Alice\Repo`,
			realWorkspace: `C:\Users\Alice\Repo`,
		}
		if got := winSet.sanitizeStructuredPath(`c:\users\alice\repo\internal\app.go`); got != "internal/app.go" {
			t.Fatalf("win inside = %q, want internal/app.go", got)
		}
		if got := winSet.sanitizeStructuredPath(`C:\Users\Alice\Repo`); got != "." {
			t.Fatalf("win root = %q, want .", got)
		}
		if got := winSet.sanitizeStructuredPath(`C:\Users\Alice\Repo-sibling\secret.env`); got != "<outside-workspace>/secret.env" {
			t.Fatalf("win sibling = %q, want <outside-workspace>/secret.env", got)
		}
		if gotErr := winSet.sanitizeErrorText(`failed opening C:\Users\Alice\Repo-sibling\secret.env and C:\Users\Alice\Repo\cmd\main.go:9`); gotErr != "failed opening <outside-workspace>/secret.env and cmd/main.go:9" {
			t.Fatalf("win error text = %q", gotErr)
		}

		uncSet := &Set{
			workspace:     `\\fileserver\TeamShare\ws`,
			realWorkspace: `\\fileserver\TeamShare\ws`,
		}
		if got := uncSet.sanitizeStructuredPath(`\\FILESERVER\teamshare\ws\sub\mod.go`); got != "sub/mod.go" {
			t.Fatalf("unc inside = %q, want sub/mod.go", got)
		}
		if got := uncSet.sanitizeStructuredPath(`\\fileserver\TeamShare\ws-sibling\creds.json`); got != "<outside-workspace>/creds.json" {
			t.Fatalf("unc sibling = %q, want <outside-workspace>/creds.json", got)
		}
		if gotErr := uncSet.sanitizeErrorText(`read \\fileserver\TeamShare\ws-sibling\creds.json failed`); gotErr != "read <outside-workspace>/creds.json failed" {
			t.Fatalf("unc error text = %q", gotErr)
		}
	})

	t.Run("unknown_rev_and_sibling_gotype_diagnostics_do_not_leak_host_paths", func(t *testing.T) {
		ctx := context.Background()
		// 1. Unknown rev refusal from SnapshotFor(s.actor(), file, "bogus")
		resUnknown, err := set.Execute(ctx, "edit_lines", map[string]any{
			"path": "pkg/inside.go",
			"rev":  "bogusrev",
			"ops":  []map[string]any{{"op": "replace", "start": "1:aaaa", "text": "package pkg"}},
		}, nil)
		if err != nil || resUnknown.OK {
			t.Fatalf("expected OK:false unknown_rev result, got res=%+v err=%v", resUnknown, err)
		}
		if strings.Contains(resUnknown.Content, ws) || strings.Contains(resUnknown.Content, base) {
			t.Fatalf("unknown_rev leaked host path: %s", resUnknown.Content)
		}
		var unkObj map[string]any
		if err := json.Unmarshal([]byte(resUnknown.Content), &unkObj); err != nil {
			t.Fatal(err)
		}
		if unkObj["path"] != "pkg/inside.go" {
			t.Fatalf("unknown_rev path = %v, want pkg/inside.go", unkObj["path"])
		}

		// 2. Sibling file pkg/helper.go type error triggered by editing pkg/inside.go
		rd, err := set.Execute(ctx, "read_file", map[string]any{"path": "pkg/inside.go", "anchors": true}, nil)
		if err != nil {
			t.Fatal(err)
		}
		rev := regexp.MustCompile(`rev=(\S+)`).FindStringSubmatch(rd.Content)[1]
		anchor := regexp.MustCompile(`(?m)^(3:[^|]+)\|`).FindStringSubmatch(rd.Content)[1]
		resType, err := set.Execute(ctx, "edit_lines", map[string]any{
			"path": "pkg/inside.go",
			"rev":  rev,
			"ops":  []map[string]any{{"op": "replace", "start": anchor, "text": "var Val = \"wrong-type\""}},
		}, nil)
		if err != nil || resType.OK {
			t.Fatalf("expected type error result, got res=%+v err=%v", resType, err)
		}
		if strings.Contains(resType.Content, ws) || strings.Contains(resType.Content, base) {
			t.Fatalf("sibling GoTypes diagnostic leaked host path: %s", resType.Content)
		}
		var typeObj map[string]any
		if err := json.Unmarshal([]byte(resType.Content), &typeObj); err != nil {
			t.Fatal(err)
		}
		if typeObj["path"] != "pkg/helper.go" {
			t.Fatalf("expected sibling diagnostic path pkg/helper.go, got %v", typeObj["path"])
		}
	})
}
