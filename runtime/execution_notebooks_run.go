package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/internal/ipynb"
)

// executionRunState is one run of one notebook: what it runs and when it started.
type executionRunState struct {
	CellID    string
	Replay    bool
	StartedAt time.Time
	// KernelID and RequestID name the execution the run waits for right now, so an interrupt of the
	// notebook stops exactly that cell and nothing else that runs on the same kernel.
	KernelID  string
	RequestID string
}

// executionRuns is the run in flight per notebook. A notebook runs one thing at a time, and a second
// run of the same notebook is refused rather than queued: a page that lost its running state (a route
// switch, a reload, a second click) must not start a duplicate run, and the Cancel a user clicks must
// reach the run that is actually executing. The registry is also what ExecutionNotebookRuns reports,
// so a page that opens while a run is in flight shows that run instead of guessing from its own state.
var executionRuns = struct {
	mu   sync.Mutex
	held map[string]executionRunState
}{held: map[string]executionRunState{}}

// executionRunKey is the registry key of one notebook of one session.
func executionRunKey(sessionID, notebookID string) string { return sessionID + "\x00" + notebookID }

// beginExecutionRun claims the notebook's run slot. It reports the run already in flight, if any, so
// the caller can say which run refused it.
func beginExecutionRun(sessionID, notebookID, cellID string, replay bool) (executionRunState, bool) {
	key := executionRunKey(sessionID, notebookID)
	executionRuns.mu.Lock()
	defer executionRuns.mu.Unlock()
	if held, busy := executionRuns.held[key]; busy {
		return held, false
	}
	executionRuns.held[key] = executionRunState{CellID: cellID, Replay: replay, StartedAt: time.Now().UTC()}
	return executionRunState{}, true
}

// setExecutionRunTarget records the execution the notebook's run is about to wait for.
func setExecutionRunTarget(sessionID, notebookID, kernelID, requestID string) {
	key := executionRunKey(sessionID, notebookID)
	executionRuns.mu.Lock()
	defer executionRuns.mu.Unlock()
	if held, ok := executionRuns.held[key]; ok {
		held.KernelID, held.RequestID = kernelID, requestID
		executionRuns.held[key] = held
	}
}

// executionRunTarget returns the execution the notebook's run waits for, if any.
func executionRunTarget(sessionID, notebookID string) (kernelID, requestID string) {
	executionRuns.mu.Lock()
	defer executionRuns.mu.Unlock()
	held := executionRuns.held[executionRunKey(sessionID, notebookID)]
	return held.KernelID, held.RequestID
}

// endExecutionRun releases the notebook's run slot.
func endExecutionRun(sessionID, notebookID string) {
	key := executionRunKey(sessionID, notebookID)
	executionRuns.mu.Lock()
	delete(executionRuns.held, key)
	executionRuns.mu.Unlock()
}

