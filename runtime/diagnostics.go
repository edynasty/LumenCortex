package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/diag"
)

// diagEventBuffer is how many bus events the bridge may hold before the bus starts dropping the
// oldest for it. The bus is lossy by design: durable state stays in SQLite.
const diagEventBuffer = 1024

// eventStatsInterval is how often the bridge compares the bus's drop counter with the value it saw
// last. A growing counter is logged as a warning.
var eventStatsInterval = 30 * time.Second

// diagBridge is the event-to-log goroutine's state: how to stop it, how to wait for it, and the
// manifest sections other runtime components register.
type diagBridge struct {
	stop func()
	done chan struct{}

	mu        sync.Mutex
	summaries []diagSummary
	// runs maps a session to the run id its events belong to. The loop's events carry no run id, so
	// the Engine records the id it drew in prepareRun. The map is bounded: one entry per session with
	// a run in flight, and maxTrackedRuns at most.
	runs     map[string]string
	runOrder []string
}

// maxTrackedRuns bounds the session-to-run map. A session has one run at a time, and a process runs
// at most concurrency.maxAgents of them, so the map only reaches this bound when runs end without a
// run.stopped event.
const maxTrackedRuns = 256

// noteRunStarted records the run id the session's next events belong to.
func (e *Engine) noteRunStarted(sessionID, runID string) {
	if e.bridge == nil || sessionID == "" || runID == "" {
		return
	}
	e.bridge.mu.Lock()
	defer e.bridge.mu.Unlock()
	if e.bridge.runs == nil {
		e.bridge.runs = map[string]string{}
	}
	if _, ok := e.bridge.runs[sessionID]; !ok {
		e.bridge.runOrder = append(e.bridge.runOrder, sessionID)
	}
	e.bridge.runs[sessionID] = runID
	for len(e.bridge.runOrder) > maxTrackedRuns {
		oldest := e.bridge.runOrder[0]
		e.bridge.runOrder = e.bridge.runOrder[1:]
		delete(e.bridge.runs, oldest)
	}
}

// runFor returns the run id the session's events belong to, "" when none is known.
func (e *Engine) runFor(sessionID string) string {
	if e.bridge == nil || sessionID == "" {
		return ""
	}
	e.bridge.mu.Lock()
	defer e.bridge.mu.Unlock()
	return e.bridge.runs[sessionID]
}

// forgetRun drops the session's run id once the run stopped.
func (e *Engine) forgetRun(sessionID string) {
	if e.bridge == nil || sessionID == "" {
		return
	}
	e.bridge.mu.Lock()
	defer e.bridge.mu.Unlock()
	if _, ok := e.bridge.runs[sessionID]; !ok {
		return
	}
	delete(e.bridge.runs, sessionID)
	for i, id := range e.bridge.runOrder {
		if id == sessionID {
			e.bridge.runOrder = append(e.bridge.runOrder[:i], e.bridge.runOrder[i+1:]...)
			break
		}
	}
}

type diagSummary struct {
	name string
	fn   func(context.Context) (any, error)
}

// DiagnosticQuery filters the diagnostics log (ADR-0019 §5). Empty fields do not filter; Limit
// defaults to 200 and is capped at 1000.
type DiagnosticQuery struct {
	// Level is the minimum level: "debug", "info", "warn" or "error". Empty reads every level.
	Level      string    `json:"level,omitempty"`
	SessionID  string    `json:"sessionId,omitempty"`
	RunID      string    `json:"runId,omitempty"`
	JobID      string    `json:"jobId,omitempty"`
	ScheduleID string    `json:"scheduleId,omitempty"`
	Since      time.Time `json:"since,omitempty"`
	Limit      int       `json:"limit,omitempty"`
}

// DiagnosticRecord is one log line, newest first. The correlation fields are the ones the runtime
// attached; Attrs holds the rest of the record's attributes, already redacted when it was written.
type DiagnosticRecord struct {
	Time       time.Time      `json:"time"`
	Level      string         `json:"level"`
	Msg        string         `json:"msg"`
	SessionID  string         `json:"sessionId,omitempty"`
	RunID      string         `json:"runId,omitempty"`
	Step       int64          `json:"step,omitempty"`
	ToolCallID string         `json:"toolCallId,omitempty"`
	JobID      string         `json:"jobId,omitempty"`
	ScheduleID string         `json:"scheduleId,omitempty"`
	Provider   string         `json:"provider,omitempty"`
	Model      string         `json:"model,omitempty"`
	DurationMS int64          `json:"durationMs,omitempty"`
	ErrorCode  string         `json:"errorCode,omitempty"`
	Attrs      map[string]any `json:"attrs,omitempty"`
}

