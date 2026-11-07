package ipynb

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// standardNotebook is a nbformat 4.5 document as Jupyter and VS Code write one: array sources, a
// vscode language id, three output kinds, an unknown metadata key and a base64 image.
const standardNotebook = `{
 "nbformat": 4,
 "nbformat_minor": 5,
 "metadata": {
  "kernelspec": {"name": "python3", "display_name": "Python 3", "language": "python"},
  "language_info": {"name": "python", "version": "3.12.0"},
  "title": "kept verbatim"
 },
 "cells": [
  {"cell_type": "markdown", "id": "md-1", "metadata": {}, "source": ["# Analysis\n", "\n", "Read the data.\n"]},
  {"cell_type": "code", "id": "py-1", "metadata": {"vscode": {"languageId": "python"}}, "execution_count": 3,
   "source": ["print(1)\n"],
   "outputs": [
    {"output_type": "stream", "name": "stdout", "text": ["1\n"]},
    {"output_type": "display_data", "data": {"image/png": "aGk="}, "metadata": {}},
    {"output_type": "error", "ename": "ValueError", "evalue": "bad", "traceback": ["line1", "line2"]}
   ]},
  {"cell_type": "code", "id": "js-1", "metadata": {"lc": {"language": "javascript"}}, "execution_count": 4,
   "source": ["1 + 1"], "outputs": []}
 ]
}`

func TestParseStandardNotebook(t *testing.T) {
	doc, err := Parse(standardNotebook)
	if err != nil {
		t.Fatal(err)
	}
	if doc.NBFormat != Format || doc.NBFormatMinor != FormatMinor {
		t.Fatalf("format = %d.%d", doc.NBFormat, doc.NBFormatMinor)
	}
	if doc.DefaultLanguage() != "python" {
		t.Fatalf("default language = %q", doc.DefaultLanguage())
	}
	if len(doc.Cells) != 3 {
		t.Fatalf("cells = %d", len(doc.Cells))
	}
	if got := string(doc.Cells[0].Source); got != "# Analysis\n\nRead the data.\n" {
		t.Fatalf("markdown source = %q", got)
	}
	if got := doc.CellLanguage(doc.Cells[0]); got != "" {
		t.Fatalf("a markdown cell has no execution language, got %q", got)
	}
	// The notebook default still applies to code cells that name none.
	if got := doc.DefaultLanguage(); got != "python" {
		t.Fatalf("default language = %q", got)
	}
	code := doc.Cells[1]
	if got := doc.CellLanguage(code); got != "python" {
		t.Fatalf("code language from vscode metadata = %q", got)
	}
	if code.ExecutionCount == nil || *code.ExecutionCount != 3 {
		t.Fatalf("execution count = %v", code.ExecutionCount)
	}
	if len(code.Outputs) != 3 {
		t.Fatalf("outputs = %d", len(code.Outputs))
	}
	if code.Outputs[0].OutputType != OutputStream || string(code.Outputs[0].Text) != "1\n" {
		t.Fatalf("stream output = %#v", code.Outputs[0])
	}
	if string(code.Outputs[1].Data["image/png"]) != "aGk=" {
		t.Fatalf("image output = %#v", code.Outputs[1].Data)
	}
	if code.Outputs[2].OutputType != OutputError || code.Outputs[2].EName != "ValueError" || len(code.Outputs[2].Traceback) != 2 {
		t.Fatalf("error output = %#v", code.Outputs[2])
	}
	if got := doc.CellLanguage(doc.Cells[2]); got != "javascript" {
		t.Fatalf("explicit per-cell language = %q", got)
	}
	if languages := doc.Languages(); len(languages) != 2 || languages[0] != "python" || languages[1] != "javascript" {
		t.Fatalf("languages = %v", languages)
	}
	if raw := string(doc.Metadata.Extra["title"]); raw != `"kept verbatim"` {
		t.Fatalf("unknown metadata key = %s", raw)
	}
	// The per-cell language is recorded so a reader does not have to guess from the document default.
	if code.Metadata.LC == nil || code.Metadata.LC.Language != "python" {
		t.Fatalf("code cell lc metadata = %#v", code.Metadata.LC)
	}
}

