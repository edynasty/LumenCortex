package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
	maxErrorBytesInToolResult   = 1024
	maxFileErrorBytesInResult   = 256
	maxPartialPayloadBytes      = 4096
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
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	const ellipsis = "…"
	if maxBytes <= len(ellipsis) {
		return s[:maxBytes]
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

func (s *Set) sanitizeErrorText(msg string) string {
	if s == nil || msg == "" {
		return msg
	}
	sep := string(filepath.Separator)
	roots := []string{s.realWorkspace, s.workspace}
	for _, root := range roots {
		if root == "" {
			continue
		}
		clean := filepath.Clean(root)
		msg = strings.ReplaceAll(msg, clean+sep, "")
		msg = strings.ReplaceAll(msg, filepath.ToSlash(clean)+"/", "")
		msg = strings.ReplaceAll(msg, clean, ".")
	}
	return msg
}

func (s *Set) formatCommitFailurePayload(result editing.Result, receipt editing.CommitReceipt, err error, editErr *editing.Error) string {
	dirty := receiptDirtyOrPublished(receipt)
	errLimit := maxErrorBytesInToolResult
	fileLimit := len(receipt.Files)
	if fileLimit > maxPartialFilesInToolResult {
		fileLimit = maxPartialFilesInToolResult
	}
	pubLimit := len(result.Files)
	if pubLimit > maxPartialFilesInToolResult {
		pubLimit = maxPartialFilesInToolResult
	}

	build := func(errBytes, fLimit, pLimit int) []byte {
		payload := map[string]any{
			"error": truncateUTF8Bytes(s.sanitizeErrorText(err.Error()), errBytes),
		}
		if editErr != nil {
			payload["code"] = editErr.Code
			payload["hint"] = editErr.Hint()
			payload["path"] = editErr.Path
			payload["currentRev"] = editErr.CurrentRev
			payload["holder"] = editErr.Holder
			payload["diagnostics"] = editErr.Diagnostics
		}
		if dirty || receipt.State != "" {
			state := receipt.State
			if state == "" {
				if dirty {
					state = editing.CommitStatePublicationUncertain
				} else {
					state = editing.CommitStatePrePublishFailure
				}
			}
			payload["commitState"] = state
			payload["dirtyFailure"] = dirty
			payload["partialPublication"] = partialPublicationFlag(receipt, dirty)
		}
		if len(receipt.Files) > 0 && fLimit > 0 {
			files := make([]toolFileState, 0, fLimit)
			for _, rf := range receipt.Files[:fLimit] {
				files = append(files, toolFileState{
					Path:         filepath.ToSlash(rf.Path),
					PublishState: rf.PublishState,
					Published:    rf.Published,
					RolledBack:   rf.RolledBack,
					Wrote:        rf.Wrote,
					Error:        truncateUTF8Bytes(s.sanitizeErrorText(rf.Error), maxFileErrorBytesInResult),
				})
			}
			payload["files"] = files
			if omitted := len(receipt.Files) - fLimit; omitted > 0 {
				payload["filesOmitted"] = omitted
			}
		} else if len(receipt.Files) > 0 {
			payload["filesOmitted"] = len(receipt.Files)
		}
		if len(result.Files) > 0 && pLimit > 0 {
			pub := make([]editing.FileResult, 0, pLimit)
			for _, pf := range result.Files[:pLimit] {
				pf.Path = filepath.ToSlash(pf.Path)
				pf.Error = truncateUTF8Bytes(s.sanitizeErrorText(pf.Error), maxFileErrorBytesInResult)
				pub = append(pub, pf)
			}
			payload["publishedFiles"] = pub
			if omitted := len(result.Files) - pLimit; omitted > 0 {
				payload["publishedFilesOmitted"] = omitted
			}
		} else if len(result.Files) > 0 {
			payload["publishedFilesOmitted"] = len(result.Files)
		}
		raw, _ := json.Marshal(payload)
		return raw
	}

	raw := build(errLimit, fileLimit, pubLimit)
	for len(raw) > maxPartialPayloadBytes && (fileLimit > 0 || pubLimit > 0 || errLimit > 256) {
		switch {
		case pubLimit > 0:
			pubLimit /= 2
		case fileLimit > 0:
			fileLimit /= 2
		default:
			errLimit /= 2
		}
		raw = build(errLimit, fileLimit, pubLimit)
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

// editResult renders a committed plan.
func editResult(result editing.Result, receipt editing.CommitReceipt, extra map[string]any) protocol.ToolResult {
	payload := map[string]any{"files": result.Files, "diagnostics": result.Diagnostics}
	for key, value := range extra {
		payload[key] = value
	}
	content, err := json.Marshal(payload)
	if err != nil {
		return protocol.ToolResult{OK: false, Content: fmt.Sprintf(`{"error":%q}`, err.Error()), MutatesWorkspace: receiptWrote(receipt)}
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
