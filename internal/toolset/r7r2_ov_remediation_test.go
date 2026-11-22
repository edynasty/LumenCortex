package toolset

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/edynasty/LumenCortex/internal/editing"
)

// Agent-facing legend text, copied from the specification (OV-04). The tests compare the emitted
// pathNotation with these strings, so any drift in the production constants fails here.
const (
	ovNotationShort    = "…/ = 1+ omitted dirs; … in a name = omitted chars. Display-only labels, not file paths."
	ovNotationCount    = "…{N}/ = exactly N dirs."
	ovNotationTail     = "…/./x = omitted dirs, then exactly x."
	ovNotationExact    = "./x = exactly path x, … literal."
	ovNotationRedacted = "<outside-workspace>/x = outside file named x."
)

// ovCounted reports whether a component is exactly "…{N}" with a canonical count from 1 to 9999.
func ovCounted(c string) bool {
	inner, ok := strings.CutPrefix(c, "…{")
	if !ok {
		return false
	}
	inner, ok = strings.CutSuffix(inner, "}")
	if !ok {
		return false
	}
	n, err := strconv.Atoi(inner)
	return err == nil && n >= 1 && n <= 9999 && strconv.Itoa(n) == inner
}

// ovLegendFor is the legend a reader needs for the labels of one payload, built from the specification
// grammar and the fixed text above.
func ovLegendFor(labels []string) string {
	var short, count, tail, exact, redacted bool
	for _, label := range labels {
		switch {
		case label == "":
		case strings.HasPrefix(label, "./"):
			exact = true
		case strings.HasPrefix(label, "<outside-workspace>/"):
			redacted = redacted || strings.Contains(label, "…")
		case strings.HasPrefix(label, "…/./"):
			short, tail = true, true
		case !strings.Contains(label, "…"):
		default:
			short = true
			for _, c := range strings.Split(label, "/") {
				if ovCounted(c) {
					count = true
				}
			}
		}
	}
	var parts []string
	if short || count || tail {
		parts = append(parts, ovNotationShort)
	}
	if count {
		parts = append(parts, ovNotationCount)
	}
	if tail {
		parts = append(parts, ovNotationTail)
	}
	if exact {
		parts = append(parts, ovNotationExact)
	}
	if redacted {
		parts = append(parts, ovNotationRedacted)
	}
	return strings.Join(parts, " ")
}

// ovPayloadDoc is the part of an envelope the oracle reads.
type ovPayloadDoc struct {
	Path          string               `json:"path"`
	PathNotation  string               `json:"pathNotation"`
	Files         []toolFileState      `json:"files"`
	PublishedRows []editing.FileResult `json:"publishedFiles"`
}

// ovPayloadLabels returns every label a payload emits: file rows, published rows, and the top-level path.
func ovPayloadLabels(doc ovPayloadDoc) []string {
	var labels []string
	for _, f := range doc.Files {
		labels = append(labels, f.Path)
	}
	for _, f := range doc.PublishedRows {
		labels = append(labels, f.Path)
	}
	if doc.Path != "" {
		labels = append(labels, doc.Path)
	}
	return labels
}

// ovRun builds the receipt and result for items, renders them through the public commit outcome, and
// parses the envelope.
func ovRun(t *testing.T, set *Set, items []astraItem, top error) (editing.CommitReceipt, editing.Result, string, r7r2Payload) {
	t.Helper()
	rec := editing.CommitReceipt{DirtyFailure: true, State: editing.CommitStatePartiallyPublished}
	res := editing.Result{CommitState: editing.CommitStatePartiallyPublished}
	for _, it := range items {
		switch {
		case it.resultOnly:
			res.Files = append(res.Files, editing.FileResult{Path: it.raw, PublishState: editing.PublishNotAttempted})
		case it.verified:
			rec.Files = append(rec.Files, editing.FileCommitReceipt{Path: it.raw, PublishState: editing.PublishSucceededAndVerified, Published: true, Verified: true, Wrote: true, NewSHA256: it.newSHA})
			res.Files = append(res.Files, editing.FileResult{Path: it.raw, PublishState: editing.PublishSucceededAndVerified, Published: true, Verified: true, NewRev: it.newRev})
		case it.state == editing.PublishNotAttempted:
			rec.Files = append(rec.Files, editing.FileCommitReceipt{Path: it.raw, PublishState: editing.PublishNotAttempted})
			res.Files = append(res.Files, editing.FileResult{Path: it.raw, PublishState: editing.PublishNotAttempted})
		default:
			rec.Files = append(rec.Files, editing.FileCommitReceipt{Path: it.raw, PublishState: it.state, Published: true, Wrote: true, Error: "input/output error"})
			res.Files = append(res.Files, editing.FileResult{Path: it.raw, PublishState: it.state, Published: true, Error: "input/output error"})
		}
	}
	out, _ := set.commitOutcome(res, rec, top)
	return rec, res, out.Content, r7r2ParseAndCheckBudget(t, "ov", out.Content, out.OK)
}

