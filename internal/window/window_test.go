package window

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

func user(s string) protocol.Message { return protocol.Message{Role: "user", Content: s} }

func asst(content string, ids ...string) protocol.Message {
	m := protocol.Message{Role: "assistant", Content: content}
	for _, id := range ids {
		m.ToolCalls = append(m.ToolCalls, protocol.ToolCall{ID: id, Name: "shell", Arguments: json.RawMessage(`{"command":"true"}`)})
	}
	return m
}

func tool(id, content string) protocol.Message {
	return protocol.Message{Role: "tool", ToolCallID: id, Name: "shell", Content: content}
}

func entries(ms ...protocol.Message) []Entry {
	out := make([]Entry, len(ms))
	for i, m := range ms {
		out[i] = Entry{Seq: int64(i), Msg: m}
	}
	return out
}

// checkPairing is an oracle for I2 written independently of Groups: every call
// of an assistant message is answered exactly once by the tool messages that
// directly follow it, and every tool message answers a call of the nearest
// preceding assistant message.
func checkPairing(ms []protocol.Message) error {
	var calls map[string]int // call id → answers seen; nil = no open assistant
	closeTurn := func() error {
		for id, n := range calls {
			if n != 1 {
				return fmt.Errorf("call %q answered %d times", id, n)
			}
		}
		return nil
	}
	for i, m := range ms {
		switch m.Role {
		case "tool":
			if calls == nil {
				return fmt.Errorf("msg %d: tool %q without preceding assistant", i, m.ToolCallID)
			}
			if _, ok := calls[m.ToolCallID]; !ok {
				return fmt.Errorf("msg %d: tool %q answers no call of preceding assistant", i, m.ToolCallID)
			}
			calls[m.ToolCallID]++
		default:
			if err := closeTurn(); err != nil {
				return err
			}
			calls = nil
			if m.Role == "assistant" {
				calls = map[string]int{}
				for _, tc := range m.ToolCalls {
					calls[tc.ID] = 0
				}
			}
		}
	}
	return closeTurn()
}

func sumEst(ms []protocol.Message) int {
	n := 0
	for _, m := range ms {
		n += Estimate(m)
	}
	return n
}

func randText(r *rand.Rand) string {
	n := r.IntN(4001)
	if r.IntN(4) == 0 {
		n = r.IntN(40)
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + r.IntN(26))
	}
	return string(b)
}

// randTranscript returns a valid transcript plus its group boundaries
// (groupStart[k] = index of first entry of group k), built independently of
// Groups so the test does not use the code under test as its own oracle.
func randTranscript(r *rand.Rand, caseID int) ([]Entry, []int) {
	var ms []protocol.Message
	var starts []int
	turns := r.IntN(25)
	for t := range turns {
		starts = append(starts, len(ms))
		if r.IntN(3) == 0 {
			ms = append(ms, user(randText(r)))
			continue
		}
		nCalls := r.IntN(4)
		ids := make([]string, nCalls)
		for c := range ids {
			ids[c] = fmt.Sprintf("c%d_%d_%d", caseID, t, c)
		}
		ms = append(ms, asst(randText(r), ids...))
		order := r.Perm(nCalls) // results may arrive in any order
		for _, c := range order {
			ms = append(ms, tool(ids[c], randText(r)))
		}
	}
	seq := int64(r.IntN(1000))
	out := make([]Entry, len(ms))
	for i, m := range ms {
		out[i] = Entry{Seq: seq, Msg: m}
		seq += int64(1 + r.IntN(3)) // strictly ascending, not necessarily dense
	}
	return out, starts
}

