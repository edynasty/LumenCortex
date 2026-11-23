package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/editing"
	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/protocol"
)

func TestR6AgentExternalAppendHelper(t *testing.T) {
	p := os.Getenv("LC_R6_AGENT_APPEND_TARGET")
	if p == "" {
		return
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("FOREIGN\n"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

type r6ApplyExperienceTools struct {
	inner   *toolset.Set
	origin  string
	sources []string
	padding int
	okFlag  bool
}

func (t *r6ApplyExperienceTools) Specs(allow []string) []protocol.ToolSpec {
	return append(t.inner.Specs(allow),
		protocol.ToolSpec{
			Name:        "adopt_experience",
			Description: "test adopt_experience",
			Permission:  "read",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		},
		protocol.ToolSpec{
			Name:             "apply_experience",
			Description:      "test apply_experience",
			Permission:       "read",
			MutatesWorkspace: true,
			Parameters:       map[string]any{"type": "object", "properties": map[string]any{}},
		},
	)
}

func (t *r6ApplyExperienceTools) Execute(ctx context.Context, name string, args map[string]any, emit func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	switch name {
	case "adopt_experience":
		raw, _ := json.Marshal(map[string]any{
			"adoptionId":     "use-1",
			"sourceSessions": []string{t.origin},
		})
		return protocol.ToolResult{OK: true, Content: string(raw)}, nil
	case "apply_experience":
		cb, ok := protocol.ToolCallbackFromContext(ctx)
		if !ok || cb == nil {
			return protocol.ToolResult{}, errors.New("missing tool callback")
		}
		nested, _ := cb.Call(ctx, "write_file", map[string]any{"path": "applied.txt", "content": "applied\n"})
		results := []protocol.ToolResult{nested}
		if t.padding > 0 {
			results = append(results, protocol.ToolResult{OK: true, Content: strings.Repeat("r", t.padding)})
		}
		payload := map[string]any{
			"applicationId":  "app-1",
			"state":          "unknown",
			"results":        results,
			"sourceSessions": []string{t.origin},
		}
		if len(t.sources) != 0 {
			payload["sourceSessions"] = t.sources
		}
		if !t.okFlag {
			payload["error"] = "nested action failed"
		}
		raw, _ := json.Marshal(payload)
		return protocol.ToolResult{OK: t.okFlag, MutatesWorkspace: nested.MutatesWorkspace, Content: string(raw)}, nil
	}
	return t.inner.Execute(ctx, name, args, emit)
}

func TestR6NestedJSONPreservationAndProvenance(t *testing.T) {
	t.Run("unit_under_exact_and_over_8192_limit_plus_int64_and_provenance", func(t *testing.T) {
		note := "write_file commitState=published_with_post_error dirtyFailure=true partialPublication=true files=[a.txt:published_post_error]"

		// 1. Valid JSON under limit preserves exact 64-bit integer precision (RawMessage).
		under := `{"applicationId":"app-1","exactInt":9007199254740993,"sourceSessions":["s-origin-1","s-origin-2"]}`
		gotUnder := appendNestedPartialPublication(under, []string{note})
		if !json.Valid([]byte(gotUnder)) || len(gotUnder) > maxPreservedToolErrorBytes {
			t.Fatalf("under-limit invalid or oversized: len=%d valid=%v", len(gotUnder), json.Valid([]byte(gotUnder)))
		}
		if !strings.Contains(gotUnder, "9007199254740993") {
			t.Fatalf("64-bit integer precision lost in re-encoding: %s", gotUnder)
		}

		// 2. Payload producing EXACTLY 8192 bytes after attaching nestedPartialPublications.
		probeBase := func(padLen int) string {
			obj := map[string]any{
				"applicationId":  "app-exact",
				"sourceSessions": []string{"s-origin-exact"},
				"pad":            strings.Repeat("p", padLen),
			}
			b, _ := json.Marshal(obj)
			return appendNestedPartialPublication(string(b), []string{note})
		}
		sample := probeBase(100)
		neededPad := 100 + (maxPreservedToolErrorBytes - len(sample))
		exactOut := probeBase(neededPad)
		if len(exactOut) != maxPreservedToolErrorBytes {
			t.Fatalf("expected exact %d-byte boundary output, got len=%d", maxPreservedToolErrorBytes, len(exactOut))
		}
		if !json.Valid([]byte(exactOut)) {
			t.Fatalf("exact-boundary output is not valid JSON")
		}
		var exactParsed map[string]any
		if err := json.Unmarshal([]byte(exactOut), &exactParsed); err != nil {
			t.Fatal(err)
		}
		if exactParsed["truncated"] != nil || exactParsed["applicationId"] != "app-exact" {
			t.Fatalf("exact-boundary output should not truncate: %+v", exactParsed)
		}
		srcs, srcOK := resultSessions("apply_experience", "s-self", protocol.ToolResult{OK: false, Content: exactOut})
		if !srcOK || len(srcs) != 1 || srcs[0] != "s-origin-exact" {
			t.Fatalf("exact-boundary lost sourceSessions: srcs=%v ok=%v", srcs, srcOK)
		}

		// 3. Payload 1 byte over 8192 shrinks note/bulk without invalidating JSON or losing sourceSessions.
		overByOne := probeBase(neededPad + 1)
		if !json.Valid([]byte(overByOne)) || len(overByOne) > maxPreservedToolErrorBytes {
			t.Fatalf("over-by-one failed: len=%d valid=%v", len(overByOne), json.Valid([]byte(overByOne)))
		}
		srcsOver1, okOver1 := resultSessions("apply_experience", "s-self", protocol.ToolResult{OK: false, Content: overByOne})
		if !okOver1 || len(srcsOver1) != 1 || srcsOver1[0] != "s-origin-exact" {
			t.Fatalf("over-by-one lost sourceSessions: %v %v", srcsOver1, okOver1)
		}

		// 4. Massively oversized parent JSON (12 KB results) keeps valid JSON <= 8192 and preserves all provenance keys.
		hugeObj, _ := json.Marshal(map[string]any{
			"applicationId":        "app-huge",
			"adoptionId":           "use-huge",
			"sessionId":            "s-child",
			"maintenanceSessionId": "s-maint",
			"state":                "unknown",
			"sourceSessions":       []string{"s-origin-a", "s-origin-b"},
			"results":              strings.Repeat("R", 12000),
		})
		gotHuge := appendNestedPartialPublication(string(hugeObj), []string{note})
		if !json.Valid([]byte(gotHuge)) || !utf8.ValidString(gotHuge) || len(gotHuge) > maxPreservedToolErrorBytes {
			t.Fatalf("huge JSON failed: len=%d valid=%v", len(gotHuge), json.Valid([]byte(gotHuge)))
		}
		if strings.Contains(gotHuge, "[nested_partial_publication]") {
			t.Fatalf("R5-02 regression: plain text block appended to JSON: %s", gotHuge)
		}
		var hugeParsed map[string]any
		if err := json.Unmarshal([]byte(gotHuge), &hugeParsed); err != nil {
			t.Fatal(err)
		}
		if hugeParsed["truncated"] != true ||
			hugeParsed["applicationId"] != "app-huge" ||
			hugeParsed["adoptionId"] != "use-huge" ||
			hugeParsed["sessionId"] != "s-child" ||
			hugeParsed["maintenanceSessionId"] != "s-maint" ||
			hugeParsed["nestedPartialPublications"] == nil {
			t.Fatalf("missing required provenance or truncation indicator: %+v", hugeParsed)
		}
		srcsHuge, okHuge := resultSessions("apply_experience", "s-self", protocol.ToolResult{OK: false, Content: gotHuge})
		if !okHuge || len(srcsHuge) != 2 || srcsHuge[0] != "s-origin-a" || srcsHuge[1] != "s-origin-b" {
			t.Fatalf("huge JSON lost sourceSessions: %v %v", srcsHuge, okHuge)
		}

		// 5. JSON array input stays valid JSON.
		arrOut := appendNestedPartialPublication(`[1,2,3]`, []string{note})
		if !json.Valid([]byte(arrOut)) {
			t.Fatalf("JSON array became invalid JSON: %s", arrOut)
		}
	})

	t.Run("apply_experience_runtime_sqlite_resume_and_revocation_edges", func(t *testing.T) {
		cases := []struct {
			name       string
			adopt      bool
			seedOrigin bool
			padding    int
		}{
			{"apply_only_under_limit", false, true, 0},
			{"apply_only_8500pad_over_limit_seeded_origin", false, true, 8500},
			{"apply_only_8500pad_over_limit_unseeded_origin", false, false, 8500},
			{"adopt_then_apply_8500pad", true, true, 8500},
			{"protected_source_overflow", false, true, 0},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				const ref = config.ModelRef("test/m")
				ctx := context.Background()
				sid, origin := "s-r6-consumer", "s-r6-origin"
				var sources []string
				if tc.name == "protected_source_overflow" {
					for i := range session.MaxEdgesPerCommit {
						sources = append(sources, fmt.Sprintf("s-%03d-%s", i, strings.Repeat("x", 122)))
					}
					origin = sources[len(sources)-1] // Revoke the last edge; no prefix is sufficient.
				}
				ws := filepath.Join(t.TempDir(), "ws")
				if err := os.MkdirAll(ws, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("a\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				initGitRepoForTest(t, ws)

				dbPath := filepath.Join(t.TempDir(), "r6_f1.db")
				raw, err := session.Open(dbPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := raw.Create(ctx, session.Session{ID: origin, Goal: "origin", Status: "completed"}); err != nil {
					t.Fatal(err)
				}
				if tc.seedOrigin {
					if _, _, err := raw.BeginRun(ctx, origin, session.RunStart{
						RunID: "run-origin-seed",
						Input: &protocol.Message{Role: "user", Content: "origin knowledge"},
					}); err != nil {
						t.Fatal(err)
					}
				}
				if err := raw.Create(ctx, session.Session{ID: sid, Goal: "consumer", Status: "running"}); err != nil {
					t.Fatal(err)
				}
				if _, _, err := raw.BeginRun(ctx, sid, session.RunStart{
					RunID: "run-seed",
					Input: &protocol.Message{Role: "user", Content: "go"},
				}); err != nil {
					t.Fatal(err)
				}
				st := r401SettleStore{realSQLiteAgentStore{s: raw}}
				if _, _, err := st.AppendMessage(ctx, sid, protocol.Message{Role: "assistant", Content: "done"}, nil); err != nil {
					t.Fatal(err)
				}
				if _, _, err := st.AppendMessage(ctx, sid, protocol.Message{
					Role:    "user",
					Origin:  protocol.OriginCompletionBlocked,
					Content: "Completion refused: verify outputs.",
				}, nil); err != nil {
					t.Fatal(err)
				}
				if _, _, err := raw.BeginRun(ctx, sid, session.RunStart{RunID: "run-live"}); err != nil {
					t.Fatal(err)
				}

				eng, err := editing.New(editing.Options{Root: ws})
				if err != nil {
					t.Fatal(err)
				}
				eng.SetPublishFaultHooks(editing.PublishFaultHooks{
					CloseAfterPublish: func(string) error { return syscall.EIO },
				})
				ts, err := toolset.New(toolset.Options{
					Workspace: ws,
					Policy:    toolset.PolicyFull,
					SessionID: sid,
					Editing:   eng,
				})
				if err != nil {
					t.Fatal(err)
				}
				repo, err := repository.New(ws)
				if err != nil {
					t.Fatal(err)
				}

				var steps []protocol.ProviderResponse
				if tc.adopt {
					steps = append(steps, toolStep(call("tc-adopt", "adopt_experience", `{}`)))
				}
				steps = append(steps, toolStep(call("tc-apply", "apply_experience", `{}`)), answerOf("done"))
				prov := &scriptedProvider{responses: steps}

				res, err := (&Loop{
					Models:    map[config.ModelRef]Model{ref: testModel(ref, prov)},
					Tools:     &r6ApplyExperienceTools{inner: ts, origin: origin, sources: sources, padding: tc.padding, okFlag: false},
					Store:     st,
					Snapshots: repo,
				}).Run(ctx, sid, Options{Session: ref, RunID: "run-live", Resumed: true, MaxSteps: 6})
				if err != nil || res.Status != StatusCompleted {
					t.Fatalf("Loop.Run failed: res=%+v err=%v", res, err)
				}
				_ = raw.Close()

				// Cold SQLite inspection: valid JSON, bounded <= 8192, message edge preserved.
				db, err := sql.Open("sqlite3", filepath.Join(dbPath, "lumencortex.db"))
				if err != nil {
					t.Fatal(err)
				}
				var applySeq int64
				var applyContent string
				if err := db.QueryRow(`SELECT seq, json_extract(json,'$.content') FROM session_messages WHERE session_id=? AND role='tool' AND json_extract(json,'$.tool_call_id')='tc-apply'`, sid).Scan(&applySeq, &applyContent); err != nil {
					_ = db.Close()
					t.Fatal(err)
				}
				var msgEdgeCount int
				if err := db.QueryRow(`SELECT COUNT(*) FROM derived_origins WHERE session_id=? AND kind='message' AND source_kind='session' AND source_ref=?`, sid, origin).Scan(&msgEdgeCount); err != nil {
					_ = db.Close()
					t.Fatal(err)
				}
				if len(sources) > 0 {
					var n int
					err := db.QueryRow(`SELECT COUNT(DISTINCT source_ref) FROM derived_origins WHERE session_id=? AND kind='message' AND source_kind='session'`, sid).Scan(&n)
					if err != nil || n != len(sources) {
						t.Fatalf("durable sources=%d want %d: %v", n, len(sources), err)
					}
					var envelope struct {
						Partial  bool     `json:"partialPublication"`
						Withheld bool     `json:"provenanceWithheld"`
						Omitted  []string `json:"omittedFields"`
					}
					if err := json.Unmarshal([]byte(applyContent), &envelope); err != nil || !envelope.Partial || !envelope.Withheld || !slices.Contains(envelope.Omitted, "sourceSessions") {
						t.Fatalf("invalid controlled overflow: %s (%v)", applyContent, err)
					}
					if _, complete := resultSessions("apply_experience", sid, protocol.ToolResult{Content: applyContent}); complete {
						t.Fatal("overflow display claimed complete source lineage")
					}
				}
				_ = db.Close()

				if !json.Valid([]byte(applyContent)) || len(applyContent) > maxPreservedToolErrorBytes {
					t.Fatalf("persisted tc-apply row invalid or oversized: len=%d valid=%v", len(applyContent), json.Valid([]byte(applyContent)))
				}
				if strings.Contains(applyContent, "[nested_partial_publication]") || strings.Contains(applyContent, ws) {
					t.Fatalf("persisted tc-apply row contains plain text fallback or host path: %s", applyContent)
				}
				wantEdges := 1
				if tc.adopt {
					wantEdges = 2
				}
				if msgEdgeCount != wantEdges {
					t.Fatalf("derived_origins message edges = %d, want %d", msgEdgeCount, wantEdges)
				}

				// Cold Resume parity (nested write_file settled OutcomeUnknown -> MutationUnknown).
				_, coldRep := assertLiveColdStreamParity(t, ctx, dbPath, sid)
				if !coldRep.Active || coldRep.PostBlockUnknownCount < 1 {
					t.Fatalf("coldRep = %+v, want PostBlockUnknownCount >= 1", coldRep)
				}

				// RevokeSession on origin MUST reach consumer session!
				cold, err := session.Open(dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = cold.Close() }()
				rev, err := cold.RevokeSession(ctx, origin, "r6_verify_cascade")
				if err != nil {
					t.Fatal(err)
				}
				reached := false
				for _, s := range rev.Sessions {
					if s == sid {
						reached = true
					}
				}
				if !reached {
					t.Fatalf("RevokeSession(%s) failed to reach consumer %s: %+v", origin, sid, rev)
				}
				proj, err := cold.Projection(ctx, sid)
				if err != nil {
					t.Fatal(err)
				}
				// Because the nested write_file inside apply_experience hit CloseAfterPublish EIO
				// (OutcomeUnknown -> incomplete_lineage), revoking origin isolates the consumer session
				// at epoch >= 1 (whereas under R5-02 invalid JSON, epoch stayed 0 and Isolated stayed "").
				if proj.Epoch < 1 || (proj.Isolated == "" && len(proj.Withheld) == 0) {
					t.Fatalf("expected origin revocation to isolate or withhold consumer projection (applySeq=%d), got %+v", applySeq, proj)
				}
			})
		}
	})

	t.Run("nested_eval_oversized_json_and_clean_proven_semantics", func(t *testing.T) {
		const ref = config.ModelRef("test/m")
		ctx := context.Background()
		sid := "s-r6-eval-proven"
		ws := filepath.Join(t.TempDir(), "ws")
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, "clean.txt"), []byte("v1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		initGitRepoForTest(t, ws)

		dbPath := filepath.Join(t.TempDir(), "r6_eval.db")
		raw, err := session.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = raw.Close() })
		if err := raw.Create(ctx, session.Session{ID: sid, Goal: "eval proven+unknown", Status: "running"}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := raw.BeginRun(ctx, sid, session.RunStart{RunID: "run-seed", Input: &protocol.Message{Role: "user", Content: "go"}}); err != nil {
			t.Fatal(err)
		}
		st := r401SettleStore{realSQLiteAgentStore{s: raw}}
		if _, _, err := st.AppendMessage(ctx, sid, protocol.Message{Role: "assistant", Content: "done"}, nil); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.AppendMessage(ctx, sid, protocol.Message{Role: "user", Origin: protocol.OriginCompletionBlocked, Content: "Completion refused: verify."}, nil); err != nil {
			t.Fatal(err)
		}
		if _, _, err := raw.BeginRun(ctx, sid, session.RunStart{RunID: "run-live"}); err != nil {
			t.Fatal(err)
		}

		eng, err := editing.New(editing.Options{Root: ws})
		if err != nil {
			t.Fatal(err)
		}
		eng.SetPublishFaultHooks(editing.PublishFaultHooks{
			CloseAfterPublish: func(dst string) error {
				if filepath.Base(dst) == "dirty_nested.txt" {
					return syscall.EIO
				}
				return nil
			},
		})
		ts, err := toolset.New(toolset.Options{Workspace: ws, Policy: toolset.PolicyFull, SessionID: sid, Editing: eng})
		if err != nil {
			t.Fatal(err)
		}
		repo, err := repository.New(ws)
		if err != nil {
			t.Fatal(err)
		}

		bridge := &r5EvalBridgeTools{
			inner: ts,
			evalSpecs: []protocol.ToolSpec{{
				Name:        "eval_go",
				Description: "test eval",
				Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
			}},
			onEval: func(callCtx context.Context, cb protocol.ToolCaller) (protocol.ToolResult, error) {
				_, _ = cb.Call(callCtx, "write_file", map[string]any{"path": "dirty_nested.txt", "content": "dirty\n"})
				out, _ := json.Marshal(map[string]any{
					"id":     "eval-run-1",
					"stdout": strings.Repeat("S", 9500),
				})
				return protocol.ToolResult{OK: true, Content: string(out)}, nil
			},
		}

		prov := &scriptedProvider{responses: []protocol.ProviderResponse{
			toolStep(call("tc-eval-big", "eval_go", `{}`)),
			toolStep(call("tc-clean-write", "write_file", `{"path":"clean.txt","content":"v2\n"}`)),
			answerOf("completed"),
		}}
		res, err := (&Loop{
			Models:    map[config.ModelRef]Model{ref: testModel(ref, prov)},
			Tools:     bridge,
			Store:     st,
			Snapshots: repo,
		}).Run(ctx, sid, Options{Session: ref, RunID: "run-live", Resumed: true, MaxSteps: 6})
		if err != nil || res.Status != StatusCompleted {
			t.Fatalf("Loop.Run: res=%+v err=%v", res, err)
		}

		var evalContent string
		_ = st.Transcript(ctx, sid, 0, func(_ int64, m protocol.Message) error {
			if m.Role == "tool" && m.ToolCallID == "tc-eval-big" {
				evalContent = m.Content
			}
			return nil
		})
		if !json.Valid([]byte(evalContent)) || len(evalContent) > maxPreservedToolErrorBytes {
			t.Fatalf("evalContent invalid or oversized: len=%d valid=%v", len(evalContent), json.Valid([]byte(evalContent)))
		}
		var evalObj map[string]any
		_ = json.Unmarshal([]byte(evalContent), &evalObj)
		if evalObj["id"] != "eval-run-1" || evalObj["truncated"] != true || evalObj["nestedPartialPublications"] == nil {
			t.Fatalf("unexpected evalObj: %+v", evalObj)
		}
		_ = raw.Close()

		_, coldRep := assertLiveColdStreamParity(t, ctx, dbPath, sid)
		if coldRep.PostBlockUnknownCount != 1 || coldRep.PostBlockProvenCount != 1 {
			t.Fatalf("expected 1 Unknown (nested dirty write) and 1 Proven (clean write), got %+v", coldRep)
		}
	})
}

// TestR6PartialRollbackAndDiagnosticsColdResume covers §5.5 #12 (SQLite cold Resume after R5-01
// partial rollback with real child-process O_APPEND) and verifies R5-03/R5-04 bounded sanitized
// diagnostics persisted in SQLite across cold Resume.
func TestR7PartialCommitAndDiagnosticsColdResume(t *testing.T) {
	const ref = config.ModelRef("test/m")
	ctx := context.Background()
	sid := "s-r6-cold-resume"
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("old-a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "b.txt"), []byte("old-b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A second link outside the workspace keeps B on the in-place update path, the only path where a
	// fault after truncate still produces a partial publication (single-link files are staged).
	if err := os.Link(filepath.Join(ws, "b.txt"), filepath.Join(base, "b-inplace-alias.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "c.txt"), []byte("old-c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "subpkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "subpkg/p.go"), []byte("package subpkg\n\nvar x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	initGitRepoForTest(t, ws)

	dbPath := filepath.Join(t.TempDir(), "r6_resume.db")
	raw, err := session.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Create(ctx, session.Session{ID: sid, Goal: "R6 cold resume", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := raw.BeginRun(ctx, sid, session.RunStart{RunID: "run-seed", Input: &protocol.Message{Role: "user", Content: "go"}}); err != nil {
		t.Fatal(err)
	}
	st := r401SettleStore{realSQLiteAgentStore{s: raw}}
	if _, _, err := st.AppendMessage(ctx, sid, protocol.Message{Role: "assistant", Content: "done"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AppendMessage(ctx, sid, protocol.Message{Role: "user", Origin: protocol.OriginCompletionBlocked, Content: "Completion refused: reconcile."}, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := raw.BeginRun(ctx, sid, session.RunStart{RunID: "run-live"}); err != nil {
		t.Fatal(err)
	}

	eng, err := editing.New(editing.Options{Root: ws})
	if err != nil {
		t.Fatal(err)
	}
	ts, err := toolset.New(toolset.Options{Workspace: ws, Policy: toolset.PolicyFull, SessionID: sid, Editing: eng})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := repository.New(ws)
	if err != nil {
		t.Fatal(err)
	}

	// Pre-read subpkg/p.go for anchored edit_lines.
	rd, err := ts.Execute(ctx, "read_file", map[string]any{"path": "subpkg/p.go", "anchors": true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rev := regexp.MustCompile(`rev=(\S+)`).FindStringSubmatch(rd.Content)[1]
	anchor := regexp.MustCompile(`(?m)^(3:[^|]+)\|`).FindStringSubmatch(rd.Content)[1]
	var badGo strings.Builder
	for i := 0; i < 100; i++ {
		badGo.WriteString("var _ int = \"s\"\n")
	}
	editLinesArgs, _ := json.Marshal(map[string]any{
		"path": "subpkg/p.go",
		"rev":  rev,
		"ops":  []map[string]any{{"op": "replace", "start": anchor, "text": badGo.String()}},
	})

	// A is verified first. While B fails after truncate a separate process appends to A.
	eng.SetPublishFaultHooks(editing.PublishFaultHooks{
		AfterTruncate: func(dst string) error {
			if filepath.Base(dst) == "b.txt" {
				cmd := exec.Command(os.Args[0], "-test.run=^TestR6AgentExternalAppendHelper$")
				cmd.Env = append(os.Environ(), "LC_R6_AGENT_APPEND_TARGET="+filepath.Join(ws, "a.txt"))
				out, cmdErr := cmd.CombinedOutput()
				if cmdErr != nil {
					return fmt.Errorf("%w: %s", cmdErr, out)
				}
				return errors.New("B failed after truncate")
			}
			return nil
		},
	})

	patchArgs := `{"patches":[{"path":"a.txt","operation":"update","edits":[{"old_text":"old-a","new_text":"longer-new-a"}]},{"path":"b.txt","operation":"update","edits":[{"old_text":"old-b","new_text":"new-b"}]},{"path":"c.txt","operation":"update","edits":[{"old_text":"old-c","new_text":"new-c"}]}]}`
	prov := &scriptedProvider{responses: []protocol.ProviderResponse{
		toolStep(call("tc-patch-r501", "apply_patch", patchArgs)),
		toolStep(call("tc-editlines-r503", "edit_lines", string(editLinesArgs))),
		answerOf("reconciliation needed"),
	}}
	res, err := (&Loop{
		Models:    map[config.ModelRef]Model{ref: testModel(ref, prov)},
		Tools:     ts,
		Store:     st,
		Snapshots: repo,
	}).Run(ctx, sid, Options{Session: ref, RunID: "run-live", Resumed: true, MaxSteps: 6})
	if err != nil || res.Status != StatusCompleted {
		t.Fatalf("Loop.Run: res=%+v err=%v", res, err)
	}
	_ = raw.Close()

	// A retained both LC's write and the child's append. B was truncated, not restored.
	gotA, err := os.ReadFile(filepath.Join(ws, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotA) != "longer-new-a\nFOREIGN\n" {
		t.Fatalf("published/external bytes lost: %q", gotA)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "b.txt")); err != nil || len(b) != 0 {
		t.Fatalf("B = %q, err = %v", b, err)
	}

	// 2. Inspect raw SQLite rows across cold reopen.
	db, err := sql.Open("sqlite3", filepath.Join(dbPath, "lumencortex.db"))
	if err != nil {
		t.Fatal(err)
	}
	var patchContent, editLinesContent string
	if err := db.QueryRow(`SELECT json_extract(json,'$.content') FROM session_messages WHERE session_id=? AND role='tool' AND json_extract(json,'$.tool_call_id')='tc-patch-r501'`, sid).Scan(&patchContent); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT json_extract(json,'$.content') FROM session_messages WHERE session_id=? AND role='tool' AND json_extract(json,'$.tool_call_id')='tc-editlines-r503'`, sid).Scan(&editLinesContent); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()

	for label, content := range map[string]string{"patch": patchContent, "edit_lines": editLinesContent} {
		if !json.Valid([]byte(content)) || len(content) > 4096 {
			t.Fatalf("%s SQLite content invalid or > 4096 bytes: len=%d valid=%v", label, len(content), json.Valid([]byte(content)))
		}
		if strings.Contains(content, ws) || strings.Contains(content, base) {
			t.Fatalf("%s SQLite content leaked absolute host path: %s", label, content)
		}
	}
	if !strings.Contains(editLinesContent, `"code":"type"`) || !strings.Contains(editLinesContent, `"diagnosticsOmitted"`) {
		t.Fatalf("edit_lines SQLite content missing bounded type diagnostics metadata: %s", editLinesContent)
	}
	if !strings.Contains(patchContent, `"commitState":"partially_published"`) || !strings.Contains(patchContent, `"external_modification_detected"`) {
		t.Fatalf("patch SQLite content missing partial-publication reconciliation details: %s", patchContent)
	}
	// R7R2-CT-01: the per-file rows the agent and a cold Resume see, and the persisted seal. A keeps
	// LC's write plus the foreign append; B was truncated and failed; C was never attempted. None of
	// them may carry a trusted revision or hash.
	var patchPayload struct {
		Files []struct {
			Path         string `json:"path"`
			PublishState string `json:"publishState"`
			Published    bool   `json:"published"`
			Verified     bool   `json:"verified"`
			NewRev       string `json:"newRev"`
			SHA256       string `json:"sha256"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(patchContent), &patchPayload); err != nil {
		t.Fatal(err)
	}
	wantStates := map[string]editing.PublishState{
		"a.txt": editing.PublishExternalModification,
		"b.txt": editing.PublishPartial,
		"c.txt": editing.PublishNotAttempted,
	}
	for _, f := range patchPayload.Files {
		want, ok := wantStates[f.Path]
		if !ok || f.PublishState != string(want) || f.Verified || f.NewRev != "" || f.SHA256 != "" {
			t.Fatalf("patch row %+v, want state %q without a trusted revision", f, want)
		}
		// An in-place write exposes its bytes at the destination, so B's truncation is published too.
		if f.Published != (f.Path != "c.txt") {
			t.Fatalf("patch row %+v reports published=%v", f, f.Published)
		}
		delete(wantStates, f.Path)
	}
	if len(wantStates) != 0 {
		t.Fatalf("patch rows missing %v: %s", wantStates, patchContent)
	}
	sealStore, err := session.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	answered, err := sealStore.ListAnsweredExecutions(ctx, sid, 0)
	_ = sealStore.Close()
	if err != nil {
		t.Fatal(err)
	}
	var sealed bool
	for _, x := range answered {
		if x.ToolName != "apply_patch" {
			continue
		}
		sealed = true
		if x.Outcome != session.OutcomeUnknown || !strings.HasSuffix(x.PostRef, ";cm=unknown") {
			t.Fatalf("partial apply_patch sealed as outcome=%s postRef=%s, want outcome_unknown and ;cm=unknown", x.Outcome, x.PostRef)
		}
	}
	if !sealed {
		t.Fatalf("no persisted apply_patch execution among %+v", answered)
	}

	// 3. Cold SQLite Resume parity: tc-patch-r501 MUST be MutationUnknown (never MutationNone),
	// and PostBlockProvenCount MUST stay 0!
	_, coldRep := assertLiveColdStreamParity(t, ctx, dbPath, sid)
	if !coldRep.Active || coldRep.PostBlockUnknownCount < 1 || coldRep.PostBlockProvenCount != 0 {
		t.Fatalf("coldRep = %+v, want PostBlockUnknownCount>=1 PostBlockProvenCount=0", coldRep)
	}
	var sawPatchUnknown bool
	for _, cp := range coldRep.CausalProgress {
		if cp.ToolCallID == "tc-patch-r501" && cp.MutationEvidence == MutationUnknown {
			sawPatchUnknown = true
		}
	}
	if !sawPatchUnknown {
		t.Fatalf("unexpected CausalProgress after cold Resume: %+v", coldRep.CausalProgress)
	}
}
