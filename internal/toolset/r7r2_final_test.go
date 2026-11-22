package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/internal/editing"
)

// r7r2FinalFailedStates are the publish states a failed row can carry; a synthetic receipt cycles
// through them so an omission count has to name more than one state.
var r7r2FinalFailedStates = []editing.PublishState{
	editing.PublishExternalModification, editing.PublishPartial, editing.PublishSucceededWithPostError,
	editing.PublishUncertain, editing.PublishRollbackFailed,
}

// r7r2FinalRender renders a dirty partial commit whose failed rows are the given paths, followed by
// untouched rows, through the same commitOutcome the edit tools use.
func r7r2FinalRender(t *testing.T, failed, untouched []string, top error) (string, r7r2Payload) {
	t.Helper()
	set, err := New(Options{Workspace: t.TempDir(), Policy: PolicyWorkspace, SessionID: "s-r7r2-final"})
	if err != nil {
		t.Fatal(err)
	}
	var files []editing.FileCommitReceipt
	var results []editing.FileResult
	for i, p := range failed {
		state := r7r2FinalFailedStates[i%len(r7r2FinalFailedStates)]
		files = append(files, editing.FileCommitReceipt{Path: p, PublishState: state, Published: true, Wrote: true, Error: "input/output error"})
		results = append(results, editing.FileResult{Path: p, PublishState: state, Published: true, Error: "input/output error"})
	}
	for _, p := range untouched {
		files = append(files, editing.FileCommitReceipt{Path: p, PublishState: editing.PublishNotAttempted})
		results = append(results, editing.FileResult{Path: p, PublishState: editing.PublishNotAttempted})
	}
	rec := editing.CommitReceipt{DirtyFailure: true, State: editing.CommitStatePartiallyPublished, Files: files}
	res, _ := set.commitOutcome(editing.Result{CommitState: editing.CommitStatePartiallyPublished, Files: results}, rec, top)
	return res.Content, r7r2ParseAndCheckBudget(t, "r7r2_final", res.Content, res.OK)
}

// r7r2FinalOmittedByState is what filesOmittedByState must say when the first shown failed rows are
// displayed and every other row is omitted.
func r7r2FinalOmittedByState(failed, shown, untouched int) map[string]int {
	want := map[string]int{}
	for i := shown; i < failed; i++ {
		want[string(r7r2FinalFailedStates[i%len(r7r2FinalFailedStates)])]++
	}
	if untouched > 0 {
		want[string(editing.PublishNotAttempted)] = untouched
	}
	return want
}

// r7r2FinalHoldsRows re-encodes the returned envelope with count failed rows at a compact basename
// candidate and reports whether that still fits. It does not go through the production ladder, so it
// tells whether a row the payload omitted could have been kept.
func r7r2FinalHoldsRows(t *testing.T, content string, failed []string, untouched, count int) bool {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &envelope); err != nil {
		t.Fatal(err)
	}
	rows := make([]toolFileState, 0, count)
	for i, p := range failed[:count] {
		rows = append(rows, toolFileState{Path: "…/" + filepath.Base(p), PublishState: r7r2FinalFailedStates[i%len(r7r2FinalFailedStates)], Published: true, Wrote: true})
	}
	envelope["files"], _ = json.Marshal(rows)
	delete(envelope, "filesOmitted")
	delete(envelope, "filesOmittedByState")
	if omitted := len(failed) - count + untouched; omitted > 0 {
		envelope["filesOmitted"], _ = json.Marshal(omitted)
		envelope["filesOmittedByState"], _ = json.Marshal(r7r2FinalOmittedByState(len(failed), count, untouched))
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return len(raw) <= maxPartialPayloadBytes
}

