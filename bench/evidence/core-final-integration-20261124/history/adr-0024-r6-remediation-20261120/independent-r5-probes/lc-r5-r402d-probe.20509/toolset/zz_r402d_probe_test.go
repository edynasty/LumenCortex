package toolset

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/editing"
)

// HEAD-only probe of formatCommitFailurePayload bound and sanitizer.
func TestZZR402DProbePayloadBound(t *testing.T) {
	root := t.TempDir()
	set, err := New(Options{Workspace: root, Policy: PolicyWorkspace, SessionID: "s-zz"})
	if err != nil {
		t.Fatal(err)
	}
	// A. Dirty CodeConcurrentWrite whose Path needs JSON escaping ('<' -> \u003c, 6 bytes).
	p := strings.Repeat("<", 900) + ".txt"
	primary := &editing.Error{Code: editing.CodeConcurrentWrite, Path: p, Detail: "changed"}
	receipt := editing.CommitReceipt{State: editing.CommitStatePartiallyPublished, DirtyFailure: true,
		Files: []editing.FileCommitReceipt{{Path: p, PublishState: editing.PublishExternalModification, Wrote: true, Published: true}}}
	res, outErr := set.commitOutcome(editing.Result{}, receipt, primary)
	var obj map[string]any
	t.Logf("PROBE A len=%d bound=%d valid=%v outErr=%v filesOmitted=%v", len(res.Content), maxPartialPayloadBytes, json.Unmarshal([]byte(res.Content), &obj) == nil, outErr, obj["filesOmitted"])
	// B. Pre-publish validation editing.Error with many diagnostics carrying absolute paths.
	diags := make([]editing.Diagnostic, 100)
	for i := range diags {
		diags[i] = editing.Diagnostic{Severity: editing.SeverityError, Code: editing.CodeSyntax, Path: filepath.Join(root, "p.go"), Line: i + 1, Text: "expected ')'"}
	}
	verr := &editing.Error{Code: editing.CodeSyntax, Path: filepath.Join(root, "p.go"), Detail: "expected ')'", Diagnostics: diags}
	res2, _ := set.commitOutcome(editing.Result{}, editing.CommitReceipt{State: editing.CommitStatePrePublishFailure}, verr)
	t.Logf("PROBE B len=%d contains_root=%v errorField_contains_root=%v", len(res2.Content), strings.Contains(res2.Content, root), func() bool { var o map[string]any; _ = json.Unmarshal([]byte(res2.Content), &o); s, _ := o["error"].(string); return strings.Contains(s, root) }())
	// C. sanitizer prefix collision.
	t.Logf("PROBE C sanitize=%q", set.sanitizeErrorText(fmt.Sprintf("open %s-sibling/secret: denied; open %s/x: e", root, root)))
	// D. non-editing error, dirty, many files -> files dropped to 0?
	files := make([]editing.FileCommitReceipt, 40)
	for i := range files {
		files[i] = editing.FileCommitReceipt{Path: fmt.Sprintf("dir/%s%02d.txt", strings.Repeat("f", 120), i), PublishState: editing.PublishRollbackFailed, Wrote: true, Error: strings.Repeat("e", 300)}
	}
	files[39].PublishState = editing.PublishExternalModification
	res3, err3 := set.commitOutcome(editing.Result{}, editing.CommitReceipt{State: editing.CommitStatePartiallyPublished, DirtyFailure: true, Files: files}, errors.New("rollback failed"))
	var o3 map[string]any
	_ = json.Unmarshal([]byte(res3.Content), &o3)
	fl, _ := o3["files"].([]any)
	t.Logf("PROBE D len=%d files=%d filesOmitted=%v err=%v mutates=%v", len(res3.Content), len(fl), o3["filesOmitted"], err3, res3.MutatesWorkspace)
}