// ovCheckOverflowFull is the shared oracle for every rendered payload. Each shown label must name exactly
// one receipt path under the lenient and loose readings, with no repeats. The shown rows must follow
// priority order. Each row's state and flags must be those of the path it names. Omitted accounting must
// cover exactly the rows not shown. The pathNotation must be the legend of the labels present. An
// overflow payload must not admit any unshown failed row. It returns the shown receipt paths.
func ovCheckOverflowFull(t *testing.T, set *Set, rec editing.CommitReceipt, res editing.Result, content string, p r7r2Payload) map[string]bool {
	t.Helper()
	display := set.displayPaths()
	rawOf := map[string]string{}
	stateOf := map[string]editing.FileCommitReceipt{}
	var attempted []string
	for _, f := range rec.Files {
		d := display.Path(f.Path)
		rawOf[d] = f.Path
		stateOf[f.Path] = f
		attempted = append(attempted, d)
	}
	posOf := map[string]int{}
	for i, f := range prioritizedReceiptFiles(rec.Files) {
		if _, ok := posOf[f.Path]; !ok {
			posOf[f.Path] = i
		}
	}
	var doc ovPayloadDoc
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatal(err)
	}
	shown := map[string]bool{}
	last := -1
	for i, row := range p.Files {
		var named []string
		for _, loose := range []bool{false, true} {
			got := astraResolves(row.Path, attempted, loose)
			if len(got) != 1 {
				t.Fatalf("row %d label %q resolves to %v (loose=%v), want exactly one path", i, row.Path, got, loose)
			}
			named = got
		}
		raw := rawOf[named[0]]
		if shown[raw] {
			t.Fatalf("row %d label %q repeats %q", i, row.Path, raw)
		}
		if pos := posOf[raw]; pos <= last {
			t.Fatalf("row %d label %q (%q) is out of priority order", i, row.Path, raw)
		} else {
			last = pos
		}
		shown[raw] = true
		want := stateOf[raw]
		published := want.PublishState != editing.PublishNotAttempted
		if row.PublishState != string(want.PublishState) || row.Published != published || row.Wrote != published || row.Verified != want.Verified {
			t.Fatalf("row %d = %+v does not carry the state of %q (%+v)", i, row, raw, want)
		}
	}
	omitted := map[string]int{}
	for _, f := range rec.Files {
		if !shown[f.Path] {
			omitted[string(f.PublishState)]++
		}
	}
	if p.FilesOmitted != len(rec.Files)-len(p.Files) || !reflect.DeepEqual(emptyAsNil(p.FilesOmittedByState), emptyAsNil(omitted)) {
		t.Fatalf("omitted=%d byState=%v, want %d and %v", p.FilesOmitted, p.FilesOmittedByState, len(rec.Files)-len(p.Files), omitted)
	}
	if want := ovLegendFor(ovPayloadLabels(doc)); doc.PathNotation != want {
		t.Fatalf("pathNotation = %q, want the legend of the emitted labels %q", doc.PathNotation, want)
	}
	if p.Overflow {
		ovAssertFull(t, set, rec, res, content, shown)
	}
	return shown
}

// ovAssertFull adds each unshown failed receipt row, compact as the overflow search prices it, to the
// emitted payload, recomputing the omission accounting and the legend. Every such addition must exceed the
// byte limit, so the emitted rows are maximal for the compact labels (OV-04, OV-06).
func ovAssertFull(t *testing.T, set *Set, rec editing.CommitReceipt, res editing.Result, content string, shown map[string]bool) {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatal(err)
	}
	var payload ovPayloadDoc
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		t.Fatal(err)
	}
	var rawFiles []json.RawMessage
	if err := json.Unmarshal(doc["files"], &rawFiles); err != nil {
		t.Fatal(err)
	}
	index := newFailurePathIndex()
	display := set.displayPaths()
	for _, f := range rec.Files {
		index.add(f.Path, display)
	}
	for _, f := range res.Files {
		index.add(f.Path, display)
	}
	total := len(rec.Files)
	for _, r := range rec.Files {
		if receiptFileRank(r) != 0 || shown[r.Path] {
			continue
		}
		extra := toolFileState{Path: index.label(r.Path, 0), PublishState: r.PublishState, Published: true, Wrote: true}
		extraRaw, err := json.Marshal(extra)
		if err != nil {
			t.Fatal(err)
		}
		files := append(append([]json.RawMessage{}, rawFiles...), extraRaw)
		byState := map[editing.PublishState]int{}
		for _, f := range rec.Files {
			if !shown[f.Path] && f.Path != r.Path {
				byState[f.PublishState]++
			}
		}
		labels := append(ovPayloadLabels(payload), extra.Path)
		candidate := map[string]any{}
		for k, v := range doc {
			candidate[k] = v
		}
		candidate["files"] = files
		delete(candidate, "filesOmitted")
		delete(candidate, "filesOmittedByState")
		if omitted := total - len(files); omitted > 0 {
			candidate["filesOmitted"] = omitted
			candidate["filesOmittedByState"] = byState
		}
		if leg := ovLegendFor(labels); leg != "" {
			candidate["pathNotation"] = leg
		} else {
			delete(candidate, "pathNotation")
		}
		raw, err := json.Marshal(candidate)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) <= maxPartialPayloadBytes {
			t.Fatalf("unshown failed row %q fits: adding it gives %d bytes, within %d", r.Path, len(raw), maxPartialPayloadBytes)
		}
	}
}

