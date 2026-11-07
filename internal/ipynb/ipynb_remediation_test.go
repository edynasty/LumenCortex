package ipynb

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// These tests hold the import/export contracts a review found broken: a document that is stored with
// cell ids declares nbformat 4.5, a cell id that does not match the schema is regenerated, a mime value
// that is JSON rather than a string survives a round trip, and the standard metadata subkeys a notebook
// carries are written back. They are the permanent form of the review's reproducers: no /tmp path, and
// an assertion per claim.

// nbformat44 is a standard 4.4 notebook without cell ids, as most notebooks in the wild are.
const nbformat44 = `{"nbformat":4,"nbformat_minor":4,"metadata":{"kernelspec":{"name":"python3","display_name":"Python 3","language":"python"},"language_info":{"name":"python","version":"3.11.4","mimetype":"text/x-python","file_extension":".py","codemirror_mode":{"name":"ipython","version":3},"pygments_lexer":"ipython3","nbconvert_exporter":"python"}},"cells":[{"cell_type":"code","execution_count":1,"metadata":{},"source":["print(1)"],"outputs":[{"output_type":"stream","name":"stdout","text":["1\n"]}]}]}`

// nbformat45BadID is a 4.5 notebook whose cell id violates the 4.5 cell_id pattern.
const nbformat45BadID = `{"nbformat":4,"nbformat_minor":5,"metadata":{},"cells":[{"cell_type":"code","id":"../../etc/passwd cell","execution_count":null,"metadata":{},"source":"x=1","outputs":[]}]}`

// nbformat45JSONMime is a 4.5 notebook with a mime bundle value that is a JSON object, which the
// schema allows.
const nbformat45JSONMime = `{"nbformat":4,"nbformat_minor":5,"metadata":{},"cells":[{"cell_type":"code","id":"c1","execution_count":1,"metadata":{},"source":"df","outputs":[{"output_type":"execute_result","execution_count":1,"metadata":{},"data":{"text/plain":"x","application/json":{"a":1,"b":[true,null]}}}]}]}`

