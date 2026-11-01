package diag

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newLog opens a log in a temporary directory; the test owns it and must Close it.
func newLog(t *testing.T, opts Options) *Log {
	t.Helper()
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	l, err := Open(opts)
	if err != nil {
		t.Fatalf("Open(%s): %v", opts.Dir, err)
	}
	return l
}

// newDebugLog opens a log that records every level.
func newDebugLog(t *testing.T, opts Options) *Log {
	t.Helper()
	level := new(slog.LevelVar)
	level.Set(slog.LevelDebug)
	opts.Level = level
	return newLog(t, opts)
}

// closeLog closes l and fails the test on error.
func closeLog(t *testing.T, l *Log) {
	t.Helper()
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// logFileIndex is the sort key of a log file: 0 is the current file, i>0 the i-th rotated one.
func logFileIndex(name string) (int, bool) {
	m := logNameRE.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	if m[1] == "" {
		return 0, true
	}
	index, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return index, true
}

// logFileNames returns the log files of dir oldest first.
func logFileNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	type indexed struct {
		index int
		name  string
	}
	var found []indexed
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		index, ok := logFileIndex(entry.Name())
		if !ok {
			continue
		}
		found = append(found, indexed{index: index, name: entry.Name()})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].index > found[j].index })
	names := make([]string, 0, len(found))
	for _, f := range found {
		names = append(names, f.name)
	}
	return names
}

// logRecords returns every record of dir, oldest first.
func logRecords(t *testing.T, dir string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, name := range logFileNames(t, dir) {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var record map[string]any
			if err := json.Unmarshal(line, &record); err != nil {
				t.Fatalf("decode %q in %s: %v", line, name, err)
			}
			out = append(out, record)
		}
		if err := scanner.Err(); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		f.Close()
	}
	return out
}

// onlyRecord returns the single record of dir.
func onlyRecord(t *testing.T, dir string) map[string]any {
	t.Helper()
	records := logRecords(t, dir)
	if len(records) != 1 {
		t.Fatalf("want 1 record, got %d: %v", len(records), records)
	}
	return records[0]
}

// rawLogText is every log file's content, for "this secret must not appear anywhere" assertions.
func rawLogText(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	for _, name := range logFileNames(t, dir) {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		b.Write(data)
	}
	return b.String()
}

// assertNoText fails when the log files contain any of the strings.
func assertNoText(t *testing.T, dir string, needles ...string) {
	t.Helper()
	text := rawLogText(t, dir)
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			t.Fatalf("log contains %q:\n%s", needle, text)
		}
	}
}

func TestWritesJSONLinesAndCloses(t *testing.T) {
	l := newDebugLog(t, Options{})
	logger := l.Logger()
	logger.Info("run.started", "session_id", "s-1", "step", 2)
	logger.Warn("tool.end", "session_id", "s-1", "ok", false)
	logger.Error("session.failed", "session_id", "s-1", "error", "boom")
	closeLog(t, l)

	records := logRecords(t, l.Dir())
	if len(records) != 3 {
		t.Fatalf("want 3 records, got %d", len(records))
	}
	want := []struct {
		level, msg string
	}{
		{"INFO", "run.started"},
		{"WARN", "tool.end"},
		{"ERROR", "session.failed"},
	}
	for i, w := range want {
		record := records[i]
		if record["level"] != w.level || record["msg"] != w.msg {
			t.Fatalf("record %d = %v, want %s %s", i, record, w.level, w.msg)
		}
		if _, err := time.Parse(time.RFC3339Nano, record["time"].(string)); err != nil {
			t.Fatalf("record %d time %v: %v", i, record["time"], err)
		}
	}
	// time, level and msg come first, then the attributes.
	line := strings.SplitN(rawLogText(t, l.Dir()), "\n", 2)[0]
	if !strings.HasPrefix(line, `{"time":"`) || !strings.Contains(line, `","level":"INFO","msg":"run.started",`) {
		t.Fatalf("unexpected field order: %s", line)
	}
	if records[0]["session_id"] != "s-1" || records[0]["step"] != float64(2) {
		t.Fatalf("attributes lost: %v", records[0])
	}
}

func TestCloseIsIdempotentAndDropsAfterClose(t *testing.T) {
	l := newDebugLog(t, Options{})
	l.Logger().Info("first")
	closeLog(t, l)
	closeLog(t, l)

	before := l.Stats()
	// A logger call after Close is dropped, never written and never panics.
	l.Logger().Info("after close")
	after := l.Stats()
	if after.Written != before.Written {
		t.Fatalf("written grew after Close: %d -> %d", before.Written, after.Written)
	}
	if after.Dropped != before.Dropped+1 {
		t.Fatalf("dropped = %d, want %d", after.Dropped, before.Dropped+1)
	}
	if records := logRecords(t, l.Dir()); len(records) != 1 || records[0]["msg"] != "first" {
		t.Fatalf("records after Close = %v", records)
	}
}

func TestNonBlockingQueueDropsWhenWriterStalls(t *testing.T) {
	gate := make(chan struct{})
	l := newDebugLog(t, Options{QueueSize: 16, beforeWrite: func() { <-gate }})
	logger := l.Logger()

	const total = 5000
	start := time.Now()
	for i := range total {
		logger.Info("probe", "i", i)
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("logging blocked for %v with a stalled writer", elapsed)
	}
	if dropped := l.Stats().Dropped; dropped == 0 {
		t.Fatalf("a stalled writer must drop records, dropped = %d", dropped)
	}
	if cap(l.queue) != 16 {
		t.Fatalf("queue capacity = %d, want 16", cap(l.queue))
	}
	close(gate)
	closeLog(t, l)

	stats := l.Stats()
	if stats.Written+stats.Dropped != total {
		t.Fatalf("written %d + dropped %d != %d logged", stats.Written, stats.Dropped, total)
	}
	if stats.Written == 0 {
		t.Fatal("no record reached the file")
	}
}

