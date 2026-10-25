package session

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

// RequestState is the persisted state of a provider request. 'prepared' exists only in memory.
//
//	admitted ──► answered   (main: AppendAnswered; other purposes: SettleRequest)
//	admitted ──► unknown    (the request may have been sent and no answer was stored)
//	admitted ──► failed     (certainly never sent: Client.Complete was not called)
//
// answered, failed and unknown are terminal. unknown counts as possibly delivered in every exposure
// report; it is never rewritten to failed because a provider error does not prove non-delivery.
type RequestState string

const (
	RequestAdmitted RequestState = "admitted"
	RequestAnswered RequestState = "answered"
	RequestFailed   RequestState = "failed"
	RequestUnknown  RequestState = "unknown"
)

func (s RequestState) valid() bool {
	switch s {
	case RequestAdmitted, RequestAnswered, RequestFailed, RequestUnknown:
		return true
	}
	return false
}

// Reason codes the persistence layer itself writes.
const (
	ReasonFenced          = "fenced"
	ReasonFencedAnswer    = "fenced_before_answer"
	ReasonRestartNoAnswer = "restart_without_answer"
)

// RequestAdmission is what the caller knows before it sends a request: the identity of the request,
// the visible parts it is made of, and nothing of their text.
type RequestAdmission struct {
	RequestID string
	SessionID string
	// RunID is the in-memory run identity (runtime prepareRun); "" is stored as NULL.
	RunID   string
	Step    int
	Attempt int
	Purpose string
	// FirstSeq/LastSeq are the transcript window of a main request. PrevUserSeq/LastAssistantSeq are
	// the context an auto_effort judge saw before the input was stored.
	FirstSeq, LastSeq, PrevUserSeq, LastAssistantSeq *int64
	// PendingInputDigest is the digest of input not yet stored in the transcript (sha256:<hex>).
	PendingInputDigest string
	// Refs identify the rendered Task State items, system layers and tool data (strict ids, at most
	// MaxEnvelopeRefs of MaxEnvelopeRefBytes). A longer list is cut and the envelope marked
	// incomplete.
	Refs              []string
	ToolSpecsDigest   string
	ProjectionVersion int
	// LineageComplete is the caller's claim; the store only ever lowers it.
	LineageComplete bool
}

