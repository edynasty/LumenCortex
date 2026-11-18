package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/internal/window"
	"github.com/edynasty/LumenCortex/protocol"
)

type catalogToolsFake struct {
	entries []toolset.CatalogTool
	execFn  func(ctx context.Context, name string, args map[string]any) (protocol.ToolResult, error)
}

func (c *catalogToolsFake) Specs(allow []string) []protocol.ToolSpec {
	out := make([]protocol.ToolSpec, 0, len(c.entries))
	for _, e := range c.entries {
		if allow != nil && !slices.Contains(allow, e.Spec.Name) {
			continue
		}
		out = append(out, e.Spec)
	}
	return out
}

func (c *catalogToolsFake) Catalog() []toolset.CatalogTool {
	return append([]toolset.CatalogTool(nil), c.entries...)
}

func (c *catalogToolsFake) Execute(ctx context.Context, name string, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	if c.execFn != nil {
		return c.execFn(ctx, name, args)
	}
	return protocol.ToolResult{OK: true, Content: "ok"}, nil
}

// TestT1_BidirectionalCandidateProjectionAndFailover exercises ADR-0024 §6 T1:
// [m-32k, m-200k] and [m-200k, m-32k] with 40 read-only mcp:ext tools (pure example blocks +
// "(MCP server: ext)"), a 40 KiB project instructions layer, and a 60-skill layer.
func TestT1_BidirectionalCandidateProjectionAndFailover(t *testing.T) {
	const (
		ref32k  = config.ModelRef("test/m-32k")
		ref200k = config.ModelRef("test/m-200k")
	)

	var entries []toolset.CatalogTool
	entries = append(entries, toolset.CatalogTool{
		Spec:   protocol.ToolSpec{Name: "read_file", Description: "Read a UTF-8 file from the workspace.", Permission: "read"},
		Source: "builtin",
	})
	examplePayload := strings.Repeat(`{"item":"alpha","count":42}`+"\n", 38) // ~1 KiB pure example JSON
	for i := 0; i < 40; i++ {
		name := fmt.Sprintf("mcp__ext__tool_%02d", i)
		desc := fmt.Sprintf(
			"Fetch non-mutating external telemetry metric %02d for inspection across workspace targets without side effects or mutations.\n\n### Example:\n```json\n%s```\n (MCP server: ext)",
			i, examplePayload,
		)
		entries = append(entries, toolset.CatalogTool{
			Spec: protocol.ToolSpec{
				Name:        name,
				Description: desc,
				Permission:  "read",
				Parameters:  map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}},
			},
			Source: "mcp:ext",
		})
	}
	tools := &catalogToolsFake{entries: entries}

	fullProjectText := "[project instructions: AGENTS.md]\n" + strings.Repeat("Project architecture detail line with context.\n", 880) // ~41 KiB
	compactProjectText := "[project instructions: AGENTS.md (outlined)]\n" + strings.Repeat("Heading 1..200\n", 180) + "\nUse read_file(path=\"AGENTS.md\", start_line=1, end_line=200, anchors=true)"
	var fullSkillsBuilder, compactSkillsBuilder strings.Builder
	fullSkillsBuilder.WriteString("Available skills (60 enabled):\n")
	compactSkillsBuilder.WriteString("Available skills (compact catalog, 60 eligible; call read_skill({\"offset\":0}) to browse):\n")
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&fullSkillsBuilder, "- skill-%02d: %s\n", i, strings.Repeat("detailed skill summary ", 18))
		if compactSkillsBuilder.Len() < 7500 {
			fmt.Fprintf(&compactSkillsBuilder, "- skill-%02d: brief summary\n", i)
		}
	}
	layers := []SystemLayer{
		{Kind: "global-instructions", Source: "/global/AGENTS.md", Text: "[global instructions]\nAlways preserve workspace boundaries."},
		{Kind: "project-instructions", Source: "AGENTS.md", Text: fullProjectText, CompactText: compactProjectText},
		{Kind: "skills", Source: "skills", Text: fullSkillsBuilder.String(), CompactText: compactSkillsBuilder.String()},
	}

	runDirection := func(t *testing.T, primary, secondary config.ModelRef) (protocol.ProviderRequest, protocol.ProviderRequest, stepRecord) {
		t.Helper()
		p32 := &rejectingProvider{
			class:            protocol.ClassAvailability,
			scriptedProvider: scriptedProvider{responses: []protocol.ProviderResponse{answerOf("answered-32k")}},
		}
		p200 := &rejectingProvider{
			class:            protocol.ClassAvailability,
			scriptedProvider: scriptedProvider{responses: []protocol.ProviderResponse{answerOf("answered-200k")}},
		}
		if primary == ref32k {
			p200.failed = true // secondary answers immediately
		} else {
			p32.failed = true // secondary answers immediately
		}
		m32 := testModel(ref32k, p32)
		m32.Context, m32.Output = 32768, 4096
		m200 := testModel(ref200k, p200)
		m200.Context, m200.Output = 200000, 8192

		store := envStore("t1")
		loop := Loop{
			Models: map[config.ModelRef]Model{ref32k: m32, ref200k: m200},
			Store:  store,
			Tools:  tools,
		}
		res, err := loop.Run(context.Background(), "t1", Options{
			Session:      primary,
			Failover:     []config.ModelRef{secondary},
			SystemLayers: layers,
			MaxSteps:     3,
		})
		if err != nil || res.Status != StatusCompleted {
			t.Fatalf("Run(%s -> %s) result=%+v err=%v", primary, secondary, res, err)
		}
		if len(p32.seen) != 1 || len(p200.seen) != 1 {
			t.Fatalf("seen requests: 32k=%d 200k=%d, want 1 each (both must pass static prefix screening)", len(p32.seen), len(p200.seen))
		}
		steps := replayAll(t, store)
		return p32.seen[0], p200.seen[0], steps[0]
	}

	t.Run("32k_to_200k", func(t *testing.T) {
		req32, req200, rec := runDirection(t, ref32k, ref200k)
		assertT1Requests(t, req32, req200, fullProjectText, compactProjectText, fullSkillsBuilder.String(), compactSkillsBuilder.String())
		assertPrefixDecisions(t, rec.Decisions, []windowPrefixOutput{
			{Mode: ModeCompact, Reason: PrefixReasonFixedPrefixBudget},
			{Mode: ModeFull, Reason: PrefixReasonFitsFull},
		})
	})

	t.Run("200k_to_32k", func(t *testing.T) {
		req32, req200, rec := runDirection(t, ref200k, ref32k)
		assertT1Requests(t, req32, req200, fullProjectText, compactProjectText, fullSkillsBuilder.String(), compactSkillsBuilder.String())
		assertPrefixDecisions(t, rec.Decisions, []windowPrefixOutput{
			{Mode: ModeFull, Reason: PrefixReasonFitsFull},
			{Mode: ModeCompact, Reason: PrefixReasonFixedPrefixBudget},
		})
	})
}

