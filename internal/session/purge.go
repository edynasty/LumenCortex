package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/edynasty/LumenCortex/internal/migrate"
	"github.com/edynasty/LumenCortex/protocol"
)

// Privacy deletion (DATA_AND_FAILURES §6 N7, execution plan T3-S4). Deleting a session removes every
// local copy of its content that this database controls and keeps only metadata without content: the
// fences, a tombstone with ids and the epoch, the edges of other sessions that cite it (now
// source_missing), their request envelopes and the body-free journal and checkpoint rows.
//
// It runs as a state machine on a 'privacy' fence row, so a process killed between batches resumes the
// cleanup instead of using the content again:
//
//  1. BeginPrivacyDeletion commits, in one write transaction, the revocation of the session (every
//     session that read it is fenced, as RevokeSession does) and one privacy row per deleted id in
//     state cleanup_pending.
//  2. PurgeStep runs short write transactions with secure_delete on, each one bounded batch: the
//     deleted sessions' rows table by table; then the copies in other sessions' transcripts (tool
//     results and task results that name a deleted id), rewritten without content; the checkpoints
//     of parents that name a deleted child; the journal rows still holding content (rows written
//     before the journal whitelist); then the session rows. The root's privacy row then owes a
//     rebuild: the next step runs VACUUM (older copies left in unallocated space by earlier page
//     splits are only gone after it). It then owes the WAL truncation ($.settle): until it
//     completes, the WAL can still hold page images of the removed rows and the main file the rows
//     the rebuild replaced, so the step truncates the WAL, and only a complete truncation turns the
//     fences of the deleted ids cleaned. A reader that keeps the WAL from being truncated leaves the
//     deletion cleanup_pending (ErrCleanupPending); the next step retries. The fences stay after
//     cleaned: an edge committed later that cites a deleted id is refused, and the edges of other
//     sessions that cite it resolve to source_missing. An explicit re-import of the source
//     supersedes a finished deletion (ImportSessions).
//  3. The caller owes the checkpoint marker before the first batch (OweCheckpoint), so an Open after
//     a crash also retries the truncation.

const (
	// ReasonPrivacyDelete is the reason of the fences a privacy deletion commits.
	ReasonPrivacyDelete = "privacy_delete"
	// PurgeBatchRows bounds the rows one PurgeStep deletes or rewrites, so a step is a short write
	// transaction.
	PurgeBatchRows = 200
	// purgeScanRows bounds the transcript rows one step searches for copies.
	purgeScanRows = 2000
	// RemovedContent replaces the content of a tool result in another session that carried content of
	// a deleted session.
	RemovedContent = "[removed: this result carried content of a deleted thread]"
	// journalCursorKey is the metadata key of the newest journal seq the purge has projected: every
	// row up to it holds only whitelisted fields (a row written since the whitelist always does).
	journalCursorKey = "journal_projected_through"
)

// purgeTables are the tables holding rows of a deleted session, by session_id; sessions is last.
// jobs and scheduled_tasks hold commands and prompts (content); a job's output files are removed by
// the job manager once their row is gone. Notebooks are the agent's working memory (ADR-0020): their
// bodies and revisions are content and are deleted with the session; notebook_revisions is keyed by
// notebook, so it is purged before notebooks through purgeWhere.
var purgeTables = []string{"session_messages", "agent_steps", "session_checkpoints", "task_states", "task_state_inbox",
	"artifacts", "eval_results", "request_envelopes", "derived_origins", "jobs", "scheduled_tasks",
	"notebook_revisions", "notebooks", "runtime_events", "runtime_event_sequences", "runtime_event_retention", "worker_messages",
	"steer_messages", "run_pauses", "work_unit_leases", "hub_commands"}

