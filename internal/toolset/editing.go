package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/internal/editing"
	"github.com/edynasty/LumenCortex/protocol"
)

// The file tools commit every write through one editing engine (internal/editing). This file binds the
// toolset to it: Resolve, Open and Event are the engine's file layer, so the engine works under exactly
// the path guard, protected-state check and event bus the tools already have.

// Resolve confines a path to the workspace and refuses protected state (ADR-0016).
func (s *Set) Resolve(input string, allowMissing bool) (string, string, error) {
	return s.resolve(input, allowMissing)
}

// Open opens a file for the engine's compare-and-swap read and write. It opens without truncating and
// refuses protected state on the descriptor, so a write the guard refuses leaves the file as it was.
// It also re-applies the workspace guard to the descriptor: a path component swapped for a symlink
// between Resolve and the open would otherwise let the descriptor (and the write that follows on it)
// reach a file outside the workspace, which the descriptor-level protected-state check alone does not
// stop.
func (s *Set) Open(path string, create bool) (*os.File, error) {
	if testHookBeforeOpen != nil {
		testHookBeforeOpen(path)
	}
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE | os.O_EXCL
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil {
		err = s.denyIfProtected(info)
	}
	if err == nil {
		err = s.verifyOpenedDescriptor(path, info)
	}
	if err != nil {
		if create {
			_ = editing.CleanupExclusivelyCreatedFile(path, f)
		} else {
			f.Close()
		}
		return nil, err
	}
	return f, nil
}

// OpenCreateTemp stages a newly created file in a private sibling temp file inside the target's
// directory under the exact same workspace and protected-state descriptor checks as Open, without
// touching targetPath (ADR-0024 §8, R2-01).
func (s *Set) OpenCreateTemp(targetPath string) (*os.File, string, error) {
	f, tmpPath, err := editing.NewCreateTempFile(filepath.Dir(targetPath))
	if err != nil {
		return nil, "", err
	}
	info, err := f.Stat()
	if err == nil {
		err = s.denyIfProtected(info)
	}
	if err == nil {
		err = s.verifyOpenedDescriptor(tmpPath, info)
	}
	if err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return nil, "", err
	}
	return f, tmpPath, nil
}

// BeforeAtomicPublish invokes testHookBeforeOpen on targetPath and re-verifies that targetPath
// still resolves within the workspace and does not name a protected file or escaped symlink.
func (s *Set) BeforeAtomicPublish(targetPath string) error {
	if testHookBeforeOpen != nil {
		testHookBeforeOpen(targetPath)
	}
	canonical, _, err := s.resolve(targetPath, true)
	if err != nil {
		return err
	}
	if linfo, lerr := os.Lstat(targetPath); lerr == nil {
		if err := s.denyIfProtected(linfo); err != nil {
			return err
		}
		if linfo.Mode()&os.ModeSymlink != 0 {
			if _, _, err := s.resolve(targetPath, false); err != nil {
				return err
			}
		}
	}
	if canonical != targetPath {
		if sinfo, serr := os.Stat(canonical); serr == nil {
			if err := s.denyIfProtected(sinfo); err != nil {
				return err
			}
		}
	}
	return nil
}

// verifyOpenedDescriptor re-runs the workspace guard on an opened descriptor. The path is resolved
// again and must still name the inode that was opened: a swap between the open and this check makes
// os.SameFile false, and a swap to a symlink out of the workspace makes resolve refuse. Editing a
// symlink whose target is inside the workspace stays allowed, as resolve validates its real target.
func (s *Set) verifyOpenedDescriptor(path string, info os.FileInfo) error {
	canonical, _, err := s.resolve(path, true)
	if err != nil {
		return err
	}
	current, err := os.Stat(canonical)
	if err != nil {
		return err
	}
	if !os.SameFile(info, current) {
		return fmt.Errorf("file identity changed after resolve: %s", path)
	}
	return nil
}

// Event publishes an edit event on the runtime's bus. A toolset built without a bus discards it.
func (s *Set) Event(kind, sessionID string, payload map[string]any) {
	if s.events != nil {
		s.events(kind, sessionID, payload)
	}
}

// actor is who a read or a write belongs to: the run's session. A toolset without a session (the
// capability catalog, a tool-level test) uses one local actor so its snapshots stay consistent.
func (s *Set) actor() editing.Actor {
	if s.sessionID == "" {
		return editing.Actor{Session: "local"}
	}
	return editing.Actor{Session: s.sessionID}
}

// editingEngine is the engine every write goes through.
func (s *Set) editingEngine() *editing.Engine { return s.edit }

// goValidators are the validators a Go edit runs before it writes: syntax always, types when the
// package's dependencies can be imported from source.
func (s *Set) goValidators() []editing.Validator {
	return []editing.Validator{editing.GoSyntax(), editing.GoTypes()}
}

// commitPlan runs Engine.Commit while capturing its CommitReceipt and forwarding it to any host-bound
// execution recorder on ctx.
func (s *Set) commitPlan(ctx context.Context, plan editing.Plan) (editing.Result, editing.CommitReceipt, error) {
	var got editing.CommitReceipt
	wrapped := editing.WithCommitRecorder(ctx, func(r editing.CommitReceipt) {
		got = r
		editing.RecordCommitReceipt(ctx, r)
	})
	res, err := s.edit.Commit(wrapped, plan)
	return res, got, err
}

func receiptWrote(r editing.CommitReceipt) bool {
	for _, f := range r.Files {
		if f.Wrote {
			return true
		}
	}
	return false
}

const (
	maxPartialFilesInToolResult = 32
	maxDiagnosticsInToolResult  = 16
	maxErrorBytesInToolResult   = 1024
	maxFileErrorBytesInResult   = 256
	maxDiagTextBytesInResult    = 256
	maxPathBytesInToolResult    = 256
	// maxFailedPathBytesInResult is the rich render's preferred path budget for a failed row, not a
	// hard identity limit: a longer path is still shown by a compact label that names the file
	// (ADR-0024 R7R2-Results-01).
	maxFailedPathBytesInResult = 1024
	maxPartialPayloadBytes     = 4096
	// maxCandidateFileRows is an upper bound on the file rows the overflow envelope can hold, using
	// the shortest possible JSON row before any metadata.
	maxCandidateFileRows = maxPartialPayloadBytes / len(`{"path":"","publishState":"","published":true,"verified":true,"wrote":true}`)
)

type toolFileState struct {
	Path         string               `json:"path"`
	PublishState editing.PublishState `json:"publishState"`
	Published    bool                 `json:"published"`
	Verified     bool                 `json:"verified"`
	Wrote        bool                 `json:"wrote"`
	RolledBack   bool                 `json:"rolledBack,omitempty"`
	Created      bool                 `json:"created,omitempty"`
	Deleted      bool                 `json:"deleted,omitempty"`
	NewRev       string               `json:"newRev,omitempty"`
	SHA256       string               `json:"sha256,omitempty"`
	Error        string               `json:"error,omitempty"`
}

// receiptDirtyOrPublished reports whether the commit really entered a destination mutation or
// staging window. A clean external conflict that LC never wrote is not dirty; a historical
// rolled-back or uncertain state, staging debris and a published/post-error effect are.
func receiptDirtyOrPublished(r editing.CommitReceipt) bool {
	if r.DirtyFailure {
		return true
	}
	for _, f := range r.Files {
		if f.Published || f.Wrote ||
			f.PublishState == editing.PublishUncertain ||
			f.PublishState == editing.PublishRolledBack ||
			f.PublishState == editing.PublishRollbackFailed ||
			f.PublishState == editing.PublishPartial {
			return true
		}
	}
	return false
}

// receiptFileRank orders the bounded display: the failed path first, then the verified published
// prefix, then every untouched or cleanly refused path. Truncation keeps the rows that tell the
// agent what actually happened.
func receiptFileRank(f editing.FileCommitReceipt) int {
	switch {
	case f.Error != "", f.PublishState == editing.PublishPartial,
		f.PublishState == editing.PublishSucceededWithPostError,
		f.PublishState == editing.PublishExternalModification,
		f.PublishState == editing.PublishUncertain,
		f.PublishState == editing.PublishRollbackFailed,
		f.PublishState == editing.PublishFailedBeforeWrite, f.PublishState == editing.PublishUnsupported:
		return 0
	case f.Verified && f.Published && f.PublishState == editing.PublishSucceededAndVerified:
		return 1
	default:
		return 2
	}
}

