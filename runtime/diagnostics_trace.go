package runtime

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/internal/diag"
)

// Trace span kinds, as DiagnosticTrace reports them.
const (
	TraceRun        = "run"
	TraceRequest    = "request"
	TraceTool       = "tool"
	TraceJob        = "job"
	TraceCompletion = "completion"
)

// Trace span statuses. "running" marks a span whose end never reached the log: the run is still
// going, or the log rotated away the record, or the process died.
const (
	TraceRunning     = "running"
	TraceOK          = "ok"
	TraceError       = "error"
	TraceDenied      = "denied"
	TraceInterrupted = "interrupted"
	TraceCancelled   = "cancelled"
	TraceTimedOut    = "timed_out"
)

// TraceSpan is one interval of a session's execution, built from the diagnostics log: a run, one
// model request, one tool call, one background job or the run's completion. It is a view, not new
// state: the log is the only source, so a span is only as complete as the records kept.
//
// ParentID links a request, tool, job or completion span to its run's span id.
type TraceSpan struct {
	ID         string     `json:"id"`
	ParentID   string     `json:"parentId,omitempty"`
	Kind       string     `json:"kind"`
	Name       string     `json:"name,omitempty"`
	SessionID  string     `json:"sessionId,omitempty"`
	RunID      string     `json:"runId,omitempty"`
	Step       int64      `json:"step,omitempty"`
	ToolCallID string     `json:"toolCallId,omitempty"`
	JobID      string     `json:"jobId,omitempty"`
	Provider   string     `json:"provider,omitempty"`
	Model      string     `json:"model,omitempty"`
	StartedAt  time.Time  `json:"startedAt"`
	EndedAt    *time.Time `json:"endedAt,omitempty"`
	DurationMS int64      `json:"durationMs,omitempty"`
	Status     string     `json:"status"`
	ErrorCode  string     `json:"errorCode,omitempty"`
	TimedOut   bool       `json:"timedOut,omitempty"`
}

// DiagnosticTrace builds the spans of one run from the diagnostics log: the run, its model requests,
// its tool calls, the jobs it started and how it ended, ordered by start time. sessionID is
// required; an empty runID covers every run of the session the log still holds.
//
// The view is bounded: it reads at most diag.MaxScanLimit records, so a very long run shows its
// newest spans. Records the log dropped are not invented: a span whose end is missing stays
// "running".
func (e *Engine) DiagnosticTrace(ctx context.Context, sessionID, runID string) ([]TraceSpan, error) {
	if e.diag == nil {
		return nil, e.diagnosticsUnavailable()
	}
	if sessionID == "" {
		return nil, errTraceNoSession
	}
	records, err := e.diag.Scan(ctx, diag.Query{
		MinLevel:  slog.LevelDebug,
		SessionID: sessionID,
		RunID:     runID,
	}, diag.MaxScanLimit)
	if err != nil {
		return nil, err
	}
	// The reader returns newest first; a trace reads forwards.
	for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
		records[i], records[j] = records[j], records[i]
	}
	return buildTrace(records, sessionID), nil
}

// errTraceNoSession is returned by DiagnosticTrace without a session.
var errTraceNoSession = errors.New("diagnostics: a trace needs a session id")

// traceBuilder accumulates spans while the records are read in order.
type traceBuilder struct {
	spans []*TraceSpan
	// byID finds a span to close; a log that lost the opening record leaves a span unopened, and a
	// closing record then only contributes nothing.
	byID map[string]*TraceSpan
	// requests and tools are the spans currently open, keyed by the identity the log carries.
	requests map[string]*TraceSpan
	tools    map[string]*TraceSpan
	jobs     map[string]*TraceSpan
	run      *TraceSpan
}

// buildTrace turns chronological records into spans.
func buildTrace(records []diag.Record, sessionID string) []TraceSpan {
	b := &traceBuilder{
		byID:     map[string]*TraceSpan{},
		requests: map[string]*TraceSpan{},
		tools:    map[string]*TraceSpan{},
		jobs:     map[string]*TraceSpan{},
	}
	for _, record := range records {
		b.apply(record, sessionID)
	}
	sort.SliceStable(b.spans, func(i, j int) bool {
		if b.spans[i].StartedAt.Equal(b.spans[j].StartedAt) {
			return b.spans[i].ID < b.spans[j].ID
		}
		return b.spans[i].StartedAt.Before(b.spans[j].StartedAt)
	})
	out := make([]TraceSpan, 0, len(b.spans))
	for _, span := range b.spans {
		out = append(out, *span)
	}
	return out
}

// add opens a span and records it.
func (b *traceBuilder) add(span *TraceSpan) *TraceSpan {
	b.spans = append(b.spans, span)
	b.byID[span.ID] = span
	return span
}