func TestBuildProperties(t *testing.T) {
	const cases = 5000
	var ok, overflow, dropped, full, emptyTranscript int
	for c := range cases {
		r := rand.New(rand.NewPCG(uint64(c), 0x5eed))
		tr, starts := randTranscript(r, c)
		var system []protocol.Message
		if r.IntN(5) != 0 {
			system = append(system, protocol.Message{Role: "system", Content: randText(r)})
		}
		state := user("TASK STATE goal=case" + fmt.Sprint(c) + " " + randText(r))
		in := Input{System: system, State: state, Transcript: tr, ToolTokens: r.IntN(2000)}
		b := Budget{Context: 1000 + r.IntN(20000), Output: r.IntN(4096), Margin: r.IntN(512)}

		res, err := Build(in, b)
		available := b.Context - b.Output - b.Margin - sumEst(system) - Estimate(state) - in.ToolTokens
		groupCost := func(k int) int {
			end := len(tr)
			if k+1 < len(starts) {
				end = starts[k+1]
			}
			n := 0
			for _, e := range tr[starts[k]:end] {
				n += Estimate(e.Msg)
			}
			return n
		}
		if err != nil {
			if !errors.Is(err, ErrOverflow) {
				t.Fatalf("case %d: unexpected error %v", c, err)
			}
			// ErrOverflow must be justified.
			if available >= 0 && (len(starts) == 0 || groupCost(len(starts)-1) <= available) {
				t.Fatalf("case %d: spurious ErrOverflow (available=%d)", c, available)
			}
			overflow++
			continue
		}
		ok++
		if available < 0 {
			t.Fatalf("case %d: available=%d but no ErrOverflow", c, available)
		}
		// I3: State at index len(System), System unchanged before it.
		if len(res.Messages) <= len(system) || !reflect.DeepEqual(res.Messages[len(system)], state) {
			t.Fatalf("case %d: State not at index %d", c, len(system))
		}
		if !reflect.DeepEqual(res.Messages[:len(system)], system) && len(system) > 0 {
			t.Fatalf("case %d: System prefix altered", c)
		}
		tail := res.Messages[len(system)+1:]
		// I4: message right after State is never a tool message.
		if len(tail) > 0 && tail[0].Role == "tool" {
			t.Fatalf("case %d: request starts transcript with tool message", c)
		}
		// I2 on the transcript part of the request.
		if err := checkPairing(tail); err != nil {
			t.Fatalf("case %d: I2 violated: %v", c, err)
		}
		// Contiguous suffix of the input transcript, starting on a group boundary.
		k := len(tr) - len(tail)
		for i, m := range tail {
			if !reflect.DeepEqual(m, tr[k+i].Msg) {
				t.Fatalf("case %d: output is not a contiguous suffix (index %d)", c, i)
			}
		}
		firstGroup := len(starts)
		for g, s := range starts {
			if s == k {
				firstGroup = g
			}
		}
		if len(tail) > 0 && (firstGroup == len(starts) || starts[firstGroup] != k) {
			t.Fatalf("case %d: suffix starts mid-group at entry %d", c, k)
		}
		if len(tail) == 0 {
			if len(tr) != 0 {
				t.Fatalf("case %d: non-empty transcript but nothing included and no ErrOverflow", c)
			}
			if res.FirstSeq != -1 {
				t.Fatalf("case %d: FirstSeq=%d for empty window", c, res.FirstSeq)
			}
			emptyTranscript++
		} else if res.FirstSeq != tr[k].Seq {
			t.Fatalf("case %d: FirstSeq=%d want %d", c, res.FirstSeq, tr[k].Seq)
		}
		if res.Dropped != firstGroup && len(tail) > 0 {
			t.Fatalf("case %d: Dropped=%d want %d", c, res.Dropped, firstGroup)
		}
		// Maximality: adding the previous group would exceed available.
		included := sumEst(tail)
		if included > available {
			t.Fatalf("case %d: included %d > available %d", c, included, available)
		}
		if res.Dropped > 0 {
			if prev := groupCost(res.Dropped - 1); included+prev <= available {
				t.Fatalf("case %d: not maximal: previous group (%d) would fit (%d+%d ≤ %d)", c, prev, included, prev, available)
			}
			dropped++
		} else if len(tr) > 0 {
			full++
		}
		// Token bound.
		if res.Tokens != sumEst(res.Messages)+in.ToolTokens {
			t.Fatalf("case %d: Tokens=%d want %d", c, res.Tokens, sumEst(res.Messages)+in.ToolTokens)
		}
		if res.Tokens > b.Context-b.Output-b.Margin {
			t.Fatalf("case %d: Tokens %d > %d", c, res.Tokens, b.Context-b.Output-b.Margin)
		}
	}
	t.Logf("cases=%d ok=%d (dropped>0: %d, whole transcript: %d, empty transcript: %d) overflow=%d",
		cases, ok, dropped, full, emptyTranscript, overflow)
	if dropped < 100 || full < 100 || overflow < 100 {
		t.Fatalf("generator does not cover all outcomes: dropped=%d full=%d overflow=%d", dropped, full, overflow)
	}
}