func prioritizedReceiptFiles(files []editing.FileCommitReceipt) []editing.FileCommitReceipt {
	if len(files) < 2 {
		return files
	}
	ordered := make([]editing.FileCommitReceipt, len(files))
	copy(ordered, files)
	sort.SliceStable(ordered, func(i, j int) bool {
		return receiptFileRank(ordered[i]) < receiptFileRank(ordered[j])
	})
	return ordered
}

// receiptFileState renders one receipt row from its sanitized error text and the display label the
// caller chose for its path; the caller decides how the path is shortened. hashes is false once the
// payload must drop optional per-row evidence, which the persisted receipt still holds.
func receiptFileState(f editing.FileCommitReceipt, label, cleanE string, byPath map[string]editing.FileResult, fErrBytes int, hashes bool) (toolFileState, bool) {
	truncE := truncateUTF8Bytes(cleanE, fErrBytes)
	truncated := truncE != cleanE
	state := toolFileState{
		Path:         label,
		PublishState: f.PublishState,
		Published:    f.Published,
		Verified:     f.Verified,
		Wrote:        f.Wrote,
		RolledBack:   f.RolledBack,
		Created:      f.Created,
		Deleted:      f.Deleted,
		Error:        truncE,
	}
	if hashes && f.Verified && f.NewSHA256 != "" {
		state.SHA256 = f.NewSHA256
	}
	if fr, ok := byPath[f.Path]; ok && hashes && f.Verified {
		state.NewRev = fr.NewRev
	}
	return state, truncated || (!hashes && f.Verified)
}

// truncatePathTail returns a slash path's component or UTF-8 suffix candidate within maxBytes. It is
// not an unconditional uniqueness guarantee; callers verify identity before showing it.
func truncatePathTail(p string, maxBytes int) string {
	if len(p) <= maxBytes {
		return p
	}
	if maxBytes <= len(pathEllipsis) {
		return ""
	}
	tail := p[len(p)-(maxBytes-len(pathEllipsis)):]
	if i := strings.IndexByte(tail, '/'); i >= 0 {
		return pathEllipsis + tail[i:]
	}
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return pathEllipsis + tail
}

const pathEllipsis = "…"

const (
	// labelContextBytes is how many visible bytes an abbreviated name keeps.
	labelContextBytes = 16
	// labelExtMaxBytes bounds the kept extension, dot included.
	labelExtMaxBytes = 8
	// labelScanMaxBytes bounds the component bytes a window check may scan.
	labelScanMaxBytes = 256 << 10
)

type failurePathLabel struct {
	compact      string
	sharedSuffix int
}

// Index the entire batch, including omitted rows. A shorthand must locate its target
// even when another path sharing its basename no longer appears in the result.
type failurePathIndex struct {
	full    map[string]string
	count   map[string]int
	groups  map[string][]string
	parents map[[2]string]map[string]string
	labels  map[string]failurePathLabel
	// names and ends are built by ensureNames: every distinct component sorted by bytes, and every
	// distinct filename sorted by bytes read backwards.
	names       []string
	ends        []string
	dirNames    map[string]bool
	nameBytes   int
	indexed     bool
	bases       map[string]string
	depthTails  map[string]map[depthTailKey]int
	pathSuffix  map[string]int
	braced      map[string]map[bracedTailKey]int
	ownerSuffix map[[2]string]map[string]int
	roots       map[string]map[string][]string
}

type bracedTailKey struct {
	root, tail string
	n          int
}

// bracedCount returns n when the component ends with a canonical "{n}" count from 1 to 9999.
func bracedCount(c string) int {
	if !strings.HasSuffix(c, "}") {
		return 0
	}
	open := strings.LastIndexByte(c, '{')
	if open < 0 {
		return 0
	}
	inner := c[open+1 : len(c)-1]
	n, err := strconv.Atoi(inner)
	if err != nil || n < 1 || n > 9999 || strconv.Itoa(n) != inner {
		return 0
	}
	return n
}

// bracedKeys records the braced component that sits just before a one- or two-component tail. A
// reader that takes "…" as elided characters and "{n}" as literal text would see that component as a
// counted gap, so the keys let a counted label be vetoed for any batch path.
func bracedKeys(full string) []bracedTailKey {
	parts := strings.Split(full, "/")
	var keys []bracedTailKey
	for t := 1; t <= 2; t++ {
		i := len(parts) - 1 - t
		if i < 1 {
			continue
		}
		if n := bracedCount(parts[i]); n > 0 {
			keys = append(keys, bracedTailKey{root: parts[0], tail: strings.Join(parts[len(parts)-t:], "/"), n: n})
		}
	}
	return keys
}

type depthTailKey struct {
	root, parent string
	depth        int
}

// exactFailureLabel returns the reversible label of a path. A literal path, one that contains
// U+2026, is qualified by "./" so that it means exactly that sanitized relative path, and it is never
// shorthand. Unqualified "…" is shorthand: "…/" elides one or more directories, "…{n}" elides exactly
// n directories, and "…" inside a component elides bytes. "…/./tail" elides directories, then matches
// the remainder literally. Redacted paths are never qualified.
func exactFailureLabel(full string) string {
	if strings.Contains(full, pathEllipsis) && !strings.HasPrefix(full, "<outside-workspace>/") {
		return "./" + full
	}
	return full
}

// depthTail keys a path by its root and slash count, and returns the name of its parent directory.
func depthTail(full string) (depthTailKey, string) {
	root, _, _ := strings.Cut(full, "/")
	last := strings.LastIndexByte(full, '/')
	if last < 0 {
		return depthTailKey{}, ""
	}
	return depthTailKey{root: root, depth: strings.Count(full, "/")}, slashBase(full[:last])
}

// countedTails offers "root/…{n}/base" and "root/…{n}/parent/base" when exactly one path of the batch
// with that basename has the same root and depth, so the counted gap names only that file.
func (p *failurePathIndex) countedTails(full, base string, consider func(string)) {
	key, parent := depthTail(full)
	if key.depth < 2 || key.depth > 10000 {
		return
	}
	if p.depthTails == nil {
		p.depthTails = make(map[string]map[depthTailKey]int)
	}
	counts := p.depthTails[base]
	if counts == nil {
		counts = make(map[depthTailKey]int)
		for _, other := range p.groups[base] {
			k, tail := depthTail(other)
			if k.depth < 2 {
				continue
			}
			counts[k]++
			k.parent = tail
			counts[k]++
		}
		p.depthTails[base] = counts
		if p.braced == nil {
			p.braced = make(map[string]map[bracedTailKey]int)
		}
		braced := make(map[bracedTailKey]int)
		for _, other := range p.groups[base] {
			for _, bk := range bracedKeys(other) {
				braced[bk]++
			}
		}
		p.braced[base] = braced
	}
	braced := p.braced[base]
	self := bracedKeys(full)
	clash := func(root, tail string, n int) bool {
		bk := bracedTailKey{root: root, tail: tail, n: n}
		own := 0
		for _, sk := range self {
			if sk == bk {
				own++
			}
		}
		return braced[bk]-own > 0
	}
	emit := func(k depthTailKey, gap int, tail string) {
		if gap > 0 && counts[k] == 1 && !clash(k.root, tail, gap) {
			consider(k.root + "/" + pathEllipsis + "{" + strconv.Itoa(gap) + "}/" + tail)
		}
	}
	emit(key, key.depth-1, base)
	key.parent = parent
	emit(key, key.depth-2, parent+"/"+base)
}

func newFailurePathIndex() *failurePathIndex {
	return &failurePathIndex{
		full:    make(map[string]string),
		count:   make(map[string]int),
		groups:  make(map[string][]string),
		parents: make(map[[2]string]map[string]string),
		labels:  make(map[string]failurePathLabel),
		bases:   make(map[string]string),
	}
}

