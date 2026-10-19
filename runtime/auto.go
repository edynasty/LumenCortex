package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// autoResolution is one Start's auto effort decision: the level the run uses as its default, the
// ceiling the per-candidate rule applies, and the decision the first step record carries.
type autoResolution struct {
	level    config.Effort
	ceiling  config.Effort
	decision agent.Decision
}

// autoTurn is the classifier's view of the transcript at this Start: the turn's new input, the
// previous substantive user request, and the last assistant reply. inProgress reports that the last
// substantive user row is already answered, so the Start continues that turn.
type autoTurn struct {
	newInput      string
	prevUser      string
	lastAssistant string
	inProgress    bool
}

// resolveAutoEffort resolves "auto" for one Start. It runs under the run lock before the Start
// transaction, so the transcript is stable and a cancelled judge call writes nothing. A turn that
// already resolved a level (a resumed run with no new input) reuses it instead of calling the judge
// again.
func (e *Engine) resolveAutoEffort(ctx context.Context, sessionID, input string, auto *autoPlan) (autoResolution, error) {
	rows, err := e.store.RecentMessages(ctx, sessionID, 500)
	if err != nil {
		return autoResolution{}, err
	}
	turn, err := classifyTurn(rows, input)
	if err != nil {
		return autoResolution{}, err
	}
	if turn.newInput == "" && turn.inProgress {
		raw, err := e.store.LatestStep(ctx, sessionID)
		if err != nil {
			return autoResolution{}, err
		}
		if len(raw) > 0 {
			if prior, ok := agent.AutoDecisionOfStep(raw); ok {
				if level, decision, ok := agent.ReusedAutoDecision(prior, auto.max, 0); ok {
					return autoResolution{level: level, ceiling: auto.max, decision: decision}, nil
				}
			}
		}
	}

	var judge protocol.Provider
	fallback := agent.AutoFallbackNoModel
	if auto.judge.Ref != "" {
		if client, buildErr := e.providerFactory(auto.judge); buildErr == nil {
			judge = client
			fallback = ""
		}
	}
	level, source := config.Effort(""), ""
	if judge != nil {
		classified, classifier, reason, judgeErr := agent.ClassifyWithJudge(ctx, judge, auto.judge.Efforts, auto.timeout,
			turn.newInput, turn.prevUser, turn.lastAssistant)
		if judgeErr != nil {
			return autoResolution{}, judgeErr
		}
		if reason == "" {
			level, source = classified, classifier
		} else {
			fallback = reason
		}
	}
	if source == "" {
		level, source = agent.HeuristicEffort(turn.newInput, turn.prevUser), agent.AutoSourceHeuristic
	}
	level = agent.CappedAutoEffort(level, auto.max)
	return autoResolution{
		level:   level,
		ceiling: auto.max,
		decision: agent.AutoDecision(source, fallback, utf8.RuneCountInString(turn.newInput),
			auto.max, auto.judge.Ref, level, false),
	}, nil
}

// classifyTurn reads the newest transcript rows and separates the turn's new input from its
// context. A Start with an input uses it as the new input. A Start without one either continues an
// answered turn (newInput empty) or, on a fresh session whose goal is already the last user row,
// treats that row as the new input.
func classifyTurn(rows []session.Message, input string) (autoTurn, error) {
	type row struct {
		role    string
		content string
		origin  string
	}
	messages := make([]row, 0, len(rows))
	for _, m := range rows {
		var message protocol.Message
		if err := json.Unmarshal(m.JSON, &message); err != nil {
			return autoTurn{}, err
		}
		role := message.Role
		if role == "" {
			role = m.Role
		}
		messages = append(messages, row{role: role, content: message.Content, origin: message.Origin})
	}
	lastUser, previousUser := -1, -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].role != "user" || messages[i].origin != "" {
			continue
		}
		if lastUser < 0 {
			lastUser = i
			continue
		}
		previousUser = i
		break
	}
	lastAssistant := ""
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].role == "assistant" && strings.TrimSpace(messages[i].content) != "" {
			lastAssistant = messages[i].content
			break
		}
	}
	turn := autoTurn{lastAssistant: lastAssistant}
	if lastUser >= 0 {
		turn.prevUser = messages[lastUser].content
	}
	if input != "" {
		turn.newInput = input
		return turn, nil
	}
	if lastUser < 0 {
		return turn, nil
	}
	for i := lastUser + 1; i < len(messages); i++ {
		if messages[i].role == "assistant" {
			turn.inProgress = true
			break
		}
	}
	if turn.inProgress {
		// The last user row is the request being continued; there is no new message.
		turn.newInput = ""
		turn.prevUser = messages[lastUser].content
		return turn, nil
	}
	// A fresh session writes its goal as the first user row; it is this turn's input.
	turn.newInput = messages[lastUser].content
	turn.prevUser = ""
	if previousUser >= 0 {
		turn.prevUser = messages[previousUser].content
	}
	return turn, nil
}
