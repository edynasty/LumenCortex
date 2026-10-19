package agent

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/protocol"
)

// Auto effort (effort.auto): one classification per Start, before the first request. The judge is
// optional; every judge failure falls back to the deterministic heuristic. The classifier's input
// text is never persisted: the decision records only its source, length, ceiling, judge ref and
// result.

// Auto classifier sources and fallback classes, recorded in rule effort.auto.
const (
	AutoSourceJudge     = "judge"
	AutoSourceHeuristic = "heuristic"

	// AutoFallbackNoModel: no judge model is configured, or its client could not be built.
	AutoFallbackNoModel = "no_model"
	// AutoFallbackTimeout: the judge request outlived effort.auto.timeoutSeconds.
	AutoFallbackTimeout = "timeout"
	// AutoFallbackUnparsable: the judge answered, but no level could be read from its reply.
	AutoFallbackUnparsable = "unparsable"
	// AutoFallbackError: a judge request failed for a reason with no provider class.
	AutoFallbackError = "error"
)

// AutoContextLimit is how many runes of each classifier input are kept: the new user input, the
// previous substantive request and the last assistant reply.
const AutoContextLimit = 2000

const (
	autoJudgeMaxTokens = 64
	// autoJudgePrompt states the rubric; the levels are the same five config accepts.
	autoJudgePrompt = `You classify how much reasoning effort a coding-agent turn needs.
Reply with exactly one JSON object and nothing else: {"level":"none|low|medium|high|max"}.
- none: no reasoning; a trivial acknowledgement or a single lookup.
- low: a short question or a small, well-specified edit in one place.
- medium: a normal coding task: a few files, a focused bug fix, a small feature.
- high: a multi-step task: debugging across files, a refactor, tests plus implementation, a migration.
- max: a long-horizon or high-risk task: architecture, concurrency, security, a large migration.
Judge the new user input in context. A short follow-up such as "continue" takes the difficulty of the previous request it continues.`
)

var autoJudgeTemperature = 0.0

// autoInputs and autoOutput are the decision log of rule effort.auto. They deliberately carry no
// user or assistant text and no judge reply: the classification is not reproducible from the log,
// so ReplayDecision checks its invariants instead of recomputing it.
type autoInputs struct {
	Source string `json:"source"`
	// FallbackReason is a class, never a provider error body: no_model, timeout, unparsable, error,
	// or the adapter's error class (auth, capacity, availability, bad_request).
	FallbackReason string `json:"fallbackReason,omitempty"`
	// InputLength is the length in runes of the turn's new input (0 for a continuation with no new
	// message).
	InputLength int             `json:"inputLength"`
	Ceiling     config.Effort   `json:"ceiling"`
	JudgeModel  config.ModelRef `json:"judgeModel,omitempty"`
	// Reused reports that a resumed turn reused the level its earlier run resolved instead of
	// calling the judge again.
	Reused bool `json:"reused,omitempty"`
}

type autoOutput struct {
	Level config.Effort `json:"level"`
}

// AutoDecision records the Start's auto resolution.
func AutoDecision(source, fallback string, inputLength int, ceiling config.Effort, judgeModel config.ModelRef, level config.Effort, reused bool) Decision {
	return newDecision(DecisionEffort, RuleEffortAuto,
		autoInputs{Source: source, FallbackReason: fallback, InputLength: inputLength, Ceiling: ceiling, JudgeModel: judgeModel, Reused: reused},
		autoOutput{Level: level})
}

// AutoDecisionOfStep returns the effort.auto decision of a persisted step record, if it has one.
func AutoDecisionOfStep(raw []byte) (Decision, bool) {
	var step struct {
		Decisions []Decision `json:"decisions"`
	}
	if json.Unmarshal(raw, &step) != nil {
		return Decision{}, false
	}
	for i := len(step.Decisions) - 1; i >= 0; i-- {
		d := step.Decisions[i]
		if d.Kind == DecisionEffort && d.Rule == RuleEffortAuto {
			return d, true
		}
	}
	return Decision{}, false
}

// AutoDecisionLevel reads a decision's resolved level and classifier source.
func AutoDecisionLevel(d Decision) (config.Effort, string, bool) {
	if d.Kind != DecisionEffort || d.Rule != RuleEffortAuto {
		return "", "", false
	}
	var in autoInputs
	var out autoOutput
	if decodeDecision(d, &in, &out) != nil || !out.Level.IsLevel() {
		return "", "", false
	}
	return out.Level, in.Source, true
}

