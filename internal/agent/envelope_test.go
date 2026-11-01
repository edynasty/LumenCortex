package agent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// T1-S2: every provider request has a durable envelope before it is sent, the envelope describes
// what was actually sent, and only a stored answer makes it answered.

func envStore(id string) *memoryStore {
	store := &memoryStore{state: SessionState{ID: id, Goal: "goal " + id, Status: "running"}, steps: map[int64]any{}}
	store.seedGoal()
	return store
}

func answerOf(text string) protocol.ProviderResponse {
	return protocol.ProviderResponse{Message: protocol.Message{Content: text}, FinishReason: "stop"}
}

func states(envs []session.RequestEnvelope) []string {
	out := make([]string, len(envs))
	for i, e := range envs {
		out[i] = string(e.State)
		if e.Reason != "" {
			out[i] += ":" + e.Reason
		}
	}
	return out
}

// The envelope's window, Task State version, system layers and tool digest are those of the request
// the provider received, step by step, and the answered envelope points at the stored assistant row.
func TestEnvelopeDescribesTheRequestThatWasSent(t *testing.T) {
	store := envStore("env")
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call("1", "bash", `{"command":"ok"}`)}}, FinishReason: "tool_calls"},
		answerOf("done"),
	}}
	loop := Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}
	opts := Options{Session: testRef, MaxSteps: 4, RunID: "run-1", SystemPrompt: "BASE", Instructions: "extra",
		SystemLayers: []SystemLayer{{Kind: "project-instructions", Source: "AGENTS.md", Text: "layer text"}}}
	if _, err := loop.Run(context.Background(), "env", opts); err != nil {
		t.Fatal(err)
	}
	envs := store.envelopes()
	if len(envs) != 2 || len(provider.seen) != 2 {
		t.Fatalf("envelopes=%d requests=%d, want one each", len(envs), len(provider.seen))
	}
	for i, env := range envs {
		req := provider.seen[i]
		if env.State != session.RequestAnswered || env.AnsweredSeq == nil {
			t.Fatalf("envelope %d = %+v, want answered", i, env)
		}
		if env.Purpose != session.PurposeMain || env.RunID != "run-1" || env.Step != i+1 || env.Attempt != 0 {
			t.Errorf("envelope %d identity = %+v", i, env)
		}
		if env.ToolSpecsDigest != toolSpecsDigest(req.Tools) {
			t.Errorf("envelope %d tool digest %s does not match the tools sent", i, env.ToolSpecsDigest)
		}
		// The transcript rows in the request are exactly the interval [first, last].
		sent := len(req.Messages) - 4 // the system prompt, one layer, the instructions and the state block
		if want := int(*env.LastSeq - *env.FirstSeq + 1); sent != want {
			t.Errorf("envelope %d interval [%d,%d] covers %d rows, the request carried %d", i, *env.FirstSeq, *env.LastSeq, want, sent)
		}
		if got := store.messages[*env.AnsweredSeq]; got.Role != "assistant" {
			t.Errorf("envelope %d answered_seq %d is %q, want the assistant row", i, *env.AnsweredSeq, got.Role)
		}
		if !env.LineageComplete {
			t.Errorf("envelope %d lineage incomplete", i)
		}
	}
	// The second request saw the first answer and the tool result as well.
	if *envs[1].LastSeq <= *envs[0].LastSeq {
		t.Errorf("second window ends at %d, first at %d: it must include the rows written in between", *envs[1].LastSeq, *envs[0].LastSeq)
	}
	joined := strings.Join(envs[0].Refs, " ")
	for _, want := range []string{"ts:v0", "sys:prompt:", "sys:project-instructions:", "sys:instructions:"} {
		if !strings.Contains(joined, want) {
			t.Errorf("refs %v lack %q", envs[0].Refs, want)
		}
	}
	for _, ref := range envs[0].Refs {
		if strings.Contains(ref, "BASE") || strings.Contains(ref, "layer text") || strings.Contains(ref, "goal") {
			t.Errorf("ref %q carries body text", ref)
		}
	}
}