func (p *failurePathIndex) add(raw string, display *editing.DisplayPaths) {
	if _, ok := p.full[raw]; ok {
		return
	}
	full := display.Path(raw)
	p.full[raw] = full
	if full != "" && utf8.ValidString(full) {
		p.count[full]++
		base := slashBase(full)
		p.groups[base] = append(p.groups[base], full)
	}
}

func slashBase(p string) string {
	return p[strings.LastIndexByte(p, '/')+1:]
}

func commonSuffixBytes(a, b string) int {
	i := 0
	for i < len(a) && i < len(b) && a[len(a)-1-i] == b[len(b)-1-i] {
		i++
	}
	return i
}

func commonPrefixLen(a, b string) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

func encodedLen(s string) int {
	encoded, _ := json.Marshal(s)
	return len(encoded)
}

// compareBackward orders strings by the bytes read from their end, then by length.
func compareBackward(a, b string) int {
	i, j := len(a)-1, len(b)-1
	for i >= 0 && j >= 0 {
		if a[i] != b[j] {
			if a[i] < b[j] {
				return -1
			}
			return 1
		}
		i--
		j--
	}
	switch {
	case i < 0 && j < 0:
		return 0
	case i < 0:
		return -1
	default:
		return 1
	}
}

// ensureNames indexes the components and filenames of the batch. It runs after every path is added.
func (p *failurePathIndex) ensureNames() {
	if p.indexed {
		return
	}
	p.indexed = true
	p.dirNames = make(map[string]bool)
	for _, fulls := range p.groups {
		for _, full := range fulls {
			eachPathParent(full, func(start, end int) {
				p.dirNames[full[start:end]] = true
			})
		}
	}
	all := make(map[string]bool, len(p.groups)+len(p.dirNames))
	for base := range p.groups {
		all[base] = true
		p.ends = append(p.ends, base)
	}
	for dir := range p.dirNames {
		all[dir] = true
	}
	p.names = make([]string, 0, len(all))
	for name := range all {
		p.names = append(p.names, name)
		p.nameBytes += len(name)
	}
	sort.Strings(p.names)
	sort.Slice(p.ends, func(i, j int) bool { return compareBackward(p.ends[i], p.ends[j]) < 0 })
}

// uniquePrefix is the shortest prefix length that tells name apart from every other component.
func (p *failurePathIndex) uniquePrefix(name string) int {
	p.ensureNames()
	i := sort.SearchStrings(p.names, name)
	shared := 0
	if i > 0 {
		shared = max(shared, commonPrefixLen(name, p.names[i-1]))
	}
	if i+1 < len(p.names) {
		shared = max(shared, commonPrefixLen(name, p.names[i+1]))
	}
	return shared + 1
}

// uniqueSuffix is the shortest suffix length that tells filename name apart from every other filename.
func (p *failurePathIndex) uniqueSuffix(name string) int {
	p.ensureNames()
	i := sort.Search(len(p.ends), func(j int) bool { return compareBackward(p.ends[j], name) >= 0 })
	shared := 0
	if i > 0 {
		shared = max(shared, commonSuffixBytes(name, p.ends[i-1]))
	}
	if i+1 < len(p.ends) {
		shared = max(shared, commonSuffixBytes(name, p.ends[i+1]))
	}
	return shared + 1
}

// shortBase abbreviates one filename to the cheapest encoded form that still names it. A suffix is
// anchored at the end, so a filename-unique suffix suffices. A prefix or window may let "…" span a
// directory, so it must also be unique among directory names; otherwise a reader could find another file.
func (p *failurePathIndex) shortBase(name string) string {
	if len(name) <= 2*labelContextBytes {
		return name
	}
	if short, ok := p.bases[name]; ok {
		return short
	}
	p.ensureNames()
	best, bestCost := name, encodedLen(name)
	fullCost := bestCost
	try := func(candidate string) {
		if cost := encodedLen(candidate); cost < bestCost {
			best, bestCost = candidate, cost
		}
	}
	ext := ""
	if i := strings.LastIndexByte(name, '.'); i > 0 && len(name)-i <= labelExtMaxBytes {
		ext = name[i:]
	}
	n := max(p.uniqueSuffix(name), labelContextBytes)
	if start := len(name) - n; start > 0 {
		for start > 0 && !utf8.RuneStart(name[start]) {
			start--
		}
		if start > 0 {
			try(pathEllipsis + name[start:])
		}
	}
	if !p.dirNames[name] {
		end := max(p.uniquePrefix(name), labelContextBytes-len(ext))
		for end < len(name) && !utf8.RuneStart(name[end]) {
			end++
		}
		if end+len(ext) < len(name) {
			try(name[:end] + pathEllipsis + ext)
		}
		if bestCost > 2+len(pathEllipsis)+labelContextBytes && p.nameBytes <= labelScanMaxBytes {
			// Both ends together tell the name apart from every other filename. The prefix also starts
			// no directory name, so a reader who lets "…" span "/" still finds one file.
			type point struct{ a, b int }
			dirBound := 1
			for dir := range p.dirNames {
				dirBound = max(dirBound, 1+commonPrefixLen(name, dir))
			}
			points := make([]point, 0, len(p.ends))
			for _, other := range p.ends {
				if other != name {
					points = append(points, point{commonPrefixLen(name, other), commonSuffixBytes(name, other)})
				}
			}
			sort.Slice(points, func(i, j int) bool { return points[i].a > points[j].a })
			qs := []int{dirBound}
			for _, pt := range points {
				if pt.a+1 >= dirBound && pt.a+1 < len(name) {
					qs = append(qs, pt.a+1)
				}
			}
			if fullCost != 2+len(name) {
				for q := dirBound; q <= min(labelContextBytes, len(name)-1); q++ {
					qs = append(qs, q)
				}
			}
			sort.Sort(sort.Reverse(sort.IntSlice(qs)))
			// Splits are priced by the JSON fragments they keep, so escaped bytes count as they will be
			// written. Raw names keep the byte count. Breakpoints move monotonically down the sweep.
			fragmentCost := func(s string) int {
				if len(s) == 0 || fullCost == 2+len(name) {
					return len(s)
				}
				return encodedLen(s) - 2
			}
			bestQ, bestS := 0, 0
			idx, maxB := 0, 0
			prevQ, prevStart := len(name), len(name)
			jointCost, minCost := fullCost-2, 0
			for _, q := range qs {
				for idx < len(points) && points[idx].a >= q {
					maxB = max(maxB, points[idx].b)
					idx++
				}
				required := 0
				if idx > 0 {
					required = maxB + 1
				}
				s := max(required, len(ext), labelContextBytes-q, 1)
				start := len(name) - s
				for start > 0 && !utf8.RuneStart(name[start]) {
					start--
				}
				qq := q
				for qq < len(name) && !utf8.RuneStart(name[qq]) {
					qq++
				}
				if qq >= start {
					continue
				}
				jointCost -= fragmentCost(name[qq:prevQ])
				jointCost += fragmentCost(name[start:prevStart])
				prevQ, prevStart = qq, start
				if bestQ == 0 || jointCost < minCost || jointCost == minCost && qq < bestQ {
					bestQ, bestS, minCost = qq, len(name)-start, jointCost
				}
			}
			if bestQ > 0 {
				try(name[:bestQ] + pathEllipsis + name[len(name)-bestS:])
			}
		}
		if bestCost > 2+2*len(pathEllipsis)+labelContextBytes+len(ext) && p.nameBytes <= labelScanMaxBytes {
			d := p.uniquePrefix(name) - 1
			for _, half := range []int{labelContextBytes / 2, labelContextBytes} {
				from := max(1, d-half)
				if from >= len(name) {
					continue
				}
				for from > 0 && !utf8.RuneStart(name[from]) {
					from--
				}
				if from == 0 {
					continue
				}
				to := from + 2*half
				for to < len(name) && !utf8.RuneStart(name[to]) {
					to++
				}
				if d < from || d >= to || to+len(ext) >= len(name) {
					continue
				}
				window := name[from:to]
				shared := false
				for _, other := range p.names {
					if other != name && strings.Contains(other, window) {
						shared = true
						break
					}
				}
				if !shared {
					try(pathEllipsis + window + pathEllipsis + ext)
					break
				}
			}
		}
	}
	p.bases[name] = best
	return best
}

