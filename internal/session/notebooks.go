package session

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/internal/notebook"
)

// Limits of the agent's working notebook (ADR-0020). They are constants rather than configuration: a
// notebook is external working memory, and an unbounded one would be a second database inside the
// session. A body that reaches the limit is refused, never truncated.
const (
	// MaxNotebookBytes bounds one notebook body.
	MaxNotebookBytes = 256 << 10
	// MaxNotebookTitleChars bounds a notebook title in characters (a title is one line).
	MaxNotebookTitleChars = 120
	// MaxNotebooksPerSession bounds the notebooks one session may hold.
	MaxNotebooksPerSession = 32
	// MaxNotebookRevisions is how many bodies of each notebook are kept; the oldest are pruned in the
	// same transaction as the write that pushes the notebook past the limit.
	MaxNotebookRevisions = 50
)

var (
	// ErrNotebookNotFound means the session holds no notebook with that id or title.
	ErrNotebookNotFound = errors.New("notebook not found")
	// ErrNotebookVersionConflict means a write expected a version the notebook no longer has.
	ErrNotebookVersionConflict = errors.New("notebook version conflict")
)

// NotebookConflict is a lost compare-and-set: the notebook is at CurrentVersion, and nothing was
// written. The caller re-reads and retries with that version.
type NotebookConflict struct {
	CurrentVersion int64
}

func (c *NotebookConflict) Error() string {
	return fmt.Sprintf("%s: the notebook is at version %d", ErrNotebookVersionConflict, c.CurrentVersion)
}

func (c *NotebookConflict) Unwrap() error { return ErrNotebookVersionConflict }

// NotebookInfo is what a list of notebooks returns: no body, but the section headings a reader needs
// to decide which notebook and which section to open.
type NotebookInfo struct {
	ID        string    `json:"id"`
	SessionID string    `json:"sessionId"`
	Title     string    `json:"title"`
	Version   int64     `json:"version"`
	Bytes     int       `json:"bytes"`
	UpdatedAt time.Time `json:"updatedAt"`
	UpdatedBy string    `json:"updatedBy"`
	Sections  []string  `json:"sections"`
}

// NotebookDoc is one notebook with its Markdown body.
type NotebookDoc struct {
	NotebookInfo
	Body string `json:"body"`
}

// NotebookRevision is one kept version of a notebook. Body is empty in a list.
type NotebookRevision struct {
	NotebookID string    `json:"notebookId"`
	Version    int64     `json:"version"`
	Op         string    `json:"op"`
	UpdatedBy  string    `json:"updatedBy"`
	Bytes      int       `json:"bytes"`
	CreatedAt  time.Time `json:"createdAt"`
	Body       string    `json:"body,omitempty"`
}

// notebookOps are the ways a notebook version comes to be, as stored in notebook_revisions.op.
var notebookOps = map[string]bool{"create": true, "replace": true, "append": true, "section": true}

// newNotebookID is a 128-bit random notebook identity.
func newNotebookID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("notebook id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// checkNotebookTitle validates a title and returns it trimmed. A title is one printable line: it is
// shown in the context index and used to name a notebook, so a control character or a newline would
// break both.
func checkNotebookTitle(title string) (string, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return "", invalid("notebook title is required")
	}
	if utf8.RuneCountInString(title) > MaxNotebookTitleChars {
		return "", invalid("notebook title must be at most %d characters", MaxNotebookTitleChars)
	}
	if !utf8.ValidString(title) || strings.IndexFunc(title, unicode.IsControl) >= 0 {
		return "", invalid("notebook title must be one printable line")
	}
	return title, nil
}

// checkNotebookBody validates a body and returns it with CRLF read as LF (the shape internal/notebook
// reads and writes).
func checkNotebookBody(body string) (string, error) {
	if !utf8.ValidString(body) {
		return "", invalid("notebook body must be valid UTF-8")
	}
	if len(body) > MaxNotebookBytes {
		return "", invalid("notebook body is %d bytes, more than the %d byte limit", len(body), MaxNotebookBytes)
	}
	return body, nil
}

// checkNotebookAuthor validates updated_by: "host" or "agent:<run id>".
func checkNotebookAuthor(author string) error {
	if author == "" || len(author) > 80 || !utf8.ValidString(author) || strings.IndexFunc(author, unicode.IsControl) >= 0 {
		return invalid("notebook author must be 1..80 printable bytes")
	}
	return nil
}

