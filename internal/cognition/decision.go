package cognition

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

type DecisionProvider interface {
	Name() string
	Model() string
	Decide(context.Context, DecisionRequest) (DecisionResult, error)
}

type DecisionRequest struct {
	State     Input          `json:"state"`
	Questions map[string]any `json:"questions,omitempty"`
}

type DecisionResult struct {
	Source  string          `json:"source"`
	Model   string          `json:"model,omitempty"`
	Signals Signals         `json:"signals"`
	Usage   json.RawMessage `json:"usage,omitempty"`
	Raw     json.RawMessage `json:"raw,omitempty"`
}

type DecisionError struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	Message  string `json:"message"`
	Status   int    `json:"status,omitempty"`
	Skipped  bool   `json:"skipped,omitempty"`
}

type DecisionSummary struct {
	Signals Signals          `json:"signals"`
	Results []DecisionResult `json:"results,omitempty"`
	Errors  []DecisionError  `json:"errors,omitempty"`
}

type DecisionLayer struct {
	Providers []DecisionProvider
	Policy    string
	Health    *HealthRegistry
}

func (l *DecisionLayer) Decide(ctx context.Context, req DecisionRequest) (DecisionSummary, error) {
	summary := DecisionSummary{}
	policy := strings.ToLower(strings.TrimSpace(l.Policy))
	if policy == "" {
		policy = "first"
	}

	for _, provider := range l.Providers {
		if provider == nil {
			continue
		}
		key := decisionHealthKey(provider)
		if l.Health != nil && !l.Health.Available(key) {
			summary.Errors = append(summary.Errors, DecisionError{
				Provider: provider.Name(),
				Model: provider.Model(),
				Message: "circuit-open",
				Skipped: true,
			})
			continue
		}

		result, err := provider.Decide(ctx, req)
		if err != nil {
			if ctx.Err() != nil {
				return summary, ctx.Err()
			}
			if l.Health != nil {
				l.Health.Failure(key, err)
			}
			status := 0
			var httpErr *DecisionHTTPError
			if errors.As(err, &httpErr) {
				status = httpErr.StatusCode
			}
			summary.Errors = append(summary.Errors, DecisionError{
				Provider: provider.Name(),
				Model: provider.Model(),
				Message: err.Error(),
				Status: status,
			})
			continue
		}

		if l.Health != nil {
			l.Health.Success(key)
		}
		summary.Results = append(summary.Results, result)
		summary.Signals = mergeSignals(summary.Signals, result.Signals)
		if policy != "all" {
			break
		}
	}
	return summary, nil
}

type SystemOneConfig struct {
	Name       string
	BaseURL    string
	APIKey     string
	Model      string
	Headers    map[string]string
	HTTPClient *http.Client
	Timeout    time.Duration
}

type SystemOneProvider struct {
	name       string
	baseURL    string
	apiKey     string
	model      string
	headers    map[string]string
	httpClient *http.Client
	timeout    time.Duration
}

type DecisionHTTPError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *DecisionHTTPError) Error() string {
	if strings.TrimSpace(e.Body) == "" {
		return fmt.Sprintf("decision provider HTTP %d: %s", e.StatusCode, e.Status)
	}
	return fmt.Sprintf("decision provider HTTP %d: %s: %s", e.StatusCode, e.Status, e.Body)
}

func NewSystemOneProvider(cfg SystemOneConfig) (*SystemOneProvider, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		return nil, errors.New("decision provider base URL is required")
	}
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return nil, errors.New("decision provider base URL must use http or https")
	}
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		name = "system-one"
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	headers := map[string]string{}
	for key, value := range cfg.Headers {
		headers[key] = value
	}
	return &SystemOneProvider{
		name: name,
		baseURL: baseURL,
		apiKey: cfg.APIKey,
		model: cfg.Model,
		headers: headers,
		httpClient: client,
		timeout: timeout,
	}, nil
}

func NewJevDecisionProvider(cfg SystemOneConfig) (*SystemOneProvider, error) {
	if strings.TrimSpace(cfg.Name) == "" {
		cfg.Name = "jev"
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		cfg.BaseURL = "https://api.typesafe.ai"
	}
	if strings.TrimSpace(cfg.Model) == "" {
		cfg.Model = "jev-latest"
	}
	return NewSystemOneProvider(cfg)
}

func NewLayaDecisionProvider(cfg SystemOneConfig) (*SystemOneProvider, error) {
	if strings.TrimSpace(cfg.Name) == "" {
		cfg.Name = "laya"
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		cfg.BaseURL = "http://127.0.0.1:8000"
	}
	return NewSystemOneProvider(cfg)
}

func (p *SystemOneProvider) Name() string  { return p.name }
func (p *SystemOneProvider) Model() string { return p.model }

