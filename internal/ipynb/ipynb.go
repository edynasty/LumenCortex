// Package ipynb is the canonical shape of a LumenCortex execution notebook: the part of the
// Jupyter notebook format (nbformat 4.5) the runtime reads, writes and exports, plus the `lc`
// metadata extension that carries the per-cell language, the execution status of each cell and the
// references to the artifacts an execution produced.
//
// The document is stored as the body of a session notebook (ADR-0020 storage, runtime
// execution_notebooks.go), so the limits here are the limits of one stored body: a document that
// would exceed them is refused, never silently cut. Outputs are bounded at the cell: text is
// truncated with a visible mark, an image larger than MaxInlineBytes is dropped with the truncation
// flag (an artifact reference, when there is one, is what the UI loads instead).
//
// The package holds no storage, no policy and no runtime state: parsing, normalizing and the cell
// edits are pure, so the host API, the import/export path and the tests share one implementation.
package ipynb

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Limits of one execution notebook. They are constants: a notebook is bounded evidence, and a
// document that grows without a limit would be a second database inside the session.
const (
	// Format and FormatMinor are the nbformat this package writes.
	Format      = 4
	FormatMinor = 5
	// MaxCells bounds the cells of one document.
	MaxCells = 200
	// MaxCellSourceBytes bounds one cell's source.
	MaxCellSourceBytes = 64 << 10
	// MaxCellOutputBytes bounds the outputs kept for one code cell, in total.
	MaxCellOutputBytes = 96 << 10
	// MaxInlineBytes bounds one inline output payload (a text block or one base64 image). An execution
	// artifact that fits is written into the document, so a completed cell keeps real content after
	// the kernel that produced it is gone; a larger one stays a reference and is marked NotDurable.
	MaxInlineBytes = 64 << 10
	// MaxHistory bounds metadata.lc.history, the durable execution history of the notebook.
	MaxHistory = 100
	// MaxDocumentBytes bounds one stored body: the session store refuses more.
	MaxDocumentBytes = 256 << 10
)

// Cell types, as nbformat names them.
const (
	CellCode     = "code"
	CellMarkdown = "markdown"
	CellRaw      = "raw"
)

// Cell statuses, as the `lc` extension records them. StatusRunning is what the UI shows while the
// host is executing a cell; it is never stored, because a persisted "running" outlives the process
// that wrote it and would invite a replay the user did not ask for.
const (
	StatusIdle      = "idle"
	StatusRunning   = "running"
	StatusOK        = "ok"
	StatusError     = "error"
	StatusCancelled = "cancelled"
	StatusSkipped   = "skipped"
)

// Output types, as nbformat names them.
const (
	OutputStream       = "stream"
	OutputDisplay      = "display_data"
	OutputExecute      = "execute_result"
	OutputError        = "error"
	StreamStdout       = "stdout"
	StreamStderr       = "stderr"
	TruncatedOutputMsg = "… output truncated (notebook limit) …\n"
)

// Document is one .ipynb document.
type Document struct {
	NBFormat      int      `json:"nbformat"`
	NBFormatMinor int      `json:"nbformat_minor"`
	Metadata      Metadata `json:"metadata"`
	Cells         []Cell   `json:"cells"`
}

// MarshalJSON writes the document in the shape nbformat requires: both format numbers, an object for
// metadata and an array for cells, never null.
func (d Document) MarshalJSON() ([]byte, error) {
	cells := d.Cells
	if cells == nil {
		cells = []Cell{}
	}
	return json.Marshal(struct {
		NBFormat      int      `json:"nbformat"`
		NBFormatMinor int      `json:"nbformat_minor"`
		Metadata      Metadata `json:"metadata"`
		Cells         []Cell   `json:"cells"`
	}{d.NBFormat, d.NBFormatMinor, d.Metadata, cells})
}

// Metadata is the notebook metadata. The standard keys the runtime understands are typed; every
// other key of an imported notebook is kept verbatim in Extra so an export does not lose it.
type Metadata struct {
	Kernelspec   *Kernelspec                `json:"-"`
	LanguageInfo *LanguageInfo              `json:"-"`
	LC           *LCMetadata                `json:"-"`
	Extra        map[string]json.RawMessage `json:"-"`
}

// Kernelspec is the standard kernelspec metadata. LumenCortex writes its own name; an imported
// notebook keeps what it carried until the document is written.
type Kernelspec struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	Language    string `json:"language,omitempty"`
}