// TestE5Regression mirrors audit E5: a long tool-using run under a budget that
// fits about 8 transcript messages.
//
// Contrast (core HEAD): buildMessages uses RecentMessages(8)
// (core:internal/agent/loop.go L39-40 default, L373), a row-count tail that includes the user goal
// message only while it is among the last 8 rows. Audit E5 observed: with
// 1 call/step the goal is gone from request 5 on; with 2 calls/step request 4
// already starts with an orphan tool message (its assistant was cut off).
// Here the goal lives in the Task State block, which is always included, and
// the window is cut on group boundaries only.
func TestE5Regression(t *testing.T) {
	const goal = "GOAL: make go test ./... pass"
	payload := strings.Repeat("x", 300)
	for _, callsPerStep := range []int{1, 2} {
		t.Run(fmt.Sprintf("calls_per_step_%d", callsPerStep), func(t *testing.T) {
			system := []protocol.Message{{Role: "system", Content: "You are LumenCortex."}}
			state := user("<task-state>" + goal + "</task-state>")
			transcript := []protocol.Message{user(goal)}
			perMsg := Estimate(tool("c00_0", payload))
			b := Budget{Context: 0, Output: 4096, Margin: 256}
			b.Context = b.Output + b.Margin + sumEst(system) + Estimate(state) + 8*perMsg + perMsg/2
			for step := 1; step <= 30; step++ {
				res, err := Build(Input{System: system, State: state, Transcript: entries(transcript...)}, b)
				if err != nil {
					t.Fatalf("request %d: %v", step, err)
				}
				if res.Messages[len(system)].Content != state.Content {
					t.Fatalf("request %d: Task State (goal) missing", step)
				}
				tail := res.Messages[len(system)+1:]
				if len(tail) == 0 || tail[0].Role == "tool" {
					t.Fatalf("request %d: window empty or starts with tool", step)
				}
				if err := checkPairing(tail); err != nil {
					t.Fatalf("request %d: %v", step, err)
				}
				// Simulated core HEAD tail: last 8 rows of the same transcript.
				naive := transcript
				if len(naive) > 8 {
					naive = naive[len(naive)-8:]
				}
				hasGoal := false
				for _, m := range naive {
					hasGoal = hasGoal || m.Content == goal
				}
				if step <= 6 || step == 30 {
					t.Logf("request %2d: window msgs=%d firstSeq=%d dropped=%d tokens=%d/%d | tail(8): goal=%v first=%s",
						step, len(tail), res.FirstSeq, res.Dropped, res.Tokens, b.Context-b.Output-b.Margin, hasGoal, naive[0].Role)
				}
				// next step: assistant with calls + results
				ids := make([]string, callsPerStep)
				for c := range ids {
					ids[c] = fmt.Sprintf("c%02d_%d", step, c)
				}
				transcript = append(transcript, asst("step", ids...))
				for _, id := range ids {
					transcript = append(transcript, tool(id, payload))
				}
			}
		})
	}
}

func TestGroupsUnpaired(t *testing.T) {
	cases := map[string][]Entry{
		"orphan tool at start":  entries(tool("a", "x"), asst("ok")),
		"missing answer":        entries(user("u"), asst("", "a", "b"), tool("a", "x"), user("next")),
		"missing answer at end": entries(asst("", "a")),
		"duplicate answer":      entries(asst("", "a"), tool("a", "x"), tool("a", "y")),
		"tool answering an older assistant": entries(
			asst("", "a"), tool("a", "x"), asst("", "b"), tool("b", "y"), tool("a", "late")),
		"tool after user":                    entries(asst("", "a"), tool("a", "x"), user("u"), tool("a", "late")),
		"tool after assistant without calls": entries(asst("plain"), tool("a", "x")),
		"repeated call id":                   entries(asst("", "a", "a"), tool("a", "x"), tool("a", "y")),
	}
	for name, tr := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Groups(tr); !errors.Is(err, ErrUnpaired) {
				t.Fatalf("Groups err=%v, want ErrUnpaired", err)
			}
			if _, err := Build(Input{State: user("s"), Transcript: tr}, Budget{Context: 1 << 20}); !errors.Is(err, ErrUnpaired) {
				t.Fatalf("Build err=%v, want ErrUnpaired", err)
			} else {
				t.Logf("%v", err)
			}
		})
	}
}

func TestOverflow(t *testing.T) {
	state := user("state")
	// fixed prefix alone too big
	if _, err := Build(Input{State: state, ToolTokens: 100}, Budget{Context: 100}); !errors.Is(err, ErrOverflow) {
		t.Fatalf("prefix overflow: err=%v", err)
	}
	// empty transcript, prefix fits: no error, FirstSeq -1
	res, err := Build(Input{State: state}, Budget{Context: Estimate(state)})
	if err != nil || res.FirstSeq != -1 || len(res.Messages) != 1 || res.Tokens != Estimate(state) {
		t.Fatalf("empty transcript: res=%+v err=%v", res, err)
	}
	// newest group does not fit
	tr := entries(user("u"), asst("", "a"), tool("a", strings.Repeat("y", 3000)))
	if _, err := Build(Input{State: state, Transcript: tr}, Budget{Context: Estimate(state) + 100}); !errors.Is(err, ErrOverflow) {
		t.Fatalf("group overflow: err=%v", err)
	}
	// exact fit is allowed (≤)
	all := Estimate(state) + sumEst([]protocol.Message{tr[0].Msg, tr[1].Msg, tr[2].Msg})
	res, err = Build(Input{State: state, Transcript: tr}, Budget{Context: all})
	if err != nil || res.Dropped != 0 || res.FirstSeq != 0 || res.Tokens != all {
		t.Fatalf("exact fit: res=%+v err=%v", res, err)
	}
	// one token less drops the oldest group only (the user message)
	res, err = Build(Input{State: state, Transcript: tr}, Budget{Context: all - 1})
	if err != nil || res.Dropped != 1 || res.FirstSeq != 1 {
		t.Fatalf("one less: res=%+v err=%v", res, err)
	}
}

