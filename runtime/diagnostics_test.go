package runtime

import (
	"archive/zip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// diagSecrets are values no log file, record or export may contain.
var diagSecrets = []string{
	"sk-live-abcdefghijklmnopqrstuvwxyz0123456789",
	"super-secret-bearer-token-value",
	"chunk-payload-secret",
}

// diagWorkspace opens an Engine on a fresh git workspace whose state lives in its own directory, so
// the test reads the logs it wrote and nothing else.
func diagWorkspace(t *testing.T) (*Engine, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	isolatedState(t)
	root := physical(t, t.TempDir())
	initGitRepo(t, root)
	engine, err := Open(Options{Workspace: root})
	if err != nil {
		t.Fatalf("Open(%s): %v", root, err)
	}
	return engine, root
}

// diagLogText is every log file of the Engine's log directory.
func diagLogText(t *testing.T, engine *Engine) string {
	t.Helper()
	entries, err := os.ReadDir(engine.diag.Dir())
	if err != nil {
		t.Fatalf("read %s: %v", engine.diag.Dir(), err)
	}
	var b strings.Builder
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(engine.diag.Dir(), entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
	}
	return b.String()
}

func TestEngineLogsEventsRedactedAndReopens(t *testing.T) {
	engine, root := diagWorkspace(t)
	// The configuration's logLevel governs the file; a test that needs debug sets it on the log.
	engine.diag.Level().Set(slog.LevelDebug)

	handle := newTestSession(t, engine, "diagnostics")
	sessionID := handle.ID
	engine.events.publish("tool.start", sessionID, map[string]any{
		"step": 3, "toolCallId": "call-1", "name": "bash",
		"args": `{"command":"curl -H 'Authorization: Bearer super-secret-bearer-token-value' https://x"}`,
	})
	engine.events.publish("tool.output", sessionID, map[string]any{
		"step": 3, "toolCallId": "call-1", "name": "bash", "stream": "stdout", "chunk": "chunk-payload-secret",
	})
	engine.events.publish("tool.end", sessionID, map[string]any{
		"step": 3, "toolCallId": "call-1", "name": "bash", "ok": false,
	})
	engine.events.publish("job.state", sessionID, map[string]any{
		"jobId": "job-1", "sessionId": sessionID, "state": "running", "source": "run",
	})
	engine.events.publish("provider.failover", sessionID, map[string]any{
		"provider": "test", "model": "b0", "apiKey": "sk-live-abcdefghijklmnopqrstuvwxyz0123456789",
		"error": "boom",
	})
	// Give the bridge a moment to drain: it is asynchronous by design.
	waitForRecords(t, engine, sessionID, 5)

	if err := engine.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen the same workspace: the log is durable, the API reads it after a restart.
	reopened, err := Open(Options{Workspace: root})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	ctx := context.Background()
	records, err := reopened.DiagnosticLogs(ctx, DiagnosticQuery{Level: "debug", SessionID: sessionID, Limit: 100})
	if err != nil {
		t.Fatalf("DiagnosticLogs: %v", err)
	}
	if len(records) < 5 {
		t.Fatalf("records = %d, want at least 5: %+v", len(records), records)
	}
	byMsg := map[string]DiagnosticRecord{}
	for _, record := range records {
		byMsg[record.Msg] = record
	}
	start, ok := byMsg["tool.start"]
	if !ok {
		t.Fatalf("tool.start missing: %+v", records)
	}
	if start.SessionID != sessionID || start.Step != 3 || start.ToolCallID != "call-1" {
		t.Fatalf("tool.start correlation = %+v", start)
	}
	end, ok := byMsg["tool.end"]
	if !ok || end.Level != "WARN" {
		t.Fatalf("tool.end = %+v, want WARN", end)
	}
	if _, ok := byMsg["tool.output"]; !ok {
		t.Fatalf("tool.output missing at debug: %+v", records)
	}
	job, ok := byMsg["job.state"]
	if !ok || job.JobID != "job-1" {
		t.Fatalf("job.state = %+v", job)
	}
	failover, ok := byMsg["provider.failover"]
	if !ok || failover.Provider != "test" || failover.Model != "b0" {
		t.Fatalf("provider.failover = %+v", failover)
	}

	text := diagLogText(t, reopened)
	for _, secret := range diagSecrets {
		if strings.Contains(text, secret) {
			t.Fatalf("log contains %q:\n%s", secret, text)
		}
	}
	raw, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "chunk-payload-secret") {
		t.Fatal("the tool output chunk reached the API")
	}
	if !strings.Contains(string(raw), `"bytes":20`) {
		t.Fatalf("the tool output record does not report the chunk's size: %s", raw)
	}

	// The level filter applies to the query too.
	infoOnly, err := reopened.DiagnosticLogs(ctx, DiagnosticQuery{Level: "info", SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range infoOnly {
		if record.Level == "DEBUG" {
			t.Fatalf("level info returned a debug record: %+v", record)
		}
	}
	if len(infoOnly) >= len(records) {
		t.Fatalf("info returned %d records, debug %d", len(infoOnly), len(records))
	}

	// Export names the workspace by base name and hash, and holds no secret.
	dest := t.TempDir()
	path, err := reopened.ExportDiagnostics(ctx, dest)
	if err != nil {
		t.Fatalf("ExportDiagnostics: %v", err)
	}
	if !strings.HasPrefix(path, dest) {
		t.Fatalf("export path %s is not inside %s", path, dest)
	}
	if !strings.HasPrefix(filepath.Base(path), "lumencortex-diagnostics-") || !strings.HasSuffix(path, ".zip") {
		t.Fatalf("export name = %s", filepath.Base(path))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("export mode = %o, want 600", perm)
	}
	entries := zipEntries(t, path)
	if len(entries) == 0 {
		t.Fatal("empty export")
	}
	var manifest map[string]any
	var logs strings.Builder
	for name, content := range entries {
		switch {
		case name == "manifest.json":
			if err := json.Unmarshal([]byte(content), &manifest); err != nil {
				t.Fatalf("manifest: %v", err)
			}
		case strings.HasPrefix(name, "logs/"):
			logs.WriteString(content)
		default:
			t.Fatalf("unexpected export entry %q", name)
		}
	}
	for _, secret := range diagSecrets {
		if strings.Contains(logs.String(), secret) || strings.Contains(entries["manifest.json"], secret) {
			t.Fatalf("export contains %q", secret)
		}
	}
	if manifest["coreVersion"] != GoRuntimePreviewVersion {
		t.Fatalf("manifest coreVersion = %v", manifest["coreVersion"])
	}
	if manifest["workspace"] != filepath.Base(physical(t, root)) {
		t.Fatalf("manifest workspace = %v, want the base name", manifest["workspace"])
	}
	if _, ok := manifest["workspaceHash"].(string); !ok {
		t.Fatalf("manifest has no workspace hash: %v", manifest)
	}
	if strings.Contains(entries["manifest.json"], root) {
		t.Fatal("manifest leaks the workspace path")
	}
	stats, ok := manifest["stats"].(map[string]any)
	if !ok {
		t.Fatalf("manifest stats = %v", manifest["stats"])
	}
	if _, ok := stats["log"]; !ok {
		t.Fatalf("manifest stats has no log section: %v", stats)
	}
}

// waitForRecords waits until the session has at least want records in the log.
func waitForRecords(t *testing.T, engine *Engine, sessionID string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		records, err := engine.DiagnosticLogs(context.Background(),
			DiagnosticQuery{Level: "debug", SessionID: sessionID, Limit: 100})
		if err != nil {
			t.Fatalf("DiagnosticLogs: %v", err)
		}
		if len(records) >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d records after 5s, want %d", len(records), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// zipEntries reads a zip into name -> content.
func zipEntries(t *testing.T, path string) map[string]string {
	t.Helper()
	reader, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("zip %s: %v", path, err)
	}
	defer reader.Close()
	out := map[string]string{}
	for _, entry := range reader.File {
		rc, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[entry.Name] = string(data)
	}
	return out
}

func TestEngineDiagnosticsUnavailableIsReported(t *testing.T) {
	isolatedState(t)
	root := physical(t, t.TempDir())
	// A file where the log directory belongs makes diag.Open fail; the Engine must still open.
	engine, err := Open(Options{Workspace: root})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	logDir := filepath.Join(engine.repoDir, "logs")
	if err := engine.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := os.RemoveAll(logDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	blocked, err := Open(Options{Workspace: root})
	if err != nil {
		t.Fatalf("Open with an unusable log directory must succeed: %v", err)
	}
	defer blocked.Close()
	stats := blocked.DiagnosticStats()
	if stats.Enabled {
		t.Fatalf("stats report an enabled log: %+v", stats)
	}
	if !strings.Contains(stats.Error, "logs") {
		t.Fatalf("stats error = %q", stats.Error)
	}
	if _, err := blocked.DiagnosticLogs(context.Background(), DiagnosticQuery{}); err == nil {
		t.Fatal("DiagnosticLogs must fail when the log is unavailable")
	}
	if _, err := blocked.ExportDiagnostics(context.Background(), t.TempDir()); err == nil {
		t.Fatal("ExportDiagnostics must fail when the log is unavailable")
	}
	// The Engine's logger is never nil, so runtime code that logs keeps working.
	if blocked.log == nil {
		t.Fatal("Engine.log is nil")
	}
	blocked.log.Info("still works")
}

func TestEngineDiagnosticsStatsCountsDrops(t *testing.T) {
	engine, _ := diagWorkspace(t)
	engine.diag.Level().Set(slog.LevelDebug)
	engine.events.publish("session.start", "s-stats", map[string]any{"goal": "x"})
	waitForRecords(t, engine, "s-stats", 1)
	stats := engine.DiagnosticStats()
	if !stats.Enabled {
		t.Fatalf("stats disabled: %+v", stats)
	}
	if stats.Log.QueueCap == 0 || stats.Log.Written == 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.Events.Published == 0 {
		t.Fatalf("event stats = %+v", stats.Events)
	}
}

func TestEngineDiagnosticTrace(t *testing.T) {
	engine, _ := diagWorkspace(t)
	engine.diag.Level().Set(slog.LevelDebug)
	ctx := context.Background()
	handle := newTestSession(t, engine, "trace")
	sessionID := handle.ID
	engine.noteRunStarted(sessionID, "run-1")
	engine.events.publish("run.started", sessionID, map[string]any{"startedAt": time.Now()})
	engine.events.publish("llm.request", sessionID, map[string]any{
		"step": 1, "modelRef": "test/b0", "provider": "test", "model": "b0",
	})
	engine.events.publish("llm.complete", sessionID, map[string]any{
		"step": 1, "provider": "test", "model": "b0", "elapsedMs": 25,
	})
	engine.events.publish("tool.start", sessionID, map[string]any{
		"step": 1, "toolCallId": "call-9", "name": "bash",
	})
	engine.events.publish("tool.end", sessionID, map[string]any{
		"step": 1, "toolCallId": "call-9", "name": "bash", "ok": true, "durationMs": 40,
	})
	engine.events.publish("job.state", sessionID, map[string]any{
		"jobId": "job-7", "state": "running", "source": "run",
	})
	engine.events.publish("job.state", sessionID, map[string]any{
		"jobId": "job-7", "state": "completed", "exitCode": 0,
	})
	engine.events.publish("llm.request", sessionID, map[string]any{
		"step": 2, "provider": "test", "model": "b1",
	})
	engine.events.publish("provider.failover", sessionID, map[string]any{
		"step": 2, "provider": "test", "model": "b1", "class": "availability", "error": "boom",
	})
	engine.events.publish("session.complete", sessionID, map[string]any{"step": 1})
	waitForRecords(t, engine, sessionID, 10)

	spans, err := engine.DiagnosticTrace(ctx, sessionID, "")
	if err != nil {
		t.Fatalf("DiagnosticTrace: %v", err)
	}
	byKind := map[string][]TraceSpan{}
	for _, span := range spans {
		byKind[span.Kind] = append(byKind[span.Kind], span)
		if span.StartedAt.IsZero() {
			t.Fatalf("span without a start: %+v", span)
		}
	}
	for _, kind := range []string{TraceRun, TraceRequest, TraceTool, TraceJob, TraceCompletion} {
		if len(byKind[kind]) == 0 {
			t.Fatalf("no %s span in %+v", kind, spans)
		}
	}
	run := byKind[TraceRun][0]
	if run.ID != "run:run-1" || run.Status == TraceRunning {
		t.Fatalf("run span = %+v", run)
	}
	request := byKind[TraceRequest][0]
	if request.ParentID != run.ID || request.Provider != "test" || request.Model != "b0" {
		t.Fatalf("request span = %+v", request)
	}
	if request.EndedAt == nil {
		t.Fatalf("request span never closed: %+v", request)
	}
	if len(byKind[TraceRequest]) != 2 {
		t.Fatalf("request spans = %+v, want two", byKind[TraceRequest])
	}
	failed := byKind[TraceRequest][1]
	if failed.Status != TraceError || failed.ErrorCode != "availability" || failed.EndedAt == nil {
		t.Fatalf("failed request span = %+v", failed)
	}
	tool := byKind[TraceTool][0]
	if tool.ToolCallID != "call-9" || tool.Status != TraceOK || tool.ParentID != run.ID {
		t.Fatalf("tool span = %+v", tool)
	}
	job := byKind[TraceJob][0]
	if job.JobID != "job-7" || job.Status != TraceOK || job.EndedAt == nil {
		t.Fatalf("job span = %+v", job)
	}
	completion := byKind[TraceCompletion][0]
	if completion.Status != TraceOK || completion.Name != "session.complete" {
		t.Fatalf("completion span = %+v", completion)
	}
	// Ordered by start time.
	for i := 1; i < len(spans); i++ {
		if spans[i].StartedAt.Before(spans[i-1].StartedAt) {
			t.Fatalf("spans are not ordered: %+v", spans)
		}
	}
	// A run filter that matches nothing is empty, not an error.
	other, err := engine.DiagnosticTrace(ctx, sessionID, "run-does-not-exist")
	if err != nil {
		t.Fatalf("DiagnosticTrace(other run): %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("other run spans = %+v", other)
	}
	if _, err := engine.DiagnosticTrace(ctx, "", ""); err == nil {
		t.Fatal("a trace without a session must fail")
	}
}

func TestEngineLogLevelFromConfig(t *testing.T) {
	useGlobalConfig(t, `{"execution": {"logLevel": "warn"}}`)
	isolatedState(t)
	engine, err := Open(Options{Workspace: physical(t, t.TempDir())})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer engine.Close()
	if got := engine.diag.Level().Level(); got != slog.LevelWarn {
		t.Fatalf("level = %v, want warn", got)
	}
	engine.log.Info("suppressed")
	engine.log.Warn("kept")
	waitForRecords(t, engine, "", 1)
	records, err := engine.DiagnosticLogs(context.Background(), DiagnosticQuery{Level: "debug"})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.Msg == "suppressed" {
			t.Fatalf("info record written at warn level: %+v", records)
		}
	}
	found := false
	for _, record := range records {
		if record.Msg == "kept" {
			found = true
		}
	}
	if !found {
		t.Fatalf("warn record missing: %+v", records)
	}
}

func TestEngineRegisterDiagnosticSummary(t *testing.T) {
	engine, _ := diagWorkspace(t)
	engine.RegisterDiagnosticSummary("jobs", func(context.Context) (any, error) {
		return map[string]any{"queued": 1}, nil
	})
	engine.RegisterDiagnosticSummary("broken", func(context.Context) (any, error) {
		return nil, context.DeadlineExceeded
	})
	path, err := engine.ExportDiagnostics(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("ExportDiagnostics: %v", err)
	}
	entries := zipEntries(t, path)
	var manifest map[string]any
	if err := json.Unmarshal([]byte(entries["manifest.json"]), &manifest); err != nil {
		t.Fatal(err)
	}
	summaries, ok := manifest["summaries"].(map[string]any)
	if !ok {
		t.Fatalf("manifest summaries = %v", manifest["summaries"])
	}
	jobs, ok := summaries["jobs"].(map[string]any)
	if !ok || jobs["queued"] != float64(1) {
		t.Fatalf("jobs summary = %v", summaries["jobs"])
	}
	broken, ok := summaries["broken"].(map[string]any)
	if !ok || broken["error"] == nil {
		t.Fatalf("broken summary = %v", summaries["broken"])
	}
}

func TestEngineCloseFlushesAndStopsBridge(t *testing.T) {
	engine, _ := diagWorkspace(t)
	engine.diag.Level().Set(slog.LevelDebug)
	engine.events.publish("session.start", "s-flush", map[string]any{"goal": "x"})
	// Close immediately: the record must still be in the file afterwards.
	if err := engine.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	text := diagLogText(t, engine)
	if !strings.Contains(text, "session.start") {
		t.Fatalf("Close lost a queued record:\n%s", text)
	}
	// The API after Close reports the closed log, and logging stays safe.
	stats := engine.DiagnosticStats()
	if !stats.Enabled {
		t.Fatalf("stats after Close = %+v", stats)
	}
	engine.log.Info("after close")
	// Reading a closed log still works: the files are flushed and nothing writes to them again.
	records, err := engine.DiagnosticLogs(context.Background(), DiagnosticQuery{Level: "debug"})
	if err != nil {
		t.Fatalf("DiagnosticLogs after Close: %v", err)
	}
	for _, record := range records {
		if record.Msg == "after close" {
			t.Fatalf("a record logged after Close reached the file: %+v", records)
		}
	}
}

// ADR-0019 §5 (RT-state-8): the session goal is the user's task text - the first user row when there is
// no Input - so the session.start record reduces it to its byte length at every level, as a prompt is,
// and the log never holds it (it would outlive a deletion of the session).
func TestSessionGoalIsReducedToItsLengthInTheLog(t *testing.T) {
	engine, _ := diagWorkspace(t)
	engine.diag.Level().Set(slog.LevelDebug)
	const goal = "GOAL-SENTINEL-the user's private task text"
	handle, err := engine.NewSession(context.Background(), SessionOptions{Goal: goal})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if text := diagLogText(t, engine); strings.Contains(text, "GOAL-SENTINEL") {
		t.Fatalf("the log holds the session goal:\n%s", text)
	}
	records, err := engine.DiagnosticLogs(context.Background(), DiagnosticQuery{Level: "debug", SessionID: handle.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.Msg != "session.start" {
			continue
		}
		data, _ := record.Attrs["data"].(map[string]any)
		stub, _ := data["goal"].(map[string]any)
		if record.Level != "INFO" || stub["redacted"] != "content" || stub["bytes"] != float64(len(goal)) {
			t.Fatalf("session.start = %+v, want INFO with the goal as a content stub of %d bytes", record, len(goal))
		}
		return
	}
	t.Fatalf("no session.start record: %+v", records)
}

func TestDiagLogDirectoryPermissions(t *testing.T) {
	engine, _ := diagWorkspace(t)
	info, err := os.Stat(engine.diag.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("log dir mode = %o, want 700", perm)
	}
	engine.log.Info("one")
	engine.Close()
	entries, err := os.ReadDir(engine.diag.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("%s mode = %o, want 600", entry.Name(), perm)
		}
	}
}

func TestEventBusDropsAreExposedAndLogged(t *testing.T) {
	previous := eventStatsInterval
	eventStatsInterval = 10 * time.Millisecond
	t.Cleanup(func() { eventStatsInterval = previous })

	engine, _ := diagWorkspace(t)
	engine.diag.Level().Set(slog.LevelDebug)
	// A subscriber that never reads makes the bus drop: that is the loss the log must report.
	_, cancel := engine.Events(1)
	defer cancel()
	for i := range 200 {
		engine.events.publish("probe.event", "s-drop", map[string]any{"i": i})
	}
	stats := engine.DiagnosticStats()
	if stats.Events.Dropped == 0 {
		t.Fatalf("event drops = %+v, want the bus's dropped deliveries", stats.Events)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		records, err := engine.DiagnosticLogs(context.Background(), DiagnosticQuery{Level: "warn", Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, record := range records {
			if record.Msg == "events.dropped" && record.Level == "WARN" {
				found = true
			}
		}
		if found {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no events.dropped warning after 5s: %+v", records)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEventLevelMapping(t *testing.T) {
	cases := []struct {
		event string
		data  map[string]any
		want  slog.Level
	}{
		{"session.failed", nil, slog.LevelError},
		{"plugin.failed", nil, slog.LevelError},
		{"skills.error", nil, slog.LevelError},
		{"session.interrupted", nil, slog.LevelWarn},
		{"tool.end", map[string]any{"ok": false}, slog.LevelWarn},
		{"tool.end", map[string]any{"ok": true}, slog.LevelInfo},
		{"run.stopped", map[string]any{"error": "boom"}, slog.LevelWarn},
		{"run.stopped", map[string]any{}, slog.LevelInfo},
		{"job.state", map[string]any{"state": "failed"}, slog.LevelWarn},
		{"job.state", map[string]any{"state": "running"}, slog.LevelInfo},
		{"tool.output", nil, slog.LevelDebug},
		{"tool.start", nil, slog.LevelInfo},
		{"run.started", nil, slog.LevelInfo},
		{"session.message", nil, slog.LevelInfo},
		{"schedule.fired", nil, slog.LevelInfo},
		{"checkpoint.saved", nil, slog.LevelDebug},
	}
	for _, tc := range cases {
		if got := eventLevel(tc.event, tc.data); got != tc.want {
			t.Fatalf("eventLevel(%q, %v) = %v, want %v", tc.event, tc.data, got, tc.want)
		}
	}
}
