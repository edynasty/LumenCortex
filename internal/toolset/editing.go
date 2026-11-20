package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
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
	maxPartialPayloadBytes      = 4096
	outsideWorkspacePlaceholder = "<outside-workspace>"
)

type toolFileState struct {
	Path         string               `json:"path"`
	PublishState editing.PublishState `json:"publishState"`
	Published    bool                 `json:"published"`
	RolledBack   bool                 `json:"rolledBack"`
	Wrote        bool                 `json:"wrote"`
	Error        string               `json:"error,omitempty"`
}

func receiptDirtyOrPublished(r editing.CommitReceipt) bool {
	if r.DirtyFailure {
		return true
	}
	for _, f := range r.Files {
		if f.Published || f.Wrote ||
			f.PublishState == editing.PublishUncertain ||
			f.PublishState == editing.PublishRolledBack ||
			f.PublishState == editing.PublishRollbackFailed ||
			f.PublishState == editing.PublishExternalModification ||
			f.PublishState == editing.PublishSucceededWithPostError {
			return true
		}
	}
	return false
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

func isASCIIAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isWindowsDrivePath(p string) bool {
	return len(p) >= 2 && isASCIIAlpha(p[0]) && p[1] == ':' && (len(p) == 2 || p[2] == '\\' || p[2] == '/')
}

func isUNCPath(p string) bool {
	return (strings.HasPrefix(p, `\\`) && len(p) > 2 && p[2] != '\\') ||
		(strings.HasPrefix(p, "//") && len(p) > 2 && p[2] != '/')
}

// parseWinOrUNCRoot splits a Windows drive path ("C:\a\b") or UNC path ("\\srv\share\a\b") into a
// case-folded volume key ("C:" or "//srv/share") and a clean slash-prefixed sub-path ("/a/b" or "/").
func parseWinOrUNCRoot(raw string) (volKey string, cleanSub string, rawSub string, ok bool) {
	if isWindowsDrivePath(raw) {
		volKey = strings.ToUpper(raw[:2])
		rest := strings.ReplaceAll(raw[2:], `\`, "/")
		cleanSub = path.Clean("/" + rest)
		rawSub = cleanSub
		return volKey, cleanSub, rawSub, true
	}
	if isUNCPath(raw) {
		slashed := strings.ReplaceAll(raw[2:], `\`, "/")
		parts := strings.SplitN(slashed, "/", 3)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return "", "", "", false
		}
		volKey = "//" + strings.ToLower(parts[0]) + "/" + strings.ToLower(parts[1])
		rest := ""
		if len(parts) == 3 {
			rest = parts[2]
		}
		cleanSub = path.Clean("/" + rest)
		rawSub = cleanSub
		return volKey, cleanSub, rawSub, true
	}
	return "", "", "", false
}

func outsidePlaceholderForSlashPath(cleanSlash string) string {
	base := path.Base(strings.TrimRight(cleanSlash, "/"))
	if base == "" || base == "." || base == "/" || base == ".." {
		return outsideWorkspacePlaceholder
	}
	return outsideWorkspacePlaceholder + "/" + base
}

// sanitizeStructuredPath converts any path entering Agent-visible structured results or diagnostics
// into a verified workspace-relative slash path, "." for the workspace root, or
// "<outside-workspace>/<basename>" when the path lies outside or escapes the workspace (ADR-0024 R5-04, R5-05).
func (s *Set) sanitizeStructuredPath(rawPath string) string {
	if rawPath == "" {
		return ""
	}
	if rawPath == "." {
		return "."
	}

	// Handle Windows drive and UNC paths portably across all host OSes.
	if targetVol, targetSub, _, ok := parseWinOrUNCRoot(rawPath); ok {
		if s != nil {
			for _, wsRoot := range []string{s.realWorkspace, s.workspace} {
				wsVol, wsSub, _, wsOK := parseWinOrUNCRoot(wsRoot)
				if !wsOK || wsVol != targetVol {
					continue
				}
				wsFold := strings.ToLower(wsSub)
				targetFold := strings.ToLower(targetSub)
				if targetFold == wsFold {
					return "."
				}
				prefix := wsFold
				if prefix != "/" {
					prefix += "/"
				}
				if strings.HasPrefix(targetFold, prefix) {
					rel := strings.TrimPrefix(targetSub[len(wsSub):], "/")
					if rel == "" {
						return "."
					}
					return rel
				}
			}
		}
		return outsidePlaceholderForSlashPath(targetSub)
	}

	if s == nil || (s.workspace == "" && s.realWorkspace == "") {
		if filepath.IsAbs(rawPath) {
			return outsidePlaceholderForSlashPath(filepath.ToSlash(filepath.Clean(rawPath)))
		}
		cleanRel := path.Clean(filepath.ToSlash(rawPath))
		if cleanRel == ".." || strings.HasPrefix(cleanRel, "../") {
			return outsidePlaceholderForSlashPath(cleanRel)
		}
		return cleanRel
	}

	wsClean := filepath.Clean(s.workspace)
	realWS := filepath.Clean(s.realWorkspace)
	if realWS == "" || realWS == "." {
		realWS = realOf(wsClean)
	}
	sep := string(filepath.Separator)

	// If workspace itself uses Windows/UNC notation (e.g. cross-OS unit test) and rawPath is relative:
	if wsVol, wsSub, _, wsOK := parseWinOrUNCRoot(wsClean); wsOK && !filepath.IsAbs(rawPath) {
		joined := wsVol + wsSub + "/" + strings.ReplaceAll(rawPath, `\`, "/")
		return s.sanitizeStructuredPath(joined)
	}

	candidate := rawPath
	wasAbs := filepath.IsAbs(rawPath)
	if !wasAbs {
		candidate = filepath.Join(wsClean, rawPath)
	}
	candidate = filepath.Clean(candidate)

	// Lexical relative check against s.workspace (preferred display path when real target also stays inside realWS).
	lexRel, lexErr := filepath.Rel(wsClean, candidate)
	lexInside := lexErr == nil && lexRel != ".." && !strings.HasPrefix(lexRel, ".."+sep) && !filepath.IsAbs(lexRel)

	// Real (symlink-resolved on deepest existing prefix) containment check against s.realWorkspace.
	realCandidate := realOf(candidate)
	realRel, realErr := filepath.Rel(realWS, realCandidate)
	realInside := realErr == nil && realRel != ".." && !strings.HasPrefix(realRel, ".."+sep) && !filepath.IsAbs(realRel)

	if realInside {
		if lexInside {
			return filepath.ToSlash(lexRel)
		}
		return filepath.ToSlash(realRel)
	}

	return outsidePlaceholderForSlashPath(filepath.ToSlash(candidate))
}

func isErrTextLeftBoundary(msg string, idx int) bool {
	if idx <= 0 {
		return true
	}
	switch msg[idx-1] {
	case ' ', '\t', '\n', '\r', '"', '\'', '(', '[', '{', '=', ':':
		return true
	}
	return false
}

func isErrTextRightBoundary(msg string, pos int) bool {
	if pos >= len(msg) {
		return true
	}
	switch msg[pos] {
	case ' ', '\t', '\n', '\r', '"', '\'', ')', ']', '}', ',', ';', ':':
		return true
	}
	return false
}

func isURLSchemeBefore(msg string, idx int) bool {
	for _, scheme := range []string{"http:", "https:"} {
		if idx >= len(scheme) && strings.EqualFold(msg[idx-len(scheme):idx], scheme) {
			return true
		}
	}
	return false
}

func looksLikeHostAbsToken(tok string) bool {
	if strings.Count(tok, "/") >= 2 {
		return true
	}
	for _, root := range []string{"/etc", "/var", "/tmp", "/usr", "/opt", "/dev", "/System", "/Library", "/Users", "/home", "/root", "/private", "/proc", "/sys"} {
		if tok == root || strings.HasPrefix(tok, root+"/") {
			return true
		}
	}
	return false
}

// sanitizeErrorText sanitizes host filesystem paths embedded inside unstructured error messages using
// strict token and path-component boundaries so sibling paths such as "<workspace>-sibling/secret"
// are never corrupted into ".-sibling/secret" and never leak host directories (ADR-0024 R5-04, R5-05).
func (s *Set) sanitizeErrorText(msg string) string {
	if msg == "" {
		return msg
	}
	var roots []string
	if s != nil {
		for _, r := range []string{s.realWorkspace, s.workspace} {
			if r == "" {
				continue
			}
			clean := filepath.Clean(r)
			if isWindowsDrivePath(r) || isUNCPath(r) {
				clean = strings.TrimRight(r, `\/`)
			}
			if clean != "" && clean != "." && clean != "/" && clean != `\` {
				roots = append(roots, clean)
				if slashed := strings.ReplaceAll(clean, `\`, "/"); slashed != clean {
					roots = append(roots, slashed)
				}
			}
		}
	}

	var out strings.Builder
	i := 0
	for i < len(msg) {
		if isErrTextLeftBoundary(msg, i) && !isURLSchemeBefore(msg, i) {
			// 1. Check explicit workspace roots first (supports workspace paths containing spaces),
			// requiring an exact right token boundary OR a '/' / '\' path-component separator!
			matchedRoot := false
			for _, root := range roots {
				if len(msg)-i < len(root) {
					continue
				}
				winRoot := isWindowsDrivePath(root) || isUNCPath(root)
				eq := msg[i:i+len(root)] == root
				if !eq && winRoot {
					eq = strings.EqualFold(msg[i:i+len(root)], root)
				}
				if !eq {
					continue
				}
				after := i + len(root)
				if isErrTextRightBoundary(msg, after) {
					out.WriteString(".")
					i = after
					matchedRoot = true
					break
				}
				end := after
				for end < len(msg) && !isErrTextRightBoundary(msg, end) {
					end++
				}
				out.WriteString(s.sanitizeStructuredPath(msg[i:end]))
				i = end
				matchedRoot = true
				break
			}
			if matchedRoot {
				continue
			}

			// 2. Windows drive path token (e.g. C:\foo\bar or C:/foo/bar).
			if isWindowsDrivePath(msg[i:]) && len(msg)-i >= 3 && (msg[i+2] == '\\' || msg[i+2] == '/') {
				end := i + 2
				for end < len(msg) && !isErrTextRightBoundary(msg, end) {
					end++
				}
				out.WriteString(s.sanitizeStructuredPath(msg[i:end]))
				i = end
				continue
			}

			// 3. UNC path token (e.g. \\server\share\foo or //server/share/foo).
			if isUNCPath(msg[i:]) {
				end := i + 2
				for end < len(msg) && !isErrTextRightBoundary(msg, end) {
					end++
				}
				if _, _, _, ok := parseWinOrUNCRoot(msg[i:end]); ok {
					out.WriteString(s.sanitizeStructuredPath(msg[i:end]))
					i = end
					continue
				}
			}

			// 4. POSIX absolute host path token (e.g. /Users/... or /var/folders/...-sibling/secret).
			if msg[i] == '/' {
				end := i
				for end < len(msg) && !isErrTextRightBoundary(msg, end) {
					end++
				}
				tok := msg[i:end]
				if looksLikeHostAbsToken(tok) {
					out.WriteString(s.sanitizeStructuredPath(tok))
					i = end
					continue
				}
			}
		}
		out.WriteByte(msg[i])
		i++
	}
	return out.String()
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
	fileLimit := len(receipt.Files)
	if fileLimit > maxPartialFilesInToolResult {
		fileLimit = maxPartialFilesInToolResult
	}
	pubLimit := len(result.Files)
	if pubLimit > maxPartialFilesInToolResult {
		pubLimit = maxPartialFilesInToolResult
	}

	sanitizedErr := ""
	if err != nil {
		sanitizedErr = s.sanitizeErrorText(err.Error())
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

	build := func(errBytes, pBytes, fErrBytes, dLimit, dTextBytes, fLimit, pLimit int) []byte {
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
			truncPath := truncateUTF8Bytes(cleanPath, pBytes)
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
			payload["partialPublication"] = partialPublicationFlag(receipt, dirty)
		}
		if len(receipt.Files) > 0 && fLimit > 0 {
			files := make([]toolFileState, 0, fLimit)
			for _, rf := range receipt.Files[:fLimit] {
				cleanP := s.sanitizeStructuredPath(rf.Path)
				truncP := truncateUTF8Bytes(cleanP, pBytes)
				cleanE := s.sanitizeErrorText(rf.Error)
				truncE := truncateUTF8Bytes(cleanE, fErrBytes)
				if truncP != cleanP || truncE != cleanE {
					wasTruncated = true
				}
				files = append(files, toolFileState{
					Path:         truncP,
					PublishState: rf.PublishState,
					Published:    rf.Published,
					RolledBack:   rf.RolledBack,
					Wrote:        rf.Wrote,
					Error:        truncE,
				})
			}
			payload["files"] = files
			if omitted := len(receipt.Files) - fLimit; omitted > 0 {
				payload["filesOmitted"] = omitted
				wasTruncated = true
			}
		} else if len(receipt.Files) > 0 {
			payload["filesOmitted"] = len(receipt.Files)
			wasTruncated = true
		}
		if len(result.Files) > 0 && pLimit > 0 {
			pub := make([]editing.FileResult, 0, pLimit)
			for _, pf := range result.Files[:pLimit] {
				cleanP := s.sanitizeStructuredPath(pf.Path)
				pf.Path = truncateUTF8Bytes(cleanP, pBytes)
				cleanE := s.sanitizeErrorText(pf.Error)
				pf.Error = truncateUTF8Bytes(cleanE, fErrBytes)
				if pf.Path != cleanP || pf.Error != cleanE {
					wasTruncated = true
				}
				pub = append(pub, pf)
			}
			payload["publishedFiles"] = pub
			if omitted := len(result.Files) - pLimit; omitted > 0 {
				payload["publishedFilesOmitted"] = omitted
				wasTruncated = true
			}
		} else if len(result.Files) > 0 {
			payload["publishedFilesOmitted"] = len(result.Files)
			wasTruncated = true
		}
		if wasTruncated {
			payload["truncated"] = true
		}
		raw, _ := json.Marshal(payload)
		return raw
	}

	raw := build(errLimit, pathLimit, fileErrLimit, diagLimit, diagTextLimit, fileLimit, pubLimit)
	for len(raw) > maxPartialPayloadBytes {
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
		case pathLimit > 64:
			pathLimit /= 2
		case fileLimit > 0:
			fileLimit /= 2
		case errLimit > 64:
			errLimit = 64
		default:
			// Safe explicit overflow envelope when extreme JSON/Unicode escaping prevents optional
			// fields from fitting within maxPartialPayloadBytes (ADR-0024 R5-03).
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
				overflow["partialPublication"] = partialPublicationFlag(receipt, dirty)
			}
			if len(receipt.Files) > 0 {
				overflow["filesOmitted"] = len(receipt.Files)
			}
			if len(result.Files) > 0 {
				overflow["publishedFilesOmitted"] = len(result.Files)
			}
			fallbackRaw, _ := json.Marshal(overflow)
			return string(fallbackRaw)
		}
		raw = build(errLimit, pathLimit, fileErrLimit, diagLimit, diagTextLimit, fileLimit, pubLimit)
	}
	return string(raw)
}

// editFailure renders a refusal as a tool result the model can act on: the full sanitized error
// (preserving any joined rollback errors), its stable code, remediation, and bounded per-file
// publication state. MutatesWorkspace stays false except where the engine entered publication or
// cannot rule out a write.
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
	return protocol.ToolResult{OK: false, Content: content, MutatesWorkspace: dirty}, err
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
