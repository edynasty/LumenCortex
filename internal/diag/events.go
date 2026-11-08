package diag

import (
	"bytes"
	"encoding/json"
	"strings"
)

// EventPayload removes provider-private fields and secrets before an event leaves Core.
// The input is already a bounded tool/provider result; the returned JSON is at most 16 KiB.
func EventPayload(raw []byte) json.RawMessage {
	if len(raw) > 1<<20 {
		return json.RawMessage(`{"withheld":"oversized event"}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return json.RawMessage(`{"withheld":"invalid event"}`)
	}
	clean := eventValue(value, 0)
	out, err := json.Marshal(clean)
	if err != nil || len(out) > maxRecordBytes {
		return json.RawMessage(`{"withheld":"oversized event"}`)
	}
	return out
}

func privateEventKey(key string) bool {
	key = normalizedKey(key)
	return strings.Contains(key, "reasoning") || strings.Contains(key, "thinking") ||
		strings.Contains(key, "scratchpad") || strings.Contains(key, "chainofthought") ||
		key == "echo" || key == "environment" || key == "env"
}

func eventValue(value any, depth int) any {
	if depth >= maxSanitizeDepth {
		return Redacted
	}
	switch v := value.(type) {
	case map[string]any:
		if kind, ok := v["type"].(string); ok && privateEventKey(kind) {
			return nil
		}
		out := make(map[string]any, min(len(v), 128))
		if len(v) > 128 {
			return Redacted
		}
		for key, item := range v {
			if privateEventKey(key) {
				continue
			}
			if IsSecretKey(key) {
				out[key] = Redacted
				continue
			}
			out[key] = eventValue(item, depth+1)
		}
		return out
	case []any:
		out := make([]any, 0, min(len(v), 128))
		for _, item := range v[:min(len(v), 128)] {
			if clean := eventValue(item, depth+1); clean != nil {
				out = append(out, clean)
			}
		}
		return out
	case string:
		// JSON arguments are decoded first so password/token fields cannot hide in text.
		if len(v) <= 64<<10 && (strings.HasPrefix(v, "{") || strings.HasPrefix(v, "[")) {
			var nested any
			if json.Unmarshal([]byte(v), &nested) == nil {
				clean, err := json.Marshal(eventValue(nested, depth+1))
				if err != nil {
					return Redacted
				}
				return truncateUTF8(string(clean), 8192)
			}
		}
		return truncateUTF8(MaskSecrets(v), 8192)
	default:
		return value
	}
}