// RequestEnvelope is the stored row.
type RequestEnvelope struct {
	RequestID, SessionID, RunID                                   string
	Step, Attempt                                                 int
	Purpose                                                       string
	State                                                         RequestState
	FirstSeq, LastSeq, PrevUserSeq, LastAssistantSeq, AnsweredSeq *int64
	PendingInputDigest                                            string
	Refs                                                          []string
	ToolSpecsDigest                                               string
	ProjectionVersion                                             int
	LineageComplete                                               bool
	FenceEpoch                                                    int64
	// Decision is the bounded host decision of a non-main answered request, e.g. {"level":"high"}.
	Decision  json.RawMessage
	Reason    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (a RequestAdmission) validate() error {
	if err := checkID("request id", a.RequestID); err != nil {
		return err
	}
	if err := checkID("session id", a.SessionID); err != nil {
		return err
	}
	if err := checkOptionalID("run id", a.RunID); err != nil {
		return err
	}
	if !knownPurposes[a.Purpose] {
		return invalid("unknown purpose %q", a.Purpose)
	}
	if a.Step < 0 || a.Attempt < 0 || a.ProjectionVersion < 0 {
		return invalid("step, attempt and projection version must not be negative")
	}
	for name, v := range map[string]*int64{"first_seq": a.FirstSeq, "last_seq": a.LastSeq, "prev_user_seq": a.PrevUserSeq, "last_assistant_seq": a.LastAssistantSeq} {
		if v != nil && *v < 0 {
			return invalid("%s must not be negative", name)
		}
	}
	if (a.FirstSeq == nil) != (a.LastSeq == nil) {
		return invalid("first_seq and last_seq come together")
	}
	if a.FirstSeq != nil && *a.FirstSeq > *a.LastSeq {
		return invalid("first_seq %d is after last_seq %d", *a.FirstSeq, *a.LastSeq)
	}
	if a.Purpose == PurposeMain && a.FirstSeq == nil {
		return invalid("a main request records its transcript window")
	}
	if a.Purpose == PurposeAutoEffort && a.FirstSeq != nil {
		// DATA §6 CR-AJ-1: a judge's input is either one stored row or a pending digest, never both.
		if *a.FirstSeq != *a.LastSeq {
			return invalid("an auto_effort request names one stored input row, not %d..%d", *a.FirstSeq, *a.LastSeq)
		}
		if a.PendingInputDigest != "" {
			return invalid("an auto_effort request names its input by a stored row or a pending digest, not both")
		}
	}
	if err := checkDigest("pending input digest", a.PendingInputDigest); err != nil {
		return err
	}
	if err := checkDigest("tool specs digest", a.ToolSpecsDigest); err != nil {
		return err
	}
	for _, ref := range a.Refs {
		if !refRE.MatchString(ref) {
			return invalid("ref %q must match %s", ref, refRE)
		}
	}
	return nil
}

// AdmitRequest records that a provider request is about to be sent. In one write transaction it reads
// the session's fence and writes the envelope: 'admitted' when the session is not fenced, otherwise
// 'failed' with reason "fenced", and ErrFenced is returned with the failed envelope. The caller sends
// the request only when the error is nil and the returned state is admitted; once the call returned
// without error the request is durable, so a crash afterwards is recovered as unknown by Reconcile.
//
// The refs, digests and window are computed by the caller before the call: nothing inside the
// transaction touches the filesystem, git or the network.
func (s *Store) AdmitRequest(ctx context.Context, a RequestAdmission) (RequestEnvelope, error) {
	if err := a.validate(); err != nil {
		return RequestEnvelope{}, err
	}
	refs, complete := a.Refs, a.LineageComplete
	if len(refs) > MaxEnvelopeRefs {
		refs, complete = refs[:MaxEnvelopeRefs], false
	}
	if refs == nil {
		refs = []string{}
	}
	raw, err := json.Marshal(refs)
	if err != nil {
		return RequestEnvelope{}, err
	}
	if len(raw) > MaxRefsJSONBytes {
		// Unreachable with the strict ref charset; kept so the DDL bound is never the first check.
		return RequestEnvelope{}, invalid("refs exceed %d bytes", MaxRefsJSONBytes)
	}
	at := now()
	env := RequestEnvelope{
		RequestID: a.RequestID, SessionID: a.SessionID, RunID: a.RunID, Step: a.Step, Attempt: a.Attempt, Purpose: a.Purpose,
		State: RequestAdmitted, FirstSeq: a.FirstSeq, LastSeq: a.LastSeq, PrevUserSeq: a.PrevUserSeq, LastAssistantSeq: a.LastAssistantSeq,
		PendingInputDigest: a.PendingInputDigest, Refs: refs, ToolSpecsDigest: a.ToolSpecsDigest, ProjectionVersion: a.ProjectionVersion,
		LineageComplete: complete, CreatedAt: parseTime(at), UpdatedAt: parseTime(at),
	}
	err = s.writeTx(ctx, func(conn *sql.Conn) error {
		if err := sessionExists(ctx, conn, a.SessionID); err != nil {
			return err
		}
		var one int
		switch err := conn.QueryRowContext(ctx, `SELECT 1 FROM request_envelopes WHERE request_id = ?`, a.RequestID).Scan(&one); {
		case err == nil:
			return fmt.Errorf("%w: request %s", ErrExists, a.RequestID)
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		epoch, fenced, err := sessionFence(ctx, conn, a.SessionID)
		if err != nil {
			return err
		}
		env.FenceEpoch = epoch
		var reason any
		if fenced {
			env.State, env.Reason = RequestFailed, ReasonFenced
			reason = ReasonFenced
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO request_envelopes(
			request_id, session_id, run_id, step, attempt, purpose, state, first_seq, last_seq, prev_user_seq, last_assistant_seq,
			pending_input_digest, refs_json, tool_specs_digest, projection_version, lineage_complete, fence_epoch, reason, created_at, updated_at)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.RequestID, a.SessionID, nullString(a.RunID), a.Step, a.Attempt, a.Purpose, string(env.State),
			nullInt(a.FirstSeq), nullInt(a.LastSeq), nullInt(a.PrevUserSeq), nullInt(a.LastAssistantSeq),
			nullString(a.PendingInputDigest), string(raw), nullString(a.ToolSpecsDigest), a.ProjectionVersion, boolInt(complete), epoch,
			reason, at, at); err != nil {
			return err
		}
		if fenced {
			return commitThen{ErrFenced}
		}
		return nil
	})
	if err != nil && !errors.Is(err, ErrFenced) {
		return RequestEnvelope{}, err
	}
	return env, err
}

// AppendAnswered stores the assistant message of a main request and marks the request answered, in
// one transaction: either the visible response exists with answered_seq pointing at it, or neither
// does. It returns the message's seq.
//
// The request must belong to the session, have purpose main and still be admitted; any other state,
// terminal or not, is ErrInvalidTransition and nothing is written. If the session has been fenced since
// admission the message is NOT stored: the envelope becomes 'unknown' (reason fenced_before_answer),
// that is committed, and ErrFenced is returned. T3 may refine this branch; until then it fails closed.
func (s *Store) AppendAnswered(ctx context.Context, sessionID, requestID string, m protocol.Message) (int64, error) {
	if m.Role != "assistant" {
		return 0, invalid("AppendAnswered stores an assistant message, not %q", m.Role)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return 0, err
	}
	var seq int64
	err = s.writeTx(ctx, func(conn *sql.Conn) error {
		purpose, state, err := loadRequestState(ctx, conn, sessionID, requestID)
		if err != nil {
			return err
		}
		if purpose != PurposeMain {
			return invalid("request %s has purpose %q: only a main request is answered by a message", requestID, purpose)
		}
		if state != RequestAdmitted {
			return transitionError(state, RequestAnswered)
		}
		epoch, fenced, err := sessionFence(ctx, conn, sessionID)
		if err != nil {
			return err
		}
		if fenced {
			if err := settle(ctx, conn, requestID, RequestUnknown, ReasonFencedAnswer, nil, epoch); err != nil {
				return err
			}
			return commitThen{ErrFenced}
		}
		if seq, err = appendMessageRow(ctx, conn, sessionID, "assistant", raw); err != nil {
			return err
		}
		at := now()
		res, err := conn.ExecContext(ctx,
			`UPDATE request_envelopes SET state = 'answered', answered_seq = ?, updated_at = ? WHERE request_id = ? AND state = 'admitted'`,
			seq, at, requestID)
		if err != nil {
			return err
		}
		return requireOne(res, requestID)
	})
	if err != nil {
		return 0, err
	}
	return seq, nil
}

// Settlement moves an admitted request to its final state without a stored message.
type Settlement struct {
	To RequestState
	// Reason is a short machine code, required for failed and unknown.
	Reason string
	// Decision is the bounded result of an answered non-main request (for the Auto judge
	// {"level":"high"}): a flat JSON object of short strings, numbers and booleans, at most
	// MaxDecisionBytes. It is rejected for any other target state.
	Decision json.RawMessage
}

// SettleRequest ends an admitted request. The allowed moves are admitted -> unknown, admitted -> failed
// (only when Client.Complete certainly was not called) and, for a purpose other than main,
// admitted -> answered. Everything else is ErrInvalidTransition, in particular any move out of a
// terminal state and any move to answered of a main request, which AppendAnswered owns.
func (s *Store) SettleRequest(ctx context.Context, requestID string, st Settlement) error {
	if err := checkID("request id", requestID); err != nil {
		return err
	}
	if !st.To.valid() {
		return invalid("unknown target state %q", st.To)
	}
	switch st.To {
	case RequestFailed, RequestUnknown:
		if err := checkReason(st.Reason); err != nil {
			return err
		}
		if len(st.Decision) != 0 {
			return invalid("only an answered request carries a decision")
		}
	case RequestAnswered:
		if st.Reason != "" {
			return invalid("an answered request has no reason")
		}
		if err := checkDecision(st.Decision); err != nil {
			return err
		}
	}
	return s.writeTx(ctx, func(conn *sql.Conn) error {
		var sessionID, purpose string
		var state RequestState
		if err := conn.QueryRowContext(ctx, `SELECT session_id, purpose, state FROM request_envelopes WHERE request_id = ?`, requestID).Scan(&sessionID, &purpose, &state); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrRequestNotFound
			}
			return err
		}
		if state != RequestAdmitted || st.To == RequestAdmitted {
			return transitionError(state, st.To)
		}
		if st.To == RequestAnswered && purpose == PurposeMain {
			return fmt.Errorf("%w: a main request is answered by AppendAnswered", ErrInvalidTransition)
		}
		epoch, _, err := sessionFence(ctx, conn, sessionID)
		if err != nil {
			return err
		}
		return settle(ctx, conn, requestID, st.To, st.Reason, st.Decision, epoch)
	})
}

