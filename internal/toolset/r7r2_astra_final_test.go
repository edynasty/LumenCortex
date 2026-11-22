package toolset

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/edynasty/LumenCortex/internal/editing"
)

// astraStates cycle over the failed rows, as the other failure fixtures do.
var astraStates = []editing.PublishState{
	editing.PublishExternalModification, editing.PublishPartial, editing.PublishSucceededWithPostError,
	editing.PublishUncertain, editing.PublishRollbackFailed,
}

// astraItem is one row in the priority order the renderer must keep: failed rows, then verified rows,
// then untouched rows. raw is the workspace path that reaches the renderer; disp is the display path a
// reader must resolve the shown label to. A resultOnly item is an attempted path that appears only in
// the result, never in the receipt.
type astraItem struct {
	raw, disp  string
	state      editing.PublishState
	verified   bool
	resultOnly bool
	newRev     string
	newSHA     string
}

func astraFailed(paths []string, stateOf func(i int) editing.PublishState) []astraItem {
	items := make([]astraItem, len(paths))
	for i, p := range paths {
		items[i] = astraItem{raw: p, disp: p, state: stateOf(i)}
	}
	return items
}

func astraCycle(i int) editing.PublishState { return astraStates[i%len(astraStates)] }

// astraReceiptRows returns the items that have a receipt row.
func astraReceiptRows(items []astraItem) []astraItem {
	var rows []astraItem
	for _, it := range items {
		if !it.resultOnly {
			rows = append(rows, it)
		}
	}
	return rows
}

// astraRun renders items through the public commit outcome and parses the envelope.
func astraRun(t *testing.T, set *Set, items []astraItem, top error) (string, r7r2Payload) {
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
	return out.Content, r7r2ParseAndCheckBudget(t, "astra", out.Content, out.OK)
}

// astraCount returns n for a whole "…{n}" component with a canonical decimal count from 1 to 9999.
func astraCount(c string) int {
	inner, ok := strings.CutPrefix(c, pathEllipsis+"{")
	if !ok {
		return 0
	}
	inner, ok = strings.CutSuffix(inner, "}")
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(inner)
	if err != nil || n < 1 || n > 9999 || strconv.Itoa(n) != inner {
		return 0
	}
	return n
}

