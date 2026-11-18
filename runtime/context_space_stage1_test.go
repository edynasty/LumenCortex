package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/edynasty/LumenCortex/internal/instructions"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/skills"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/protocol"
)

type funcProvider struct {
	fn func(ctx context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error)
}

func (p funcProvider) Complete(ctx context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	return p.fn(ctx, req)
}

func (p funcProvider) Model() string { return "func" }

// TestT5_ProjectInstructionsOutlineAndAnchoredRead verifies T5:
// 36 KiB fenced AGENTS.md (with ```bash, ~~~py, and unclosed fence fake headings + 6 real headings)
// and 24 KiB headingless AGENTS.md, read_file(..., anchors=true) exact 1-based line alignment under
// PolicyReadOnly, <globalDir>/AGENTS.md sandbox rejection, and unchanged file SHA-256.
func TestT5_ProjectInstructionsOutlineAndAnchoredRead(t *testing.T) {
	f := newPromptFixture(t, true)
	globalAgents := filepath.Join(f.globalDir, instructions.FileName)
	writeFile(t, globalAgents, "Global immutable security policy.\n"+strings.Repeat("Keep global intact.\n", 800))

	var lines []string
	lines = append(lines, "Preamble line 1", "Preamble line 2")
	realHeadings := []string{
		"# Section 1 Architecture",
		"## Section 2 Boundaries",
		"### Section 3 Tool Rules",
		"## Section 4 Recovery",
		"## Section 5 Testing",
		"## Section 6 Release",
	}
	var expectedHeadingLines []int
	for idx, h := range realHeadings {
		expectedHeadingLines = append(expectedHeadingLines, len(lines)+1)
		lines = append(lines, h)
		for j := range 95 {
			lines = append(lines, fmt.Sprintf("Body %d line %d: %s", idx+1, j+1, strings.Repeat("rule-detail ", 5)))
		}
		switch idx {
		case 0:
			lines = append(lines, "```bash", "# Fake Heading In Backtick Fence", "## Another Fake", "echo ok", "```")
		case 2:
			lines = append(lines, "~~~py", "# Fake Heading In Tilde Fence", "print('hi')", "~~~")
		case 4:
			// Unclosed fence in middle section? Wait: an unclosed fence runs to EOF in CommonMark, so place
			// > 2 KiB of code lines inside the unclosed fence at idx == 5 so the 2 KiB verbatim tail holds code,
			// proving the outline table itself contains zero fake headings!
		case 5:
			lines = append(lines, "```sh", "# Fake Heading In Unclosed Fence")
			for k := range 45 {
				lines = append(lines, fmt.Sprintf("echo unclosed_fence_body_line_%02d_%s", k, strings.Repeat("x", 40)))
			}
		}
	}
	fencedDoc := strings.Join(lines, "\n") + "\n"
	if len(fencedDoc) < 36<<10 {
		t.Fatalf("expected >= 36 KiB AGENTS.md, got %d bytes", len(fencedDoc))
	}
	projectAgents := filepath.Join(f.workspace, instructions.FileName)
	writeFile(t, projectAgents, fencedDoc)
	beforeSum := fileSHA256(t, projectAgents)

	assembled := assemblePrompt(promptInput{
		workspace: f.workspace,
		globalDir: f.globalDir,
		trusted:   true,
		base:      "base",
		skills:    f.engine.skillRegistry,
	})
	sysLayers := systemLayers(assembled.Layers)
	var globalLayer, projLayer *struct {
		text, compact string
	}
	for _, l := range sysLayers {
		switch l.Kind {
		case instructions.KindGlobal:
			globalLayer = &struct{ text, compact string }{l.Text, l.CompactText}
		case instructions.KindProject:
			projLayer = &struct{ text, compact string }{l.Text, l.CompactText}
		}
	}
	if globalLayer == nil || globalLayer.compact != "" {
		t.Fatalf("global instructions must never have CompactText: %+v", globalLayer)
	}
	if projLayer == nil || projLayer.compact == "" {
		t.Fatalf("expected non-empty CompactText for 36 KiB project AGENTS.md")
	}
	if strings.Contains(projLayer.compact, "Fake Heading") {
		t.Fatalf("fence fake heading leaked into compact outline:\n%s", projLayer.compact)
	}
	if !strings.Contains(projLayer.compact, "anchors=true") {
		t.Fatalf("compact outline missing anchors=true guidance:\n%s", projLayer.compact)
	}
	for i, h := range realHeadings {
		startLine := expectedHeadingLines[i]
		wantPrefix := fmt.Sprintf("- L%d-", startLine)
		if !strings.Contains(projLayer.compact, wantPrefix) || !strings.Contains(projLayer.compact, h) {
			t.Fatalf("missing real heading %q at %s in compact outline:\n%s", h, wantPrefix, projLayer.compact)
		}
	}

	// Execute real read_file(..., anchors=true) under PolicyReadOnly and verify exact line alignment & sandbox.
	ts, err := toolset.New(toolset.Options{
		Workspace: f.workspace,
		Policy:    toolset.PolicyReadOnly,
	})
	if err != nil {
		t.Fatal(err)
	}
	sec3Line := expectedHeadingLines[2]
	readRes, err := ts.Execute(context.Background(), "read_file", map[string]any{
		"path":       "AGENTS.md",
		"start_line": float64(sec3Line),
		"end_line":   float64(sec3Line + 2),
		"anchors":    true,
	}, nil)
	if err != nil || !readRes.OK {
		t.Fatalf("read_file(anchors=true) failed: res=%+v err=%v", readRes, err)
	}
	if !strings.Contains(readRes.Content, fmt.Sprintf("\n%d:", sec3Line)) || !strings.Contains(readRes.Content, realHeadings[2]) {
		t.Fatalf("anchored read_file line mismatch for L%d:\n%s", sec3Line, readRes.Content)
	}

	// Reading <globalDir>/AGENTS.md via read_file must be blocked by workspace sandbox.
	if escapeRes, escapeErr := ts.Execute(context.Background(), "read_file", map[string]any{
		"path":    globalAgents,
		"anchors": true,
	}, nil); escapeErr == nil && escapeRes.OK {
		t.Fatalf("sandbox allowed reading globalDir AGENTS.md: %+v", escapeRes)
	}
	if afterSum := fileSHA256(t, projectAgents); afterSum != beforeSum {
		t.Fatalf("AGENTS.md SHA256 mutated: %s -> %s", beforeSum, afterSum)
	}

	// Verify 24 KiB headingless AGENTS.md falls back to 200-line chunk index.
	var plainLines []string
	for i := range 450 {
		plainLines = append(plainLines, fmt.Sprintf("Plain instruction line %03d: %s", i+1, strings.Repeat("alpha ", 8)))
	}
	plainDoc := strings.Join(plainLines, "\n") + "\n"
	if len(plainDoc) < 24<<10 {
		t.Fatalf("expected >= 24 KiB plain AGENTS.md, got %d", len(plainDoc))
	}
	writeFile(t, projectAgents, plainDoc)
	plainAssembled := assemblePrompt(promptInput{
		workspace: f.workspace, globalDir: f.globalDir, trusted: true, base: "base",
	})
	plainLayers := systemLayers(plainAssembled.Layers)
	var plainCompact string
	for _, l := range plainLayers {
		if l.Kind == instructions.KindProject {
			plainCompact = l.CompactText
		}
	}
	if !strings.Contains(plainCompact, "- L1-L200:") || !strings.Contains(plainCompact, "- L201-L400:") || !strings.Contains(plainCompact, "- L401-L450:") || !strings.Contains(plainCompact, "anchors=true") {
		t.Fatalf("unexpected headingless 200-line chunk outline:\n%s", plainCompact)
	}
}