// LanguageInfo is the standard language_info metadata.
type LanguageInfo struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

// LCMetadata is the `lc` extension at notebook level: the default cell language, the runtime each
// language executes on, the notebook-wide execution counter and the bounded execution history.
type LCMetadata struct {
	DefaultLanguage string            `json:"defaultLanguage,omitempty"`
	Runtimes        map[string]string `json:"runtimes,omitempty"`
	ExecutionCount  int               `json:"executionCount,omitempty"`
	History         []HistoryEntry    `json:"history,omitempty"`
}

// HistoryEntry is one executed cell in the durable history: what ran, on which kernel generation,
// how it ended and which artifacts it produced. It never carries a kernel heap or a host path.
type HistoryEntry struct {
	CellID         string    `json:"cellId"`
	Language       string    `json:"language"`
	Status         string    `json:"status"`
	Kernel         string    `json:"kernel,omitempty"`
	Generation     uint64    `json:"generation,omitempty"`
	ExecutionCount int       `json:"executionCount,omitempty"`
	StartedAt      time.Time `json:"startedAt,omitempty"`
	DurationMS     int64     `json:"durationMs,omitempty"`
	ExitCode       *int      `json:"exitCode,omitempty"`
	Artifacts      []string  `json:"artifacts,omitempty"`
}

// Cell is one notebook cell. Source, ExecutionCount and Outputs follow nbformat; the `lc` cell
// metadata carries what the runtime knows about the last execution of the cell.
type Cell struct {
	CellType       string                       `json:"cell_type"`
	ID             string                       `json:"id"`
	Metadata       CellMetadata                 `json:"metadata"`
	Source         Source                       `json:"source"`
	ExecutionCount *int                         `json:"execution_count,omitempty"`
	Outputs        []Output                     `json:"outputs,omitempty"`
	Attachments    map[string]map[string]string `json:"attachments,omitempty"`
}

// MarshalJSON writes the cell in the shape nbformat requires for its type: a code cell always carries
// execution_count (null when it never ran) and outputs (an empty list), and a markdown or raw cell
// carries neither. A reader that validates a notebook therefore accepts an export.
func (c Cell) MarshalJSON() ([]byte, error) {
	fields := make(map[string]json.RawMessage, 6)
	put := func(key string, value any) error {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		fields[key] = raw
		return nil
	}
	if err := put("cell_type", c.CellType); err != nil {
		return nil, err
	}
	if err := put("id", c.ID); err != nil {
		return nil, err
	}
	if err := put("metadata", c.Metadata); err != nil {
		return nil, err
	}
	if err := put("source", c.Source); err != nil {
		return nil, err
	}
	if c.CellType == CellCode {
		if c.ExecutionCount == nil {
			fields["execution_count"] = json.RawMessage("null")
		} else if err := put("execution_count", *c.ExecutionCount); err != nil {
			return nil, err
		}
		if len(c.Outputs) == 0 {
			fields["outputs"] = json.RawMessage("[]")
		} else if err := put("outputs", c.Outputs); err != nil {
			return nil, err
		}
	}
	if len(c.Attachments) > 0 {
		if err := put("attachments", c.Attachments); err != nil {
			return nil, err
		}
	}
	return json.Marshal(fields)
}

// CellMetadata is the cell metadata: the `lc` extension plus any other key, kept verbatim.
type CellMetadata struct {
	LC    *CellLC                    `json:"-"`
	Extra map[string]json.RawMessage `json:"-"`
}

// CellLC is the `lc` cell metadata: the cell's language, how its last execution ended, and the
// kernel generation and request that produced it.
type CellLC struct {
	Language       string     `json:"language,omitempty"`
	Status         string     `json:"status,omitempty"`
	StartedAt      *time.Time `json:"startedAt,omitempty"`
	DurationMS     int64      `json:"durationMs,omitempty"`
	Kernel         string     `json:"kernel,omitempty"`
	Generation     uint64     `json:"generation,omitempty"`
	RequestID      string     `json:"requestId,omitempty"`
	ExitCode       *int       `json:"exitCode,omitempty"`
	StateStatus    string     `json:"stateStatus,omitempty"`
	PartialEffects bool       `json:"partialEffects,omitempty"`
	Truncated      bool       `json:"truncated,omitempty"`
}