func assertT1Requests(t *testing.T, req32, req200 protocol.ProviderRequest, fullProj, compactProj, fullSkills, compactSkills string) {
	t.Helper()
	// m-32k receives ModeCompact: folded pure example fence, intact "(MCP server: ext)", outlined project & compact skills.
	foundMCP32 := 0
	for _, spec := range req32.Tools {
		if !strings.HasPrefix(spec.Name, "mcp__ext__") {
			continue
		}
		foundMCP32++
		if !strings.Contains(spec.Description, "[example omitted]") {
			t.Fatalf("32k tool %s did not fold verified pure example fence: %q", spec.Name, spec.Description)
		}
		if !strings.HasSuffix(spec.Description, "(MCP server: ext)") {
			t.Fatalf("32k tool %s lost MCP provenance suffix: %q", spec.Name, spec.Description)
		}
	}
	if foundMCP32 != 40 {
		t.Fatalf("32k MCP tool count = %d, want 40", foundMCP32)
	}
	if req32.Messages[2].Content != compactProj {
		t.Fatalf("32k project layer len=%d, want compact outline len=%d", len(req32.Messages[2].Content), len(compactProj))
	}
	if req32.Messages[3].Content != compactSkills || len(req32.Messages[3].Content) > 8192 {
		t.Fatalf("32k skills layer len=%d, want compact <= 8192", len(req32.Messages[3].Content))
	}

	// m-200k receives 100% unmutated ModeFull: original 1 KiB JSON fence, intact suffix, full 40 KiB project & full skills.
	for _, spec := range req200.Tools {
		if !strings.HasPrefix(spec.Name, "mcp__ext__") {
			continue
		}
		if strings.Contains(spec.Description, "[example omitted]") || !strings.Contains(spec.Description, "```json") {
			t.Fatalf("200k tool %s was polluted by compact folding: %q", spec.Name, spec.Description)
		}
		if !strings.HasSuffix(spec.Description, "(MCP server: ext)") {
			t.Fatalf("200k tool %s lost MCP provenance suffix: %q", spec.Name, spec.Description)
		}
	}
	if req200.Messages[2].Content != fullProj {
		t.Fatalf("200k project layer len=%d, want full len=%d", len(req200.Messages[2].Content), len(fullProj))
	}
	if req200.Messages[3].Content != fullSkills {
		t.Fatalf("200k skills layer len=%d, want full len=%d", len(req200.Messages[3].Content), len(fullSkills))
	}
}

func assertPrefixDecisions(t *testing.T, decisions []Decision, want []windowPrefixOutput) {
	t.Helper()
	var got []windowPrefixOutput
	for _, d := range decisions {
		if d.Rule != RuleWindowPrefix {
			continue
		}
		var out windowPrefixOutput
		if err := json.Unmarshal(d.Output, &out); err != nil {
			t.Fatal(err)
		}
		got = append(got, out)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("RuleWindowPrefix sequence = %+v, want %+v", got, want)
	}
}

