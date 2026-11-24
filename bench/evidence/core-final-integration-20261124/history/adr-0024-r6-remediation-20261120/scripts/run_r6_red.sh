#!/usr/bin/env bash
set -uo pipefail

export PATH="/Users/tangxingpeng/.local/share/mise/installs/node/24/bin:/opt/homebrew/bin:/usr/local/go/bin:/usr/local/bin:$PATH"

EVID_DIR="/Users/tangxingpeng/IdeaProjects/me/LumenCortex/bench/evidence/adr-0024-r6-remediation-20261120"
PROBES_DIR="${EVID_DIR}/independent-r5-probes"
LOG_DIR="${EVID_DIR}/logs"
mkdir -p "${LOG_DIR}"

MAIN_WT="/Users/tangxingpeng/IdeaProjects/me/LumenCortex/LumenCortex-Core"
R4_WT="/Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-r4-20261120"
R6_WT="/Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-r6-20261120"

TMP_OV="$(mktemp -d "${TMPDIR:-/tmp}/lc-r6-red-ov.XXXXXX")"
trap 'rm -rf "${TMP_OV}"' EXIT

# ---------------------------------------------------------------------------
# 1. R5-01 RED + main/R4 comparison (exact archived independent probe)
# ---------------------------------------------------------------------------
{
  echo "=== R6 PHASE A: R5-01 MANDATORY RED REPRODUCTION + MAIN/R4 COMPARISON ==="
  echo "timestamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "platform=$(uname -s)/$(uname -m)"
  echo "go_version=$(go version)"

  # 1a. main comparison (expect foreign_lost=false -> exit 0)
  python3 - "${MAIN_WT}" "${PROBES_DIR}" "${TMP_OV}/ordering_main.json" <<'PY'
import json, os, sys
wt, probes, out = sys.argv[1], sys.argv[2], sys.argv[3]
stub = os.path.join(os.path.dirname(out), "stub_main_vars_test.go")
with open(stub, "w") as f:
    f.write("//go:build !windows\npackage editing\nimport \"os\"\nvar auditR5AfterRestoreReadback func(string, *os.File) error\n")
rep = {
    os.path.join(wt, "internal/editing/engine.go"): os.path.join(probes, "lc-r5-final-audit.OHWLHa/probes/main_engine_overlay.go"),
    os.path.join(wt, "internal/editing/zz_independent_r5_ordering_test.go"): os.path.join(probes, "lc-r5-final-audit.OHWLHa/probes/zz_independent_r5_ordering_test.go"),
    os.path.join(wt, "internal/editing/zz_stub_vars_test.go"): stub,
}
with open(out, "w") as f:
    json.dump({"Replace": rep}, f)
PY
  echo "--- [comparison: main@$(git -C "${MAIN_WT}" rev-parse HEAD)] ---"
  (cd "${MAIN_WT}" && LC_R5_EXPECT_FOREIGN_LOST=false go test -overlay "${TMP_OV}/ordering_main.json" -v -count=1 -run '^TestIndependentR5AppendAfterRestoreWrite$' ./internal/editing)
  echo "main_exit=$?"

  # 1b. R4 comparison (expect foreign_lost=false -> exit 0)
  python3 - "${R4_WT}" "${PROBES_DIR}" "${TMP_OV}/ordering_r4.json" <<'PY'
import json, os, sys
wt, probes, out = sys.argv[1], sys.argv[2], sys.argv[3]
stub = os.path.join(os.path.dirname(out), "stub_r4_vars_test.go")
with open(stub, "w") as f:
    f.write("//go:build !windows\npackage editing\nimport \"os\"\nvar auditR5AfterRestoreReadback func(string, *os.File) error\n")
rep = {
    os.path.join(wt, "internal/editing/engine.go"): os.path.join(probes, "lc-r5-final-audit.OHWLHa/probes/base_engine_overlay.go"),
    os.path.join(wt, "internal/editing/zz_independent_r5_ordering_test.go"): os.path.join(probes, "lc-r5-final-audit.OHWLHa/probes/zz_independent_r5_ordering_test.go"),
    os.path.join(wt, "internal/editing/zz_stub_vars_test.go"): stub,
}
with open(out, "w") as f:
    json.dump({"Replace": rep}, f)
PY
  echo "--- [comparison: R4@$(git -C "${R4_WT}" rev-parse HEAD) dirty_files=$(git -C "${R4_WT}" status --porcelain | wc -l | tr -d ' ')] ---"
  (cd "${R4_WT}" && LC_R5_EXPECT_FOREIGN_LOST=false go test -overlay "${TMP_OV}/ordering_r4.json" -v -count=1 -run '^TestIndependentR5AppendAfterRestoreWrite$' ./internal/editing)
  echo "r4_exit=$?"

  # 1c. Unmodified R5 RED (expect foreign_lost=false -> MUST FAIL with exit=1!)
  python3 - "${R6_WT}" "${PROBES_DIR}" "${TMP_OV}/ordering_r5_red.json" <<'PY'
import json, os, sys
wt, probes, out = sys.argv[1], sys.argv[2], sys.argv[3]
rep = {
    os.path.join(wt, "internal/editing/engine.go"): os.path.join(probes, "lc-r5-final-audit.OHWLHa/probes/engine_overlay.go"),
    os.path.join(wt, "internal/editing/zz_independent_r5_ordering_test.go"): os.path.join(probes, "lc-r5-final-audit.OHWLHa/probes/zz_independent_r5_ordering_test.go"),
    os.path.join(wt, "internal/editing/zz_independent_r5_races_test.go"): os.path.join(probes, "lc-r5-final-audit.OHWLHa/probes/zz_independent_r5_races_test.go"),
}
with open(out, "w") as f:
    json.dump({"Replace": rep}, f)
PY
  echo "--- [RED: R5@$(git -C "${R6_WT}" rev-parse HEAD) dirty_files=$(git -C "${R6_WT}" status --porcelain | wc -l | tr -d ' ')] ---"
  echo "cmd=LC_R5_EXPECT_FOREIGN_LOST=false go test -overlay <tmp>/ordering_r5_red.json -v -count=1 -run '^TestIndependentR5AppendAfterRestoreWrite$' ./internal/editing"
  (cd "${R6_WT}" && LC_R5_EXPECT_FOREIGN_LOST=false go test -overlay "${TMP_OV}/ordering_r5_red.json" -v -count=1 -run '^TestIndependentR5AppendAfterRestoreWrite$' ./internal/editing)
  r5_exit=$?
  echo "exit=${r5_exit}"
} > "${LOG_DIR}/r6-r501-red.log" 2>&1

