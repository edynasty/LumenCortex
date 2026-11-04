package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
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
//
// The turn also carries what the judge request's envelope records: the seqs of the two transcript
// rows the judge saw, and the digest of input the transcript does not hold yet. They are read and
// computed here, before the admission transaction, so nothing inside it does I/O.
type autoTurn struct {
	newInput      string
	prevUser      string
	lastAssistant string
	inProgress    bool
	// prevUserSeq and lastAssistantSeq identify the rows above; nil when the turn has none.
	prevUserSeq, lastAssistantSeq *int64
	// pendingInputDigest is the digest of newInput when BeginRun has not stored it yet, and "" when
	// newInput is already a transcript row.
	pendingInputDigest string
	// inputSeq is the seq of newInput when it is already a transcript row (a fresh session's goal),
	// and nil otherwise. At most one of inputSeq and pendingInputDigest is set (DATA §6 CR-AJ-1).
	inputSeq *int64
}

// resolveAutoEffort resolves "auto" for one Start. It runs under the run lock before the Start
// transaction, so the transcript is stable and a cancelled judge call writes nothing to the session
// or its transcript. A turn that already resolved a level (a resumed run with no new input) reuses
// it instead of calling the judge again.
//
// A judge request is a model request: it is admitted before it is sent and its outcome is settled on
// its envelope (ADR-0018). Only the judge paths write, and only to request_envelopes: the session row
// and the transcript are written by BeginRun alone.
func (e *Engine) resolveAutoEffort(ctx context.Context, sessionID, runID, input string, auto *autoPlan) (autoResolution, error) {
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
		// The admission is the last write before the call and is committed before it: a request the
		// store did not admit is not sent (R-1). A fenced session gets its failed envelope from the
		// store, and ErrFenced ends the Start here, without a judge call.
		requestID, err := e.admitAutoJudge(ctx, sessionID, runID, turn)
		if err != nil {
			return autoResolution{}, err
		}
		classified, classifier, reason, judgeErr := agent.ClassifyWithJudge(ctx, judge, auto.judge.Efforts, auto.timeout,
			turn.newInput, turn.prevUser, turn.lastAssistant)
		if judgeErr != nil {
			// The request was admitted and may have been delivered: unknown, never failed. The
			// cancellation is the Start's cause; the envelope's residue is reconciled if this write
			// fails too.
			reason := agent.AutoFallbackError
			if ctx.Err() != nil {
				reason = agent.AutoReasonCancelled
			}
			if settleErr := e.settleAutoJudge(requestID, session.RequestUnknown, reason, nil); settleErr != nil {
				return autoResolution{}, judgeErr
			}
			return autoResolution{}, judgeErr
		}
		if reason == "" {
			// The judge answered: the envelope carries the level its reply was read as, the decision
			// log the level the run uses after the ceiling.
			if err := e.settleAutoJudge(requestID, session.RequestAnswered, "", agent.AutoJudgeDecision(classified)); err != nil {
				return autoResolution{}, err
			}
			level, source = classified, classifier
		} else {
			// Timeout, unparsable reply, provider error: the request may have reached the provider,
			// so it is unknown, and the deterministic heuristic decides.
			if err := e.settleAutoJudge(requestID, session.RequestUnknown, reason, nil); err != nil {
				return autoResolution{}, err
			}
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

// autoSettleTimeout bounds the write that settles a judge envelope. It runs on a fresh context, so a
// cancelled Start can still record how its request ended.
const autoSettleTimeout = 5 * time.Second

// newAutoRequestID is a 128-bit random request identity. It is drawn per judge call and never
// derived from the session or the run: an id is never reused, and a restarted Start cannot collide
// with an envelope it did not write.
func newAutoRequestID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("request id: %w", err)
	}
	return "rq-" + hex.EncodeToString(buf), nil
}

