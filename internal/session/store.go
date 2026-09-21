package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const schemaVersion = 1

var ErrNotFound = errors.New("session not found")

type Store struct {
	db   *sql.DB
	path string
}

type Session struct {
	ID        string          `json:"id"`
	CreatedAt time.Time       `json:"createdAt"`
	UpdatedAt time.Time       `json:"updatedAt"`
	Status    string          `json:"status"`
	Provider  string          `json:"provider,omitempty"`
	Model     string          `json:"model,omitempty"`
	Goal      string          `json:"goal"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	Final     *string         `json:"final,omitempty"`
	Usage     json.RawMessage `json:"usage,omitempty"`
	Error     json.RawMessage `json:"error,omitempty"`
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

func Open(repositoryDir string) (*Store, error) {
	if err := os.MkdirAll(repositoryDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(repositoryDir, "lumencortex.db")
	dsn := fmt.Sprintf("file:%s?_busy_timeout=5000&_foreign_keys=on&_journal_mode=WAL&_synchronous=NORMAL", filepath.ToSlash(path))
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	// One connection keeps SQLite memory bounded and avoids avoidable writer contention.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, path: path}
	if err := store.ensureSchema(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Path() string { return s.path }
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) ensureSchema(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;

CREATE TABLE IF NOT EXISTS metadata (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS sessions (
  id TEXT PRIMARY KEY,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  status TEXT NOT NULL,
  provider TEXT,
  model TEXT,
  goal TEXT NOT NULL,
  metadata_json TEXT NOT NULL,
  final TEXT,
  usage_json TEXT,
  error_json TEXT
) STRICT;

CREATE INDEX IF NOT EXISTS idx_sessions_updated ON sessions(updated_at DESC);

CREATE TABLE IF NOT EXISTS session_messages (
  session_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  role TEXT NOT NULL,
  json TEXT NOT NULL,
  PRIMARY KEY(session_id, seq),
  FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
) STRICT;

CREATE TABLE IF NOT EXISTS agent_steps (
  session_id TEXT NOT NULL,
  step INTEGER NOT NULL,
  json TEXT NOT NULL,
  PRIMARY KEY(session_id, step),
  FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
) STRICT;

CREATE TABLE IF NOT EXISTS journal (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  at TEXT NOT NULL,
  event TEXT NOT NULL,
  payload_json TEXT NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS idx_journal_event_at ON journal(event, at DESC);
`)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO metadata(key, value) VALUES('schema_version', ?)
ON CONFLICT(key) DO NOTHING
`, fmt.Sprintf("%d", schemaVersion))
	return err
}

func normalizeJSON(raw json.RawMessage, fallback string) string {
	if len(raw) == 0 {
		return fallback
	}
	return string(raw)
}

func (s *Store) Create(ctx context.Context, v Session) error {
	if v.CreatedAt.IsZero() {
		v.CreatedAt = time.Now().UTC()
	}
	if v.UpdatedAt.IsZero() {
		v.UpdatedAt = v.CreatedAt
	}
	if v.Status == "" {
		v.Status = "running"
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO sessions(id, created_at, updated_at, status, provider, model, goal, metadata_json, final, usage_json, error_json)
VALUES(?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''))
`, v.ID, v.CreatedAt.Format(time.RFC3339Nano), v.UpdatedAt.Format(time.RFC3339Nano), v.Status,
		v.Provider, v.Model, v.Goal, normalizeJSON(v.Metadata, "{}"), v.Final,
		normalizeJSON(v.Usage, ""), normalizeJSON(v.Error, ""))
	return err
}

func (s *Store) Get(ctx context.Context, id string) (Session, error) {
	var v Session
	var created, updated string
	var provider, model sql.NullString
	var metadata string
	var final, usage, errJSON sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT id, created_at, updated_at, status, provider, model, goal, metadata_json, final, usage_json, error_json
FROM sessions WHERE id = ?
`, id).Scan(&v.ID, &created, &updated, &v.Status, &provider, &model, &v.Goal, &metadata, &final, &usage, &errJSON)
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
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), -1) + 1 FROM session_messages WHERE session_id = ?`, sessionID).Scan(&seq); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_messages(session_id, seq, role, json) VALUES(?, ?, ?, ?)`, sessionID, seq, role, string(raw)); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339Nano), sessionID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return seq, nil
}

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
