package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

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

func receiptDirtyOrPublished(r editing.CommitReceipt) bool {
	if r.DirtyFailure {
		return true
	}
	for _, f := range r.Files {
		if f.Published || f.Wrote || f.PublishState == editing.PublishUncertain || f.PublishState == editing.PublishRolledBack {
			return true
		}
	}
	return false
}

// editFailure renders a refusal as a tool result the model can act on: the error, its stable code, and
// the remediation. MutatesWorkspace stays false except where the engine entered publication or cannot
// rule out a write.
func editFailure(result editing.Result, receipt editing.CommitReceipt, err error) protocol.ToolResult {
	dirty := receiptDirtyOrPublished(receipt)
	editErr, ok := editing.AsError(err)
	if !ok {
		payload := map[string]any{"error": err.Error()}
		if len(result.Files) > 0 {
			payload["publishedFiles"] = result.Files
		}
		if receipt.State != "" {
			payload["commitState"] = receipt.State
		}
		content, _ := json.Marshal(payload)
		return protocol.ToolResult{OK: false, Content: string(content), MutatesWorkspace: dirty}
	}
	payload := map[string]any{
		"error": editErr.Error(), "code": editErr.Code, "hint": editErr.Hint(),
		"path": editErr.Path, "currentRev": editErr.CurrentRev, "holder": editErr.Holder,
		"diagnostics": editErr.Diagnostics,
	}
	if len(result.Files) > 0 {
		payload["publishedFiles"] = result.Files
	}
	if receipt.State != "" && dirty {
		payload["commitState"] = receipt.State
	}
	content, _ := json.Marshal(payload)
	return protocol.ToolResult{OK: false, Content: string(content),
		MutatesWorkspace: dirty || editErr.Code == editing.CodeConcurrentWrite}
}

// editOutcome is how a tool returns a pre-commit failure: a refusal the model can act on (an editing.Error)
// becomes an OK:false result carrying its code and hint; anything else — a path guard refusal, an I/O
// error — stays an error, so the caller's contract is unchanged.
func editOutcome(err error) (protocol.ToolResult, error) {
	return commitOutcome(editing.Result{}, editing.CommitReceipt{}, err)
}

func commitOutcome(result editing.Result, receipt editing.CommitReceipt, err error) (protocol.ToolResult, error) {
	dirty := receiptDirtyOrPublished(receipt)
	if _, ok := editing.AsError(err); ok {
		return editFailure(result, receipt, err), nil
	}
	var content string
	if len(result.Files) > 0 {
		if raw, mErr := json.Marshal(map[string]any{
			"error":          err.Error(),
			"commitState":    receipt.State,
			"publishedFiles": result.Files,
		}); mErr == nil {
			content = string(raw)
		}
	}
	return protocol.ToolResult{Content: content, MutatesWorkspace: dirty}, err
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