// Output is one cell output: a stream, a display or execute result, or an error. LC is the
// extension that ties a bounded inline payload to the artifact an execution stored; it is written
// inside metadata, the only key nbformat allows beyond an output's own fields.
type Output struct {
	OutputType     string
	Name           string
	Text           Source
	Data           map[string]MimeData
	Metadata       map[string]json.RawMessage
	ExecutionCount *int
	EName          string
	EValue         string
	Traceback      []string
	LC             *OutputLC
}

// MarshalJSON writes the output in the shape nbformat requires for its type: a stream always carries
// name and text, a display or execute result always carries data and metadata, and an error always
// carries ename, evalue and traceback. The `lc` extension is written inside metadata, because
// nbformat allows no other extra key on an output; a stream and an error carry no metadata at all, so
// their bookkeeping lives on the cell (see checkCell).
func (o Output) MarshalJSON() ([]byte, error) {
	fields := make(map[string]json.RawMessage, 8)
	put := func(key string, value any) error {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		fields[key] = raw
		return nil
	}
	if err := put("output_type", o.OutputType); err != nil {
		return nil, err
	}
	switch o.OutputType {
	case OutputStream:
		name := o.Name
		if name == "" {
			name = StreamStdout
		}
		if err := put("name", name); err != nil {
			return nil, err
		}
		if err := put("text", o.Text); err != nil {
			return nil, err
		}
	case OutputDisplay, OutputExecute:
		data := o.Data
		if data == nil {
			data = map[string]MimeData{}
		}
		if err := put("data", data); err != nil {
			return nil, err
		}
		metadata := make(map[string]json.RawMessage, len(o.Metadata)+1)
		for key, raw := range o.Metadata {
			metadata[key] = raw
		}
		if o.LC != nil {
			raw, err := json.Marshal(o.LC)
			if err != nil {
				return nil, err
			}
			metadata["lc"] = raw
		}
		if err := put("metadata", metadata); err != nil {
			return nil, err
		}
		if o.OutputType == OutputExecute {
			if err := put("execution_count", o.ExecutionCount); err != nil {
				return nil, err
			}
		}
	case OutputError:
		if err := put("ename", o.EName); err != nil {
			return nil, err
		}
		if err := put("evalue", o.EValue); err != nil {
			return nil, err
		}
		traceback := o.Traceback
		if traceback == nil {
			traceback = []string{}
		}
		if err := put("traceback", traceback); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("ipynb output: unknown output_type %q", o.OutputType)
	}
	return json.Marshal(fields)
}

// UnmarshalJSON reads the standard nbformat output shape and metadata.lc extension.
func (o *Output) UnmarshalJSON(data []byte) error {
	fields, err := objectFields(data)
	if err != nil {
		return err
	}
	if raw, ok := fields["metadata"]; ok {
		metadata, err := objectFields(raw)
		if err != nil {
			return err
		}
		if lc, ok := metadata["lc"]; ok {
			var value OutputLC
			if err := json.Unmarshal(lc, &value); err != nil {
				return fmt.Errorf("ipynb output metadata.lc: %w", err)
			}
			o.LC = &value
			delete(metadata, "lc")
		}
		if len(metadata) > 0 {
			o.Metadata = metadata
		}
	}
	for key, raw := range fields {
		var err error
		switch key {
		case "output_type":
			err = json.Unmarshal(raw, &o.OutputType)
		case "name":
			err = json.Unmarshal(raw, &o.Name)
		case "text":
			err = json.Unmarshal(raw, &o.Text)
		case "data":
			err = json.Unmarshal(raw, &o.Data)
		case "execution_count":
			err = json.Unmarshal(raw, &o.ExecutionCount)
		case "ename":
			err = json.Unmarshal(raw, &o.EName)
		case "evalue":
			err = json.Unmarshal(raw, &o.EValue)
		case "traceback":
			err = json.Unmarshal(raw, &o.Traceback)
		}
		if err != nil {
			return fmt.Errorf("ipynb output %s: %w", key, err)
		}
	}
	return nil
}

