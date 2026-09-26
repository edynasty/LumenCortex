package cognition

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type Effort string

const (
	EffortNone   Effort = "none"
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortMax    Effort = "max"
)

type Category struct {
	Description string   `json:"description,omitempty"`
	Models      []string `json:"models,omitempty"`
	Default     bool     `json:"default,omitempty"`
}

type Profile struct {
	Categories map[string]Category `json:"categories,omitempty"`
	Health     HealthConfig        `json:"health,omitempty"`
}

type Progress struct {
	SameFailureCount   int            `json:"sameFailureCount,omitempty"`
	MaxRepeatedFailure int            `json:"maxRepeatedFailure,omitempty"`
	NoProgressSteps    int            `json:"noProgressSteps,omitempty"`
	LastFailure        string         `json:"lastFailure,omitempty"`
	Failures           map[string]int `json:"failures,omitempty"`
}

type Signals struct {
	Category               string  `json:"category,omitempty"`
	CategoryConfidence     float64 `json:"categoryConfidence,omitempty"`
	NeedThink              float64 `json:"needThink,omitempty"`
	Stuck                  float64 `json:"stuck,omitempty"`
	EvidenceSufficiency    float64 `json:"evidenceSufficiency,omitempty"`
	Retrieval               string  `json:"retrieval,omitempty"`
	RetrievalConfidence     float64 `json:"retrievalConfidence,omitempty"`
	HasNeedThink            bool    `json:"-"`
	HasStuck                bool    `json:"-"`
	HasEvidenceSufficiency bool    `json:"-"`
}

type Input struct {
	Goal     string   `json:"goal,omitempty"`
	Focus    string   `json:"focus,omitempty"`
	Progress Progress `json:"progress,omitempty"`
	Signals  Signals  `json:"signals,omitempty"`
}

func (p *Progress) ObserveTool(name string, ok bool, content string) {
	if ok {
		p.NoProgressSteps = 0
		return
	}
	if p.Failures == nil {
		p.Failures = map[string]int{}
	}
	p.NoProgressSteps++
	signature := FailureSignature(name, content)
	p.Failures[signature]++
	p.LastFailure = signature
	p.SameFailureCount = p.Failures[signature]
	if p.SameFailureCount > p.MaxRepeatedFailure {
		p.MaxRepeatedFailure = p.SameFailureCount
	}
}

var (
	failureLargeNumber = regexp.MustCompile(`\b\d{4,}\b`)
	failureHex         = regexp.MustCompile(`(?i)0x[0-9a-f]+`)
	failurePath        = regexp.MustCompile(`/(?:[^\s:]+/)*[^\s:]+`)
)

func FailureSignature(tool, content string) string {
	normalized := failureLargeNumber.ReplaceAllString(content, "#")
	normalized = failureHex.ReplaceAllString(normalized, "0x#")
	normalized = failurePath.ReplaceAllString(normalized, "/PATH")
	if len(normalized) > 1600 {
		normalized = normalized[:1600]
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(tool) + "\n" + normalized))
	return hex.EncodeToString(sum[:10])
}

type Plan struct {
	Category   string   `json:"category"`
	Think      bool     `json:"think"`
	Effort     Effort   `json:"effort"`
	ThinkScore float64  `json:"thinkScore"`
	Retrieval  string   `json:"retrieval"`
	Reasons    []string `json:"reasons,omitempty"`
}

type Router struct {
	CategoryConfidenceThreshold          float64
	ThinkThreshold                       float64
	RetrievalConfidenceThreshold         float64
	ExpensiveRetrievalConfidenceThreshold float64
}