// ReusedAutoDecision turns the prior resolution of an in-progress turn into this Start's decision,
// capped to the current ceiling. It is used when a resumed Start has no new user input: the turn
// already resolved its level, and re-running the judge could change it mid-turn.
func ReusedAutoDecision(prior Decision, ceiling config.Effort, inputLength int) (config.Effort, Decision, bool) {
	level, source, ok := AutoDecisionLevel(prior)
	if !ok {
		return "", Decision{}, false
	}
	var in autoInputs
	var out autoOutput
	if decodeDecision(prior, &in, &out) != nil {
		return "", Decision{}, false
	}
	level = CappedAutoEffort(level, ceiling)
	return level, AutoDecision(source, "", inputLength, ceiling, in.JudgeModel, level, true), true
}

// AutoJudgeMessages builds the judge's request: one system message with the rubric and one user
// message holding the truncated new input and its context. It has no tools.
func AutoJudgeMessages(input, prevUser, lastAssistant string) []protocol.Message {
	var b strings.Builder
	b.WriteString("Previous user request:\n")
	b.WriteString(orNone(truncateRunes(prevUser, AutoContextLimit)))
	b.WriteString("\n\nLast assistant reply:\n")
	b.WriteString(orNone(truncateRunes(lastAssistant, AutoContextLimit)))
	b.WriteString("\n\nNew user input:\n")
	b.WriteString(orNone(truncateRunes(input, AutoContextLimit)))
	return []protocol.Message{
		{Role: "system", Content: autoJudgePrompt},
		{Role: "user", Content: b.String()},
	}
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}

func truncateRunes(s string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit]) + "…"
}

// JudgeEffort is the reasoning effort of the judge request: none when the model lists it, else low
// when it lists that, else "" (send no reasoning parameter). The judge is a small classification
// call and must not spend a large reasoning budget.
func JudgeEffort(supported []config.Effort) string {
	if len(supported) == 0 {
		return string(config.EffortNone)
	}
	for _, want := range []config.Effort{config.EffortNone, config.EffortLow} {
		if slices.Contains(supported, want) {
			return string(want)
		}
	}
	return ""
}

// ClassifyWithJudge makes one non-streaming, tool-less request and reads the level from the reply.
// A returned error is only the run's own cancellation; every other failure is reported as a
// fallback class with source heuristic left to the caller. The reply itself is not returned.
func ClassifyWithJudge(ctx context.Context, p protocol.Provider, supported []config.Effort, timeout time.Duration, input, prevUser, lastAssistant string) (config.Effort, string, string, error) {
	if timeout <= 0 {
		timeout = time.Duration(config.DefaultAutoTimeout) * time.Second
	}
	judgeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	response, err := p.Complete(judgeCtx, protocol.ProviderRequest{
		Messages:        AutoJudgeMessages(input, prevUser, lastAssistant),
		Temperature:     &autoJudgeTemperature,
		MaxTokens:       autoJudgeMaxTokens,
		ReasoningEffort: JudgeEffort(supported),
	})
	if err != nil {
		if ctx.Err() != nil {
			return "", "", "", ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(judgeCtx.Err(), context.DeadlineExceeded) {
			return "", "", AutoFallbackTimeout, nil
		}
		var providerErr *protocol.ProviderError
		if errors.As(err, &providerErr) && providerErr.Class != "" {
			return "", "", string(providerErr.Class), nil
		}
		return "", "", AutoFallbackError, nil
	}
	level, ok := ParseJudgeLevel(response.Message.Content)
	if !ok {
		return "", "", AutoFallbackUnparsable, nil
	}
	return level, AutoSourceJudge, "", nil
}

// ParseJudgeLevel reads a level from the judge's reply tolerantly: the first JSON object with a
// "level" member, else the first standalone level word. "auto" is not a judge output.
func ParseJudgeLevel(content string) (config.Effort, bool) {
	s := strings.TrimSpace(content)
	if object, ok := firstJSONObject(s); ok {
		var value struct {
			Level string `json:"level"`
		}
		if json.Unmarshal([]byte(object), &value) == nil {
			if level, ok := levelWord(value.Level); ok {
				return level, true
			}
		}
	}
	for _, field := range strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) }) {
		if level, ok := levelWord(field); ok {
			return level, true
		}
	}
	return "", false
}