func TestRotationKeepsMaxFilesAndBoundedBytes(t *testing.T) {
	const (
		maxFileBytes = 1024
		maxFiles     = 3
		records      = 300
	)
	l := newDebugLog(t, Options{MaxFileBytes: maxFileBytes, MaxFiles: maxFiles})
	logger := l.Logger()
	for i := range records {
		logger.Info("rotation", "i", i, "pad", strings.Repeat("x", 40))
	}
	closeLog(t, l)

	names := logFileNames(t, l.Dir())
	if len(names) > maxFiles {
		t.Fatalf("kept %d files, want at most %d: %v", len(names), maxFiles, names)
	}
	var total int64
	for _, name := range names {
		info, err := os.Stat(filepath.Join(l.Dir(), name))
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
		// One line may cross the bound only when it alone exceeds it; these lines are ~100 bytes.
		if info.Size() > maxFileBytes+256 {
			t.Fatalf("%s is %d bytes, want at most %d", name, info.Size(), maxFileBytes+256)
		}
	}
	if total > maxFiles*(maxFileBytes+256) {
		t.Fatalf("log holds %d bytes, want at most %d", total, maxFiles*(maxFileBytes+256))
	}
	stats := l.Stats()
	if stats.Files != uint64(len(names)) || stats.Bytes != uint64(total) {
		t.Fatalf("stats files/bytes = %d/%d, want %d/%d", stats.Files, stats.Bytes, len(names), total)
	}
	// The newest record survives in the current file, and every file is a complete JSON line set.
	recordsRead := logRecords(t, l.Dir())
	if len(recordsRead) == 0 {
		t.Fatal("no records after rotation")
	}
	last := recordsRead[len(recordsRead)-1]
	if last["msg"] != "rotation" || last["i"] != float64(records-1) {
		t.Fatalf("newest record = %v, want i=%d", last, records-1)
	}
}

func TestRetentionDeletesOldFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour)
	write := func(name string, at time.Time) {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(`{"time":"x","level":"INFO","msg":"old"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	write(FileName, old)
	write("lumencortex.1.jsonl", old)
	write("lumencortex.2.jsonl", now)
	write("other.jsonl", old)
	write("lumencortex.notes", old)

	l := newLog(t, Options{Dir: dir, RetentionDays: 14, Now: func() time.Time { return now }})
	defer closeLog(t, l)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	want := []string{"lumencortex.2.jsonl", "lumencortex.notes", "other.jsonl"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("files after retention = %v, want %v", names, want)
	}

	// Rotation prunes too: an aged rotated file goes when the next rotation runs.
	aged := filepath.Join(dir, "lumencortex.2.jsonl")
	if err := os.Chtimes(aged, old, old); err != nil {
		t.Fatal(err)
	}
	l.prune(false)
	if _, err := os.Stat(aged); !os.IsNotExist(err) {
		t.Fatalf("aged rotated file survived rotation-time retention: %v", err)
	}
}

func TestCloseStopsWriterGoroutine(t *testing.T) {
	// Warm up so a lazily started runtime goroutine is not counted as a leak.
	warm := newDebugLog(t, Options{})
	warm.Logger().Info("warm")
	closeLog(t, warm)
	baseline := runtime.NumGoroutine()

	for range 5 {
		l := newDebugLog(t, Options{})
		for j := range 50 {
			l.Logger().Info("probe", "i", j)
		}
		closeLog(t, l)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := runtime.NumGoroutine()
		if got <= baseline {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines = %d after Close, baseline %d", got, baseline)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStatsReportQueueCapacity(t *testing.T) {
	l := newDebugLog(t, Options{QueueSize: 64})
	defer closeLog(t, l)
	stats := l.Stats()
	if stats.QueueCap != 64 {
		t.Fatalf("queue capacity = %d, want 64", stats.QueueCap)
	}
	l.Logger().Info("probe")
	deadline := time.Now().Add(2 * time.Second)
	for l.Stats().Written != 1 {
		if time.Now().After(deadline) {
			t.Fatal("record never reached the file")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFlushMakesNewestRecordsReadable(t *testing.T) {
	l := newDebugLog(t, Options{})
	defer closeLog(t, l)
	l.Logger().Info("visible now")
	records, err := l.Query(context.Background(), Query{MinLevel: slog.LevelDebug})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Msg != "visible now" {
		t.Fatalf("query before flush/close = %v", records)
	}
}

func TestRecordLargerThanBoundBecomesStub(t *testing.T) {
	l := newDebugLog(t, Options{})
	logger := l.Logger()
	// A map value that the redactor does not reduce (a content key would) and that is large.
	big := map[string]any{}
	for i := range 200 {
		big[fmt.Sprintf("k%03d", i)] = strings.Repeat("v", 200)
	}
	logger.Info("huge", "data", big)
	closeLog(t, l)
	record := onlyRecord(t, l.Dir())
	if record["truncated"] != true {
		t.Fatalf("record was not stubbed: %v", record)
	}
	if record["msg"] != "huge" || record["bytes"] == nil {
		t.Fatalf("stub lost the message or the size: %v", record)
	}
}