// TestR7R2FinalOverflowKeepsEveryFailedRowThatFits reproduces R7R2-FINAL-01. The overflow envelope
// tried every failed row down to a 96-byte path and then searched for a shorter list at 128 bytes,
// so twenty failed files that all fit at 64 bytes came back as fifteen.
func TestR7R2FinalOverflowKeepsEveryFailedRowThatFits(t *testing.T) {
	dir := strings.TrimSuffix(strings.Repeat("pkg/", 195), "/")
	for _, n := range []int{17, 20, 22, 23} {
		t.Run(fmt.Sprintf("failed_%d", n), func(t *testing.T) {
			failed := make([]string, n)
			for i := range failed {
				failed[i] = fmt.Sprintf("%s/Adapter%03d.kt", dir, i)
			}
			set := astraSet(t)
			rec, res, content, p := ovRun(t, set, ovFinalItems(failed, []string{dir + "/Untouched.kt"}), syscall.EIO)
			if !p.Overflow || !p.Truncated {
				t.Fatalf("expected the overflow envelope: %s", content)
			}
			// OV-06: the untouched row is lower-class and fills leftover budget, so it may be shown too.
			if got := ovFailedShown(p); got != n {
				t.Fatalf("%d failed rows shown, want all %d (omitted=%d byState=%v)", got, n, p.FilesOmitted, p.FilesOmittedByState)
			}
			ovCheckOverflowFull(t, set, rec, res, content, p)
		})
	}
}

// TestR7R2FinalOverflowKeepsLongestFailedPrefix: when the failed rows cannot all fit, the envelope
// keeps the longest prefix any path budget holds, and the omission counts name the rest by state.
func TestR7R2FinalOverflowKeepsLongestFailedPrefix(t *testing.T) {
	dir := strings.TrimSuffix(strings.Repeat("pkg/", 195), "/")
	for _, n := range []int{50, 100, 200} {
		t.Run(fmt.Sprintf("failed_%d", n), func(t *testing.T) {
			failed := make([]string, n)
			for i := range failed {
				failed[i] = fmt.Sprintf("%s/Adapter%03d.kt", dir, i)
			}
			set := astraSet(t)
			rec, res, content, p := ovRun(t, set, ovFinalItems(failed, []string{dir + "/Untouched.kt"}), syscall.EIO)
			// OV-06: the rule is the maximal failed count that fits, with ties broken by canonical path.
			shown := ovFailedShown(p)
			if !p.Overflow || shown == 0 || shown >= n {
				t.Fatalf("expected fewer than %d failed rows, got %d: %s", n, shown, content)
			}
			t.Logf("bytes=%d shown=%d omitted=%d byState=%v", len(content), len(p.Files), p.FilesOmitted, p.FilesOmittedByState)
			ovCheckOverflowFull(t, set, rec, res, content, p)
		})
	}
}