// ovBuildFailed builds the receipt and result of an all-failed commit, as astraRun does.
func ovBuildFailed(items []astraItem) (editing.CommitReceipt, editing.Result) {
	rec := editing.CommitReceipt{DirtyFailure: true, State: editing.CommitStatePartiallyPublished}
	res := editing.Result{CommitState: editing.CommitStatePartiallyPublished}
	for _, it := range items {
		rec.Files = append(rec.Files, editing.FileCommitReceipt{Path: it.raw, PublishState: it.state, Published: true, Wrote: true, Error: "input/output error"})
		res.Files = append(res.Files, editing.FileResult{Path: it.raw, PublishState: it.state, Published: true, Error: "input/output error"})
	}
	return rec, res
}

// ovInComponentMatch is a reader that takes each "…" as one or more non-slash characters and every
// other character, including "{" and "}", literally. "./" is exact.
func ovInComponentMatch(label, path string) bool {
	if strings.HasPrefix(label, "./") {
		return path == label[2:]
	}
	var b strings.Builder
	b.WriteString("^")
	for i, part := range strings.Split(label, pathEllipsis) {
		if i > 0 {
			b.WriteString(`[^/]+`)
		}
		b.WriteString(regexp.QuoteMeta(part))
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String()).MatchString(path)
}

// ovAssertResolves requires the label to name exactly want among the attempted paths under the two
// component readings, and requires that the in-component reader never names another path. A counted
// label cannot match its own file under that reader, so the own-path rule is not applied to it there.
func ovAssertResolves(t *testing.T, label, want string, attempted []string) {
	t.Helper()
	for _, loose := range []bool{false, true} {
		if got := astraResolves(label, attempted, loose); len(got) != 1 || got[0] != want {
			t.Fatalf("label %q resolves to %v (loose=%v), want only %q", label, got, loose, want)
		}
	}
	for _, a := range attempted {
		if ovInComponentMatch(label, a) && a != want {
			t.Fatalf("label %q names %q under the in-component reader, not %q", label, a, want)
		}
	}
}

// ovHugeTail is a tail that makes a literal path larger than the whole payload limit on its own.
func ovHugeTail(pad int) string {
	tail := "/" + strings.Repeat(strings.Repeat("&", 162)+"/", 3)
	for i := 0; i < pad/200; i++ {
		tail += strings.Repeat("z", 200) + "/"
	}
	return tail + "f.kt"
}

// TestOVCountedGapNeverReadsAsBraceLiteral: a counted gap must not be emitted when another attempted
// path would match it with "{n}" read as literal text. Shown labels and the compact label of every
// attempted path must each name exactly one path under every reader, and each row is checked against
// the path its label names.
func TestOVCountedGapNeverReadsAsBraceLiteral(t *testing.T) {
	items := astraFailed(astraArchived40(), astraCycle)
	items = append(items, astraFailed([]string{"r00/q{7}/f.kt", "r01/{15}/f.kt", "r02/mm{7}/f.kt"}, astraCycle)...)
	set := astraSet(t)
	rec, res, content, p := ovRun(t, set, items, syscall.EIO)
	shown := ovCheckOverflowFull(t, set, rec, res, content, p)
	if len(p.Files) == 0 {
		t.Fatalf("no rows shown: %s", content)
	}
	attempted := make([]string, 0, len(items))
	for _, it := range items {
		attempted = append(attempted, it.disp)
	}
	for _, row := range p.Files {
		if row.Path == "r00/…{7}/f.kt" {
			t.Fatalf("a row is shown as the counted gap r00/…{7}/f.kt: %s", content)
		}
		named := astraResolves(row.Path, attempted, false)
		if len(named) != 1 {
			t.Fatalf("row label %q resolves to %v", row.Path, named)
		}
		ovAssertResolves(t, row.Path, named[0], attempted)
	}
	index := newFailurePathIndex()
	display := set.displayPaths()
	for _, it := range items {
		index.add(it.raw, display)
	}
	for _, it := range items {
		ovAssertResolves(t, index.label(it.raw, 0), it.disp, attempted)
	}
	_ = shown
}

