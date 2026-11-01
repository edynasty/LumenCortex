package diag

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Query limits: the reader returns at most MaxQueryLimit records, newest first, and defaults to
// DefaultQueryLimit when the caller names none.
const (
	DefaultQueryLimit = 200
	MaxQueryLimit     = 1000
)

// Query filters records. A zero filter field does not filter; MinLevel is the minimum level and its
// zero value is slog.LevelInfo, so use slog.LevelDebug to read everything.
type Query struct {
	MinLevel   slog.Level
	SessionID  string
	RunID      string
	JobID      string
	ScheduleID string
	ToolCallID string
	Since      time.Time
	Limit      int
}

// Record is one log line as the reader returns it: the attributes are the top-level fields besides
// time, level and msg, already redacted when they were written.
type Record struct {
	Time  time.Time      `json:"time"`
	Level string         `json:"level"`
	Msg   string         `json:"msg"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

// MaxScanLimit is the largest number of records Scan returns.
const MaxScanLimit = 5000

// Query returns the newest records that match q, reading the files from newest to oldest. Memory is
// bounded by the limit: each file is streamed and only its newest matches are kept.
func (l *Log) Query(ctx context.Context, q Query) ([]Record, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultQueryLimit
	}
	if limit > MaxQueryLimit {
		limit = MaxQueryLimit
	}
	return l.query(ctx, q, limit)
}

// Scan returns the newest records that match q, up to limit, above Query's cap. It exists for a
// reader that has to see a whole run (the runtime's trace view); a host reads through Query, whose
// bound keeps a UI responsive. limit is clamped to MaxScanLimit.
func (l *Log) Scan(ctx context.Context, q Query, limit int) ([]Record, error) {
	if limit <= 0 {
		limit = MaxScanLimit
	}
	if limit > MaxScanLimit {
		limit = MaxScanLimit
	}
	return l.query(ctx, q, limit)
}

// query is the shared reader: files newest to oldest, each streamed with a ring of the newest
// matches.
func (l *Log) query(ctx context.Context, q Query, limit int) ([]Record, error) {
	l.flushNow()
	names, err := l.fileNames()
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, limit)
	for _, name := range names {
		if len(out) >= limit {
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		recs, err := readFileNewest(ctx, filepath.Join(l.dir, name), q, limit-len(out))
		if err != nil {
			return nil, err
		}
		out = append(out, recs...)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// readFileNewest streams one file and returns its newest limit matching records, newest first. A
// ring keeps the last limit matches, so memory does not grow with the file.
func readFileNewest(ctx context.Context, path string, q Query, limit int) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("diag: open %s: %w", path, err)
	}
	defer f.Close()
	ring := make([]Record, 0, limit)
	start := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), maxRecordBytes+1024)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rec, ok := decodeRecord(scanner.Bytes())
		if !ok || !matches(rec, q) {
			continue
		}
		if len(ring) < limit {
			ring = append(ring, rec)
			continue
		}
		ring[start] = rec
		start = (start + 1) % limit
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("diag: read %s: %w", path, err)
	}
	if len(ring) < limit {
		start = 0
	}
	out := make([]Record, 0, len(ring))
	for i := len(ring) - 1; i >= 0; i-- {
		out = append(out, ring[(start+i)%len(ring)])
	}
	return out, nil
}

// decodeRecord parses one JSON line. A partial line left by a crash, or a line of another shape, is
// skipped rather than failing the read.
func decodeRecord(line []byte) (Record, bool) {
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return Record{}, false
	}
	rec := Record{}
	if s, ok := raw["time"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			rec.Time = t
		}
	}
	rec.Level, _ = raw["level"].(string)
	rec.Msg, _ = raw["msg"].(string)
	delete(raw, "time")
	delete(raw, "level")
	delete(raw, "msg")
	if len(raw) > 0 {
		rec.Attrs = raw
	}
	return rec, true
}

// matches applies the query's filters to one record.
func matches(rec Record, q Query) bool {
	level, ok := ParseLevel(rec.Level)
	if !ok || level < q.MinLevel {
		return false
	}
	if !q.Since.IsZero() && rec.Time.Before(q.Since) {
		return false
	}
	if q.SessionID != "" && attrString(rec.Attrs, "session_id") != q.SessionID {
		return false
	}
	if q.RunID != "" && attrString(rec.Attrs, "run_id") != q.RunID {
		return false
	}
	if q.JobID != "" && attrString(rec.Attrs, "job_id") != q.JobID {
		return false
	}
	if q.ScheduleID != "" && attrString(rec.Attrs, "schedule_id") != q.ScheduleID {
		return false
	}
	if q.ToolCallID != "" && attrString(rec.Attrs, "tool_call_id") != q.ToolCallID {
		return false
	}
	return true
}

// attrString reads a string attribute; numbers are not correlation ids.
func attrString(attrs map[string]any, key string) string {
	if attrs == nil {
		return ""
	}
	s, _ := attrs[key].(string)
	return s
}

// ParseLevel parses a logged level name ("DEBUG", "INFO", "WARN", "ERROR", and slog's "INFO+2"
// form). The empty string is info, the level a log defaults to.
func ParseLevel(name string) (slog.Level, bool) {
	name = strings.TrimSpace(name)
	// The names a log writes are hit for every record a reader filters; EqualFold allocates nothing.
	switch {
	case name == "":
		return slog.LevelInfo, true
	case strings.EqualFold(name, "debug"):
		return slog.LevelDebug, true
	case strings.EqualFold(name, "info"):
		return slog.LevelInfo, true
	case strings.EqualFold(name, "warn"):
		return slog.LevelWarn, true
	case strings.EqualFold(name, "error"):
		return slog.LevelError, true
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(name)); err != nil {
		return 0, false
	}
	return level, true
}
