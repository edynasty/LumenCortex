package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// This file is the shared part of the request-provenance persistence layer (ADR-0018): errors, bounds,
// input validation, the one write-transaction shape and the fence read helpers. requests.go and
// executions.go hold the state machines.
//
// Transaction discipline (execution plan §3.1). Every write goes through writeTx: one pinned
// connection, BEGIN IMMEDIATE, and a callback that receives only that connection. The pool holds one
// connection (migrate.openDB), so a callback that called any other Store method, or s.db, would wait
// for the connection it is holding and never return. Nothing that needs the filesystem, git, the
// network or a model call may run inside a callback: callers compute digests, snapshots and refs first.
// A busy database longer than busy_timeout (5 s) is a storage failure the caller surfaces; nothing is
// retried here.
//
// No row written here carries message text, tool arguments, provider echo or hidden reasoning. Bodies
// are represented by ids, sequence ranges, epochs and sha256 digests; the validators below reject
// anything shaped like prose.

var (
	// ErrFenced reports that the session, or the source an edge would cite, is fenced: nothing new
	// derived from it may be admitted or stored. The writer that returns it has already committed
	// whatever body-free record the state machine needs (see AdmitRequest, AppendAnswered,
	// CommitToolResult).
	ErrFenced = errors.New("session or source is fenced")
	// ErrInvalidTransition reports a state change the machine does not allow, including any change
	// of a terminal state.
	ErrInvalidTransition = errors.New("invalid state transition")
	// ErrRequestNotFound and ErrExecutionNotFound report an id the session does not hold.
	ErrRequestNotFound   = errors.New("request envelope not found")
	ErrExecutionNotFound = errors.New("execution record not found")
	// ErrExists reports an id that is already recorded; ids are never reused.
	ErrExists = errors.New("record already exists")
	// ErrInvalid reports input the persistence layer refuses to store.
	ErrInvalid = errors.New("invalid provenance input")
)

// Bounds. The byte limits repeat the CHECKs of 0006 and TestBoundsMatchSchema keeps them equal.
const (
	// MaxEnvelopeRefs is how many refs one envelope keeps; a longer list is cut and the envelope is
	// stored with lineage_complete = 0 (plan T1-S2 step 1: over the bound means incomplete).
	MaxEnvelopeRefs = 128
	// MaxEnvelopeRefBytes bounds one envelope ref. 128 refs of 96 bytes plus JSON punctuation stay
	// under MaxRefsJSONBytes.
	MaxEnvelopeRefBytes = 96
	// MaxRefsJSONBytes is the length limit of request_envelopes.refs_json.
	MaxRefsJSONBytes = 16384
	// MaxEdgesPerCommit is how many derived edges one CommitToolResult stores; more are cut and every
	// stored edge and the execution row are marked lineage_complete = 0.
	MaxEdgesPerCommit = 64
	// MaxEdgeRefBytes is the length limit of an edge or execution ref column.
	MaxEdgeRefBytes = 512
	// MaxIDBytes is the length limit of request, run and execution ids.
	MaxIDBytes = 128
	// MaxReasonBytes is the length limit of request_envelopes.reason.
	MaxReasonBytes = 200
	// MaxDecisionBytes is the length limit of request_envelopes.decision_json.
	MaxDecisionBytes = 256
)

// Request purposes. The set is checked here and not by a CHECK so that a later purpose does not need a
// table rebuild.
const (
	PurposeMain       = "main"
	PurposeAutoEffort = "auto_effort"
	PurposeReview     = "review"
)

var knownPurposes = map[string]bool{PurposeMain: true, PurposeAutoEffort: true, PurposeReview: true}

// Origin classes (DATA_AND_FAILURES §3) plus the class of an execution row.
const (
	OriginUserDecision  = "user_decision"
	OriginHostCheck     = "host_check"
	OriginExternalData  = "external_data"
	OriginAgentGen      = "agent_generated"
	OriginToolExecution = "tool_execution"
)

var knownOriginClasses = map[string]bool{
	OriginUserDecision: true, OriginHostCheck: true, OriginExternalData: true, OriginAgentGen: true, OriginToolExecution: true,
}

// Source kinds of the cited side of an edge or an execution row.
const (
	SourceRequest = "request"
	SourceSession = "session"
	SourceFile    = "file"
	SourceCheck   = "check"
	// SourceMessage names an entry or row of the same session, such as a Task State entry ref.
	SourceMessage = "message"
)

var knownSourceKinds = map[string]bool{SourceRequest: true, SourceSession: true, SourceFile: true, SourceCheck: true, SourceMessage: true, "artifact": true, "execution": true}