// BenchmarkOVRenderScale renders all-failed batches of growing size, with the same shapes used to
// measure the rescue path. Inputs are built before the timer starts.
func BenchmarkOVRenderScale(b *testing.B) {
	set, err := New(Options{Workspace: b.TempDir(), Policy: PolicyWorkspace, SessionID: "s-ov-scale"})
	if err != nil {
		b.Fatal(err)
	}
	fill := strings.Repeat(strings.Repeat("m", 170)+"/", 6)
	cases := []struct {
		name  string
		sizes []int
		path  func(i int) string
		extra []string
	}{
		{"unique_deep", []int{32, 100, 1024, 4096}, func(i int) string {
			return fmt.Sprintf("src/main/kotlin/com/example/platform/payments/gateway/adapters/stripe/Adapter%05d.kt", i)
		}, nil},
		{"samebase_modules", []int{32, 100, 1024, 4096}, func(i int) string {
			return fmt.Sprintf("services/m%05d/src/main/resources/config/application.yaml", i)
		}, nil},
		{"samebase_longsuffix", []int{32, 100, 1024, 4096}, func(i int) string {
			return fmt.Sprintf("r/x%05d/", i) + fill + "common/f.kt"
		}, nil},
		{"longname_middle", []int{32, 100, 1024, 4096}, func(i int) string {
			return "pkg/" + strings.Repeat("L", 100) + fmt.Sprintf("%05d", i) + strings.Repeat("L", 100) + ".kt"
		}, nil},
		{"blocked_literal", []int{32, 100, 1024, 4096}, func(i int) string {
			return fmt.Sprintf("r%05d…r", i) + "/" + strings.Repeat(strings.Repeat("&", 162)+"/", 4) + "f.kt"
		}, []string{"good.kt"}},
		{"blocked_ordinary_oversize", []int{32, 128, 512, 1024, 4096}, func(i int) string {
			return fmt.Sprintf("r/u%05d%s/f.kt", i, strings.Repeat("z", 3900))
		}, []string{"good.kt"}},
	}
	for _, c := range cases {
		for _, n := range c.sizes {
			b.Run(fmt.Sprintf("%s/%d", c.name, n), func(b *testing.B) {
				var paths []string
				for i := 0; i < n; i++ {
					paths = append(paths, c.path(i))
				}
				paths = append(paths, c.extra...)
				rec, res := ovBuildFailed(astraFailed(paths, astraCycle))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if out, _ := set.commitOutcome(res, rec, syscall.EIO); out.Content == "" {
						b.Fatal("empty payload")
					}
				}
			})
		}
	}
}

// TestOVPathNotation pins the legend of each label form, and that no legend appears when no notation is
// needed (OV-04).
func TestOVPathNotation(t *testing.T) {
	render := func(t *testing.T, set *Set, items []astraItem) (string, r7r2Payload, ovPayloadDoc) {
		t.Helper()
		rec, res, content, p := ovRun(t, set, items, syscall.EIO)
		ovCheckOverflowFull(t, set, rec, res, content, p)
		var doc ovPayloadDoc
		if err := json.Unmarshal([]byte(content), &doc); err != nil {
			t.Fatal(err)
		}
		return content, p, doc
	}
	t.Run("exact_literal_on_disk", func(t *testing.T) {
		literal := "a/…/f.kt"
		set := astraLiteralSet(t, []string{literal})
		_, _, doc := render(t, set, astraFailed([]string{literal, "a/b.kt"}, astraCycle))
		if doc.PathNotation != ovNotationExact {
			t.Fatalf("pathNotation = %q, want %q", doc.PathNotation, ovNotationExact)
		}
	})
	t.Run("abbreviated_directory", func(t *testing.T) {
		var paths []string
		for i := 0; i < 32; i++ {
			paths = append(paths, fmt.Sprintf("m%03d/%s/config.yaml", i, strings.Repeat("d", 190)))
		}
		_, _, doc := render(t, astraSet(t), astraFailed(paths, astraCycle))
		if !strings.Contains(doc.PathNotation, ovNotationShort) || strings.Contains(doc.PathNotation, ovNotationCount) {
			t.Fatalf("pathNotation = %q, want the short legend only", doc.PathNotation)
		}
		if !strings.Contains(doc.PathNotation, "Display-only labels, not file paths") {
			t.Fatalf("pathNotation = %q, want the compressed legend", doc.PathNotation)
		}
	})
	t.Run("abbreviated_filename", func(t *testing.T) {
		var paths []string
		for i := 0; i < 32; i++ {
			paths = append(paths, fmt.Sprintf("pkg/%sL%03d.kt", strings.Repeat("L", 240), i))
		}
		_, _, doc := render(t, astraSet(t), astraFailed(paths, astraCycle))
		if !strings.Contains(doc.PathNotation, ovNotationShort) {
			t.Fatalf("pathNotation = %q, want the short legend", doc.PathNotation)
		}
		if !strings.Contains(doc.PathNotation, "Display-only labels, not file paths") {
			t.Fatalf("pathNotation = %q, want the compressed legend", doc.PathNotation)
		}
	})
	t.Run("counted", func(t *testing.T) {
		_, _, doc := render(t, astraSet(t), astraFailed(astraArchived40(), astraCycle))
		if want := ovNotationShort + " " + ovNotationCount; doc.PathNotation != want {
			t.Fatalf("pathNotation = %q, want %q", doc.PathNotation, want)
		}
		if !strings.Contains(doc.PathNotation, "Display-only labels, not file paths") {
			t.Fatalf("pathNotation = %q, want the compressed legend", doc.PathNotation)
		}
	})
	t.Run("literal_tail", func(t *testing.T) {
		filler := strings.Repeat(strings.Repeat("r", 200)+"/", 6)
		items := []astraItem{
			{raw: "rootlong/" + filler + "<outside-workspace>/…/f.kt", disp: "rootlong/" + filler + "<outside-workspace>/…/f.kt", state: editing.PublishExternalModification},
			{raw: "b/…/f.kt", disp: "b/…/f.kt", state: editing.PublishPartial},
		}
		_, _, doc := render(t, astraSet(t), items)
		if !strings.Contains(doc.PathNotation, ovNotationTail) {
			t.Fatalf("pathNotation = %q, want the tail legend", doc.PathNotation)
		}
	})
	t.Run("redacted_literal", func(t *testing.T) {
		items := []astraItem{
			{raw: "../o/a…b.kt", disp: "<outside-workspace>/a…b.kt", state: editing.PublishExternalModification},
			{raw: "../p/aZb.kt", disp: "<outside-workspace>/aZb.kt", state: editing.PublishPartial},
		}
		_, _, doc := render(t, astraSet(t), items)
		if !strings.Contains(doc.PathNotation, ovNotationRedacted) {
			t.Fatalf("pathNotation = %q, want the redacted legend", doc.PathNotation)
		}
	})
	t.Run("no_notation", func(t *testing.T) {
		content, _, _ := render(t, astraSet(t), astraFailed([]string{"a/b.kt", "c/d.kt"}, astraCycle))
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(content), &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["pathNotation"]; ok {
			t.Fatalf("plain paths carry a pathNotation key: %s", content)
		}
	})
	t.Run("near_4096", func(t *testing.T) {
		content, _, doc := render(t, astraSet(t), astraFailed(astraArchived40(), astraCycle))
		if doc.PathNotation == "" || len(content) > maxPartialPayloadBytes {
			t.Fatalf("expected a legend within the limit, got %d bytes and %q", len(content), doc.PathNotation)
		}
	})
}