var (
	quickTerms    = regexp.MustCompile(`(?i)\b(typo|rename|format|lint|small|tiny|quick|one[- ]?line|single[- ]?file|copy change)\b`)
	visualTerms   = regexp.MustCompile(`(?i)\b(ui|ux|css|layout|frontend|front-end|visual|design|responsive|react|vue|svelte|wails|figma)\b`)
	researchTerms = regexp.MustCompile(`(?i)\b(research|investigate|compare|paper|papers|source|sources|latest|benchmark|survey|literature|web search)\b`)
	writingTerms  = regexp.MustCompile(`(?i)\b(readme|documentation|docs|write|rewrite|copy|guide|tutorial|explain)\b`)
	deepTerms     = regexp.MustCompile(`(?i)\b(debug|deadlock|race|concurrency|architecture|migration|refactor|security|performance|root cause|multi[- ]?module|cross[- ]?module|distributed|transaction)\b`)
	highRiskTerms = regexp.MustCompile(`(?i)\b(delete|drop|migration|production|security|auth|credential|payment|billing|database|schema|release|deploy)\b`)
	dependencyRetrievalTerms = regexp.MustCompile(`(?i)\b(dependency|dependencies|depends?|imports?|calls?|caller|callee|references?|referenced|symbols?|ownership|owns?|used by|defined in)\b`)
	causalRetrievalTerms = regexp.MustCompile(`(?i)\b(root cause|why|causes?|caused|failure|failing|failed|bug|debug|error|incident|race|deadlock|effects?|derived from)\b`)
	historicalRetrievalTerms = regexp.MustCompile(`(?i)\b(history|historical|previous|before|commit|version|regression|superseded|prior|changed since|used to)\b`)
)

func DefaultProfile() Profile {
	return Profile{
		Categories: map[string]Category{
			"quick":              {Description: "Small bounded work with obvious local scope."},
			"general":            {Description: "Normal coding, implementation, and reasoning work.", Default: true},
			"deep":               {Description: "Difficult multi-step reasoning, debugging, refactoring, or architecture work."},
			"ultrabrain":         {Description: "Exceptionally difficult work with repeated failures, contradictions, or major uncertainty."},
			"visual-engineering": {Description: "UI, layout, interaction, visual implementation, and frontend design work."},
			"research":           {Description: "Broad investigation, evidence gathering, comparison, and synthesis."},
			"writing":            {Description: "Documentation, technical writing, and explanatory content."},
		},
		Health: HealthConfig{FailureThreshold: 3, Cooldown: 30 * time.Second},
	}
}