// inputDigest is the "sha256:<hex>" digest of input the transcript does not hold yet.
func inputDigest(input string) string {
	sum := sha256.Sum256([]byte(input))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// admitAutoJudge records the judge request before it is sent. The request id, the seqs and the
// digest are all computed before the call; the store's transaction only reads the session's fence
// and inserts the row. A session that has been fenced gets a failed envelope from the store and
// ErrFenced, and the caller must not send the request.
func (e *Engine) admitAutoJudge(ctx context.Context, sessionID, runID string, turn autoTurn) (string, error) {
	id, err := newAutoRequestID()
	if err != nil {
		return "", err
	}
	adm := session.RequestAdmission{
		RequestID: id, SessionID: sessionID, RunID: runID, Purpose: session.PurposeAutoEffort,
		PrevUserSeq: turn.prevUserSeq, LastAssistantSeq: turn.lastAssistantSeq,
		// The judge's input is named by exactly one pointer: the stored row as a one-row window, or
		// the digest of the input BeginRun has not stored yet. A turn without new input has neither.
		FirstSeq: turn.inputSeq, LastSeq: turn.inputSeq,
		PendingInputDigest: turn.pendingInputDigest,
		// The judge request has no Task State, system layer or tool: its context is the two seqs. An
		// input that no pointer names makes the envelope incomplete rather than claim a full lineage.
		LineageComplete: turn.newInput == "" || turn.inputSeq != nil || turn.pendingInputDigest != "",
	}
	if _, err := e.store.AdmitRequest(ctx, adm); err != nil {
		return "", err
	}
	return id, nil
}

// settleAutoJudge ends an admitted judge request. A settlement that cannot be written fails the
// Start: nothing else has been written yet, and the next Reconcile turns the still-admitted row into
// unknown.
func (e *Engine) settleAutoJudge(requestID string, to session.RequestState, reason string, decision json.RawMessage) error {
	ctx, cancel := context.WithTimeout(context.Background(), autoSettleTimeout)
	defer cancel()
	return e.store.SettleRequest(ctx, requestID, session.Settlement{To: to, Reason: reason, Decision: decision})
}

// classifyTurn reads the newest transcript rows and separates the turn's new input from its
// context. A Start with an input uses it as the new input. A Start without one either continues an
// answered turn (newInput empty) or, on a fresh session whose goal is already the last user row,
// treats that row as the new input.
//
// It also records the seqs of the rows the judge sees and, for an input BeginRun has not stored yet,
// its digest: the envelope's admission takes them as they are, so no digest is computed inside the
// store's transaction.
func classifyTurn(rows []session.Message, input string) (autoTurn, error) {
	type row struct {
		seq     int64
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
		messages = append(messages, row{seq: m.Seq, role: role, content: message.Content, origin: message.Origin})
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
	lastAssistant, lastAssistantIndex := "", -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].role == "assistant" && strings.TrimSpace(messages[i].content) != "" {
			lastAssistant, lastAssistantIndex = messages[i].content, i
			break
		}
	}
	turn := autoTurn{lastAssistant: lastAssistant}
	if lastAssistantIndex >= 0 {
		seq := messages[lastAssistantIndex].seq
		turn.lastAssistantSeq = &seq
	}
	if lastUser >= 0 {
		turn.prevUser = messages[lastUser].content
		seq := messages[lastUser].seq
		turn.prevUserSeq = &seq
	}
	if input != "" {
		// The Start stores this input in BeginRun, after the judge has read it.
		turn.newInput = input
		turn.pendingInputDigest = inputDigest(input)
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
	// A fresh session writes its goal as the first user row; it is this turn's input, already
	// stored, so there is no pending input and the envelope names that row.
	turn.newInput = messages[lastUser].content
	inputSeq := messages[lastUser].seq
	turn.inputSeq = &inputSeq
	turn.prevUser = ""
	turn.prevUserSeq = nil
	if previousUser >= 0 {
		turn.prevUser = messages[previousUser].content
		seq := messages[previousUser].seq
		turn.prevUserSeq = &seq
	}
	return turn, nil
}