func TestMarshalIsStableAndCompact(t *testing.T) {
	doc, err := Parse(standardNotebook)
	if err != nil {
		t.Fatal(err)
	}
	first, err := Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("marshalling the same document twice produced different bytes")
	}
	if strings.Contains(first, "\n") {
		t.Fatal("the stored form must be one line: a multi-line body would expose cell headings")
	}
	again, err := Parse(first)
	if err != nil {
		t.Fatal(err)
	}
	third, err := Marshal(again)
	if err != nil {
		t.Fatal(err)
	}
	if first != third {
		t.Fatalf("round trip changed the document:\n%s\n%s", first, third)
	}
	exported, err := Export(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(exported, "\n") || !strings.Contains(exported, "\n \"nbformat\": 4,") {
		t.Fatalf("exported form = %q", exported[:80])
	}
	var reparsed map[string]any
	if err := json.Unmarshal([]byte(exported), &reparsed); err != nil {
		t.Fatal(err)
	}
}

func TestParseRefusesUnsupportedFormat(t *testing.T) {
	if _, err := Parse(`{"nbformat": 3, "cells": []}`); err == nil {
		t.Fatal("nbformat 3 was accepted")
	}
	if _, err := Parse(`not json`); err == nil {
		t.Fatal("a non-JSON body was accepted")
	}
}

func TestLimits(t *testing.T) {
	cells := make([]Cell, 0, MaxCells+1)
	for i := 0; i <= MaxCells; i++ {
		cells = append(cells, Cell{CellType: CellMarkdown, ID: fmt.Sprintf("c%d", i), Source: "text"})
	}
	if err := Normalize(&Document{NBFormat: Format, Cells: cells}); err == nil {
		t.Fatal("a document past the cell limit was accepted")
	}

	big := strings.Repeat("x", MaxCellSourceBytes+1)
	if err := Normalize(&Document{NBFormat: Format, Cells: []Cell{{CellType: CellMarkdown, Source: Source(big)}}}); err == nil {
		t.Fatal("a cell past the source limit was accepted")
	}
}

func TestOutputsAreBounded(t *testing.T) {
	text := strings.Repeat("a", MaxCellOutputBytes*2)
	image := strings.Repeat("i", MaxInlineBytes*2)
	doc := Document{NBFormat: Format, Cells: []Cell{{
		CellType: CellCode, ID: "c1", Source: "print()",
		Outputs: []Output{
			{OutputType: OutputStream, Name: StreamStdout, Text: Source(text)},
			{OutputType: OutputDisplay, Data: map[string]MimeData{"image/png": MimeData(image)}},
			{OutputType: OutputStream, Name: StreamStdout, Text: "tail"},
		},
	}}}
	if err := Normalize(&doc); err != nil {
		t.Fatal(err)
	}
	outputs := doc.Cells[0].Outputs
	if len(outputs) == 0 || len(outputs[0].Text) > MaxCellOutputBytes {
		t.Fatalf("text output was not bounded: %d bytes", len(outputs[0].Text))
	}
	if !strings.Contains(string(outputs[0].Text), TruncatedOutputMsg) || outputs[0].LC == nil || !outputs[0].LC.Truncated {
		t.Fatalf("truncated text output = %#v", outputs[0])
	}
	var images int
	for _, output := range outputs {
		if _, ok := output.Data["image/png"]; ok {
			images++
		}
	}
	if images != 0 {
		t.Fatal("an image past the inline limit was kept")
	}
	total := 0
	for _, output := range outputs {
		total += outputSize(output)
	}
	if total > MaxCellOutputBytes {
		t.Fatalf("cell outputs total %d bytes, more than %d", total, MaxCellOutputBytes)
	}
}

