package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/internal/ipynb"
	"github.com/edynasty/LumenCortex/internal/session"
)

// Execution notebooks (the notebook runtime of the unified execution runtime). An execution notebook
// is a .ipynb document: cells of Python, JavaScript or any other language an execution runtime
// plugin provides, the outputs each cell produced, and the durable history of the executions.
//
// It is stored as one session notebook (ADR-0020 storage, migration 0009): the notebook body is the
// canonical ipynb JSON, the notebook version is the compare-and-set the host writes against, and the
// revisions keep the previous documents. There is no second database and no separate kernel state:
// kernels belong to the execution runtime and are read live (ExecutionKernels), so a stored notebook
// never names a kernel that has gone away.
//
// An execution notebook is told apart from an agent working notebook by its title: the store keeps
// "Execution notebook: <title>" so the agent's notebook tools, the run-start index and this API each
// see only their own kind.
const (
	executionNotebookTitlePrefix = "Execution notebook: "
	// ExecutionNotebookMaxTitleChars bounds the user-visible title of one execution notebook.
	ExecutionNotebookMaxTitleChars = 96
	// ExecutionNotebookMaxImportBytes bounds an imported .ipynb file: the stored body is smaller.
	ExecutionNotebookMaxImportBytes = ipynb.MaxDocumentBytes
	// ExecutionArtifactMaxBytes bounds the bytes ExecutionArtifact returns to a host.
	ExecutionArtifactMaxBytes = 8 << 20
)

var (
	// ErrExecutionNotebookNotFound means the session holds no execution notebook with that id or
	// title, or the notebook named is an agent working notebook, which this API never edits.
	ErrExecutionNotebookNotFound = errors.New("execution notebook not found")
)

// ExecutionNotebook is one execution notebook with its document. Version is the store's
// compare-and-set: SaveExecutionNotebook and the cell edits apply only to the version they name.
type ExecutionNotebook struct {
	ID        string         `json:"id"`
	SessionID string         `json:"sessionId"`
	Title     string         `json:"title"`
	Version   int64          `json:"version"`
	UpdatedAt time.Time      `json:"updatedAt"`
	UpdatedBy string         `json:"updatedBy"`
	Document  ipynb.Document `json:"document"`
}

// ExecutionNotebookDocument is the ipynb document of one execution notebook, for hosts that build or
// edit a document before saving it.
type ExecutionNotebookDocument = ipynb.Document

// ExecutionCell is one cell of an execution notebook, as a host adds or updates it.
type ExecutionCell = ipynb.Cell

// ExecutionCellMetadata is a cell's typed metadata: the `lc` extension plus any other key.
type ExecutionCellMetadata = ipynb.CellMetadata

// ExecutionCellLC is a cell's `lc` metadata: language, last status and kernel provenance.
type ExecutionCellLC = ipynb.CellLC

// ExecutionNotebookInfo is one notebook without its document: what a list needs to show the
// notebook and offer to open it.
type ExecutionNotebookInfo struct {
	ID        string    `json:"id"`
	SessionID string    `json:"sessionId"`
	Title     string    `json:"title"`
	Version   int64     `json:"version"`
	Bytes     int       `json:"bytes"`
	UpdatedAt time.Time `json:"updatedAt"`
	UpdatedBy string    `json:"updatedBy"`
	// Withdrawn is set when the session is withdrawn as a source: the notebook keeps its identity,
	// version and author and loses its title, because the title is the session's content.
	Withdrawn bool `json:"withdrawn,omitempty"`
}

// ExecutionRuntime is one language an execution runtime plugin provides, as the notebook offers it in
// its language picker. ID is what CreateEvalSession takes.
type ExecutionRuntime struct {
	ID             string `json:"id"`
	Language       string `json:"language"`
	Version        string `json:"version"`
	RuntimeVersion string `json:"runtimeVersion"`
}