# ---------------------------------------------------------------------------
# 2. R5-02 .. R5-05 RED on unmodified R5 HEAD (dirty_files=0)
# ---------------------------------------------------------------------------
cat > "${TMP_OV}/red_r502_agent_test.go" <<'GO'
package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/editing"
	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/protocol"
)

type r6RedApplyTools struct {
	inner   *toolset.Set
	origin  string
	padding int
}

func (t *r6RedApplyTools) Specs(allow []string) []protocol.ToolSpec {
	return append(t.inner.Specs(allow),
		protocol.ToolSpec{Name: "apply_experience", Description: "probe", Permission: "read", MutatesWorkspace: true, Parameters: map[string]any{"type": "object", "properties": map[string]any{}}})
}

func (t *r6RedApplyTools) Execute(ctx context.Context, name string, args map[string]any, emit func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	if name == "apply_experience" {
		cb, ok := protocol.ToolCallbackFromContext(ctx)
		if !ok || cb == nil {
			return protocol.ToolResult{}, errors.New("missing tool callback")
		}
		nested, _ := cb.Call(ctx, "write_file", map[string]any{"path": "applied.txt", "content": "applied\n"})
		results := []protocol.ToolResult{nested, {OK: true, Content: strings.Repeat("r", t.padding)}}
		raw, _ := json.Marshal(map[string]any{
			"applicationId":  "app-1",
			"state":          "unknown",
			"results":        results,
			"sourceSessions": []string{t.origin},
			"error":          "nested action failed",
		})
		return protocol.ToolResult{OK: false, MutatesWorkspace: nested.MutatesWorkspace, Content: string(raw)}, nil
	}
	return t.inner.Execute(ctx, name, args, emit)
}