// TestR7R2FinalSameNameRowsStayDistinct reproduces R7R2-FINAL-02. A shortened path kept only its
// tail, so failed files with one name in several directories, sharing more than the row budget
// behind the directory that tells them apart, all came back as the same text.
func TestR7R2FinalSameNameRowsStayDistinct(t *testing.T) {
	long := strings.Repeat("d", 190) + "/" + strings.Repeat("e", 190) + "/" + strings.Repeat("f", 190) + "/" + strings.Repeat("g", 190)
	for _, tc := range []struct {
		name string
		n    int
		path func(i int) string
		mark func(i int) string // text only file i has
	}{
		{"five_modules_long_shared_tail", 5,
			func(i int) string { return fmt.Sprintf("module%02d/%s/config.yaml", i, long) },
			func(i int) string { return fmt.Sprintf("module%02d/", i) }},
		{"eight_modules_between_long_shared_ends", 8,
			func(i int) string { return fmt.Sprintf("%s/module%02d/%s/config.yaml", long[:381], i, long[382:]) },
			func(i int) string { return fmt.Sprintf("/module%02d/", i) }},
		{"sixteen_long_names_differing_at_the_end", 16,
			func(i int) string { return fmt.Sprintf("%s-module%02d/config.yaml", strings.Repeat("p", 240), i) },
			func(i int) string { return fmt.Sprintf("-module%02d/", i) }},
		{"twenty_four_services_one_package", 24,
			func(i int) string {
				return fmt.Sprintf("services/svc%02d/src/main/kotlin/com/example/platform/payments/gateway/config/AppConfig.kt", i)
			},
			func(i int) string { return fmt.Sprintf("/svc%02d/", i) }},
		{"twelve_unicode_names_with_spaces", 12,
			func(i int) string {
				return fmt.Sprintf("OneDrive - Acme Corp/项目 %02d/%s/设计 文档 🔥.md", i, strings.Repeat("共享 目录/", 40))
			},
			func(i int) string { return fmt.Sprintf("/项目 %02d/", i) }},
		{"two_paths_over_the_failed_row_bound", 2,
			func(i int) string { return fmt.Sprintf("tenant%02d/%s/config.yaml", i, strings.Repeat("x/", 520)) },
			func(i int) string { return fmt.Sprintf("tenant%02d/", i) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failed := make([]string, tc.n)
			for i := range failed {
				failed[i] = tc.path(i)
			}
			content, p := r7r2FinalRender(t, failed, nil, syscall.EIO)
			if len(p.Files) == 0 || len(p.Files)+p.FilesOmitted != tc.n {
				t.Fatalf("rows=%d omitted=%d of %d: %s", len(p.Files), p.FilesOmitted, tc.n, content)
			}
			base := failed[0][strings.LastIndexByte(failed[0], '/'):]
			seen := map[string]int{}
			for i, f := range p.Files {
				if prev, dup := seen[f.Path]; dup {
					t.Fatalf("rows %d and %d both read %q", prev, i, f.Path)
				}
				seen[f.Path] = i
				if !strings.HasSuffix(f.Path, base) {
					t.Fatalf("row %d = %q lost its filename %q", i, f.Path, base)
				}
				if f.Path != failed[i] && !strings.Contains(f.Path, tc.mark(i)) {
					t.Fatalf("row %d = %q does not carry %q, the part only %q has", i, f.Path, tc.mark(i), failed[i])
				}
				for j := range failed {
					if j != i && strings.Contains(f.Path, tc.mark(j)) {
						t.Fatalf("row %d = %q carries the mark of file %d", i, f.Path, j)
					}
				}
			}
		})
	}
}

