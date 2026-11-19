package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/internal/window"
	"github.com/edynasty/LumenCortex/protocol"
)

const r304GiantMarker = "verbose output"

// r304Tools runs read_file through the real toolset and answers bash itself: the giant-th bash call
// returns a result that alone exceeds a 32K window.
type r304Tools struct {
	real  *toolset.Set
	specs []protocol.ToolSpec
	bash  *atomic.Int64
	giant int64
}

func (z r304Tools) Specs([]string) []protocol.ToolSpec { return z.specs }

func (z r304Tools) Execute(ctx context.Context, name string, args map[string]any, out func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	if name == "bash" {
		if z.bash.Add(1) == z.giant {
			return protocol.ToolResult{OK: true, Content: strings.Repeat("FAIL github.com/acme/pkg/sub 0.12s "+r304GiantMarker+"\n", 4000), MutatesWorkspace: true}, nil
		}
		return protocol.ToolResult{OK: true, Content: strings.Repeat("ok   github.com/acme/pkg/sub 0.12s\n", 150), MutatesWorkspace: true}, nil
	}
	return z.real.Execute(ctx, name, args, out)
}

// r304Jobs reports a running background job exactly while the model's 4th reply proposes completion,
// so the completion gate refuses it once.
type r304Jobs struct{ reply *atomic.Int64 }

func (j r304Jobs) ActiveJobs(context.Context, string) ([]ActiveJob, error) {
	if j.reply.Load() == 4 {
		return []ActiveJob{{ID: "job_r304_gate", State: "running", Command: "make test"}}, nil
	}
	return nil, nil
}