// ExecutionKernel is one live kernel of the session, as the notebook shows it. SessionID is the
// execution session id: the notebook reads it only to fetch artifacts and to control the kernel, and
// never stores it.
type ExecutionKernel struct {
	Language   string `json:"language"`
	RuntimeID  string `json:"runtimeId"`
	SessionID  string `json:"sessionId"`
	State      string `json:"state"`
	Generation uint64 `json:"generation"`
	Alive      bool   `json:"alive"`
	// CreatedAt orders kernels of one language: the current kernel is the newest live one. Generation
	// is not an order between kernels — it counts the restarts of one kernel — so a host that shows
	// or controls "the" kernel of a language reads CreatedAt, never the highest generation.
	CreatedAt time.Time `json:"createdAt"`
}

// ExecutionNotebookRun is one run of a notebook that is in flight right now. A page reads it on open
// and after an event, so a run that started before a route switch is still shown as running.
type ExecutionNotebookRun struct {
	NotebookID string `json:"notebookId"`
	// CellID is the cell being executed, empty for a whole-notebook run.
	CellID    string    `json:"cellId,omitempty"`
	Replay    bool      `json:"replay"`
	StartedAt time.Time `json:"startedAt"`
}

// ExecutionArtifact is the bytes of one artifact an execution produced, with what the notebook
// recorded about it. Data is bounded by ExecutionArtifactMaxBytes.
type ExecutionArtifact struct {
	ArtifactID string    `json:"artifactId"`
	SessionID  string    `json:"sessionId"`
	CellID     string    `json:"cellId"`
	MIME       string    `json:"mime"`
	Size       int64     `json:"size"`
	SHA256     string    `json:"sha256"`
	CreatedAt  time.Time `json:"createdAt"`
	Data       []byte    `json:"data"`
}

// executionExecutor is the part of the execution runtime an execution notebook uses. The Engine's Eval
// methods are the production implementation; a test binds a deterministic fake, so the notebook
// behaviour (kernel reuse, clean replay, status mapping, artifact references) is checked without
// starting a worker.
type executionExecutor interface {
	runtimes(ctx context.Context) ([]evalapi.Manifest, error)
	sessions(ctx context.Context, sessionID string) ([]evalapi.Session, error)
	create(ctx context.Context, sessionID, runtimeID string) (evalapi.Session, error)
	execute(ctx context.Context, sessionID string, req evalapi.Request) (evalapi.Result, error)
	control(ctx context.Context, sessionID, evalSessionID, action string) error
	interrupt(ctx context.Context, sessionID, evalSessionID string, target evalapi.InterruptTarget) (evalapi.InterruptOutcome, error)
	artifact(ctx context.Context, sessionID, evalSessionID, artifactID string) (evalapi.Artifact, []byte, error)
}

// engineExecutor is the executor over the Engine's own Eval methods. Every call re-checks the LC
// session's fence and owner, so a notebook write can never reach another session's kernel.
type engineExecutor struct{ engine *Engine }

func (x engineExecutor) runtimes(ctx context.Context) ([]evalapi.Manifest, error) {
	return x.engine.EvalRuntimes(ctx)
}

func (x engineExecutor) sessions(ctx context.Context, sessionID string) ([]evalapi.Session, error) {
	return x.engine.EvalSessions(ctx, sessionID)
}

func (x engineExecutor) create(ctx context.Context, sessionID, runtimeID string) (evalapi.Session, error) {
	return x.engine.CreateEvalSession(ctx, sessionID, runtimeID)
}

func (x engineExecutor) execute(ctx context.Context, sessionID string, req evalapi.Request) (evalapi.Result, error) {
	return x.engine.ExecuteEval(ctx, sessionID, req)
}

func (x engineExecutor) control(ctx context.Context, sessionID, evalSessionID, action string) error {
	switch action {
	case "reset":
		return x.engine.EvalReset(ctx, sessionID, evalSessionID)
	case "close":
		return x.engine.EvalClose(ctx, sessionID, evalSessionID)
	}
	return fmt.Errorf("unknown eval control action %q", action)
}

