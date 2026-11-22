package toolset

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/internal/editing"
)

func TestSOL01ArchivedDeep32(t *testing.T) {
	input, err := os.ReadFile("testdata/adr0024-sol/deep_f32_v00_u00_eio.input.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Result  editing.Result        `json:"result"`
		Receipt editing.CommitReceipt `json:"receipt"`
	}
	if err := json.Unmarshal(input, &fixture); err != nil {
		t.Fatal(err)
	}
	set, err := New(Options{Workspace: t.TempDir(), Policy: PolicyWorkspace, SessionID: "s-sol-deep32"})
	if err != nil {
		t.Fatal(err)
	}
	out, returned := set.commitOutcome(fixture.Result, fixture.Receipt, syscall.EIO)
	payload := r7r2ParseAndCheckBudget(t, "archived deep_f32", out.Content, out.OK)
	t.Logf("bytes=%d rows=%d omitted=%d byState=%v", len(out.Content), len(payload.Files), payload.FilesOmitted, payload.FilesOmittedByState)
	// OV-04: pathNotation bytes; the legend leaves 30 of the 32 failed rows, with two named omissions.
	if len(out.Content) != 4061 || len(payload.Files) != 30 || payload.FilesOmitted != 2 || payload.FilesOmittedByState["external_modification_detected"] != 1 || payload.FilesOmittedByState["partially_modified"] != 1 {
		t.Fatalf("expected 30 rows at 4061 bytes with two named omissions, got bytes=%d rows=%d omitted=%d byState=%v", len(out.Content), len(payload.Files), payload.FilesOmitted, payload.FilesOmittedByState)
	}
	ovCheckOverflowFull(t, set, fixture.Receipt, fixture.Result, out.Content, payload)
	if !out.MutatesWorkspace || returned != syscall.EIO || payload.Code != "" ||
		payload.CommitState != string(editing.CommitStatePartiallyPublished) ||
		!payload.Truncated || !payload.Overflow || !payload.DirtyFailure || !payload.PartialPublication {
		t.Fatalf("changed the commit or errno classification: %+v, error=%v", payload, returned)
	}
	var rows struct {
		Files                 []toolFileState `json:"files"`
		PublishedFilesOmitted int             `json:"publishedFilesOmitted"`
	}
	if err := json.Unmarshal([]byte(out.Content), &rows); err != nil {
		t.Fatal(err)
	}
	if rows.PublishedFilesOmitted != 32 {
		t.Fatalf("omitted %d compatibility rows, want 32", rows.PublishedFilesOmitted)
	}
	// OV-06: identity and state are checked by label resolution in ovCheckOverflowFull, not by position.
	for _, f := range rows.Files {
		if !f.Published || !f.Wrote || f.Verified || f.NewRev != "" || f.SHA256 != "" {
			t.Fatalf("row lost its flags: %+v", f)
		}
	}
	after, err := json.Marshal(fixture)
	var before any
	if err != nil || json.Unmarshal(input, &before) != nil {
		t.Fatal("cannot compare fixture")
	}
	var afterFields any
	if err := json.Unmarshal(after, &afterFields); err != nil || !reflect.DeepEqual(before, afterFields) {
		t.Fatal("rendering mutated the supplied result/receipt")
	}
}

func TestSOL02PathIdentity(t *testing.T) {
	deep := strings.Repeat(strings.Repeat("m", 170)+"/", 7)
	lead := strings.Repeat("shared-parent/", 9)
	group := []string{"a/" + lead + "x/" + deep + "f.kt", "a/" + lead + "y/" + deep + "f.kt", "b/" + lead + "x/" + deep + "f.kt"}
	for _, tc := range []struct {
		name  string
		paths []string
		min   int
	}{
		{"wide_distinction", group, 3},
		{"repeated_directories_at_different_depths", []string{
			"alpha/" + deep + "common/f.kt", "alpha/" + deep + "common/deep/" + deep + "common/f.kt",
			"beta/" + deep + "common/f.kt", "beta/" + deep + "common/deep/" + deep + "common/f.kt",
		}, 4},
		{"same_basename", []string{"module00/config.yaml", "module01/config.yaml", "module02/config.yaml", "module03/config.yaml", "module04/config.yaml"}, 5},
		{"similar_basenames", []string{"auth/config.yaml", "billing/config.yaml", "auth/config.yaml.bak", "auth/config.yaml.template"}, 4},
		{"long_parent_components", []string{strings.Repeat("α", 70) + "-一/" + deep + "配置 文件.yaml", strings.Repeat("α", 70) + "-二/" + deep + "配置 文件.yaml"}, 2},
		{"reserved_literal_label", []string{deep + "f.kt", "…/f.kt", deep + "other.kt"}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, p := r7r2FinalRender(t, tc.paths, nil, syscall.EIO)
			seen := make(map[string]bool)
			for i, row := range solDecodedRows(t, out) {
				base := filepath.Base(tc.paths[i])
				if seen[row.Path] || row.Path == "" || !utf8.ValidString(row.Path) ||
					filepath.Base(row.Path) != base || row.PublishState != r7r2FinalFailedStates[i%len(r7r2FinalFailedStates)] ||
					row.Verified || row.NewRev != "" || row.SHA256 != "" {
					t.Fatalf("row %d has ambiguous identity or incorrect state: %q (%+v)", i, row.Path, row)
				}
				seen[row.Path] = true
			}
			t.Logf("bytes=%d rows=%d unique=%d omitted=%d byState=%v", len(out), len(p.Files), len(seen), p.FilesOmitted, p.FilesOmittedByState)
			if len(p.Files) < tc.min || p.FilesOmitted != len(tc.paths)-len(p.Files) ||
				!reflect.DeepEqual(emptyAsNil(p.FilesOmittedByState), emptyAsNil(r7r2FinalOmittedByState(len(tc.paths), len(p.Files), 0))) {
				t.Fatal("lost identifiable rows or their omission accounting")
			}
		})
	}
}

