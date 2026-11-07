package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/edynasty/LumenCortex/protocol"
	"github.com/santhosh-tekuri/jsonschema/v5"
)

// A hook that may replace a tool call's arguments (MutableArgs) must not be able to authorize input
// the tool's own schema refuses: the tool validated the model's arguments before the hook ran, so the
// replacement is validated again against the same schema. This is the tool's schema, not a second
// permission system.
var toolSchemaCache sync.Map // string -> *jsonschema.Schema

// validateToolArgs validates args against spec.Parameters. A spec without a schema validates
// nothing; a schema that cannot be compiled refuses the call (fail closed).
func validateToolArgs(spec protocol.ToolSpec, args map[string]any) error {
	if len(spec.Parameters) == 0 {
		return nil
	}
	raw, err := json.Marshal(spec.Parameters)
	if err != nil {
		return fmt.Errorf("tool schema is not serializable: %w", err)
	}
	sum := sha256.Sum256(raw)
	key := spec.Name + "@" + hex.EncodeToString(sum[:8])
	schema, ok := toolSchemaCache.Load(key)
	if !ok {
		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource("mem://tool/"+key, bytes.NewReader(raw)); err != nil {
			return fmt.Errorf("tool schema is invalid: %w", err)
		}
		compiled, err := compiler.Compile("mem://tool/" + key)
		if err != nil {
			return fmt.Errorf("tool schema is invalid: %w", err)
		}
		schema, _ = toolSchemaCache.LoadOrStore(key, compiled)
	}
	if err := schema.(*jsonschema.Schema).Validate(args); err != nil {
		message := err.Error()
		if len(message) > 512 {
			message = message[:512] + "…"
		}
		return fmt.Errorf("%s", message)
	}
	return nil
}

// toolSpecOf returns the spec named name.
func toolSpecOf(specs []protocol.ToolSpec, name string) (protocol.ToolSpec, bool) {
	for _, spec := range specs {
		if spec.Name == name {
			return spec, true
		}
	}
	return protocol.ToolSpec{}, false
}