// close marks a span ended; a second close keeps the first.
func closeSpan(span *TraceSpan, at time.Time, status, errorCode string, timedOut bool) {
	if span.EndedAt != nil {
		return
	}
	ended := at
	span.EndedAt = &ended
	if !span.StartedAt.IsZero() && !at.Before(span.StartedAt) {
		span.DurationMS = at.Sub(span.StartedAt).Milliseconds()
	}
	if status != "" {
		span.Status = status
	}
	if errorCode != "" {
		span.ErrorCode = errorCode
	}
	if timedOut {
		span.TimedOut = true
		if span.Status != TraceError {
			span.Status = TraceTimedOut
		}
	}
}

// parentID is the run span's id when one is known.
func (b *traceBuilder) parentID(runID string) string {
	if b.run != nil && b.run.ID != "" {
		return b.run.ID
	}
	if runID != "" {
		return "run:" + runID
	}
	return ""
}

// apply folds one record into the spans.
func (b *traceBuilder) apply(record diag.Record, sessionID string) {
	attrs := record.Attrs
	runID := attrText(attrs, "run_id")
	step := attrInt(attrs, "step")
	toolCallID := attrText(attrs, "tool_call_id")
	jobID := attrText(attrs, "job_id")
	errorCode := attrText(attrs, "error_code")

	switch record.Msg {
	case "run.started":
		span := &TraceSpan{
			ID:        runSpanID(runID, sessionID),
			Kind:      TraceRun,
			SessionID: sessionID,
			RunID:     runID,
			StartedAt: record.Time,
			Status:    TraceRunning,
		}
		b.run = b.add(span)
	case "run.stopped":
		if b.run == nil {
			return
		}
		status := TraceOK
		if nestedHas(attrs, "data", "error") {
			status = TraceError
		}
		if b.run.Status != TraceRunning {
			// The terminal session event already said how the run ended; it is the better answer.
			status = b.run.Status
		}
		closeSpan(b.run, record.Time, status, errorCode, false)
	case "llm.request":
		base := requestSpanID(runID, sessionID, step)
		id := base
		for n := 1; b.byID[id] != nil; n++ {
			id = base + "#" + itoa(int64(n))
		}
		span := &TraceSpan{
			ID:        id,
			ParentID:  b.parentID(runID),
			Kind:      TraceRequest,
			Name:      attrText(attrs, "modelRef"),
			SessionID: sessionID,
			RunID:     runID,
			Step:      step,
			Provider:  attrText(attrs, "provider"),
			Model:     attrText(attrs, "model"),
			StartedAt: record.Time,
			Status:    TraceRunning,
		}
		b.requests[id] = b.add(span)
	case "llm.complete":
		span := b.openRequest(runID, requestSpanID(runID, sessionID, step))
		if span == nil {
			return
		}
		delete(b.requests, span.ID)
		closeSpan(span, record.Time, TraceOK, "", false)
	case "tool.start":
		id := toolSpanID(toolCallID, runID, sessionID, step)
		span := &TraceSpan{
			ID:         id,
			ParentID:   b.parentID(runID),
			Kind:       TraceTool,
			Name:       attrText(attrs, "name"),
			SessionID:  sessionID,
			RunID:      runID,
			Step:       step,
			ToolCallID: toolCallID,
			StartedAt:  record.Time,
			Status:     TraceRunning,
		}
		b.tools[id] = b.add(span)
	case "tool.end":
		id := toolSpanID(toolCallID, runID, sessionID, step)
		span := b.tools[id]
		if span == nil {
			span = b.lastOpen(b.tools, runID)
		}
		if span == nil {
			return
		}
		delete(b.tools, span.ID)
		status := TraceOK
		switch {
		case boolAttr(attrs, "timedOut"):
			status = TraceTimedOut
		case boolAttr(attrs, "denied"):
			status = TraceDenied
		case hasKey(attrs, "ok") && !boolAttr(attrs, "ok"):
			status = TraceError
		}
		if code := errorCode; code != "" {
			span.ErrorCode = code
		}
		closeSpan(span, record.Time, status, "", boolAttr(attrs, "timedOut"))
	case "job.state":
		state := nestedText(attrs, "data", "state")
		span := b.jobs[jobID]
		if span == nil {
			if jobID == "" {
				return
			}
			span = &TraceSpan{
				ID:        "job:" + jobID,
				ParentID:  b.parentID(runID),
				Kind:      TraceJob,
				Name:      nestedText(attrs, "data", "source"),
				SessionID: sessionID,
				RunID:     runID,
				JobID:     jobID,
				StartedAt: record.Time,
				Status:    TraceRunning,
			}
			b.jobs[jobID] = b.add(span)
		}
		if span.ParentID == "" {
			span.ParentID = b.parentID(runID)
		}
		if span.RunID == "" {
			span.RunID = runID
		}
		switch state {
		case "completed":
			closeSpan(span, record.Time, TraceOK, "", false)
			delete(b.jobs, jobID)
		case "failed":
			closeSpan(span, record.Time, TraceError, "", false)
			delete(b.jobs, jobID)
		case "timed_out":
			closeSpan(span, record.Time, TraceTimedOut, "", true)
			delete(b.jobs, jobID)
		case "cancelled":
			closeSpan(span, record.Time, TraceCancelled, "", false)
			delete(b.jobs, jobID)
		case "outcome_unknown":
			closeSpan(span, record.Time, TraceError, "outcome_unknown", false)
			delete(b.jobs, jobID)
		}
	case "session.complete", "session.interrupted", "session.failed":
		span := &TraceSpan{
			ID:        "completion:" + record.Msg + ":" + runID + ":" + record.Time.UTC().Format(time.RFC3339Nano),
			ParentID:  b.parentID(runID),
			Kind:      TraceCompletion,
			Name:      record.Msg,
			SessionID: sessionID,
			RunID:     runID,
			StartedAt: record.Time,
			Status:    completionStatus(record.Msg, attrs),
		}
		if errorCode != "" {
			span.ErrorCode = errorCode
		}
		ended := record.Time
		span.EndedAt = &ended
		b.add(span)
		if b.run != nil {
			// The terminal event is the run's outcome, even before run.stopped arrives.
			b.run.Status = span.Status
			if span.ErrorCode != "" {
				b.run.ErrorCode = span.ErrorCode
			}
		}
	}
}