func TestToolSpecsDigestFollowsWhatTheModelSees(t *testing.T) {
	base := []protocol.ToolSpec{{Name: "a", Description: "d", Parameters: map[string]any{"type": "object", "x": 1}}}
	same := []protocol.ToolSpec{{Name: "a", Description: "d", Parameters: map[string]any{"x": 1, "type": "object"}, Permission: "write", MutatesWorkspace: true}}
	if toolSpecsDigest(base) != toolSpecsDigest(same) {
		t.Error("map order or a permission flag the model never sees changed the digest")
	}
	for name, changed := range map[string][]protocol.ToolSpec{
		"name":        {{Name: "b", Description: "d", Parameters: base[0].Parameters}},
		"description": {{Name: "a", Description: "e", Parameters: base[0].Parameters}},
		"schema":      {{Name: "a", Description: "d", Parameters: map[string]any{"type": "object", "x": 2}}},
		"extra tool":  append(append([]protocol.ToolSpec(nil), base...), protocol.ToolSpec{Name: "c"}),
		"empty":       nil,
	} {
		if toolSpecsDigest(changed) == toolSpecsDigest(base) {
			t.Errorf("a changed %s left the digest unchanged", name)
		}
	}
}

// Ids the model chose may hold any character; a ref keeps the store's alphabet and stays stable.
func TestRefIDKeepsRefsInsideTheStoreAlphabet(t *testing.T) {
	if got := refID("unit-1"); got != "unit-1" {
		t.Errorf("a plain id changed: %q", got)
	}
	odd := "naïve id/with spaces\nand a very long tail " + strings.Repeat("z", 200)
	a, b := refID(odd), refID(odd)
	if a != b || a == odd || refID(odd+"x") == a {
		t.Errorf("refID(odd) = %q, %q: want stable, distinct, rewritten", a, b)
	}
	alphabet := regexp.MustCompile(`^[A-Za-z0-9._:/@#+=%~-]{1,96}$`)
	if !alphabet.MatchString("ts:unit:"+a) || !strings.HasPrefix(a, "h") {
		t.Errorf("ref id %q is outside the store's alphabet", a)
	}
}

// A capacity rejection retried on the same model and a failover to a second model are three attempts
// in total here: each has its own envelope, the rejected ones end unknown, the answered one answered.
func TestEveryAttemptHasItsOwnEnvelope(t *testing.T) {
	const small, large = config.ModelRef("test/small"), config.ModelRef("test/large")
	ps := &sequenceProvider{errs: []error{
		&protocol.ProviderError{Class: protocol.ClassCapacity, Err: errors.New("too long")},
		&protocol.ProviderError{Class: protocol.ClassAvailability, Err: errors.New("overloaded")},
	}}
	pl := &scriptedProvider{responses: []protocol.ProviderResponse{answerOf("done")}}
	ms, ml := testModel(small, ps), testModel(large, pl)
	store := envStore("att")
	store.messages = oldRows(60) // enough rows for the halved window of the retry to be a window
	loop := Loop{Models: map[config.ModelRef]Model{small: ms, large: ml}, Store: store, Tools: fakeTools{}}
	result, err := loop.Run(context.Background(), "att", Options{Session: small, Failover: []config.ModelRef{large}, MaxSteps: 2, RunID: "r"})
	if err != nil || result.Status != StatusCompleted {
		t.Fatalf("result %+v err %v", result, err)
	}
	envs := store.envelopes()
	want := []string{"unknown:capacity_rejected", "unknown:provider_error", "answered"}
	if got := states(envs); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("envelope states = %v, want %v", got, want)
	}
	for i, env := range envs {
		if env.Attempt != i || env.Step != 1 {
			t.Errorf("envelope %d: step %d attempt %d, want step 1 attempt %d", i, env.Step, env.Attempt, i)
		}
	}
	// One admission per provider attempt: the clients were called three times in all, once per envelope.
	if calls := ps.calls + len(pl.seen); calls != len(envs) || store.admissions != len(envs) {
		t.Errorf("provider attempts=%d admissions=%d envelopes=%d, want equal", calls, store.admissions, len(envs))
	}
	ids := map[string]bool{}
	for _, env := range envs {
		ids[env.RequestID] = true
	}
	if len(ids) != 3 {
		t.Errorf("request ids are not distinct: %v", ids)
	}
}