// CreateNotebook writes a notebook of the session with body as version 1. It refuses a session at the
// notebook limit, a title the session already uses and a body past the limit.
func (s *Store) CreateNotebook(ctx context.Context, sessionID, title, body, author string) (NotebookDoc, error) {
	if err := checkID("session id", sessionID); err != nil {
		return NotebookDoc{}, err
	}
	title, err := checkNotebookTitle(title)
	if err != nil {
		return NotebookDoc{}, err
	}
	if body, err = checkNotebookBody(body); err != nil {
		return NotebookDoc{}, err
	}
	if err := checkNotebookAuthor(author); err != nil {
		return NotebookDoc{}, err
	}
	id, err := newNotebookID()
	if err != nil {
		return NotebookDoc{}, err
	}
	now := time.Now().UTC()
	var doc NotebookDoc
	err = s.writeTx(ctx, func(conn *sql.Conn) error {
		if err := sessionExists(ctx, conn, sessionID); err != nil {
			return err
		}
		var count int
		if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM notebooks WHERE session_id = ?`, sessionID).Scan(&count); err != nil {
			return err
		}
		if count >= MaxNotebooksPerSession {
			return invalid("session already holds %d notebooks, the limit; update one of them instead of adding another", count)
		}
		var exists int
		err := conn.QueryRowContext(ctx, `SELECT 1 FROM notebooks WHERE session_id = ? AND title = ?`, sessionID, title).Scan(&exists)
		switch {
		case err == nil:
			return invalid("session already holds a notebook titled %q; read and update it, or choose another title", title)
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		stamp := now.Format(time.RFC3339Nano)
		bytes, headings, err := notebookDerived(body)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `
INSERT INTO notebooks(id, session_id, title, body, bytes, headings_json, version, created_at, updated_at, updated_by)
VALUES(?, ?, ?, ?, ?, ?, 1, ?, ?, ?)
`, id, sessionID, title, body, bytes, headings, stamp, stamp, author); err != nil {
			return err
		}
		if err := insertNotebookRevision(ctx, conn, id, 1, body, "create", author, stamp); err != nil {
			return err
		}
		doc = NotebookDoc{
			NotebookInfo: NotebookInfo{
				ID: id, SessionID: sessionID, Title: title, Version: 1, Bytes: bytes,
				UpdatedAt: now, UpdatedBy: author, Sections: notebook.Headings(body),
			},
			Body: body,
		}
		return nil
	})
	if err != nil {
		return NotebookDoc{}, err
	}
	return doc, nil
}

// ListNotebooks returns the metadata of the session's notebooks, newest first. No body is read: the
// byte count and the section headings are derived from it at every write and stored beside it.
func (s *Store) ListNotebooks(ctx context.Context, sessionID string) ([]NotebookInfo, error) {
	if err := checkID("session id", sessionID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, session_id, title, bytes, headings_json, version, updated_at, updated_by
FROM notebooks WHERE session_id = ? ORDER BY updated_at DESC, title
`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]NotebookInfo, 0, 4)
	for rows.Next() {
		var info NotebookInfo
		var headings, updated string
		if err := rows.Scan(&info.ID, &info.SessionID, &info.Title, &info.Bytes, &headings, &info.Version, &updated, &info.UpdatedBy); err != nil {
			return nil, err
		}
		info.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		if err := json.Unmarshal([]byte(headings), &info.Sections); err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, rows.Err()
}

// GetNotebook returns one notebook of the session by id or, failing that, by title.
func (s *Store) GetNotebook(ctx context.Context, sessionID, ref string) (NotebookDoc, error) {
	if err := checkID("session id", sessionID); err != nil {
		return NotebookDoc{}, err
	}
	doc, err := getNotebook(ctx, s.db, sessionID, ref)
	if err != nil {
		return NotebookDoc{}, err
	}
	return doc, nil
}

// getNotebook reads one notebook inside a read or write transaction. ref is an id first and a title
// second, so a model may name a notebook either way.
func getNotebook(ctx context.Context, q rowQuerier, sessionID, ref string) (NotebookDoc, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return NotebookDoc{}, invalid("notebook is required")
	}
	const selectRow = `SELECT id, session_id, title, body, bytes, headings_json, version, updated_at, updated_by
		FROM notebooks WHERE session_id = ? AND `
	for _, where := range []string{"id = ?", "title = ?"} {
		doc, err := scanNotebook(q.QueryRowContext(ctx, selectRow+where, sessionID, ref))
		if err == nil {
			return doc, nil
		}
		if !errors.Is(err, ErrNotebookNotFound) {
			return NotebookDoc{}, err
		}
	}
	return NotebookDoc{}, ErrNotebookNotFound
}