func (r Router) Route(input Input) Plan {
	categoryThreshold := r.CategoryConfidenceThreshold
	if categoryThreshold <= 0 {
		categoryThreshold = 0.55
	}
	thinkThreshold := r.ThinkThreshold
	if thinkThreshold <= 0 {
		thinkThreshold = 0.56
	}
	retrievalThreshold := r.RetrievalConfidenceThreshold
	if retrievalThreshold <= 0 {
		retrievalThreshold = 0.68
	}
	expensiveRetrievalThreshold := r.ExpensiveRetrievalConfidenceThreshold
	if expensiveRetrievalThreshold <= 0 {
		expensiveRetrievalThreshold = 0.82
	}

	algorithmCategory, algorithmThink, algorithmRetrieval := algorithmic(input)
	retrieval := algorithmRetrieval
	category := algorithmCategory
	if strings.TrimSpace(input.Signals.Category) != "" && input.Signals.CategoryConfidence >= categoryThreshold {
		category = input.Signals.Category
	}

	thinkScore := algorithmThink
	if input.Signals.HasNeedThink || input.Signals.NeedThink > 0 {
		thinkScore = math.Max(thinkScore, clamp01(input.Signals.NeedThink)*0.9)
	}
	repeated := input.Progress.MaxRepeatedFailure
	if input.Progress.SameFailureCount > repeated {
		repeated = input.Progress.SameFailureCount
	}
	thinkScore += math.Min(0.35, float64(repeated)*0.12)
	thinkScore += math.Min(0.2, float64(input.Progress.NoProgressSteps)*0.06)
	if (input.Signals.HasStuck || input.Signals.Stuck > 0) && input.Signals.Stuck > 0.7 {
		thinkScore += 0.12
	}
	if (input.Signals.HasEvidenceSufficiency || input.Signals.EvidenceSufficiency > 0) && input.Signals.EvidenceSufficiency < 0.35 {
		thinkScore += 0.08
	}
	thinkScore = clamp01(thinkScore)

	reasons := []string{}
	if thinkScore >= thinkThreshold {
		reasons = append(reasons, "deliberation-value")
	}
	if repeated >= 2 {
		reasons = append(reasons, "repeated-failure")
	}
	if (input.Signals.HasStuck || input.Signals.Stuck > 0) && input.Signals.Stuck > 0.7 {
		reasons = append(reasons, "stuck")
	}
	if (input.Signals.HasEvidenceSufficiency || input.Signals.EvidenceSufficiency > 0) && input.Signals.EvidenceSufficiency < 0.35 {
		reasons = append(reasons, "insufficient-evidence")
	}
	modelRetrieval := strings.TrimSpace(input.Signals.Retrieval)
	if modelRetrieval != "" {
		switch {
		case algorithmRetrieval != "lexical":
			if modelRetrieval != algorithmRetrieval {
				reasons = append(reasons, "retrieval-model-constrained")
			}
		case !retrievalRelationEligible(input, modelRetrieval):
			reasons = append(reasons, "retrieval-model-constrained")
		default:
			threshold := retrievalThreshold
			if modelRetrieval == "associative" || modelRetrieval == "hybrid" {
				threshold = expensiveRetrievalThreshold
			}
			if input.Signals.RetrievalConfidence >= threshold {
				retrieval = modelRetrieval
				reasons = append(reasons, "retrieval-model-high-confidence")
			} else if modelRetrieval != "lexical" {
				reasons = append(reasons, "retrieval-model-constrained")
			}
		}
	}

	return Plan{
		Category:   category,
		Think:      thinkScore >= thinkThreshold,
		Effort:     effortFor(thinkScore, repeated),
		ThinkScore: thinkScore,
		Retrieval:  retrieval,
		Reasons:    reasons,
	}
}

func ResolveChain(profile Profile, category string, available func(string) bool) ([]string, error) {
	if len(profile.Categories) == 0 {
		profile = DefaultProfile()
	}
	selected, ok := profile.Categories[category]
	if !ok {
		var found bool
		for name, item := range profile.Categories {
			if item.Default {
				category, selected, found = name, item, true
				break
			}
		}
		if !found {
			return nil, errors.New("cognitive profile has no matching or default category")
		}
	}
	out := make([]string, 0, len(selected.Models))
	for _, model := range selected.Models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if available == nil || available(model) {
			out = append(out, model)
		}
	}
	return out, nil
}

