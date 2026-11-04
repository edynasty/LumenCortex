package diag

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// logSession writes records with the correlation attributes a run produces.
func logSession(t *testing.T, l *Log, session, job string, n int, level slog.Level) {
	t.Helper()
	for i := range n {
		args := []any{"session_id", session, "step", i}
		if job != "" {
			args = append(args, "job_id", job)
		}
		l.Logger().Log(context.Background(), level, "probe", args...)
	}
}

func TestQueryFiltersAndOrders(t *testing.T) {
	l := newDebugLog(t, Options{})
	logSession(t, l, "s-1", "", 3, slog.LevelInfo)
	logSession(t, l, "s-2", "job-1", 2, slog.LevelWarn)
	l.Logger().Error("session.failed", "session_id", "s-1", "error_code", "provider:timeout")
	closeLog(t, l)

	ctx := context.Background()
	all, err := l.Query(ctx, Query{MinLevel: slog.LevelDebug})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 6 {
		t.Fatalf("all = %d records, want 6", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].Time.After(all[i-1].Time) {
			t.Fatalf("records are not newest first: %v then %v", all[i-1].Time, all[i].Time)
		}
	}
	if all[0].Msg != "session.failed" {
		t.Fatalf("newest record = %v, want session.failed", all[0])
	}

	bySession, err := l.Query(ctx, Query{MinLevel: slog.LevelDebug, SessionID: "s-2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(bySession) != 2 {
		t.Fatalf("session s-2 = %d records, want 2", len(bySession))
	}
	byJob, err := l.Query(ctx, Query{MinLevel: slog.LevelDebug, JobID: "job-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byJob) != 2 {
		t.Fatalf("job-1 = %d records, want 2", len(byJob))
	}
	warnOnly, err := l.Query(ctx, Query{MinLevel: slog.LevelWarn})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnOnly) != 3 {
		t.Fatalf("warn and above = %d records, want 3", len(warnOnly))
	}
	for _, record := range warnOnly {
		level, _ := ParseLevel(record.Level)
		if level < slog.LevelWarn {
			t.Fatalf("level filter returned %s", record.Level)
		}
	}
	limited, err := l.Query(ctx, Query{MinLevel: slog.LevelDebug, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 2 || limited[0].Msg != "session.failed" {
		t.Fatalf("limit 2 = %v", limited)
	}
	// A limit above the cap is capped, not an error.
	capped, err := l.Query(ctx, Query{MinLevel: slog.LevelDebug, Limit: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if len(capped) != 6 {
		t.Fatalf("capped = %d records", len(capped))
	}
	since := all[1].Time
	recent, err := l.Query(ctx, Query{MinLevel: slog.LevelDebug, Since: since})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range recent {
		if record.Time.Before(since) {
			t.Fatalf("since returned %v, before %v", record.Time, since)
		}
	}
}

func TestQueryReadsAcrossRotatedFiles(t *testing.T) {
	l := newDebugLog(t, Options{MaxFileBytes: 2048, MaxFiles: 4})
	const records = 400
	for i := range records {
		l.Logger().Info("rotation", "i", i, "pad", strings.Repeat("p", 30))
	}
	closeLog(t, l)
	if len(logFileNames(t, l.Dir())) < 2 {
		t.Fatal("test did not rotate")
	}
	got, err := l.Query(context.Background(), Query{MinLevel: slog.LevelInfo, Limit: records})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no records read")
	}
	// Newest first, and strictly decreasing in time, across file boundaries.
	for i := 1; i < len(got); i++ {
		if got[i].Time.After(got[i-1].Time) {
			t.Fatalf("record %d is newer than its predecessor", i)
		}
	}
	last := got[0]
	if last.Attrs["i"] != float64(records-1) {
		t.Fatalf("newest record i = %v, want %d", last.Attrs["i"], records-1)
	}
}

func TestQuerySkipsCorruptLines(t *testing.T) {
	dir := t.TempDir()
	content := `{"time":"2026-01-01T00:00:00Z","level":"INFO","msg":"first"}` + "\n" +
		`{"time":"2026-01-01T00:00:01Z","level":"INFO","msg":"cut` + "\n" +
		`{"time":"2026-01-01T00:00:02Z","level":"INFO","msg":"second","session_id":"s-9"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	l := newLog(t, Options{Dir: dir})
	defer closeLog(t, l)
	got, err := l.Query(context.Background(), Query{MinLevel: slog.LevelDebug})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Msg != "second" || got[1].Msg != "first" {
		t.Fatalf("records = %v, want the two complete lines", got)
	}
	if got[0].Attrs["session_id"] != "s-9" {
		t.Fatalf("attrs = %v", got[0].Attrs)
	}
}

func TestQueryRespectsCancellation(t *testing.T) {
	l := newDebugLog(t, Options{})
	for i := range 50 {
		l.Logger().Info("probe", "i", i)
	}
	closeLog(t, l)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Query(ctx, Query{MinLevel: slog.LevelDebug}); err == nil {
		t.Fatal("a cancelled query must fail")
	}
}

func TestQueryMemoryIsBoundedByLimit(t *testing.T) {
	const total = 20000
	// The queue holds every record: a full queue drops by design (it never makes the logger wait),
	// and this test is about the query, not the writer keeping up.
	l := newDebugLog(t, Options{QueueSize: total})
	for i := range total {
		l.Logger().Info("probe", "i", i, "pad", strings.Repeat("m", 200))
	}
	closeLog(t, l)
	// A limit of 10 over 20000 records must return exactly the newest 10.
	got, err := l.Query(context.Background(), Query{MinLevel: slog.LevelInfo, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 {
		t.Fatalf("limit 10 returned %d records", len(got))
	}
	for i := range got {
		want := float64(total - 1 - i)
		if got[i].Attrs["i"] != want {
			t.Fatalf("record %d = %v, want i=%v", i, got[i].Attrs["i"], want)
		}
	}
}

func TestExportZipIsValidAndRedacted(t *testing.T) {
	l := newDebugLog(t, Options{MaxFileBytes: 4096, MaxFiles: 3})
	for i := range 100 {
		l.Logger().Info("probe", "i", i, "pad", strings.Repeat("z", 100))
	}
	l.Logger().Error("session.failed", "session_id", "s-1", "authorization", "Bearer super-secret-bearer-token-value",
		"error", "boom", "prompt", "the whole prompt")
	closeLog(t, l)

	path := filepath.Join(t.TempDir(), "diag.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{"coreVersion": "0.1.4-preview", "createdAt": "2026-01-01T00:00:00Z"}
	if err := l.Export(context.Background(), file, manifest); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	defer reader.Close()
	var logs, manifests int
	var text strings.Builder
	for _, entry := range reader.File {
		switch {
		case strings.HasPrefix(entry.Name, "logs/") && strings.HasSuffix(entry.Name, ".jsonl"):
			logs++
		case entry.Name == "manifest.json":
			manifests++
		default:
			t.Fatalf("unexpected entry %q", entry.Name)
		}
		rc, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		data := make([]byte, entry.UncompressedSize64)
		if _, err := rc.Read(data); err != nil && err.Error() != "EOF" {
			t.Fatalf("read %s: %v", entry.Name, err)
		}
		rc.Close()
		text.Write(data)
	}
	if logs == 0 || manifests != 1 {
		t.Fatalf("entries: %d logs, %d manifests", logs, manifests)
	}
	if logs != len(logFileNames(t, l.Dir())) {
		t.Fatalf("exported %d log files, directory has %d", logs, len(logFileNames(t, l.Dir())))
	}
	body := text.String()
	for _, secret := range plantedSecrets {
		if strings.Contains(body, secret) {
			t.Fatalf("export contains %q", secret)
		}
	}
	if strings.Contains(body, "the whole prompt") {
		t.Fatal("export contains a prompt")
	}
	if !strings.Contains(body, "coreVersion") {
		t.Fatal("manifest missing from the archive")
	}
	// Every log entry is JSON Lines.
	for _, entry := range reader.File {
		if entry.Name == "manifest.json" {
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(rc)
		for decoder.More() {
			var record map[string]any
			if err := decoder.Decode(&record); err != nil {
				t.Fatalf("%s is not JSON Lines: %v", entry.Name, err)
			}
		}
		rc.Close()
	}
}

func TestExportEmptyLogIsStillValid(t *testing.T) {
	l := newDebugLog(t, Options{})
	closeLog(t, l)
	path := filepath.Join(t.TempDir(), "empty.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Export(context.Background(), file, nil); err != nil {
		t.Fatalf("Export: %v", err)
	}
	file.Close()
	reader, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	defer reader.Close()
	if len(reader.File) != 1 || reader.File[0].Name != "manifest.json" {
		t.Fatalf("entries = %v, want only the manifest", reader.File)
	}
}

func TestExportCancellationRemovesNothingButFails(t *testing.T) {
	l := newDebugLog(t, Options{})
	l.Logger().Info("probe")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	path := filepath.Join(t.TempDir(), "cancelled.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	err = l.Export(ctx, file, nil)
	file.Close()
	if err == nil {
		t.Fatal("a cancelled export must fail")
	}
	closeLog(t, l)
}

func TestParseLevel(t *testing.T) {
	cases := map[string]struct {
		level slog.Level
		ok    bool
	}{
		"":      {slog.LevelInfo, true},
		"debug": {slog.LevelDebug, true},
		"DEBUG": {slog.LevelDebug, true},
		"info":  {slog.LevelInfo, true},
		"warn":  {slog.LevelWarn, true},
		"error": {slog.LevelError, true},
		"bogus": {0, false},
	}
	for name, want := range cases {
		got, ok := ParseLevel(name)
		if ok != want.ok || (ok && got != want.level) {
			t.Fatalf("ParseLevel(%q) = %v, %v; want %v, %v", name, got, ok, want.level, want.ok)
		}
	}
}

func BenchmarkHandlerLog(b *testing.B) {
	dir := b.TempDir()
	level := new(slog.LevelVar)
	level.Set(slog.LevelInfo)
	l, err := Open(Options{Dir: dir, Level: level})
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()
	logger := l.Logger().With("session_id", "bench", "run_id", "r-1")
	attrs := []any{"tool_call_id", "call-1", "step", 7, "name", "bash", "duration_ms", 12}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		logger.Info("tool.end", attrs...)
	}
	b.StopTimer()
	stats := l.Stats()
	b.ReportMetric(float64(stats.Dropped)/float64(b.N), "dropped/op")
}

func BenchmarkEncodeRedactedRecord(b *testing.B) {
	l := &Log{now: time.Now, level: new(slog.LevelVar)}
	record := slog.NewRecord(time.Now(), slog.LevelInfo, "tool.end", 0)
	record.AddAttrs(
		slog.String("session_id", "s-1"),
		slog.String("tool_call_id", "call-1"),
		slog.Int("step", 7),
		slog.String("command", "go test ./... -run TestSomething"),
		slog.String("error", "exit status 1"),
	)
	pre := []preAttr{{attr: slog.String("run_id", "r-1")}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = l.encode(record, pre, nil)
	}
}

// ExampleStats documents the writer's memory bound: QueueSize records of at most maxRecordBytes.
func ExampleStats() {
	fmt.Println(DefaultQueueSize * maxRecordBytes)
	// Output: 67108864
}