// TestT2_DemandDrivenMonotonicSingleStepCompactRecovery exercises ADR-0024 §6 T2:
// Step 1 fits ModeFull, Step 2 tool output (~42K tokens) only fits under ModeCompact (newest_group_fit),
// Step 3 normal 300-token tool output returns immediately to ModeFull (fits_full) without latching.
func TestT2_DemandDrivenMonotonicSingleStepCompactRecovery(t *testing.T) {
	const ref64k = config.ModelRef("test/m-64k")
	// Build a ~12K fullFixed vs ~3K compactFixed prefix so fullFixed <= usableFull/2 (~23K),
	// while Step 2's newest group (~38K tokens) satisfies:
	// fullFixed(12K) + 38K = 50K > usableFull(46.2K), AND compactFixed(3K) + 38K = 41K <= usableCompact(52K)!
	fullProj := "[project instructions: AGENTS.md]\n" + strings.Repeat("word ", 5400) // ~27 KB -> ~9K tokens
	compactProj := "[project instructions: AGENTS.md]\nOutline summary."
	fullSkills := "Available skills:\n" + strings.Repeat("skill description words ", 800) // ~19 KB -> ~6.4K tokens
	compactSkills := "Available skills (compact):\n- s1"
	layers := []SystemLayer{
		{Kind: "project-instructions", Source: "AGENTS.md", Text: fullProj, CompactText: compactProj},
		{Kind: "skills", Source: "skills", Text: fullSkills, CompactText: compactSkills},
	}

	stepNum := 0
	tools := &catalogToolsFake{
		entries: []toolset.CatalogTool{{
			Spec:   protocol.ToolSpec{Name: "read_file", Description: "Read file.", Permission: "read"},
			Source: "builtin",
		}},
		execFn: func(_ context.Context, _ string, _ map[string]any) (protocol.ToolResult, error) {
			stepNum++
			if stepNum == 1 {
				// Step 1 tool output read on Step 2: ~110 KB ASCII letters (~37K tokens)
				return protocol.ToolResult{OK: true, Content: strings.Repeat("abcd ", 22500)}, nil
			}
			// Step 2 tool output read on Step 3: ~900 bytes (~300 tokens)
			return protocol.ToolResult{OK: true, Content: strings.Repeat("small output ", 70)}, nil
		},
	}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		toolStep(call("c1", "read_file", `{"path":"big.txt"}`)),
		toolStep(call("c2", "read_file", `{"path":"small.txt"}`)),
		answerOf("all three steps complete"),
	}}
	m64 := testModel(ref64k, provider)
	m64.Context, m64.Output = 64000, 4096

	store := envStore("t2")
	loop := Loop{Models: map[config.ModelRef]Model{ref64k: m64}, Store: store, Tools: tools}
	res, err := loop.Run(context.Background(), "t2", Options{
		Session:      ref64k,
		SystemLayers: layers,
		MaxSteps:     5,
	})
	if err != nil || res.Status != StatusCompleted {
		t.Fatalf("T2 Run result=%+v err=%v", res, err)
	}
	steps := replayAll(t, store)
	if len(steps) != 3 {
		t.Fatalf("steps=%d, want 3", len(steps))
	}
	want := []windowPrefixOutput{
		{Mode: ModeFull, Reason: PrefixReasonFitsFull},
		{Mode: ModeCompact, Reason: PrefixReasonNewestGroupFit},
		{Mode: ModeFull, Reason: PrefixReasonFitsFull},
	}
	for i, st := range steps {
		assertPrefixDecisions(t, st.Decisions, want[i:i+1])
	}
}

