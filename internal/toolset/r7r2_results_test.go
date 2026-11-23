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
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/internal/editing"
)

type r7r2Payload struct {
	Error string `json:"error"`
	Code  string `json:"code"`
	Path  string `json:"path"`
	Files []struct {
		Path         string `json:"path"`
		PublishState string `json:"publishState"`
		Published    bool   `json:"published"`
		Verified     bool   `json:"verified"`
		Wrote        bool   `json:"wrote"`
	} `json:"files"`
	FilesOmitted        int            `json:"filesOmitted"`
	FilesOmittedByState map[string]int `json:"filesOmittedByState"`
	CommitState         string         `json:"commitState"`
	DirtyFailure        bool           `json:"dirtyFailure"`
	PartialPublication  bool           `json:"partialPublication"`
	Truncated           bool           `json:"truncated"`
	Overflow            bool           `json:"overflow"`
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

func r7r2SetupLongDirFiles(t *testing.T, rels []string) (*Set, *editing.Engine, string) {
	t.Helper()
	ws := filepath.Join(t.TempDir(), "ws")
	for i, rel := range rels {
		full := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(fmt.Sprintf("OLD_%02d\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	eng, err := editing.New(editing.Options{Root: ws})
	if err != nil {
		t.Fatal(err)
	}
	set, err := New(Options{Workspace: ws, Policy: PolicyFull, SessionID: "s-r7r2-new01", Editing: eng})
	if err != nil {
		t.Fatal(err)
	}
	return set, eng, ws
}

// r7r2KeepInPlace hard-links rel outside the workspace. A multi-link file keeps its shared inode and
// is updated in place, the only path where an AfterTruncate fault still yields a partial publication
// (single-link files are staged and swapped, so the same fault is a clean pre-write refusal).
func r7r2KeepInPlace(t *testing.T, ws, rel string) {
	t.Helper()
	full := filepath.Join(ws, filepath.FromSlash(rel))
	if err := os.Link(full, filepath.Join(filepath.Dir(ws), "inplace-alias-"+filepath.Base(full))); err != nil {
		t.Fatal(err)
	}
}

func r7r2ParseAndCheckBudget(t *testing.T, label, content string, ok bool) r7r2Payload {
	t.Helper()
	if ok {
		t.Fatalf("%s: expected tool failure, got OK=true", label)
	}
	if !utf8.ValidString(content) {
		t.Fatalf("%s: payload is not valid UTF-8", label)
	}
	if !json.Valid([]byte(content)) {
		t.Fatalf("%s: payload is not valid JSON: %s", label, content)
	}
	if len(content) > maxPartialPayloadBytes {
		t.Fatalf("%s: payload %d bytes exceeds %d limit", label, len(content), maxPartialPayloadBytes)
	}
	var p r7r2Payload
	if err := json.Unmarshal([]byte(content), &p); err != nil {
		t.Fatalf("%s: unmarshal failed: %v", label, err)
	}
	byState := 0
	for _, n := range p.FilesOmittedByState {
		byState += n
	}
	if byState != p.FilesOmitted {
		t.Fatalf("%s: filesOmittedByState %v sums to %d, want %d", label, p.FilesOmittedByState, byState, p.FilesOmitted)
	}
	return p
}

// TestR7R2New01FiveFailedLongDirOverflowKeepsAllFailedIdentities reproduces R7R2-NEW-01:
// 5 failed files under ~780-byte directory components with bare syscall.EIO must keep all 5
// failed rows identifiable inside the 4096-byte JSON payload instead of collapsing to 1 row.
func TestR7R2New01FiveFailedLongDirOverflowKeepsAllFailedIdentities(t *testing.T) {
	longDir := strings.Repeat("d", 190) + "/" + strings.Repeat("e", 190) + "/" + strings.Repeat("f", 190) + "/" + strings.Repeat("g", 190)
	rels := make([]string, 6)
	var patches []any
	for i := range rels {
		rels[i] = fmt.Sprintf("%s/Adapter%02d.kt", longDir, i)
		patches = append(patches, map[string]any{
			"path":      rels[i],
			"operation": "update",
			"edits":     []any{map[string]any{"old_text": fmt.Sprintf("OLD_%02d", i), "new_text": fmt.Sprintf("NEW_%02d", i)}},
		})
	}
	set, eng, ws := r7r2SetupLongDirFiles(t, rels)
	r7r2KeepInPlace(t, ws, rels[4])
	eng.SetPublishFaultHooks(editing.PublishFaultHooks{
		AfterTruncate: func(abs string) error {
			if filepath.Base(abs) == "Adapter04.kt" {
				for j := 0; j < 4; j++ {
					_ = os.WriteFile(filepath.Join(ws, filepath.FromSlash(rels[j])), []byte("EXT\n"), 0o644)
				}
				return syscall.EIO
			}
			return nil
		},
	})
	res, err := set.Execute(context.Background(), "apply_patch", map[string]any{"patches": patches}, nil)
	if err != nil {
		t.Fatalf("unexpected go error: %v", err)
	}
	p := r7r2ParseAndCheckBudget(t, "five_failed_longdir", res.Content, res.OK)
	if !res.MutatesWorkspace || !p.DirtyFailure || !p.PartialPublication || !p.Truncated {
		t.Fatalf("expected dirty partial failure flags, got mut=%v payload=%+v", res.MutatesWorkspace, p)
	}
	if p.CommitState != string(editing.CommitStatePartiallyPublished) {
		t.Fatalf("commitState=%q, want %q", p.CommitState, editing.CommitStatePartiallyPublished)
	}
	// OV-06: the untouched row is lower-class and may use leftover budget, so failed rows are counted apart.
	if ovFailedShown(p) != 5 || len(p.Files)+p.FilesOmitted != 6 {
		t.Fatalf("expected all 5 failed rows preserved with accounting for 6 receipt rows, got len=%d omitted=%d byState=%v content=%s",
			len(p.Files), p.FilesOmitted, p.FilesOmittedByState, res.Content)
	}
	wantStates := map[string]string{
		"/Adapter00.kt": string(editing.PublishExternalModification),
		"/Adapter01.kt": string(editing.PublishExternalModification),
		"/Adapter02.kt": string(editing.PublishExternalModification),
		"/Adapter03.kt": string(editing.PublishExternalModification),
		"/Adapter04.kt": string(editing.PublishPartial),
	}
	for _, f := range p.Files {
		// OV-06: an untouched lower-class row may fill leftover budget; only failed rows are identified here.
		if f.PublishState == string(editing.PublishNotAttempted) {
			continue
		}
		var matchedSuffix string
		for suffix := range wantStates {
			if strings.HasSuffix(f.Path, suffix) {
				matchedSuffix = suffix
				break
			}
		}
		if matchedSuffix == "" {
			t.Fatalf("unidentifiable or duplicate row path %q in %+v", f.Path, p.Files)
		}
		if f.PublishState != wantStates[matchedSuffix] || f.Verified {
			t.Fatalf("row %+v has state %q, want %q with verified=false", f, f.PublishState, wantStates[matchedSuffix])
		}
		delete(wantStates, matchedSuffix)
	}
	if len(wantStates) != 0 {
		t.Fatalf("missing failed identities: %v", wantStates)
	}
}

// TestR7R2New01OverflowBoundaryMatrix covers single/multiple failures, mixed states, >=5 failed
// rows, Unicode/long filenames, identical basenames in different directories, bare errno,
// pathless cancellation, budget boundaries, and impossible-to-fit omission disclosure.
func TestR7R2New01OverflowBoundaryMatrix(t *testing.T) {
	longDir := strings.Repeat("d", 185) + "/" + strings.Repeat("e", 185) + "/" + strings.Repeat("f", 185) + "/" + strings.Repeat("g", 185)

	t.Run("single_failed_long_path_bare_eio", func(t *testing.T) {
		var rels []string
		var patches []any
		for i := 0; i < 11; i++ {
			rel := fmt.Sprintf("%s/Item%02d.kt", longDir, i)
			rels = append(rels, rel)
			patches = append(patches, map[string]any{
				"path": rel, "operation": "update",
				"edits": []any{map[string]any{"old_text": fmt.Sprintf("OLD_%02d", i), "new_text": fmt.Sprintf("NEW_%02d", i)}},
			})
		}
		set, eng, ws := r7r2SetupLongDirFiles(t, rels)
		r7r2KeepInPlace(t, ws, rels[5])
		eng.SetPublishFaultHooks(editing.PublishFaultHooks{
			AfterTruncate: func(abs string) error {
				if filepath.Base(abs) == "Item05.kt" {
					return syscall.EIO
				}
				return nil
			},
		})
		res, _ := set.Execute(context.Background(), "apply_patch", map[string]any{"patches": patches}, nil)
		p := r7r2ParseAndCheckBudget(t, "single_failed_long_path", res.Content, res.OK)
		if len(p.Files) == 0 || !strings.HasSuffix(p.Files[0].Path, "/Item05.kt") || p.Files[0].PublishState != string(editing.PublishPartial) {
			t.Fatalf("failed row lost or wrong state: %+v", p.Files)
		}
	})

	t.Run("mixed_failed_unicode_longname_and_identical_basenames", func(t *testing.T) {
		unicodeDir := strings.Repeat("d", 155) + "/" + strings.Repeat("e", 155) + "/" + strings.Repeat("f", 155) + "/" + strings.Repeat("g", 155)
		longUnicodeName := "aa_" + strings.Repeat("LongName_", 9) + "支付_🔥_01.kt"
		rels := []string{
			unicodeDir + "/" + longUnicodeName,
			unicodeDir + "/auth/config.yaml",
			unicodeDir + "/billing/config.yaml",
			unicodeDir + "/mid/Adapter01.kt",
			unicodeDir + "/z_fail/Adapter02.kt",
			unicodeDir + "/z_not/Tail01.kt",
			unicodeDir + "/z_not/Tail02.kt",
			unicodeDir + "/z_not/Tail03.kt",
			unicodeDir + "/z_not/Tail04.kt",
		}
		var patches []any
		for i, rel := range rels {
			patches = append(patches, map[string]any{
				"path": rel, "operation": "update",
				"edits": []any{map[string]any{"old_text": fmt.Sprintf("OLD_%02d", i), "new_text": fmt.Sprintf("NEW_%02d", i)}},
			})
		}
		set, eng, ws := r7r2SetupLongDirFiles(t, rels)
		r7r2KeepInPlace(t, ws, rels[4])
		eng.SetPublishFaultHooks(editing.PublishFaultHooks{
			AfterTruncate: func(abs string) error {
				if strings.HasSuffix(filepath.ToSlash(abs), "z_fail/Adapter02.kt") {
					for j := 0; j < 4; j++ {
						_ = os.WriteFile(filepath.Join(ws, filepath.FromSlash(rels[j])), []byte("EXT\n"), 0o644)
					}
					return syscall.EIO
				}
				return nil
			},
		})
		res, _ := set.Execute(context.Background(), "apply_patch", map[string]any{"patches": patches}, nil)
		p := r7r2ParseAndCheckBudget(t, "unicode_long_identical_basenames", res.Content, res.OK)
		// OV-06: lower-class untouched rows may fill leftover budget; every failed row must be shown.
		if ovFailedShown(p) != 5 || len(p.Files)+p.FilesOmitted != 9 {
			t.Fatalf("expected all 5 failed rows visible with accounting for 9 receipt rows, got len=%d omitted=%d content=%s", len(p.Files), p.FilesOmitted, res.Content)
		}
		var gotUnicode, gotAuth, gotBilling, gotA1, gotA2 bool
		for _, f := range p.Files {
			switch {
			case strings.Contains(f.Path, "支付_🔥_01.kt") && f.PublishState == string(editing.PublishExternalModification):
				gotUnicode = true
			case strings.HasSuffix(f.Path, "/auth/config.yaml") && f.PublishState == string(editing.PublishExternalModification):
				gotAuth = true
			case strings.HasSuffix(f.Path, "/billing/config.yaml") && f.PublishState == string(editing.PublishExternalModification):
				gotBilling = true
			case strings.HasSuffix(f.Path, "/mid/Adapter01.kt") && f.PublishState == string(editing.PublishExternalModification):
				gotA1 = true
			case strings.HasSuffix(f.Path, "/z_fail/Adapter02.kt") && f.PublishState == string(editing.PublishPartial):
				gotA2 = true
			}
		}
		if !gotUnicode || !gotAuth || !gotBilling || !gotA1 || !gotA2 {
			t.Fatalf("lost distinct failed identity (unicode=%v auth=%v billing=%v a1=%v a2=%v): %+v",
				gotUnicode, gotAuth, gotBilling, gotA1, gotA2, p.Files)
		}
	})

	t.Run("eight_failed_rows_long_paths", func(t *testing.T) {
		var rels []string
		var patches []any
		for i := 0; i < 12; i++ {
			rel := fmt.Sprintf("%s/Service%02d.kt", longDir, i)
			rels = append(rels, rel)
			patches = append(patches, map[string]any{
				"path": rel, "operation": "update",
				"edits": []any{map[string]any{"old_text": fmt.Sprintf("OLD_%02d", i), "new_text": fmt.Sprintf("NEW_%02d", i)}},
			})
		}
		set, eng, ws := r7r2SetupLongDirFiles(t, rels)
		eng.SetPublishFaultHooks(editing.PublishFaultHooks{
			AfterTruncate: func(abs string) error {
				if filepath.Base(abs) == "Service07.kt" {
					for j := 0; j < 7; j++ {
						_ = os.WriteFile(filepath.Join(ws, filepath.FromSlash(rels[j])), []byte("EXT\n"), 0o644)
					}
					return syscall.EIO
				}
				return nil
			},
		})
		res, _ := set.Execute(context.Background(), "apply_patch", map[string]any{"patches": patches}, nil)
		p := r7r2ParseAndCheckBudget(t, "eight_failed_long_paths", res.Content, res.OK)
		// OV-06: lower-class untouched rows may fill leftover budget; every failed row must be shown.
		if ovFailedShown(p) != 8 || len(p.Files)+p.FilesOmitted != 12 {
			t.Fatalf("expected all 8 failed rows visible with accounting for 12 receipt rows, got len=%d omitted=%d", len(p.Files), p.FilesOmitted)
		}
		seen := map[string]bool{}
		for _, f := range p.Files {
			seen[filepath.Base(f.Path)] = true
		}
		for i := 0; i < 8; i++ {
			base := fmt.Sprintf("Service%02d.kt", i)
			if !seen[base] {
				t.Fatalf("missing failed row %s in %+v", base, p.Files)
			}
		}
	})

	t.Run("cancellation_without_path_long_dirs", func(t *testing.T) {
		var rels []string
		var patches []any
		for i := 0; i < 12; i++ {
			rel := fmt.Sprintf("%s/Cancel%02d.kt", longDir, i)
			rels = append(rels, rel)
			patches = append(patches, map[string]any{
				"path": rel, "operation": "update",
				"edits": []any{map[string]any{"old_text": fmt.Sprintf("OLD_%02d", i), "new_text": fmt.Sprintf("NEW_%02d", i)}},
			})
		}
		set, eng, _ := r7r2SetupLongDirFiles(t, rels)
		ctx, cancel := context.WithCancel(context.Background())
		eng.SetPublishFaultHooks(editing.PublishFaultHooks{
			AfterInPlaceWriteClose: func(abs string) error {
				if filepath.Base(abs) == "Cancel05.kt" {
					cancel()
				}
				return nil
			},
		})
		res, _ := set.Execute(ctx, "apply_patch", map[string]any{"patches": patches}, nil)
		p := r7r2ParseAndCheckBudget(t, "cancel_long_dirs", res.Content, res.OK)
		if p.CommitState != string(editing.CommitStatePartiallyPublished) || len(p.Files)+p.FilesOmitted != 12 {
			t.Fatalf("unexpected cancel payload: %+v", p)
		}
	})

	t.Run("impossible_to_fit_50_failed_rows_discloses_omissions", func(t *testing.T) {
		root := t.TempDir()
		set, err := New(Options{Workspace: root, Policy: PolicyWorkspace, SessionID: "s-r7r2-50"})
		if err != nil {
			t.Fatal(err)
		}
		prefix := strings.Repeat("seg/", 70)
		var files []editing.FileCommitReceipt
		var results []editing.FileResult
		for i := 0; i < 50; i++ {
			rel := fmt.Sprintf("%sFailedItem%02d.kt", prefix, i)
			state := editing.PublishExternalModification
			if i%2 == 1 {
				state = editing.PublishPartial
			}
			files = append(files, editing.FileCommitReceipt{
				Path:         rel,
				PublishState: state,
				Published:    true,
				Wrote:        true,
				Error:        "input/output error",
			})
			results = append(results, editing.FileResult{
				Path:         rel,
				PublishState: state,
				Published:    true,
				Error:        "input/output error",
			})
		}
		rec := editing.CommitReceipt{DirtyFailure: true, State: editing.CommitStatePartiallyPublished, Files: files}
		result := editing.Result{CommitState: editing.CommitStatePartiallyPublished, Files: results}
		res, _ := set.commitOutcome(result, receiptCopy(rec), syscall.EIO)
		p := r7r2ParseAndCheckBudget(t, "impossible_50_failed", res.Content, res.OK)
		if !p.Overflow || !p.Truncated {
			t.Fatalf("expected overflow=true truncated=true, got %+v", p)
		}
		if len(p.Files) <= 1 || len(p.Files)+p.FilesOmitted != 50 {
			t.Fatalf("expected maximum fitting failed prefix (>1) with exact omission count, got len=%d omitted=%d", len(p.Files), p.FilesOmitted)
		}
		omittedFailed := p.FilesOmittedByState[string(editing.PublishExternalModification)] + p.FilesOmittedByState[string(editing.PublishPartial)]
		if omittedFailed != p.FilesOmitted || omittedFailed == 0 {
			t.Fatalf("expected omitted failed states disclosed in filesOmittedByState, got %v (omitted=%d)", p.FilesOmittedByState, p.FilesOmitted)
		}
		for _, f := range p.Files {
			if !strings.Contains(f.Path, "/FailedItem") {
				t.Fatalf("displayed row %q lost its filename", f.Path)
			}
		}
	})
}

func receiptCopy(r editing.CommitReceipt) editing.CommitReceipt {
	out := r
	out.Files = append([]editing.FileCommitReceipt(nil), r.Files...)
	return out
}
