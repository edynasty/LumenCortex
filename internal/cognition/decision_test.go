package cognition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

func TestSystemOneProviderParsesTypedSignalsIncludingExplicitZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" {
			t.Fatalf("path=%q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("authorization=%q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "jev-test" {
			t.Fatalf("model=%#v", body["model"])
		}
		if _, ok := body["questions"].(map[string]any); !ok {
			t.Fatalf("questions=%#v", body["questions"])
		}
		fmt.Fprint(w, `{
			"model":"jev-test",
			"answers":{
				"category":{"type":"choice","choice":"research","confidence":0.93},
				"need_think":{"type":"noul","noul":0.88},
				"evidence_sufficient":{"type":"noul","noul":0.0},
				"stuck":{"type":"noul","noul":0.76},
				"retrieval":{"type":"choice","choice":"historical","confidence":0.84}
			},
			"usage":{"requests":1}
		}`)
	}))
	defer server.Close()

	provider, err := NewSystemOneProvider(SystemOneConfig{
		Name: "jev-test",
		BaseURL: server.URL,
		APIKey: "secret",
		Model: "jev-test",
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.Decide(context.Background(), DecisionRequest{
		State: Input{Goal: "compare prior architecture decisions"},
		Questions: DefaultDecisionQuestions(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Signals.Category != "research" || result.Signals.CategoryConfidence != 0.93 {
		t.Fatalf("category signals=%#v", result.Signals)
	}
	if !result.Signals.HasNeedThink || result.Signals.NeedThink != 0.88 {
		t.Fatalf("need_think=%#v", result.Signals)
	}
	if !result.Signals.HasEvidenceSufficiency || result.Signals.EvidenceSufficiency != 0 {
		t.Fatalf("explicit zero evidence signal lost: %#v", result.Signals)
	}
	if !result.Signals.HasStuck || result.Signals.Stuck != 0.76 {
		t.Fatalf("stuck=%#v", result.Signals)
	}
	if result.Signals.Retrieval != "historical" || result.Signals.RetrievalConfidence != 0.84 {
		t.Fatalf("retrieval=%q confidence=%f", result.Signals.Retrieval, result.Signals.RetrievalConfidence)
	}

	plan := (Router{}).Route(Input{
		Goal: "compare prior architecture decisions",
		Signals: result.Signals,
	})
	if plan.Category != "research" || !plan.Think {
		t.Fatalf("plan=%#v", plan)
	}
	if !containsReason(plan.Reasons, "insufficient-evidence") || !containsReason(plan.Reasons, "stuck") {
		t.Fatalf("reasons=%#v", plan.Reasons)
	}
}

type fakeDecisionProvider struct {
	name    string
	model   string
	signals Signals
	fail    bool
	calls   int
}

func (p *fakeDecisionProvider) Name() string  { return p.name }
func (p *fakeDecisionProvider) Model() string { return p.model }

func (p *fakeDecisionProvider) Decide(context.Context, DecisionRequest) (DecisionResult, error) {
	p.calls++
	if p.fail {
		return DecisionResult{}, errors.New("decision unavailable")
	}
	return DecisionResult{
		Source: p.name,
		Model: p.model,
		Signals: p.signals,
	}, nil
}

func TestDecisionLayerUsesCircuitBreakerAndFallsThroughProviders(t *testing.T) {
	health := NewHealthRegistry(HealthConfig{
		FailureThreshold: 1,
		Cooldown: time.Hour,
	})
	bad := &fakeDecisionProvider{name: "jev", model: "bad", fail: true}
	good := &fakeDecisionProvider{
		name: "laya",
		model: "good",
		signals: Signals{Category: "writing", CategoryConfidence: 0.91},
	}
	layer := DecisionLayer{
		Providers: []DecisionProvider{bad, good},
		Health: health,
	}

	first, err := layer.Decide(context.Background(), DecisionRequest{State: Input{Goal: "write docs"}})
	if err != nil {
		t.Fatal(err)
	}
	if first.Signals.Category != "writing" {
		t.Fatalf("signals=%#v", first.Signals)
	}
	if bad.calls != 1 || good.calls != 1 {
		t.Fatalf("calls bad=%d good=%d", bad.calls, good.calls)
	}
	if health.Available("decision:jev:bad") {
		t.Fatal("expected Jev decision circuit open")
	}
	if len(first.Errors) != 1 || first.Errors[0].Provider != "jev" {
		t.Fatalf("errors=%#v", first.Errors)
	}

	second, err := layer.Decide(context.Background(), DecisionRequest{State: Input{Goal: "write docs"}})
	if err != nil {
		t.Fatal(err)
	}
	if bad.calls != 1 || good.calls != 2 {
		t.Fatalf("second calls bad=%d good=%d", bad.calls, good.calls)
	}
	if len(second.Errors) != 1 || !second.Errors[0].Skipped || second.Errors[0].Message != "circuit-open" {
		t.Fatalf("second errors=%#v", second.Errors)
	}
}

func TestSystemOneProviderHTTPErrorCarriesStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	provider, err := NewSystemOneProvider(SystemOneConfig{BaseURL: server.URL, Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Decide(context.Background(), DecisionRequest{State: Input{Goal: "x"}})
	var httpErr *DecisionHTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("err=%v", err)
	}
}

func containsReason(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}


type fakeGenerativeProtocolProvider struct {
	model    string
	response protocol.ProviderResponse
	err      error
	last     protocol.ProviderRequest
	calls    int
}

func (p *fakeGenerativeProtocolProvider) Model() string { return p.model }

func (p *fakeGenerativeProtocolProvider) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.calls++
	p.last = req
	if p.err != nil {
		return protocol.ProviderResponse{}, p.err
	}
	return p.response, nil
}

func TestGenerativeDecisionProviderParsesFencedTypedSignalsAndDiscountsConfidence(t *testing.T) {
	base := &fakeGenerativeProtocolProvider{
		model: "router-model",
		response: protocol.ProviderResponse{
			Message: protocol.Message{
				Role: "assistant",
				Content: "```json\n" + `{"answers":{"category":{"type":"choice","choice":"deep","confidence":0.9},"need_think":{"type":"noul","noul":0.8},"evidence_sufficient":{"type":"noul","noul":0.3},"stuck":{"type":"noul","noul":0.2},"retrieval":{"type":"choice","choice":"causal","confidence":0.95}}}` + "\n```",
			},
			Usage: protocol.Usage{TotalTokens: 42},
		},
	}
	provider, err := NewGenerativeDecisionProvider(GenerativeDecisionConfig{
		Name: "generative:deepseek",
		Provider: base,
		ConfidenceScale: 0.85,
		ConfidenceCap: 0.9,
		ScoreScale: 0.85,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.Decide(context.Background(), DecisionRequest{
		State: Input{Goal: "Debug the root cause of a race"},
		Questions: DefaultDecisionQuestions(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if base.calls != 1 || len(base.last.Messages) != 2 {
		t.Fatalf("calls=%d request=%#v", base.calls, base.last)
	}
	if base.last.ToolChoice != "none" || base.last.ReasoningEffort != "low" {
		t.Fatalf("request=%#v", base.last)
	}
	if !strings.Contains(base.last.Messages[0].Content, "bounded Decision Layer") ||
		!strings.Contains(base.last.Messages[0].Content, "Do not execute the task") {
		t.Fatalf("system prompt=%q", base.last.Messages[0].Content)
	}
	if result.Signals.Category != "deep" || math.Abs(result.Signals.CategoryConfidence-0.765) > 1e-9 {
		t.Fatalf("category signals=%#v", result.Signals)
	}
	if result.Signals.Retrieval != "causal" || math.Abs(result.Signals.RetrievalConfidence-0.8075) > 1e-9 {
		t.Fatalf("retrieval signals=%#v", result.Signals)
	}
	if !result.Signals.HasNeedThink || math.Abs(result.Signals.NeedThink-0.755) > 1e-9 {
		t.Fatalf("need think=%#v", result.Signals)
	}
	if !result.Signals.HasEvidenceSufficiency || math.Abs(result.Signals.EvidenceSufficiency-0.33) > 1e-9 {
		t.Fatalf("evidence=%#v", result.Signals)
	}
}

func TestGenerativeDecisionProviderMalformedOutputFallsThroughCircuit(t *testing.T) {
	badBase := &fakeGenerativeProtocolProvider{
		model: "bad-model",
		response: protocol.ProviderResponse{
			Message: protocol.Message{Role: "assistant", Content: "not-json"},
		},
	}
	bad, err := NewGenerativeDecisionProvider(GenerativeDecisionConfig{
		Name: "bad-generative",
		Provider: badBase,
	})
	if err != nil {
		t.Fatal(err)
	}
	good := &fakeDecisionProvider{
		name: "laya",
		model: "good",
		signals: Signals{Category: "writing", CategoryConfidence: 0.9},
	}
	health := NewHealthRegistry(HealthConfig{
		FailureThreshold: 1,
		Cooldown: time.Hour,
	})
	layer := DecisionLayer{
		Providers: []DecisionProvider{bad, good},
		Health: health,
	}
	first, err := layer.Decide(context.Background(), DecisionRequest{
		State: Input{Goal: "write docs"},
		Questions: DefaultDecisionQuestions(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Signals.Category != "writing" || badBase.calls != 1 {
		t.Fatalf("first=%#v calls=%d", first, badBase.calls)
	}
	second, err := layer.Decide(context.Background(), DecisionRequest{
		State: Input{Goal: "write docs"},
		Questions: DefaultDecisionQuestions(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if badBase.calls != 1 || len(second.Errors) == 0 || !second.Errors[0].Skipped {
		t.Fatalf("second=%#v calls=%d", second, badBase.calls)
	}
}