func (x engineExecutor) interrupt(ctx context.Context, sessionID, evalSessionID string, target evalapi.InterruptTarget) (evalapi.InterruptOutcome, error) {
	return x.engine.EvalInterrupt(ctx, sessionID, evalSessionID, target)
}

func (x engineExecutor) artifact(ctx context.Context, sessionID, evalSessionID, artifactID string) (evalapi.Artifact, []byte, error) {
	return x.engine.EvalArtifact(ctx, sessionID, evalSessionID, artifactID)
}

// executionNotebookHost is the execution-notebook API bound to one Engine and one executor. The
// Engine methods below each build the production host; tests build it with a fake executor.
type executionNotebookHost struct {
	engine *Engine
	exec   executionExecutor
}

// executionNotebooks is the production host of the Engine's execution-notebook methods.
func (e *Engine) executionNotebooks() *executionNotebookHost {
	return &executionNotebookHost{engine: e, exec: engineExecutor{engine: e}}
}

// ExecutionNotebooks lists the execution notebooks of a session, newest first, without their
// documents. A session withdrawn as a source returns its notebooks with identity and version and
// without titles (see ExecutionNotebookInfo.Withdrawn).
func (e *Engine) ExecutionNotebooks(ctx context.Context, sessionID string) ([]ExecutionNotebookInfo, error) {
	return e.executionNotebooks().list(ctx, sessionID)
}

// ExecutionNotebook returns one execution notebook of the session by id (or by its stored title).
func (e *Engine) ExecutionNotebook(ctx context.Context, sessionID, ref string) (ExecutionNotebook, error) {
	return e.executionNotebooks().get(ctx, sessionID, ref)
}

// CreateExecutionNotebook creates an empty execution notebook whose default cell language is
// language. A language names one runtime an execution plugin provides; an empty language creates a
// notebook that runs nothing until a cell picks a language.
func (e *Engine) CreateExecutionNotebook(ctx context.Context, sessionID, title, language string) (ExecutionNotebook, error) {
	return e.executionNotebooks().create(ctx, sessionID, title, language)
}

// SaveExecutionNotebook replaces the document of one execution notebook. The write applies only to
// expectedVersion: a notebook at another version is refused with *NotebookConflict and nothing is
// written. It never executes a cell.
func (e *Engine) SaveExecutionNotebook(ctx context.Context, sessionID, ref string, document ExecutionNotebookDocument, expectedVersion int64) (ExecutionNotebook, error) {
	return e.executionNotebooks().save(ctx, sessionID, ref, document, expectedVersion)
}

// DeleteExecutionNotebook deletes one execution notebook and its kept revisions.
func (e *Engine) DeleteExecutionNotebook(ctx context.Context, sessionID, ref string) error {
	return e.executionNotebooks().delete(ctx, sessionID, ref)
}

// ImportExecutionNotebook stores a standard .ipynb document as a new execution notebook of the
// session. It executes nothing: importing a notebook never starts a kernel, and its recorded outputs
// are kept as evidence of what produced them.
func (e *Engine) ImportExecutionNotebook(ctx context.Context, sessionID, title, data string) (ExecutionNotebook, error) {
	return e.executionNotebooks().importNotebook(ctx, sessionID, title, data)
}

// ExportExecutionNotebook returns one notebook as a standard .ipynb document and the file name a host
// should save it as. A session withdrawn as a source is refused with ErrNotebookWithdrawn.
func (e *Engine) ExportExecutionNotebook(ctx context.Context, sessionID, ref string) (document, filename string, err error) {
	return e.executionNotebooks().export(ctx, sessionID, ref)
}