func (p *failurePathIndex) hasIdentity(raw string) bool {
	full := p.full[raw]
	// Don't identify two redacted paths as the same file or let JSON replace invalid filename bytes.
	return p.count[full] == 1
}

func eachPathParent(full string, visit func(start, end int)) {
	for start := 0; start < len(full); {
		n := strings.IndexByte(full[start:], '/')
		if n < 0 {
			return
		}
		visit(start, start+n)
		start += n + 1
	}
}

// rootGroup returns the paths of a basename group whose first component is root, or the whole group
// when root is empty. The root index is built once per basename.
func (p *failurePathIndex) rootGroup(base, root string) []string {
	if root == "" {
		return p.groups[base]
	}
	if p.roots == nil {
		p.roots = make(map[string]map[string][]string)
	}
	index, ok := p.roots[base]
	if !ok {
		index = make(map[string][]string)
		for _, full := range p.groups[base] {
			if first, _, found := strings.Cut(full, "/"); found {
				index[first] = append(index[first], full)
			}
		}
		p.roots[base] = index
	}
	return index[root]
}

// ownedSuffix is the longest common byte suffix between name and another owned name whose owner
// differs. In reverse-byte order the longest suffix shared with a subset is attained at the nearest
// subset member on either side, so each name needs only its nearest neighbours with another owner.
// The table is built once per key.
func (p *failurePathIndex) ownedSuffix(key [2]string, owners map[string]string, name string) int {
	table, ok := p.ownerSuffix[key]
	if !ok {
		names := make([]string, 0, len(owners))
		for k := range owners {
			names = append(names, k)
		}
		sort.Slice(names, func(i, j int) bool { return compareBackward(names[i], names[j]) < 0 })
		left, right := make([]int, len(names)), make([]int, len(names))
		for i := range names {
			left[i] = -1
			if i > 0 {
				if owners[names[i-1]] != owners[names[i]] {
					left[i] = i - 1
				} else {
					left[i] = left[i-1]
				}
			}
		}
		for i := len(names) - 1; i >= 0; i-- {
			right[i] = -1
			if i+1 < len(names) {
				if owners[names[i+1]] != owners[names[i]] {
					right[i] = i + 1
				} else {
					right[i] = right[i+1]
				}
			}
		}
		table = make(map[string]int, len(names))
		for i, k := range names {
			if owners[k] == "" {
				continue
			}
			v := 0
			if left[i] >= 0 {
				v = max(v, commonSuffixBytes(k, names[left[i]]))
			}
			if right[i] >= 0 {
				v = max(v, commonSuffixBytes(k, names[right[i]]))
			}
			table[k] = v
		}
		if p.ownerSuffix == nil {
			p.ownerSuffix = make(map[[2]string]map[string]int)
		}
		p.ownerSuffix[key] = table
	}
	return table[name]
}

func (p *failurePathIndex) parentOwners(base, root string) map[string]string {
	key := [2]string{base, root}
	owners, ok := p.parents[key]
	if ok {
		return owners
	}
	owners = make(map[string]string)
	for _, full := range p.rootGroup(base, root) {
		eachPathParent(full, func(start, end int) {
			name := full[start:end]
			if owner, ok := owners[name]; !ok {
				owners[name] = full
			} else if owner != full {
				owners[name] = ""
			}
		})
	}
	p.parents[key] = owners
	return owners
}

// sharedPathSuffix is the longest common byte suffix between full and another path of its basename
// group. For a large group only reverse-lexicographic neighbours can share the longest suffix, so the
// group is sorted once and each path's maximum is stored.
func (p *failurePathIndex) sharedPathSuffix(full, base string) int {
	group := p.groups[base]
	if len(group) < 2 {
		return 0
	}
	if v, ok := p.pathSuffix[full]; ok {
		return v
	}
	if len(group) <= maxCandidateFileRows {
		best := 0
		for _, other := range group {
			if full != other {
				best = max(best, commonSuffixBytes(full, other))
			}
		}
		return best
	}
	ordered := append([]string(nil), group...)
	sort.Slice(ordered, func(i, j int) bool { return compareBackward(ordered[i], ordered[j]) < 0 })
	if p.pathSuffix == nil {
		p.pathSuffix = make(map[string]int)
	}
	prev := 0
	for i, s := range ordered {
		next := 0
		if i+1 < len(ordered) {
			next = commonSuffixBytes(s, ordered[i+1])
		}
		p.pathSuffix[s] = max(prev, next)
		prev = next
	}
	return p.pathSuffix[full]
}

func (p *failurePathIndex) makeLabel(full string) failurePathLabel {
	best := failurePathLabel{compact: exactFailureLabel(full)}
	// The redaction prefix must not turn into display shorthand.
	if strings.HasPrefix(full, "<outside-workspace>/") {
		return best
	}
	base := slashBase(full)
	if strings.Contains(full, pathEllipsis) {
		// A literal path is never abbreviated by unqualified "…", only by its exact tail.
		best.sharedSuffix = p.sharedPathSuffix(full, base)
		if cut := strings.LastIndexByte(full[:len(full)-best.sharedSuffix], '/'); cut >= 0 {
			tail := full[cut+1:]
			tailLabel := tail
			if strings.Contains(tail, pathEllipsis) {
				tailLabel = "./" + tail
			}
			candidate := pathEllipsis + "/" + tailLabel
			if p.count[candidate] == 0 && p.count[pathEllipsis+"/"+tail] == 0 && p.count[tail] == 0 && encodedLen(candidate) < encodedLen(best.compact) {
				best.compact = candidate
			}
		}
		return best
	}
	bestCost := -1
	consider := func(candidate string) {
		// No shorthand may masquerade as a literal workspace path in the batch.
		if p.count[candidate] != 0 {
			return
		}
		// A leading wildcard may name zero components, so "…/x" also names the batch path "x".
		if tail, ok := strings.CutPrefix(candidate, pathEllipsis+"/"); ok && p.count[tail] != 0 {
			return
		}
		if bestCost < 0 {
			encoded, _ := json.Marshal(best.compact)
			bestCost = len(encoded)
		}
		encoded, _ := json.Marshal(candidate)
		if len(encoded) < bestCost {
			best.compact, bestCost = candidate, len(encoded)
		}
	}
	short := p.shortBase(base)
	group := p.groups[base]
	// Only a filename unique in the batch may be abbreviated: a repeated one would not name one file.
	if short != base && len(group) == 1 {
		consider(full[:len(full)-len(base)] + short)
		if base != full {
			consider(pathEllipsis + "/" + short)
		}
	}
	if base == full {
		return best
	}
	best.sharedSuffix = p.sharedPathSuffix(full, base)
	// The slash at the start of the suffix counts as a distinguishing byte too.
	if cut := strings.LastIndexByte(full[:len(full)-best.sharedSuffix], '/'); cut >= 0 {
		consider(pathEllipsis + full[cut:])
	}
	if len(group) == 1 {
		return best
	}
	// A unique parent component is sufficient even if there are many common components
	// between it and the filename. Mark, rather than pretend to preserve, those directories.
	considerParents := func(owners map[string]string, root string) {
		eachPathParent(full, func(start, end int) {
			name := full[start:end]
			if owners[name] != full || (root != "" && start == 0) {
				return
			}
			lead, rest := "", "/"+short
			if root != "" {
				lead = root + "/"
				if start != len(lead) {
					lead += pathEllipsis + "/"
				}
			} else if start > 0 {
				lead = pathEllipsis + "/"
			}
			if end+1 != len(full)-len(base) {
				rest = "/" + pathEllipsis + rest
			}
			consider(lead + name + rest)
			if len(name) <= 2*labelContextBytes {
				return
			}
			shared := p.ownedSuffix([2]string{base, root}, owners, name)
			if cut := len(name) - max(labelContextBytes, shared+1); cut > 0 {
				for cut > 0 && !utf8.RuneStart(name[cut]) {
					cut--
				}
				if cut > 0 {
					consider(lead + pathEllipsis + name[cut:] + rest)
				}
			}
		})
	}
	owners := p.parentOwners(base, "")
	considerParents(owners, "")
	// Keep the actual first directory as an anchor when neither of two useful components
	// is unique by itself (for example alpha/…/deep/…/f.kt).
	root, _, _ := strings.Cut(full, "/")
	if owners[root] != full {
		considerParents(p.parentOwners(base, root), root)
	}
	if best.compact == full {
		p.countedTails(full, base, consider)
	}
	return best
}