func (p *SystemOneProvider) Decide(ctx context.Context, req DecisionRequest) (DecisionResult, error) {
	activeCtx := ctx
	cancel := func() {}
	if p.timeout > 0 {
		activeCtx, cancel = context.WithTimeout(ctx, p.timeout)
	}
	defer cancel()

	payload := map[string]any{
		"state": req.State,
	}
	if len(req.Questions) > 0 {
		payload["questions"] = req.Questions
	}
	if strings.TrimSpace(p.model) != "" {
		payload["model"] = p.model
	}
	rawBody, err := json.Marshal(payload)
	if err != nil {
		return DecisionResult{}, err
	}

	httpReq, err := http.NewRequestWithContext(
		activeCtx,
		http.MethodPost,
		p.baseURL+"/v1/systemone",
		bytes.NewReader(rawBody),
	)
	if err != nil {
		return DecisionResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	for key, value := range p.headers {
		httpReq.Header.Set(key, value)
	}

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return DecisionResult{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return DecisionResult{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return DecisionResult{}, &DecisionHTTPError{
			StatusCode: resp.StatusCode,
			Status: resp.Status,
			Body: strings.TrimSpace(string(raw)),
		}
	}

	var decoded struct {
		Model   string                     `json:"model"`
		Answers map[string]json.RawMessage `json:"answers"`
		Usage   json.RawMessage            `json:"usage"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return DecisionResult{}, err
	}
	signals := signalsFromAnswers(decoded.Answers)
	model := decoded.Model
	if model == "" {
		model = p.model
	}
	return DecisionResult{
		Source: p.name,
		Model: model,
		Signals: signals,
		Usage: decoded.Usage,
		Raw: append(json.RawMessage(nil), raw...),
	}, nil
}

func signalsFromAnswers(answers map[string]json.RawMessage) Signals {
	out := Signals{}
	if answer, ok := decodeAnswer(answers["category"]); ok {
		out.Category = answer.Choice
		out.CategoryConfidence = answer.confidence()
	}
	if answer, ok := decodeAnswer(answers["need_think"]); ok && answer.Noul != nil {
		out.NeedThink = clamp01(*answer.Noul)
		out.HasNeedThink = true
	}
	if answer, ok := decodeAnswer(answers["stuck"]); ok && answer.Noul != nil {
		out.Stuck = clamp01(*answer.Noul)
		out.HasStuck = true
	}
	if answer, ok := decodeAnswer(answers["evidence_sufficient"]); ok && answer.Noul != nil {
		out.EvidenceSufficiency = clamp01(*answer.Noul)
		out.HasEvidenceSufficiency = true
	}
	if answer, ok := decodeAnswer(answers["retrieval"]); ok {
		out.Retrieval = answer.Choice
	}
	return out
}

type typedAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Confidence    *float64           `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
	Noul          *float64           `json:"noul"`
	Score         *float64           `json:"score"`
}

func decodeAnswer(raw json.RawMessage) (typedAnswer, bool) {
	if len(raw) == 0 {
		return typedAnswer{}, false
	}
	var answer typedAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return typedAnswer{}, false
	}
	return answer, true
}

func (a typedAnswer) confidence() float64 {
	if a.Confidence != nil {
		return clamp01(*a.Confidence)
	}
	best := 0.0
	for _, probability := range a.Probabilities {
		if probability > best {
			best = probability
		}
	}
	return clamp01(best)
}

func mergeSignals(base, next Signals) Signals {
	out := base
	if strings.TrimSpace(next.Category) != "" && next.CategoryConfidence >= 0.5 {
		out.Category = next.Category
		out.CategoryConfidence = next.CategoryConfidence
	}
	if next.HasNeedThink {
		out.NeedThink = next.NeedThink
		out.HasNeedThink = true
	}
	if next.HasStuck {
		out.Stuck = next.Stuck
		out.HasStuck = true
	}
	if next.HasEvidenceSufficiency {
		out.EvidenceSufficiency = next.EvidenceSufficiency
		out.HasEvidenceSufficiency = true
	}
	if strings.TrimSpace(next.Retrieval) != "" {
		out.Retrieval = next.Retrieval
	}
	return out
}

func decisionHealthKey(provider DecisionProvider) string {
	model := strings.TrimSpace(provider.Model())
	if model == "" {
		model = "default"
	}
	return "decision:" + provider.Name() + ":" + model
}

func topProbability(values map[string]float64) float64 {
	best := 0.0
	for _, value := range values {
		best = math.Max(best, value)
	}
	return best
}


func DefaultDecisionQuestions() map[string]any {
	profile := DefaultProfile()
	criteria := map[string]string{}
	for name, category := range profile.Categories {
		description := strings.TrimSpace(category.Description)
		if description == "" {
			description = name
		}
		criteria[name] = description
	}
	return map[string]any{
		"category": map[string]any{
			"type": "choice",
			"instructions": "Which work category best matches the current task state?",
			"criteria": criteria,
		},
		"need_think": map[string]any{
			"type": "noul",
			"instructions": "Would deliberate multi-step reasoning materially improve the next decision?",
		},
		"evidence_sufficient": map[string]any{
			"type": "noul",
			"instructions": "Is the currently selected evidence sufficient for the next action?",
		},
		"stuck": map[string]any{
			"type": "noul",
			"instructions": "Is the current strategy stuck or repeating without useful progress?",
		},
		"retrieval": map[string]any{
			"type": "choice",
			"instructions": "Which retrieval direction is most useful next?",
			"criteria": map[string]string{
				"lexical": "Exact or lexical lookup is sufficient.",
				"dependency": "Follow calls, imports, dependencies, and structural relations.",
				"causal": "Follow causes, derived evidence, effects, and failure chains.",
				"historical": "Use prior sessions, changes, superseded facts, or temporal history.",
			},
		},
	}
}