func TestFitDropsOldestOutputsToStayStorable(t *testing.T) {
	doc := New("python")
	start := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	for i := range 8 {
		count := i + 1
		at := start.Add(time.Duration(i) * time.Minute)
		text := strings.Repeat("o", MaxCellOutputBytes-MaxInlineBytes)
		cell := Cell{
			CellType: CellCode, ID: fmt.Sprintf("c%d", i), Source: "run()", ExecutionCount: &count,
			Metadata: CellMetadata{LC: &CellLC{Language: "python", Status: StatusOK, StartedAt: &at}},
			Outputs:  []Output{{OutputType: OutputStream, Name: StreamStdout, Text: Source(text)}},
		}
		if err := doc.Insert(-1, cell); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Marshal(doc); err == nil {
		t.Fatal("the unfitted document was reported as storable")
	}
	body, err := Fit(&doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > MaxDocumentBytes {
		t.Fatalf("fitted body = %d bytes", len(body))
	}
	var dropped int
	for _, cell := range doc.Cells {
		if len(cell.Outputs) == 0 && cell.Metadata.LC != nil && cell.Metadata.LC.Truncated {
			dropped++
		}
	}
	if dropped == 0 {
		t.Fatal("no cell records that its outputs were dropped")
	}
	// The oldest executions lose their outputs first, and the sources stay.
	if len(doc.Cells[0].Outputs) != 0 || len(doc.Cells[len(doc.Cells)-1].Outputs) == 0 {
		t.Fatalf("the wrong cells lost their outputs: first=%d last=%d",
			len(doc.Cells[0].Outputs), len(doc.Cells[len(doc.Cells)-1].Outputs))
	}
	if string(doc.Cells[0].Source) != "run()" {
		t.Fatal("a source was dropped")
	}
}

func TestCellEdits(t *testing.T) {
	doc := New("python")
	if err := doc.Insert(-1, Cell{CellType: CellCode, ID: "a", Source: "1"}); err != nil {
		t.Fatal(err)
	}
	if err := doc.Insert(0, Cell{CellType: CellMarkdown, ID: "b", Source: "text"}); err != nil {
		t.Fatal(err)
	}
	if doc.Cells[0].ID != "b" || doc.Cells[1].ID != "a" {
		t.Fatalf("insert order = %v", []string{doc.Cells[0].ID, doc.Cells[1].ID})
	}
	if err := doc.Insert(-1, Cell{CellType: CellCode, ID: "a", Source: "2"}); err == nil {
		t.Fatal("a duplicate cell id was accepted")
	}
	if err := doc.Move("a", 0); err != nil {
		t.Fatal(err)
	}
	if doc.Cells[0].ID != "a" {
		t.Fatalf("move order = %v", doc.Cells[0].ID)
	}
	replacement := Cell{CellType: CellCode, ID: "a", Source: "2 + 2"}
	if err := doc.Replace(replacement); err != nil {
		t.Fatal(err)
	}
	if string(doc.Cells[0].Source) != "2 + 2" {
		t.Fatal("replace did not write the cell")
	}
	if err := doc.Remove("b"); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cells) != 1 {
		t.Fatalf("cells after remove = %d", len(doc.Cells))
	}
	if err := doc.Remove("missing"); err == nil {
		t.Fatal("removing an unknown cell was accepted")
	}
	if err := doc.Replace(Cell{CellType: CellCode, ID: "missing", Source: "x"}); err == nil {
		t.Fatal("replacing an unknown cell was accepted")
	}
}

func TestResetExecution(t *testing.T) {
	count := 4
	at := time.Now().UTC()
	doc := New("python")
	doc.Extension().ExecutionCount = 4
	if err := doc.Insert(-1, Cell{
		CellType: CellCode, ID: "a", Source: "1", ExecutionCount: &count,
		Metadata: CellMetadata{LC: &CellLC{Language: "python", Status: StatusOK, StartedAt: &at, ExitCode: &count}},
		Outputs:  []Output{{OutputType: OutputStream, Name: StreamStdout, Text: "1\n"}},
	}); err != nil {
		t.Fatal(err)
	}
	ResetExecution(&doc)
	cell := doc.Cells[0]
	if cell.Outputs != nil || cell.ExecutionCount != nil {
		t.Fatal("reset kept an output or an execution count")
	}
	if cell.Metadata.LC.Status != StatusIdle || cell.Metadata.LC.ExitCode != nil || cell.Metadata.LC.StartedAt != nil {
		t.Fatalf("reset kept the execution status: %#v", cell.Metadata.LC)
	}
	if doc.Extension().ExecutionCount != 0 {
		t.Fatal("reset kept the notebook execution counter")
	}
	if string(cell.Source) != "1" {
		t.Fatal("reset changed the source")
	}
}

func TestHistoryIsBounded(t *testing.T) {
	extension := &LCMetadata{}
	for i := range MaxHistory + 20 {
		extension.AppendHistory(HistoryEntry{CellID: fmt.Sprintf("c%d", i), Status: StatusOK})
	}
	if len(extension.History) != MaxHistory {
		t.Fatalf("history = %d entries", len(extension.History))
	}
	if extension.History[len(extension.History)-1].CellID != fmt.Sprintf("c%d", MaxHistory+19) {
		t.Fatal("the newest history entry was dropped")
	}
}

// strictFixture is a nbformat 4.5 export the runtime writes: an empty markdown cell, a code cell with
// every output kind, and a code cell that never ran. It is the document an external validator reads.
const strictFixture = "testdata/strict-export.ipynb"

// TestExportIsStrictNbformat: the exported .ipynb is valid nbformat 4.5, not a LumenCortex dialect.
// Required fields are written even when empty (a markdown source, a code cell's outputs and
// execution_count, a display's data and metadata, a stream's text, an error's traceback), and the
// `lc` extension lives in metadata, the only extra key nbformat allows on an output.
func TestExportIsStrictNbformat(t *testing.T) {
	raw, err := os.ReadFile(strictFixture)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Parse(string(raw))
	if err != nil {
		t.Fatalf("the strict fixture did not parse: %v", err)
	}
	exported, err := Export(doc)
	if err != nil {
		t.Fatal(err)
	}
	assertStrictNbformat(t, exported)
	// Parsing the export and exporting it again changes nothing: the standard shape is stable, so a
	// notebook written here does not drift as it is read and rewritten.
	again, err := Parse(exported)
	if err != nil {
		t.Fatalf("the export did not parse: %v", err)
	}
	reexported, err := Export(again)
	if err != nil {
		t.Fatal(err)
	}
	if reexported != exported {
		t.Fatalf("the export is not stable:\n%s\n%s", exported, reexported)
	}
	// The extension survives the round trip: the fixture's reference is what a reader sees again.
	if got := again.Cells[1].Outputs[1].LC; got == nil || got.ArtifactID == "" || got.SessionID != "s1" {
		t.Fatalf("the export lost the artifact reference: %#v", got)
	}
}

// assertStrictNbformat checks the required shape of one exported document.
func assertStrictNbformat(t *testing.T, exported string) {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal([]byte(exported), &root); err != nil {
		t.Fatalf("the export is not JSON: %v", err)
	}
	if root["nbformat"] != float64(Format) || root["nbformat_minor"] != float64(FormatMinor) {
		t.Fatalf("format = %v.%v", root["nbformat"], root["nbformat_minor"])
	}
	asObject(t, root["metadata"], "notebook metadata")
	cells := asArray(t, root["cells"], "cells")
	for index, value := range cells {
		cell := asObject(t, value, fmt.Sprintf("cell %d", index))
		for _, key := range []string{"cell_type", "id", "metadata", "source"} {
			if _, ok := cell[key]; !ok {
				t.Fatalf("cell %d has no %q: %#v", index, key, cell)
			}
		}
		asObject(t, cell["metadata"], "cell metadata")
		if _, ok := cell["source"].(string); !ok {
			t.Fatalf("cell %d source is %T, not a string", index, cell["source"])
		}
		if cell["cell_type"] != CellCode {
			if _, ok := cell["outputs"]; ok {
				t.Fatalf("cell %d is %v and carries outputs", index, cell["cell_type"])
			}
			continue
		}
		if count, ok := cell["execution_count"]; !ok {
			t.Fatalf("cell %d has no execution_count", index)
		} else if count != nil {
			if _, ok := count.(float64); !ok {
				t.Fatalf("cell %d execution_count is %T", index, count)
			}
		}
		for outputIndex, outputValue := range asArray(t, cell["outputs"], "outputs") {
			assertStrictOutput(t, outputValue, fmt.Sprintf("cell %d output %d", index, outputIndex))
		}
	}
}

// assertStrictOutput checks one output: its own required fields, no extra keys, and the extension
// only where nbformat allows metadata.
func assertStrictOutput(t *testing.T, value any, what string) {
	t.Helper()
	output := asObject(t, value, what)
	if _, ok := output["lc"]; ok {
		t.Fatalf("%s carries a top-level lc extension: %#v", what, output)
	}
	switch output["output_type"] {
	case OutputStream:
		for _, key := range []string{"name", "text"} {
			if _, ok := output[key]; !ok {
				t.Fatalf("%s has no %q: %#v", what, key, output)
			}
		}
		if _, ok := output["metadata"]; ok {
			t.Fatalf("%s is a stream with metadata: %#v", what, output)
		}
	case OutputDisplay, OutputExecute:
		asObject(t, output["data"], what+" data")
		metadata := asObject(t, output["metadata"], what+" metadata")
		if lc, ok := metadata["lc"]; ok {
			asObject(t, lc, what+" metadata.lc")
		}
		if output["output_type"] == OutputExecute {
			if _, ok := output["execution_count"]; !ok {
				t.Fatalf("%s has no execution_count", what)
			}
		}
	case OutputError:
		for _, key := range []string{"ename", "evalue", "traceback"} {
			if _, ok := output[key]; !ok {
				t.Fatalf("%s has no %q: %#v", what, key, output)
			}
		}
		asArray(t, output["traceback"], what+" traceback")
	default:
		t.Fatalf("%s has an unknown output_type: %#v", what, output)
	}
}

func asObject(t *testing.T, value any, what string) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, not an object", what, value)
	}
	return object
}

func asArray(t *testing.T, value any, what string) []any {
	t.Helper()
	array, ok := value.([]any)
	if !ok {
		t.Fatalf("%s is %T, not an array", what, value)
	}
	return array
}