// purgeWhere is the WHERE clause selecting the rows of the sessions named by the id list `in` for one
// table. Every table but notebook_revisions holds session_id; a revision is reached through the
// notebook it belongs to.
func purgeWhere(table, in string) string {
	if table == "worker_messages" {
		return "EXISTS (SELECT 1 FROM sessions victim WHERE victim.id IN (" + in + ") AND (victim.id=worker_messages.session_id OR victim.id=worker_messages.sender_id OR victim.id=worker_messages.recipient_id))"
	}
	if table == "notebook_revisions" {
		return "notebook_id IN (SELECT id FROM notebooks WHERE session_id IN (" + in + "))"
	}
	if table == "work_unit_leases" {
		// A claim names a unit of the root's DAG; a deleted holder's claim is left to expire rather than
		// matched here, so this clause keeps one placeholder set like every other table.
		return "root_id IN (" + in + ")"
	}
	return "session_id IN (" + in + ")"
}

// PrivacyDeletion is one deletion in progress: the root session the user deleted, every id it deletes
// (the root first, then the temporary descendants deleted with it) and the epoch of its fences.
type PrivacyDeletion struct {
	Root  string   `json:"root"`
	IDs   []string `json:"ids"`
	Epoch int64    `json:"epoch"`
}

// BeginPrivacyDeletion commits the fences of a deletion of ids (root first; each must exist): the
// revocation of the root, which fences it, its descendants and every session that read one of them,
// and a privacy row per id in state cleanup_pending, the root's carrying the deletion. A deletion of
// the same root that is still pending is returned as it is, to resume.
func (s *Store) BeginPrivacyDeletion(ctx context.Context, ids []string) (PrivacyDeletion, Revocation, error) {
	if len(ids) == 0 {
		return PrivacyDeletion{}, Revocation{}, ErrNotFound
	}
	for _, id := range ids {
		if err := checkID("session id", id); err != nil {
			return PrivacyDeletion{}, Revocation{}, err
		}
	}
	var d PrivacyDeletion
	var rev Revocation
	err := s.writeTx(ctx, func(conn *sql.Conn) error {
		pending, err := pendingDeletion(ctx, conn, ids[0])
		if err != nil || pending != nil {
			if pending != nil {
				d = *pending
			}
			return err
		}
		for _, id := range ids {
			if err := sessionExists(ctx, conn, id); err != nil {
				return err
			}
		}
		if rev, err = revokeIn(ctx, conn, ids[0], ReasonPrivacyDelete); err != nil {
			return err
		}
		d = PrivacyDeletion{Root: ids[0], IDs: ids, Epoch: rev.Epoch}
		at := now()
		pointer, err := json.Marshal(map[string]any{"revocation": ids[0], "epoch": rev.Epoch})
		if err != nil {
			return err
		}
		root, err := json.Marshal(d)
		if err != nil {
			return err
		}
		for i, id := range ids {
			// A descendant the walk did not reach (none should be) is fenced too.
			if _, fenced, err := sessionFence(ctx, conn, id); err != nil {
				return err
			} else if !fenced {
				if _, err := conn.ExecContext(ctx, `INSERT INTO source_fences(scope_kind, scope_ref, epoch, reason, state, tombstone_json, created_at, updated_at)
					VALUES('session', ?, ?, ?, 'active', ?, ?, ?)`, id, rev.Epoch, ReasonPrivacyDelete, string(pointer), at, at); err != nil {
					return err
				}
			}
			tombstone := `{"root":` + strconv.Quote(ids[0]) + `,"epoch":` + strconv.FormatInt(rev.Epoch, 10) + `}`
			if i == 0 {
				tombstone = string(root)
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO source_fences(scope_kind, scope_ref, epoch, reason, state, tombstone_json, created_at, updated_at)
				VALUES('privacy', ?, ?, ?, 'cleanup_pending', ?, ?, ?)`, id, rev.Epoch, ReasonPrivacyDelete, tombstone, at, at); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return PrivacyDeletion{}, Revocation{}, err
	}
	return d, rev, nil
}

// PendingPrivacyDeletion returns the deletion of root whose cleanup has not finished, nil when there is
// none.
func (s *Store) PendingPrivacyDeletion(ctx context.Context, root string) (*PrivacyDeletion, error) {
	return pendingDeletion(ctx, s.db, root)
}

// pendingDeletion returns the pending deletion whose root is id, nil when there is none.
func pendingDeletion(ctx context.Context, q rowQuerier, id string) (*PrivacyDeletion, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT tombstone_json FROM source_fences
		WHERE scope_kind = 'privacy' AND scope_ref = ? AND state = 'cleanup_pending' AND json_type(tombstone_json, '$.ids') = 'array'
		ORDER BY epoch DESC LIMIT 1`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var d PrivacyDeletion
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// PendingPrivacyDeletions returns the deletions whose cleanup has not finished.
func (s *Store) PendingPrivacyDeletions(ctx context.Context) ([]PrivacyDeletion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tombstone_json FROM source_fences
		WHERE scope_kind = 'privacy' AND state = 'cleanup_pending' AND json_type(tombstone_json, '$.ids') = 'array' ORDER BY epoch`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PrivacyDeletion
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var d PrivacyDeletion
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ErrCleanupPending is returned by PurgeStep while the WAL truncation a privacy deletion owes cannot
// complete (a reader of another connection keeps an older snapshot, or a schema rebuild is owed
// first): the content may still be at rest in the WAL or the main file, so the deletion is not
// finished and its fences stay cleanup_pending. A later step retries.
var ErrCleanupPending = errors.New("privacy deletion not finished: the WAL could not be truncated yet (another reader holds the database)")

// OweCheckpoint records durably, before the first batch of a deletion, that the WAL must be truncated
// once the content is gone: until then it can hold page images of the removed rows.
func (s *Store) OweCheckpoint() error { return migrate.OweCheckpoint(filepath.Dir(s.path)) }

// PurgeStep runs one bounded batch of the deletion d in one write transaction with secure_delete on,
// so the cells it frees are zeroed and not merely unlinked. done is true when the deletion is
// finished: the session rows are gone, the file is rebuilt, the WAL is truncated and the fences of
// the deleted ids are cleaned. Each step reads where the cleanup stands from the database, so a step
// that was interrupted is simply run again.
//
// secure_delete alone is not enough: an earlier page split or defragmentation (with secure_delete
// off, as every ordinary write runs) can have left older copies of a deleted row in unallocated space
// of pages the deletion never touches. So once the rows are gone the root's privacy row owes a
// rebuild ($.rebuild), and the next step runs VACUUM on its own connection, outside any transaction
// it opened. VACUUM costs one copy of the database and holds the write lock while it runs; a writer
// of another process waits for it or fails busy, and an error keeps the deletion pending for the next
// attempt. Readers of other processes are not blocked (WAL). The rebuilt pages go through the WAL, so
// the row then owes the truncation ($.settle): wal_checkpoint(TRUNCATE), bounded by
// migrate.CheckpointWait. It fails with ErrCleanupPending while a reader blocks it, and only a
// complete truncation marks the fences cleaned.
func (s *Store) PurgeStep(ctx context.Context, d PrivacyDeletion) (done bool, err error) {
	if len(d.IDs) == 0 || d.IDs[0] != d.Root {
		return false, invalid("privacy deletion without its root")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA secure_delete = ON`); err != nil {
		return false, err
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA secure_delete = OFF`)
	in, args := idList(d.IDs)
	rebuild, settle := false, false
	err = writeTxOn(ctx, conn, func(conn *sql.Conn) error {
		var state, raw string
		err := conn.QueryRowContext(ctx, `SELECT state, tombstone_json FROM source_fences
			WHERE scope_kind = 'privacy' AND scope_ref = ? AND epoch = ?`, d.Root, d.Epoch).Scan(&state, &raw)
		if err != nil {
			return err
		}
		if state != "cleanup_pending" {
			done = true
			return nil
		}
		var cursor struct {
			Scan    int64 `json:"scan"`
			Rebuild bool  `json:"rebuild"`
			Settle  bool  `json:"settle"`
		}
		if err := json.Unmarshal([]byte(raw), &cursor); err != nil {
			return err
		}
		if cursor.Rebuild || cursor.Settle {
			rebuild, settle = cursor.Rebuild, cursor.Settle
			return nil
		}
		budget := int64(PurgeBatchRows)
		for _, table := range purgeTables {
			res, err := conn.ExecContext(ctx, `DELETE FROM `+table+` WHERE rowid IN
				(SELECT rowid FROM `+table+` WHERE `+purgeWhere(table, in)+` LIMIT ?)`, append(args, budget)...)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if budget -= n; budget == 0 {
				return nil
			}
		}
		if budget < PurgeBatchRows {
			return nil
		}
		var newest sql.NullInt64
		if err := conn.QueryRowContext(ctx, `SELECT MAX(rowid) FROM session_messages`).Scan(&newest); err != nil {
			return err
		}
		if cursor.Scan < newest.Int64 {
			through := cursor.Scan + purgeScanRows
			if err := removeCopies(ctx, conn, d.IDs, cursor.Scan, through); err != nil {
				return err
			}
			_, err := conn.ExecContext(ctx, `UPDATE source_fences SET tombstone_json = json_set(tombstone_json, '$.scan', ?), updated_at = ?
				WHERE scope_kind = 'privacy' AND scope_ref = ? AND epoch = ?`, through, now(), d.Root, d.Epoch)
			return err
		}
		if err := projectParentCheckpoints(ctx, conn, d.IDs); err != nil {
			return err
		}
		if more, err := projectJournal(ctx, conn); err != nil || more {
			return err
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM sessions WHERE id IN (`+in+`)`, args...); err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `UPDATE source_fences SET tombstone_json = json_set(json_remove(tombstone_json, '$.scan'), '$.rebuild', json('true')), updated_at = ?
			WHERE scope_kind = 'privacy' AND scope_ref = ? AND epoch = ?`, now(), d.Root, d.Epoch)
		return err
	})
	if err != nil || !(rebuild || settle) {
		return done, err
	}
	if rebuild {
		if _, err := conn.ExecContext(ctx, `VACUUM`); err != nil {
			return false, fmt.Errorf("rebuild database after a privacy deletion: %w", err)
		}
		err = writeTxOn(ctx, conn, func(conn *sql.Conn) error {
			_, err := conn.ExecContext(ctx, `UPDATE source_fences SET tombstone_json = json_set(json_remove(tombstone_json, '$.rebuild'), '$.settle', json('true')), updated_at = ?
				WHERE scope_kind = 'privacy' AND scope_ref = ? AND epoch = ? AND state = 'cleanup_pending'`, now(), d.Root, d.Epoch)
			return err
		})
		if err != nil {
			return false, err
		}
	}
	complete, err := migrate.SettleCheckpoint(ctx, conn, filepath.Dir(s.path))
	if err != nil {
		return false, fmt.Errorf("truncate the WAL after a privacy deletion: %w", err)
	}
	if !complete {
		return false, ErrCleanupPending
	}
	// Written after the truncation: these pages hold the fences, not the removed content.
	err = writeTxOn(ctx, conn, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `UPDATE source_fences SET state = 'cleaned', tombstone_json = json_remove(tombstone_json, '$.scan', '$.rebuild', '$.settle'), updated_at = ?
			WHERE scope_kind IN ('privacy','session') AND scope_ref IN (`+in+`) AND state = 'cleanup_pending'`, append([]any{now()}, args...)...)
		return err
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// idList returns "?,?,..." and the ids as arguments.
func idList(ids []string) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}

// copyFilter selects the rows of other sessions that carry content of a deleted session: tool results
// and task results that name a deleted id. Every exit that returns content of a session names it
// (read_thread, recall, spawn/wait/list_subagents, notebook_list/notebook_read, a task result), with
// or without an edge: rows written before request provenance have none. A row the user or a model
// authored is not a copy and is kept.
func copyFilter(ids []string) (string, []any) {
	in, args := idList(ids)
	match := strings.TrimSuffix(strings.Repeat("instr(json, ?) > 0 OR ", len(ids)), " OR ")
	return `session_id NOT IN (` + in + `) AND (role = 'tool' OR (role = 'user' AND json_extract(json, '$.origin') = 'task_result'))
		AND (` + match + `)`, append(args, args...)
}

// removeCopies rewrites the copies among the transcript rows with rowid in (after, through]: a tool
// result keeps its call id and gets RemovedContent; a task result keeps its header and child id and is
// marked withdrawn, so the edge of its delivery still names the row (projection). Rewriting twice
// writes nothing.
func removeCopies(ctx context.Context, conn *sql.Conn, ids []string, after, through int64) error {
	filter, args := copyFilter(ids)
	rows, err := conn.QueryContext(ctx, `SELECT rowid, json FROM session_messages WHERE rowid > ? AND rowid <= ? AND `+filter,
		append([]any{after, through}, args...)...)
	if err != nil {
		return err
	}
	type rewrite struct {
		rowid int64
		json  string
	}
	var rewrites []rewrite
	for rows.Next() {
		var rowid int64
		var raw string
		if err := rows.Scan(&rowid, &raw); err != nil {
			rows.Close()
			return err
		}
		next, err := withoutContent(raw)
		if err != nil {
			rows.Close()
			return err
		}
		if next != raw {
			rewrites = append(rewrites, rewrite{rowid, next})
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, r := range rewrites {
		if _, err := conn.ExecContext(ctx, `UPDATE session_messages SET json = ? WHERE rowid = ?`, r.json, r.rowid); err != nil {
			return err
		}
	}
	return nil
}

// withoutContent is the row raw with its content removed; see removeCopies.
func withoutContent(raw string) (string, error) {
	var m protocol.Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return "", fmt.Errorf("transcript row: %w", err)
	}
	out := protocol.Message{Role: m.Role, Name: m.Name, ToolCallID: m.ToolCallID, Origin: m.Origin, Content: RemovedContent}
	if m.Origin == protocol.OriginTaskResult {
		body, err := json.Marshal(map[string]any{"sessionId": deliveredChild(raw), "withdrawn": true})
		if err != nil {
			return "", err
		}
		out.Content = protocol.TaskResultHeader + "\n" + string(body)
	}
	next, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	if m.Content == out.Content && len(m.ToolCalls) == 0 {
		return raw, nil
	}
	return string(next), nil
}

// projectParentCheckpoints reduces the checkpoints of other sessions that name a deleted child to the
// journal whitelist: a checkpoint written before T1-S6 carried the child's goal and final.
func projectParentCheckpoints(ctx context.Context, conn *sql.Conn, ids []string) error {
	in, args := idList(ids)
	rows, err := conn.QueryContext(ctx, `SELECT session_id, seq, json FROM session_checkpoints
		WHERE session_id NOT IN (`+in+`) AND json_extract(json, '$.childSessionId') IN (`+in+`)`, append(args, args...)...)
	if err != nil {
		return err
	}
	type rewrite struct {
		session string
		seq     int64
		json    string
	}
	var rewrites []rewrite
	for rows.Next() {
		var r rewrite
		var raw string
		if err := rows.Scan(&r.session, &r.seq, &raw); err != nil {
			rows.Close()
			return err
		}
		projected, err := projectJournalPayload([]byte(raw))
		if err != nil {
			rows.Close()
			return err
		}
		if string(projected) != raw {
			r.json = string(projected)
			rewrites = append(rewrites, r)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, r := range rewrites {
		if _, err := conn.ExecContext(ctx, `UPDATE session_checkpoints SET json = ? WHERE session_id = ? AND seq = ?`, r.json, r.session, r.seq); err != nil {
			return err
		}
	}
	return nil
}

// projectJournal reduces the next batch of journal rows past the cursor to the whitelist and advances
// the cursor. more is true while rows remain. Rows written before the whitelist can hold any content,
// including shell commands without a session id, so every such row is projected, not only those that
// name a deleted session (owner decision O-3, recommended default).
func projectJournal(ctx context.Context, conn *sql.Conn) (more bool, err error) {
	var cursor int64
	var raw string
	err = conn.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key = ?`, journalCursorKey).Scan(&raw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return false, err
	default:
		if cursor, err = strconv.ParseInt(raw, 10, 64); err != nil {
			return false, fmt.Errorf("%s: %w", journalCursorKey, err)
		}
	}
	rows, err := conn.QueryContext(ctx, `SELECT seq, payload_json FROM journal WHERE seq > ? ORDER BY seq LIMIT ?`, cursor, PurgeBatchRows)
	if err != nil {
		return false, err
	}
	type rewrite struct {
		seq     int64
		payload string
	}
	var rewrites []rewrite
	last := cursor
	for rows.Next() {
		var seq int64
		var payload string
		if err := rows.Scan(&seq, &payload); err != nil {
			rows.Close()
			return false, err
		}
		last = seq
		projected, err := projectJournalPayload([]byte(payload))
		if err != nil {
			rows.Close()
			return false, err
		}
		if string(projected) != payload {
			rewrites = append(rewrites, rewrite{seq, string(projected)})
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return false, err
	}
	if last == cursor {
		return false, nil
	}
	for _, r := range rewrites {
		if _, err := conn.ExecContext(ctx, `UPDATE journal SET payload_json = ? WHERE seq = ?`, r.payload, r.seq); err != nil {
			return false, err
		}
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO metadata(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		journalCursorKey, strconv.FormatInt(last, 10))
	return true, err
}

// DeletionPreview is what a deletion of a session would remove and what it cannot reach, counted
// before anything is written. Nothing in it is content.
type DeletionPreview struct {
	// Rows counts the rows of the deleted sessions, by table.
	Rows map[string]int64 `json:"rows"`
	// Readers are the other sessions the deletion fences (they read a deleted session); Expanded when
	// the walk passed MaxRevokeSessions and every session would be fenced.
	Readers     []string `json:"readers"`
	ReaderCount int      `json:"readerCount"`
	Expanded    bool     `json:"expanded,omitempty"`
	// Copies counts the rows of other sessions whose content is removed (tool and task results that
	// carry a deleted session); ParentCheckpoints the checkpoints of parents that name a deleted child.
	Copies            int64 `json:"copies"`
	ParentCheckpoints int64 `json:"parentCheckpoints"`
	// JournalRows counts the journal rows not yet reduced to the whitelist.
	JournalRows int64 `json:"journalRows"`
}

// DeletionPreview counts, in one read transaction, what deleting ids (root first) would do.
func (s *Store) DeletionPreview(ctx context.Context, ids []string) (DeletionPreview, error) {
	if len(ids) == 0 {
		return DeletionPreview{}, ErrNotFound
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return DeletionPreview{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN`); err != nil {
		return DeletionPreview{}, err
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
	if err := sessionExists(ctx, conn, ids[0]); err != nil {
		return DeletionPreview{}, err
	}
	p := DeletionPreview{Rows: map[string]int64{}}
	in, args := idList(ids)
	for _, table := range append(purgeTables, "sessions") {
		where := purgeWhere(table, in)
		if table == "sessions" {
			where = "id IN (" + in + ")"
		}
		var n int64
		if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE `+where, args...).Scan(&n); err != nil {
			return DeletionPreview{}, err
		}
		p.Rows[table] = n
	}
	deleted := map[string]bool{}
	for _, id := range ids {
		deleted[id] = true
	}
	reached, expanded, err := revocationReach(ctx, conn, ids[0])
	if err != nil {
		return DeletionPreview{}, err
	}
	p.Expanded = expanded
	p.Readers = []string{}
	for _, id := range reached {
		if !deleted[id] {
			p.ReaderCount++
			p.Readers = appendCapped(p.Readers, id)
		}
	}
	filter, fargs := copyFilter(ids)
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM session_messages WHERE `+filter, fargs...).Scan(&p.Copies); err != nil {
		return DeletionPreview{}, err
	}
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM session_checkpoints
		WHERE session_id NOT IN (`+in+`) AND json_extract(json, '$.childSessionId') IN (`+in+`)`, append(args, args...)...).Scan(&p.ParentCheckpoints); err != nil {
		return DeletionPreview{}, err
	}
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM journal
		WHERE seq > COALESCE((SELECT CAST(value AS INTEGER) FROM metadata WHERE key = ?), 0)`, journalCursorKey).Scan(&p.JournalRows); err != nil {
		return DeletionPreview{}, err
	}
	return p, nil
}