// TestT3A_RepairFloorFailoverAndControlledWindowOverflow exercises ADR-0024 §6 T3 Subset A (A1 & A2).
func TestT3A_RepairFloorFailoverAndControlledWindowOverflow(t *testing.T) {
	t.Run("A1_small_candidate_overflows_repair_floor_and_fails_over_to_200k", func(t *testing.T) {
		const (
			ref32k  = config.ModelRef("test/m-32k")
			ref200k = config.ModelRef("test/m-200k")
		)
		store := envStore("t3a1")
		// Seed: user (seq 0) -> assistant+tool write_file with 28K-token content (seq 1, 2) ->
		// assistant proposal (seq 3) -> OriginCompletionBlocked (seq 4) -> small read_file group (seq 5, 6).
		// Notice: newest group (seq 5, 6) is tiny (~50 tokens), so without MinRequiredSeq=1 m-32k would
		// silently drop seq 1..2 (the 28K edit context) and send only seq 3..6! With MinRequiredSeq=1,
		// m-32k MUST overflow and fail over to m-200k, which sends seq 0..6 intact!
		bigEditResult := strings.Repeat("edited line content ", 4200) // ~84 KB -> ~28K tokens (> m-32k usable ~24K)
		req1, err := store.AdmitRequest(context.Background(), session.RequestAdmission{
			RequestID: "req-seed-1", SessionID: "t3a1", Purpose: session.PurposeMain,
			FirstSeq: ptrInt64(0), LastSeq: ptrInt64(0), ProjectionVersion: 2, LineageComplete: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		assistantEdit := protocol.Message{
			Role:      "assistant",
			ToolCalls: []protocol.ToolCall{call("w1", "write_file", `{"path":"a.go","content":"v2"}`)},
		}
		seq1, err := store.AppendAnswered(context.Background(), "t3a1", req1.RequestID, assistantEdit)
		if err != nil || seq1 != 1 {
			t.Fatalf("seq1=%d err=%v", seq1, err)
		}
		preRef := "files:1:sha256:" + strings.Repeat("1", 64)
		postSnap := "files:1:sha256:" + strings.Repeat("2", 64)
		if err := store.AdmitExecution(context.Background(), session.ExecutionAdmission{
			SessionID: "t3a1", ExecutionID: "ex-seed-1", RequestID: req1.RequestID,
			ToolName: "write_file", ArgsDigest: "sha256:" + strings.Repeat("a", 64), PreRef: preRef,
		}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.CommitToolResult(context.Background(), ToolResultCommit{
			SessionID: "t3a1",
			Message:   protocol.Message{Role: "tool", ToolCallID: "w1", Name: "write_file", Content: bigEditResult},
			Execution: &session.ExecutionResult{
				ExecutionID: "ex-seed-1", Outcome: session.OutcomeComplete, Coverage: session.CoverageComplete,
				PostRef: postSnap + ";cm=proven",
			},
			Edges: []session.DerivedEdge{{
				Kind: "file", TargetRef: "a.go@" + strings.Repeat("2", 40),
				SourceKind: session.SourceRequest, SourceRef: req1.RequestID, OriginClass: session.OriginAgentGen,
			}},
		}); err != nil {
			t.Fatal(err)
		}
		// Proposal (seq 3) + OriginCompletionBlocked (seq 4) + small read_file group (seq 5, 6)
		_, _, _ = store.AppendMessage(context.Background(), "t3a1", protocol.Message{Role: "assistant", Content: "done proposal"}, nil)
		_, _, _ = store.AppendMessage(context.Background(), "t3a1", protocol.Message{Role: "user", Origin: protocol.OriginCompletionBlocked, Content: "check test failed"}, nil)
		_, _, _ = store.AppendMessage(context.Background(), "t3a1", protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{call("r1", "read_file", `{"path":"a.go"}`)}}, nil)
		_, _, _ = store.AppendMessage(context.Background(), "t3a1", protocol.Message{Role: "tool", ToolCallID: "r1", Name: "read_file", Content: "small"}, nil)

		p32 := &scriptedProvider{responses: []protocol.ProviderResponse{answerOf("should-not-answer")}}
		p200 := &scriptedProvider{responses: []protocol.ProviderResponse{answerOf("repaired-on-200k")}}
		m32 := testModel(ref32k, p32)
		m32.Context, m32.Output = 32768, 4096
		m200 := testModel(ref200k, p200)
		m200.Context, m200.Output = 200000, 8192

		loop := Loop{
			Models: map[config.ModelRef]Model{ref32k: m32, ref200k: m200},
			Store:  store,
			Tools:  fakeTools{},
		}
		res, err := loop.Run(context.Background(), "t3a1", Options{
			Session:  ref32k,
			Failover: []config.ModelRef{ref200k},
			MaxSteps: 2,
		})
		if err != nil || res.Status != StatusCompleted || res.Final != "repaired-on-200k" {
			t.Fatalf("A1 result=%+v err=%v", res, err)
		}
		if len(p32.seen) != 0 || len(p200.seen) != 1 {
			t.Fatalf("seen 32k=%d 200k=%d, want 0 and 1", len(p32.seen), len(p200.seen))
		}
		steps := replayAll(t, store)
		if steps[0].Window.FirstSeq > 1 {
			t.Fatalf("200k window FirstSeq=%d, want <= 1 (retaining proven edit group seq 1)", steps[0].Window.FirstSeq)
		}
		if !hasRule(steps[0].Decisions, RuleFailoverPrefix+"capacity") {
			t.Fatalf("decisions %+v missing capacity failover", steps[0].Decisions)
		}
	})

	t.Run("A2_largest_candidate_cannot_fit_80K_repair_zone_exits_KindWindowOverflow", func(t *testing.T) {
		const (
			ref32k = config.ModelRef("test/m-32k")
			ref64k = config.ModelRef("test/m-64k")
		)
		store := envStore("t3a2")
		// Prefix with old user rows before a fresh external user turn so r.groups WOULD trim if not clamped,
		// then external user u (seq 5) -> 80K-token Unknown bash group (seq 6, 7) -> proposal (seq 8) ->
		// OriginCompletionBlocked (seq 9) -> tiny read group (seq 10, 11).
		for i := 0; i < 4; i++ {
			_, _, _ = store.AppendMessage(context.Background(), "t3a2", protocol.Message{Role: "user", Content: strings.Repeat("old ", 800)}, nil)
		}
		uSeq, _, _ := store.AppendMessage(context.Background(), "t3a2", protocol.Message{Role: "user", Content: "new task"}, nil)
		reqU, err := store.AdmitRequest(context.Background(), session.RequestAdmission{
			RequestID: "req-a2", SessionID: "t3a2", Purpose: session.PurposeMain,
			FirstSeq: &uSeq, LastSeq: &uSeq, ProjectionVersion: 2, LineageComplete: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		seqBash, err := store.AppendAnswered(context.Background(), "t3a2", reqU.RequestID, protocol.Message{
			Role:      "assistant",
			ToolCalls: []protocol.ToolCall{call("b1", "bash", `{"command":"./gen.sh"}`)},
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = store.AdmitExecution(context.Background(), session.ExecutionAdmission{
			SessionID: "t3a2", ExecutionID: "ex-a2", RequestID: reqU.RequestID,
			ToolName: "bash", ArgsDigest: "sha256:" + strings.Repeat("b", 64), PreUnavailable: true,
		})
		huge80k := strings.Repeat("abcd ", 48000) // 240 KB -> ~80,004 tokens (> m-64k Context 64000!)
		_, _, _ = store.CommitToolResult(context.Background(), ToolResultCommit{
			SessionID: "t3a2",
			Message:   protocol.Message{Role: "tool", ToolCallID: "b1", Name: "bash", Content: huge80k},
			Execution: &session.ExecutionResult{ExecutionID: "ex-a2", Outcome: session.OutcomeComplete, Coverage: session.CoverageUnavailable},
		})
		_, _, _ = store.AppendMessage(context.Background(), "t3a2", protocol.Message{Role: "assistant", Content: "claiming done"}, nil)
		_, _, _ = store.AppendMessage(context.Background(), "t3a2", protocol.Message{Role: "user", Origin: protocol.OriginCompletionBlocked, Content: "gate rejected"}, nil)
		_, _, _ = store.AppendMessage(context.Background(), "t3a2", protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{call("r2", "read_file", `{"path":"x"}`)}}, nil)
		_, _, _ = store.AppendMessage(context.Background(), "t3a2", protocol.Message{Role: "tool", ToolCallID: "r2", Name: "read_file", Content: "tiny"}, nil)
		_ = seqBash

		p32 := &scriptedProvider{responses: []protocol.ProviderResponse{answerOf("never")}}
		p64 := &scriptedProvider{responses: []protocol.ProviderResponse{answerOf("never")}}
		m32 := testModel(ref32k, p32)
		m32.Context, m32.Output = 32768, 4096
		m64 := testModel(ref64k, p64)
		m64.Context, m64.Output = 64000, 4096

		loop := Loop{
			Models: map[config.ModelRef]Model{ref32k: m32, ref64k: m64},
			Store:  store,
			Tools:  fakeTools{},
		}
		res, err := loop.Run(context.Background(), "t3a2", Options{
			Session:  ref32k,
			Failover: []config.ModelRef{ref64k},
			MaxSteps: 2,
		})
		if err == nil || res.Status != StatusFailed || !errors.Is(err, window.ErrOverflow) {
			t.Fatalf("A2 result=%+v err=%v, want StatusFailed wrapping ErrOverflow", res, err)
		}
		assertExit(t, store, res, StatusFailed, KindWindowOverflow)
		if len(p32.seen) != 0 || len(p64.seen) != 0 {
			t.Fatalf("requests sent 32k=%d 64k=%d, want 0 (never drop protected repair evidence)", len(p32.seen), len(p64.seen))
		}
	})
}

type steerTestStore struct {
	*memoryStore
	steersMu sync.Mutex
	steers   []session.SteerMessage
}

func (s *steerTestStore) PendingSteers(_ context.Context, sessionID string, limit int) ([]session.SteerMessage, error) {
	s.steersMu.Lock()
	defer s.steersMu.Unlock()
	var out []session.SteerMessage
	for _, m := range s.steers {
		if m.SessionID == sessionID && m.AppliedAt == nil {
			out = append(out, m)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (s *steerTestStore) MarkSteerApplied(_ context.Context, sessionID string, seq, appliedSeq int64) error {
	s.steersMu.Lock()
	defer s.steersMu.Unlock()
	now := time.Now().UTC()
	for i := range s.steers {
		if s.steers[i].SessionID == sessionID && s.steers[i].Seq == seq {
			s.steers[i].AppliedAt = &now
			s.steers[i].AppliedSeq = appliedSeq
			return nil
		}
	}
	return fmt.Errorf("steer %d not found", seq)
}

func (s *steerTestStore) enqueueSteer(sessionID, body string) {
	s.steersMu.Lock()
	defer s.steersMu.Unlock()
	seq := int64(len(s.steers) + 1)
	s.steers = append(s.steers, session.SteerMessage{
		Seq:       seq,
		SessionID: sessionID,
		CommandID: fmt.Sprintf("cmd-%d", seq),
		Body:      body,
		CreatedAt: time.Now().UTC(),
	})
}

// TestT7_BeforeModelCallSteerRebuildsWindowClearsEchoAndUpdatesEnvelope exercises ADR-0024 §6 T7.
func TestT7_BeforeModelCallSteerRebuildsWindowClearsEchoAndUpdatesEnvelope(t *testing.T) {
	baseStore := envStore("t7")
	store := &steerTestStore{memoryStore: baseStore}

	echoPayload := protocol.NewEcho("openai-reasoning", []byte(strings.Repeat("thought ", 600)), 1600)
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{
			Message:      protocol.Message{ToolCalls: []protocol.ToolCall{call("c1", "bash", `{"command":"true"}`)}},
			FinishReason: "tool_calls",
			Echo:         echoPayload,
		},
		answerOf("steered completion"),
	}}

	loop := Loop{
		Models: testModels(provider),
		Store:  store,
		Tools:  fakeTools{},
		DebugBoundary: func(_ context.Context, b DebugBoundary) error {
			if b.Event == "BeforeModelCall" && b.Step == 2 {
				store.enqueueSteer("t7", "Focus on null pointer check in handler.go")
			}
			return nil
		},
	}
	res, err := loop.Run(context.Background(), "t7", Options{Session: testRef, MaxSteps: 4})
	if err != nil || res.Status != StatusCompleted || res.Final != "steered completion" {
		t.Fatalf("T7 result=%+v err=%v", res, err)
	}
	if len(provider.seen) != 2 {
		t.Fatalf("seen=%d, want 2", len(provider.seen))
	}
	step2Req := provider.seen[1]
	lastMsg := step2Req.Messages[len(step2Req.Messages)-1]
	if lastMsg.Role != "user" || lastMsg.Origin != protocol.OriginSteer || !strings.Contains(lastMsg.Content, "Focus on null pointer check") {
		t.Fatalf("Step 2 last message = %+v, want injected OriginSteer", lastMsg)
	}
	if step2Req.Echo != nil {
		t.Fatalf("Step 2 req.Echo = %+v, want nil after OriginSteer ended previous turn", step2Req.Echo)
	}
	envs := store.envelopes()
	if len(envs) != 2 || envs[1].LastSeq == nil {
		t.Fatalf("envelopes = %+v", envs)
	}
	// Transcript seqs: 0=goal user, 1=step1 assistant, 2=step1 tool, 3=step2 OriginSteer, 4=step2 final assistant.
	if *envs[1].LastSeq != 3 {
		t.Fatalf("Step 2 RequestEnvelope.LastSeq = %d, want 3 (the OriginSteer row seq)", *envs[1].LastSeq)
	}
}

// TestT8_TurnBoundaryDryRunZeroDBWritesAndMinimumProgressGuard exercises ADR-0024 §6 T8:
// Step 1 on m-200k (with 2K EchoTokens) produces [assistant + 14K-token tool_result].
// Step 2 m-200k returns 503; chain [m-200k, m-16k, m-128k]:
// m-16k dry-runs TurnBoundary with zeroed EchoTokens + Minimum Progress Guard -> overflows with 0 OriginTurn DB writes;
// m-128k commits exactly 1 OriginTurn row and sends the intact 14K tool group + OriginTurn.
func TestT8_TurnBoundaryDryRunZeroDBWritesAndMinimumProgressGuard(t *testing.T) {
	const (
		ref200k = config.ModelRef("test/m-200k")
		ref16k  = config.ModelRef("test/m-16k")
		ref128k = config.ModelRef("test/m-128k")
	)

	calls200 := 0
	p200 := &funcProvider{fn: func(_ context.Context, _ int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		calls200++
		if calls200 == 1 {
			return protocol.ProviderResponse{
				Message:      protocol.Message{ToolCalls: []protocol.ToolCall{call("t8-1", "bash", `{"command":"dump"}`)}},
				FinishReason: "tool_calls",
				Echo:         protocol.NewEcho("thinking", []byte(strings.Repeat("r", 6000)), 2000),
			}, nil
		}
		return protocol.ProviderResponse{}, &protocol.ProviderError{Class: protocol.ClassAvailability, Err: errors.New("503 service unavailable")}
	}}
	p16 := &scriptedProvider{responses: []protocol.ProviderResponse{answerOf("should-not-fit-16k")}}
	p128 := &scriptedProvider{responses: []protocol.ProviderResponse{answerOf("answered-by-128k")}}

	m200 := testModel(ref200k, p200)
	m200.Context, m200.Output = 200000, 8192
	m16 := testModel(ref16k, p16)
	m16.Context, m16.Output = 16384, 2048
	m128 := testModel(ref128k, p128)
	m128.Context, m128.Output = 128000, 4096

	// 14K-token tool result (~42 KB ASCII text): exceeds m-16k usable (~16384 - 2048 - 1024 - 1950 = 11,362),
	// while m-16k STILL passes static minimal prefix pre-screening!
	big14k := strings.Repeat("tool result payload ", 2100)
	tools := &catalogToolsFake{
		entries: []toolset.CatalogTool{{
			Spec:   protocol.ToolSpec{Name: "bash", Description: "Run shell.", Permission: "exec", MutatesWorkspace: true},
			Source: "builtin",
		}},
		execFn: func(_ context.Context, _ string, _ map[string]any) (protocol.ToolResult, error) {
			return protocol.ToolResult{OK: true, Content: big14k, MutatesWorkspace: true}, nil
		},
	}

	store := envStore("t8")
	loop := Loop{
		Models: map[config.ModelRef]Model{ref200k: m200, ref16k: m16, ref128k: m128},
		Store:  store,
		Tools:  tools,
	}
	res, err := loop.Run(context.Background(), "t8", Options{
		Session:  ref200k,
		Failover: []config.ModelRef{ref16k, ref128k},
		MaxSteps: 4,
	})
	if err != nil || res.Status != StatusCompleted || res.Final != "answered-by-128k" {
		t.Fatalf("T8 result=%+v err=%v", res, err)
	}
	if len(p16.seen) != 0 || len(p128.seen) != 1 {
		t.Fatalf("seen 16k=%d 128k=%d, want 0 and 1", len(p16.seen), len(p128.seen))
	}

	// Count OriginTurn rows persisted in store: MUST be exactly 1 (zero written during m-16k dry-run!).
	turnCount := 0
	for _, msg := range store.messages {
		if msg.Origin == protocol.OriginTurn {
			turnCount++
		}
	}
	if turnCount != 1 {
		t.Fatalf("persisted OriginTurn rows = %d, want 1", turnCount)
	}

	// Verify m-16k survived static prefix pre-screening and triggered capacity failover in Step 2!
	steps := replayAll(t, store)
	if len(steps) != 2 {
		t.Fatalf("steps=%d, want 2", len(steps))
	}
	var candsOut candidatesOutput
	if err := decodeDecision(steps[1].Decisions[0], new(candidatesInputs), &candsOut); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(candsOut.Candidates, []config.ModelRef{ref200k, ref16k, ref128k}) {
		t.Fatalf("Step 2 static candidates = %v, want [m-200k m-16k m-128k]", candsOut.Candidates)
	}
	if !hasRule(steps[1].Decisions, RuleFailoverPrefix+"availability") || !hasRule(steps[1].Decisions, RuleFailoverPrefix+"capacity") {
		t.Fatalf("Step 2 decisions = %+v, want availability failover (200k->16k) then capacity failover (16k->128k)", steps[1].Decisions)
	}

	// Verify m-128k received the 14K tool output AND the committed OriginTurn!
	req128 := p128.seen[0]
	found14k := false
	for _, msg := range req128.Messages {
		if msg.Role == "tool" && msg.Content == big14k {
			found14k = true
		}
	}
	if !found14k {
		t.Fatal("m-128k request dropped the 14K tool output")
	}
	if last := req128.Messages[len(req128.Messages)-1]; last.Origin != protocol.OriginTurn {
		t.Fatalf("m-128k last message = %+v, want OriginTurn", last)
	}

	// Subcase: verify cloneForTurnPreview zeroes trailing EchoTokens so a 9K tool output + 4K old EchoTokens
	// DOES fit m-16k (usableCompact ~11.3K) during TurnBoundary dry-run instead of false-overflowing!
	t.Run("echo_tokens_zeroed_on_turn_boundary_preview", func(t *testing.T) {
		r := &run{}
		r.appendRow(0, protocol.Message{Role: "user", Content: "start"})
		r.appendAssistant(1, protocol.Message{
			Role:      "assistant",
			ToolCalls: []protocol.ToolCall{call("e1", "bash", `{"command":"ls"}`)},
		}, ref200k, protocol.NewEcho("thinking", []byte("x"), 4500))
		r.appendRow(2, protocol.Message{Role: "tool", ToolCallID: "e1", Name: "bash", Content: strings.Repeat("abcd ", 5400)}) // ~9K tokens
		preview, floorSeq := r.cloneForTurnPreview()
		if floorSeq != 1 {
			t.Fatalf("floorSeq = %d, want 1", floorSeq)
		}
		if preview[1].EchoTokens != 0 {
			t.Fatalf("preview assistant EchoTokens = %d, want 0", preview[1].EchoTokens)
		}
		if r.transcript[1].EchoTokens != 4500 {
			t.Fatalf("original r.transcript EchoTokens mutated to %d, want 4500", r.transcript[1].EchoTokens)
		}
		cand16 := Candidate{Ref: ref16k, Context: 16384, Output: 2048, ToolCalling: true}
		canonical := canonicalStepInput{
			SystemPrompt: "You are an agent.",
			State:        protocol.Message{Role: "user", Content: "Goal: test"},
		}
		proj, _, err := projectCandidate(r, cand16, m16, Options{}, canonical, preview, deriveRepairContext(preview, nil), floorSeq)
		if err != nil {
			t.Fatalf("expected 9K group (with 4.5K EchoTokens zeroed) to fit m-16k, got err=%v", err)
		}
		if proj.Window.FirstSeq != 0 {
			t.Fatalf("FirstSeq = %d, want 0", proj.Window.FirstSeq)
		}
	})
}

func ptrInt64(v int64) *int64 { return &v }

// TestProjectCandidate8KModelClampsCompactSlackWithoutFalseOverflow is the regression test for Finding C-F01:
// when !repair.Active and an 8K model passes static prefix screening with 0 <= poolBeforeSlack - compactFixed - requiredTranscript < 1950,
// ModeCompact clamps EffectiveGateSlack to the remaining room (dropping optional older groups and reserving all remaining room)
// instead of false-overflowing Step 1; when compactFixed + requiredTranscript > poolBeforeSlack, it still returns window.ErrOverflow.
func TestProjectCandidate8KModelClampsCompactSlackWithoutFalseOverflow(t *testing.T) {
	const ref8k = config.ModelRef("test/m-8k")
	m8 := Model{Ref: ref8k, Context: 8000, Output: 1000, ToolCalling: true}
	cand8 := Candidate{Ref: ref8k, Context: 8000, Output: 1000, ToolCalling: true}

	// Build a ~4,600-token fixed prefix so poolBeforeSlack (6488) - compactFixed (~4600) - newest (~50) ≈ 1838 < 1950.
	canonical := canonicalStepInput{
		SystemPrompt: strings.Repeat("word ", 2700), // ~13.5 KB -> ~4,504 tokens
		State:        protocol.Message{Role: "user", Content: "Goal: stay read-only"},
	}
	r := &run{}
	r.appendRow(0, protocol.Message{Role: "user", Content: strings.Repeat("older optional row ", 80)})
	r.appendRow(1, protocol.Message{Role: "user", Content: "newest user prompt"})

	proj, _, err := projectCandidate(r, cand8, m8, Options{}, canonical, r.transcript, deriveRepairContext(r.transcript, nil), -1)
	if err != nil {
		t.Fatalf("expected 8K model with positive post-prefix room to succeed in ModeCompact, got err=%v", err)
	}
	if proj.Mode != ModeCompact || proj.EffectiveGateSlack <= 0 || proj.EffectiveGateSlack >= proj.BaseGateSlack {
		t.Fatalf("proj = {Mode:%s EffSlack:%d BaseSlack:%d}, want ModeCompact with 0 < EffSlack < BaseSlack", proj.Mode, proj.EffectiveGateSlack, proj.BaseGateSlack)
	}
	if proj.Window.FirstSeq != 1 || proj.Window.Dropped != 1 {
		t.Fatalf("expected optional older group dropped so 100%% of remaining room stays gate slack, got FirstSeq=%d Dropped=%d", proj.Window.FirstSeq, proj.Window.Dropped)
	}

	// Now make newest group exceed poolBeforeSlack - compactFixed: MUST still return window.ErrOverflow!
	rOverflow := &run{}
	rOverflow.appendRow(0, protocol.Message{Role: "user", Content: strings.Repeat("abcd ", 1500)}) // ~2,504 tokens > 1,888 room
	if _, _, err := projectCandidate(rOverflow, cand8, m8, Options{}, canonical, rOverflow.transcript, deriveRepairContext(rOverflow.transcript, nil), -1); !errors.Is(err, window.ErrOverflow) {
		t.Fatalf("expected true overflow when compactFixed + newestGroup > poolBeforeSlack, got err=%v", err)
	}
}

// TestQGR01_BeforeModelCallOverflowRecoversAndNeverOrphansOriginTurn verifies Finding QGR-01:
// when a primary large candidate fails transiently (503) and a smaller fallback pauses at BeforeModelCall
// where an InboxEdit enlarges TaskState past the fallback's window while TurnBoundary is due, zero orphan
// OriginTurn rows are written to SQLite, the primary's in-turn Echo is preserved, and outage recovery
// returns to the primary candidate instead of aborting with KindWindowOverflow.
func TestQGR01_BeforeModelCallOverflowRecoversAndNeverOrphansOriginTurn(t *testing.T) {
	const (
		ref200k = config.ModelRef("test/m-200k")
		ref16k  = config.ModelRef("test/m-16k")
	)
	store := envStore("qgr01")

	// Step 1: m-200k emits a ~10.8K-token tool result + Echo (fits m-16k pre-hook ~11.3K with ~500 tokens spare).
	// Step 2 first attempt on m-200k: transient 503 -> failover to m-16k (TurnBoundary due == true).
	// BeforeModelCall on m-16k: host queues a 4KB Brief + Note edit (~2,300 tokens), overflowing m-16k!
	// Expected: 0 OriginTurn rows written, recovery retries m-200k, which receives its Step 1 Echo intact!
	var step2Calls200k int
	var recoveredReq protocol.ProviderRequest
	p200 := &funcProvider{fn: func(_ context.Context, n int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if n == 1 {
			return protocol.ProviderResponse{
				Message:      protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{call("c1", "read_file", `{"path":"payload.txt"}`)}},
				FinishReason: "tool_calls",
				Echo:         protocol.NewEcho("thinking", []byte(`{"sig":"keep-me"}`), 400),
			}, nil
		}
		step2Calls200k++
		if step2Calls200k == 1 {
			return protocol.ProviderResponse{}, &protocol.ProviderError{Class: protocol.ClassAvailability, Err: errors.New("transient 503")}
		}
		recoveredReq = req
		return answerOf("recovered-on-200k"), nil
	}}
	p16 := &funcProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		t.Fatal("m-16k should have overflowed after BeforeModelCall InboxEdit")
		return protocol.ProviderResponse{}, nil
	}}

	m200 := testModel(ref200k, p200)
	m200.Context, m200.Output = 200000, 4096
	m16 := testModel(ref16k, p16)
	m16.Context, m16.Output = 16384, 2048

	tools := &catalogToolsFake{
		entries: []toolset.CatalogTool{{
			Spec:   protocol.ToolSpec{Name: "read_file", Description: "Read file.", Permission: "read"},
			Source: "builtin",
		}},
		execFn: func(_ context.Context, _ string, _ map[string]any) (protocol.ToolResult, error) {
			return protocol.ToolResult{OK: true, Content: strings.Repeat("word ", 6000)}, nil // ~10.0K tokens (comfortably fits m-16k pre-hook, overflows post-hook with 10KB TaskState)
		},
	}

	var injected bool
	loop := Loop{
		Models: map[config.ModelRef]Model{ref200k: m200, ref16k: m16},
		Store:  store,
		Tools:  tools,
		DebugBoundary: func(_ context.Context, b DebugBoundary) error {
			if b.Event == "BeforeModelCall" && b.Step == 2 && step2Calls200k == 1 && !injected {
				injected = true
				bigBrief := strings.Repeat("constraint ", 360) // ~3,960B
				note := strings.Repeat("note ", 400)           // ~2,000B each -> fills 10,240B MaxBlockBytes (~3,420 tokens)
				store.mu.Lock()
				store.inbox = append(store.inbox,
					taskstate.QueuedEdit{Seq: 1, Edit: taskstate.Edit{Brief: &bigBrief}},
					taskstate.QueuedEdit{Seq: 2, Edit: taskstate.Edit{Reference: &taskstate.ReferenceEdit{Kind: "file", Target: "p1.txt", Note: &note, State: taskstate.RefIncluded}}},
					taskstate.QueuedEdit{Seq: 3, Edit: taskstate.Edit{Reference: &taskstate.ReferenceEdit{Kind: "file", Target: "p2.txt", Note: &note, State: taskstate.RefIncluded}}},
					taskstate.QueuedEdit{Seq: 4, Edit: taskstate.Edit{Reference: &taskstate.ReferenceEdit{Kind: "file", Target: "p3.txt", Note: &note, State: taskstate.RefIncluded}}},
				)
				store.mu.Unlock()
			}
			return nil
		},
	}

	res, err := loop.Run(context.Background(), "qgr01", Options{
		Session:  ref200k,
		Failover: []config.ModelRef{ref16k},
		MaxSteps: 4,
		Recovery: Recovery{BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, MaxRetries: 3, MaxOutage: 5 * time.Second},
	})
	if !injected {
		t.Fatal("m-16k did not reach BeforeModelCall in Step 2")
	}
	if err != nil || res.Status != StatusCompleted || res.Final != "recovered-on-200k" {
		t.Fatalf("QGR-01 run result=%+v err=%v, want completed on recovered m-200k", res, err)
	}
	for _, msg := range store.messages {
		if msg.Origin == protocol.OriginTurn {
			t.Fatalf("orphan OriginTurn row committed when m-16k overflowed post-BeforeModelCall: %+v", store.messages)
		}
	}
	hasEcho := false
	for _, e := range recoveredReq.Echo {
		if !e.IsZero() {
			hasEcho = true
		}
	}
	if !hasEcho {
		t.Fatalf("m-200k lost its in-turn reasoning Echo across aborted m-16k TurnBoundary preview: echo=%+v", recoveredReq.Echo)
	}
}

// TestQGR03_OriginImportedToolGroupClassifiesAsMutationUnknown verifies Finding QGR-03:
// imported tool-call groups (OriginImported, which have no tool_executions rows) classify conservatively
// as MutationUnknown rather than MutationNone so they lock preBlockFloor and never falsely advance MinRequiredSeq.
func TestQGR03_OriginImportedToolGroupClassifiesAsMutationUnknown(t *testing.T) {
	importedAssistant := protocol.Message{
		Role:      "assistant",
		Origin:    protocol.OriginImported,
		ToolCalls: []protocol.ToolCall{call("imp1", "bash", `{"command":"make build"}`)},
	}
	importedTool := protocol.Message{
		Role:       "tool",
		Origin:     protocol.OriginImported,
		ToolCallID: "imp1",
		Name:       "bash",
		Content:    "ok",
	}
	if got := AggregateGroupMutation(importedAssistant, []protocol.Message{importedTool}, nil); got != MutationUnknown {
		t.Fatalf("AggregateGroupMutation(OriginImported) = %q, want %q", got, MutationUnknown)
	}
}
