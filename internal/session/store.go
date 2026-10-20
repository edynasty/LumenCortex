package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/edynasty/LumenCortex/internal/migrate"
)

var ErrNotFound = errors.New("session not found")

type Store struct {
	db   *sql.DB
	ws   *migrate.Workspace
	path string
}

type Session struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Status    string    `json:"status"`
	Provider  string    `json:"provider,omitempty"`
	Model     string    `json:"model,omitempty"`
	// ModelRef is the catalog reference ("<provider>/<model>") the session is pinned to. Provider
	// and Model above are the provider id and upstream model id of the last Start.
	ModelRef string `json:"modelRef,omitempty"`
	// AgentID is the catalog agent the session runs (ADR-0009), "" for a session no Start has
	// pinned yet (created before migration 0005, or never started).
	AgentID  string          `json:"agentId,omitempty"`
	Goal     string          `json:"goal"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
	Final    *string         `json:"final,omitempty"`
	Usage    json.RawMessage `json:"usage,omitempty"`
	Error    json.RawMessage `json:"error,omitempty"`
}

type Message struct {
	SessionID string          `json:"sessionId"`
	Seq       int64           `json:"seq"`
	Role      string          `json:"role"`
	JSON      json.RawMessage `json:"json"`
}

type Step struct {
	SessionID string          `json:"sessionId"`
	Step      int64           `json:"step"`
	JSON      json.RawMessage `json:"json"`
}

// Open opens the workspace database under repositoryDir, running any pending schema migration first
// (internal/migrate). The Store holds <repositoryDir>/db.lock shared until Close.
func Open(repositoryDir string) (*Store, error) {
	return OpenContext(context.Background(), repositoryDir)
}

// OpenContext is Open with a context that bounds the wait for db.lock and cancels a migration in
// progress. It returns migrate.ErrSchemaTooNew for a database a newer binary migrated, and
// migrate.ErrDatabaseInUse when other processes keep the database open too long for a migration.
func OpenContext(ctx context.Context, repositoryDir string) (*Store, error) {
	if err := os.MkdirAll(repositoryDir, 0o755); err != nil {
		return nil, err
	}
	set, err := migrate.Embedded()
	if err != nil {
		return nil, err
	}
	ws, err := migrate.Open(ctx, repositoryDir, set)
	if err != nil {
		return nil, err
	}
	return &Store{db: ws.DB, ws: ws, path: filepath.Join(repositoryDir, "lumencortex.db")}, nil
}

func (s *Store) Path() string { return s.path }

// Close closes the database and releases the shared schema lock.
func (s *Store) Close() error { return s.ws.Close() }

func normalizeJSON(raw json.RawMessage, fallback string) string {
	if len(raw) == 0 {
		return fallback
	}
	return string(raw)
}

func (s *Store) Create(ctx context.Context, v Session) error {
	return insertSession(ctx, s.db, v)
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func insertSession(ctx context.Context, db execer, v Session) error {
	if v.CreatedAt.IsZero() {
		v.CreatedAt = time.Now().UTC()
	}
	if v.UpdatedAt.IsZero() {
		v.UpdatedAt = v.CreatedAt
	}
	if v.Status == "" {
		v.Status = "running"
	}
	_, err := db.ExecContext(ctx, `
INSERT INTO sessions(id, created_at, updated_at, status, provider, model, model_ref, agent_id, goal, metadata_json, final, usage_json, error_json)
VALUES(?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''))
`, v.ID, v.CreatedAt.Format(time.RFC3339Nano), v.UpdatedAt.Format(time.RFC3339Nano), v.Status,
		v.Provider, v.Model, v.ModelRef, v.AgentID, v.Goal, normalizeJSON(v.Metadata, "{}"), v.Final,
		normalizeJSON(v.Usage, ""), normalizeJSON(v.Error, ""))
	return err
}

func (s *Store) Get(ctx context.Context, id string) (Session, error) {
	return getSession(ctx, s.db, id)
}

// rowQuerier is the part of *sql.DB and *sql.Conn that a single-row read needs, so
// the same read can run inside a transaction.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getSession(ctx context.Context, q rowQuerier, id string) (Session, error) {
	var v Session
	var created, updated string
	var provider, model, modelRef, agentID sql.NullString
	var metadata string
	var final, usage, errJSON sql.NullString
	err := q.QueryRowContext(ctx, `