// AddExecutionCell inserts cell at index (0..len; out of range appends). The write applies only to
// expectedVersion.
func (e *Engine) AddExecutionCell(ctx context.Context, sessionID, ref string, cell ExecutionCell, index int, expectedVersion int64) (ExecutionNotebook, error) {
	return e.executionNotebooks().addCell(ctx, sessionID, ref, cell, index, expectedVersion)
}

// UpdateExecutionCell replaces the cell with the same id. The write applies only to expectedVersion.
func (e *Engine) UpdateExecutionCell(ctx context.Context, sessionID, ref string, cell ExecutionCell, expectedVersion int64) (ExecutionNotebook, error) {
	return e.executionNotebooks().updateCell(ctx, sessionID, ref, cell, expectedVersion)
}

// DeleteExecutionCell deletes one cell. The write applies only to expectedVersion.
func (e *Engine) DeleteExecutionCell(ctx context.Context, sessionID, ref, cellID string, expectedVersion int64) (ExecutionNotebook, error) {
	return e.executionNotebooks().deleteCell(ctx, sessionID, ref, cellID, expectedVersion)
}

// MoveExecutionCell moves one cell to index. The write applies only to expectedVersion.
func (e *Engine) MoveExecutionCell(ctx context.Context, sessionID, ref, cellID string, index int, expectedVersion int64) (ExecutionNotebook, error) {
	return e.executionNotebooks().moveCell(ctx, sessionID, ref, cellID, index, expectedVersion)
}

// RunExecutionCell executes one code cell on the kernel of its language and records the result, its
// outputs and the history entry. A kernel is created on demand and reused while it lives.
func (e *Engine) RunExecutionCell(ctx context.Context, sessionID, ref, cellID string) (ExecutionNotebook, error) {
	return e.executionNotebooks().run(ctx, sessionID, ref, cellID, false)
}

// RunExecutionNotebook executes every code cell in order on the current kernels, stopping at the
// first cell that fails. replay closes the kernels first and clears every recorded result, so the
// notebook is rebuilt from a clean session: it is the explicit way to reproduce a notebook, and it
// never happens on load or import.
func (e *Engine) RunExecutionNotebook(ctx context.Context, sessionID, ref string, replay bool) (ExecutionNotebook, error) {
	return e.executionNotebooks().run(ctx, sessionID, ref, "", replay)
}

// InterruptExecutionNotebook interrupts the cell the notebook's run is executing - that execution
// only, by its request id: a cell another notebook or the agent runs on the same kernel is not
// touched, and an interrupt that arrives after the cell finished stops nothing. The cell in flight
// returns with status cancelled; nothing is replayed. A notebook that runs nothing is a no-op.
func (e *Engine) InterruptExecutionNotebook(ctx context.Context, sessionID, ref string) error {
	return e.executionNotebooks().interrupt(ctx, sessionID, ref)
}

// ExecutionKernels lists the session's kernels with their live state, so the notebook can show the
// current kernel of each language instead of guessing one from a stored id. The list is oldest first:
// the current kernel of a language is the last live one.
func (e *Engine) ExecutionKernels(ctx context.Context, sessionID string) ([]ExecutionKernel, error) {
	return e.executionNotebooks().kernels(ctx, sessionID)
}

// ExecutionNotebookRuns lists the runs of the session's notebooks that are in flight in this process.
// A page reads it on open, so a run that started before a route switch is still shown as running and
// offers Cancel instead of queueing a second run.
func (e *Engine) ExecutionNotebookRuns(ctx context.Context, sessionID string) ([]ExecutionNotebookRun, error) {
	return e.executionNotebooks().runs(ctx, sessionID)
}

// ResetExecutionKernel resets every live kernel of one language to a clean session, keeping each
// kernel's identity. A language with no live kernel is a no-op: the next run creates one.
func (e *Engine) ResetExecutionKernel(ctx context.Context, sessionID, language string) error {
	return e.executionNotebooks().controlLanguage(ctx, sessionID, language, "reset")
}