// OutputLC is the `lc` output extension: the artifact an image (or a payload over the inline limit)
// was stored as, and whether the payload shown here is bounded.
type OutputLC struct {
	ArtifactID string `json:"artifactId,omitempty"`
	SessionID  string `json:"sessionId,omitempty"`
	MIME       string `json:"mime,omitempty"`
	Size       int64  `json:"size,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	// NotDurable marks a reference whose bytes live only in the execution runtime that produced them:
	// it is readable while that kernel is alive and not after a restart. A reference that is not
	// marked was inlined at execution time and needs no kernel.
	NotDurable bool `json:"notDurable,omitempty"`
}

// Source is a cell source or a stream text. nbformat allows a string or an array of strings; this
// type reads both and writes one string, so a stored body stays compact and diffable.
type Source string

// UnmarshalJSON accepts nbformat's string-or-array-of-strings source.
func (s *Source) UnmarshalJSON(data []byte) error {
	text, err := joinStringOrLines(data)
	if err != nil {
		return fmt.Errorf("ipynb source must be a string or an array of strings: %w", err)
	}
	*s = Source(text)
	return nil
}

// MarshalJSON writes the source as one string.
func (s Source) MarshalJSON() ([]byte, error) { return json.Marshal(string(s)) }

// MimeData is one entry of a mime bundle. nbformat allows a string or an array of strings; binary
// payloads are base64 in a string.
type MimeData string

// UnmarshalJSON accepts nbformat's string-or-array-of-strings value.
func (d *MimeData) UnmarshalJSON(data []byte) error {
	text, err := joinStringOrLines(data)
	if err != nil {
		return fmt.Errorf("ipynb mime bundle value must be a string or an array of strings: %w", err)
	}
	*d = MimeData(text)
	return nil
}

// MarshalJSON writes the value as one string.
func (d MimeData) MarshalJSON() ([]byte, error) { return json.Marshal(string(d)) }

// joinStringOrLines reads a JSON string or array of strings.
func joinStringOrLines(data []byte) (string, error) {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		return text, nil
	}
	var lines []string
	if err := json.Unmarshal(data, &lines); err != nil {
		return "", err
	}
	return strings.Join(lines, ""), nil
}

// MarshalJSON writes the known metadata keys and every extra key. A map marshals with sorted keys,
// so two runs over the same document produce the same bytes.
func (m Metadata) MarshalJSON() ([]byte, error) {
	fields := make(map[string]json.RawMessage, len(m.Extra)+3)
	for key, raw := range m.Extra {
		fields[key] = raw
	}
	for _, field := range []struct {
		key     string
		value   any
		present bool
	}{
		{"kernelspec", m.Kernelspec, m.Kernelspec != nil},
		{"language_info", m.LanguageInfo, m.LanguageInfo != nil},
		{"lc", m.LC, m.LC != nil},
	} {
		if !field.present {
			continue
		}
		raw, err := json.Marshal(field.value)
		if err != nil {
			return nil, err
		}
		fields[field.key] = raw
	}
	return json.Marshal(fields)
}

// UnmarshalJSON reads the known metadata keys and keeps every other key in Extra.
func (m *Metadata) UnmarshalJSON(data []byte) error {
	fields, err := objectFields(data)
	if err != nil {
		return err
	}
	for key, raw := range fields {
		if string(raw) == "null" {
			continue
		}
		switch key {
		case "kernelspec":
			var value Kernelspec
			if err := json.Unmarshal(raw, &value); err != nil {
				return fmt.Errorf("ipynb metadata.kernelspec: %w", err)
			}
			m.Kernelspec = &value
		case "language_info":
			var value LanguageInfo
			if err := json.Unmarshal(raw, &value); err != nil {
				return fmt.Errorf("ipynb metadata.language_info: %w", err)
			}
			m.LanguageInfo = &value
		case "lc":
			var value LCMetadata
			if err := json.Unmarshal(raw, &value); err != nil {
				return fmt.Errorf("ipynb metadata.lc: %w", err)
			}
			m.LC = &value
		default:
			if m.Extra == nil {
				m.Extra = make(map[string]json.RawMessage, len(fields))
			}
			m.Extra[key] = raw
		}
	}
	return nil
}

// MarshalJSON writes the known cell metadata keys and every extra key.
func (m CellMetadata) MarshalJSON() ([]byte, error) {
	fields := make(map[string]json.RawMessage, len(m.Extra)+1)
	for key, raw := range m.Extra {
		fields[key] = raw
	}
	if m.LC != nil {
		raw, err := json.Marshal(m.LC)
		if err != nil {
			return nil, err
		}
		fields["lc"] = raw
	}
	return json.Marshal(fields)
}

// UnmarshalJSON reads the `lc` cell metadata and keeps every other key in Extra.
func (m *CellMetadata) UnmarshalJSON(data []byte) error {
	fields, err := objectFields(data)
	if err != nil {
		return err
	}
	for key, raw := range fields {
		if key == "lc" {
			var value CellLC
			if err := json.Unmarshal(raw, &value); err != nil {
				return fmt.Errorf("ipynb cell metadata.lc: %w", err)
			}
			m.LC = &value
			continue
		}
		if m.Extra == nil {
			m.Extra = make(map[string]json.RawMessage, len(fields))
		}
		m.Extra[key] = raw
	}
	return nil
}

// objectFields reads a JSON object into its raw fields. A missing or null object reads as empty.
func objectFields(data []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	return fields, nil
}

// New returns an empty document whose default cell language is language.
func New(language string) Document {
	doc := Document{NBFormat: Format, NBFormatMinor: FormatMinor, Cells: []Cell{}}
	doc.Metadata.LC = &LCMetadata{}
	doc.SetDefaultLanguage(language)
	return doc
}

// LC returns the notebook extension, creating it when the document has none.
func (d *Document) Extension() *LCMetadata {
	if d.Metadata.LC == nil {
		d.Metadata.LC = &LCMetadata{}
	}
	return d.Metadata.LC
}

// SetDefaultLanguage records the language a cell without its own language executes on, and keeps the
// standard kernelspec/language_info metadata in step so an export is readable by Jupyter.
func (d *Document) SetDefaultLanguage(language string) {
	language = strings.TrimSpace(language)
	d.Extension().DefaultLanguage = language
	if language == "" {
		return
	}
	d.Metadata.Kernelspec = &Kernelspec{Name: "lc-" + language, DisplayName: language + " (LumenCortex)", Language: language}
	if d.Metadata.LanguageInfo == nil || d.Metadata.LanguageInfo.Name == "" {
		d.Metadata.LanguageInfo = &LanguageInfo{Name: language}
	}
}

// DefaultLanguage is the language of a cell that names none: the `lc` default, then the standard
// language_info, then the kernelspec language.
func (d Document) DefaultLanguage() string {
	if lc := d.Metadata.LC; lc != nil && strings.TrimSpace(lc.DefaultLanguage) != "" {
		return strings.TrimSpace(lc.DefaultLanguage)
	}
	if info := d.Metadata.LanguageInfo; info != nil && strings.TrimSpace(info.Name) != "" {
		return strings.TrimSpace(info.Name)
	}
	if spec := d.Metadata.Kernelspec; spec != nil {
		return strings.TrimSpace(spec.Language)
	}
	return ""
}

// CellLanguage is the language one cell executes on: its own `lc` language, then the language a
// VS Code notebook records, then the document default. A cell that is not code has no execution
// language, so it reports none.
func (d Document) CellLanguage(cell Cell) string {
	if cell.CellType != "" && cell.CellType != CellCode {
		return ""
	}
	if lc := cell.Metadata.LC; lc != nil && strings.TrimSpace(lc.Language) != "" {
		return strings.TrimSpace(lc.Language)
	}
	if raw, ok := cell.Metadata.Extra["vscode"]; ok {
		var vscode struct {
			LanguageID string `json:"languageId"`
		}
		if err := json.Unmarshal(raw, &vscode); err == nil && strings.TrimSpace(vscode.LanguageID) != "" {
			return strings.TrimSpace(vscode.LanguageID)
		}
	}
	return d.DefaultLanguage()
}

// SetCellLanguage records the language one cell executes on.
func SetCellLanguage(cell *Cell, language string) {
	language = strings.TrimSpace(language)
	if language == "" && cell.Metadata.LC == nil {
		return
	}
	if cell.Metadata.LC == nil {
		cell.Metadata.LC = &CellLC{}
	}
	cell.Metadata.LC.Language = language
}

// Languages lists the languages of the document's code cells in first-use order, then the default
// language when no code cell names one.
func (d Document) Languages() []string {
	seen := make(map[string]bool, 2)
	out := make([]string, 0, 2)
	for _, cell := range d.Cells {
		if cell.CellType != CellCode {
			continue
		}
		language := d.CellLanguage(cell)
		if language == "" || seen[language] {
			continue
		}
		seen[language] = true
		out = append(out, language)
	}
	if len(out) == 0 {
		if language := d.DefaultLanguage(); language != "" {
			out = append(out, language)
		}
	}
	return out
}

// CountCode counts the code cells of the document.
func (d Document) CountCode() int {
	count := 0
	for _, cell := range d.Cells {
		if cell.CellType == CellCode {
			count++
		}
	}
	return count
}

// Index returns the position of the cell with id, or -1.
func (d Document) Index(id string) int {
	for i := range d.Cells {
		if d.Cells[i].ID == id {
			return i
		}
	}
	return -1
}

// Find returns the cell with id.
func (d Document) Find(id string) (Cell, bool) {
	if i := d.Index(id); i >= 0 {
		return d.Cells[i], true
	}
	return Cell{}, false
}

// Insert adds cell at index (0..len); index < 0 or past the end appends.
func (d *Document) Insert(index int, cell Cell) error {
	if len(d.Cells) >= MaxCells {
		return fmt.Errorf("the notebook already holds %d cells, the limit", MaxCells)
	}
	if index < 0 || index > len(d.Cells) {
		index = len(d.Cells)
	}
	if cell.ID == "" {
		cell.ID = NewCellID()
	}
	if d.Index(cell.ID) >= 0 {
		return fmt.Errorf("the notebook already holds a cell %q", cell.ID)
	}
	if err := checkCell(&cell); err != nil {
		return err
	}
	d.Cells = append(d.Cells, Cell{})
	copy(d.Cells[index+1:], d.Cells[index:])
	d.Cells[index] = cell
	return nil
}

// Replace writes cell over the cell with the same id.
func (d *Document) Replace(cell Cell) error {
	index := d.Index(cell.ID)
	if index < 0 {
		return fmt.Errorf("the notebook holds no cell %q", cell.ID)
	}
	if err := checkCell(&cell); err != nil {
		return err
	}
	d.Cells[index] = cell
	return nil
}

// Remove deletes the cell with id.
func (d *Document) Remove(id string) error {
	index := d.Index(id)
	if index < 0 {
		return fmt.Errorf("the notebook holds no cell %q", id)
	}
	d.Cells = append(d.Cells[:index], d.Cells[index+1:]...)
	return nil
}

// Move puts the cell with id at index (0..len-1).
func (d *Document) Move(id string, index int) error {
	from := d.Index(id)
	if from < 0 {
		return fmt.Errorf("the notebook holds no cell %q", id)
	}
	if index < 0 || index >= len(d.Cells) {
		index = len(d.Cells) - 1
	}
	if from == index {
		return nil
	}
	cell := d.Cells[from]
	d.Cells = append(d.Cells[:from], d.Cells[from+1:]...)
	d.Cells = append(d.Cells, Cell{})
	copy(d.Cells[index+1:], d.Cells[index:])
	d.Cells[index] = cell
	return nil
}

// AppendHistory records one executed cell, keeping the newest MaxHistory entries.
func (m *LCMetadata) AppendHistory(entry HistoryEntry) {
	m.History = append(m.History, entry)
	if len(m.History) > MaxHistory {
		m.History = append([]HistoryEntry(nil), m.History[len(m.History)-MaxHistory:]...)
	}
}

// NewCellID is a 96-bit random cell identity.
func NewCellID() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand does not fail on a supported platform; fall back to a time-based identity so a
		// missing cell id cannot take the notebook write down with it.
		return fmt.Sprintf("c%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

// Parse reads an ipynb document, normalizes it and enforces the limits above. It never executes
// anything and never touches a kernel.
func Parse(data string) (Document, error) {
	var doc Document
	if err := json.Unmarshal([]byte(data), &doc); err != nil {
		return Document{}, fmt.Errorf("not an ipynb document: %w", err)
	}
	if err := Normalize(&doc); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// Normalize completes and checks a document in place: nbformat 4, known cell types, unique ids, the
// per-cell language, bounded outputs and a bounded history. A document that cannot be stored as it
// stands is refused rather than trimmed, except for outputs, which are bounded with a visible mark.
func Normalize(doc *Document) error {
	if doc.NBFormat == 0 {
		doc.NBFormat = Format
	}
	if doc.NBFormat != Format {
		return fmt.Errorf("unsupported nbformat %d.%d: only nbformat 4 is supported", doc.NBFormat, doc.NBFormatMinor)
	}
	if doc.NBFormatMinor < 0 || doc.NBFormatMinor > FormatMinor {
		doc.NBFormatMinor = FormatMinor
	}
	if doc.Cells == nil {
		doc.Cells = []Cell{}
	}
	if len(doc.Cells) > MaxCells {
		return fmt.Errorf("the notebook holds %d cells, more than the %d limit", len(doc.Cells), MaxCells)
	}
	if doc.Metadata.LC != nil {
		if len(doc.Metadata.LC.History) > MaxHistory {
			doc.Metadata.LC.History = doc.Metadata.LC.History[len(doc.Metadata.LC.History)-MaxHistory:]
		}
		if doc.Metadata.LC.ExecutionCount < 0 {
			doc.Metadata.LC.ExecutionCount = 0
		}
	}
	seen := make(map[string]bool, len(doc.Cells))
	for i := range doc.Cells {
		cell := &doc.Cells[i]
		if cell.CellType == "" {
			return fmt.Errorf("cell %d has no cell_type", i)
		}
		switch cell.CellType {
		case CellCode, CellMarkdown, CellRaw:
		default:
			return fmt.Errorf("cell %d has unknown cell_type %q", i, cell.CellType)
		}
		if cell.ID == "" || seen[cell.ID] {
			cell.ID = NewCellID()
			for seen[cell.ID] {
				cell.ID = NewCellID()
			}
		}
		seen[cell.ID] = true
		if err := checkCell(cell); err != nil {
			return fmt.Errorf("cell %q: %w", cell.ID, err)
		}
		if cell.CellType != CellCode {
			cell.Outputs, cell.ExecutionCount = nil, nil
			if cell.Metadata.LC != nil {
				cell.Metadata.LC.Status = ""
				cell.Metadata.LC.ExitCode = nil
			}
			continue
		}
		if language := doc.CellLanguage(*cell); language != "" {
			SetCellLanguage(cell, language)
		}
	}
	return nil
}

// checkCell validates one cell and bounds its source and outputs.
func checkCell(cell *Cell) error {
	if !utf8.ValidString(string(cell.Source)) {
		return errors.New("source must be valid UTF-8")
	}
	if len(cell.Source) > MaxCellSourceBytes {
		return fmt.Errorf("source is %d bytes, more than the %d byte limit", len(cell.Source), MaxCellSourceBytes)
	}
	for _, output := range cell.Outputs {
		switch output.OutputType {
		case OutputStream, OutputDisplay, OutputExecute, OutputError:
		default:
			return fmt.Errorf("unknown output_type %q", output.OutputType)
		}
	}
	cell.Outputs = BoundOutputs(cell.Outputs)
	// A stream and an error output carry no metadata in nbformat, so the `lc` extension cannot record
	// their truncation. A bounded stream records it on the cell instead, where the stored document
	// still says that output was cut.
	for _, output := range cell.Outputs {
		if output.LC == nil || !output.LC.Truncated {
			continue
		}
		if output.OutputType != OutputStream && output.OutputType != OutputError {
			continue
		}
		if cell.Metadata.LC == nil {
			cell.Metadata.LC = &CellLC{}
		}
		cell.Metadata.LC.Truncated = true
	}
	if cell.ExecutionCount != nil && *cell.ExecutionCount < 0 {
		cell.ExecutionCount = nil
	}
	return nil
}

// BoundOutputs returns outputs bounded to MaxCellOutputBytes in total and MaxInlineBytes per payload.
// Text over the inline limit is cut with a visible mark; an image over it is dropped with the
// truncation flag set, because half a base64 image is not an image.
func BoundOutputs(outputs []Output) []Output {
	if len(outputs) == 0 {
		return nil
	}
	total := 0
	dropped := false
	out := make([]Output, 0, len(outputs)+1)
	for _, output := range outputs {
		// Each payload is bounded first: an oversized text block is cut with a mark, an oversized
		// image is dropped with the flag, and only then is the cell's own budget applied.
		output = boundPayloads(output)
		size := outputSize(output)
		if total+size > MaxCellOutputBytes {
			dropped = true
			continue
		}
		out = append(out, output)
		total += size
	}
	if dropped {
		out = append(out, Output{
			OutputType: OutputStream, Name: StreamStdout, Text: Source(TruncatedOutputMsg),
			LC: &OutputLC{Truncated: true},
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// boundPayloads bounds one output's own payloads.
func boundPayloads(output Output) Output {
	if len(output.Text) > MaxInlineBytes {
		output.Text = Source(cutText(string(output.Text)))
		output = markTruncated(output)
	}
	for mime, data := range output.Data {
		if len(data) <= MaxInlineBytes {
			continue
		}
		delete(output.Data, mime)
		output = markTruncated(output)
	}
	if output.Data != nil && len(output.Data) == 0 {
		output.Data = nil
	}
	return output
}

// markTruncated records that a payload was bounded.
func markTruncated(output Output) Output {
	if output.LC == nil {
		output.LC = &OutputLC{}
	}
	output.LC.Truncated = true
	return output
}

// cutText cuts text to MaxInlineBytes without splitting a rune and appends the mark.
func cutText(text string) string {
	limit := MaxInlineBytes - len(TruncatedOutputMsg)
	if limit < 0 {
		limit = 0
	}
	cut := text
	if len(cut) > limit {
		cut = cut[:limit]
		for len(cut) > 0 && !utf8.ValidString(cut) {
			cut = cut[:len(cut)-1]
		}
	}
	return cut + TruncatedOutputMsg
}

// outputSize is the payload bytes one output keeps.
func outputSize(output Output) int {
	size := len(output.Text)
	for _, data := range output.Data {
		size += len(data)
	}
	for _, line := range output.Traceback {
		size += len(line)
	}
	return size
}

// Marshal writes the canonical stored form: one compact JSON object whose metadata keys are sorted.
func Marshal(doc Document) (string, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	if len(raw) > MaxDocumentBytes {
		return "", fmt.Errorf("the notebook is %d bytes, more than the %d byte limit; remove a cell or clear its outputs", len(raw), MaxDocumentBytes)
	}
	return string(raw), nil
}

// Fit writes the canonical stored form, dropping the recorded outputs of the oldest executed cells
// until the document fits MaxDocumentBytes. Sources are never dropped: a document whose cells alone
// are too large is refused. A cell whose outputs were dropped keeps a visible truncation mark, so the
// notebook says what it lost instead of pretending the execution produced nothing.
func Fit(doc *Document) (string, error) {
	for {
		raw, err := json.Marshal(*doc)
		if err != nil {
			return "", err
		}
		if len(raw) <= MaxDocumentBytes {
			return string(raw), nil
		}
		if !dropOldestOutputs(doc) {
			return "", fmt.Errorf("the notebook is %d bytes without any output, more than the %d byte limit", len(raw), MaxDocumentBytes)
		}
	}
}

// dropOldestOutputs clears the outputs of the executed cell that ran earliest. It reports whether it
// dropped anything.
func dropOldestOutputs(doc *Document) bool {
	oldest := -1
	var oldestAt time.Time
	for i := range doc.Cells {
		cell := &doc.Cells[i]
		if len(cell.Outputs) == 0 {
			continue
		}
		at := time.Time{}
		if lc := cell.Metadata.LC; lc != nil && lc.StartedAt != nil {
			at = *lc.StartedAt
		}
		if oldest < 0 || at.Before(oldestAt) {
			oldest, oldestAt = i, at
		}
	}
	if oldest < 0 {
		return false
	}
	cell := &doc.Cells[oldest]
	cell.Outputs = nil
	cell.ExecutionCount = nil
	if cell.Metadata.LC == nil {
		cell.Metadata.LC = &CellLC{}
	}
	cell.Metadata.LC.Truncated = true
	return true
}

// Export writes the .ipynb form: the same document, indented and with the standard metadata keys a
// Jupyter reader expects. It is not stored, so it is not bounded by MaxDocumentBytes.
func Export(doc Document) (string, error) {
	raw, err := json.MarshalIndent(doc, "", " ")
	if err != nil {
		return "", err
	}
	return string(raw) + "\n", nil
}

// ResetExecution clears every execution result in place: outputs, execution counts, cell status and
// the notebook-wide counter. It is what an explicit replay starts from; it executes nothing.
func ResetExecution(doc *Document) {
	for i := range doc.Cells {
		cell := &doc.Cells[i]
		if cell.CellType != CellCode {
			continue
		}
		cell.Outputs = nil
		cell.ExecutionCount = nil
		if lc := cell.Metadata.LC; lc != nil {
			lc.Status = StatusIdle
			lc.ExitCode = nil
			lc.DurationMS = 0
			lc.StartedAt = nil
			lc.PartialEffects = false
			lc.Truncated = false
			lc.RequestID = ""
		}
	}
	doc.Extension().ExecutionCount = 0
}