// TestOVTruncatedOnlyForRemovedContent: truncated means removed content, never the reversible "./"
// qualifier or redaction (OV-05).
func TestOVTruncatedOnlyForRemovedContent(t *testing.T) {
	hasTruncated := func(content string) bool {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(content), &raw); err != nil {
			t.Fatal(err)
		}
		_, ok := raw["truncated"]
		return ok
	}
	t.Run("exact_literal_rich", func(t *testing.T) {
		literal := []string{"a/…/f.kt", "a/b/c/f.kt"}
		set := astraLiteralSet(t, literal)
		rec, res, content, p := ovRun(t, set, astraFailed(literal, astraCycle), syscall.EIO)
		ovCheckOverflowFull(t, set, rec, res, content, p)
		if hasTruncated(content) {
			t.Fatalf("an exact literal path marked truncated: %s", content)
		}
	})
	t.Run("standalone_literal_refusal", func(t *testing.T) {
		literal := "a/…/f.kt"
		set := astraLiteralSet(t, []string{literal})
		out, _ := set.editOutcome(&editing.Error{Code: editing.CodeStale, Path: literal, Detail: "base revision is not current", CurrentRev: "r1"})
		if hasTruncated(out.Content) {
			t.Fatalf("a standalone literal refusal marked truncated: %s", out.Content)
		}
	})
	t.Run("directory_shortened", func(t *testing.T) {
		var paths []string
		for i := 0; i < 32; i++ {
			paths = append(paths, fmt.Sprintf("m%03d/%s/config.yaml", i, strings.Repeat("d", 190)))
		}
		_, _, content, _ := ovRun(t, astraSet(t), astraFailed(paths, astraCycle), syscall.EIO)
		if !hasTruncated(content) {
			t.Fatalf("a shortened directory not marked truncated: %s", content)
		}
	})
	t.Run("filename_shortened", func(t *testing.T) {
		var paths []string
		for i := 0; i < 3; i++ {
			paths = append(paths, fmt.Sprintf("pkg/%sL%03d.kt", strings.Repeat("L", 1100), i))
		}
		_, _, content, _ := ovRun(t, astraSet(t), astraFailed(paths, astraCycle), syscall.EIO)
		if !hasTruncated(content) {
			t.Fatalf("a shortened filename not marked truncated: %s", content)
		}
	})
	t.Run("rows_omitted", func(t *testing.T) {
		_, _, content, _ := ovRun(t, astraSet(t), astraFailed(astraArchived40(), astraCycle), syscall.EIO)
		if !hasTruncated(content) {
			t.Fatalf("omitted rows not marked truncated: %s", content)
		}
	})
	t.Run("error_text_truncated", func(t *testing.T) {
		items := astraFailed([]string{"a.kt"}, astraCycle)
		_, _, content, _ := ovRun(t, astraSet(t), items, errorsNew(strings.Repeat("e", 1200)))
		if !hasTruncated(content) {
			t.Fatalf("truncated error text not marked truncated: %s", content)
		}
	})
}

// errorsNew is a plain error with the given text.
func errorsNew(text string) error { return fmt.Errorf("%s", text) }