// CloseExecutionKernel closes every live kernel of one language. A language with no live kernel is a
// no-op.
func (e *Engine) CloseExecutionKernel(ctx context.Context, sessionID, language string) error {
	return e.executionNotebooks().controlLanguage(ctx, sessionID, language, "close")
}

// ExecutionArtifact returns the bytes of one artifact an execution recorded, from the kernel that
// produced it. It is the path for a reference the notebook marked NotDurable: readable while that
// kernel lives.
func (e *Engine) ExecutionArtifact(ctx context.Context, sessionID, evalSessionID, artifactID string) (ExecutionArtifact, error) {
	return e.executionNotebooks().artifact(ctx, sessionID, evalSessionID, artifactID)
}

// StoredExecutionArtifact returns the bytes of one artifact the notebook kept durably (a reference
// without NotDurable). The bytes are content-addressed inside the session's private execution
// subtree and verified on every read, so they survive a restart and go with the session.
func (e *Engine) StoredExecutionArtifact(ctx context.Context, sessionID, artifactID string) (ExecutionArtifact, error) {
	return e.executionNotebooks().notebookArtifact(ctx, sessionID, artifactID)
}

// list implements ExecutionNotebooks.
func (h *executionNotebookHost) list(ctx context.Context, sessionID string) ([]ExecutionNotebookInfo, error) {
	if _, err := h.engine.store.Get(ctx, sessionID); err != nil {
		return nil, err
	}
	infos, err := h.engine.store.ListNotebooks(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	withdrawn, err := h.engine.store.SessionWithdrawn(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]ExecutionNotebookInfo, 0, len(infos))
	for _, info := range infos {
		if !isExecutionNotebookTitle(info.Title) {
			continue
		}
		entry := ExecutionNotebookInfo{
			ID: info.ID, SessionID: info.SessionID, Title: executionNotebookTitle(info.Title),
			Version: info.Version, Bytes: info.Bytes, UpdatedAt: info.UpdatedAt, UpdatedBy: info.UpdatedBy,
		}
		if withdrawn {
			entry.Title, entry.Withdrawn = "", true
		}
		out = append(out, entry)
	}
	return out, nil
}

// get implements ExecutionNotebook.
func (h *executionNotebookHost) get(ctx context.Context, sessionID, ref string) (ExecutionNotebook, error) {
	stored, err := h.stored(ctx, sessionID, ref)
	if err != nil {
		return ExecutionNotebook{}, err
	}
	withdrawn, err := h.engine.store.SessionWithdrawn(ctx, sessionID)
	if err != nil {
		return ExecutionNotebook{}, err
	}
	if withdrawn {
		return ExecutionNotebook{
			ID: stored.ID, SessionID: stored.SessionID, Version: stored.Version,
			UpdatedAt: stored.UpdatedAt, UpdatedBy: stored.UpdatedBy,
			Document: ipynb.Document{NBFormat: ipynb.Format, NBFormatMinor: ipynb.FormatMinor, Cells: []ipynb.Cell{}},
		}, nil
	}
	document, err := ipynb.Parse(stored.Body)
	if err != nil {
		return ExecutionNotebook{}, fmt.Errorf("execution notebook %s: %w", stored.ID, err)
	}
	return executionNotebookOf(stored, document), nil
}

// create implements CreateExecutionNotebook.
func (h *executionNotebookHost) create(ctx context.Context, sessionID, title, language string) (ExecutionNotebook, error) {
	title, err := checkExecutionNotebookTitle(title)
	if err != nil {
		return ExecutionNotebook{}, err
	}
	if language != "" {
		if _, err := h.runtimeID(ctx, language); err != nil {
			return ExecutionNotebook{}, err
		}
	}
	document := ipynb.New(language)
	body, err := ipynb.Marshal(document)
	if err != nil {
		return ExecutionNotebook{}, err
	}
	stored, err := h.engine.store.CreateNotebook(ctx, sessionID, executionNotebookTitlePrefix+title, body, "host")
	if err != nil {
		return ExecutionNotebook{}, err
	}
	h.engine.notebookUpdated(ctx, sessionID, stored)
	return executionNotebookOf(stored, document), nil
}