// executionNotebookRuns lists the runs in flight in this process for one session, oldest first.
func executionNotebookRuns(sessionID string) []ExecutionNotebookRun {
	prefix := sessionID + "\x00"
	executionRuns.mu.Lock()
	defer executionRuns.mu.Unlock()
	out := make([]ExecutionNotebookRun, 0, len(executionRuns.held))
	for key, held := range executionRuns.held {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		out = append(out, ExecutionNotebookRun{
			NotebookID: strings.TrimPrefix(key, prefix),
			CellID:     held.CellID, Replay: held.Replay, StartedAt: held.StartedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// ExecutionNotebookBusyError reports a run that did not start because the notebook is already
// running. Nothing was queued and nothing was executed: the caller waits for the run in flight, or
// interrupts it, and then runs again.
type ExecutionNotebookBusyError struct {
	NotebookID string
	// CellID is the cell the run in flight executes, empty for a whole-notebook run.
	CellID    string
	StartedAt time.Time
}

func (e *ExecutionNotebookBusyError) Error() string {
	target := "the notebook"
	if e.CellID != "" {
		target = fmt.Sprintf("cell %s of the notebook", e.CellID)
	}
	return fmt.Sprintf("%s is already running (started %s): wait for it to finish, or interrupt it, instead of queueing a second run",
		target, e.StartedAt.UTC().Format(time.RFC3339))
}

// run implements RunExecutionCell (cellID set) and RunExecutionNotebook (cellID empty).
func (h *executionNotebookHost) run(ctx context.Context, sessionID, ref, cellID string, replay bool) (ExecutionNotebook, error) {
	stored, err := h.stored(ctx, sessionID, ref)
	if err != nil {
		return ExecutionNotebook{}, err
	}
	if held, ok := beginExecutionRun(sessionID, stored.ID, cellID, replay); !ok {
		return ExecutionNotebook{}, &ExecutionNotebookBusyError{NotebookID: stored.ID, CellID: held.CellID, StartedAt: held.StartedAt}
	}
	defer endExecutionRun(sessionID, stored.ID)

	// Read again now that this run owns the notebook: the document it starts from is the one a
	// previous run finished writing.
	stored, err = h.stored(ctx, sessionID, stored.ID)
	if err != nil {
		return ExecutionNotebook{}, err
	}
	document, err := ipynb.Parse(stored.Body)
	if err != nil {
		return ExecutionNotebook{}, fmt.Errorf("execution notebook %s: %w", stored.ID, err)
	}
	notebookID := stored.ID
	latest := executionNotebookOf(stored, document)

	if replay {
		for _, language := range document.Languages() {
			if err := h.controlLanguage(ctx, sessionID, language, "close"); err != nil {
				return ExecutionNotebook{}, err
			}
		}
		// Write the clean document first: a run that dies halfway leaves the results of this run, never
		// a mixture with the previous one.
		latest, err = h.mutate(ctx, sessionID, notebookID, -1, func(next *ipynb.Document) error {
			ipynb.ResetExecution(next)
			return nil
		})
		if err != nil {
			return ExecutionNotebook{}, err
		}
		document = latest.Document
	}

	targets := make([]string, 0, len(document.Cells))
	for _, cell := range document.Cells {
		if cell.CellType != ipynb.CellCode {
			continue
		}
		if cellID != "" && cell.ID != cellID {
			continue
		}
		targets = append(targets, cell.ID)
	}
	if cellID != "" && len(targets) == 0 {
		return ExecutionNotebook{}, fmt.Errorf("the notebook holds no code cell %q", cellID)
	}

	kernels := make(map[string]evalapi.Session, 2)
	for _, id := range targets {
		cell, ok := document.Find(id)
		if !ok {
			// The cell was deleted while the run was in flight; there is nothing to execute.
			continue
		}
		language := document.CellLanguage(cell)
		if language == "" {
			latest, err = h.mutate(ctx, sessionID, notebookID, -1, func(next *ipynb.Document) error {
				markCellFailure(next, id, "a cell needs a language before it can run")
				return nil
			})
			if err != nil {
				return ExecutionNotebook{}, err
			}
			// The cell never started, but the page shows it as running from the event that announced
			// it: every path that ends a cell's turn publishes how it ended.
			h.publishCell(ctx, sessionID, notebookID, id, "", ipynb.StatusError)
			break
		}
		kernel, ok := kernels[language]
		if !ok {
			kernel, err = h.kernel(ctx, sessionID, language)
			if err != nil {
				recorded, recErr := h.mutate(ctx, sessionID, notebookID, -1, func(next *ipynb.Document) error {
					markCellFailure(next, id, err.Error())
					return nil
				})
				if recErr != nil {
					return ExecutionNotebook{}, recErr
				}
				h.publishCell(ctx, sessionID, notebookID, id, language, ipynb.StatusError)
				return recorded, err
			}
			kernels[language] = kernel
		}
		requestID, err := newEvalRequestID()
		if err != nil {
			return ExecutionNotebook{}, err
		}
		setExecutionRunTarget(sessionID, notebookID, kernel.ID, requestID)
		startedAt := time.Now().UTC()
		h.publishCell(ctx, sessionID, notebookID, id, language, ipynb.StatusRunning)
		result, execErr := h.exec.execute(ctx, sessionID, evalapi.Request{
			ID:        requestID,
			SessionID: kernel.ID,
			CellID:    id,
			Language:  language,
			Code:      string(cell.Source),
		})
		if execErr != nil {
			status := ipynb.StatusError
			if ctx.Err() != nil || errors.Is(execErr, context.Canceled) {
				status = ipynb.StatusCancelled
			}
			recorded, recErr := h.mutate(ctx, sessionID, notebookID, -1, func(next *ipynb.Document) error {
				markCellOutcome(next, id, language, kernel, startedAt, status, execErr.Error(), nil, nil, nil)
				return nil
			})
			if recErr != nil {
				return ExecutionNotebook{}, recErr
			}
			// A failed execution still ends the cell: without this event the page keeps the cell at
			// running until something else refreshes it.
			h.publishCell(ctx, sessionID, notebookID, id, language, cellStatus(recorded.Document, id))
			return recorded, execErr
		}
		produced, artifactRefs, releaseArtifacts := h.persistResultArtifacts(ctx, sessionID, kernel.ID, &result)
		latest, err = h.mutate(ctx, sessionID, notebookID, -1, func(next *ipynb.Document) error {
			markCellOutcome(next, id, language, kernel, startedAt, "", "", &result, artifactRefs, produced)
			return nil
		})
		// The output that references the artifacts is written: a collection may now decide about them.
		releaseArtifacts()
		if err != nil {
			return ExecutionNotebook{}, err
		}
		for _, reference := range artifactRefs {
			payload := map[string]any{"notebookId": notebookID, "cellId": id, "artifactId": reference.ArtifactID, "mime": reference.MIME, "bytes": reference.Size, "sha256": reference.SHA256}
			if reference.NotDurable {
				payload["evalSessionId"] = reference.SessionID
			}
			h.engine.events.publish("execution_notebook.artifact", sessionID, payload)
		}
		document = latest.Document
		status := cellStatus(document, id)
		h.publishCell(ctx, sessionID, notebookID, id, language, status)
		if status != ipynb.StatusOK {
			break
		}
	}
	return latest, nil
}

// kernel returns the kernel of one language, creating it when the session has none. A kernel that is
// stopped, failed or disposed is not reused: the next run starts a clean one.
func (h *executionNotebookHost) kernel(ctx context.Context, sessionID, language string) (evalapi.Session, error) {
	if strings.TrimSpace(language) == "" {
		return evalapi.Session{}, errors.New("a cell needs a language before it can run")
	}
	sessions, sessionErr := h.exec.sessions(ctx, sessionID)
	if sessionErr == nil {
		if live, ok := currentKernel(ownedKernels(sessions, sessionID), language); ok {
			return live, nil
		}
	}
	runtimeID, err := h.runtimeID(ctx, language)
	if err != nil {
		if sessionErr != nil {
			return evalapi.Session{}, sessionErr
		}
		return evalapi.Session{}, err
	}
	return h.exec.create(ctx, sessionID, runtimeID)
}

// ownedKernels is the kernels of one session. A kernel executes for its immutable owner and for nobody
// else, and a kernel another agent shared with this session (an observer grant) is not this session's
// to run, reset or close: Core lists the kernels a session owns, and the owner check here keeps the
// notebook on the kernels it created even if a grant ever reaches that list.
func ownedKernels(sessions []evalapi.Session, sessionID string) []evalapi.Session {
	out := make([]evalapi.Session, 0, len(sessions))
	for _, value := range sessions {
		if value.OwnerID != "" && value.OwnerID != sessionID {
			continue
		}
		out = append(out, value)
	}
	return out
}

// liveKernels returns every kernel of one language that can execute, oldest first.
func liveKernels(sessions []evalapi.Session, language string) []evalapi.Session {
	out := make([]evalapi.Session, 0, len(sessions))
	for _, value := range sessions {
		if !strings.EqualFold(strings.TrimSpace(value.Language), language) || !kernelAlive(value.State) {
			continue
		}
		out = append(out, value)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// currentKernel is the live kernel of one language: the newest one by creation time. Generation counts
// restarts of one kernel and never orders two kernels, so it is not used here: a stopped kernel that
// was reset several times carries a higher generation than the kernel that is alive now, and choosing
// by generation sends a run, a reset or an interrupt to a kernel that is gone.
func currentKernel(sessions []evalapi.Session, language string) (evalapi.Session, bool) {
	live := liveKernels(sessions, language)
	if len(live) == 0 {
		return evalapi.Session{}, false
	}
	return live[len(live)-1], true
}

// kernelAlive reports whether a kernel can execute. A kernel that is stopped, failed or disposed is
// never reused: the next run starts a clean one. Every other state is usable, because a kernel that is
// starting or being reset accepts the next cell and refusing it would be worse than trying.
func kernelAlive(state string) bool {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case evalapi.StateStopped, evalapi.StateFailed, evalapi.StateDisposed:
		return false
	}
	return true
}

// kernels implements ExecutionKernels. The list is oldest first, so the current kernel of a language
// is the last live one.
func (h *executionNotebookHost) kernels(ctx context.Context, sessionID string) ([]ExecutionKernel, error) {
	sessions, err := h.exec.sessions(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	sessions = ownedKernels(sessions, sessionID)
	out := make([]ExecutionKernel, 0, len(sessions))
	for _, value := range sessions {
		out = append(out, ExecutionKernel{
			Language: value.Language, RuntimeID: value.RuntimeID, SessionID: value.ID,
			State: value.State, Generation: value.Generation, Alive: kernelAlive(value.State),
			CreatedAt: value.CreatedAt,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// runs implements ExecutionNotebookRuns.
func (h *executionNotebookHost) runs(ctx context.Context, sessionID string) ([]ExecutionNotebookRun, error) {
	if _, err := h.engine.store.Get(ctx, sessionID); err != nil {
		return nil, err
	}
	return executionNotebookRuns(sessionID), nil
}

// controlLanguage implements ResetExecutionKernel, CloseExecutionKernel and the replay's clean start.
// Every live kernel of the language is controlled, never the one with the highest generation: a
// stopped kernel cannot be reset or interrupted, and a live one must not be skipped because an older
// kernel counted more restarts. A language with no live kernel is a no-op: the next run creates one.
func (h *executionNotebookHost) controlLanguage(ctx context.Context, sessionID, language, action string) error {
	sessions, err := h.exec.sessions(ctx, sessionID)
	if err != nil {
		return err
	}
	var firstErr error
	for _, kernel := range liveKernels(ownedKernels(sessions, sessionID), language) {
		if err := h.exec.control(ctx, sessionID, kernel.ID, action); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// interrupt implements InterruptExecutionNotebook.
func (h *executionNotebookHost) interrupt(ctx context.Context, sessionID, ref string) error {
	stored, err := h.stored(ctx, sessionID, ref)
	if err != nil {
		return err
	}
	kernelID, requestID := executionRunTarget(sessionID, stored.ID)
	if requestID == "" {
		// The notebook runs nothing: there is nothing of it to stop.
		return nil
	}
	_, err = h.exec.interrupt(ctx, sessionID, kernelID, evalapi.InterruptTarget{RequestID: requestID})
	return err
}

// artifact implements ExecutionArtifact.
func (h *executionNotebookHost) artifact(ctx context.Context, sessionID, evalSessionID, artifactID string) (ExecutionArtifact, error) {
	meta, data, err := h.exec.artifact(ctx, sessionID, evalSessionID, artifactID)
	if err != nil {
		return ExecutionArtifact{}, err
	}
	if len(data) > ExecutionArtifactMaxBytes {
		return ExecutionArtifact{}, fmt.Errorf("artifact %s is %d bytes, more than the %d byte limit", artifactID, len(data), ExecutionArtifactMaxBytes)
	}
	return ExecutionArtifact{
		ArtifactID: meta.ID, SessionID: meta.SessionID, CellID: meta.CellID, MIME: meta.MIME,
		Size: meta.Size, SHA256: meta.SHA256, CreatedAt: meta.CreatedAt, Data: data,
	}, nil
}

// mutate applies one change as a single compare-and-set-free write: it reads the current document
// inside the write transaction and applies the change to it, so a result never overwrites a cell edit
// that committed while a run was in flight. A document that no longer fits is fitted by ipynb.Fit.
func (h *executionNotebookHost) mutate(ctx context.Context, sessionID, notebookID string, expectedVersion int64, apply func(*ipynb.Document) error) (ExecutionNotebook, error) {
	var document ipynb.Document
	updated, err := h.engine.store.UpdateNotebook(ctx, sessionID, notebookID, "host", "replace", expectedVersion,
		func(current string) (string, error) {
			next, err := ipynb.Parse(current)
			if err != nil {
				return "", err
			}
			if err := apply(&next); err != nil {
				return "", err
			}
			// Normalize what the edit produced: a new code cell takes the document's default language,
			// its outputs are bounded, and a markdown cell keeps no output.
			if err := ipynb.Normalize(&next); err != nil {
				return "", err
			}
			body, err := ipynb.Fit(&next)
			if err != nil {
				return "", err
			}
			document = next
			return body, nil
		})
	if err != nil {
		return ExecutionNotebook{}, err
	}
	h.engine.notebookUpdated(ctx, sessionID, updated)
	return executionNotebookOf(updated, document), nil
}

// markCellFailure records a host-side failure on one cell: the cell is at status error with the
// reason, and it keeps the outputs it had.
func markCellFailure(document *ipynb.Document, cellID, reason string) {
	index := document.Index(cellID)
	if index < 0 {
		return
	}
	cell := &document.Cells[index]
	if cell.Metadata.LC == nil {
		cell.Metadata.LC = &ipynb.CellLC{}
	}
	cell.Metadata.LC.Status = ipynb.StatusError
	cell.Outputs = []ipynb.Output{{OutputType: ipynb.OutputError, EName: "ExecutionError", EValue: reason}}
}

// markCellOutcome records the outcome of one execution on its cell: status, timing, kernel identity,
// outputs, execution count and the history entry. A result carries the execution runtime's own view;
// the notebook stores it and derives nothing about the kernel's heap. refs is the reference of every
// artifact this execution stored, keyed by the artifact id its display names.
func markCellOutcome(document *ipynb.Document, cellID, language string, kernel evalapi.Session, startedAt time.Time, status, reason string, result *evalapi.Result, refs map[string]ipynb.OutputLC, produced []evalapi.Artifact) {
	index := document.Index(cellID)
	if index < 0 {
		return
	}
	cell := &document.Cells[index]
	if cell.Metadata.LC == nil {
		cell.Metadata.LC = &ipynb.CellLC{}
	}
	lc := cell.Metadata.LC
	lc.Language = language
	lc.Kernel = kernel.ID
	lc.Generation = kernel.Generation
	lc.StartedAt = &startedAt

	if result == nil {
		lc.Status = status
		lc.DurationMS = time.Since(startedAt).Milliseconds()
		if reason != "" {
			cell.Outputs = []ipynb.Output{{OutputType: ipynb.OutputError, EName: "ExecutionError", EValue: reason}}
		}
		document.Extension().AppendHistory(ipynb.HistoryEntry{
			CellID: cellID, Language: language, Status: lc.Status, Kernel: kernel.ID,
			Generation: kernel.Generation, StartedAt: startedAt, DurationMS: lc.DurationMS,
		})
		return
	}

	count := document.Extension().ExecutionCount + 1
	document.Extension().ExecutionCount = count
	cell.ExecutionCount = &count
	lc.Status = resultStatus(*result)
	lc.DurationMS = result.DurationMS
	if lc.DurationMS <= 0 {
		lc.DurationMS = time.Since(startedAt).Milliseconds()
	}
	lc.RequestID = result.RequestID
	lc.StateStatus = result.StateStatus
	lc.PartialEffects = result.PartialEffects
	if result.SessionID != "" {
		lc.Kernel = result.SessionID
	}
	if result.Generation != 0 {
		lc.Generation = result.Generation
	}
	exitCode := result.ExitCode
	lc.ExitCode = &exitCode
	cell.Outputs = resultOutputs(*result, lc.Kernel, refs)
	artifacts := make([]string, 0, len(produced))
	for _, artifact := range produced {
		if artifact.ID != "" {
			artifacts = append(artifacts, artifact.ID)
		}
	}
	document.Extension().AppendHistory(ipynb.HistoryEntry{
		CellID: cellID, Language: language, Status: lc.Status, Kernel: lc.Kernel,
		Generation: lc.Generation, ExecutionCount: count, StartedAt: startedAt,
		DurationMS: lc.DurationMS, ExitCode: lc.ExitCode, Artifacts: artifacts,
	})
}

// cellStatus reads back the status recorded on one cell.
func cellStatus(document ipynb.Document, cellID string) string {
	cell, ok := document.Find(cellID)
	if !ok || cell.Metadata.LC == nil {
		return ipynb.StatusIdle
	}
	return cell.Metadata.LC.Status
}

// resultStatus maps one execution result onto a cell status. A run that reports no status is judged
// by its exit code, which is what a worker that exited normally reports.
func resultStatus(result evalapi.Result) string {
	switch result.Status {
	case evalapi.ResultStatusCompleted:
		return ipynb.StatusOK
	case evalapi.ResultStatusCancelled:
		return ipynb.StatusCancelled
	case evalapi.ResultStatusFailed, evalapi.ResultStatusTimeout, evalapi.ResultStatusCrashed, evalapi.ResultStatusRejected:
		return ipynb.StatusError
	}
	// A status this build does not recognise is never a pass: the runtime did not say the cell
	// completed, so the notebook records an error rather than claiming success.
	return ipynb.StatusError
}

// resultOutputs maps one execution result onto nbformat outputs: stdout and stderr as streams, the
// runtime's displays as mime bundles or artifact references, and a failure as an error output. refs
// is the stored reference of every artifact the execution produced, keyed by the artifact id its
// display names; an artifact without a display of its own is added as a reference-only display.
func resultOutputs(result evalapi.Result, kernelSessionID string, refs map[string]ipynb.OutputLC) []ipynb.Output {
	out := make([]ipynb.Output, 0, 3+len(result.Displays))
	if result.Stdout != "" {
		out = append(out, ipynb.Output{OutputType: ipynb.OutputStream, Name: ipynb.StreamStdout, Text: ipynb.Source(result.Stdout)})
	}
	if result.Stderr != "" {
		out = append(out, ipynb.Output{OutputType: ipynb.OutputStream, Name: ipynb.StreamStderr, Text: ipynb.Source(result.Stderr)})
	}
	out = append(out, displayOutputs(result.Displays, kernelSessionID, refs)...)
	if result.Error != "" || result.Traceback != "" {
		out = append(out, ipynb.Output{
			OutputType: ipynb.OutputError,
			EName:      "ExecutionError",
			EValue:     result.Error,
			Traceback:  tracebackLines(result.Traceback),
		})
	}
	return ipynb.BoundOutputs(out)
}

// persistResultArtifacts gives every artifact of one execution a durable home: the bytes are copied
// into the session's notebook-artifact directory (bounded, content-addressed, purged with the
// session) and the document keeps a reference. An artifact past the size or quota limit, or one whose
// bytes cannot be read, stays a reference marked NotDurable — readable while its kernel lives, never
// presented as a durable link. It returns every artifact the execution produced, for the history, the
// reference of each artifact keyed by the id its display names, and the release of the artifacts it
// kept: the caller calls it once the cell's output is written, so a collection never takes an artifact
// between the write of its bytes and the write of the reference.
func (h *executionNotebookHost) persistResultArtifacts(ctx context.Context, sessionID, kernelSessionID string, result *evalapi.Result) ([]evalapi.Artifact, map[string]ipynb.OutputLC, func()) {
	produced := append([]evalapi.Artifact(nil), result.Artifacts...)
	if len(result.Artifacts) == 0 {
		return produced, nil, func() {}
	}
	refs := make(map[string]ipynb.OutputLC, len(result.Artifacts))
	durable := make([]string, 0, len(result.Artifacts))
	release := func() {
		for _, id := range durable {
			releaseNotebookArtifact(sessionID, id)
		}
	}
	for _, artifact := range result.Artifacts {
		owner := artifact.SessionID
		if owner == "" {
			owner = kernelSessionID
		}
		reference, data, ok := h.durableReference(ctx, sessionID, owner, artifact)
		if !ok {
			reference = volatileArtifactReference(artifact, owner)
		} else {
			durable = append(durable, reference.ArtifactID)
		}
		refs[reference.ArtifactID] = reference
		// The execution runtime can report the same picture twice (an inline display and an artifact).
		// The display then keeps one output: the stored reference, with the inline copy only when the
		// document can hold it. An artifact without a display of its own becomes a display of its own.
		if mergeArtifactIntoDisplay(result.Displays, artifact, reference, data) {
			continue
		}
		result.Displays = append(result.Displays, evalapi.Display{
			MIME: reference.MIME, ArtifactID: reference.ArtifactID, Size: reference.Size,
		})
	}
	result.Artifacts = nil
	return produced, refs, release
}

// durableReference copies one artifact's bytes into the notebook's own store and returns the document
// reference. ok is false for an artifact past the size limit, one whose bytes cannot be read, and one
// the store refuses: the caller keeps a volatile reference instead.
func (h *executionNotebookHost) durableReference(ctx context.Context, sessionID, owner string, artifact evalapi.Artifact) (ipynb.OutputLC, []byte, bool) {
	if artifact.Size > ExecutionNotebookArtifactMaxBytes {
		return ipynb.OutputLC{}, nil, false
	}
	meta, data, err := h.exec.artifact(ctx, sessionID, owner, artifact.ID)
	if err != nil {
		return ipynb.OutputLC{}, nil, false
	}
	if meta.MIME != "" && artifact.MIME == "" {
		artifact.MIME = meta.MIME
	}
	reference, err := h.putNotebookArtifact(ctx, sessionID, artifact, data)
	if err != nil {
		return ipynb.OutputLC{}, nil, false
	}
	return reference, data, true
}

// mergeArtifactIntoDisplay attaches an artifact's stored reference to the display that already holds
// the same bytes. A display that names the artifact is the match even when the runtime did not inline
// the bytes (a payload past the inline limit), so a large image is one reference rather than a broken
// inline copy beside a duplicate artifact. It reports whether it found a display.
func mergeArtifactIntoDisplay(displays []evalapi.Display, artifact evalapi.Artifact, reference ipynb.OutputLC, data []byte) bool {
	payload := ""
	if data != nil {
		payload = ipynb.Payload(reference.MIME, data)
	}
	for i := range displays {
		display := &displays[i]
		// A display that names another artifact is that artifact's output, never this one's.
		named := artifact.ID != "" && display.ArtifactID == artifact.ID
		if display.ArtifactID != "" && !named {
			continue
		}
		if !named {
			// A display that names no artifact is the same output only when it carries this artifact's
			// bytes under the same mime type.
			if payload == "" || !strings.EqualFold(strings.TrimSpace(display.MIME), reference.MIME) || display.Text != payload {
				continue
			}
		}
		display.ArtifactID = reference.ArtifactID
		display.Size = reference.Size
		if display.MIME == "" {
			display.MIME = reference.MIME
		}
		// The reference is what the page loads; the inline copy stays only when it fits the document's
		// own inline limit, so a bounded payload never leaves a truncated image behind.
		if len(display.Text) > ipynb.MaxInlineBytes {
			display.Text = ""
		}
		return true
	}
	return false
}

// volatileArtifactReference is the reference of an artifact whose bytes could not be kept.
func volatileArtifactReference(artifact evalapi.Artifact, owner string) ipynb.OutputLC {
	return ipynb.OutputLC{
		ArtifactID: artifact.ID, SessionID: owner, MIME: artifact.MIME,
		Size: artifact.Size, SHA256: artifact.SHA256, NotDurable: true,
	}
}

// tracebackLines splits a traceback into the lines nbformat stores.
func tracebackLines(traceback string) []string {
	traceback = strings.TrimRight(traceback, "\n")
	if traceback == "" {
		return nil
	}
	return strings.Split(traceback, "\n")
}

// displayOutputs maps the execution runtime's displays onto display_data outputs. A display that
// names an artifact keeps one output: the reference the document stores, with the payload in the mime
// bundle only when it fits. Any other display carries its payload in the mime bundle, base64 already
// when it is binary.
func displayOutputs(displays []evalapi.Display, kernelSessionID string, refs map[string]ipynb.OutputLC) []ipynb.Output {
	if len(displays) == 0 {
		return nil
	}
	out := make([]ipynb.Output, 0, len(displays))
	seen := make(map[string]bool, len(displays))
	for _, display := range displays {
		mime := strings.TrimSpace(display.MIME)
		if display.ArtifactID != "" {
			if seen[display.ArtifactID] {
				// One artifact is one output, however many displays named it.
				continue
			}
			seen[display.ArtifactID] = true
			reference, ok := refs[display.ArtifactID]
			if !ok {
				// A display that names an artifact the document does not hold: readable only from the
				// kernel that produced it, never presented as a durable link.
				reference = ipynb.OutputLC{
					ArtifactID: display.ArtifactID, SessionID: kernelSessionID, MIME: mime,
					Size: display.Size, NotDurable: true,
				}
			}
			if reference.MIME == "" {
				reference.MIME = mime
			}
			output := ipynb.Output{OutputType: ipynb.OutputDisplay, LC: &reference}
			if display.Text != "" && len(display.Text) <= ipynb.MaxInlineBytes {
				payloadMime := mime
				if payloadMime == "" {
					payloadMime = "application/octet-stream"
				}
				output.Data = map[string]ipynb.MimeData{payloadMime: ipynb.Text(display.Text)}
			}
			out = append(out, output)
			continue
		}
		if display.Text == "" {
			continue
		}
		if mime == "" {
			mime = "text/plain"
		}
		out = append(out, ipynb.Output{OutputType: ipynb.OutputDisplay, Data: map[string]ipynb.MimeData{mime: ipynb.Text(display.Text)}})
	}
	return out
}

// publishCell tells the host which cell started or finished, so an open notebook can refresh without
// polling. The event carries identity and status only, never code or output.
func (h *executionNotebookHost) publishCell(ctx context.Context, sessionID, notebookID, cellID, language, status string) {
	h.engine.events.publish("execution_notebook.cell", sessionID, map[string]any{
		"notebookId": notebookID, "cellId": cellID, "language": language, "status": status,
	})
	_ = h.engine.store.Journal(context.WithoutCancel(ctx), "execution_notebook.cell", map[string]any{
		"sessionId": sessionID, "notebookId": notebookID, "cellId": cellID, "language": language, "status": status,
	})
}