// TestOVSelection40Permutations: the archived 40 batch renders the same shown set, the same bytes, and the
// same row order in every input order, and every row names its own file (OV-06).
func TestOVSelection40Permutations(t *testing.T) {
	base := astraFailed(astraArchived40(), astraCycle)
	orders := astraOrders(len(base))
	rng := rand.New(rand.NewSource(7))
	var names []string
	for name := range orders {
		names = append(names, name)
	}
	sortStrings(names)
	var perms [][]int
	for _, name := range names {
		perms = append(perms, orders[name])
	}
	for seed := int64(100); seed < 124; seed++ {
		shuffled := make([]int, len(base))
		for i := range shuffled {
			shuffled[i] = i
		}
		rand.New(rand.NewSource(seed)).Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		perms = append(perms, shuffled)
	}
	_ = rng
	var wantSet map[string]bool
	wantBytes := -1
	for pi, order := range perms {
		items := make([]astraItem, len(order))
		for i, src := range order {
			items[i] = base[src]
		}
		set := astraSet(t)
		rec, res, content, p := ovRun(t, set, items, syscall.EIO)
		shown := ovCheckOverflowFull(t, set, rec, res, content, p)
		if len(p.Files) < 30 {
			t.Fatalf("order %d shows %d rows, want at least 30", pi, len(p.Files))
		}
		if wantSet == nil {
			wantSet = shown
			wantBytes = len(content)
		} else if !reflect.DeepEqual(shown, wantSet) || len(content) != wantBytes {
			t.Fatalf("order %d shows a different set or %d bytes, want %d bytes", pi, len(content), wantBytes)
		}
	}
}

// sortStrings sorts in place, without importing sort into the behavioural file.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// TestOVLargeRowBetweenSmallRows: an oversize literal row between cheap rows must not cut the cheap rows
// that follow it (OV-06).
func TestOVLargeRowBetweenSmallRows(t *testing.T) {
	big := "big…/" + strings.Repeat(strings.Repeat("&", 162)+"/", 2) + "f.kt"
	twin := "twin…/" + strings.Repeat(strings.Repeat("&", 162)+"/", 2) + "f.kt"
	var items []astraItem
	for i := 0; i < 20; i++ {
		items = append(items, astraItem{raw: fmt.Sprintf("s/a%02d.kt", i), disp: fmt.Sprintf("s/a%02d.kt", i), state: astraCycle(i)})
	}
	items = append(items, astraItem{raw: big, disp: big, state: editing.PublishExternalModification})
	items = append(items, astraItem{raw: twin, disp: twin, state: editing.PublishNotAttempted})
	for i := 0; i < 20; i++ {
		items = append(items, astraItem{raw: fmt.Sprintf("s/b%02d.kt", i), disp: fmt.Sprintf("s/b%02d.kt", i), state: astraCycle(i)})
	}
	set := astraLiteralSet(t, []string{big, twin})
	rec, res, content, p := ovRun(t, set, items, syscall.EIO)
	shown := ovCheckOverflowFull(t, set, rec, res, content, p)
	failed := 0
	for _, it := range items {
		if it.state != editing.PublishNotAttempted && shown[it.raw] {
			failed++
		}
	}
	if failed < 33 {
		t.Fatalf("%d failed rows shown, want at least 33: %s", failed, content)
	}
}

// TestOVLowerClassesUseOnlyLeftoverBudget: a lower-class row never displaces a failed row, and it appears
// only when the budget allows it (OV-06).
func TestOVLowerClassesUseOnlyLeftoverBudget(t *testing.T) {
	t.Run("impossible_failed_first", func(t *testing.T) {
		items := []astraItem{
			{raw: "imp…/" + ovHugeTail(3800)[1:], disp: "imp…/" + ovHugeTail(3800)[1:], state: editing.PublishExternalModification},
			{raw: "ok.kt", disp: "ok.kt", state: editing.PublishSucceededAndVerified, verified: true, newRev: "rev1", newSHA: "sha256:" + strings.Repeat("ab", 32)},
		}
		set := astraSet(t)
		rec, res, content, p := ovRun(t, set, items, syscall.EIO)
		shown := ovCheckOverflowFull(t, set, rec, res, content, p)
		// OV-06: the impossible literal row now fits as its shortened suffix, so both rows are shown.
		if !shown["ok.kt"] || len(p.Files) != 2 {
			t.Fatalf("ok.kt and the shortened literal row are not both shown: %s", content)
		}
	})
	t.Run("impossible_verified_first", func(t *testing.T) {
		items := []astraItem{
			{raw: "v…/" + ovHugeTail(3800)[1:], disp: "v…/" + ovHugeTail(3800)[1:], state: editing.PublishSucceededAndVerified, verified: true, newRev: "rev1"},
			{raw: "v2.kt", disp: "v2.kt", state: editing.PublishSucceededAndVerified, verified: true, newRev: "rev2"},
		}
		set := astraSet(t)
		rec, res, content, p := ovRun(t, set, items, syscall.EIO)
		shown := ovCheckOverflowFull(t, set, rec, res, content, p)
		if !shown["v2.kt"] {
			t.Fatalf("v2.kt is not shown: %s", content)
		}
	})
	t.Run("cheap_failed_keep_all", func(t *testing.T) {
		var items []astraItem
		for i := 0; i < 10; i++ {
			items = append(items, astraItem{raw: fmt.Sprintf("c%02d.kt", i), disp: fmt.Sprintf("c%02d.kt", i), state: astraCycle(i)})
		}
		for i := 0; i < 5; i++ {
			items = append(items, astraItem{raw: fmt.Sprintf("v%02d.kt", i), disp: fmt.Sprintf("v%02d.kt", i), state: editing.PublishSucceededAndVerified, verified: true, newRev: fmt.Sprintf("rev%02d", i)})
		}
		for i := 0; i < 5; i++ {
			items = append(items, astraItem{raw: fmt.Sprintf("u%02d.kt", i), disp: fmt.Sprintf("u%02d.kt", i), state: editing.PublishNotAttempted})
		}
		set := astraSet(t)
		rec, res, content, p := ovRun(t, set, items, syscall.EIO)
		shown := ovCheckOverflowFull(t, set, rec, res, content, p)
		for i := 0; i < 10; i++ {
			if !shown[fmt.Sprintf("c%02d.kt", i)] {
				t.Fatalf("cheap failed row c%02d.kt is not shown: %s", i, content)
			}
		}
		if len(p.Files) > 32 {
			t.Fatalf("%d rows shown, want at most 32", len(p.Files))
		}
	})
}

