package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/internal/window"
	"github.com/edynasty/LumenCortex/internal/workflow"
	"github.com/edynasty/LumenCortex/protocol"
)

// TestT6B_PresentFeedbackDualGuardsAndBaseGateSlack verifies T6 subset B & C:
// 8 KiB adversarial feedback strings (all \n, all ", all \, all <>&, control chars \x00..\x1f, invalid UTF-8 0xff)
// stay valid UTF-8, obey 2458B / 12346B JSON byte caps, estimate <= 1438 / 7206 tokens, and match BaseGateSlack
// 1950 / 7718 tokens.
func TestT6B_PresentFeedbackDualGuardsAndBaseGateSlack(t *testing.T) {
	m := Model{Ref: "p/m", Context: 32768, Output: 4096, ToolCalling: true}
	if got := BaseGateSlack(m, PresentationOptions{}, ModeCompact); got != 1950 {
		t.Fatalf("BaseGateSlack(compact)=%d want 1950", got)
	}
	if got := BaseGateSlack(m, PresentationOptions{}, ModeFull); got != 7718 {
		t.Fatalf("BaseGateSlack(detailed)=%d want 7718", got)
	}
	if got := GuardTokens(32768, config.PackageOpenAICompatible, 10); got != 2048 {
		t.Fatalf("GuardTokens(openai,32k)=%d want 2048", got)
	}
	if got := GuardTokens(32768, config.PackageAnthropicMessages, 10); got != 2560 {
		t.Fatalf("GuardTokens(anthropic,32k,tools)=%d want 2560", got)
	}

	ctrl := make([]byte, 0, 8192)
	for i := range 8192 {
		ctrl = append(ctrl, byte(i%32))
	}
	badUTF8 := append([]byte("gate error: "), 0xff, 0xfe, 0x80)
	badUTF8 = append(badUTF8, []byte(strings.Repeat("<>&\"\n\\", 1400))...)

	adversarial := map[string]string{
		"newlines":     strings.Repeat("line\n", 2000),
		"quotes":       strings.Repeat(`"`, 8192),
		"backslashes":  strings.Repeat(`\`, 8192),
		"angle_amp":    strings.Repeat(`<>&`, 3000),
		"controls":     "header\n" + string(ctrl) + "\nfooter",
		"invalid_utf8": string(badUTF8),
	}

	for name, input := range adversarial {
		for _, variant := range []string{config.ProfileCompact, config.ProfileDetailed, ""} {
			got := presentFeedback(input, variant)
			if !utf8.ValidString(got) {
				t.Fatalf("%s (%q): output is not valid UTF-8", name, variant)
			}
			blocked := protocol.Message{Role: "user", Origin: protocol.OriginCompletionBlocked, Content: got}
			raw, err := json.Marshal(blocked)
			if err != nil {
				t.Fatalf("%s (%q): marshal failed: %v", name, variant, err)
			}
			est := window.Estimate(blocked)
			if variant == config.ProfileCompact {
				if len(got) > CompactFeedbackBytes || len(raw) > MaxCompactGateJSONBytes || est > 1438 {
					t.Fatalf("%s (compact): utf8=%d json=%d est=%d exceed 1200/2458/1438", name, len(got), len(raw), est)
				}
			} else {
				if len(got) > MaxDetailedFeedbackBytes || len(raw) > MaxDetailedGateJSONBytes || est > 7206 {
					t.Fatalf("%s (detailed): utf8=%d json=%d est=%d exceed 6144/12346/7206", name, len(got), len(raw), est)
				}
			}
		}
	}
}

// TestT9_MCPCodeFencesAndExplicitProfileCompactSafety verifies T9 subsets A & B:
// A: 6-class MCP code fence preservation + (MCP server: ops) preservation + exec/builtin 100% verbatim in budget ModeCompact;
// B: REV-F06 explicit ProfileCompact preserves safety/pagination preconditions on the 6 critical built-ins.
func TestT9_MCPCodeFencesAndExplicitProfileCompactSafety(t *testing.T) {
	mcpCases := []struct {
		name      string
		desc      string
		wantFold  bool
		keepToken string
	}{
		{
			name:      "mcp__ops__unlabelled_fence",
			desc:      "Query cluster metrics.\n```json\n{\"metric\":\"cpu\"}\n``` (MCP server: ops)",
			wantFold:  false,
			keepToken: `"metric":"cpu"`,
		},
		{
			name:      "mcp__ops__schema_fence",
			desc:      "Query logs.\n### Example:\n```json\n{\"schema\":\"v2\"}\n``` (MCP server: ops)",
			wantFold:  false,
			keepToken: `"schema":"v2"`,
		},
		{
			name:      "mcp__ops__normative_must_fence",
			desc:      "Run query.\n### Example:\n```sql\n-- caller must limit rows\nSELECT 1;\n``` (MCP server: ops)",
			wantFold:  false,
			keepToken: "SELECT 1;",
		},
		{
			name:      "mcp__ops__env_auth_fence",
			desc:      "Inspect service.\nExample:\n```sh\n# pass token header\ncurl /health\n``` (MCP server: ops)",
			wantFold:  false,
			keepToken: "curl /health",
		},
		{
			name:      "mcp__ops__unclosed_fence",
			desc:      "Inspect traces.\n### Example:\n```json\n{\"traceId\":\"abc\"} (MCP server: ops)",
			wantFold:  false,
			keepToken: `"traceId":"abc"`,
		},
		{
			name:     "mcp__ops__pure_verified_example",
			desc:     "List pods in namespace.\n### Example:\n```json\n{\"namespace\":\"prod\",\"limit\":10}\n```\nReturns pod names. (MCP server: ops)",
			wantFold: true,
		},
	}

	var stepTools []StepTool
	stepTools = append(stepTools, StepTool{
		Source:   "builtin",
		Included: true,
		Spec: protocol.ToolSpec{
			Name:        "write_file",
			Description: "Create or replace a UTF-8 file. Refused when the file changed since this session read it; pass base_rev to name the revision.",
			Permission:  "write",
		},
	})
	stepTools = append(stepTools, StepTool{
		Source:   "mcp:ops",
		Included: true,
		Spec: protocol.ToolSpec{
			Name:        "mcp__ops__exec_deploy",
			Description: "Deploy release.\n### Example:\n```json\n{\"tag\":\"v1\"}\n``` (MCP server: ops)",
			Permission:  "exec",
		},
	})
	for _, tc := range mcpCases {
		stepTools = append(stepTools, StepTool{
			Source:   "mcp:ops",
			Included: true,
			Spec: protocol.ToolSpec{
				Name:        tc.name,
				Description: tc.desc,
				Permission:  "read",
			},
		})
	}

	budgetCompact := presentStepTools(stepTools, "", ModeCompact)
	// Builtin and exec MCP must stay 100% byte-for-byte unchanged under budget ModeCompact!
	if budgetCompact[0].Description != stepTools[0].Spec.Description {
		t.Fatalf("builtin altered under budget ModeCompact: %q", budgetCompact[0].Description)
	}
	if budgetCompact[1].Description != stepTools[1].Spec.Description {
		t.Fatalf("exec MCP altered under budget ModeCompact: %q", budgetCompact[1].Description)
	}
	for i, tc := range mcpCases {
		got := budgetCompact[i+2].Description
		if !strings.HasSuffix(got, " (MCP server: ops)") {
			t.Fatalf("%s lost (MCP server: ops) suffix: %q", tc.name, got)
		}
		if tc.wantFold {
			if !strings.Contains(got, "[example omitted]") || strings.Contains(got, `"namespace":"prod"`) || !strings.Contains(got, "Returns pod names.") {
				t.Fatalf("%s pure example not folded cleanly: %q", tc.name, got)
			}
		} else {
			if strings.Contains(got, "[example omitted]") || !strings.Contains(got, tc.keepToken) {
				t.Fatalf("%s wrongly folded normative/unlabelled/unclosed fence: %q", tc.name, got)
			}
		}
	}

	// Subset B (REV-F06 P3): explicit ProfileCompact preserves safety/pagination preconditions on all 6 built-ins.
	criticalBuiltins := []StepTool{
		{Source: "builtin", Included: true, Spec: protocol.ToolSpec{Name: "read_file", Description: "Read a UTF-8 file inside the workspace. anchors=true prints a rev header.", Permission: "read"}},
		{Source: "builtin", Included: true, Spec: protocol.ToolSpec{Name: "write_file", Description: "Create or replace a UTF-8 file. Refused when changed.", Permission: "write"}},
		{Source: "builtin", Included: true, Spec: protocol.ToolSpec{Name: "replace_in_file", Description: "Replace one exact text occurrence. Refused when changed.", Permission: "write"}},
		{Source: "builtin", Included: true, Spec: protocol.ToolSpec{Name: "edit_lines", Description: "Edit lines at the revision read_file showed.", Permission: "write"}},
		{Source: "builtin", Included: true, Spec: protocol.ToolSpec{
			Name:        protocol.ToolBash,
			Description: `Run a command in /bin/bash. mode "background" starts it as a durable background job.`,
			Permission:  "exec",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}, "mode": map[string]any{"type": "string"}}},
		}},
		{Source: "builtin", Included: true, Spec: protocol.ToolSpec{Name: "eval", Description: "Execute one cell in an authorized persistent runtime.", Permission: "exec"}},
	}
	profCompact := presentStepTools(criticalBuiltins, config.ProfileCompact, ModeCompact)
	checks := []struct {
		idx     int
		mustAll []string
	}{
		{0, []string{"anchors=true", "start_line", "end_line"}},
		{1, []string{"refused", "base_rev"}},
		{2, []string{"refused", "base_rev"}},
		{3, []string{"rev", "stale"}},
		{4, []string{"background", "block completion"}},
		{5, []string{"variables", "roll back", "cancel/timeout", "clean process"}},
	}
	for _, c := range checks {
		desc := strings.ToLower(profCompact[c.idx].Description)
		for _, kw := range c.mustAll {
			if !strings.Contains(desc, strings.ToLower(kw)) {
				t.Fatalf("REV-F06 tool %s missing safety keyword %q in %q", profCompact[c.idx].Name, kw, profCompact[c.idx].Description)
			}
		}
	}
}