// notebookRef resolves a notebook of the session by id, then by title, without reading its body: the
// revision list, a revision read and a delete need only its metadata.
func notebookRef(ctx context.Context, q rowQuerier, sessionID, ref string) (NotebookInfo, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return NotebookInfo{}, invalid("notebook is required")
	}
	const selectRow = `SELECT id, session_id, title, bytes, headings_json, version, updated_at, updated_by
		FROM notebooks WHERE session_id = ? AND `
	for _, where := range []string{"id = ?", "title = ?"} {
		var info NotebookInfo
		var headings, updated string
		err := q.QueryRowContext(ctx, selectRow+where, sessionID, ref).
			Scan(&info.ID, &info.SessionID, &info.Title, &info.Bytes, &headings, &info.Version, &updated, &info.UpdatedBy)
		switch {
		case err == nil:
			info.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
			if err := json.Unmarshal([]byte(headings), &info.Sections); err != nil {
				return NotebookInfo{}, err
			}
			return info, nil
		case errors.Is(err, sql.ErrNoRows):
			continue
		default:
			return NotebookInfo{}, err
		}
	}
	return NotebookInfo{}, ErrNotebookNotFound
}

func scanNotebook(row *sql.Row) (NotebookDoc, error) {
	var doc NotebookDoc
	var headings, updated string
	err := row.Scan(&doc.ID, &doc.SessionID, &doc.Title, &doc.Body, &doc.Bytes, &headings, &doc.Version, &updated, &doc.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return NotebookDoc{}, ErrNotebookNotFound
	}
	if err != nil {
		return NotebookDoc{}, err
	}
	doc.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	if err := json.Unmarshal([]byte(headings), &doc.Sections); err != nil {
		return NotebookDoc{}, err
	}
	return doc, nil
}

// UpdateNotebook writes the next version of one notebook of the session. mutate receives the body at
// the version the write applies to and returns the new body; both run inside the same write
// transaction, so an append or a section write is atomic with the version it extends. expected >= 0
// refuses the write with *NotebookConflict when the notebook is at another version, writing nothing;
// expected < 0 applies to whatever version is current. op names the revision (replace, append or
// section).
func (s *Store) UpdateNotebook(ctx context.Context, sessionID, ref, author, op string, expected int64, mutate func(current string) (string, error)) (NotebookDoc, error) {
	if err := checkID("session id", sessionID); err != nil {
		return NotebookDoc{}, err
	}
	if !notebookOps[op] || op == "create" {
		return NotebookDoc{}, invalid("notebook update op must be replace, append or section")
	}
	if err := checkNotebookAuthor(author); err != nil {
		return NotebookDoc{}, err
	}
	if mutate == nil {
		return NotebookDoc{}, invalid("notebook update needs a mutation")
	}
	now := time.Now().UTC()
	var doc NotebookDoc
	err := s.writeTx(ctx, func(conn *sql.Conn) error {
		current, err := getNotebook(ctx, conn, sessionID, ref)
		if err != nil {
			return err
		}
		if expected >= 0 && expected != current.Version {
			return &NotebookConflict{CurrentVersion: current.Version}
		}
		next, err := mutate(current.Body)
		if err != nil {
			return err
		}
		if next, err = checkNotebookBody(next); err != nil {
			return err
		}
		version := current.Version + 1
		stamp := now.Format(time.RFC3339Nano)
		bytes, headings, err := notebookDerived(next)
		if err != nil {
			return err
		}
		res, err := conn.ExecContext(ctx, `
UPDATE notebooks SET body = ?, bytes = ?, headings_json = ?, version = ?, updated_at = ?, updated_by = ?
WHERE id = ? AND version = ?
`, next, bytes, headings, version, stamp, author, current.ID, current.Version)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			// The read above is serialized with every writer by the write transaction, so this is a
			// corrupted row rather than a race; report it as a conflict with what the row says now.
			return &NotebookConflict{CurrentVersion: current.Version}
		}
		if err := insertNotebookRevision(ctx, conn, current.ID, version, next, op, author, stamp); err != nil {
			return err
		}
		if err := pruneNotebookRevisions(ctx, conn, current.ID); err != nil {
			return err
		}
		doc = NotebookDoc{
			NotebookInfo: NotebookInfo{
				ID: current.ID, SessionID: sessionID, Title: current.Title, Version: version, Bytes: bytes,
				UpdatedAt: now, UpdatedBy: author, Sections: notebook.Headings(next),
			},
			Body: next,
		}
		return nil
	})
	if err != nil {
		return NotebookDoc{}, err
	}
	return doc, nil
}