// label uses readable whole-component tails when safe, or the least expensive encoded
// identity from makeLabel. A zero budget explicitly requests the compact form.
func (p *failurePathIndex) label(raw string, budget int) string {
	if !p.hasIdentity(raw) {
		return ""
	}
	full := p.full[raw]
	if strings.HasPrefix(full, "<outside-workspace>/") {
		return full
	}
	if budget > 0 && len(full) <= budget {
		return exactFailureLabel(full)
	}
	label, ok := p.labels[full]
	if !ok {
		label = p.makeLabel(full)
		p.labels[full] = label
	}
	if strings.Contains(full, pathEllipsis) {
		return label.compact
	}
	if budget > 0 {
		tail := truncatePathTail(full, budget)
		if suffix, ok := strings.CutPrefix(tail, pathEllipsis+"/"); ok &&
			len(suffix) >= len(slashBase(full)) && len(suffix)+1 > label.sharedSuffix &&
			p.count[tail] == 0 && p.count[suffix] == 0 {
			return tail
		}
	}
	return label.compact
}

// memoStrings returns f memoized over [0, n).
func memoStrings(n int, f func(int) string) func(int) string {
	values, known := make([]string, n), make([]bool, n)
	return func(i int) string {
		if !known[i] {
			values[i], known[i] = f(i), true
		}
		return values[i]
	}
}

func partialPublicationFlag(r editing.CommitReceipt, dirty bool) bool {
	switch r.State {
	case editing.CommitStatePartiallyPublished,
		editing.CommitStatePublishedWithPostError,
		editing.CommitStatePublicationUncertain:
		return true
	case editing.CommitStatePrePublishFailure,
		editing.CommitStateRolledBack,
		editing.CommitStateCommitted:
		return false
	default:
		return dirty
	}
}