SELECT id, created_at, updated_at, status, provider, model, model_ref, agent_id, goal, metadata_json, final, usage_json, error_json
FROM sessions WHERE id = ?
`, id).Scan(&v.ID, &created, &updated, &v.Status, &provider, &model, &modelRef, &agentID, &v.Goal, &metadata, &final, &usage, &errJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	v.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	v.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	v.Provider = provider.String
	v.Model = model.String
	v.ModelRef = modelRef.String
	v.AgentID = agentID.String
	v.Metadata = json.RawMessage(metadata)
	if final.Valid {
		v.Final = &final.String
	}
	if usage.Valid {
		v.Usage = json.RawMessage(usage.String)
	}
	if errJSON.Valid {
		v.Error = json.RawMessage(errJSON.String)
	}
	return v, nil
}

func (s *Store) List(ctx context.Context, limit, offset int) ([]Session, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id FROM sessions ORDER BY updated_at DESC LIMIT ? OFFSET ?
`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(ids))
	for _, id := range ids {
		v, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Store) AppendMessage(ctx context.Context, sessionID, role string, payload any) (int64, error) {
	seq, _, err := s.AppendMessageTaskState(ctx, sessionID, role, payload, nil)
	return seq, err
}

func (s *Store) MessagesBefore(ctx context.Context, sessionID string, beforeSeq int64, limit int) ([]Message, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	query := `
SELECT session_id, seq, role, json FROM session_messages
WHERE session_id = ?`
	args := []any{sessionID}
	if beforeSeq >= 0 {
		query += " AND seq < ?"
		args = append(args, beforeSeq)
	}
	query += " ORDER BY seq DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Message, 0, limit)
	for rows.Next() {
		var m Message
		var raw string
		if err := rows.Scan(&m.SessionID, &m.Seq, &m.Role, &raw); err != nil {
			return nil, err
		}
		m.JSON = json.RawMessage(raw)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// RecentMessages returns the newest limit rows, oldest first.
func (s *Store) RecentMessages(ctx context.Context, sessionID string, limit int) ([]Message, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT session_id, seq, role, json FROM session_messages
WHERE session_id = ? ORDER BY seq DESC LIMIT ?
`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Message, 0, limit)
	for rows.Next() {
		var m Message
		var raw string
		if err := rows.Scan(&m.SessionID, &m.Seq, &m.Role, &raw); err != nil {
			return nil, err
		}
		m.JSON = json.RawMessage(raw)
		out = append(out, m)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// NewestMessageSeq is the seq of the session's newest transcript row, -1 when it has none.
func (s *Store) NewestMessageSeq(ctx context.Context, sessionID string) (int64, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), -1) FROM session_messages WHERE session_id = ?`, sessionID).Scan(&seq)
	return seq, err
}

// EachMessageFrom calls fn with every row whose seq is at least fromSeq, oldest first. The request
// window reads the transcript from the Task State's windowStartSeq this way, one row at a time.
func (s *Store) EachMessageFrom(ctx context.Context, sessionID string, fromSeq int64, fn func(Message) error) error {
	rows, err := s.db.QueryContext(ctx, `
SELECT session_id, seq, role, json FROM session_messages
WHERE session_id = ? AND seq >= ? ORDER BY seq
`, sessionID, fromSeq)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var m Message
		var raw []byte
		if err := rows.Scan(&m.SessionID, &m.Seq, &m.Role, &raw); err != nil {
			return err
		}
		m.JSON = raw
		if err := fn(m); err != nil {
			return err
		}
	}
	return rows.Err()
}

// LatestStep returns the newest step record's JSON for a session, nil when it has none.
func (s *Store) LatestStep(ctx context.Context, sessionID string) (json.RawMessage, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `
SELECT json FROM agent_steps WHERE session_id = ? ORDER BY step DESC LIMIT 1
`, sessionID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

func (s *Store) AppendStep(ctx context.Context, sessionID string, step int64, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO agent_steps(session_id, step, json) VALUES(?, ?, ?)
ON CONFLICT(session_id, step) DO UPDATE SET json = excluded.json
`, sessionID, step, string(raw))
	return err
}

func (s *Store) Journal(ctx context.Context, event string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO journal(at, event, payload_json) VALUES(?, ?, ?)`, time.Now().UTC().Format(time.RFC3339Nano), event, string(raw))
	return err
}