// DeleteNotebook deletes one notebook of the session and every revision of it, and returns the
// metadata of what was deleted (its body is not read).
func (s *Store) DeleteNotebook(ctx context.Context, sessionID, ref string) (NotebookInfo, error) {
	if err := checkID("session id", sessionID); err != nil {
		return NotebookInfo{}, err
	}
	var deleted NotebookInfo
	err := s.writeTx(ctx, func(conn *sql.Conn) error {
		current, err := notebookRef(ctx, conn, sessionID, ref)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM notebook_revisions WHERE notebook_id = ?`, current.ID); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM notebooks WHERE id = ?`, current.ID); err != nil {
			return err
		}
		deleted = current
		return nil
	})
	if err != nil {
		return NotebookInfo{}, err
	}
	return deleted, nil
}

// NotebookRevisions lists the kept revisions of one notebook, newest first, without their bodies.
func (s *Store) NotebookRevisions(ctx context.Context, sessionID, ref string) ([]NotebookRevision, error) {
	if err := checkID("session id", sessionID); err != nil {
		return nil, err
	}
	current, err := notebookRef(ctx, s.db, sessionID, ref)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT version, op, updated_by, length(body), created_at FROM notebook_revisions
WHERE notebook_id = ? ORDER BY version DESC
`, current.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]NotebookRevision, 0, MaxNotebookRevisions)
	for rows.Next() {
		revision := NotebookRevision{NotebookID: current.ID}
		var created string
		if err := rows.Scan(&revision.Version, &revision.Op, &revision.UpdatedBy, &revision.Bytes, &created); err != nil {
			return nil, err
		}
		revision.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, revision)
	}
	return out, rows.Err()
}

// NotebookRevision returns one kept revision of a notebook, with its body.
func (s *Store) NotebookRevision(ctx context.Context, sessionID, ref string, version int64) (NotebookRevision, error) {
	if err := checkID("session id", sessionID); err != nil {
		return NotebookRevision{}, err
	}
	current, err := notebookRef(ctx, s.db, sessionID, ref)
	if err != nil {
		return NotebookRevision{}, err
	}
	revision := NotebookRevision{NotebookID: current.ID}
	var created string
	err = s.db.QueryRowContext(ctx, `
SELECT version, op, updated_by, length(body), created_at, body FROM notebook_revisions
WHERE notebook_id = ? AND version = ?
`, current.ID, version).Scan(&revision.Version, &revision.Op, &revision.UpdatedBy, &revision.Bytes, &created, &revision.Body)
	if errors.Is(err, sql.ErrNoRows) {
		return NotebookRevision{}, invalid("notebook %q keeps no revision %d (the newest %d are kept)", current.Title, version, MaxNotebookRevisions)
	}
	if err != nil {
		return NotebookRevision{}, err
	}
	revision.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return revision, nil
}

// notebookDerived returns what a list and the run-start index read beside a body: its byte count and
// its section headings as JSON, so neither has to load the body.
func notebookDerived(body string) (int, string, error) {
	raw, err := json.Marshal(notebook.Headings(body))
	if err != nil {
		return 0, "", err
	}
	return len(body), string(raw), nil
}

// insertNotebookRevision records one version of a notebook.
func insertNotebookRevision(ctx context.Context, conn *sql.Conn, notebookID string, version int64, body, op, author, at string) error {
	_, err := conn.ExecContext(ctx, `
INSERT INTO notebook_revisions(notebook_id, version, body, op, updated_by, created_at) VALUES(?, ?, ?, ?, ?, ?)
`, notebookID, version, body, op, author, at)
	return err
}

// pruneNotebookRevisions deletes the revisions older than the newest MaxNotebookRevisions.
func pruneNotebookRevisions(ctx context.Context, conn *sql.Conn, notebookID string) error {
	_, err := conn.ExecContext(ctx, `
DELETE FROM notebook_revisions WHERE notebook_id = ? AND version <=
  (SELECT MAX(version) FROM notebook_revisions WHERE notebook_id = ?) - ?
`, notebookID, notebookID, MaxNotebookRevisions)
	return err
}
