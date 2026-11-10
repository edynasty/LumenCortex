package editing

import (
	"errors"
	"fmt"
	"strings"
)

// Error codes an edit can fail with. A tool turns one into a machine-readable code and a hint the
// model can act on; nothing is written when one is returned.
const (
	// CodeInvalid: the request itself is malformed.
	CodeInvalid = "invalid"
	// CodeUnknownRev: the revision was never read in this session; read the file first.
	CodeUnknownRev = "unknown_rev"
	// CodeStale: the file moved on since the revision the edit was computed against.
	CodeStale = "stale"
	// CodeConflict: the file changed between validation and the write (compare-and-swap refused it).
	CodeConflict = "conflict"
	// CodeLeaseConflict: another holder's write lease covers the path.
	CodeLeaseConflict = "lease_conflict"
	// CodeNotUTF8: the file or the result is not UTF-8; the engine never writes such a file.
	CodeNotUTF8 = "not_utf8"
	// CodeTooLarge: the file or the result is over MaxFileBytes.
	CodeTooLarge = "too_large"
	// CodeConcurrentWrite: a write landed while the engine was writing; the file is not what it wrote.
	CodeConcurrentWrite = "concurrent_write_detected"
	// CodeSyntax and CodeType: validation refused the result.
	CodeSyntax = "syntax"
	CodeType   = "type"
	// CodeOverlap: two operations of one plan address the same lines.
	CodeOverlap = "overlap"
	// CodeAnchorMismatch: an anchor's line hash is not the line at that number.
	CodeAnchorMismatch = "anchor_mismatch"
	// CodeNotSeen: the anchored line was never shown to this session.
	CodeNotSeen = "not_seen"
	// CodeNotFound and CodeExists: the file the change names is in the wrong state.
	CodeNotFound = "not_found"
	CodeExists   = "exists"
	// CodeUnknownSymbol and CodeAmbiguous: AST symbol resolution failed.
	CodeUnknownSymbol = "unknown_symbol"
	CodeAmbiguous     = "ambiguous"
	// CodeTypeCheckDegraded is a diagnostic, not a refusal: the package's dependencies could not be
	// imported, so only syntax was validated.
	CodeTypeCheckDegraded = "type_check_degraded"
)

// Diagnostic is one validation finding about a planned file.
type Diagnostic struct {
	// Severity is SeverityError or SeverityWarning.
	Severity string `json:"severity"`
	// Code is the error code the finding maps to when it refuses the plan.
	Code string `json:"code,omitempty"`
	// Path is the file the finding is about, as the tool named it when possible.
	Path string `json:"path,omitempty"`
	// Line and Column are 1-based.
	Line   int    `json:"line,omitempty"`
	Column int    `json:"column,omitempty"`
	Text   string `json:"message"`
}

// Severity levels of a Diagnostic.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
)

// Error is an edit refusal. Code is stable; Detail and the hint are for the model.
type Error struct {
	Code        string
	Path        string
	Rev         string
	CurrentRev  string
	Holder      string
	Detail      string
	Diagnostics []Diagnostic
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Code)
	if e.Path != "" {
		b.WriteString(" " + e.Path)
	}
	if e.Detail != "" {
		b.WriteString(": " + e.Detail)
	}
	if e.CurrentRev != "" {
		fmt.Fprintf(&b, " (current rev %s)", e.CurrentRev)
	}
	if e.Holder != "" && e.Code == CodeLeaseConflict {
		b.WriteString(" (held by " + e.Holder + ")")
	}
	if hint := e.Hint(); hint != "" {
		b.WriteString("; " + hint)
	}
	return b.String()
}

// Hint is the remediation a model should take for this refusal.
func (e *Error) Hint() string {
	switch e.Code {
	case CodeUnknownRev:
		return "read the file with read_file anchors=true, then edit that revision"
	case CodeStale:
		return "re-read the file with read_file anchors=true and redo the edit against the new revision"
	case CodeConflict:
		return "something else wrote the file; re-read it and redo the edit"
	case CodeConcurrentWrite:
		return "another writer changed the file during the write; re-read it before editing again"
	case CodeLeaseConflict:
		return "another agent holds the write lease; wait for it to be released, or ask its holder to release it"
	case CodeAnchorMismatch:
		return "the anchor does not name the line you think it does; re-read the file with anchors=true"
	case CodeNotSeen:
		return "read the lines you want to edit first, with read_file start_line/end_line anchors=true"
	case CodeOverlap:
		return "address each line in at most one operation"
	case CodeNotUTF8:
		return "the file is not UTF-8; edit it with another tool"
	case CodeTooLarge:
		return "the file is over the 4 MiB edit limit"
	case CodeSyntax, CodeType:
		return "fix the edit; nothing was written"
	case CodeUnknownSymbol, CodeAmbiguous:
		return "list the symbol's candidates and name exactly one"
	}
	return ""
}

// AsError returns the edit refusal in err, if there is one.
func AsError(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

// CodeOf returns an edit's error code, or "" for a non-edit error.
func CodeOf(err error) string {
	if e, ok := AsError(err); ok {
		return e.Code
	}
	return ""
}

// Errorf builds a refusal.
func Errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}
