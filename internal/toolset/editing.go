package toolset

import (
	"encoding/json"
	"fmt"
	"os"

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
		flags |= os.O_CREATE
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
		f.Close()
		return nil, err
	}
	return f, nil
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

// editFailure renders a refusal as a tool result the model can act on: the error, its stable code, and
// the remediation. MutatesWorkspace stays false except where the engine cannot rule out a write.
func editFailure(err error) protocol.ToolResult {
	editErr, ok := editing.AsError(err)
	if !ok {
		content, _ := json.Marshal(map[string]any{"error": err.Error()})
		return protocol.ToolResult{OK: false, Content: string(content)}
	}
	content, _ := json.Marshal(map[string]any{
		"error": editErr.Error(), "code": editErr.Code, "hint": editErr.Hint(),
		"path": editErr.Path, "currentRev": editErr.CurrentRev, "holder": editErr.Holder,
		"diagnostics": editErr.Diagnostics,
	})
	return protocol.ToolResult{OK: false, Content: string(content),
		MutatesWorkspace: editErr.Code == editing.CodeConcurrentWrite}
}

// editOutcome is how a tool returns a commit failure: a refusal the model can act on (an editing.Error)
// becomes an OK:false result carrying its code and hint; anything else — a path guard refusal, an I/O
// error — stays an error, so the caller's contract is unchanged.
func editOutcome(err error) (protocol.ToolResult, error) {
	if _, ok := editing.AsError(err); ok {
		return editFailure(err), nil
	}
	return protocol.ToolResult{}, err
}

// editResult renders a committed plan.
func editResult(result editing.Result, extra map[string]any) protocol.ToolResult {
	payload := map[string]any{"files": result.Files, "diagnostics": result.Diagnostics}
	for key, value := range extra {
		payload[key] = value
	}
	content, err := json.Marshal(payload)
	if err != nil {
		return protocol.ToolResult{OK: false, Content: fmt.Sprintf(`{"error":%q}`, err.Error())}
	}
	return protocol.ToolResult{OK: true, Content: string(content), MutatesWorkspace: true}
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
