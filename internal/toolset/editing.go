package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	// maxFailedPathBytesInResult bounds a failed row's path. The shrink loop never lowers it, so the
	// path that tells the agent which file failed is cut only by the overflow envelope, and then
	// from the front (ADR-0024 R7R2-Results-01).
	maxFailedPathBytesInResult = 1024
	maxPartialPayloadBytes     = 4096
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
		f.PublishState == editing.PublishRollbackFailed:
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

// receiptFileState renders one receipt row from its sanitized path and error text. Paths are
// shortened from the front so the filename survives; hashes is false once the payload must drop
// optional per-row evidence, which the persisted receipt still holds.
func receiptFileState(f editing.FileCommitReceipt, cleanP, cleanE string, byPath map[string]editing.FileResult, pBytes, fErrBytes int, hashes bool) (toolFileState, bool) {
	truncP := truncatePathTail(cleanP, pBytes)
	truncE := truncateUTF8Bytes(cleanE, fErrBytes)
	truncated := truncP != cleanP || truncE != cleanE
	state := toolFileState{
		Path:         truncP,
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

// truncatePathTail shortens a slash path to maxBytes by keeping its tail, from a component boundary
// where one fits, so rows that share a deep prefix stay distinct by filename (R7R2-Results-01).
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

const (
	pathEllipsis = "…"
	// maxDistinctSegmentBytes bounds the segment distinguishShortenedPaths adds to a row, and
	// distinctSegmentLeadBytes the bytes it keeps in front of the first difference when that
	// difference lies deep inside a long name.
	maxDistinctSegmentBytes  = 48
	distinctSegmentLeadBytes = 16
)

// distinguishShortenedPaths makes each shortened row path name one file of the commit.
// truncatePathTail keeps a path's tail, so paths that differ only in front of it, such as one file
// name in several modules, shorten to the same text and a row no longer says which of them it
// reports. A row whose tail also fits another path of the commit gains the segment where its own
// path leaves those paths, and keeps its tail: "…/billing/…/config/App.kt" (R7R2-NEW-01).
// rows[i] renders pathAt(i), and pathAt covers all n paths of the commit. A row stays as it is
// when its tail is unique or no segment within the bound tells it apart.
func distinguishShortenedPaths(rows []toolFileState, n int, pathAt func(int) string) {
	for i := range rows {
		full := pathAt(i)
		tail, shortened := strings.CutPrefix(rows[i].Path, pathEllipsis)
		if !shortened || len(tail) >= len(full) || !strings.HasSuffix(full, tail) {
			continue
		}
		keep := len(full) - len(tail) // the kept tail starts here
		// first is where the paths that share this tail begin to differ, last where this path
		// leaves the closest of them. Every path of that group computes the same first.
		first, last, shared := keep, 0, false
		for j := 0; j < n; j++ {
			if j == i {
				continue
			}
			if other := pathAt(j); strings.HasSuffix(other, tail) {
				common := commonPrefixLen(full, other)
				first, last, shared = min(first, common), max(last, common), true
			}
		}
		if !shared || last >= keep {
			continue
		}
		// The segment starts at the name the paths differ in, or a few bytes before the difference
		// when that name is long, and covers the first byte no other path of the group has. The
		// start depends only on what the group shares, so two rows of one group always differ
		// inside their segments.
		start := strings.LastIndexByte(full[:first], '/') + 1
		if first-start > distinctSegmentLeadBytes {
			start = first - distinctSegmentLeadBytes
			for start > 0 && !utf8.RuneStart(full[start]) {
				start--
			}
		} else if start > 0 {
			start-- // the separator in front of the name
		}
		end := last + 1
		for end < keep && !utf8.RuneStart(full[end]) {
			end++
		}
		if end-start > maxDistinctSegmentBytes {
			continue
		}
		// Run on to the end of that name while the segment stays within its bound.
		if next := strings.IndexByte(full[end:keep], '/'); next < 0 {
			if keep-start <= maxDistinctSegmentBytes {
				end = keep
			}
		} else if end+next+1-start <= maxDistinctSegmentBytes {
			end += next + 1
		}
		lead := ""
		if start > 0 {
			lead = pathEllipsis
		}
		if end >= keep {
			rows[i].Path = lead + full[start:]
		} else {
			rows[i].Path = lead + full[start:end] + rows[i].Path
		}
	}
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
	// Every receipt state, including a clean refusal and the untouched suffix, gets a files row.
	// At least the failed path must survive truncation.
	orderedFiles := prioritizedReceiptFiles(receipt.Files)
	fileLimit := len(orderedFiles)
	if fileLimit > maxPartialFilesInToolResult {
		fileLimit = maxPartialFilesInToolResult
	}
	if len(orderedFiles) > 0 && fileLimit == 0 {
		fileLimit = 1
	}
	// Sanitizing a path resolves it on disk, so a row's display path and error text are computed
	// once, on first use; every reduction below only cuts them.
	rowPath := memoStrings(len(orderedFiles), func(i int) string { return s.sanitizeStructuredPath(orderedFiles[i].Path) })
	rowErr := memoStrings(len(orderedFiles), func(i int) string { return s.sanitizeErrorText(orderedFiles[i].Error) })

	// publishedFiles lists only paths LC really published. Result.Files now also contains untouched
	// and refused paths for every receipt state, so filtering is mandatory.
	published := make([]editing.FileResult, 0, len(result.Files))
	for _, fr := range result.Files {
		if fr.Published {
			published = append(published, fr)
		}
	}
	pubLimit := len(published)
	if pubLimit > maxPartialFilesInToolResult {
		pubLimit = maxPartialFilesInToolResult
	}

	byPath := make(map[string]editing.FileResult, len(result.Files))
	for _, fr := range result.Files {
		byPath[fr.Path] = fr
	}

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

	// The failed path and any other unresolved row lead orderedFiles; the shrink loop never drops
	// them or shortens their paths. Rows that state a verified or untouched path go first.
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
	omittedByState := func(from int) map[editing.PublishState]int {
		counts := map[editing.PublishState]int{}
		for _, f := range orderedFiles[from:] {
			counts[f.PublishState]++
		}
		return counts
	}

	build := func(errBytes, pBytes, fErrBytes, dLimit, dTextBytes, fLimit, pLimit int, hashes bool) []byte {
		wasTruncated := false
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
			cleanPath := s.sanitizeStructuredPath(editErr.Path)
			truncPath := truncatePathTail(cleanPath, maxFailedPathBytesInResult)
			if truncPath != cleanPath {
				wasTruncated = true
			}
			payload["path"] = truncPath
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
		if len(orderedFiles) > 0 {
			// Keep the key present in every state; truncation shrinks rows but never removes it.
			if fLimit < fileFloor {
				fLimit = fileFloor
			}
			if fLimit > len(orderedFiles) {
				fLimit = len(orderedFiles)
			}
			files := make([]toolFileState, 0, fLimit)
			for i, rf := range orderedFiles[:fLimit] {
				state, rowTrunc := receiptFileState(rf, rowPath(i), rowErr(i), byPath, rowPathLimit(rf, pBytes), fErrBytes, hashes)
				if rowTrunc {
					wasTruncated = true
				}
				files = append(files, state)
			}
			distinguishShortenedPaths(files, len(orderedFiles), rowPath)
			payload["files"] = files
			if omitted := len(orderedFiles) - fLimit; omitted > 0 {
				payload["filesOmitted"] = omitted
				payload["filesOmittedByState"] = omittedByState(fLimit)
				wasTruncated = true
			}
		}
		if len(published) > 0 && pLimit > 0 {
			if pLimit > len(published) {
				pLimit = len(published)
			}
			pub := make([]editing.FileResult, 0, pLimit)
			for _, pf := range published[:pLimit] {
				cleanP := s.sanitizeStructuredPath(pf.Path)
				pf.Path = truncatePathTail(cleanP, pBytes)
				cleanE := s.sanitizeErrorText(pf.Error)
				pf.Error = truncateUTF8Bytes(cleanE, fErrBytes)
				if pf.Path != cleanP || pf.Error != cleanE {
					wasTruncated = true
				}
				pub = append(pub, pf)
			}
			payload["publishedFiles"] = pub
			if omitted := len(published) - pLimit; omitted > 0 {
				payload["publishedFilesOmitted"] = omitted
				wasTruncated = true
			}
		} else if len(published) > 0 {
			payload["publishedFilesOmitted"] = len(published)
			wasTruncated = true
		}
		if wasTruncated {
			payload["truncated"] = true
		}
		raw, _ := json.Marshal(payload)
		return raw
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
			// Safe explicit overflow envelope when extreme JSON/Unicode escaping prevents optional
			// fields from fitting within maxPartialPayloadBytes (ADR-0024 R5-03). It still carries
			// the failed rows that fit and explicit omission totals.
			overflow := map[string]any{
				"error":     truncateUTF8Bytes(sanitizedErr, 96),
				"truncated": true,
				"overflow":  true,
			}
			if editErr != nil {
				overflow["code"] = truncateUTF8Bytes(editErr.Code, 64)
				if totalDiags > 0 {
					overflow["diagnosticsOmitted"] = totalDiags
				}
			}
			if dirty || receipt.State != "" {
				overflow["commitState"] = resolveCommitState()
				overflow["dirtyFailure"] = dirty
				overflow["partialPublication"] = partial
			}
			if recovery != "" {
				overflow["recovery"] = recovery
			}
			if len(published) > 0 {
				overflow["publishedFilesOmitted"] = len(published)
			}
			// A refusal with no receipt row is named by its path alone, so the envelope keeps it.
			errPath := ""
			if editErr != nil && len(orderedFiles) == 0 {
				errPath = s.sanitizeStructuredPath(editErr.Path)
			}
			render := func(count, pBytes, fErrBytes int) []byte {
				if len(orderedFiles) > 0 {
					rows := make([]toolFileState, 0, count)
					for i, rf := range orderedFiles[:count] {
						state, _ := receiptFileState(rf, rowPath(i), rowErr(i), byPath, pBytes, fErrBytes, false)
						rows = append(rows, state)
					}
					distinguishShortenedPaths(rows, len(orderedFiles), rowPath)
					overflow["files"] = rows
					delete(overflow, "filesOmitted")
					delete(overflow, "filesOmittedByState")
					if omitted := len(orderedFiles) - count; omitted > 0 {
						overflow["filesOmitted"] = omitted
						overflow["filesOmittedByState"] = omittedByState(count)
					}
				} else if errPath != "" {
					overflow["path"] = truncatePathTail(errPath, pBytes)
				}
				fallbackRaw, _ := json.Marshal(overflow)
				return fallbackRaw
			}
			// The failed rows outrank the length of their paths (R7R2-NEW-01). Budgets run from the
			// longest path to the shortest; a path is cut from the front and keeps its filename.
			budgets := []struct{ pBytes, fErrBytes int }{
				{maxFailedPathBytesInResult, 32}, {512, 32}, {256, 32}, {256, 0}, {128, 0}, {96, 0}, {64, 0},
			}
			// Every failed row, at the longest paths that fit.
			for _, b := range budgets {
				if fallbackRaw := render(fileFloor, b.pBytes, b.fErrBytes); len(fallbackRaw) <= maxPartialPayloadBytes {
					return string(fallbackRaw)
				}
			}
			// They cannot all fit: keep the longest prefix of failed rows that any budget holds and
			// report the rest in filesOmitted and filesOmittedByState. A shorter budget replaces a
			// longer one only by keeping more failed rows; the size grows with the row count, so each
			// budget's longest fitting prefix is found by bisection.
			var best []byte
			kept := 0
			for _, b := range budgets {
				for lo, hi := kept+1, fileFloor-1; lo <= hi; {
					mid := (lo + hi) / 2
					if fallbackRaw := render(mid, b.pBytes, b.fErrBytes); len(fallbackRaw) <= maxPartialPayloadBytes {
						best, kept, lo = fallbackRaw, mid, mid+1
					} else {
						hi = mid - 1
					}
				}
			}
			if best == nil {
				// Not reachable with the engine's states and codes: one row at the shortest budget fits.
				last := budgets[len(budgets)-1]
				best = render(min(fileFloor, 1), last.pBytes, last.fErrBytes)
			}
			return string(best)
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