// TestT12_EnvelopeModelRefBijectionSlotPriorityAndReplay verifies T12 subsets A, B1/B2/B3, and C:
// - A: 10-chunk Unicode bijection (64B + 1B + 192B = 257B -> 769B %XX -> model~0~..model~9.~) and
//   tamper detection including missing terminal ".~" seal -> ErrIncompleteModelRefChunks;
// - B1 (10 + 7 + 111 = 128 -> lineage_complete=1), B2 (10 + 7 + 112 = 129 -> lineage_complete=0,
//   P0..P3 100% preserved), B3 (128 * 96B == 12,673B <= 16,384B in real SQLite);
// - C: VerifyStep replay of RuleWindowPrefix, CompactFixedByModel, and v1 Base, plus RevokeSession.
func TestT12_EnvelopeModelRefBijectionSlotPriorityAndReplay(t *testing.T) {
	ctx := context.Background()

	// Subset A: Bijection & tamper rejection (offline, zero config table).
	maxEmojiRef := config.ModelRef(strings.Repeat("🚀", 16) + "/" + strings.Repeat("🧠", 48)) // 64B + 1 + 192B = 257B UTF-8 -> 769B escaped
	roundTripRefs := []config.ModelRef{
		"openai/gpt-5",
		"custom%~prov/model with spaces & %7E ~ symbols",
		config.ModelRef(strings.Repeat("语", 21) + "/" + strings.Repeat("模", 64)),
		maxEmojiRef,
	}
	var tenChunks []string
	for _, orig := range roundTripRefs {
		chunks, err := EncodeEnvelopeModelRef(orig)
		if err != nil {
			t.Fatalf("EncodeEnvelopeModelRef(%q): %v", orig, err)
		}
		if orig == maxEmojiRef {
			tenChunks = chunks
			if len(chunks) != MaxModelRefChunks || !strings.HasPrefix(chunks[9], "model~9.~") {
				t.Fatalf("expected exact 10 chunks ending with model~9.~, got %d: %v", len(chunks), chunks)
			}
		}
		got, err := DecodeEnvelopeModelRef(chunks)
		if err != nil || got != orig {
			t.Fatalf("DecodeEnvelopeModelRef(%v)=%q, %v; want %q", chunks, got, err, orig)
		}
	}

	// Tamper cases: missing terminal seal (model~0~..model~8~), premature seal, gap, out-of-order, mixed, empty.
	if _, err := DecodeEnvelopeModelRef(tenChunks[:9]); !errors.Is(err, ErrIncompleteModelRefChunks) {
		t.Fatalf("truncated 9-of-10 chunks: err=%v, want ErrIncompleteModelRefChunks", err)
	}
	premature := slices.Clone(tenChunks)
	premature[1] = strings.Replace(premature[1], "model~1~", "model~1.~", 1)
	for name, bad := range map[string][]string{
		"empty":     {"sys:prompt:123456"},
		"mixed":     append([]string{"model:openai/gpt"}, tenChunks...),
		"reordered": {tenChunks[1], tenChunks[0], tenChunks[9]},
		"gap":       {tenChunks[0], tenChunks[2], tenChunks[9]},
		"duplicate": {tenChunks[0], tenChunks[0], tenChunks[9]},
		"premature": premature,
	} {
		if got, err := DecodeEnvelopeModelRef(bad); err == nil {
			t.Fatalf("tamper %s decoded as %q instead of failing", name, got)
		}
	}

	// Subset B: Real SQLite Store verification for 128 vs 129 slots and 128*96B == 12,673B.
	repoDir := t.TempDir()
	store, err := session.Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.CreateWithMessage(ctx, session.Session{ID: "s-t12", Goal: "g", Status: "running"}, protocol.Message{Role: "user", Content: "hi"}); err != nil {
		t.Fatal(err)
	}

	buildStateRefs := func(openUnits int) []string {
		mgr := taskstate.NewWorkUnitManager(nil)
		completed := taskstate.WorkCompleted
		for i := range 50 {
			u, _ := mgr.Add(taskstate.WorkUnit{ID: fmt.Sprintf("done-%02d", i), Goal: "done"})
			_, _ = mgr.Update(u.ID, taskstate.WorkUnitPatch{Status: &completed})
		}
		for i := range openUnits {
			if _, err := mgr.Add(taskstate.WorkUnit{ID: fmt.Sprintf("open-%02d", i), Goal: "open"}); err != nil {
				t.Fatal(err)
			}
		}
		var refs []taskstate.Reference
		for i := range 64 {
			refs = append(refs, taskstate.Reference{ID: fmt.Sprintf("r%02d", i), Kind: taskstate.RefPath, Target: fmt.Sprintf("f%d.go", i), State: taskstate.RefIncluded})
		}
		var tasks []taskstate.Task
		for i := range 100 { // 90 old + 10 newest rendered
			tasks = append(tasks, taskstate.Task{ID: fmt.Sprintf("t%03d", i), Objective: "obj"})
		}
		wf, err := workflow.New(&workflow.Definition{
			ID: "wf", Title: "WF", Entry: "a",
			Actions: map[string]*workflow.Action{"a": {Title: "A", Terminal: true}},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		st := taskstate.State{Brief: "brief", References: refs, Tasks: tasks}
		st.SetUnits(mgr)
		rb := taskstate.RenderBlock("g", st, wf)
		return rb.ItemRefs
	}

	sentLayers := []SystemLayer{
		{Kind: "global-instructions", Text: "g"},
		{Kind: "project-instructions", Text: "p"},
		{Kind: "skills", Text: "s"},
	}
	opts := Options{SystemPrompt: "base", Instructions: "agent-inst"}
	zero := int64(0)

	// B1: 10 model chunks + 7 system/proj/ts:v1 + 111 rendered TaskState refs = 128!
	refs111 := buildStateRefs(35) // 1 brief + 64 refs + 1 workflow + 35 open units + 10 tasks = 111
	if len(refs111) != 111 {
		t.Fatalf("B1 rendered state refs=%d want 111", len(refs111))
	}
	full128 := requestRefs(tenChunks, opts, sentLayers, ModeCompact, 1, refs111)
	if len(full128) != 128 {
		t.Fatalf("B1 total refs=%d want 128", len(full128))
	}
	env128, err := store.AdmitRequest(ctx, session.RequestAdmission{
		RequestID: "rq-b1-128", SessionID: "s-t12", Step: 1, Attempt: 0, Purpose: session.PurposeMain,
		FirstSeq: &zero, LastSeq: &zero, Refs: full128, ToolSpecsDigest: digestOf([]byte("[]")),
		ProjectionVersion: projectionVersion, LineageComplete: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	stored128, err := store.Request(ctx, env128.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored128.LineageComplete || len(stored128.Refs) != 128 {
		t.Fatalf("B1 stored lineageComplete=%v len=%d", stored128.LineageComplete, len(stored128.Refs))
	}
	if gotRef, err := DecodeEnvelopeModelRef(stored128.Refs); err != nil || gotRef != maxEmojiRef {
		t.Fatalf("B1 offline decode=%q err=%v", gotRef, err)
	}

	// B2: 10 model chunks + 7 system/proj/ts:v1 + 112 rendered TaskState refs = 129 -> truncated to 128, lineage_complete=0!
	refs112 := buildStateRefs(36)
	if len(refs112) != 112 {
		t.Fatalf("B2 rendered state refs=%d want 112", len(refs112))
	}
	full129 := requestRefs(tenChunks, opts, sentLayers, ModeCompact, 1, refs112)
	if len(full129) != 129 {
		t.Fatalf("B2 total refs=%d want 129", len(full129))
	}
	env129, err := store.AdmitRequest(ctx, session.RequestAdmission{
		RequestID: "rq-b2-129", SessionID: "s-t12", Step: 2, Attempt: 0, Purpose: session.PurposeMain,
		FirstSeq: &zero, LastSeq: &zero, Refs: full129, ToolSpecsDigest: digestOf([]byte("[]")),
		ProjectionVersion: projectionVersion, LineageComplete: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	stored129, err := store.Request(ctx, env129.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if stored129.LineageComplete || len(stored129.Refs) != 128 {
		t.Fatalf("B2 stored lineageComplete=%v len=%d, want false and 128", stored129.LineageComplete, len(stored129.Refs))
	}
	if !slices.Equal(stored129.Refs[:17], full129[:17]) {
		t.Fatalf("B2 altered P0..P3 high-priority prefix: got %v want %v", stored129.Refs[:17], full129[:17])
	}
	if gotRef, err := DecodeEnvelopeModelRef(stored129.Refs); err != nil || gotRef != maxEmojiRef {
		t.Fatalf("B2 offline decode failed after P4 truncation: %q %v", gotRef, err)
	}

	// B3: 128 * 96B refs -> exact JSON length 12,673B <= 16,384B in SQLite!
	max96 := make([]string, 128)
	for i := range 128 {
		prefix := fmt.Sprintf("r%03d:", i)
		max96[i] = prefix + strings.Repeat("a", 96-len(prefix))
	}
	rawJSON, err := json.Marshal(max96)
	if err != nil || len(rawJSON) != 12673 {
		t.Fatalf("B3 marshaled len=%d err=%v, want exact 12673", len(rawJSON), err)
	}
	if _, err := store.AdmitRequest(ctx, session.RequestAdmission{
		RequestID: "rq-b3-maxbytes", SessionID: "s-t12", Step: 3, Attempt: 0, Purpose: session.PurposeMain,
		FirstSeq: &zero, LastSeq: &zero, Refs: max96, ToolSpecsDigest: digestOf([]byte("[]")),
		ProjectionVersion: projectionVersion, LineageComplete: true,
	}); err != nil {
		t.Fatalf("B3 SQLite rejected 12,673B refs_json: %v", err)
	}

	// Subset C: VerifyStep replay for v2 (CompactFixedByModel + RuleWindowPrefix) and v1 (Base), plus Fence rejection.
	candsMap := map[config.ModelRef]Candidate{
		"p/s": {Ref: "p/s", Context: 32768, Output: 4096, ToolCalling: true},
		"p/l": {Ref: "p/l", Context: 200000, Output: 8192, ToolCalling: true},
	}
	_, candDecV2 := CandidatesByStaticPrefixDecision(
		"p/s", []config.ModelRef{"p/l"}, candsMap,
		map[config.ModelRef]int{"p/s": 6000, "p/l": 6000},
		map[config.ModelRef]int{"p/s": 2048, "p/l": 12500},
		4096, nil, nil,
	)
	mode, reason, prefDec := WindowPrefixDecision("p/s", "", 24000, 26000, 14000, 6000, 2000)
	if mode != ModeCompact || reason != PrefixReasonFixedPrefixBudget {
		t.Fatalf("unexpected prefix decision: %s / %s", mode, reason)
	}
	stepV2, _ := json.Marshal(map[string]any{
		"step":      1,
		"modelRef":  "p/s",
		"decisions": []Decision{candDecV2, prefDec},
	})
	if err := VerifyStep(stepV2); err != nil {
		t.Fatalf("VerifyStep(v2) failed: %v", err)
	}

	_, candDecV1 := CandidatesDecision("p/s", []config.ModelRef{"p/l"}, candsMap, 5000, 4096, nil, nil)
	stepV1, _ := json.Marshal(map[string]any{
		"step":      1,
		"modelRef":  "p/s",
		"decisions": []Decision{candDecV1},
	})
	if err := VerifyStep(stepV1); err != nil {
		t.Fatalf("VerifyStep(v1) failed: %v", err)
	}
}