// astraMatch is an independent resolver written against the grammar, not the production index. A
// redacted "<outside-workspace>/" label is literal. A "./" label is exactly one sanitized path, and
// "…/./tail" is any path with at least one directory before the literal tail. The counted form
// "root/…{n}/[parent/]base" is checked by splitting the path, so the count is never a regular
// expression repetition. Otherwise a whole-component "…" is one or more directories (component
// reading) or any text (loose reading), and a "…" inside a component is one or more bytes (component
// reading) or any text (loose reading).
func astraMatch(label, path string, loose bool) bool {
	if strings.HasPrefix(label, "<outside-workspace>/") {
		return path == label
	}
	if strings.HasPrefix(label, "./") {
		return path == label[2:]
	}
	if tail, ok := strings.CutPrefix(label, pathEllipsis+"/./"); ok {
		return strings.HasSuffix(path, "/"+tail) && len(path) > len(tail)+1
	}
	comps := strings.Split(label, "/")
	if len(comps) >= 3 && astraCount(comps[1]) > 0 {
		n := astraCount(comps[1])
		pathComps := strings.Split(path, "/")
		tail := strings.Join(comps[2:], "/")
		return pathComps[0] == comps[0] && len(pathComps) == 1+n+len(comps)-2 && strings.Join(pathComps[1+n:], "/") == tail
	}
	var b strings.Builder
	b.WriteString("(?s)^")
	for i, c := range comps {
		last := i == len(comps)-1
		if !last && c == pathEllipsis {
			if loose {
				b.WriteString(`.+/`)
			} else {
				b.WriteString(`(?:[^/]+/)+`)
			}
			continue
		}
		for j, part := range strings.Split(c, pathEllipsis) {
			if j > 0 {
				if loose {
					b.WriteString(`.+`)
				} else {
					b.WriteString(`[^/]+`)
				}
			}
			b.WriteString(regexp.QuoteMeta(part))
		}
		if !last {
			b.WriteString("/")
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String()).MatchString(path)
}

// astraResolves returns the attempted paths a label names under one reading.
func astraResolves(label string, attempted []string, loose bool) []string {
	var matches []string
	for _, a := range attempted {
		if a == label || astraMatch(label, a, loose) {
			matches = append(matches, a)
		}
	}
	return matches
}

// astraCheck asserts the invariants shared by every scenario in this file: the envelope classifies
// the failure, every shown label resolves to exactly its row under both readings, states and flags
// match, no unverified row carries a revision or hash, and the omissions and published rows count
// exactly. Result-only paths take part in resolution but have no receipt row.
func astraCheck(t *testing.T, all []astraItem, content string, p r7r2Payload, top error, wantRows int) {
	t.Helper()
	var envelope struct {
		Files []toolFileState `json:"files"`
	}
	if err := json.Unmarshal([]byte(content), &envelope); err != nil {
		t.Fatal(err)
	}
	if top != nil && p.Error != top.Error() {
		t.Fatalf("error = %q, want %q", p.Error, top.Error())
	}
	if !p.DirtyFailure || !p.PartialPublication || p.CommitState != string(editing.CommitStatePartiallyPublished) || p.Code != "" {
		t.Fatalf("classification dirty=%v partial=%v commitState=%q code=%q", p.DirtyFailure, p.PartialPublication, p.CommitState, p.Code)
	}
	attempted := make([]string, 0, len(all))
	for _, it := range all {
		attempted = append(attempted, it.disp)
	}
	items := astraReceiptRows(all)
	shown := len(p.Files)
	if shown != wantRows {
		t.Fatalf("shown %d rows, want %d: %s", shown, wantRows, content)
	}
	for i, row := range p.Files {
		want := items[i]
		for _, loose := range []bool{false, true} {
			if got := astraResolves(row.Path, attempted, loose); len(got) != 1 || got[0] != want.disp {
				t.Fatalf("row %d label %q resolves to %v (loose=%v), want only %q", i, row.Path, got, loose, want.disp)
			}
		}
		if row.PublishState != string(want.state) || row.Verified != want.verified {
			t.Fatalf("row %d = %+v, want state %s verified %v", i, row, want.state, want.verified)
		}
		published := want.state != editing.PublishNotAttempted
		if row.Published != published || row.Wrote != published {
			t.Fatalf("row %d published=%v wrote=%v, want %v", i, row.Published, row.Wrote, published)
		}
		raw := envelope.Files[i]
		if !want.verified {
			if raw.NewRev != "" || raw.SHA256 != "" {
				t.Fatalf("unverified row %d carries a revision or hash: %+v", i, raw)
			}
			continue
		}
		if raw.NewRev != want.newRev || raw.SHA256 != want.newSHA {
			t.Fatalf("verified row %d rev=%q sha=%q, want rev=%q sha=%q", i, raw.NewRev, raw.SHA256, want.newRev, want.newSHA)
		}
	}
	omitted := map[string]int{}
	for _, it := range items[shown:] {
		omitted[string(it.state)]++
	}
	if p.FilesOmitted != len(items)-shown || !reflect.DeepEqual(emptyAsNil(p.FilesOmittedByState), emptyAsNil(omitted)) {
		t.Fatalf("omitted=%d byState=%v, want %d and %v", p.FilesOmitted, p.FilesOmittedByState, len(items)-shown, omitted)
	}
	// publishedFiles lists the published rows in receipt order, each shown label naming its own path.
	var published []astraItem
	for _, it := range items {
		if it.state != editing.PublishNotAttempted {
			published = append(published, it)
		}
	}
	var pub struct {
		PublishedFiles        []editing.FileResult `json:"publishedFiles"`
		PublishedFilesOmitted int                  `json:"publishedFilesOmitted"`
	}
	if err := json.Unmarshal([]byte(content), &pub); err != nil {
		t.Fatal(err)
	}
	if len(pub.PublishedFiles)+pub.PublishedFilesOmitted != len(published) {
		t.Fatalf("publishedFiles %d plus omitted %d, want %d published rows", len(pub.PublishedFiles), pub.PublishedFilesOmitted, len(published))
	}
	for i, row := range pub.PublishedFiles {
		for _, loose := range []bool{false, true} {
			if got := astraResolves(row.Path, attempted, loose); len(got) != 1 || got[0] != published[i].disp {
				t.Fatalf("published row %d label %q resolves to %v (loose=%v), want only %q", i, row.Path, got, loose, published[i].disp)
			}
		}
	}
	t.Logf("shown=%d omitted=%d byState=%v published=%d publishedOmitted=%d bytes=%d", shown, p.FilesOmitted, p.FilesOmittedByState, len(pub.PublishedFiles), pub.PublishedFilesOmitted, len(content))
}

// astraOrders returns the permutations of 0..n-1 that the ordering scenario applies to whole pairs.
func astraOrders(n int) map[string][]int {
	identity := make([]int, n)
	for i := range identity {
		identity[i] = i
	}
	orders := map[string][]int{"original": identity}
	reverse := make([]int, n)
	for i := range reverse {
		reverse[i] = n - 1 - i
	}
	orders["reverse"] = reverse
	rotate := make([]int, n)
	for i := range rotate {
		rotate[i] = (i + 1) % n
	}
	orders["rotate1"] = rotate
	byShape := append([]int(nil), identity...)
	sort.SliceStable(byShape, func(i, j int) bool { return byShape[i]%4 < byShape[j]%4 })
	orders["by_shape"] = byShape
	for _, seed := range []int64{7, 31} {
		shuffled := append([]int(nil), identity...)
		rand.New(rand.NewSource(seed)).Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		orders[fmt.Sprintf("seed%d", seed)] = shuffled
	}
	return orders
}

// astraArchived40 is the archived 40-file batch: ten roots, four directory shapes each, and a filler of
// six 170-byte directories.
func astraArchived40() []string {
	fill := strings.Repeat(strings.Repeat("m", 170)+"/", 6)
	var paths []string
	for i := 0; i < 10; i++ {
		r := fmt.Sprintf("r%02d", i)
		paths = append(paths,
			r+"/"+fill+"common/f.kt",
			r+"/"+fill+"common/deep/"+fill+"common/f.kt",
			r+"/x/"+fill+"common/f.kt",
			r+"/common/"+fill+"deep/f.kt")
	}
	return paths
}

func astraSet(t *testing.T) *Set {
	t.Helper()
	set, err := New(Options{Workspace: t.TempDir(), Policy: PolicyWorkspace, SessionID: "s-astra-final"})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// astraLiteralSet creates real files so literal paths are sanitized as on-disk paths. The workspace
// always exists, because the set needs it even when nothing is written.
func astraLiteralSet(t *testing.T, paths []string) *Set {
	t.Helper()
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		full := filepath.Join(ws, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	set, err := New(Options{Workspace: ws, Policy: PolicyWorkspace, SessionID: "s-astra-literal"})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// TestAstraDepthTailArchived40 renders the archived 40-file batch in six orders. Each order keeps every
// file's state with its path, and each shown label names exactly its own file.
func TestAstraDepthTailArchived40(t *testing.T) {
	base := astraFailed(astraArchived40(), astraCycle)
	for name, order := range astraOrders(len(base)) {
		items := make([]astraItem, len(order))
		for i, src := range order {
			items[i] = base[src]
		}
		t.Run(name, func(t *testing.T) {
			set := astraSet(t)
			content, p := astraRun(t, set, items, syscall.EIO)
			if len(p.Files) < 30 {
				t.Fatalf("%d failed rows shown, want at least 30", len(p.Files))
			}
			astraCheck(t, items, content, p, syscall.EIO, 31)
		})
	}
}

// TestAstraDepthTailNeverMasquerades adds a result-only path that spells the counted gap of the first
// file. That literal name is attempted, so the first file must not be shown by it, and every shown
// label must still name exactly one attempted path.
func TestAstraDepthTailNeverMasquerades(t *testing.T) {
	items := astraFailed(astraArchived40(), astraCycle)
	items = append(items, astraItem{raw: "r00/…{7}/f.kt", disp: "r00/…{7}/f.kt", state: editing.PublishNotAttempted, resultOnly: true})
	set := astraSet(t)
	content, p := astraRun(t, set, items, syscall.EIO)
	if len(p.Files) == 0 || p.Files[0].Path == "r00/…{7}/f.kt" {
		t.Fatalf("first row is the result-only literal: %s", content)
	}
	astraCheck(t, items, content, p, syscall.EIO, 30)
}

// TestAstraLiteralEllipsisOnDisk renders real literal paths: single and multiple literal ellipses,
// literal filenames, percent and JSON-sensitive names. Every label must resolve to exactly its own path
// under both readings, and literal names must never resolve as shorthand.
func TestAstraLiteralEllipsisOnDisk(t *testing.T) {
	literals := []string{
		"a/…/f.kt", "a/b/c/f.kt", "a/real…tail.kt", "a/realXYZtail.kt", "a/…/nest…ed/t…t",
		"a/dir/nestQRed/tZt", "a/%E2%80%A6/f.kt", "a/…{2}/f.kt", "a/…{2}/real…tail.kt",
		"tree/…/…/…", "tree/alpha/beta/filename", "tree/lit…eral/na…me.kt", "tree/litXeral/naYme.kt",
		"tree/…{6}/quote&.kt", "tree/ümlaut /quote&.kt",
	}
	set := astraLiteralSet(t, literals)
	items := astraFailed(literals, astraCycle)
	content, p := astraRun(t, set, items, syscall.EIO)
	astraCheck(t, items, content, p, syscall.EIO, 15)
	for _, row := range p.Files {
		if strings.Contains(row.Path, pathEllipsis) && !strings.HasPrefix(row.Path, "./") && !strings.HasPrefix(row.Path, pathEllipsis+"/./") {
			t.Fatalf("literal label %q is neither exact nor a literal-tail form", row.Path)
		}
	}
}

// TestAstraSyntheticQuoteNames covers names with a double quote without creating them on disk, which
// Windows cannot represent.
func TestAstraSyntheticQuoteNames(t *testing.T) {
	names := []string{`tree/quote"&/f.kt`, `tree/…{6}/quote"&.kt`}
	set := astraSet(t)
	items := astraFailed(names, astraCycle)
	content, p := astraRun(t, set, items, syscall.EIO)
	astraCheck(t, items, content, p, syscall.EIO, len(names))
}

// TestAstraLiteralOmittedComparisons: literal rows compete with omitted verified and unattempted
// literal rows, and the shown literal row must not resolve to an omitted one.
func TestAstraLiteralOmittedComparisons(t *testing.T) {
	literals := []string{"a/…/f.kt", "a/b/c/f.kt", "a/%E2%80%A6/f.kt"}
	var padding []string
	for i := 0; i < 50; i++ {
		padding = append(padding, fmt.Sprintf("x/%s%03d.kt", strings.Repeat("s", 40), i))
	}
	set := astraLiteralSet(t, literals)
	items := append(astraFailed(append([]string{literals[0]}, padding...), astraCycle), astraItem{
		raw: literals[1], disp: literals[1], state: editing.PublishSucceededAndVerified, verified: true,
		newRev: "rev00000", newSHA: "sha256:" + strings.Repeat("ab", 32),
	}, astraItem{raw: literals[2], disp: literals[2], state: editing.PublishNotAttempted})
	content, p := astraRun(t, set, items, syscall.EIO)
	astraCheck(t, items, content, p, syscall.EIO, 29)
}

// TestAstraStandaloneRefusalNames: a refusal naming a literal path with no receipt rows is named by its
// exact label. The redacted outside marker stays unqualified, including when the basename is literal.
func TestAstraStandaloneRefusalNames(t *testing.T) {
	literal := "a/…/f.kt"
	set := astraLiteralSet(t, []string{literal})
	out, goErr := set.editOutcome(&editing.Error{Code: editing.CodeStale, Path: literal, Detail: "base revision is not current", CurrentRev: "r1"})
	if goErr != nil {
		t.Fatalf("unexpected go error: %v", goErr)
	}
	p := r7r2ParseAndCheckBudget(t, "standalone", out.Content, out.OK)
	if p.Path != "./"+literal || p.Code != editing.CodeStale {
		t.Fatalf("path=%q code=%q, want %q and %q", p.Path, p.Code, "./"+literal, editing.CodeStale)
	}
	redacted := astraSet(t)
	for _, raw := range []string{"../outside/key.txt", "../outside/key…file.kt"} {
		out, _ = redacted.editOutcome(&editing.Error{Code: editing.CodeStale, Path: raw, Detail: "base revision is not current", CurrentRev: "r1"})
		p = r7r2ParseAndCheckBudget(t, "redacted", out.Content, out.OK)
		want := "<outside-workspace>/" + strings.TrimPrefix(raw, "../outside/")
		if p.Path != want || strings.Contains(p.Path, "./") {
			t.Fatalf("redacted path=%q, want the unqualified marker %q", p.Path, want)
		}
	}
}

// TestAstraSharedDirectoryNeverHidesSuccessors is the blocked-row case. The first failed path can
// never fit with the mandatory envelope, so it is skipped and the independently identifiable good.kt
// must keep its original state and error, with omissions counted exactly.
func TestAstraSharedDirectoryNeverHidesSuccessors(t *testing.T) {
	tail := "/" + strings.Repeat(strings.Repeat("&", 162)+"/", 4) + "f.kt"
	blocked := "a…a" + tail
	untouched := "b…b" + tail
	set := astraLiteralSet(t, []string{blocked, "good.kt", untouched})
	items := []astraItem{
		{raw: blocked, disp: blocked, state: editing.PublishExternalModification},
		{raw: "good.kt", disp: "good.kt", state: editing.PublishPartial},
		{raw: untouched, disp: untouched, state: editing.PublishNotAttempted},
	}
	content, p := astraRun(t, set, items, syscall.EIO)
	if len(p.Files) == 0 || p.Files[0].Path != "good.kt" || p.Files[0].PublishState != string(editing.PublishPartial) {
		t.Fatalf("good.kt is not the first shown row: %s", content)
	}
	var env struct {
		Files []toolFileState `json:"files"`
	}
	if err := json.Unmarshal([]byte(content), &env); err != nil {
		t.Fatal(err)
	}
	if env.Files[0].Error != "input/output error" {
		t.Fatalf("good.kt lost its row error: %+v", env.Files[0])
	}
	shownItems := append([]astraItem{items[1]}, items[0], items[2])
	astraCheck(t, shownItems, content, p, syscall.EIO, 1)
}

// TestAstraExpensiveLiteralDirectoryKeepsNormalBasename: a unique expensive literal directory can
// shorten its prefix, so both the literal file and the normal basename fit.
func TestAstraExpensiveLiteralDirectoryKeepsNormalBasename(t *testing.T) {
	large := "lit/…/" + strings.Repeat(strings.Repeat("&", 162)+"/", 4) + "item.kt"
	set := astraLiteralSet(t, []string{large, "good.kt"})
	items := []astraItem{
		{raw: large, disp: large, state: editing.PublishExternalModification},
		{raw: "good.kt", disp: "good.kt", state: editing.PublishPartial},
	}
	content, p := astraRun(t, set, items, syscall.EIO)
	astraCheck(t, items, content, p, syscall.EIO, 2)
}

// TestAstraLiteralTailExemptionRedactedSegment: the literal tail "<outside-workspace>/…/f.kt" inside a
// long ordinary path is a suffix of a workspace path, so it must be qualified. Its compact form must
// name only the target, and the rendered label is the exact tail.
func TestAstraLiteralTailExemptionRedactedSegment(t *testing.T) {
	filler := strings.Repeat(strings.Repeat("r", 200)+"/", 6)
	target := "rootlong/" + filler + "<outside-workspace>/…/f.kt"
	set := astraSet(t)
	items := []astraItem{
		{raw: target, disp: target, state: editing.PublishExternalModification},
		{raw: "b/…/f.kt", disp: "b/…/f.kt", state: editing.PublishPartial},
		{raw: "c/<outside-workspace>/ordinary/f.kt", disp: "c/<outside-workspace>/ordinary/f.kt", state: editing.PublishNotAttempted, resultOnly: true},
	}
	content, p := astraRun(t, set, items, syscall.EIO)
	if len(p.Files) == 0 || p.Files[0].Path != "…/./<outside-workspace>/…/f.kt" {
		t.Fatalf("target label = %v, want the exact literal tail: %s", p.Files, content)
	}
	astraCheck(t, items, content, p, syscall.EIO, 2)
}

// TestAstraEscapedJointBatch: 32 failed targets share escaped prefixes with 96 unattempted
// comparators. The renderer must keep at least 29 failed rows, where the joint choice had kept 20.
func TestAstraEscapedJointBatch(t *testing.T) {
	var targets, comparators []string
	for i := 0; i < 32; i++ {
		root := fmt.Sprintf("%02d", i)
		pre := root + "A" + strings.Repeat("a", 70)
		target := pre + strings.Repeat("&", 16) + "0.kt"
		targets = append(targets, "pkg/"+target)
		comparators = append(comparators,
			"pkg/"+pre+strings.Repeat("&", 16)+"1.kt",
			"pkg/B"+target,
			"pkg/"+target[:12]+"zOTHER.kt")
	}
	set := astraSet(t)
	items := astraFailed(targets, astraCycle)
	for _, c := range comparators {
		items = append(items, astraItem{raw: c, disp: c, state: editing.PublishNotAttempted})
	}
	content, p := astraRun(t, set, items, syscall.EIO)
	if len(p.Files) < 29 {
		t.Fatalf("%d failed rows shown, want at least 29: %s", len(p.Files), content)
	}
	astraCheck(t, items, content, p, syscall.EIO, 29)
}

// astraOmittedRootSuffixItems is the J3 shape: a long-root first failure, sixty fillers, and the real
// root file common/f.kt. Its suffix "…/common/f.kt" would also name the root file if the wildcard may
// name zero components, so the first label must not be that suffix.
func astraOmittedRootSuffixItems(first string) []astraItem {
	var paths []string
	paths = append(paths, first)
	for k := 0; k < 60; k++ {
		paths = append(paths, fmt.Sprintf("filler/%s%02d.kt", strings.Repeat("q", 30), k))
	}
	paths = append(paths, "common/f.kt")
	return astraFailed(paths, astraCycle)
}

// TestAstraOmittedRootSuffix renders the exact J3 shape and a long-root variant (which uses a positive
// byte budget). The first emitted label must not be the zero-gap suffix that names the omitted root file.
func TestAstraOmittedRootSuffix(t *testing.T) {
	const zeroGap = "…/common/f.kt"
	shapes := map[string]string{
		"exact_j3":  strings.Repeat("z", 60) + "/common/f.kt",
		"long_root": strings.Repeat(strings.Repeat("z", 200)+"/", 6) + "common/f.kt",
	}
	for name, first := range shapes {
		t.Run(name, func(t *testing.T) {
			items := astraOmittedRootSuffixItems(first)
			set := astraSet(t)
			content, p := astraRun(t, set, items, syscall.EIO)
			if len(p.Files) == 0 {
				t.Fatalf("no failed row shown: %s", content)
			}
			if p.Files[0].Path == zeroGap {
				t.Fatalf("first label %q names the omitted root file: %s", p.Files[0].Path, content)
			}
			astraCheck(t, items, content, p, syscall.EIO, 29)
		})
	}
}