func algorithmic(input Input) (string, float64, string) {
	text := strings.TrimSpace(input.Goal + " " + input.Focus)
	scores := map[string]float64{
		"quick": 0.12, "general": 0.3, "deep": 0.12, "ultrabrain": 0.02,
		"visual-engineering": 0.04, "research": 0.04, "writing": 0.04,
	}
	if quickTerms.MatchString(text) || len(text) < 80 {
		scores["quick"] += 0.35
	}
	if visualTerms.MatchString(text) {
		scores["visual-engineering"] += 0.7
	}
	if researchTerms.MatchString(text) {
		scores["research"] += 0.65
	}
	if writingTerms.MatchString(text) {
		scores["writing"] += 0.45
	}
	if deepTerms.MatchString(text) {
		scores["deep"] += 0.6
	}
	if input.Progress.MaxRepeatedFailure >= 2 {
		scores["deep"] += 0.3
	}
	if input.Progress.MaxRepeatedFailure >= 3 || input.Progress.NoProgressSteps >= 4 {
		scores["ultrabrain"] += 0.85
	}

	keys := make([]string, 0, len(scores))
	for key := range scores {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	category := "general"
	best := -1.0
	for _, key := range keys {
		if scores[key] > best {
			category, best = key, scores[key]
		}
	}

	think := 0.18
	if deepTerms.MatchString(text) {
		think += 0.32
	}
	if researchTerms.MatchString(text) {
		think += 0.16
	}
	if highRiskTerms.MatchString(text) {
		think += 0.15
	}
	if len(text) > 500 {
		think += 0.08
	}

	retrieval := "lexical"
	switch {
	case historicalRetrievalTerms.MatchString(text):
		retrieval = "historical"
	case causalRetrievalTerms.MatchString(text):
		retrieval = "causal"
	case dependencyRetrievalTerms.MatchString(text):
		retrieval = "dependency"
	}
	return category, clamp01(think), retrieval
}

func retrievalRelationEligible(input Input, choice string) bool {
	text := strings.TrimSpace(input.Goal + " " + input.Focus)
	switch choice {
	case "dependency":
		return dependencyRetrievalTerms.MatchString(text)
	case "historical":
		return historicalRetrievalTerms.MatchString(text)
	case "causal":
		if causalRetrievalTerms.MatchString(text) {
			return true
		}
		repeated := input.Progress.MaxRepeatedFailure
		if input.Progress.SameFailureCount > repeated {
			repeated = input.Progress.SameFailureCount
		}
		return repeated >= 2
	case "lexical", "associative", "hybrid":
		return true
	default:
		return false
	}
}

func effortFor(score float64, repeated int) Effort {
	switch {
	case repeated >= 4 || score >= 0.86:
		return EffortMax
	case score >= 0.7:
		return EffortHigh
	case score >= 0.48:
		return EffortMedium
	default:
		return EffortLow
	}
}

func clamp01(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

type HealthConfig struct {
	FailureThreshold int           `json:"failureThreshold,omitempty"`
	Cooldown         time.Duration `json:"-"`
	CooldownMS       int64         `json:"cooldownMs,omitempty"`
}

type HealthState struct {
	Successes           int       `json:"successes"`
	Failures            int       `json:"failures"`
	ConsecutiveFailures int       `json:"consecutiveFailures"`
	OpenUntil           time.Time `json:"openUntil,omitempty"`
	LastError           string    `json:"lastError,omitempty"`
	UpdatedAt           time.Time `json:"updatedAt,omitempty"`
}

type HealthRegistry struct {
	mu       sync.Mutex
	config   HealthConfig
	now      func() time.Time
	states   map[string]HealthState
}

func NewHealthRegistry(config HealthConfig) *HealthRegistry {
	if config.FailureThreshold <= 0 {
		config.FailureThreshold = 3
	}
	if config.Cooldown <= 0 {
		if config.CooldownMS > 0 {
			config.Cooldown = time.Duration(config.CooldownMS) * time.Millisecond
		} else {
			config.Cooldown = 30 * time.Second
		}
	}
	return &HealthRegistry{
		config: config,
		now:    time.Now,
		states: map[string]HealthState{},
	}
}

func (r *HealthRegistry) SetClock(now func() time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now != nil {
		r.now = now
	}
}

func (r *HealthRegistry) Available(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.states[key]
	return state.OpenUntil.IsZero() || !r.now().Before(state.OpenUntil)
}

func (r *HealthRegistry) Success(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.states[key]
	state.Successes++
	state.ConsecutiveFailures = 0
	state.OpenUntil = time.Time{}
	state.LastError = ""
	state.UpdatedAt = r.now()
	r.states[key] = state
}

func (r *HealthRegistry) Failure(key string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.states[key]
	state.Failures++
	state.ConsecutiveFailures++
	if err != nil {
		state.LastError = err.Error()
	}
	state.UpdatedAt = r.now()
	if state.ConsecutiveFailures >= r.config.FailureThreshold {
		state.OpenUntil = state.UpdatedAt.Add(r.config.Cooldown)
	}
	r.states[key] = state
}

func (r *HealthRegistry) Snapshot() map[string]HealthState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]HealthState, len(r.states))
	for key, value := range r.states {
		out[key] = value
	}
	return out
}