// settle performs admitted -> to. The WHERE clause is the last line of defence: a row that is no longer
// admitted matches nothing and the caller's transaction fails.
func settle(ctx context.Context, q querier, requestID string, to RequestState, reason string, decision json.RawMessage, epoch int64) error {
	res, err := q.ExecContext(ctx,
		`UPDATE request_envelopes SET state = ?, reason = ?, decision_json = ?, fence_epoch = MAX(fence_epoch, ?), updated_at = ?
		 WHERE request_id = ? AND state = 'admitted'`,
		string(to), nullString(reason), nullDecision(decision), epoch, now(), requestID)
	if err != nil {
		return err
	}
	return requireOne(res, requestID)
}

func nullDecision(d json.RawMessage) any {
	if len(d) == 0 {
		return nil
	}
	return string(d)
}

func requireOne(res sql.Result, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("%w: %s changed under the transaction", ErrInvalidTransition, id)
	}
	return nil
}

func transitionError(from, to RequestState) error {
	return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
}

// loadRequestState reads purpose and state of a request the session owns. A request of another
// session is reported as not found.
func loadRequestState(ctx context.Context, q rowQuerier, sessionID, requestID string) (purpose string, state RequestState, err error) {
	var owner string
	if err := q.QueryRowContext(ctx, `SELECT session_id, purpose, state FROM request_envelopes WHERE request_id = ?`, requestID).Scan(&owner, &purpose, &state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", ErrRequestNotFound
		}
		return "", "", err
	}
	if owner != sessionID {
		return "", "", ErrRequestNotFound
	}
	return purpose, state, nil
}