var (
	idRE     = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	refRE    = regexp.MustCompile(`^[A-Za-z0-9._:/@#+=%~-]{1,96}$`)
	digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// reasonRE keeps a reason a short machine code. Provider error text can echo the request, so it
	// is never accepted.
	reasonRE = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

func checkID(field, v string) error {
	if !idRE.MatchString(v) {
		return invalid("%s must match %s", field, idRE)
	}
	return nil
}

func checkOptionalID(field, v string) error {
	if v == "" {
		return nil
	}
	return checkID(field, v)
}

func checkDigest(field, v string) error {
	if v != "" && !digestRE.MatchString(v) {
		return invalid("%s must be sha256:<64 hex>", field)
	}
	return nil
}

// checkLooseRef accepts a ref that may hold a workspace path: valid UTF-8 without control characters,
// at most MaxEdgeRefBytes.
func checkLooseRef(field, v string, required bool) error {
	if v == "" {
		if required {
			return invalid("%s is required", field)
		}
		return nil
	}
	if len(v) > MaxEdgeRefBytes || !utf8.ValidString(v) || strings.IndexFunc(v, unicode.IsControl) >= 0 {
		return invalid("%s must be at most %d bytes of printable UTF-8", field, MaxEdgeRefBytes)
	}
	return nil
}

func checkReason(v string) error {
	if !reasonRE.MatchString(v) || len(v) > MaxReasonBytes {
		return invalid("reason must be a short code matching %s", reasonRE)
	}
	return nil
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func nullInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// commitThen is returned by a writeTx callback that wants its writes committed and err returned to the
// caller. Fenced paths use it: the body-free terminal record must survive the refusal.
type commitThen struct{ err error }

func (c commitThen) Error() string { return c.err.Error() }
func (c commitThen) Unwrap() error { return c.err }

// writeTx runs fn inside one BEGIN IMMEDIATE transaction on a pinned connection. fn must use only the
// connection it is given. COMMIT runs without the caller's cancellation, as in BeginRun: once writeTx
// returns, whether the commit happened is known. A callback error rolls everything back, except a
// commitThen, which commits first.
func (s *Store) writeTx(ctx context.Context, fn func(conn *sql.Conn) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return writeTxOn(ctx, conn, fn)
}

// writeTxOn is writeTx on a connection the caller pinned.
func writeTxOn(ctx context.Context, conn *sql.Conn, fn func(conn *sql.Conn) error) error {
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
		}
	}()
	var after error
	if err := fn(conn); err != nil {
		var keep commitThen
		if !errors.As(err, &keep) {
			return err
		}
		after = keep.err
	}
	if _, err := conn.ExecContext(context.WithoutCancel(ctx), `COMMIT`); err != nil {
		return err
	}
	committed = true
	return after
}

// sessionExists returns ErrNotFound for a session the database does not hold.
func sessionExists(ctx context.Context, q rowQuerier, sessionID string) error {
	var one int
	if err := q.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, sessionID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// sessionFence is the source read of ADR-0018: the newest epoch of a fence on the session, and whether
// one exists. A fence is never lifted by this layer, whatever its cleanup state or projection: a
// cleaned or projected fence still records that the session was revoked, so nothing new may cite it.
// Callers hold the write transaction, so the answer is serialized with every fence write.
func sessionFence(ctx context.Context, q rowQuerier, sessionID string) (epoch int64, fenced bool, err error) {
	var newest sql.NullInt64
	if err := q.QueryRowContext(ctx,
		`SELECT MAX(epoch) FROM source_fences WHERE scope_kind = 'session' AND scope_ref = ?`, sessionID).Scan(&newest); err != nil {
		return 0, false, err
	}
	return newest.Int64, newest.Valid, nil
}

// consumerFence is the admission read of ADR-0018 for work of the session itself: the newest epoch of
// a fence on the session, and whether one of its fences is not yet projected. A fence a Start
// projected (AcknowledgeProjection, DATA_AND_FAILURES §5 step 3) no longer refuses the session's own
// requests, tools and commits: each run withholds the content the fence names. A newer fence refuses
// them again until a Start projects it. Same cost as sessionFence: one indexed read.
func consumerFence(ctx context.Context, q rowQuerier, sessionID string) (epoch int64, fenced bool, err error) {
	var newest sql.NullInt64
	var open int64
	if err := q.QueryRowContext(ctx,
		`SELECT MAX(epoch), COALESCE(MAX(json_extract(tombstone_json, '$.projected') IS NULL), 0)
		 FROM source_fences WHERE scope_kind = 'session' AND scope_ref = ?`, sessionID).Scan(&newest, &open); err != nil {
		return 0, false, err
	}
	return newest.Int64, open == 1, nil
}

// SourceScopeRef is the scope_ref a source-scoped fence uses for the source (kind, ref): the cited
// side of an edge. Edges are checked against it on commit.
func SourceScopeRef(kind, ref string) string { return kind + ":" + ref }

// sourceFenced reports whether the source an edge cites is fenced: a source-scoped fence on it, or,
// for a session source, a fence on that session. A revocation fences every session it reaches (T3-S1),
// so an edge that cites one of them and commits after it - read before, committed after - is refused.
func sourceFenced(ctx context.Context, q rowQuerier, kind, ref string) (bool, error) {
	if kind == SourceSession {
		if _, fenced, err := sessionFence(ctx, q, ref); err != nil || fenced {
			return fenced, err
		}
	}
	var one int
	err := q.QueryRowContext(ctx,
		`SELECT 1 FROM source_fences WHERE scope_kind = 'source' AND scope_ref = ? LIMIT 1`, SourceScopeRef(kind, ref)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