func levelWord(s string) (config.Effort, bool) {
	level := config.Effort(strings.ToLower(strings.TrimSpace(s)))
	if level.IsLevel() {
		return level, true
	}
	return "", false
}

// firstJSONObject returns the first balanced {...} span, ignoring braces inside strings.
func firstJSONObject(s string) (string, bool) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", false
	}
	depth := 0
	inString, escaped := false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

var (
	stackTracePattern = regexp.MustCompile(`(?i)(panic:|traceback \(most recent call last\)|goroutine \d+ \[|fatal error:|segmentation fault|error ts\d+|\bat .+\(.+:\d+:\d+\))`)
	fileRefPattern    = regexp.MustCompile(`(?i)\b[\w./-]+\.(go|ts|tsx|js|jsx|py|rs|java|kt|rb|php|cs|cpp|c|h|md|json|ya?ml|toml|sql|sh|css|html)\b`)
	numberedPattern   = regexp.MustCompile(`(?m)^\s*(?:\d+[.)]|[-*])\s+\S`)
	sequencePattern   = regexp.MustCompile(`(?i)\b(then|after that|afterwards|next|finally|first|second|third)\b`)
	imperativePattern = regexp.MustCompile(`(?i)\b(add|build|change|create|debug|delete|fix|implement|investigate|migrate|move|optimi[sz]e|profile|refactor|remove|rename|replace|run|test|update|write)\b`)
)

// continuationWords are short follow-ups that inherit the previous request's difficulty.
var continuationWords = map[string]bool{
	"continue": true, "go on": true, "keep going": true, "proceed": true, "next": true,
	"do it": true, "yes": true, "ok": true, "okay": true, "go ahead": true, "sounds good": true,
}

// HeuristicEffort is the deterministic fallback classifier. It returns low, medium or high (never
// none or max) from signals in the new input, or the previous substantive request for a short
// continuation. The rules are documented in docs/go-provider.md.
func HeuristicEffort(input, prevUser string) config.Effort {
	text := strings.TrimSpace(input)
	if text == "" || isContinuation(text) {
		if strings.TrimSpace(prevUser) != "" {
			return heuristicLevel(prevUser)
		}
		return config.EffortLow
	}
	return heuristicLevel(text)
}

func isContinuation(s string) bool {
	s = strings.ToLower(strings.TrimSpace(strings.TrimRight(s, ".!")))
	return utf8.RuneCountInString(s) <= 24 && continuationWords[s]
}

func heuristicLevel(text string) config.Effort {
	text = strings.TrimSpace(text)
	if text == "" {
		return config.EffortLow
	}
	score := 0
	switch runes := utf8.RuneCountInString(text); {
	case runes >= 1500:
		score += 2
	case runes >= 600:
		score++
	}
	if strings.Contains(text, "```") {
		score++
	}
	if stackTracePattern.MatchString(text) {
		score++
	}
	if fileRefPattern.MatchString(text) {
		score++
	}
	imperatives := distinctImperatives(text)
	steps := 0
	if numberedPattern.MatchString(text) {
		steps++
	}
	if sequencePattern.MatchString(text) {
		steps++
	}
	if imperatives >= 2 {
		steps++
	}
	if imperatives >= 4 {
		steps++
	}
	switch {
	case steps >= 3:
		score += 2
	case steps >= 1:
		score++
	}
	if imperatives >= 3 {
		score += 2
	}
	if questionOnly(text, imperatives) {
		score--
	}
	switch {
	case score >= 3:
		return config.EffortHigh
	case score >= 1:
		return config.EffortMedium
	default:
		return config.EffortLow
	}
}

func distinctImperatives(text string) int {
	seen := map[string]bool{}
	for _, match := range imperativePattern.FindAllString(text, -1) {
		seen[strings.ToLower(match)] = true
	}
	return len(seen)
}

func questionOnly(text string, imperatives int) bool {
	return strings.HasSuffix(strings.TrimSpace(text), "?") &&
		utf8.RuneCountInString(text) <= 240 &&
		imperatives == 0 &&
		!strings.Contains(text, "```") &&
		!stackTracePattern.MatchString(text) &&
		!fileRefPattern.MatchString(text) &&
		!numberedPattern.MatchString(text)
}