// checkDecision accepts nothing but a small flat object, so a decision cannot carry a body.
func checkDecision(d json.RawMessage) error {
	if len(d) == 0 {
		return nil
	}
	if len(d) > MaxDecisionBytes {
		return invalid("decision exceeds %d bytes", MaxDecisionBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(d))
	dec.UseNumber()
	var fields map[string]any
	if err := dec.Decode(&fields); err != nil || fields == nil || dec.More() {
		return invalid("decision must be one JSON object")
	}
	for k, v := range fields {
		if !idRE.MatchString(k) {
			return invalid("decision key %q must match %s", k, idRE)
		}
		switch x := v.(type) {
		case bool, json.Number:
		case string:
			if len(x) > 32 || !idRE.MatchString(x) {
				return invalid("decision value of %q must be a short identifier", k)
			}
		default:
			return invalid("decision value of %q must be a string, number or boolean", k)
		}
	}
	return nil
}

const requestColumns = `request_id, session_id, COALESCE(run_id,''), COALESCE(step,0), COALESCE(attempt,0), purpose, state,
	first_seq, last_seq, prev_user_seq, last_assistant_seq, COALESCE(pending_input_digest,''), refs_json, COALESCE(tool_specs_digest,''),
	projection_version, lineage_complete, fence_epoch, answered_seq, COALESCE(decision_json,''), COALESCE(reason,''), created_at, updated_at`

// Request returns one envelope, or ErrRequestNotFound.
func (s *Store) Request(ctx context.Context, requestID string) (RequestEnvelope, error) {
	var e RequestEnvelope
	var first, last, prev, lastAsst, answered sql.NullInt64
	var refs, decision, created, updated string
	var complete int
	err := s.db.QueryRowContext(ctx, `SELECT `+requestColumns+` FROM request_envelopes WHERE request_id = ?`, requestID).Scan(
		&e.RequestID, &e.SessionID, &e.RunID, &e.Step, &e.Attempt, &e.Purpose, &e.State,
		&first, &last, &prev, &lastAsst, &e.PendingInputDigest, &refs, &e.ToolSpecsDigest,
		&e.ProjectionVersion, &complete, &e.FenceEpoch, &answered, &decision, &e.Reason, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return RequestEnvelope{}, ErrRequestNotFound
	}
	if err != nil {
		return RequestEnvelope{}, err
	}
	e.FirstSeq, e.LastSeq, e.PrevUserSeq, e.LastAssistantSeq, e.AnsweredSeq = int64Ptr(first), int64Ptr(last), int64Ptr(prev), int64Ptr(lastAsst), int64Ptr(answered)
	if err := json.Unmarshal([]byte(refs), &e.Refs); err != nil {
		return RequestEnvelope{}, err
	}
	if decision != "" {
		e.Decision = json.RawMessage(decision)
	}
	e.LineageComplete = complete == 1
	e.CreatedAt, e.UpdatedAt = parseTime(created), parseTime(updated)
	return e, nil
}

// RequestsExposing returns, in admission order, the ids of the session's requests that may have
// carried the transcript row seq to a provider: a main request whose window covers it, an
// auto_effort judge that saw it as context or as its stored input, or a judge whose pending input
// had contentDigest (the "sha256:<hex>" of the row's text; "" skips that match). A failed request
// was never sent and exposes nothing. A digest that matches a duplicate text over-reports, which is
// the safe direction (DATA §6 CR-AJ-1).
func (s *Store) RequestsExposing(ctx context.Context, sessionID string, seq int64, contentDigest string) ([]string, error) {
	if contentDigest != "" {
		if err := checkDigest("content digest", contentDigest); err != nil {
			return nil, err
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT request_id FROM request_envelopes
		WHERE session_id = ? AND state <> ?
		  AND ((first_seq IS NOT NULL AND first_seq <= ? AND ? <= last_seq)
		    OR prev_user_seq = ? OR last_assistant_seq = ?
		    OR (? <> '' AND pending_input_digest = ?))
		ORDER BY created_at, request_id`,
		sessionID, string(RequestFailed), seq, seq, seq, seq, contentDigest, contentDigest)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func int64Ptr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}