// Run-memory echo counts toward the group of the row it is attached to.
func TestEchoTokensCountTowardGroup(t *testing.T) {
	state := user("state")
	tr := entries(user("u"), asst("", "a"), tool("a", "ok"))
	all := Estimate(state) + sumEst([]protocol.Message{tr[0].Msg, tr[1].Msg, tr[2].Msg})
	if res, err := Build(Input{State: state, Transcript: tr}, Budget{Context: all}); err != nil || res.Dropped != 0 {
		t.Fatalf("no echo: res=%+v err=%v", res, err)
	}
	echo := Estimate(tr[0].Msg) // as large as the oldest group
	tr[1].EchoTokens = echo
	res, err := Build(Input{State: state, Transcript: tr}, Budget{Context: all + echo})
	if err != nil || res.Dropped != 0 || res.Tokens != all+echo {
		t.Fatalf("echo fits: res=%+v err=%v", res, err)
	}
	if res, err = Build(Input{State: state, Transcript: tr}, Budget{Context: all}); err != nil || res.Dropped != 1 || res.FirstSeq != 1 {
		t.Fatalf("echo pushes the oldest group out: res=%+v err=%v", res, err)
	}
	if _, err = Build(Input{State: state, Transcript: tr}, Budget{Context: all - 1}); !errors.Is(err, ErrOverflow) {
		t.Fatalf("echo makes the newest group overflow: err=%v", err)
	}
}

// §5: after a capacity rejection the retry keeps the newest half of the transcript tokens the rejected
// request held — also when the budget was far larger than the transcript, where halving the budget
// would leave the window unchanged.
func TestCapacityRetryHalvesTranscript(t *testing.T) {
	state := user("task state")
	ms := []protocol.Message{user("goal")}
	for i := range 6 {
		id := fmt.Sprintf("c%d", i)
		ms = append(ms, asst("", id), tool(id, strings.Repeat("x", 600)))
	}
	in := Input{State: state, Transcript: entries(ms...), ToolTokens: 50}
	b := Budget{Context: 1 << 20, Output: 4096, Margin: 512}
	rejected, err := Build(in, b)
	if err != nil || rejected.Dropped != 0 {
		t.Fatalf("first build: res=%+v err=%v", rejected, err)
	}
	fixed := fixedTokens(in)
	retry, err := Build(in, CapacityRetry(in, b, rejected))
	if err != nil {
		t.Fatalf("retry build: %v", err)
	}
	if got, limit := retry.Tokens-fixed, (rejected.Tokens-fixed)/2; got > limit || retry.Dropped == 0 {
		t.Fatalf("retry transcript tokens=%d dropped=%d, want ≤ %d with groups dropped", got, retry.Dropped, limit)
	}
	if !reflect.DeepEqual(retry.Messages[len(retry.Messages)-1], ms[len(ms)-1]) {
		t.Fatalf("retry lost the newest group")
	}
	if err := checkPairing(retry.Messages[1:]); err != nil {
		t.Fatalf("retry window unpaired: %v", err)
	}
	t.Logf("rejected transcript tokens=%d, retry=%d, dropped %d groups", rejected.Tokens-fixed, retry.Tokens-fixed, retry.Dropped)

	// The newest group alone exceeds half of the rejected transcript: overflow, and failover decides.
	big := Input{State: state, Transcript: entries(user("goal"), asst("", "b"), tool("b", strings.Repeat("y", 3000)))}
	rej, err := Build(big, b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(big, CapacityRetry(big, b, rej)); !errors.Is(err, ErrOverflow) {
		t.Fatalf("retry with an oversized newest group: err=%v, want ErrOverflow", err)
	}
}

func TestEstimateDeterministic(t *testing.T) {
	m := asst("hello", "a")
	b, _ := json.Marshal(m)
	if got, want := Estimate(m), (len(b)+2)/3+4; got != want {
		t.Fatalf("Estimate=%d want %d", got, want)
	}
	bad := protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{{ID: "a", Name: "read", Arguments: json.RawMessage(`{"path":`)}}}
	if Estimate(bad) != Estimate(bad) || Estimate(bad) <= 4 {
		t.Fatalf("invalid-arguments estimate not deterministic/positive: %d", Estimate(bad))
	}
	t.Logf("Estimate(valid)=%d Estimate(invalid args)=%d", Estimate(m), Estimate(bad))
}