// TestT10_SkillSnapshotLifecyclePaginationAndConcurrency verifies T10 (REV-F02 & RC33-F02) end-to-end
// through real Engine.RunAgent -> Loop.Run tool execution, Resume snapshot refresh, untrusted withholding,
// >16 KiB single-entry truncation, and 8-goroutine concurrent -race stress.
func TestT10_SkillSnapshotLifecyclePaginationAndConcurrency(t *testing.T) {
	ctx := context.Background()

	// Verify fitSingleCatalogEntry handles an oversized >16 KiB entry cleanly with DescriptionTruncated=true.
	oversizedReader := newSkillReader([]skills.SnapshottedSkill{{
		Skill: skills.Skill{
			ID:          "skill-oversized",
			Name:        "Oversized Skill",
			Scope:       skills.ScopeGlobal,
			Description: strings.Repeat("<>&\"\\\n", 4000), // > 24 KiB when JSON-escaped
		},
		Content: "oversized body",
	}}).(*skillReader)
	overPage, err := oversizedReader.ListSkills(ctx, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	overRaw, err := json.Marshal(overPage)
	if err != nil || len(overRaw) > toolset.MaxSkillCatalogResponseBytes {
		t.Fatalf("oversized page len=%d err=%v", len(overRaw), err)
	}
	if len(overPage.Skills) != 1 || !overPage.Skills[0].DescriptionTruncated || overPage.Skills[0].ID != "skill-oversized" {
		t.Fatalf("unexpected oversized fallback page: %+v", overPage)
	}

	// Build workspace + 3 global roots totaling 240 enabled global skills + 3 disabled + 2 shadowed + 5 project skills.
	workspace := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(workspace); err == nil {
		workspace = resolved
	}
	home := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	useGlobalConfig(t, strings.ReplaceAll(
		modelConfig(`"trustedWorkspaces": [`+quoteJSON(workspace)+`]`),
		`"context": 8000`, `"context": 128000`,
	))
	globalDir, err := globalDirPath()
	if err != nil {
		t.Fatal(err)
	}
	rootLC := filepath.Join(globalDir, "skills")
	rootAgents := filepath.Join(home, ".agents", "skills")
	rootClaude := filepath.Join(home, ".claude", "skills")

	escaped1K := strings.Repeat(`a<>&"b `, 140) // ~980B raw, >2 KiB JSON-escaped -> exercises 16 KiB page cap!
	// 80 enabled + 3 to-be-disabled in rootLC (83 <= 128)
	for i := 1; i <= 80; i++ {
		id := fmt.Sprintf("skill-%03d", i)
		desc := fmt.Sprintf("global lc skill %03d", i)
		if i <= 50 {
			desc = fmt.Sprintf("escaped-%03d %s", i, escaped1K)
		}
		writeFile(t, filepath.Join(rootLC, id, "SKILL.md"), fmt.Sprintf("---\nname: %s\ndescription: %q\n---\nINITIAL_BODY_%s", id, desc, id))
	}
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("skill-disabled-%d", i)
		writeFile(t, filepath.Join(rootLC, id, "SKILL.md"), fmt.Sprintf("---\ndescription: disabled %d\n---\nDISABLED_BODY_%d", i, i))
	}
	// 80 enabled in rootAgents + 2 shadowed duplicates of skill-001/skill-002 (82 <= 128)
	for i := 81; i <= 160; i++ {
		id := fmt.Sprintf("skill-%03d", i)
		writeFile(t, filepath.Join(rootAgents, id, "SKILL.md"), fmt.Sprintf("---\ndescription: agents skill %03d\n---\nINITIAL_BODY_%s", i, id))
	}
	writeFile(t, filepath.Join(rootAgents, "skill-001", "SKILL.md"), "---\ndescription: shadowed 1\n---\nSHADOWED_1")
	writeFile(t, filepath.Join(rootAgents, "skill-002", "SKILL.md"), "---\ndescription: shadowed 2\n---\nSHADOWED_2")
	// 80 enabled in rootClaude (80 <= 128) -> total 240 enabled global skills!
	for i := 161; i <= 240; i++ {
		id := fmt.Sprintf("skill-%03d", i)
		writeFile(t, filepath.Join(rootClaude, id, "SKILL.md"), fmt.Sprintf("---\ndescription: claude skill %03d\n---\nINITIAL_BODY_%s", i, id))
	}
	// 5 project skills
	for i := 1; i <= 5; i++ {
		id := fmt.Sprintf("proj-skill-%d", i)
		writeFile(t, filepath.Join(workspace, ".lumencortex", "skills", id, "SKILL.md"), fmt.Sprintf("---\ndescription: project skill %d\n---\nPROJ_BODY_%d", i, i))
	}

	engine, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	for i := 1; i <= 3; i++ {
		if err := engine.SetSkillEnabled(skills.ScopeGlobal, fmt.Sprintf("skill-disabled-%d", i), false); err != nil {
			t.Fatal(err)
		}
	}

	sess := newTestSession(t, engine, "Verify immutable skill snapshot")

	// Start 8 background goroutines mutating & reading Registry concurrently during Run 1 (-race check, step 10).
	stopRace := make(chan struct{})
	var raceWG sync.WaitGroup
	for g := range 8 {
		raceWG.Add(1)
		go func(w int) {
			defer raceWG.Done()
			id := fmt.Sprintf("race-ephem-%d", w)
			for {
				select {
				case <-stopRace:
					_ = engine.DeleteSkill(skills.ScopeProject, id)
					return
				default:
					_ = engine.SaveSkill(skills.ScopeProject, id, "---\ndescription: concurrent\n---\nrace body")
					_, _ = engine.Skills(skills.ScopeEffective)
					_, _ = engine.skillRegistry.Prompt(skills.PromptOptions{IncludeProject: true})
					_ = engine.DeleteSkill(skills.ScopeProject, id)
				}
			}
		}(g)
	}

	// Clean up race goroutines before deterministic Run 1 assertions? Wait: keep them running DURING Run 1
	// on distinct race-ephem-* IDs after Run 1's prepareRun snapshot has been captured!
	var stepNum atomic.Int32
	var run1Pages [10][]string
	var curPass int
	var curOffset int
	var passIDs []string
	mutatedMidRun := false

	run1Provider := funcProvider{fn: func(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		n := int(stepNum.Add(1))
		if n == 1 {
			// Trigger first catalog page (offset=0).
			return toolCallResp("c1", "read_skill", `{"offset":0}`), nil
		}
		// Inspect previous tool result at tail of req.Messages.
		lastTool := req.Messages[len(req.Messages)-1]
		if !mutatedMidRun {
			// Steps ②–④: mutate Registry mid-run right after Run 1 started!
			mutatedMidRun = true
			if err := engine.SaveSkill(skills.ScopeGlobal, "skill-new", "---\ndescription: added mid run\n---\nNEW_BODY"); err != nil {
				return protocol.ProviderResponse{}, err
			}
			if err := engine.SaveSkill(skills.ScopeGlobal, "skill-001", "---\ndescription: overwritten mid run\n---\nOVERWRITTEN_BODY_001"); err != nil {
				return protocol.ProviderResponse{}, err
			}
			if err := engine.DeleteSkill(skills.ScopeGlobal, "skill-002"); err != nil {
				return protocol.ProviderResponse{}, err
			}
			if err := engine.SetSkillEnabled(skills.ScopeGlobal, "skill-003", false); err != nil {
				return protocol.ProviderResponse{}, err
			}
			if err := engine.SetSkillEnabled(skills.ScopeGlobal, "skill-disabled-1", true); err != nil {
				return protocol.ProviderResponse{}, err
			}
		}

		if curPass < 10 {
			if len(lastTool.Content) > toolset.MaxSkillCatalogResponseBytes {
				return protocol.ProviderResponse{}, fmt.Errorf("catalog response %d bytes > 16384", len(lastTool.Content))
			}
			var page toolset.SkillCatalogPage
			if err := json.Unmarshal([]byte(lastTool.Content), &page); err != nil {
				return protocol.ProviderResponse{}, fmt.Errorf("unmarshal page: %w (%s)", err, lastTool.Content)
			}
			if len(page.Skills) == 0 || len(page.Skills) > toolset.MaxSkillCatalogPage {
				return protocol.ProviderResponse{}, fmt.Errorf("invalid page count %d", len(page.Skills))
			}
			for _, sk := range page.Skills {
				passIDs = append(passIDs, sk.ID)
			}
			if page.HasMore {
				curOffset = page.NextOffset
				return toolCallResp(fmt.Sprintf("p%d_%d", curPass, curOffset), "read_skill", fmt.Sprintf(`{"offset":%d}`, curOffset)), nil
			}
			run1Pages[curPass] = passIDs
			passIDs = nil
			curPass++
			curOffset = 0
			if curPass < 10 {
				return toolCallResp(fmt.Sprintf("p%d_0", curPass), "read_skill", `{"offset":0}`), nil
			}
			// Step ⑦ + illegal mutual-exclusion checks in one batch!
			return protocol.ProviderResponse{
				FinishReason: "tool_calls",
				Message: protocol.Message{
					Role: "assistant",
					ToolCalls: []protocol.ToolCall{
						{ID: "r001", Name: "read_skill", Arguments: json.RawMessage(`{"id":"skill-001"}`)},
						{ID: "r002", Name: "read_skill", Arguments: json.RawMessage(`{"id":"skill-002"}`)},
						{ID: "rnew", Name: "read_skill", Arguments: json.RawMessage(`{"id":"skill-new"}`)},
						{ID: "rdis", Name: "read_skill", Arguments: json.RawMessage(`{"id":"skill-disabled-1"}`)},
						{ID: "rbad1", Name: "read_skill", Arguments: json.RawMessage(`{"id":""}`)},
						{ID: "rbad2", Name: "read_skill", Arguments: json.RawMessage(`{"id":"skill-001","offset":0}`)},
						{ID: "rbad3", Name: "read_skill", Arguments: json.RawMessage(`{"path":"SKILL.md"}`)},
					},
				},
			}, nil
		}

		return protocol.ProviderResponse{
			FinishReason: "stop",
			Message:      protocol.Message{Role: "assistant", Content: "Run 1 verification done."},
		}, nil
	}}

	ref1 := bindRef(engine, "a", run1Provider)
	res1, err := engine.RunAgent(ctx, sess.ID, AgentOptions{
		ModelRef: ref1,
		Input:    "Audit skills snapshot",
		MaxSteps: 220,
	})
	close(stopRace)
	raceWG.Wait()
	if err != nil || res1.Status != "completed" {
		t.Fatalf("Run 1 failed: res=%+v err=%v", res1, err)
	}

	// Verify 10 full pagination passes in Run 1 are strictly sorted and bit-identical, excluding race/mid-run additions!
	firstPass := run1Pages[0]
	if !slices.IsSorted(firstPass) {
		t.Fatalf("catalog IDs not sorted ascending: %v", firstPass)
	}
	if slices.Contains(firstPass, "skill-new") || slices.Contains(firstPass, "skill-disabled-1") {
		t.Fatalf("mid-run skills leaked into Run 1 catalog: %v", firstPass)
	}
	if !slices.Contains(firstPass, "skill-001") || !slices.Contains(firstPass, "skill-002") || !slices.Contains(firstPass, "skill-003") || !slices.Contains(firstPass, "proj-skill-1") {
		t.Fatalf("Run 1 initial eligible skills missing from catalog (count=%d)", len(firstPass))
	}
	for pass := 1; pass < 10; pass++ {
		if !slices.Equal(run1Pages[pass], firstPass) {
			t.Fatalf("pagination pass %d diverged from pass 0", pass)
		}
	}

	// Inspect stored tool results from Step ⑦ in Run 1.
	byCall := loadToolResultsByCallID(t, engine, sess.ID)
	if !strings.Contains(byCall["r001"], "INITIAL_BODY_skill-001") || strings.Contains(byCall["r001"], "OVERWRITTEN_BODY_001") {
		t.Fatalf("skill-001 tore mid-run: %s", byCall["r001"])
	}
	if !strings.Contains(byCall["r002"], "INITIAL_BODY_skill-002") {
		t.Fatalf("deleted skill-002 failed to read from Run 1 snapshot: %s", byCall["r002"])
	}
	for _, deniedID := range []string{"rnew", "rdis", "rbad1", "rbad2", "rbad3"} {
		if !strings.Contains(strings.ToLower(byCall[deniedID]), "error") && !strings.Contains(byCall[deniedID], "unknown skill") && !strings.Contains(byCall[deniedID], "cannot combine") && !strings.Contains(byCall[deniedID], "requires id") && !strings.Contains(byCall[deniedID], "must not be empty") {
			t.Fatalf("expected tool error for %s, got %q", deniedID, byCall[deniedID])
		}
	}

	// Step ⑧: Start a second run ("Resume" / continuation on same session) and verify fresh snapshot!
	run2Step := 0
	run2Provider := funcProvider{fn: func(_ context.Context, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		run2Step++
		if run2Step == 1 {
			return protocol.ProviderResponse{
				FinishReason: "tool_calls",
				Message: protocol.Message{
					Role: "assistant",
					ToolCalls: []protocol.ToolCall{
						{ID: "u_new", Name: "read_skill", Arguments: json.RawMessage(`{"query":"skill-new"}`)},
						{ID: "u_001", Name: "read_skill", Arguments: json.RawMessage(`{"id":"skill-001"}`)},
						{ID: "u_dis1", Name: "read_skill", Arguments: json.RawMessage(`{"id":"skill-disabled-1"}`)},
						{ID: "u_002", Name: "read_skill", Arguments: json.RawMessage(`{"id":"skill-002"}`)},
						{ID: "u_003", Name: "read_skill", Arguments: json.RawMessage(`{"id":"skill-003"}`)},
					},
				},
			}, nil
		}
		return protocol.ProviderResponse{
			FinishReason: "stop",
			Message:      protocol.Message{Role: "assistant", Content: "Run 2 done."},
		}, nil
	}}
	ref2 := bindRef(engine, "b", run2Provider)
	res2, err := engine.RunAgent(ctx, sess.ID, AgentOptions{
		ModelRef: ref2,
		Input:    "Check refreshed snapshot",
		MaxSteps: 10,
	})
	if err != nil || res2.Status != "completed" {
		t.Fatalf("Run 2 failed: res=%+v err=%v", res2, err)
	}
	byCall = loadToolResultsByCallID(t, engine, sess.ID)
	if !strings.Contains(byCall["u_new"], `"skill-new"`) {
		t.Fatalf("Run 2 missing skill-new: %s", byCall["u_new"])
	}
	if !strings.Contains(byCall["u_001"], "OVERWRITTEN_BODY_001") {
		t.Fatalf("Run 2 did not refresh skill-001 body: %s", byCall["u_001"])
	}
	if !strings.Contains(byCall["u_dis1"], "DISABLED_BODY_1") {
		t.Fatalf("Run 2 did not include newly enabled skill-disabled-1: %s", byCall["u_dis1"])
	}
	// Note: deleting skill-002 in rootLC unshadowed rootAgents/skill-002 ("SHADOWED_2"), while skill-003 was disabled!
	if !strings.Contains(byCall["u_002"], "SHADOWED_2") {
		t.Fatalf("Run 2 expected unshadowed agents skill-002, got: %s", byCall["u_002"])
	}
	if !strings.Contains(byCall["u_003"], "unknown skill") {
		t.Fatalf("Run 2 allowed disabled skill-003: %s", byCall["u_003"])
	}

	// Step ⑨: Untrusted workspace must withhold project skills and block reading proj-skill-1 by id.
	useGlobalConfig(t, strings.ReplaceAll(modelConfig(""), `"context": 8000`, `"context": 128000`))
	untrustedEngine, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer untrustedEngine.Close()
	uSess := newTestSession(t, untrustedEngine, "Test withheld")
	uStep := 0
	uProv := funcProvider{fn: func(_ context.Context, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		uStep++
		if uStep == 1 {
			return protocol.ProviderResponse{
				FinishReason: "tool_calls",
				Message: protocol.Message{
					Role: "assistant",
					ToolCalls: []protocol.ToolCall{
						{ID: "w_list", Name: "read_skill", Arguments: json.RawMessage(`{"query":"proj-skill"}`)},
						{ID: "w_read", Name: "read_skill", Arguments: json.RawMessage(`{"id":"proj-skill-1"}`)},
					},
				},
			}, nil
		}
		return protocol.ProviderResponse{FinishReason: "stop", Message: protocol.Message{Role: "assistant", Content: "ok"}}, nil
	}}
	uRef := bindRef(untrustedEngine, "a", uProv)
	if _, err := untrustedEngine.RunAgent(ctx, uSess.ID, AgentOptions{ModelRef: uRef, Input: "go", MaxSteps: 5}); err != nil {
		t.Fatal(err)
	}
	uByCall := loadToolResultsByCallID(t, untrustedEngine, uSess.ID)
	if strings.Contains(uByCall["w_list"], `"proj-skill-1"`) || !strings.Contains(uByCall["w_read"], "unknown skill") {
		t.Fatalf("untrusted workspace leaked withheld project skill: list=%s read=%s", uByCall["w_list"], uByCall["w_read"])
	}
}

func toolCallResp(id, name, rawArgs string) protocol.ProviderResponse {
	return protocol.ProviderResponse{
		FinishReason: "tool_calls",
		Message: protocol.Message{
			Role: "assistant",
			ToolCalls: []protocol.ToolCall{
				{ID: id, Name: name, Arguments: json.RawMessage(rawArgs)},
			},
		},
	}
}

func loadToolResultsByCallID(t *testing.T, engine *Engine, sessionID string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := engine.store.EachMessageFrom(context.Background(), sessionID, 0, func(row session.Message) error {
		var m protocol.Message
		if err := json.Unmarshal(row.JSON, &m); err != nil {
			return err
		}
		if m.Role == "tool" {
			out[m.ToolCallID] = m.Content
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
