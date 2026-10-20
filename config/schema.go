package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/edynasty/LumenCortex/docs"
	"github.com/santhosh-tekuri/jsonschema/v5"
)

const schemaURL = "https://raw.githubusercontent.com/edynasty/LumenCortex/main/docs/lumencortex.schema.json"

// maxFileBytes bounds a configuration file; real ones are a few KiB.
const maxFileBytes = 1 << 20

var (
	schemaOnce sync.Once
	schema     *jsonschema.Schema
	schemaErr  error
)

func compiledSchema() (*jsonschema.Schema, error) {
	schemaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.Draft = jsonschema.Draft2020
		if schemaErr = c.AddResource(schemaURL, bytes.NewReader(docs.ConfigSchema)); schemaErr != nil {
			return
		}
		schema, schemaErr = c.Compile(schemaURL)
	})
	return schema, schemaErr
}

// Violation is one schema failure: where it is and which keyword failed.
type Violation struct {
	Path    string `json:"path"`    // JSON pointer of the offending value, "" for the root
	Keyword string `json:"keyword"` // schema keyword, e.g. pattern, additionalProperties
	Detail  string `json:"detail,omitempty"`
}

// ValidationError lists the schema violations of a document. It matches ErrInvalidConfig.
//
// It never contains a value from the document: literal API keys and header values must not reach
// logs or the bridge (I12). Only JSON pointers, keywords and, for the keywords that name
// properties (additionalProperties, required, dependentRequired), the property names are kept.
type ValidationError struct {
	Violations []Violation
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Violations))
	for _, v := range e.Violations {
		p := v.Path
		if p == "" {
			p = "/"
		}
		s := p + ": " + v.Keyword
		if v.Detail != "" {
			s += " (" + v.Detail + ")"
		}
		parts = append(parts, s)
	}
	return ErrInvalidConfig.Error() + ": " + strings.Join(parts, "; ")
}

func (e *ValidationError) Is(target error) bool { return target == ErrInvalidConfig }

const maxReportedViolations = 8

// Parse decodes a document and validates it against the schema. Unknown keys, literal API keys and
// every other schema violation are errors; the result is the typed document.
func Parse(raw []byte) (File, error) {
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	generic, err := decodeGeneric(raw)
	if err != nil {
		return File{}, err
	}
	// A retired key gets its replacement named, not the schema's generic unknown-property error.
	if obj, ok := generic.(map[string]any); ok {
		if _, retired := obj["subagentModel"]; retired {
			return File{}, fmt.Errorf("%w: %w", ErrInvalidConfig, ErrSubagentModelRetired)
		}
	}
	if err := validateGeneric(generic); err != nil {
		return File{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var f File
	if err := dec.Decode(&f); err != nil {
		return File{}, decodeError(err)
	}
	if err := validateAgents(f.Agents); err != nil {
		return File{}, err
	}
	return f, nil
}

// Validate checks a typed document against the schema, as Save does before writing.
func Validate(f File) error {
	raw, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	generic, err := decodeGeneric(raw)
	if err != nil {
		return err
	}
	if err := validateGeneric(generic); err != nil {
		return err
	}
	return validateAgents(f.Agents)
}

func decodeGeneric(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, decodeError(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, invalid("unexpected data after the top-level value")
	}
	return v, nil
}

func validateGeneric(v any) error {
	s, err := compiledSchema()
	if err != nil {
		return fmt.Errorf("config schema: %w", err)
	}
	err = s.Validate(v)
	if err == nil {
		return nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return fmt.Errorf("%w: schema validation failed", ErrInvalidConfig)
	}
	out := &ValidationError{}
	collectViolations(ve, out)
	if len(out.Violations) == 0 {
		out.Violations = []Violation{{Path: ve.InstanceLocation, Keyword: lastSegment(ve.KeywordLocation)}}
	}
	return out
}

func collectViolations(ve *jsonschema.ValidationError, out *ValidationError) {
	if len(out.Violations) >= maxReportedViolations {
		return
	}
	if len(ve.Causes) == 0 {
		kw := lastSegment(ve.KeywordLocation)
		v := Violation{Path: ve.InstanceLocation, Keyword: kw}
		switch kw {
		case "additionalProperties", "required", "dependentRequired":
			v.Detail = ve.Message
		}
		out.Violations = append(out.Violations, v)
		return
	}
	for _, c := range ve.Causes {
		collectViolations(c, out)
	}
}

// lastSegment names the failed keyword: the last segment of its schema location, except that a
// dependentRequired location continues with the property and an index.
func lastSegment(pointer string) string {
	if strings.Contains(pointer, "/dependentRequired/") {
		return "dependentRequired"
	}
	segs := strings.Split(pointer, "/")
	for i := len(segs) - 1; i >= 0; i-- {
		if _, err := strconv.Atoi(segs[i]); err != nil {
			return segs[i]
		}
	}
	return ""
}

// decodeError reports where a document is malformed without echoing any of its bytes: a syntax
// error message would otherwise quote the offending character of a file that may hold a secret.
func decodeError(err error) error {
	var syn *json.SyntaxError
	if errors.As(err, &syn) {
		return invalid("JSON syntax error at byte %d", syn.Offset)
	}
	var typ *json.UnmarshalTypeError
	if errors.As(err, &typ) {
		return invalid("%s: expected %s", typ.Field, typ.Type)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return invalid("document is empty or truncated")
	}
	return invalid("document cannot be decoded")
}