type sequenceProvider struct {
	errs  []error
	calls int
}

func (p *sequenceProvider) Model() string { return "seq" }
func (p *sequenceProvider) Complete(context.Context, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	defer func() { p.calls++ }()
	if p.calls < len(p.errs) {
		return protocol.ProviderResponse{}, p.errs[p.calls]
	}
	return protocol.ProviderResponse{}, errors.New("unexpected extra request")
}

// A provider error, a network error and a cancelled context all leave the request unknown: none of
// them proves the request was not delivered, and none is recorded as failed or answered.
func TestUndeliveredOrUnansweredRequestsAreUnknown(t *testing.T) {
	cases := map[string]struct {
		err    error
		reason string
		kind   string
	}{
		"provider error": {&protocol.ProviderError{Class: protocol.ClassAuth, Err: errors.New("denied")}, "provider_error", KindProviderAuth},
		"network error":  {errors.New("connection reset by peer"), "provider_error", KindProviderAvailability},
		"cancelled":      {context.Canceled, "cancelled", KindCancelled},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := envStore("unk")
			loop := Loop{Models: testModels(&sequenceProvider{errs: []error{tc.err}}), Store: store, Tools: fakeTools{}}
			ctx := context.Background()
			if name == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				loop.Models = testModels(cancelAfterProvider{cancel: cancel})
			}
			result, _ := loop.Run(ctx, "unk", Options{Session: testRef, MaxSteps: 2})
			if result.Status == StatusCompleted {
				t.Fatalf("result %+v", result)
			}
			envs := store.envelopes()
			if len(envs) != 1 || envs[0].State != session.RequestUnknown || envs[0].Reason != tc.reason {
				t.Fatalf("envelopes = %v, want one unknown:%s", states(envs), tc.reason)
			}
			if store.cause["kind"] != tc.kind {
				t.Errorf("run cause = %v, want %s", store.cause["kind"], tc.kind)
			}
		})
	}
}

type cancelAfterProvider struct{ cancel context.CancelFunc }

func (p cancelAfterProvider) Model() string { return "cancel" }
func (p cancelAfterProvider) Complete(ctx context.Context, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.cancel()
	return protocol.ProviderResponse{}, ctx.Err()
}

// A context that ended before the request would be sent sends nothing and records no envelope.
func TestCancelledBeforeSendRecordsNothing(t *testing.T) {
	store := envStore("pre")
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{answerOf("never")}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	loop := Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}
	_, _ = loop.Run(ctx, "pre", Options{Session: testRef, MaxSteps: 2})
	if len(provider.seen) != 0 || len(store.envelopes()) != 0 {
		t.Fatalf("requests=%d envelopes=%d, want none", len(provider.seen), len(store.envelopes()))
	}
}

// R-1: a fenced session at admission sends nothing and runs no tool; the run ends failed/fenced and
// the failed envelope the store committed is the only record.
func TestFencedAtAdmissionSendsNothing(t *testing.T) {
	store := envStore("fa")
	store.admitErr = session.ErrFenced
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{answerOf("never")}}
	tools := &scriptTools{fn: func(context.Context, string, map[string]any) (protocol.ToolResult, error) {
		return protocol.ToolResult{OK: true}, nil
	}}
	loop := Loop{Models: testModels(provider), Store: store, Tools: tools}
	result, err := loop.Run(context.Background(), "fa", Options{Session: testRef, MaxSteps: 3})
	if !errors.Is(err, session.ErrFenced) {
		t.Fatalf("err = %v, want ErrFenced", err)
	}
	assertExit(t, store, result, StatusFailed, KindFenced)
	if len(provider.seen) != 0 || tools.executed != 0 {
		t.Fatalf("requests=%d tools=%d: a fenced session sends and runs nothing", len(provider.seen), tools.executed)
	}
	if got := states(store.envelopes()); fmt.Sprint(got) != "[failed:fenced]" {
		t.Fatalf("envelopes = %v", got)
	}
}