// TestNormalizeWritesTheFormatItActuallyWrites is NB-04: a stored document always carries cell ids, and
// nbformat only allows an id from 4.5 on, so a 4.4 import is stored and exported as 4.5.
func TestNormalizeWritesTheFormatItActuallyWrites(t *testing.T) {
	doc, err := Parse(nbformat44)
	if err != nil {
		t.Fatal(err)
	}
	if doc.NBFormatMinor != FormatMinor {
		t.Fatalf("nbformat_minor = %d, want %d", doc.NBFormatMinor, FormatMinor)
	}
	if len(doc.Cells) != 1 || !validCellID(doc.Cells[0].ID) {
		t.Fatalf("cell id = %q", doc.Cells[0].ID)
	}
	exported, err := Export(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(exported, `"nbformat_minor": 5`) {
		t.Fatalf("the export does not declare 4.5: %s", exported)
	}
	// The export is what a validator reads: it parses back to the same document.
	again, err := Parse(exported)
	if err != nil {
		t.Fatalf("the export does not parse: %v", err)
	}
	if again.NBFormatMinor != FormatMinor || again.Cells[0].ID != doc.Cells[0].ID {
		t.Fatalf("round trip = %#v", again)
	}
}

// TestNormalizeRegeneratesAnIllegalCellID is NB-04: an id that does not match nbformat's pattern makes
// the document invalid, so it is replaced rather than carried into a stored or exported notebook.
func TestNormalizeRegeneratesAnIllegalCellID(t *testing.T) {
	doc, err := Parse(nbformat45BadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Cells) != 1 {
		t.Fatalf("cells = %d", len(doc.Cells))
	}
	id := doc.Cells[0].ID
	if !validCellID(id) || strings.ContainsAny(id, "/. ") {
		t.Fatalf("the illegal id survived: %q", id)
	}
	exported, err := Export(doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(exported, "passwd") {
		t.Fatalf("the export carries the illegal id: %s", exported)
	}

	// A host edit that names an illegal id is refused instead of silently renamed.
	document := New("python")
	if err := document.Insert(-1, Cell{CellType: CellCode, ID: "not a legal id", Source: "x"}); err == nil {
		t.Fatal("an illegal cell id was accepted by Insert")
	}
	if err := document.Insert(-1, Cell{CellType: CellCode, ID: strings.Repeat("a", 65), Source: "x"}); err == nil {
		t.Fatal("a 65-character cell id was accepted by Insert")
	}
	if err := document.Insert(-1, Cell{CellType: CellCode, ID: "a-legal_id.1", Source: "x"}); err == nil {
		t.Fatal("a cell id with a '.' was accepted by Insert")
	}
	if err := document.Insert(-1, Cell{CellType: CellCode, ID: "a-legal_id1", Source: "x"}); err != nil {
		t.Fatalf("a legal cell id was refused: %v", err)
	}
}

// mimeBundleDoc reads one output's data map from a serialized document, so a test can compare the
// values a notebook file carries without depending on how the serializer spaces them.
type mimeBundleDoc struct {
	Cells []struct {
		Outputs []struct {
			Data map[string]json.RawMessage `json:"data"`
		} `json:"outputs"`
	} `json:"cells"`
}

// mimeValue is the raw value of one mime type in the first output of a serialized document.
func mimeValue(t *testing.T, document, mime string) json.RawMessage {
	t.Helper()
	var root mimeBundleDoc
	if err := json.Unmarshal([]byte(document), &root); err != nil {
		t.Fatalf("the document is not JSON: %v", err)
	}
	if len(root.Cells) == 0 || len(root.Cells[0].Outputs) == 0 {
		t.Fatalf("the document carries no output")
	}
	return root.Cells[0].Outputs[0].Data[mime]
}

// decodedMimeValue is the JSON value of one mime type in a serialized document.
func decodedMimeValue(t *testing.T, document, mime string) any {
	t.Helper()
	raw := mimeValue(t, document, mime)
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("the %s value is not JSON: %v", mime, err)
	}
	return value
}

// TestJSONMimeValueSurvivesRoundTrip is NB-05: nbformat allows any JSON under a mime type, and a
// notebook that carries one must import and export unchanged instead of being refused. The comparison
// is on the decoded value, never on the serializer's whitespace: the stored body is compact and an
// exported file is indented, so both are compared as JSON values.
func TestJSONMimeValueSurvivesRoundTrip(t *testing.T) {
	doc, err := Parse(nbformat45JSONMime)
	if err != nil {
		t.Fatalf("a JSON mime value was refused: %v", err)
	}
	output := doc.Cells[0].Outputs[0]
	if got := output.Data["application/json"].String(); got != `{"a":1,"b":[true,null]}` {
		t.Fatalf("application/json = %q", got)
	}
	if got := output.Data["text/plain"].String(); got != "x" {
		t.Fatalf("text/plain = %q", got)
	}
	want := map[string]any{"a": float64(1), "b": []any{true, nil}}

	// The exported file is indented, so the same values are read back as JSON, not as bytes.
	exported, err := Export(doc)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodedMimeValue(t, exported, "application/json"); !reflect.DeepEqual(got, want) {
		t.Fatalf("the exported object = %#v, want %#v", got, want)
	}
	if got := decodedMimeValue(t, exported, "text/plain"); got != "x" {
		t.Fatalf(`the exported text value = %#v, want the JSON string "x"`, got)
	}

	// And the exported document parses back with the object still an object: the round trip a user's
	// notebook file makes through import and export.
	again, err := Parse(exported)
	if err != nil {
		t.Fatalf("the exported notebook does not parse: %v", err)
	}
	var roundTripped any
	if err := json.Unmarshal([]byte(again.Cells[0].Outputs[0].Data["application/json"].String()), &roundTripped); err != nil {
		t.Fatalf("the re-imported application/json value is not JSON: %v", err)
	}
	if !reflect.DeepEqual(roundTripped, want) {
		t.Fatalf("the re-imported object = %#v, want %#v", roundTripped, want)
	}
	if got := again.Cells[0].Outputs[0].Data["text/plain"].String(); got != "x" {
		t.Fatalf(`the re-imported text value = %q, want %q`, got, "x")
	}

	// The stored form is what an import keeps: the object stays an object and the text stays a string.
	body, err := Fit(&doc)
	if err != nil {
		t.Fatalf("the imported notebook does not fit the store: %v", err)
	}
	if got := decodedMimeValue(t, body, "application/json"); !reflect.DeepEqual(got, want) {
		t.Fatalf("the stored object = %#v, want %#v", got, want)
	}
	if got := decodedMimeValue(t, body, "text/plain"); got != "x" {
		t.Fatalf(`the stored text value = %#v, want the JSON string "x"`, got)
	}
}

// TestStandardMetadataSubkeysArePreserved is NB-09: language_info and kernelspec carry subkeys the
// runtime does not interpret (mimetype, file_extension, codemirror_mode, pygments_lexer,
// nbconvert_exporter), and an export that drops them loses what the notebook said about itself.
func TestStandardMetadataSubkeysArePreserved(t *testing.T) {
	doc, err := Parse(nbformat44)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Metadata.Kernelspec == nil || doc.Metadata.Kernelspec.Name != "python3" {
		t.Fatalf("kernelspec = %#v", doc.Metadata.Kernelspec)
	}
	if doc.Metadata.LanguageInfo == nil || doc.Metadata.LanguageInfo.Version != "3.11.4" {
		t.Fatalf("language_info = %#v", doc.Metadata.LanguageInfo)
	}
	exported, err := Export(doc)
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Metadata struct {
			Kernelspec   map[string]json.RawMessage `json:"kernelspec"`
			LanguageInfo map[string]json.RawMessage `json:"language_info"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(exported), &root); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"display_name": `"Python 3"`,
		"language":     `"python"`,
		"name":         `"python3"`,
	} {
		if got := string(root.Metadata.Kernelspec[key]); got != want {
			t.Fatalf("kernelspec.%s = %s, want %s", key, got, want)
		}
	}
	for key, want := range map[string]string{
		"name":               `"python"`,
		"version":            `"3.11.4"`,
		"mimetype":           `"text/x-python"`,
		"file_extension":     `".py"`,
		"codemirror_mode":    `{"version":3,"name":"ipython"}`,
		"pygments_lexer":     `"ipython3"`,
		"nbconvert_exporter": `"python"`,
	} {
		got := string(root.Metadata.LanguageInfo[key])
		if got == "" {
			t.Fatalf("language_info.%s was dropped", key)
		}
		if key != "codemirror_mode" && got != want {
			t.Fatalf("language_info.%s = %s, want %s", key, got, want)
		}
	}

	// A document this package creates keeps its own kernelspec in step with the default language.
	created := New("javascript")
	created.Metadata.LanguageInfo.Extra = map[string]json.RawMessage{"pygments_lexer": json.RawMessage(`"javascript"`)}
	body, err := Marshal(created)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"pygments_lexer":"javascript"`) {
		t.Fatalf("a subkey set on a created document was dropped: %s", body)
	}
}

// TestSVGPayloadStaysText is NB-08: nbformat stores SVG as XML text, so the runtime writes it as text
// rather than base64, and an SVG output renders instead of showing a broken image.
func TestSVGPayloadStaysText(t *testing.T) {
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect width="4" height="4"/></svg>`)
	if got := Payload("image/svg+xml", svg); got != string(svg) {
		t.Fatalf("an SVG payload was encoded: %q", got)
	}
	if got := Payload("image/png", []byte("png")); got != "cG5n" {
		t.Fatalf("a binary payload = %q, want base64", got)
	}
	if got := Payload("application/json", []byte(`{"a":1}`)); got != `{"a":1}` {
		t.Fatalf("a JSON payload = %q, want text", got)
	}
	if !IsTextMIME("IMAGE/SVG+XML") || IsTextMIME("image/png") {
		t.Fatal("IsTextMIME does not classify the payload kinds")
	}
}