// TestR304ResumeNoticeDoesNotClaimOmittedLatestStepPreserved reproduces R3-04 on a real SQLite store:
// a gated repair whose newest post-gate bash result alone exceeds the only (32K) model is resumed cold.
// The Resume row becomes the newest group, the oversized bash step is omitted, and the relaxation
// notice must not claim the latest atomic step is preserved. The step stays in the causal ledger as
// Unknown evidence and the gate refusal stays pinned.
func TestR304ResumeNoticeDoesNotClaimOmittedLatestStepPreserved(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	ws := filepath.Join(root, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepoForTest(t, ws)
	line := "// read-only context line 0123456789abcdef\n"
	if err := os.WriteFile(filepath.Join(ws, "big.txt"), []byte(strings.Repeat(line, 6500/len(line))), 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, "db")
	ref := config.ModelRef("test/m-32768_4096")
	const sid = "s-r304"

	runOnce := func(fresh bool, maxSteps int) []protocol.ProviderRequest {
		raw, err := session.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = raw.Close() }()
		runID := "run-live"
		if fresh {
			if err := raw.Create(ctx, session.Session{ID: sid, Goal: "make the suite pass", Status: "running"}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := raw.BeginRun(ctx, sid, session.RunStart{RunID: runID, Input: &protocol.Message{Role: "user", Content: "make the suite pass"}}); err != nil {
				t.Fatal(err)
			}
		} else {
			runID = "run-resume"
			if _, err := raw.Reconcile(ctx, sid); err != nil {
				t.Fatal(err)
			}
			if _, _, err := raw.BeginRun(ctx, sid, session.RunStart{RunID: runID}); err != nil {
				t.Fatal(err)
			}
		}
		set, err := toolset.New(toolset.Options{Workspace: ws, Policy: toolset.PolicyFull, SessionID: sid})
		if err != nil {
			t.Fatal(err)
		}
		var specs []protocol.ToolSpec
		for _, s := range set.Specs(nil) {
			if s.Name == "read_file" || s.Name == "bash" {
				specs = append(specs, s)
			}
		}
		repo, err := repository.New(ws)
		if err != nil {
			t.Fatal(err)
		}
		var bash, reply atomic.Int64
		giant := int64(3)
		if !fresh {
			giant = -1
		}
		var reqs []protocol.ProviderRequest
		n := 0
		prov := &funcProvider{fn: func(_ context.Context, _ int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
			n++
			reply.Store(int64(n))
			reqs = append(reqs, req)
			if !fresh {
				return answer("resumed answer"), nil
			}
			id := fmt.Sprintf("g%d", n)
			switch {
			case n <= 3:
				return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call(id, "read_file", `{"path":"big.txt"}`)}}}, nil
			case n == 4:
				return answer("All done."), nil
			default:
				return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call(id, "bash", `{"command":"go test -v ./..."}`)}}}, nil
			}
		}}
		m := testModel(ref, prov)
		m.Context, m.Output = 32768, 4096
		l := &Loop{
			Models:    map[config.ModelRef]Model{ref: m},
			Store:     realSQLiteAgentStore{s: raw},
			Tools:     r304Tools{real: set, specs: specs, bash: &bash, giant: giant},
			Snapshots: repo,
			Jobs:      r304Jobs{reply: &reply},
		}
		_, _ = l.Run(ctx, sid, Options{Session: ref, RunID: runID, MaxSteps: maxSteps, Resumed: !fresh})
		return reqs
	}

	runOnce(true, 20)

	// The oversized step is the newest assistant group; a Resume row follows it.
	raw, err := session.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var entries []window.Entry
	if err := (realSQLiteAgentStore{s: raw}).Transcript(ctx, sid, 0, func(seq int64, m protocol.Message) error {
		entries = append(entries, window.Entry{Seq: seq, Msg: m})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	giantSeq := int64(-1)
	for i, e := range entries {
		if e.Msg.Role == "tool" && strings.Contains(e.Msg.Content, r304GiantMarker) {
			for j := i - 1; j >= 0; j-- {
				if entries[j].Msg.Role == "assistant" {
					giantSeq = entries[j].Seq
					break
				}
			}
		}
	}
	if giantSeq < 0 || latestAtomicStepSeq(entries) != giantSeq {
		t.Fatalf("live run did not leave the oversized bash step as the latest atomic step (giant seq %d, latest %d)", giantSeq, latestAtomicStepSeq(entries))
	}

	reqs := runOnce(false, 1)
	if len(reqs) == 0 {
		t.Fatal("cold Resume sent no model request")
	}
	req := reqs[0]
	var state string
	for _, msg := range req.Messages {
		if strings.Contains(msg.Content, r304GiantMarker) {
			t.Fatalf("the oversized step was expected to be omitted from the Resume request")
		}
		if strings.Contains(msg.Content, "[Context Window Notice — Floor Relaxation]") {
			state = msg.Content
		}
	}
	if state == "" {
		t.Fatal("Resume request carries no floor-relaxation notice")
	}
	if strings.Contains(state, "the latest atomic step is preserved") {
		t.Fatalf("notice claims the omitted latest atomic step is preserved:\n%s", state)
	}
	if !strings.Contains(state, fmt.Sprintf("the latest atomic step (seq %d) may have been omitted", giantSeq)) {
		t.Fatalf("notice does not name the possibly omitted latest atomic step seq %d:\n%s", giantSeq, state)
	}
	if !strings.Contains(state, "[Pinned Completion-Gate State — seq ") {
		t.Fatalf("gate refusal is not pinned:\n%s", state)
	}
	if !regexp.MustCompile(fmt.Sprintf(`seq=%d tool=bash exec=\S+ evidence=unknown`, giantSeq)).MatchString(state) {
		t.Fatalf("causal ledger lost the omitted step's Unknown evidence:\n%s", state)
	}
}

// TestR304NoticesClaimLatestStepOnlyWhenGuaranteed pins both relaxation notices: the preservation claim
// is kept verbatim when the guaranteed suffix starts at or before the latest atomic step, and replaced
// by the floor and step seqs otherwise. Runtime-appended rows do not count as the latest step.
func TestR304NoticesClaimLatestStepOnlyWhenGuaranteed(t *testing.T) {
	tx := []window.Entry{
		{Seq: 7, Msg: protocol.Message{Role: "user", Origin: protocol.OriginCompletionBlocked, Content: "refused"}},
		{Seq: 8, Msg: protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{{ID: "c", Name: "bash", Arguments: json.RawMessage(`{}`)}}}},
		{Seq: 9, Msg: protocol.Message{Role: "tool", ToolCallID: "c", Content: "huge"}},
		{Seq: 10, Msg: protocol.Message{Role: "user", Origin: protocol.OriginResume, Content: protocol.ResumeContent}},
	}
	if got := latestAtomicStepSeq(tx); got != 8 {
		t.Fatalf("latestAtomicStepSeq = %d, want 8 (the Resume row is not a step)", got)
	}
	if got := latestAtomicStepSeq(tx[:1]); got != 7 {
		t.Fatalf("latestAtomicStepSeq = %d, want the gate refusal 7", got)
	}
	repair := repairContext{Active: true, BlockSeq: 7, BlockCount: 1, OriginalMinRequiredSeq: 3, MinRequiredSeq: 3}
	const pinnedClaim = "and the latest atomic step is preserved."
	const r1Claim = "and the latest atomic step are preserved in the message window."
	for _, tc := range []struct {
		floor   int64
		claimed bool
	}{{floor: 8, claimed: true}, {floor: 3, claimed: true}, {floor: 10, claimed: false}} {
		pinned := appendPinnedGateAndRelaxationNotice("", repair, 3, "", tc.floor, 8)
		r1 := appendFloorRelaxationNotice("", repair, 3, tc.floor, 8)
		omitted := fmt.Sprintf("seq %d onward are guaranteed", tc.floor)
		for name, text := range map[string]string{"pinned": pinned, "r1": r1} {
			claim := pinnedClaim
			if name == "r1" {
				claim = r1Claim
			}
			if strings.Contains(text, claim) != tc.claimed || strings.Contains(text, omitted) == tc.claimed {
				t.Fatalf("%s notice with floor %d (latest step 8): claimed=%v\n%s", name, tc.floor, tc.claimed, text)
			}
			if !tc.claimed && !strings.Contains(text, "latest atomic step (seq 8) may have been omitted") {
				t.Fatalf("%s notice does not name the omitted step:\n%s", name, text)
			}
		}
	}
}
