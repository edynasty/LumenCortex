package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// reasoningPredicate selects transcript rows that store provider reasoning. 0001_v1.sql and the
// backup scrub use it verbatim in their UPDATE (TestScrubStatementMatches0001 compares them); the
// runner uses it to decide whether the file must be rebuilt.
const reasoningPredicate = `json_valid(json)
  AND (json_type(json, '$.reasoning') IS NOT NULL OR json_type(json, '$.reasoning_content') IS NOT NULL)`

// scrubStatement removes provider reasoning from stored transcript rows. It is the same statement
// 0001_v1.sql runs on the live database, so the backup kept for rollback is not a second store of
// hidden reasoning (I5).
const scrubStatement = `UPDATE session_messages
SET json = json_remove(json, '$.reasoning', '$.reasoning_content')
WHERE ` + reasoningPredicate + `;`

// testHookAfterVACUUM, when non-nil, runs after VACUUM INTO and before the scrub; tests inject a
// failure, or stop the process, to check that no durable backup is left behind.
var testHookAfterVACUUM func(partial string) error

// backup writes a durable copy of the live database before a schema change, under the exclusive
// db.lock: VACUUM INTO <stateDir>/backups/lumencortex.db.v<version>.<UTC ts>.partial, scrub provider
// reasoning from the copy, then rename to the final name. Leftover *.partial files are interrupted
// attempts that may still hold reasoning, so they are deleted first. On any error the .partial is
// deleted and nothing is renamed: a durable backup never contains provider reasoning. A database
// without a sessions table has nothing to back up.
func backup(ctx context.Context, conn *sql.Conn, stateDir string, version int) error {
	backupDir := filepath.Join(stateDir, "backups")
	if err := removePartials(backupDir); err != nil {
		return err
	}
	var hasSessions int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='sessions'`).Scan(&hasSessions); err != nil {
		return err
	}
	if hasSessions == 0 {
		return nil
	}
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return err
	}
	final, partial := backupNames(backupDir, version)
	if _, err := conn.ExecContext(ctx, `VACUUM INTO '`+strings.ReplaceAll(partial, "'", "''")+`'`); err != nil {
		_ = os.Remove(partial)
		return fmt.Errorf("backup before schema change: %w", err)
	}
	if err := finishBackup(ctx, partial, final); err != nil {
		_ = os.Remove(partial)
		return fmt.Errorf("backup before schema change: %w", err)
	}
	return syncDir(backupDir)
}

func finishBackup(ctx context.Context, partial, final string) error {
	if err := os.Chmod(partial, 0o600); err != nil {
		return err
	}
	if testHookAfterVACUUM != nil {
		if err := testHookAfterVACUUM(partial); err != nil {
			return err
		}
	}
	if err := scrubReasoning(ctx, partial); err != nil {
		return err
	}
	return os.Rename(partial, final)
}

// backupNames picks a final name that no earlier backup uses, so two changes within one second never
// overwrite each other.
func backupNames(dir string, version int) (final, partial string) {
	base := fmt.Sprintf("lumencortex.db.v%d.%s", version, time.Now().UTC().Format("20060102T150405Z"))
	name := base
	for n := 1; ; n++ {
		_, errFinal := os.Stat(filepath.Join(dir, name))
		_, errPartial := os.Stat(filepath.Join(dir, name+".partial"))
		if os.IsNotExist(errFinal) && os.IsNotExist(errPartial) {
			return filepath.Join(dir, name), filepath.Join(dir, name+".partial")
		}
		name = fmt.Sprintf("%s.%d", base, n)
	}
}

// removePartials deletes the *.partial files of interrupted backup attempts.
func removePartials(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".partial") {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

// scrubReasoning opens the .partial backup in rollback-journal mode and removes provider reasoning
// from its transcript rows. The rollback journal keeps old page images out of a WAL file, and
// secure_delete zeroes the freed bytes, so the reasoning is gone from the file and not merely from the
// row. Tables the copy does not use are foreign and stay unchanged.
func scrubReasoning(ctx context.Context, path string) error {
	b, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(path)+"?_journal_mode=DELETE&_synchronous=FULL")
	if err != nil {
		return err
	}
	defer b.Close()
	b.SetMaxOpenConns(1)
	var n int
	if err := b.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='session_messages'`).Scan(&n); err != nil || n == 0 {
		return err
	}
	var mode string
	if err := b.QueryRowContext(ctx, `PRAGMA journal_mode = DELETE`).Scan(&mode); err != nil {
		return fmt.Errorf("scrub reasoning: %w", err)
	}
	if mode != "delete" {
		return fmt.Errorf("scrub reasoning: journal_mode is %q, want delete", mode)
	}
	for _, stmt := range []string{`PRAGMA secure_delete = ON`, scrubStatement} {
		if _, err := b.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("scrub reasoning: %w", err)
		}
	}
	return nil
}