// R-1: a fence that appears between the request and its answer keeps the answer out of the
// transcript, ends the envelope unknown, and neither retries the request nor runs the answer's tools.
func TestFencedBeforeAnswerStoresNothingAndRunsNothing(t *testing.T) {
	store := envStore("fb")
	store.fenceAnswer = true
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call("1", "bash", `{"command":"rm -rf x"}`)}}, FinishReason: "tool_calls"},
		answerOf("never"),
	}}
	tools := &scriptTools{fn: func(context.Context, string, map[string]any) (protocol.ToolResult, error) {
		return protocol.ToolResult{OK: true}, nil
	}}
	rows := len(store.messages)
	loop := Loop{Models: testModels(provider), Store: store, Tools: tools}
	result, err := loop.Run(context.Background(), "fb", Options{Session: testRef, MaxSteps: 3})
	if !errors.Is(err, session.ErrFenced) {
		t.Fatalf("err = %v, want ErrFenced", err)
	}
	assertExit(t, store, result, StatusFailed, KindFenced)
	if len(provider.seen) != 1 || tools.executed != 0 || len(store.messages) != rows {
		t.Fatalf("requests=%d tools=%d rows %d->%d: want one request, no tool, no new row", len(provider.seen), tools.executed, rows, len(store.messages))
	}
	if got := states(store.envelopes()); fmt.Sprint(got) != "[unknown:fenced_before_answer]" {
		t.Fatalf("envelopes = %v", got)
	}
}

// Admission that cannot be written is a storage failure: nothing is sent.
func TestAdmissionStorageFailureSendsNothing(t *testing.T) {
	store := envStore("sf")
	store.admitErr = errors.New("database is locked")
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{answerOf("never")}}
	loop := Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}
	result, _ := loop.Run(context.Background(), "sf", Options{Session: testRef, MaxSteps: 2})
	assertExit(t, store, result, StatusFailed, KindStorage)
	if len(provider.seen) != 0 {
		t.Fatalf("a request was sent without an envelope")
	}
}

// An answer the store cannot write leaves the request unknown, never answered.
func TestAnswerNotStoredLeavesTheRequestUnknown(t *testing.T) {
	store := envStore("ns")
	fs := &faultStore{memoryStore: store, failAppend: func(m protocol.Message) error {
		if m.Role == "assistant" {
			return errors.New("disk full")
		}
		return nil
	}}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{answerOf("lost")}}
	loop := Loop{Models: testModels(provider), Store: fs, Tools: fakeTools{}}
	result, _ := loop.Run(context.Background(), "ns", Options{Session: testRef, MaxSteps: 2})
	assertExit(t, store, result, StatusFailed, KindStorage)
	if got := states(store.envelopes()); fmt.Sprint(got) != "[unknown:answer_not_stored]" {
		t.Fatalf("envelopes = %v", got)
	}
}

type panicProvider struct{}

func (panicProvider) Model() string { return "panic" }
func (panicProvider) Complete(context.Context, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	panic("adapter bug")
}

// A panic inside the client still settles the envelope: the request may have been sent.
func TestPanicInTheClientSettlesTheEnvelope(t *testing.T) {
	store := envStore("pn")
	loop := Loop{Models: testModels(panicProvider{}), Store: store, Tools: fakeTools{}}
	result, _ := loop.Run(context.Background(), "pn", Options{Session: testRef, MaxSteps: 2})
	assertExit(t, store, result, StatusFailed, KindPanic)
	if got := states(store.envelopes()); fmt.Sprint(got) != "[unknown:panic]" {
		t.Fatalf("envelopes = %v", got)
	}
}
