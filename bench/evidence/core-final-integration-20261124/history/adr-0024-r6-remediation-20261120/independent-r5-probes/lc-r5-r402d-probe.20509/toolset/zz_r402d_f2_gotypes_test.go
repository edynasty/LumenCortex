package toolset

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/editing"
)

// Real edit_lines on a .go file: syntactically valid, many type errors (GoTypes collects all via
// types.Config.Error). Measures the tool-result Content bytes against maxPartialPayloadBytes.
func TestZZR402DF2GoTypesReachableBound(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "p.go"), []byte("package p\n\nvar x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng, err := editing.New(editing.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	set, err := New(Options{Workspace: root, Policy: PolicyWorkspace, Editing: eng, SessionID: "s-zz-f2"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rd, err := set.Execute(ctx, "read_file", map[string]any{"path": "p.go", "anchors": true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rev := regexp.MustCompile(`rev=(\S+)`).FindStringSubmatch(rd.Content)[1]
	anchor := regexp.MustCompile(`(?m)^(3:[^|]+)\|`).FindStringSubmatch(rd.Content)[1]
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString("var _ int = \"s\"\n")
	}
	res, execErr := set.Execute(ctx, "edit_lines", map[string]any{"path": "p.go", "rev": rev, "ops": []map[string]any{{"op": "replace", "start": anchor, "text": b.String()}}}, nil)
	var obj map[string]any
	valid := json.Unmarshal([]byte(res.Content), &obj) == nil
	diags, _ := obj["diagnostics"].([]any)
	t.Logf("PROBE F2 ok=%v execErr=%v len=%d bound=%d valid=%v code=%v commitState=%v diagnostics=%d contains_root=%v", res.OK, execErr, len(res.Content), maxPartialPayloadBytes, valid, obj["code"], obj["commitState"], len(diags), strings.Contains(res.Content, root))
	disk, _ := os.ReadFile(filepath.Join(root, "p.go"))
	t.Logf("PROBE F2 disk_unchanged=%v", string(disk) == "package p\n\nvar x = 1\n")
}