// save implements SaveExecutionNotebook.
func (h *executionNotebookHost) save(ctx context.Context, sessionID, ref string, document ipynb.Document, expectedVersion int64) (ExecutionNotebook, error) {
	stored, err := h.stored(ctx, sessionID, ref)
	if err != nil {
		return ExecutionNotebook{}, err
	}
	if err := ipynb.Normalize(&document); err != nil {
		return ExecutionNotebook{}, err
	}
	body, err := ipynb.Marshal(document)
	if err != nil {
		return ExecutionNotebook{}, err
	}
	updated, err := h.engine.store.UpdateNotebook(ctx, sessionID, stored.ID, "host", "replace", expectedVersion,
		func(string) (string, error) { return body, nil })
	if err != nil {
		return ExecutionNotebook{}, err
	}
	h.engine.notebookUpdated(ctx, sessionID, updated)
	return executionNotebookOf(updated, document), nil
}

// delete implements DeleteExecutionNotebook.
func (h *executionNotebookHost) delete(ctx context.Context, sessionID, ref string) error {
	stored, err := h.stored(ctx, sessionID, ref)
	if err != nil {
		return err
	}
	if err := h.engine.deleteNotebook(ctx, sessionID, stored.ID); err != nil {
		return err
	}
	// The notebook's images and files are no longer referenced by anything, so the session's quota
	// stops charging for them: a deleted notebook never leaves its artifacts behind.
	h.collectNotebookArtifacts(ctx, sessionID)
	return nil
}

// importNotebook implements ImportExecutionNotebook.
func (h *executionNotebookHost) importNotebook(ctx context.Context, sessionID, title, data string) (ExecutionNotebook, error) {
	if len(data) > ExecutionNotebookMaxImportBytes {
		return ExecutionNotebook{}, fmt.Errorf("the notebook file is %d bytes, more than the %d byte limit", len(data), ExecutionNotebookMaxImportBytes)
	}
	title, err := checkExecutionNotebookTitle(title)
	if err != nil {
		return ExecutionNotebook{}, err
	}
	document, err := ipynb.Parse(data)
	if err != nil {
		return ExecutionNotebook{}, err
	}
	body, err := ipynb.Fit(&document)
	if err != nil {
		return ExecutionNotebook{}, err
	}
	stored, err := h.engine.store.CreateNotebook(ctx, sessionID, executionNotebookTitlePrefix+title, body, "host")
	if err != nil {
		return ExecutionNotebook{}, err
	}
	h.engine.notebookUpdated(ctx, sessionID, stored)
	return executionNotebookOf(stored, document), nil
}

// export implements ExportExecutionNotebook.
func (h *executionNotebookHost) export(ctx context.Context, sessionID, ref string) (string, string, error) {
	stored, err := h.stored(ctx, sessionID, ref)
	if err != nil {
		return "", "", err
	}
	withdrawn, err := h.engine.store.SessionWithdrawn(ctx, sessionID)
	if err != nil {
		return "", "", err
	}
	if withdrawn {
		return "", "", ErrNotebookWithdrawn
	}
	document, err := ipynb.Parse(stored.Body)
	if err != nil {
		return "", "", fmt.Errorf("execution notebook %s: %w", stored.ID, err)
	}
	h.inlineArtifacts(ctx, sessionID, &document)
	exported, err := ipynb.Export(document)
	if err != nil {
		return "", "", err
	}
	// notebookFilename is the Markdown export's name (it appends .md); an ipynb export needs its own
	// suffix and no Markdown one.
	name := strings.TrimSuffix(notebookFilename(executionNotebookTitle(stored.Title)), ".md")
	return exported, name + ".ipynb", nil
}