// TestR7R2FinalFiveFailedSameNameFilesStayDistinct drives R7R2-FINAL-02 through apply_patch on real
// files: the R7R2-NEW-01 shape (five failed files, long directories, a bare EIO) with one file name
// in six module directories.
func TestR7R2FinalFiveFailedSameNameFilesStayDistinct(t *testing.T) {
	shared := strings.Repeat("d", 190) + "/" + strings.Repeat("e", 190) + "/" + strings.Repeat("f", 190) + "/" + strings.Repeat("g", 150)
	rels := make([]string, 6)
	var patches []any
	for i := range rels {
		rels[i] = fmt.Sprintf("module%02d/%s/config.yaml", i, shared)
		patches = append(patches, map[string]any{
			"path": rels[i], "operation": "update",
			"edits": []any{map[string]any{"old_text": fmt.Sprintf("OLD_%02d", i), "new_text": fmt.Sprintf("NEW_%02d", i)}},
		})
	}
	set, eng, ws := r7r2SetupLongDirFiles(t, rels)
	eng.SetPublishFaultHooks(editing.PublishFaultHooks{
		AfterTruncate: func(abs string) error {
			if strings.Contains(filepath.ToSlash(abs), "/module04/") {
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
	p := r7r2ParseAndCheckBudget(t, "five_failed_same_name", res.Content, res.OK)
	// OV-06: the shown rows are the failed rows that fit, in priority order; each must name its own module.
	if !p.Overflow || ovFailedShown(p) != 5 {
		t.Fatalf("expected five failed rows: %s", res.Content)
	}
	seen := map[string]bool{}
	for i, f := range p.Files {
		module := ""
		for j := range rels {
			if strings.HasPrefix(f.Path, fmt.Sprintf("module%02d/", j)) {
				module = fmt.Sprintf("module%02d", j)
			}
		}
		if module == "" || seen[module] || !strings.HasSuffix(f.Path, "/config.yaml") {
			t.Fatalf("row %d = %+v does not name one module config", i, f)
		}
		seen[module] = true
	}
}

// TestR7R2FinalRefusedPathSurvivesOverflow reproduces R7R2-FINAL-03. A single-file refusal has no
// receipt row, and the overflow envelope carried no path, so a refused path that JSON escaping
// pushes past the budget left the result without any name for the file.
func TestR7R2FinalRefusedPathSurvivesOverflow(t *testing.T) {
	set, err := New(Options{Workspace: t.TempDir(), Policy: PolicyWorkspace, SessionID: "s-r7r2-final"})
	if err != nil {
		t.Fatal(err)
	}
	refused := strings.Repeat("&&&&&&&/", 200) + "Target&Notes.kt"
	res, goErr := set.editOutcome(&editing.Error{Code: editing.CodeStale, Path: refused, Detail: "base revision is not current", CurrentRev: "r1"})
	if goErr != nil || res.OK || res.MutatesWorkspace {
		t.Fatalf("expected a clean refusal result: res=%+v err=%v", res, goErr)
	}
	if !json.Valid([]byte(res.Content)) || !utf8.ValidString(res.Content) || len(res.Content) > maxPartialPayloadBytes {
		t.Fatalf("payload invalid or %d bytes over budget", len(res.Content))
	}
	var p r7r2Payload
	if err := json.Unmarshal([]byte(res.Content), &p); err != nil {
		t.Fatal(err)
	}
	if !p.Overflow || !p.Truncated || p.Code != editing.CodeStale {
		t.Fatalf("expected the overflow envelope with the refusal code: %s", res.Content)
	}
	if !strings.HasPrefix(p.Path, "…/") || !strings.HasSuffix(p.Path, "/Target&Notes.kt") || !strings.HasSuffix(refused, strings.TrimPrefix(p.Path, "…")) {
		t.Fatalf("overflow envelope does not name the refused file: path=%q", p.Path)
	}
}

// TestR7R2FinalOverflowBoundWorstCase: the bound holds when every string field needs six output
// bytes per input byte and every failed state appears among two hundred failed rows.
func TestR7R2FinalOverflowBoundWorstCase(t *testing.T) {
	set := astraSet(t)
	escaped := strings.Repeat("\x01&<>\x02", 400)
	var items []astraItem
	for i := 0; i < 200; i++ {
		p := fmt.Sprintf("%s/m%03d/%s/f.kt", escaped[:300], i, escaped[:300])
		items = append(items, astraItem{raw: p, disp: p, state: r7r2FinalFailedStates[i%len(r7r2FinalFailedStates)]})
	}
	rec, res, content, p := ovRun(t, set, items, syscall.EIO)
	if !p.Overflow || len(p.Files) == 0 || len(p.Files)+p.FilesOmitted != 200 {
		t.Fatalf("rows=%d omitted=%d: %s", len(p.Files), p.FilesOmitted, content)
	}
	// OV-06: rows follow priority order, so each must name its own module by the path mark, not by position.
	seen := map[string]bool{}
	for i, f := range p.Files {
		mark := ""
		for j := 0; j < 200; j++ {
			if strings.Contains(f.Path, fmt.Sprintf("/m%03d/", j)) {
				mark = fmt.Sprintf("m%03d", j)
			}
		}
		if mark == "" || seen[mark] || !strings.HasSuffix(f.Path, "/f.kt") {
			t.Fatalf("row %d = %q is repeated or does not name one module file", i, f.Path)
		}
		seen[mark] = true
	}
	ovCheckOverflowFull(t, set, rec, res, content, p)
}