func truncateUTF8Bytes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	const ellipsis = "…"
	if maxBytes <= len(ellipsis) {
		cut := maxBytes
		for cut > 0 && !utf8.ValidString(s[:cut]) {
			cut--
		}
		return s[:cut]
	}
	limit := maxBytes - len(ellipsis)
	cut := 0
	for i := range s {
		if i > limit {
			break
		}
		cut = i
	}
	for cut > 0 && !utf8.ValidString(s[:cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

// displayPaths is the shared host-path scrubber. The workspace root and its real (symlink-resolved)
// root are both recognized, so diagnostics and error text never reveal where the workspace lives,
// whether it was reached by a Windows drive/UNC spelling or through a symlink.
func (s *Set) displayPaths() *editing.DisplayPaths {
	if s == nil {
		return editing.NewDisplayPaths("", "")
	}
	return editing.NewDisplayPaths(s.workspace, s.realWorkspace)
}

// sanitizeStructuredPath converts any path entering Agent-visible structured results or diagnostics
// into a verified workspace-relative slash path, "." for the workspace root, or
// "<outside-workspace>/<basename>" when the path lies outside or escapes the workspace (ADR-0024 R5-04, R5-05, R7).
func (s *Set) sanitizeStructuredPath(rawPath string) string {
	return s.displayPaths().Path(rawPath)
}

// sanitizeErrorText sanitizes host filesystem paths embedded inside unstructured error messages using
// strict token and path-component boundaries so sibling paths such as "<workspace>-sibling/secret"
// are never corrupted into ".-sibling/secret" and never leak host directories (ADR-0024 R5-04, R5-05, R7).
func (s *Set) sanitizeErrorText(msg string) string {
	return s.displayPaths().Text(msg)
}

func (s *Set) sanitizeDiagnostics(in []editing.Diagnostic, limit, pathBytes, textBytes int) ([]editing.Diagnostic, bool) {
	if len(in) == 0 || limit <= 0 {
		return nil, len(in) > 0
	}
	truncated := len(in) > limit
	count := len(in)
	if count > limit {
		count = limit
	}
	out := make([]editing.Diagnostic, 0, count)
	for _, d := range in[:count] {
		origPath := s.sanitizeStructuredPath(d.Path)
		origText := s.sanitizeErrorText(d.Text)
		d.Path = truncateUTF8Bytes(origPath, pathBytes)
		d.Text = truncateUTF8Bytes(origText, textBytes)
		if d.Path != origPath || d.Text != origText {
			truncated = true
		}
		out = append(out, d)
	}
	return out, truncated
}

const (
	notationShort    = "… marks omitted text: …/ is one or more directories, … in a name is characters. Such labels are IDs, not paths: match each to a path you sent."
	notationCount    = "…{N}/ is exactly N directories."
	notationTail     = "…/./x is omitted directories, then exactly x."
	notationExact    = "./x is exactly workspace path x; any … in it is literal."
	notationRedacted = "<outside-workspace>/x is a file outside the workspace named exactly x."
)

type notationForms uint8

const (
	formShort notationForms = 1 << iota
	formCount
	formTail
	formExact
	formRedacted
)

// isCountedComponent reports whether a component is exactly "…{N}" with a canonical count from 1 to 9999.
func isCountedComponent(c string) bool {
	inner, ok := strings.CutPrefix(c, pathEllipsis+"{")
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

// labelForms names the notation forms one emitted label relies on.
func labelForms(label string) notationForms {
	switch {
	case label == "":
		return 0
	case strings.HasPrefix(label, "./"):
		return formExact
	case strings.HasPrefix(label, "<outside-workspace>/"):
		if strings.Contains(label, pathEllipsis) {
			return formRedacted
		}
		return 0
	case strings.HasPrefix(label, pathEllipsis+"/./"):
		return formShort | formTail
	case !strings.Contains(label, pathEllipsis):
		return 0
	}
	forms := formShort
	for _, c := range strings.Split(label, "/") {
		if isCountedComponent(c) {
			forms |= formCount
		}
	}
	return forms
}

// pathNotation is the fixed legend for the forms in use, in a fixed order.
func pathNotation(forms notationForms) string {
	var parts []string
	if forms&(formShort|formCount|formTail) != 0 {
		parts = append(parts, notationShort)
	}
	if forms&formCount != 0 {
		parts = append(parts, notationCount)
	}
	if forms&formTail != 0 {
		parts = append(parts, notationTail)
	}
	if forms&formExact != 0 {
		parts = append(parts, notationExact)
	}
	if forms&formRedacted != 0 {
		parts = append(parts, notationRedacted)
	}
	return strings.Join(parts, " ")
}

func setPathNotation(payload map[string]any, forms notationForms) {
	if text := pathNotation(forms); text != "" {
		payload["pathNotation"] = text
	} else {
		delete(payload, "pathNotation")
	}
}

const maxOverflowCandidates = 256 // renderable rows considered per priority class

// overflowRow is one renderable row of the overflow search. at is its index in orderedFiles, key its
// sanitized display path, and cost the exact JSON bytes of its compact row.
type overflowRow struct {
	at    int
	key   string
	state editing.PublishState
	cost  int
	forms notationForms
}

// stateEntryBytes is the exact size of one `"state":count,` entry of filesOmittedByState.
func stateEntryBytes(state editing.PublishState, count int) int {
	if count <= 0 {
		return 0
	}
	return len(state) + 4 + len(strconv.Itoa(count))
}

// omissionBytes is the exact size of the filesOmitted and filesOmittedByState keys added to an object.
func omissionBytes(n int, byState map[editing.PublishState]int) int {
	if n == 0 {
		return 0
	}
	total := 40 + len(strconv.Itoa(n))
	for state, count := range byState {
		total += stateEntryBytes(state, count)
	}
	return total
}

// rowBudget is the exact size of a payload whose rows are already chosen. base is the payload with
// "files":[] and no omission keys; rows and rowBytes are the chosen rows; shown counts their states.
type rowBudget struct {
	limit, base, total, rows, rowBytes int
	shown, stateTotal                  map[editing.PublishState]int
}

func (b rowBudget) size(extraRows, extraBytes int, extraShown map[editing.PublishState]int) int {
	n := b.rows + extraRows
	byState := map[editing.PublishState]int{}
	for state, total := range b.stateTotal {
		if left := total - b.shown[state] - extraShown[state]; left > 0 {
			byState[state] = left
		}
	}
	commas := max(n-1, 0)
	return b.base + b.rowBytes + extraBytes + commas + omissionBytes(b.total-n, byState)
}

// fewestBytes returns, for each k up to maxK, the least bytes of k pool rows together with the omitted
// state entries they leave. Rows of one state are taken cheapest first. Unreachable counts hold a
// sentinel.
func fewestBytes(pool []overflowRow, maxK int, shown, stateTotal map[editing.PublishState]int) []int {
	const unreachable = 1 << 60
	states := make([]editing.PublishState, 0, len(stateTotal))
	for state := range stateTotal {
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool { return states[i] < states[j] })
	costs := map[editing.PublishState][]int{}
	for _, row := range pool {
		costs[row.state] = append(costs[row.state], row.cost)
	}
	best := make([]int, maxK+1)
	for k := range best {
		best[k] = unreachable
	}
	best[0] = 0
	for _, state := range states {
		stateCosts := costs[state]
		sort.Ints(stateCosts)
		prefix := make([]int, len(stateCosts)+1)
		for i, c := range stateCosts {
			prefix[i+1] = prefix[i] + c
		}
		next := make([]int, maxK+1)
		for k := range next {
			next[k] = unreachable
		}
		for k := 0; k <= maxK; k++ {
			if best[k] == unreachable {
				continue
			}
			for j := 0; j <= len(stateCosts) && k+j <= maxK; j++ {
				cost := best[k] + prefix[j] + stateEntryBytes(state, stateTotal[state]-shown[state]-j)
				if cost < next[k+j] {
					next[k+j] = cost
				}
			}
		}
		best = next
	}
	return best
}

// selectRows returns the largest feasible set of at most maxK pool rows, and among equal sizes the
// lexicographically smallest by key. cands must be sorted by key.
func selectRows(cands []overflowRow, maxK int, b rowBudget) []overflowRow {
	head := func(n int) int {
		if n > 0 {
			return 40 + len(strconv.Itoa(n))
		}
		return 0
	}
	dp := fewestBytes(cands, maxK, b.shown, b.stateTotal)
	kStar := 0
	for k := maxK; k >= 1; k-- {
		if dp[k] < 1<<60 && b.base+b.rowBytes+dp[k]+max(b.rows+k-1, 0)+head(b.total-b.rows-k) <= b.limit {
			kStar = k
			break
		}
	}
	if kStar == 0 {
		return nil
	}
	if b.size(kStar, sumCost(cands[:min(kStar, len(cands))]), stateCounts(cands[:min(kStar, len(cands))])) <= b.limit {
		return cands[:kStar]
	}
	var picked []overflowRow
	forced := 0
	forcedShown := map[editing.PublishState]int{}
	for i, c := range cands {
		if len(picked) == kStar {
			break
		}
		need := kStar - len(picked) - 1
		trial := map[editing.PublishState]int{}
		for state, n := range b.shown {
			trial[state] = n
		}
		for state, n := range forcedShown {
			trial[state] += n
		}
		trial[c.state]++
		dpNeed := fewestBytes(cands[i+1:], need, trial, b.stateTotal)
		if dpNeed[need] < 1<<60 && b.base+b.rowBytes+forced+c.cost+dpNeed[need]+max(b.rows+kStar-1, 0)+head(b.total-b.rows-kStar) <= b.limit {
			picked = append(picked, c)
			forced += c.cost
			forcedShown[c.state]++
		}
	}
	return picked
}

func sumCost(rows []overflowRow) int {
	total := 0
	for _, r := range rows {
		total += r.cost
	}
	return total
}

func stateCounts(rows []overflowRow) map[editing.PublishState]int {
	counts := map[editing.PublishState]int{}
	for _, r := range rows {
		counts[r.state]++
	}
	return counts
}

func (s *Set) formatCommitFailurePayload(result editing.Result, receipt editing.CommitReceipt, err error, editErr *editing.Error) string {
	dirty := receiptDirtyOrPublished(receipt)
	partial := partialPublicationFlag(receipt, dirty)
	errLimit := maxErrorBytesInToolResult
	pathLimit := maxPathBytesInToolResult
	fileErrLimit := maxFileErrorBytesInResult
	diagTextLimit := maxDiagTextBytesInResult

	totalDiags := 0
	if editErr != nil {
		totalDiags = len(editErr.Diagnostics)
	}
	diagLimit := totalDiags
	if diagLimit > maxDiagnosticsInToolResult {
		diagLimit = maxDiagnosticsInToolResult
	}
	// Pre-index both views before trimming either one; omitted paths still participate in identity.
	display := s.displayPaths()
	paths := newFailurePathIndex()
	for _, f := range receipt.Files {
		paths.add(f.Path, display)
	}
	for _, f := range result.Files {
		paths.add(f.Path, display)
	}
	errPath := ""
	standalonePath := len(receipt.Files) == 0 && len(result.Files) == 0
	if editErr != nil {
		if standalonePath {
			errPath = display.Path(editErr.Path)
			if strings.Contains(errPath, pathEllipsis) {
				paths.add(editErr.Path, display)
				errPath, standalonePath = "", false
			}
		} else {
			paths.add(editErr.Path, display)
		}
	}
	// A label needing the entire payload by itself is not renderable at any row count.
	identifiable := func(raw string) bool {
		if !paths.hasIdentity(raw) {
			return false
		}
		full := paths.full[raw]
		if strings.HasPrefix(full, "<outside-workspace>/") {
			return len(full) < maxPartialPayloadBytes/6 || encodedLen(full) < maxPartialPayloadBytes
		}
		if strings.Contains(full, pathEllipsis) {
			return len(full)+2 < maxPartialPayloadBytes/6 || encodedLen(paths.label(raw, 0)) < maxPartialPayloadBytes
		}
		base := slashBase(full)
		if len(base) < maxPartialPayloadBytes/6 || encodedLen(base) < maxPartialPayloadBytes {
			return true
		}
		return encodedLen(paths.shortBase(base)) < maxPartialPayloadBytes
	}
	orderedFiles := make([]editing.FileCommitReceipt, 0, len(receipt.Files))
	unidentifiable := make(map[editing.PublishState]int)
	seen := make(map[string]bool)
	for _, f := range prioritizedReceiptFiles(receipt.Files) {
		if !identifiable(f.Path) || seen[f.Path] {
			unidentifiable[f.PublishState]++
			continue
		}
		seen[f.Path] = true
		orderedFiles = append(orderedFiles, f)
	}
	fileLimit := min(len(orderedFiles), maxPartialFilesInToolResult)
	rowErr := memoStrings(len(orderedFiles), func(i int) string { return display.Text(orderedFiles[i].Error) })

	// Keep the compatibility view of actual publications when space permits.
	var published []editing.FileResult
	totalPublished := 0
	clear(seen)
	byPath := make(map[string]editing.FileResult, len(result.Files))
	for _, f := range result.Files {
		byPath[f.Path] = f
		if f.Published {
			totalPublished++
			if identifiable(f.Path) && !seen[f.Path] {
				seen[f.Path] = true
				published = append(published, f)
			}
		}
	}
	pubLimit := min(len(published), maxPartialFilesInToolResult)

	sanitizedErr := ""
	if err != nil {
		sanitizedErr = s.sanitizeErrorText(err.Error())
	}

	recovery := ""
	if dirty || partial {
		recovery = "LC does not roll back published paths. Re-read every path listed in files, including the failed path, from disk and reconcile manually. Do not replay a path whose verified state is true."
	}

	resolveCommitState := func() editing.CommitState {
		state := receipt.State
		if state == "" {
			if dirty {
				state = editing.CommitStatePublicationUncertain
			} else {
				state = editing.CommitStatePrePublishFailure
			}
		}
		return state
	}

	// They lead the rich envelope; it never drops a failed row; the compact fallback chooses a bounded prefix.
	fileFloor := 0
	for fileFloor < len(orderedFiles) && receiptFileRank(orderedFiles[fileFloor]) == 0 {
		fileFloor++
	}
	if fileFloor == 0 && len(orderedFiles) > 0 {
		fileFloor = 1
	}
	if fileFloor > fileLimit {
		fileLimit = fileFloor
	}
	rowPathLimit := func(f editing.FileCommitReceipt, pBytes int) int {
		if receiptFileRank(f) == 0 {
			return maxFailedPathBytesInResult
		}
		return pBytes
	}
	setFileOmissions := func(payload map[string]any, from int) bool {
		delete(payload, "filesOmitted")
		delete(payload, "filesOmittedByState")
		if omitted := len(receipt.Files) - from; omitted > 0 {
			counts := make(map[editing.PublishState]int, len(unidentifiable))
			for state, count := range unidentifiable {
				counts[state] = count
			}
			for _, f := range orderedFiles[from:] {
				counts[f.PublishState]++
			}
			payload["filesOmitted"] = omitted
			payload["filesOmittedByState"] = counts
			return true
		}
		return false
	}

	build := func(errBytes, pBytes, fErrBytes, dLimit, dTextBytes, fLimit, pLimit int, hashes bool) []byte {
		// truncated means removed content (shortened text or paths, omitted rows or diagnostics, dropped
		// optional evidence), never the reversible "./" qualifier or redaction.
		wasTruncated := false
		var forms notationForms
		errStr := truncateUTF8Bytes(sanitizedErr, errBytes)
		if errStr != sanitizedErr {
			wasTruncated = true
		}
		payload := map[string]any{
			"error": errStr,
		}
		if editErr != nil {
			payload["code"] = truncateUTF8Bytes(editErr.Code, 64)
			hint := editErr.Hint()
			if h := truncateUTF8Bytes(hint, 256); h != "" {
				if h != hint {
					wasTruncated = true
				}
				payload["hint"] = h
			}
			label := truncatePathTail(errPath, maxFailedPathBytesInResult)
			clean := errPath
			if !standalonePath {
				clean = exactFailureLabel(paths.full[editErr.Path])
				label = paths.label(editErr.Path, maxFailedPathBytesInResult)
			}
			if label != clean {
				wasTruncated = true
			}
			payload["path"] = label
			forms |= labelForms(label)
			if editErr.CurrentRev != "" {
				payload["currentRev"] = truncateUTF8Bytes(editErr.CurrentRev, 64)
			}
			if editErr.Holder != "" {
				payload["holder"] = truncateUTF8Bytes(editErr.Holder, 128)
			}
			diags, dTrunc := s.sanitizeDiagnostics(editErr.Diagnostics, dLimit, pBytes, dTextBytes)
			if dTrunc {
				wasTruncated = true
			}
			if len(diags) > 0 {
				payload["diagnostics"] = diags
			}
			if omitted := totalDiags - len(diags); omitted > 0 {
				payload["diagnosticsOmitted"] = omitted
				wasTruncated = true
			}
		}
		if dirty || receipt.State != "" {
			payload["commitState"] = resolveCommitState()
			payload["dirtyFailure"] = dirty
			payload["partialPublication"] = partial
		}
		if recovery != "" {
			payload["recovery"] = recovery
		}
		var rowLabels map[string]string
		if pLimit > 0 {
			rowLabels = make(map[string]string)
		}
		if len(receipt.Files) > 0 {
			files := make([]toolFileState, 0, fLimit)
			for i, rf := range orderedFiles[:fLimit] {
				label := paths.label(rf.Path, rowPathLimit(rf, pBytes))
				forms |= labelForms(label)
				state, rowTrunc := receiptFileState(rf, label, rowErr(i), byPath, fErrBytes, hashes)
				if rowTrunc || label != exactFailureLabel(paths.full[rf.Path]) {
					wasTruncated = true
				}
				files = append(files, state)
				if rowLabels != nil {
					rowLabels[rf.Path] = label
				}
			}
			payload["files"] = files
			if setFileOmissions(payload, fLimit) {
				wasTruncated = true
			}
		}
		if pLimit > 0 {
			pub := make([]editing.FileResult, 0, pLimit)
			for _, pf := range published[:pLimit] {
				path, ok := rowLabels[pf.Path]
				if !ok {
					path = paths.label(pf.Path, pBytes)
				}
				forms |= labelForms(path)
				cleanE := display.Text(pf.Error)
				if path != exactFailureLabel(paths.full[pf.Path]) || len(cleanE) > fErrBytes {
					wasTruncated = true
				}
				pf.Path, pf.Error = path, truncateUTF8Bytes(cleanE, fErrBytes)
				pub = append(pub, pf)
			}
			payload["publishedFiles"] = pub
		}
		if omitted := totalPublished - pLimit; omitted > 0 {
			payload["publishedFilesOmitted"] = omitted
			wasTruncated = true
		}
		if wasTruncated {
			payload["truncated"] = true
		}
		setPathNotation(payload, forms)
		raw, _ := json.Marshal(payload)
		return raw
	}

	overflow := func() string {
		payload := map[string]any{
			"error":     truncateUTF8Bytes(sanitizedErr, 96),
			"truncated": true,
			"overflow":  true,
		}
		if editErr != nil {
			payload["code"] = truncateUTF8Bytes(editErr.Code, 64)
			if totalDiags > 0 {
				payload["diagnosticsOmitted"] = totalDiags
			}
		}
		if dirty || receipt.State != "" {
			payload["commitState"] = resolveCommitState()
			payload["dirtyFailure"] = dirty
			payload["partialPublication"] = partial
		}
		if recovery != "" {
			payload["recovery"] = recovery
		}
		if totalPublished > 0 {
			payload["publishedFilesOmitted"] = totalPublished
		}
		encode := func() []byte {
			raw, _ := json.Marshal(payload)
			return raw
		}
		budgets := []struct{ path, rowError int }{
			{maxFailedPathBytesInResult, 32}, {512, 32}, {256, 32}, {256, 0}, {128, 0}, {96, 0}, {64, 0},
		}
		if len(receipt.Files) == 0 {
			// A refusal with no receipt row is named by its path alone.
			for _, b := range budgets {
				if errPath != "" {
					payload["path"] = truncatePathTail(errPath, b.path)
				} else if editErr != nil {
					if label := paths.label(editErr.Path, 0); label != "" {
						payload["path"] = label
					}
				}
				pathLabel, _ := payload["path"].(string)
				setPathNotation(payload, labelForms(pathLabel))
				if raw := encode(); len(raw) <= maxPartialPayloadBytes {
					return string(raw)
				}
			}
			delete(payload, "path")
			setPathNotation(payload, 0)
			return string(encode())
		}

		stateTotal := map[editing.PublishState]int{}
		for _, f := range receipt.Files {
			stateTotal[f.PublishState]++
		}
		total := len(receipt.Files)
		payload["files"] = []toolFileState{}
		delete(payload, "filesOmitted")
		delete(payload, "filesOmittedByState")
		delete(payload, "pathNotation")
		bare := len(encode())

		var classes [3][]int
		for at, f := range orderedFiles {
			c := receiptFileRank(f)
			classes[c] = append(classes[c], at)
		}
		var cands [3][]overflowRow
		forms := notationForms(0)
		for c := range classes {
			sort.Slice(classes[c], func(i, j int) bool {
				return paths.full[orderedFiles[classes[c][i]].Path] < paths.full[orderedFiles[classes[c][j]].Path]
			})
			for _, at := range classes[c] {
				f := orderedFiles[at]
				label := paths.label(f.Path, 0)
				row, _ := receiptFileState(f, label, "", byPath, 0, false)
				raw, _ := json.Marshal(row)
				alone := rowBudget{limit: maxPartialPayloadBytes, base: bare, total: total, stateTotal: stateTotal, shown: map[editing.PublishState]int{}}
				if alone.size(1, len(raw), map[editing.PublishState]int{f.PublishState: 1}) > maxPartialPayloadBytes {
					continue
				}
				r := overflowRow{at: at, key: paths.full[f.Path], state: f.PublishState, cost: len(raw), forms: labelForms(label)}
				cands[c] = append(cands[c], r)
				forms |= r.forms
				if len(cands[c]) == maxOverflowCandidates {
					break
				}
			}
		}

		var chosen []overflowRow
		for {
			setPathNotation(payload, forms)
			b := rowBudget{limit: maxPartialPayloadBytes, base: len(encode()), total: total, stateTotal: stateTotal, shown: map[editing.PublishState]int{}}
			chosen = nil
			for c := 0; c < 3; c++ {
				maxK := maxCandidateFileRows
				if c > 0 {
					maxK = maxPartialFilesInToolResult - b.rows
				}
				if maxK <= 0 {
					continue
				}
				var pool []overflowRow
				for _, r := range cands[c] {
					if r.forms&^forms == 0 {
						pool = append(pool, r)
					}
				}
				for _, r := range selectRows(pool, maxK, b) {
					chosen = append(chosen, r)
					b.rows++
					b.rowBytes += r.cost
					b.shown[r.state]++
				}
			}
			got := notationForms(0)
			for _, r := range chosen {
				got |= r.forms
			}
			if got == forms {
				break
			}
			forms = got
		}
		sort.Slice(chosen, func(i, j int) bool { return chosen[i].at < chosen[j].at })

		setOverflowRows := func(rows []overflowRow, rowLabel func(f editing.FileCommitReceipt) string, rowError func(at int) string, rowErrBytes int) {
			files := make([]toolFileState, len(rows))
			shownState := map[editing.PublishState]int{}
			rowForms := notationForms(0)
			for i, r := range rows {
				f := orderedFiles[r.at]
				label := rowLabel(f)
				rowForms |= labelForms(label)
				files[i], _ = receiptFileState(f, label, rowError(r.at), byPath, rowErrBytes, false)
				shownState[f.PublishState]++
			}
			payload["files"] = files
			delete(payload, "filesOmitted")
			delete(payload, "filesOmittedByState")
			if omitted := total - len(rows); omitted > 0 {
				byState := map[editing.PublishState]int{}
				for state, n := range stateTotal {
					if left := n - shownState[state]; left > 0 {
						byState[state] = left
					}
				}
				payload["filesOmitted"] = omitted
				payload["filesOmittedByState"] = byState
			}
			setPathNotation(payload, rowForms)
		}
		fits := func() bool { return len(encode()) <= maxPartialPayloadBytes }
		for _, b := range budgets {
			setOverflowRows(chosen, func(f editing.FileCommitReceipt) string {
				return paths.label(f.Path, b.path)
			}, rowErr, b.rowError)
			if fits() {
				return string(encode())
			}
		}
		for len(chosen) > 0 {
			setOverflowRows(chosen, func(f editing.FileCommitReceipt) string {
				return paths.label(f.Path, 0)
			}, func(int) string { return "" }, 0)
			if fits() {
				return string(encode())
			}
			chosen = chosen[:len(chosen)-1]
		}
		setOverflowRows(nil, nil, nil, 0)
		return string(encode())
	}
	if fileFloor > maxCandidateFileRows {
		return overflow()
	}

	hashes := true
	raw := build(errLimit, pathLimit, fileErrLimit, diagLimit, diagTextLimit, fileLimit, pubLimit, hashes)
	for len(raw) > maxPartialPayloadBytes {
		// Order (R7R2-Results-01): diagnostics, the publishedFiles duplicate of the files rows, per-row
		// error text, the top-level error, per-row hashes, then whole rows from the untouched and
		// verified end, and only then the paths of the rows that remain. A path is cut from the front.
		switch {
		case diagLimit > 4:
			diagLimit /= 2
		case diagTextLimit > 96:
			diagTextLimit /= 2
		case diagLimit > 0:
			diagLimit--
		case pubLimit > 0:
			pubLimit /= 2
		case fileErrLimit > 0:
			fileErrLimit /= 2
		case errLimit > 192:
			errLimit /= 2
		case hashes:
			hashes = false
		case fileLimit > fileFloor:
			fileLimit--
		case pathLimit > 64:
			pathLimit /= 2
		case errLimit > 64:
			errLimit = 64
		default:
			return overflow()
		}
		raw = build(errLimit, pathLimit, fileErrLimit, diagLimit, diagTextLimit, fileLimit, pubLimit, hashes)
	}
	return string(raw)
}

// displayError carries a host-path-scrubbed message while preserving the original error chain, so
// errors.Is/errors.As still classify a preflight or I/O failure that leaves a tool handler.
type displayError struct {
	msg string
	err error
}

func (e *displayError) Error() string { return e.msg }
func (e *displayError) Unwrap() error { return e.err }

// sanitizeToolError scrubs host paths from a generic error before it leaves a tool handler through
// the ordinary error return. Editing refusals render their own structured payload and are not
// wrapped here; an unchanged message keeps the original error value.
func (s *Set) sanitizeToolError(err error) error {
	if err == nil {
		return nil
	}
	msg := s.sanitizeErrorText(err.Error())
	if msg == err.Error() {
		return err
	}
	return &displayError{msg: msg, err: err}
}

// editFailure renders a refusal as a tool result the model can act on: the full sanitized error
// (preserving any joined publication errors), its stable code, remediation, and bounded per-file
// publication state for every receipt state. MutatesWorkspace stays false except where the engine
// entered publication or cannot rule out a write.
func (s *Set) editFailure(result editing.Result, receipt editing.CommitReceipt, err error) protocol.ToolResult {
	dirty := receiptDirtyOrPublished(receipt)
	editErr, ok := editing.AsError(err)
	if !ok {
		return protocol.ToolResult{
			OK:               false,
			Content:          s.formatCommitFailurePayload(result, receipt, err, nil),
			MutatesWorkspace: dirty,
		}
	}
	return protocol.ToolResult{
		OK:               false,
		Content:          s.formatCommitFailurePayload(result, receipt, err, editErr),
		MutatesWorkspace: dirty || editErr.Code == editing.CodeConcurrentWrite,
	}
}

// editOutcome is how a tool returns a pre-commit failure: a refusal the model can act on (an editing.Error)
// becomes an OK:false result carrying its code and hint; anything else — a path guard refusal, an I/O
// error — stays an error, so the caller's contract is unchanged.
func (s *Set) editOutcome(err error) (protocol.ToolResult, error) {
	return s.commitOutcome(editing.Result{}, editing.CommitReceipt{}, err)
}

func (s *Set) commitOutcome(result editing.Result, receipt editing.CommitReceipt, err error) (protocol.ToolResult, error) {
	dirty := receiptDirtyOrPublished(receipt)
	if _, ok := editing.AsError(err); ok {
		return s.editFailure(result, receipt, err), nil
	}
	var content string
	if dirty || len(result.Files) > 0 || len(receipt.Files) > 0 {
		content = s.formatCommitFailurePayload(result, receipt, err, nil)
	}
	return protocol.ToolResult{OK: false, Content: content, MutatesWorkspace: dirty}, s.sanitizeToolError(err)
}

// editResult renders a committed plan with sanitized paths and diagnostics.
func (s *Set) editResult(result editing.Result, receipt editing.CommitReceipt, extra map[string]any) protocol.ToolResult {
	files := make([]editing.FileResult, 0, len(result.Files))
	for _, f := range result.Files {
		f.Path = s.sanitizeStructuredPath(f.Path)
		f.Error = s.sanitizeErrorText(f.Error)
		files = append(files, f)
	}
	diags, _ := s.sanitizeDiagnostics(result.Diagnostics, len(result.Diagnostics), maxPathBytesInToolResult, maxErrorBytesInToolResult)
	payload := map[string]any{"files": files, "diagnostics": diags}
	for key, value := range extra {
		payload[key] = value
	}
	content, err := json.Marshal(payload)
	if err != nil {
		return protocol.ToolResult{OK: false, Content: fmt.Sprintf(`{"error":%q}`, s.sanitizeErrorText(err.Error())), MutatesWorkspace: receiptWrote(receipt)}
	}
	return protocol.ToolResult{OK: true, Content: string(content), MutatesWorkspace: receiptWrote(receipt)}
}

// typeCheckNote summarizes whether the Go type checker really ran, for a tool result the model reads.
func typeCheckNote(diagnostics []editing.Diagnostic) string {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == editing.CodeTypeCheckDegraded {
			return diagnostic.Text
		}
	}
	return ""
}