// inlineArtifacts writes a small artifact's bytes into the exported document, so the .ipynb opens
// outside LumenCortex. A larger artifact keeps its reference and gains a text note naming it. The
// stored document is not changed: it keeps references and stays small.
func (h *executionNotebookHost) inlineArtifacts(ctx context.Context, sessionID string, document *ipynb.Document) {
	budget := 32
	for i := range document.Cells {
		for j := range document.Cells[i].Outputs {
			output := &document.Cells[i].Outputs[j]
			if output.LC == nil || output.LC.ArtifactID == "" {
				continue
			}
			if budget <= 0 {
				output.Data = artifactNote(output.LC)
				continue
			}
			budget--
			meta, data, err := h.artifactBytes(ctx, sessionID, output.LC)
			if err != nil || len(data) > ipynb.MaxInlineBytes {
				output.Data = artifactNote(output.LC)
				continue
			}
			mime := meta.MIME
			if mime == "" {
				mime = output.LC.MIME
			}
			if mime == "" {
				mime = "application/octet-stream"
			}
			output.Data = map[string]ipynb.MimeData{mime: ipynb.Text(ipynb.Payload(mime, data))}
		}
	}
}

// artifactBytes reads one referenced artifact: from the notebook's durable store when the reference is
// durable, from the kernel that produced it when it is not.
func (h *executionNotebookHost) artifactBytes(ctx context.Context, sessionID string, reference *ipynb.OutputLC) (evalapi.Artifact, []byte, error) {
	if !reference.NotDurable {
		stored, err := h.notebookArtifact(ctx, sessionID, reference.ArtifactID)
		if err != nil {
			return evalapi.Artifact{}, nil, err
		}
		return evalapi.Artifact{
			ID: stored.ArtifactID, SessionID: stored.SessionID, CellID: stored.CellID,
			MIME: stored.MIME, SHA256: stored.SHA256, Size: stored.Size, CreatedAt: stored.CreatedAt,
		}, stored.Data, nil
	}
	return h.exec.artifact(ctx, sessionID, reference.SessionID, reference.ArtifactID)
}

// artifactNote is what an exported notebook says about an artifact it could not inline.
func artifactNote(reference *ipynb.OutputLC) map[string]ipynb.MimeData {
	note := fmt.Sprintf("[LumenCortex artifact %s, %s, %d bytes, sha256 %s]",
		reference.ArtifactID, reference.MIME, reference.Size, reference.SHA256)
	return map[string]ipynb.MimeData{"text/plain": ipynb.Text(note)}
}

// addCell implements AddExecutionCell.
func (h *executionNotebookHost) addCell(ctx context.Context, sessionID, ref string, cell ipynb.Cell, index int, expectedVersion int64) (ExecutionNotebook, error) {
	return h.edit(ctx, sessionID, ref, expectedVersion, func(document *ipynb.Document) error {
		return document.Insert(index, cell)
	})
}

// updateCell implements UpdateExecutionCell.
func (h *executionNotebookHost) updateCell(ctx context.Context, sessionID, ref string, cell ipynb.Cell, expectedVersion int64) (ExecutionNotebook, error) {
	return h.edit(ctx, sessionID, ref, expectedVersion, func(document *ipynb.Document) error {
		return document.Replace(cell)
	})
}

// deleteCell implements DeleteExecutionCell.
func (h *executionNotebookHost) deleteCell(ctx context.Context, sessionID, ref, cellID string, expectedVersion int64) (ExecutionNotebook, error) {
	return h.edit(ctx, sessionID, ref, expectedVersion, func(document *ipynb.Document) error {
		return document.Remove(cellID)
	})
}

// moveCell implements MoveExecutionCell.
func (h *executionNotebookHost) moveCell(ctx context.Context, sessionID, ref, cellID string, index int, expectedVersion int64) (ExecutionNotebook, error) {
	return h.edit(ctx, sessionID, ref, expectedVersion, func(document *ipynb.Document) error {
		return document.Move(cellID, index)
	})
}