// completionStatus maps a terminal session event to a span status.
func completionStatus(msg string, attrs map[string]any) string {
	switch msg {
	case "session.complete":
		return TraceOK
	case "session.interrupted":
		if attrText(attrs, "kind") == "cancelled" {
			return TraceCancelled
		}
		return TraceInterrupted
	default:
		return TraceError
	}
}

// lastOpen returns the newest still-open span of a map, for a closing record whose key does not
// match (a log that dropped the opening record's attributes). Only spans of the same run are
// considered: a fallback must never close another run's span.
func (b *traceBuilder) lastOpen(spans map[string]*TraceSpan, runID string) *TraceSpan {
	var newest *TraceSpan
	for _, span := range spans {
		if runID != "" && span.RunID != runID {
			continue
		}
		if newest == nil || span.StartedAt.After(newest.StartedAt) {
			newest = span
		}
	}
	return newest
}

// runSpanID names the run span: the run id when known.
func runSpanID(runID, sessionID string) string {
	if runID != "" {
		return "run:" + runID
	}
	return "run:" + sessionID
}

// requestSpanID names a request span. A closing record carries the same step, so the id is stable.
func requestSpanID(runID, sessionID string, step int64) string {
	return runSpanID(runID, sessionID) + ":request:" + itoa(step)
}

// openRequest finds the request span a completion closes: the one of this step, else the oldest open
// request of the same run, so a log that lost the request's step still closes something.
func (b *traceBuilder) openRequest(runID, id string) *TraceSpan {
	if span, ok := b.requests[id]; ok {
		return span
	}
	var oldest *TraceSpan
	for key, span := range b.requests {
		if !strings.HasPrefix(key, id+"#") {
			continue
		}
		if oldest == nil || span.StartedAt.Before(oldest.StartedAt) {
			oldest = span
		}
	}
	if oldest == nil {
		oldest = b.lastOpen(b.requests, runID)
	}
	return oldest
}

// toolSpanID names a tool span by the call it executes.
func toolSpanID(toolCallID, runID, sessionID string, step int64) string {
	if toolCallID != "" {
		return "tool:" + toolCallID
	}
	return runSpanID(runID, sessionID) + ":tool:" + itoa(step)
}

// itoa formats an int64 without importing strconv at every call site.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// attrText reads a string attribute.
func attrText(attrs map[string]any, key string) string {
	if attrs == nil {
		return ""
	}
	s, _ := attrs[key].(string)
	return s
}

// nestedHas reports whether a nested object attribute holds a key.
func nestedHas(attrs map[string]any, group, key string) bool {
	if attrs == nil {
		return false
	}
	inner, ok := attrs[group].(map[string]any)
	if !ok {
		return false
	}
	return hasKey(inner, key)
}

// nestedText reads a string field of a nested object attribute.
func nestedText(attrs map[string]any, group, key string) string {
	if attrs == nil {
		return ""
	}
	inner, ok := attrs[group].(map[string]any)
	if !ok {
		return ""
	}
	return attrText(inner, key)
}

// attrInt reads a numeric attribute.
func attrInt(attrs map[string]any, key string) int64 {
	if attrs == nil {
		return 0
	}
	switch v := attrs[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

// boolAttr reads a boolean attribute.
func boolAttr(attrs map[string]any, key string) bool {
	if attrs == nil {
		return false
	}
	b, _ := attrs[key].(bool)
	return b
}

// hasKey reports whether an attribute is present at all, whatever its value.
func hasKey(attrs map[string]any, key string) bool {
	if attrs == nil {
		return false
	}
	_, ok := attrs[key]
	return ok
}