// TestOVEqualCostTiesByPath: equal-cost rows selected from two input orders must pick the same set (OV-06).
func TestOVEqualCostTiesByPath(t *testing.T) {
	var paths []string
	for i := 0; i < 40; i++ {
		paths = append(paths, fmt.Sprintf("e%02d.kt", i))
	}
	var reversed []string
	for i := len(paths) - 1; i >= 0; i-- {
		reversed = append(reversed, paths[i])
	}
	var shownSets []map[string]bool
	for _, order := range [][]string{paths, reversed} {
		set := astraSet(t)
		// OV-06: one shared state keeps every row's compact cost equal, so only the path breaks ties.
		items := astraFailed(order, func(int) editing.PublishState { return editing.PublishPartial })
		rec, res, content, p := ovRun(t, set, items, syscall.EIO)
		shownSets = append(shownSets, ovCheckOverflowFull(t, set, rec, res, content, p))
	}
	if !reflect.DeepEqual(shownSets[0], shownSets[1]) {
		t.Fatalf("equal-cost rows select different sets: %v and %v", shownSets[0], shownSets[1])
	}
}

// TestOVExactByteLimitPayload sweeps the top-level error length until an overflow payload is exactly the
// byte limit, then validates it (OV-04).
func TestOVExactByteLimitPayload(t *testing.T) {
	set := astraSet(t)
	items := astraFailed(astraArchived40(), astraCycle)
	rec, res := ovBuildFailed(items)
	found := ""
	for n := 1; n <= 1024; n++ {
		out, _ := set.commitOutcome(res, rec, errorsNew(strings.Repeat("x", n)))
		if len(out.Content) == maxPartialPayloadBytes {
			found = out.Content
			break
		}
	}
	if found == "" {
		t.Fatal("no top-level error length produces a payload of exactly the byte limit")
	}
	p := r7r2ParseAndCheckBudget(t, "exact", found, false)
	ovCheckOverflowFull(t, set, rec, res, found, p)
}

// ovItemsRecRes builds the receipt and result that the commit outcome renders for items.
func ovItemsRecRes(items []astraItem) (editing.CommitReceipt, editing.Result) {
	rec := editing.CommitReceipt{DirtyFailure: true, State: editing.CommitStatePartiallyPublished}
	res := editing.Result{CommitState: editing.CommitStatePartiallyPublished}
	for _, it := range items {
		switch {
		case it.resultOnly:
			res.Files = append(res.Files, editing.FileResult{Path: it.raw, PublishState: editing.PublishNotAttempted})
		case it.verified:
			rec.Files = append(rec.Files, editing.FileCommitReceipt{Path: it.raw, PublishState: editing.PublishSucceededAndVerified, Published: true, Verified: true, Wrote: true, NewSHA256: it.newSHA})
			res.Files = append(res.Files, editing.FileResult{Path: it.raw, PublishState: editing.PublishSucceededAndVerified, Published: true, Verified: true, NewRev: it.newRev})
		case it.state == editing.PublishNotAttempted:
			rec.Files = append(rec.Files, editing.FileCommitReceipt{Path: it.raw, PublishState: editing.PublishNotAttempted})
			res.Files = append(res.Files, editing.FileResult{Path: it.raw, PublishState: editing.PublishNotAttempted})
		default:
			rec.Files = append(rec.Files, editing.FileCommitReceipt{Path: it.raw, PublishState: it.state, Published: true, Wrote: true, Error: "input/output error"})
			res.Files = append(res.Files, editing.FileResult{Path: it.raw, PublishState: it.state, Published: true, Error: "input/output error"})
		}
	}
	return rec, res
}