// DiagnosticStats reports the log's counters and files and the event bus's counters. Enabled is
// false when the log could not be opened; Error then says why.
type DiagnosticStats struct {
	Log     diag.Stats `json:"log"`
	Events  EventStats `json:"events"`
	Enabled bool       `json:"enabled"`
	Error   string     `json:"error,omitempty"`
}

// openDiagnostics opens <state>/logs at the configured level. A configuration that does not load
// leaves the level at info: the Engine reports the configuration problem on its own paths.
func openDiagnostics(repoDir, workspace string) (*diag.Log, error) {
	level := slog.LevelInfo
	if cfg, err := config.Load(workspace); err == nil {
		level = parseLogLevel(cfg.Effective().Execution.LogLevel)
	}
	levelVar := new(slog.LevelVar)
	levelVar.Set(level)
	return diag.Open(diag.Options{Dir: filepath.Join(repoDir, "logs"), Level: levelVar})
}

// parseLogLevel maps execution.logLevel to a slog level; an unknown value is info.
func parseLogLevel(name string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// discardLogger is the stand-in when the diagnostics log could not be opened: it never fails and
// writes nothing.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startDiagnosticsBridge subscribes to the event bus and maps its events to log records. It returns
// nil when the log is unavailable: there is nothing to write to.
func (e *Engine) startDiagnosticsBridge() *diagBridge {
	if e.diag == nil || e.log == nil {
		return nil
	}
	events, cancel := e.events.subscribe(diagEventBuffer)
	bridge := &diagBridge{stop: cancel, done: make(chan struct{})}
	go func() {
		defer close(bridge.done)
		ticker := time.NewTicker(eventStatsInterval)
		defer ticker.Stop()
		var lastDropped uint64
		for {
			select {
			case event, ok := <-events:
				if !ok {
					return
				}
				e.logEvent(event)
			case <-ticker.C:
				stats := e.events.stats()
				if stats.Dropped > lastDropped {
					e.log.Warn("events.dropped",
						slog.Uint64("delta", stats.Dropped-lastDropped),
						slog.Uint64("dropped", stats.Dropped),
						slog.Uint64("published", stats.Published))
					lastDropped = stats.Dropped
				}
			}
		}
	}()
	return bridge
}

// closeDiagnostics closes the log. The bridge goroutine is stopped by Close before this runs.
func (e *Engine) closeDiagnostics() error {
	if e.diag == nil {
		return nil
	}
	return e.diag.Close()
}

// correlationKeys maps an event payload key to the correlation field it fills. The first key that
// provides a field wins; the others stay in the record's data.
var correlationKeys = []struct{ from, to string }{
	{"runId", "run_id"},
	{"step", "step"},
	{"toolCallId", "tool_call_id"},
	{"jobId", "job_id"},
	{"scheduleId", "schedule_id"},
	{"provider", "provider"},
	{"model", "model"},
	{"modelRef", "model"},
	{"durationMs", "duration_ms"},
	{"elapsedMs", "duration_ms"},
	{"errorCode", "error_code"},
	{"kind", "error_code"},
}

// logEvent writes one bus event as a log record. The event's payload is redacted by the log; the
// correlation fields are lifted to the record's top level so a query can filter on them.
func (e *Engine) logEvent(event Event) {
	if e.log == nil {
		return
	}
	data := map[string]any{}
	if len(event.Data) > 0 {
		if err := json.Unmarshal(event.Data, &data); err != nil {
			data = map[string]any{"decode_error": err.Error()}
		}
	}
	level := eventLevel(event.Type, data)
	args := make([]any, 0, 2*len(correlationKeys)+6)
	if event.SessionID != "" {
		args = append(args, "session_id", event.SessionID)
	}
	filled := make(map[string]bool, len(correlationKeys))
	for _, key := range correlationKeys {
		if filled[key.to] {
			continue
		}
		value, ok := data[key.from]
		if !ok {
			continue
		}
		delete(data, key.from)
		filled[key.to] = true
		args = append(args, key.to, value)
	}
	if event.Type == "tool.output" {
		// A tool's output chunk never reaches the log: only the stream it came from and its size.
		if chunk, ok := data["chunk"]; ok {
			delete(data, "chunk")
			args = append(args, "bytes", textLen(chunk))
		}
		if stream, ok := data["stream"]; ok {
			delete(data, "stream")
			args = append(args, "stream", stream)
		}
	}
	if !filled["run_id"] {
		// The loop's events carry no run id: the Engine knows which run of the session is live.
		if runID := e.runFor(event.SessionID); runID != "" {
			args = append(args, "run_id", runID)
			filled["run_id"] = true
		}
	}
	if len(data) > 0 {
		args = append(args, "data", data)
	}
	e.log.Log(context.Background(), level, event.Type, args...)
	if event.Type == "run.stopped" {
		e.forgetRun(event.SessionID)
	}
}

// eventLevel maps an event type to a log level: failures and error events are errors, an exit that
// did not complete is a warning, the runtime's state transitions are info and everything else is
// debug.
func eventLevel(eventType string, data map[string]any) slog.Level {
	switch {
	case strings.HasSuffix(eventType, ".failed"), strings.HasSuffix(eventType, ".error"),
		strings.HasSuffix(eventType, "_failed"), eventType == "window.overflow":
		return slog.LevelError
	case eventType == "session.interrupted":
		return slog.LevelWarn
	case eventType == "tool.output":
		return slog.LevelDebug
	case eventType == "tool.end":
		if ok, _ := data["ok"].(bool); !ok {
			return slog.LevelWarn
		}
		return slog.LevelInfo
	case eventType == "job.state":
		switch state, _ := data["state"].(string); state {
		case "failed", "timed_out", "outcome_unknown":
			return slog.LevelWarn
		case "cancelled":
			return slog.LevelWarn
		}
		return slog.LevelInfo
	case eventType == "run.stopped":
		if _, ok := data["error"]; ok {
			return slog.LevelWarn
		}
		return slog.LevelInfo
	}
	switch eventType {
	case "tool.start", "run.started", "schedule.changed", "schedule.fired":
		return slog.LevelInfo
	}
	if strings.HasPrefix(eventType, "session.") || strings.HasPrefix(eventType, "job.") ||
		strings.HasPrefix(eventType, "schedule.") {
		return slog.LevelInfo
	}
	return slog.LevelDebug
}

// textLen is the size of a value as text, for a content field that is reduced to its length.
func textLen(value any) int {
	switch v := value.(type) {
	case string:
		return len(v)
	case []byte:
		return len(v)
	case nil:
		return 0
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	return len(encoded)
}

// DiagnosticLogs returns the newest log records that match q. It reads the files from newest to
// oldest and stops at the limit, so a large log does not have to be loaded.
func (e *Engine) DiagnosticLogs(ctx context.Context, q DiagnosticQuery) ([]DiagnosticRecord, error) {
	if e.diag == nil {
		return nil, e.diagnosticsUnavailable()
	}
	level := slog.LevelDebug
	if q.Level != "" {
		parsed, ok := diag.ParseLevel(q.Level)
		if !ok {
			return nil, fmt.Errorf("diagnostics: unknown level %q", q.Level)
		}
		level = parsed
	}
	records, err := e.diag.Query(ctx, diag.Query{
		MinLevel:   level,
		SessionID:  q.SessionID,
		RunID:      q.RunID,
		JobID:      q.JobID,
		ScheduleID: q.ScheduleID,
		Since:      q.Since,
		Limit:      q.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]DiagnosticRecord, 0, len(records))
	for _, record := range records {
		out = append(out, diagnosticRecord(record))
	}
	return out, nil
}

// diagnosticRecord lifts a record's correlation attributes into the API's camelCase fields.
func diagnosticRecord(record diag.Record) DiagnosticRecord {
	out := DiagnosticRecord{Time: record.Time, Level: record.Level, Msg: record.Msg, Attrs: record.Attrs}
	take := func(key string) any {
		value, ok := out.Attrs[key]
		if !ok {
			return nil
		}
		delete(out.Attrs, key)
		return value
	}
	out.SessionID, _ = take("session_id").(string)
	out.RunID, _ = take("run_id").(string)
	out.ToolCallID, _ = take("tool_call_id").(string)
	out.JobID, _ = take("job_id").(string)
	out.ScheduleID, _ = take("schedule_id").(string)
	out.Provider, _ = take("provider").(string)
	out.Model, _ = take("model").(string)
	out.ErrorCode, _ = take("error_code").(string)
	switch step := take("step").(type) {
	case float64:
		out.Step = int64(step)
	case int64:
		out.Step = step
	case int:
		out.Step = int64(step)
	}
	switch duration := take("duration_ms").(type) {
	case float64:
		out.DurationMS = int64(duration)
	case int64:
		out.DurationMS = duration
	case int:
		out.DurationMS = int64(duration)
	}
	if len(out.Attrs) == 0 {
		out.Attrs = nil
	}
	return out
}

// DiagnosticStats reports the diagnostics log and the event bus. It never fails: a log that could
// not be opened reports Enabled false and the reason.
func (e *Engine) DiagnosticStats() DiagnosticStats {
	out := DiagnosticStats{Events: e.events.stats(), Enabled: e.diag != nil}
	if e.diag != nil {
		out.Log = e.diag.Stats()
	}
	if e.diagErr != nil {
		out.Error = e.diagErr.Error()
	}
	return out
}

// RegisterDiagnosticSummary adds a section to the manifest ExportDiagnostics writes. The job manager
// and the scheduler register their summaries here, so an exported log names the workspace's jobs
// and scheduled tasks without their commands or output. A section whose function fails is reported
// with its error; it never fails the export. Registering on an Engine whose log could not be opened
// does nothing, because the export is unavailable too.
func (e *Engine) RegisterDiagnosticSummary(name string, fn func(context.Context) (any, error)) {
	if e.bridge == nil || name == "" || fn == nil {
		return
	}
	e.bridge.mu.Lock()
	defer e.bridge.mu.Unlock()
	e.bridge.summaries = append(e.bridge.summaries, diagSummary{name: name, fn: fn})
}

// ExportDiagnostics writes a zip of the redacted logs and a manifest to dest and returns its path.
// dest may be a directory, in which case the file is named lumencortex-diagnostics-<UTC>.zip, or a
// file path. The file is created 0600; a partial file is removed when the export fails.
func (e *Engine) ExportDiagnostics(ctx context.Context, dest string) (string, error) {
	if e.diag == nil {
		return "", e.diagnosticsUnavailable()
	}
	if strings.TrimSpace(dest) == "" {
		return "", errors.New("diagnostics: no export destination")
	}
	path := dest
	info, err := os.Stat(path)
	switch {
	case err == nil && info.IsDir():
		path = filepath.Join(path, fmt.Sprintf("lumencortex-diagnostics-%s.zip",
			time.Now().UTC().Format("20060102T150405Z")))
	case err == nil:
		// An existing file is overwritten.
	case errors.Is(err, os.ErrNotExist):
		// A destination that does not exist names the file to create.
	default:
		return "", fmt.Errorf("diagnostics: destination %s: %w", dest, err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("diagnostics: create %s: %w", path, err)
	}
	if err := e.diag.Export(ctx, file, e.diagnosticsManifest(ctx)); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("diagnostics: write %s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path, nil
	}
	return abs, nil
}

// diagnosticsManifest is what the export says about the workspace. It names the workspace by its
// base name and a hash of the path: an exported log is shared with someone else.
func (e *Engine) diagnosticsManifest(ctx context.Context) map[string]any {
	health := e.Health()
	sum := sha256.Sum256([]byte(health.Workspace))
	manifest := map[string]any{
		"coreVersion":   GoRuntimePreviewVersion,
		"workspace":     filepath.Base(health.Workspace),
		"workspaceHash": hex.EncodeToString(sum[:8]),
		"os":            goruntime.GOOS,
		"arch":          goruntime.GOARCH,
		"createdAt":     time.Now().UTC(),
		"stats":         e.DiagnosticStats(),
		"health": map[string]any{
			"version":      health.Version,
			"database":     filepath.Base(health.Database),
			"budget":       health.Budget,
			"usedBytes":    health.UsedBytes,
			"activeAgents": health.ActiveAgents,
			"pressure":     health.Pressure,
		},
	}
	if e.bridge != nil {
		if sections := e.diagnosticSummaries(ctx); len(sections) > 0 {
			manifest["summaries"] = sections
		}
	}
	return manifest
}

// diagnosticSummaries runs the registered sections. A section that fails is reported with its
// error; one failing section never hides the others or the logs.
func (e *Engine) diagnosticSummaries(ctx context.Context) map[string]any {
	e.bridge.mu.Lock()
	summaries := make([]diagSummary, len(e.bridge.summaries))
	copy(summaries, e.bridge.summaries)
	e.bridge.mu.Unlock()
	sections := make(map[string]any, len(summaries))
	for _, summary := range summaries {
		value, err := summary.fn(ctx)
		if err != nil {
			sections[summary.name] = map[string]any{"error": err.Error()}
			continue
		}
		sections[summary.name] = value
	}
	return sections
}

// diagnosticsUnavailable is the error the API returns when the log could not be opened.
func (e *Engine) diagnosticsUnavailable() error {
	if e.diagErr != nil {
		return fmt.Errorf("diagnostics: log unavailable: %w", e.diagErr)
	}
	return errors.New("diagnostics: log unavailable")
}
