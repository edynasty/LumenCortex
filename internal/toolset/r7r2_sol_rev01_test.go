package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/internal/editing"
)

const (
	solRevLenient = iota
	solRevStrict
	solRevLoose
)

// solRevPattern matches a shortened label against a display path. Under the component readings a
// whole-component "…" removes at least one component (lenient) or zero or more (strict), and an
// in-component "…" removes at least one byte. The loose reading lets every "…" match any text, slash
// included.
func solRevPattern(label string, reading int) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	if reading == solRevLoose {
		b.WriteString("(?s)")
		for i, part := range strings.Split(label, pathEllipsis) {
			if i > 0 {
				b.WriteString(".+")
			}
			b.WriteString(regexp.QuoteMeta(part))
		}
	} else {
		comps := strings.Split(label, "/")
		for i, c := range comps {
			last := i == len(comps)-1
			if c == pathEllipsis && !last {
				if reading == solRevLenient {
					b.WriteString(`(?:[^/]+/)+`)
				} else {
					b.WriteString(`(?:[^/]+/)*`)
				}
				continue
			}
			for j, part := range strings.Split(c, pathEllipsis) {
				if j > 0 {
					b.WriteString(`[^/]+`)
				}
				b.WriteString(regexp.QuoteMeta(part))
			}
			if !last {
				b.WriteString("/")
			}
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// solRevMatches returns the attempted display paths a label denotes. Literal equality counts.
func solRevMatches(label string, attempted []string, reading int) []string {
	re := solRevPattern(label, reading)
	var out []string
	for _, a := range attempted {
		if a == label || re.MatchString(a) {
			out = append(out, a)
		}
	}
	return out
}

func solRevSet(t *testing.T) *Set {
	t.Helper()
	set, err := New(Options{Workspace: t.TempDir(), Policy: PolicyWorkspace, SessionID: "s-sol-rev01"})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// solRevInputs builds the receipt and result of a failed-row commit exactly as r7r2FinalRender does:
// failed rows cycle through r7r2FinalFailedStates with a row error, untouched rows are not attempted.
func solRevInputs(failed, untouched []string) (editing.CommitReceipt, editing.Result) {
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
	return editing.CommitReceipt{DirtyFailure: true, State: editing.CommitStatePartiallyPublished, Files: files},
		editing.Result{CommitState: editing.CommitStatePartiallyPublished, Files: results}
}

type solRevSnapshot struct {
	rec editing.CommitReceipt
	res editing.Result
}

func solRevSnap(rec editing.CommitReceipt, res editing.Result) solRevSnapshot {
	rec.Files = append([]editing.FileCommitReceipt(nil), rec.Files...)
	res.Files = append([]editing.FileResult(nil), res.Files...)
	return solRevSnapshot{rec, res}
}

func solRevRender(t *testing.T, set *Set, rec editing.CommitReceipt, res editing.Result, top error) (string, r7r2Payload) {
	t.Helper()
	before := solRevSnap(rec, res)
	out, _ := set.commitOutcome(res, rec, top)
	if !reflect.DeepEqual(before, solRevSnap(rec, res)) {
		t.Fatal("rendering mutated the receipt or result")
	}
	return out.Content, r7r2ParseAndCheckBudget(t, "sol-rev01", out.Content, out.OK)
}

// solRevCheck asserts the invariants of every case: the envelope classifies the failure, each shown
// label resolves to exactly the row it names under the lenient and loose readings, rows keep the
// priority order and their states, no unverified row carries a revision or hash, the omission
// accounting covers exactly the rows not shown, and no shown label can be shortened further.
func solRevCheck(t *testing.T, set *Set, rec editing.CommitReceipt, res editing.Result, content string, p r7r2Payload, top error, attempted, failedTotal, minRows, wantRows, wantFailedRows int) {
	t.Helper()
	display := set.displayPaths()
	ordered := prioritizedReceiptFiles(rec.Files)
	ranked := 0
	for _, f := range ordered {
		if receiptFileRank(f) == 0 {
			ranked++
		}
	}
	if len(rec.Files) != attempted || ranked != failedTotal {
		t.Fatalf("receipt has %d attempted and %d failed rows, want %d and %d", len(rec.Files), ranked, attempted, failedTotal)
	}
	var envelope struct {
		Error              string          `json:"error"`
		Code               string          `json:"code"`
		CommitState        string          `json:"commitState"`
		DirtyFailure       bool            `json:"dirtyFailure"`
		PartialPublication bool            `json:"partialPublication"`
		Files              []toolFileState `json:"files"`
	}
	if err := json.Unmarshal([]byte(content), &envelope); err != nil {
		t.Fatal(err)
	}
	if want := top.Error(); envelope.Error != want && !(p.Truncated && strings.HasSuffix(envelope.Error, pathEllipsis) &&
		strings.HasPrefix(want, strings.TrimSuffix(envelope.Error, pathEllipsis))) {
		t.Fatalf("error = %q, want %q", envelope.Error, want)
	}
	if envelope.Code != "" || envelope.CommitState != string(editing.CommitStatePartiallyPublished) ||
		!envelope.PartialPublication || !envelope.DirtyFailure {
		t.Fatalf("classification code=%q commitState=%q partialPublication=%v dirtyFailure=%v", envelope.Code, envelope.CommitState, envelope.PartialPublication, envelope.DirtyFailure)
	}
	byPath := make(map[string]editing.FileResult, len(res.Files))
	for _, f := range res.Files {
		byPath[f.Path] = f
	}
	shown := len(p.Files)
	if shown < minRows || shown != wantRows {
		t.Fatalf("shown %d rows, want %d (at least %d): %s", shown, wantRows, minRows, content)
	}
	failedShown := 0
	for _, f := range ordered[:shown] {
		if receiptFileRank(f) == 0 {
			failedShown++
		}
	}
	if failedShown != wantFailedRows {
		t.Fatalf("retained %d failed rows, want %d", failedShown, wantFailedRows)
	}
	var attemptedPaths []string
	attemptedSet := map[string]bool{}
	for _, f := range rec.Files {
		if d := display.Path(f.Path); !attemptedSet[d] {
			attemptedSet[d] = true
			attemptedPaths = append(attemptedPaths, d)
		}
	}
	// Each shown label names one receipt path (OV-06: rows need not follow input order), so the state
	// and accounting are checked against the path it names, not against the input position.
	byDisp := map[string]editing.FileCommitReceipt{}
	for _, f := range rec.Files {
		byDisp[display.Path(f.Path)] = f
	}
	shownDisp := map[string]bool{}
	for i, row := range p.Files {
		matches := solRevMatches(row.Path, attemptedPaths, solRevLenient)
		if len(matches) != 1 {
			t.Fatalf("row %d label %q resolves to %v, want exactly one path", i, row.Path, matches)
		}
		for _, reading := range []int{solRevStrict, solRevLoose} {
			loose := solRevMatches(row.Path, attemptedPaths, reading)
			if reading == solRevLoose && (len(loose) != 1 || loose[0] != matches[0]) {
				t.Fatalf("row %d label %q resolves to %v under the loose reading, want only %q", i, row.Path, loose, matches[0])
			}
		}
		want := matches[0]
		f, ok := byDisp[want]
		if !ok || shownDisp[want] {
			t.Fatalf("row %d label %q names %q, which is not a new receipt row", i, row.Path, want)
		}
		shownDisp[want] = true
		if row.PublishState != string(f.PublishState) || row.Verified != f.Verified ||
			row.Published != f.Published || row.Wrote != f.Wrote {
			t.Fatalf("row %d = %+v does not carry the state of %q", i, row, want)
		}
		raw := envelope.Files[i]
		if !f.Verified {
			if raw.NewRev != "" || raw.SHA256 != "" {
				t.Fatalf("unverified row %d carries a revision or hash: %+v", i, raw)
			}
			continue
		}
		if raw.NewRev != "" && raw.NewRev != byPath[f.Path].NewRev {
			t.Fatalf("verified row %d revision %q, want %q", i, raw.NewRev, byPath[f.Path].NewRev)
		}
		if raw.SHA256 != "" && raw.SHA256 != f.NewSHA256 {
			t.Fatalf("verified row %d hash %q, want %q", i, raw.SHA256, f.NewSHA256)
		}
	}
	omitted := map[string]int{}
	for _, f := range rec.Files {
		if !shownDisp[display.Path(f.Path)] {
			omitted[string(f.PublishState)]++
		}
	}
	if p.FilesOmitted != len(rec.Files)-shown || !reflect.DeepEqual(emptyAsNil(p.FilesOmittedByState), emptyAsNil(omitted)) {
		t.Fatalf("omitted=%d byState=%v, want %d and %v", p.FilesOmitted, p.FilesOmittedByState, len(rec.Files)-shown, omitted)
	}
	failedShownSet := 0
	for d := range shownDisp {
		if receiptFileRank(byDisp[d]) == 0 {
			failedShownSet++
		}
	}
	if failedShownSet != wantFailedRows {
		t.Fatalf("retained %d failed rows, want %d", failedShownSet, wantFailedRows)
	}
	shownRaw := map[string]bool{}
	for _, f := range rec.Files {
		if shownDisp[display.Path(f.Path)] {
			shownRaw[f.Path] = true
		}
	}
	ovAssertFull(t, set, rec, res, content, shownRaw)
	t.Logf("bytes=%d shown=%d omitted=%d byState=%v", len(content), shown, p.FilesOmitted, p.FilesOmittedByState)
}

// solRevMaximal rebuilds the envelope with k compact rows. The shown rows must fit, and every k above
// them up to the overflow candidate space (the leading failed rows, bounded by maxCandidateFileRows)
// must exceed the byte limit.
func solRevMaximal(t *testing.T, set *Set, rec editing.CommitReceipt, res editing.Result, content string, shown int) {
	t.Helper()
	ordered := prioritizedReceiptFiles(rec.Files)
	fileFloor := 0
	for fileFloor < len(ordered) && receiptFileRank(ordered[fileFloor]) == 0 {
		fileFloor++
	}
	fileFloor = max(fileFloor, 1)
	display := set.displayPaths()
	byPath := make(map[string]editing.FileResult, len(res.Files))
	idx := newFailurePathIndex()
	for _, f := range rec.Files {
		idx.add(f.Path, display)
	}
	for _, f := range res.Files {
		idx.add(f.Path, display)
		byPath[f.Path] = f
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &envelope); err != nil {
		t.Fatal(err)
	}
	size := func(k int) int {
		rows := make([]toolFileState, 0, k)
		for _, f := range ordered[:k] {
			state, _ := receiptFileState(f, idx.label(f.Path, 0), "", byPath, 0, false)
			rows = append(rows, state)
		}
		candidate := make(map[string]json.RawMessage, len(envelope))
		for key, v := range envelope {
			candidate[key] = v
		}
		candidate["files"], _ = json.Marshal(rows)
		if omitted := len(ordered) - k; omitted > 0 {
			counts := map[string]int{}
			for _, f := range ordered[k:] {
				counts[string(f.PublishState)]++
			}
			candidate["filesOmitted"], _ = json.Marshal(omitted)
			candidate["filesOmittedByState"], _ = json.Marshal(counts)
		} else {
			delete(candidate, "filesOmitted")
			delete(candidate, "filesOmittedByState")
		}
		raw, _ := json.Marshal(candidate)
		return len(raw)
	}
	if n := size(shown); n > maxPartialPayloadBytes {
		t.Fatalf("the %d shown rows rebuild to %d bytes, over %d", shown, n, maxPartialPayloadBytes)
	}
	for k := shown + 1; k <= min(fileFloor, maxCandidateFileRows); k++ {
		if n := size(k); n <= maxPartialPayloadBytes {
			t.Fatalf("maximality: %d compact rows fit in %d bytes but the payload kept %d", k, n, shown)
		}
	}
}

func solRevPathsPrefix(prefix string, n int, name func(i int) string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = prefix + name(i)
	}
	return out
}

type solRevCase struct {
	name        string
	failed      []string
	untouched   []string
	top         error
	minRows     int
	rows        int
	failedRows  int
	attempted   int
	failedTotal int
}

func solRevRunCase(t *testing.T, c solRevCase) {
	set := solRevSet(t)
	rec, res := solRevInputs(c.failed, c.untouched)
	content, p := solRevRender(t, set, rec, res, c.top)
	solRevCheck(t, set, rec, res, content, p, c.top, c.attempted, c.failedTotal, c.minRows, c.rows, c.failedRows)
}

// TestSOLREV01SolFloorCases pins the five shapes where the compact labels lost failed rows: a
// filename whose distinguishing text sits at the end, at the start, or in three paths of 1404 bytes.
func TestSOLREV01SolFloorCases(t *testing.T) {
	dir := "src/main/kotlin/pkg/"
	for _, tc := range []solRevCase{
		{name: "end_k70", minRows: 21, rows: 29, failedRows: 29, // OV-04: compressed legend
			attempted: 32, failedTotal: 32, failed: solRevPathsPrefix(dir, 32, func(i int) string {
				return strings.Repeat("L", 70) + fmt.Sprintf("%03d", i) + ".kt"
			})},
		{name: "end_k100", minRows: 21, rows: 29, failedRows: 29, // OV-04: compressed legend
			attempted: 32, failedTotal: 32, failed: solRevPathsPrefix(dir, 32, func(i int) string {
				return strings.Repeat("L", 100) + fmt.Sprintf("%03d", i) + ".kt"
			})},
		{name: "end_k240", minRows: 20, rows: 29, failedRows: 29, // OV-04: compressed legend
			attempted: 32, failedTotal: 32, failed: solRevPathsPrefix(dir, 32, func(i int) string {
				return strings.Repeat("L", 240) + fmt.Sprintf("%03d", i) + ".kt"
			})},
		{name: "start_k240", minRows: 20, rows: 29, failedRows: 29, // OV-04: compressed legend
			attempted: 32, failedTotal: 32, failed: solRevPathsPrefix(dir, 32, func(i int) string {
				return fmt.Sprintf("%03d", i) + strings.Repeat("L", 240) + ".kt"
			})},
		{name: "three_1404", minRows: 3, rows: 3, failedRows: 3, attempted: 3, failedTotal: 3, failed: []string{
			"a/" + strings.Repeat("n", 1400) + "1.kt",
			"b/" + strings.Repeat("n", 1400) + "2.kt",
			"c/" + strings.Repeat("n", 1400) + "3.kt",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.top = syscall.EIO
			solRevRunCase(t, tc)
		})
	}
}

// TestSOLREV01PositionSweep pins 32 failed rows with the distinguishing number placed at the start,
// middle, or end of a filename of growing length.
func TestSOLREV01PositionSweep(t *testing.T) {
	sizes := []int{20, 40, 50, 60, 70, 80, 100, 120, 160, 200, 240}
	floors := map[string][]int{
		"end":    {27, 23, 22, 21, 21, 21, 21, 21, 21, 21, 21},
		"middle": {27, 23, 22, 21, 21, 21, 21, 19, 18, 19, 19},
		"start":  {27, 23, 22, 21, 19, 18, 17, 20, 20, 20, 20},
	}
	// OV-04: pathNotation bytes shift the shown counts by one row.
	exact := map[string][]int{
		"end":    {27, 29, 29, 29, 29, 29, 29, 29, 29, 29, 29},
		"middle": {27, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28},
		"start":  {27, 29, 29, 29, 29, 29, 29, 29, 29, 29, 29},
	}
	for _, pos := range []string{"end", "middle", "start"} {
		for j, k := range sizes {
			name := func(i int) string {
				id := fmt.Sprintf("%03d", i)
				switch pos {
				case "start":
					return id + strings.Repeat("L", k) + ".kt"
				case "middle":
					return strings.Repeat("L", k/2) + id + strings.Repeat("L", k-k/2) + ".kt"
				default:
					return strings.Repeat("L", k) + id + ".kt"
				}
			}
			t.Run(fmt.Sprintf("%s_%d", pos, k), func(t *testing.T) {
				solRevRunCase(t, solRevCase{
					name:        pos,
					failed:      solRevPathsPrefix("src/main/kotlin/pkg/", 32, name),
					top:         syscall.EIO,
					minRows:     floors[pos][j],
					rows:        exact[pos][j],
					failedRows:  exact[pos][j],
					attempted:   32,
					failedTotal: 32,
				})
			})
		}
	}
}

// TestSOLREV01LongNameShapes covers repeated, Unicode, spaced, sibling, and case-varied long filenames.
func TestSOLREV01LongNameShapes(t *testing.T) {
	long := strings.Repeat("abcde", 40)
	caseVariant := func(i int) string {
		var b strings.Builder
		for j, r := range long {
			if (i>>(j%5))&1 == 1 {
				b.WriteRune(r - 'a' + 'A')
			} else {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	for _, tc := range []solRevCase{
		{name: "identical_long_prefixes", minRows: 1, rows: 29, failedRows: 29, // OV-04: compressed legend
			attempted: 32, failedTotal: 32, failed: solRevPathsPrefix("dir/", 32, func(i int) string {
				return strings.Repeat("p", 220) + fmt.Sprintf("_%02d_end.kt", i)
			})},
		{name: "identical_long_suffixes", minRows: 1, rows: 30, failedRows: 30, // OV-04: compressed legend
			attempted: 32, failedTotal: 32, failed: solRevPathsPrefix("", 32, func(i int) string {
				return fmt.Sprintf("m%02d_", i) + strings.Repeat("s", 200) + ".kt"
			})},
		{name: "same_name_32_modules", minRows: 1, rows: 26, failedRows: 26, attempted: 32, failedTotal: 32, failed: solRevPathsPrefix("", 32, func(i int) string {
			return fmt.Sprintf("services/module%02d/src/", i) + strings.Repeat("L", 200) + ".kt"
		})},
		{name: "unicode_end_and_middle", minRows: 1, rows: 28, failedRows: 28, attempted: 32, failedTotal: 32, failed: append(
			solRevPathsPrefix("界/", 16, func(i int) string {
				return strings.Repeat("界", 80) + fmt.Sprintf("%02d", i) + ".kt"
			}),
			solRevPathsPrefix("中/", 16, func(i int) string {
				return strings.Repeat("界", 40) + fmt.Sprintf("%02d", i) + strings.Repeat("界", 40) + ".kt"
			})...)},
		{name: "filenames_with_spaces", minRows: 1, rows: 29, failedRows: 29, // OV-04: compressed legend
			attempted: 32, failedTotal: 32, failed: solRevPathsPrefix("dir a/", 32, func(i int) string {
				return strings.Repeat("sp ace ", 40) + fmt.Sprintf("%02d", i) + ".kt"
			})},
		{name: "same_long_name_two_siblings", minRows: 2, rows: 2, failedRows: 2, attempted: 2, failedTotal: 2, failed: []string{
			"left/" + strings.Repeat("L", 200) + ".kt",
			"right/" + strings.Repeat("L", 200) + ".kt",
		}},
		{name: "long_names_under_190_byte_dirs", minRows: 1, rows: 29, failedRows: 29, // OV-04: compressed legend
			attempted: 32, failedTotal: 32, failed: solRevPathsPrefix("", 32, func(i int) string {
				return fmt.Sprintf("%02d", i) + strings.Repeat("q", 188) + "/" + strings.Repeat("r", 190) + "/" +
					fmt.Sprintf("f%02d", i) + strings.Repeat("z", 100) + ".kt"
			})},
		{name: "repeated_dirs_at_different_depths", minRows: 1, rows: 29, failedRows: 29, // OV-04: compressed legend
			attempted: 32, failedTotal: 32, failed: solRevPathsPrefix("", 32, func(i int) string {
				return strings.Repeat("rep/", i%4+1) + fmt.Sprintf("%02d", i) + strings.Repeat("L", 200) + ".kt"
			})},
		{name: "directory_shares_unique_prefix", minRows: 1, rows: 29, failedRows: 29, // OV-04: compressed legend
			attempted: 33, failedTotal: 32, failed: solRevPathsPrefix("pkg/", 32, func(i int) string {
				return "000" + strings.Repeat("L", 240) + fmt.Sprintf("%02d.kt", i)
			}), untouched: []string{"000" + strings.Repeat("L", 20) + "_dir/other.kt"}},
		{name: "case_variants_stay_distinct", minRows: 1, rows: 29, failedRows: 29, // OV-04: compressed legend
			attempted: 32, failedTotal: 32, failed: solRevPathsPrefix("case/", 32, func(i int) string {
				return caseVariant(i) + ".kt"
			})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.top = syscall.EIO
			solRevRunCase(t, tc)
		})
	}
}

// TestSOLREV01PathlessCancellation: a cancellation names no path, so only the receipt rows are shown.
func TestSOLREV01PathlessCancellation(t *testing.T) {
	solRevRunCase(t, solRevCase{
		name:    "pathless",
		minRows: 20, rows: 29, failedRows: 29, // OV-04: compressed legend
		attempted: 32, failedTotal: 32,
		top: context.Canceled,
		failed: solRevPathsPrefix("src/main/kotlin/pkg/", 32, func(i int) string {
			return strings.Repeat("L", 100) + fmt.Sprintf("%03d", i) + ".kt"
		}),
	})
}

// TestSOLREV01MixedStatesWithoutErrorText covers verified, untouched, failed-before-write, and
// unsupported rows, where the last two carry no error text.
func TestSOLREV01MixedStatesWithoutErrorText(t *testing.T) {
	long := func(i int) string { return "mix/" + strings.Repeat("M", 120) + fmt.Sprintf("%02d.kt", i) }
	rec := editing.CommitReceipt{DirtyFailure: true, State: editing.CommitStatePartiallyPublished}
	res := editing.Result{CommitState: editing.CommitStatePartiallyPublished}
	for i := 0; i < 6; i++ {
		state := r7r2FinalFailedStates[i%len(r7r2FinalFailedStates)]
		rec.Files = append(rec.Files, editing.FileCommitReceipt{Path: long(i), PublishState: state, Published: true, Wrote: true, Error: "input/output error"})
		res.Files = append(res.Files, editing.FileResult{Path: long(i), PublishState: state, Published: true, Error: "input/output error"})
	}
	for i := 6; i < 9; i++ {
		rec.Files = append(rec.Files, editing.FileCommitReceipt{Path: long(i), PublishState: editing.PublishSucceededAndVerified, Published: true, Wrote: true, Verified: true})
		res.Files = append(res.Files, editing.FileResult{Path: long(i), PublishState: editing.PublishSucceededAndVerified, Published: true})
	}
	for i := 9; i < 11; i++ {
		rec.Files = append(rec.Files, editing.FileCommitReceipt{Path: long(i), PublishState: editing.PublishFailedBeforeWrite})
		res.Files = append(res.Files, editing.FileResult{Path: long(i), PublishState: editing.PublishFailedBeforeWrite})
	}
	for i := 11; i < 13; i++ {
		rec.Files = append(rec.Files, editing.FileCommitReceipt{Path: long(i), PublishState: editing.PublishUnsupported})
		res.Files = append(res.Files, editing.FileResult{Path: long(i), PublishState: editing.PublishUnsupported})
	}
	for i := 13; i < 16; i++ {
		rec.Files = append(rec.Files, editing.FileCommitReceipt{Path: long(i), PublishState: editing.PublishNotAttempted})
		res.Files = append(res.Files, editing.FileResult{Path: long(i), PublishState: editing.PublishNotAttempted})
	}
	set := solRevSet(t)
	content, p := solRevRender(t, set, rec, res, syscall.EIO)
	solRevCheck(t, set, rec, res, content, p, syscall.EIO, 16, 10, 1, 16, 10)
}

// TestSOLREV01BoundaryExactly4096 tunes the top-level error so the rich payload is exactly the byte
// limit, then one more byte must force a truncated result that still fits.
func TestSOLREV01BoundaryExactly4096(t *testing.T) {
	set := solRevSet(t)
	name := func(i int) string { return strings.Repeat("p", 200) + fmt.Sprintf("%02d.kt", i) }
	// The row count and the top-level error length together reach the exact byte limit.
	var rec editing.CommitReceipt
	var res editing.Result
	exact := -1
	for rows := 1; rows <= 31 && exact < 0; rows++ {
		failed := solRevPathsPrefix("b/", rows, name)
		rec, res = solRevInputs(failed, nil)
		for n := 1; n <= 1024; n++ {
			out, _ := set.commitOutcome(res, rec, errors.New(strings.Repeat("x", n)))
			p := r7r2ParseAndCheckBudget(t, "boundary", out.Content, out.OK)
			if len(out.Content) == maxPartialPayloadBytes && !p.Truncated {
				exact = n
				break
			}
		}
	}
	if exact < 0 {
		t.Fatal("no row count and top-level error length produce an untruncated payload of exactly 4096 bytes")
	}
	out, _ := set.commitOutcome(res, rec, errors.New(strings.Repeat("x", exact+1)))
	p := r7r2ParseAndCheckBudget(t, "boundary+1", out.Content, out.OK)
	if !p.Truncated {
		t.Fatalf("one more byte kept an untruncated payload: %d bytes", len(out.Content))
	}
	solRevCheck(t, set, rec, res, out.Content, p, errors.New(strings.Repeat("x", exact+1)), 5, 5, 1, 5, 5)
}

// TestSOLREV01ImpossibleToFit: 200 long failed filenames cannot all fit, so the omissions must be exact.
func TestSOLREV01ImpossibleToFit(t *testing.T) {
	solRevRunCase(t, solRevCase{
		name:    "impossible",
		minRows: 1, rows: 29, failedRows: 29, attempted: 200, failedTotal: 200,
		top: syscall.EIO,
		failed: solRevPathsPrefix("src/main/kotlin/pkg/", 200, func(i int) string {
			return strings.Repeat("L", 240) + fmt.Sprintf("%03d", i) + ".kt"
		}),
	})
}

// TestSOLREV01SharedSuffixNeverResolvesToOmittedFile: a shown label must not denote the omitted file
// that shares its long suffix. The overflow envelope must also show a filename whose JSON exceeds the
// budget, abbreviated.
func TestSOLREV01SharedSuffixNeverResolvesToOmittedFile(t *testing.T) {
	t.Run("shown_vs_omitted_shared_suffix", func(t *testing.T) {
		failed := solRevPathsPrefix("", 60, func(i int) string {
			return "A" + strings.Repeat("L", 240) + fmt.Sprintf("%03d", i) + ".kt"
		})
		solRevRunCase(t, solRevCase{
			name:      "shared_suffix",
			failed:    failed,
			untouched: []string{"B" + strings.Repeat("L", 240) + "000.kt"},
			top:       syscall.EIO,
			minRows:   1, rows: 30, failedRows: 30, attempted: 61, failedTotal: 60,
		})
	})
	t.Run("json_over_budget_filename_is_abbreviated", func(t *testing.T) {
		solRevRunCase(t, solRevCase{
			name:    "json_over_budget",
			minRows: 2, rows: 2, failedRows: 2, attempted: 2, failedTotal: 2,
			top: syscall.EIO,
			failed: []string{
				"d/" + strings.Repeat("\x01", 1400) + "_1.kt",
				"d/" + strings.Repeat("\x01", 1400) + "_2.kt",
			},
		})
	})
}

// TestSOLREV01ShortBaseForms calls shortBase directly. Each case names a target filename first and
// a batch of display paths after it; the abbreviation must name only the target under all readings.
func TestSOLREV01ShortBaseForms(t *testing.T) {
	L := func(c string, k int) string { return strings.Repeat(c, k) }
	whole := L("L", 20) + L("M", 20) + ".kt"
	wholeBatch := []string{"d/" + whole}
	for i := 1; i < len(whole); i++ {
		wholeBatch = append(wholeBatch, "d/"+whole[:i]+"z"+whole[i:])
	}
	for _, tc := range []struct {
		name  string
		batch []string
		want  string
	}{
		{"i_whole_name_kept_when_nothing_cheaper_is_unique", wholeBatch, whole},
		{"h_prime_directory_named_like_the_window_target", []string{"d/" + L("a", 100) + "X" + L("b", 100) + ".kt", "d/" + L("a", 100) + "Y" + L("b", 100) + ".kt", "f/" + L("a", 100) + "X" + L("b", 100) + ".kt/g.kt"}, "…X" + L("b", 100) + ".kt"},
		{"a_short_name_unchanged", []string{"d/short.kt", "d/other.kt"}, "short.kt"},
		{"b_end_distinguished_suffix", []string{"d/" + L("p", 60) + "1.kt", "d/" + L("p", 60) + "2.kt"}, "…" + L("p", 12) + "1.kt"},
		{"c_start_distinguished_prefix", []string{"d/1" + L("p", 60) + ".kt", "d/2" + L("p", 60) + ".kt"}, "1" + L("p", 12) + "….kt"},
		{"d_both_ends_joint", []string{"d/A" + L("L", 240) + "000.kt", "d/A" + L("L", 240) + "001.kt", "d/B" + L("L", 240) + "000.kt"}, "A…" + L("L", 9) + "000.kt"},
		{"e_middle_window", []string{"d/" + L("a", 100) + "X" + L("b", 100) + ".kt", "d/" + L("a", 100) + "Y" + L("b", 100) + ".kt"}, "…" + L("a", 8) + "X" + L("b", 7) + "….kt"},
		{"f_multibyte_cut_points", []string{"d/" + L("界", 60) + "A" + L("界", 60) + ".kt", "d/" + L("界", 60) + "B" + L("界", 60) + ".kt"}, "…" + L("界", 3) + "A" + L("界", 2) + "….kt"},
		{"g_directory_shares_long_prefix", []string{"d/000" + L("L", 200) + ".kt", "d/000" + L("L", 200) + "9.kt", "e/000" + L("L", 20) + "_dir/other.kt"}, "…" + L("L", 13) + ".kt"},
		{"h_directory_named_like_the_file", []string{"d/" + L("p", 60) + "1.kt", "e/" + L("p", 60) + "2.kt", "f/" + L("p", 60) + "1.kt/g.kt"}, "…" + L("p", 12) + "1.kt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set := solRevSet(t)
			idx := newFailurePathIndex()
			for _, raw := range tc.batch {
				idx.add(raw, set.displayPaths())
			}
			target := slashBase(tc.batch[0])
			got := idx.shortBase(target)
			if got != tc.want {
				t.Fatalf("shortBase(%q) = %q, want %q", target, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("shortBase(%q) = %q is not valid UTF-8", target, got)
			}
			var names []string
			seen := map[string]bool{}
			for _, raw := range tc.batch {
				if b := slashBase(raw); !seen[b] {
					seen[b] = true
					names = append(names, b)
				}
			}
			for _, reading := range []int{solRevLenient, solRevStrict, solRevLoose} {
				if matches := solRevMatches(got, names, reading); len(matches) != 1 || matches[0] != target {
					t.Fatalf("reading %d: %q resolves to %v, want only %q", reading, got, matches, target)
				}
			}
			// The label a renderer emits names the target among the full paths of the batch. A
			// whole-name result is literal, so it resolves under the lenient component reading.
			emitted, reading := pathEllipsis+"/"+got, solRevLoose
			if got == target {
				reading = solRevLenient
			}
			if matches := solRevMatches(emitted, tc.batch, reading); len(matches) != 1 || matches[0] != tc.batch[0] {
				t.Fatalf("emitted %q resolves to %v under reading %d, want only %q", emitted, matches, reading, tc.batch[0])
			}
		})
	}
}