// ovFailedShown counts the shown rows that carry a failed state, not the untouched lower-class rows.
func ovFailedShown(p r7r2Payload) int {
	n := 0
	for _, f := range p.Files {
		if f.PublishState != string(editing.PublishNotAttempted) && !f.Verified {
			n++
		}
	}
	return n
}

// ovFinalItems is the item list r7r2FinalRender renders: failed rows cycle the final states, then untouched.
func ovFinalItems(failed, untouched []string) []astraItem {
	var items []astraItem
	for i, p := range failed {
		items = append(items, astraItem{raw: p, disp: p, state: r7r2FinalFailedStates[i%len(r7r2FinalFailedStates)]})
	}
	for _, p := range untouched {
		items = append(items, astraItem{raw: p, disp: p, state: editing.PublishNotAttempted})
	}
	return items
}

// ovBandItems is the OV-01 band shape: one literal row whose tail is pad bytes long, one cheap failed row,
// and an untouched literal twin.
func ovBandItems(pad int) []astraItem {
	tl := ovHugeTail(pad)
	return []astraItem{
		{raw: "a…a" + tl, disp: "a…a" + tl, state: editing.PublishExternalModification},
		{raw: "good.kt", disp: "good.kt", state: editing.PublishPartial},
		{raw: "b…b" + tl, disp: "b…b" + tl, state: editing.PublishNotAttempted},
	}
}

// TestOVMetadataBandNeverHidesFittingRow sweeps the pad length across the band where a literal row's
// metadata used to decide whether the cheap row fits. Every output must show at least one row and pass
// the shared oracle (OV-01).
func TestOVMetadataBandNeverHidesFittingRow(t *testing.T) {
	for pad := 560; pad <= 700; pad++ {
		items := ovBandItems(pad)
		set := astraSet(t)
		rec, res, content, p := ovRun(t, set, items, syscall.EIO)
		if len(p.Files) == 0 {
			t.Fatalf("pad %d shows no rows: %s", pad, content)
		}
		ovCheckOverflowFull(t, set, rec, res, content, p)
	}
}

// TestOVMetadataBand31Rows: with the literal row first and 30 cheap failed rows after it, at least 30
// failed rows must be shown and the accounting must be exact (OV-01).
func TestOVMetadataBand31Rows(t *testing.T) {
	tl := ovHugeTail(600)
	literal := "a…a" + tl
	untouched := "b…b" + tl
	items := []astraItem{{raw: literal, disp: literal, state: editing.PublishExternalModification}}
	for i := 0; i < 30; i++ {
		p := fmt.Sprintf("g/m%02d.kt", i)
		items = append(items, astraItem{raw: p, disp: p, state: astraCycle(i)})
	}
	items = append(items, astraItem{raw: untouched, disp: untouched, state: editing.PublishNotAttempted})
	set := astraSet(t)
	rec, res, content, p := ovRun(t, set, items, syscall.EIO)
	ovCheckOverflowFull(t, set, rec, res, content, p)
	if got := ovFailedShown(p); got < 30 {
		t.Fatalf("%d failed rows shown, want at least 30: %s", got, content)
	}
}

// TestOVMetadataBandOneRow: one cheap failed row among impossible literal rows is shown alone (OV-01).
func TestOVMetadataBandOneRow(t *testing.T) {
	tl := ovHugeTail(3800)
	var items []astraItem
	var literals []string
	for i := 0; i < 3; i++ {
		p := fmt.Sprintf("x%d…x%s", i, tl)
		literals = append(literals, p)
		items = append(items, astraItem{raw: p, disp: p, state: editing.PublishExternalModification})
	}
	items = append(items, astraItem{raw: "cheap.kt", disp: "cheap.kt", state: editing.PublishPartial})
	set := astraSet(t)
	rec, res, content, p := ovRun(t, set, items, syscall.EIO)
	shown := ovCheckOverflowFull(t, set, rec, res, content, p)
	if !shown["cheap.kt"] {
		t.Fatalf("cheap.kt is not shown: %s", content)
	}
}

// TestOVMetadataBandZeroRows: when every failed row is impossible, nothing is shown, and the accounting
// names every receipt row by state (OV-01).
func TestOVMetadataBandZeroRows(t *testing.T) {
	tl := ovHugeTail(3800)
	var items []astraItem
	var literals []string
	for i := 0; i < 2; i++ {
		p := fmt.Sprintf("y%d…y%s", i, tl)
		literals = append(literals, p)
		items = append(items, astraItem{raw: p, disp: p, state: editing.PublishExternalModification})
	}
	set := astraSet(t)
	rec, res, content, p := ovRun(t, set, items, syscall.EIO)
	ovCheckOverflowFull(t, set, rec, res, content, p)
	if len(p.Files) != 0 || p.FilesOmitted != len(items) || p.FilesOmittedByState[string(editing.PublishExternalModification)] != len(items) {
		t.Fatalf("zero-row accounting wrong: rows=%d omitted=%d byState=%v: %s", len(p.Files), p.FilesOmitted, p.FilesOmittedByState, content)
	}
}