func TestR6RedR502ApplyExperienceInvalidJSONAndLostEdge(t *testing.T) {
	const ref = config.ModelRef("test/m")
	ctx := context.Background()
	sid, origin := "s-zz-f1", "s-zz-origin"
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	initGitRepoForTest(t, ws)
	dbPath := filepath.Join(t.TempDir(), "f1.db")
	raw, err := session.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Create(ctx, session.Session{ID: origin, Goal: "origin", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	if err := raw.Create(ctx, session.Session{ID: sid, Goal: "consumer", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := raw.BeginRun(ctx, sid, session.RunStart{RunID: "run-seed", Input: &protocol.Message{Role: "user", Content: "go"}}); err != nil {
		t.Fatal(err)
	}
	st := r401SettleStore{realSQLiteAgentStore{s: raw}}
	if _, _, err := st.AppendMessage(ctx, sid, protocol.Message{Role: "assistant", Content: "done"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AppendMessage(ctx, sid, protocol.Message{Role: "user", Origin: protocol.OriginCompletionBlocked, Content: "Completion refused: verify outputs."}, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := raw.BeginRun(ctx, sid, session.RunStart{RunID: "run-live"}); err != nil {
		t.Fatal(err)
	}
	eng, err := editing.New(editing.Options{Root: ws})
	if err != nil {
		t.Fatal(err)
	}
	eng.SetPublishFaultHooks(editing.PublishFaultHooks{CloseAfterPublish: func(string) error { return syscall.EIO }})
	ts, err := toolset.New(toolset.Options{Workspace: ws, Policy: toolset.PolicyFull, SessionID: sid, Editing: eng})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := repository.New(ws)
	if err != nil {
		t.Fatal(err)
	}
	prov := &scriptedProvider{responses: []protocol.ProviderResponse{
		toolStep(call("tc-apply", "apply_experience", `{}`)),
		answerOf("done"),
	}}
	if _, err := (&Loop{
		Models:    map[config.ModelRef]Model{ref: testModel(ref, prov)},
		Tools:     &r6RedApplyTools{inner: ts, origin: origin, padding: 8500},
		Store:     st,
		Snapshots: repo,
	}).Run(ctx, sid, Options{Session: ref, RunID: "run-live", Resumed: true, MaxSteps: 6}); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	db, err := sql.Open("sqlite3", filepath.Join(dbPath, "lumencortex.db"))
	if err != nil {
		t.Fatal(err)
	}
	var applyContent string
	_ = db.QueryRow(`SELECT json_extract(json,'$.content') FROM session_messages WHERE session_id=? AND role='tool' AND json_extract(json,'$.tool_call_id')='tc-apply'`, sid).Scan(&applyContent)
	_ = db.Close()

	validJSON := json.Valid([]byte(applyContent))
	cold, err := session.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cold.Close()
	rev, err := cold.RevokeSession(ctx, origin, "probe_revoke")
	if err != nil {
		t.Fatal(err)
	}
	consumerReached := false
	for _, s := range rev.Sessions {
		if s == sid {
			consumerReached = true
		}
	}
	t.Logf("R5-02 observed: bytes=%d valid_json=%v consumer_reached=%v", len(applyContent), validJSON, consumerReached)
	if !validJSON || !consumerReached {
		t.Fatalf("R5-02 RED: valid_json=%v consumer_reached=%v", validJSON, consumerReached)
	}
}
GO

cat > "${TMP_OV}/red_r503_05_toolset_test.go" <<'GO'
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

func TestR6RedR503ToR505(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "p.go"), []byte("package p\n\nvar x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng, err := editing.New(editing.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	set, err := New(Options{Workspace: root, Policy: PolicyWorkspace, Editing: eng, SessionID: "s-red"})
	if err != nil {
		t.Fatal(err)
	}

	sibPath := root + "-sibling/secret.txt"
	sanitized := set.sanitizeErrorText("failed opening " + sibPath)
	corruptedSibling := strings.Contains(sanitized, ".-sibling/secret.txt")
	t.Logf("R5-05 observed: input=%q sanitized=%q corrupted_sibling=%v", sibPath, sanitized, corruptedSibling)

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
	res, err := set.Execute(ctx, "edit_lines", map[string]any{
		"path": "p.go",
		"rev":  rev,
		"ops":  []map[string]any{{"op": "replace", "start": anchor, "text": b.String()}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	valid := json.Unmarshal([]byte(res.Content), &obj) == nil
	containsRoot := strings.Contains(res.Content, root)
	t.Logf("R5-03/04 observed: len=%d bound=%d valid=%v contains_root=%v", len(res.Content), maxPartialPayloadBytes, valid, containsRoot)
	if len(res.Content) > maxPartialPayloadBytes || containsRoot || corruptedSibling {
		t.Fatalf("R5-03/04/05 RED: len=%d bound=%d contains_root=%v corrupted_sibling=%v",
			len(res.Content), maxPartialPayloadBytes, containsRoot, corruptedSibling)
	}
}
GO

python3 - "${R6_WT}" "${TMP_OV}" "${TMP_OV}/r502_05_red.json" <<'PY'
import json, os, sys
wt, ovdir, out = sys.argv[1], sys.argv[2], sys.argv[3]
rep = {
    os.path.join(wt, "internal/agent/zz_r6_red_r502_test.go"): os.path.join(ovdir, "red_r502_agent_test.go"),
    os.path.join(wt, "internal/toolset/zz_r6_red_r503_05_test.go"): os.path.join(ovdir, "red_r503_05_toolset_test.go"),
}
with open(out, "w") as f:
    json.dump({"Replace": rep}, f)
PY

{
  echo "=== R6 PHASE A: R5-02 .. R5-05 RED REPRODUCTION ==="
  echo "timestamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "commit=$(git -C "${R6_WT}" rev-parse HEAD)"
  echo "dirty_files=$(git -C "${R6_WT}" status --porcelain | wc -l | tr -d ' ')"
  echo "cmd=go test -overlay <tmp>/r502_05_red.json -v -count=1 -run '^TestR6Red' ./internal/agent ./internal/toolset"
  (cd "${R6_WT}" && go test -overlay "${TMP_OV}/r502_05_red.json" -v -count=1 -run '^TestR6Red' ./internal/agent ./internal/toolset)
  echo "exit=$?"
} > "${LOG_DIR}/r6-r502-05-red.log" 2>&1

echo "=== SUMMARY ==="
cat "${LOG_DIR}/r6-r501-red.log"
echo "---"
cat "${LOG_DIR}/r6-r502-05-red.log"