func emptyAsNil(m map[string]int) map[string]int {
	if len(m) == 0 {
		return nil
	}
	return m
}

func solDecodedRows(t *testing.T, content string) []toolFileState {
	t.Helper()
	var p struct {
		Files []toolFileState `json:"files"`
	}
	if err := json.Unmarshal([]byte(content), &p); err != nil {
		t.Fatal(err)
	}
	return p.Files
}

func TestSOL02UnidentifiableRedactionsAreOmitted(t *testing.T) {
	root := t.TempDir()
	set, err := New(Options{Workspace: root, Policy: PolicyWorkspace, SessionID: "s-sol-redacted"})
	if err != nil {
		t.Fatal(err)
	}
	rec := editing.CommitReceipt{State: editing.CommitStatePartiallyPublished, DirtyFailure: true}
	res := editing.Result{CommitState: rec.State}
	for i, name := range []string{"../unrelated-a/key.txt", "../unrelated-b/key.txt", "workspace/key.txt", "workspace/other.txt"} {
		state := []editing.PublishState{editing.PublishExternalModification, editing.PublishPartial, editing.PublishExternalModification, editing.PublishPartial}[i]
		rec.Files = append(rec.Files, editing.FileCommitReceipt{Path: name, PublishState: state, Published: true, Wrote: true})
		res.Files = append(res.Files, editing.FileResult{Path: name, PublishState: state, Published: true, Changed: true})
	}
	content, _ := set.commitOutcome(res, rec, syscall.EIO)
	p := r7r2ParseAndCheckBudget(t, "redactions", content.Content, content.OK)
	var duplicateView struct {
		PublishedFiles        []editing.FileResult `json:"publishedFiles"`
		PublishedFilesOmitted int                  `json:"publishedFilesOmitted"`
	}
	if err := json.Unmarshal([]byte(content.Content), &duplicateView); err != nil {
		t.Fatal(err)
	}
	if len(p.Files) != 2 || p.FilesOmitted != 2 || p.FilesOmittedByState[string(editing.PublishExternalModification)] != 1 ||
		p.FilesOmittedByState[string(editing.PublishPartial)] != 1 || !p.Truncated ||
		len(duplicateView.PublishedFiles) != 2 || duplicateView.PublishedFilesOmitted != 2 {
		t.Fatalf("redacted collisions must be explicitly omitted in both views: %s", content.Content)
	}
	for i, row := range p.Files {
		if row.Path != rec.Files[i+2].Path || duplicateView.PublishedFiles[i].Path != row.Path {
			t.Fatalf("wrong identity after omitting redactions: %s", content.Content)
		}
	}
	t.Logf("bytes=%d rows=%d unique=%d omitted=%d byState=%v", len(content.Content), len(p.Files), len(p.Files), p.FilesOmitted, p.FilesOmittedByState)
}

func TestSOL02PressureLeavesUniqueLabels(t *testing.T) {
	deep := strings.Repeat(strings.Repeat("d", 180)+"/", 6)
	paths := make([]string, 100)
	for i := range paths {
		paths[i] = fmt.Sprintf("模块 %03d/%s配置 文件.yaml", i, deep)
	}
	set := astraSet(t)
	rec, res, out, p := ovRun(t, set, ovFinalItems(paths, nil), syscall.EIO)
	// OV-06: rows follow priority order, not input position, so identity is checked by label resolution.
	ovCheckOverflowFull(t, set, rec, res, out, p)
	for _, f := range p.Files {
		if !strings.HasSuffix(f.Path, "/配置 文件.yaml") {
			t.Fatalf("row %q lost its filename", f.Path)
		}
	}
	t.Logf("bytes=%d rows=%d omitted=%d byState=%v", len(out), len(p.Files), p.FilesOmitted, p.FilesOmittedByState)
	if len(out) < 3800 || len(p.Files) < 20 || p.FilesOmitted == 0 || p.FilesOmitted != len(paths)-len(p.Files) {
		t.Fatal("expected bounded prefix and exact omission counts near the byte limit")
	}
}