// edit applies one cell edit as one compare-and-set write. The document is parsed inside the write
// transaction, so the edit applies to exactly the version it extends.
func (h *executionNotebookHost) edit(ctx context.Context, sessionID, ref string, expectedVersion int64, apply func(*ipynb.Document) error) (ExecutionNotebook, error) {
	stored, err := h.stored(ctx, sessionID, ref)
	if err != nil {
		return ExecutionNotebook{}, err
	}
	return h.mutate(ctx, sessionID, stored.ID, expectedVersion, apply)
}

// stored reads one execution notebook of the session. A working notebook (a Markdown document the
// agent maintains) is refused: this API never edits it.
func (h *executionNotebookHost) stored(ctx context.Context, sessionID, ref string) (session.NotebookDoc, error) {
	if _, err := h.engine.store.Get(ctx, sessionID); err != nil {
		return session.NotebookDoc{}, err
	}
	doc, err := h.engine.store.GetNotebook(ctx, sessionID, ref)
	if err != nil {
		if errors.Is(err, ErrNotebookNotFound) {
			return session.NotebookDoc{}, ErrExecutionNotebookNotFound
		}
		return session.NotebookDoc{}, err
	}
	if !isExecutionNotebookTitle(doc.Title) {
		return session.NotebookDoc{}, ErrExecutionNotebookNotFound
	}
	return doc, nil
}

// runtimeID resolves the execution runtime that provides one language.
func (h *executionNotebookHost) runtimeID(ctx context.Context, language string) (string, error) {
	manifests, err := h.exec.runtimes(ctx)
	if err != nil {
		return "", err
	}
	return runtimeForLanguage(manifests, language)
}

// runtimeForLanguage picks the manifest that provides language. The first manifest by id wins, so the
// choice does not depend on discovery order.
func runtimeForLanguage(manifests []evalapi.Manifest, language string) (string, error) {
	language = strings.TrimSpace(language)
	if language == "" {
		return "", errors.New("a cell needs a language before it can run")
	}
	match := ""
	for _, manifest := range manifests {
		if !strings.EqualFold(strings.TrimSpace(manifest.Language), language) {
			continue
		}
		if match == "" || manifest.ID < match {
			match = manifest.ID
		}
	}
	if match == "" {
		return "", fmt.Errorf("no execution runtime provides the language %q", language)
	}
	return match, nil
}

// executionNotebookOf pairs a stored notebook with its parsed document.
func executionNotebookOf(stored session.NotebookDoc, document ipynb.Document) ExecutionNotebook {
	return ExecutionNotebook{
		ID: stored.ID, SessionID: stored.SessionID, Title: executionNotebookTitle(stored.Title),
		Version: stored.Version, UpdatedAt: stored.UpdatedAt, UpdatedBy: stored.UpdatedBy, Document: document,
	}
}

// isExecutionNotebookTitle reports whether a stored notebook title belongs to this API.
func isExecutionNotebookTitle(title string) bool {
	return strings.HasPrefix(title, executionNotebookTitlePrefix)
}

// executionNotebookTitle is the user-visible title of a stored execution notebook.
func executionNotebookTitle(stored string) string {
	return strings.TrimPrefix(stored, executionNotebookTitlePrefix)
}

// checkExecutionNotebookTitle validates the user-visible title and returns it trimmed. A title is one
// printable line: it names the notebook in the picker and in the export file name.
func checkExecutionNotebookTitle(title string) (string, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return "", errors.New("a notebook title is required")
	}
	if utf8.RuneCountInString(title) > ExecutionNotebookMaxTitleChars {
		return "", fmt.Errorf("a notebook title must be at most %d characters", ExecutionNotebookMaxTitleChars)
	}
	if !utf8.ValidString(title) || strings.IndexFunc(title, unicode.IsControl) >= 0 {
		return "", errors.New("a notebook title must be one printable line")
	}
	return title, nil
}
