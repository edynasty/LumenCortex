package workflow

import (
	"encoding/json"
	"sort"
)

func seedFactSources(target map[string]any, facts map[string]any, prefix string) {
	for key, value := range facts {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if nested, ok := value.(map[string]any); ok {
			seedFactSources(target, nested, path)
		} else {
			target[path] = map[string]any{"at": nowISO(), "source": "initial"}
		}
	}
}

func compactResult(result, parsed map[string]any) map[string]any {
	return map[string]any{"ok": truthy(result["ok"]), "denied": truthy(result["denied"]), "permission": result["permission"], "data": compact(parsed)}
}

func compact(value any) any {
	raw, _ := json.Marshal(value)
	if len(raw) <= 1200 {
		return deepAny(value)
	}
	return map[string]any{"truncated": true, "preview": string(raw[:1200])}
}

func same(left, right any) bool {
	leftJSON, _ := json.Marshal(sortValue(left))
	rightJSON, _ := json.Marshal(sortValue(right))
	return string(leftJSON) == string(rightJSON)
}

func sortValue(value any) any {
	if m, ok := toMap(value); ok {
		keys := sortedKeys(m)
		out := make(map[string]any, len(m))
		for _, key := range keys {
			out[key] = sortValue(m[key])
		}
		return out
	}
	if arr, ok := toSlice(value); ok {
		out := make([]any, len(arr))
		for i, item := range arr {
			out[i] = sortValue(item)
		}
		return out
	}
	return value
}

func clone[T any](value T) (T, error) {
	var out T
	raw, err := json.Marshal(value)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(raw, &out)
	return out, err
}

func deepAny(value any) any {
	raw, _ := json.Marshal(value)
	var out any
	_ = json.Unmarshal(raw, &out)
	return out
}

func deepMap(value map[string]any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	out, ok := deepAny(value).(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return out
}

func cloneSlice(value []map[string]any) []map[string]any {
	out := make([]map[string]any, len(value))
	for i := range value {
		out[i] = deepMap(value[i])
	}
	return out
}

func cloneStringPtr(value *[]string) *[]string {
	if value == nil {
		return nil
	}
	out := append([]string(nil), (*value)...)
	return &out
}

func gateSummary(gate Gate) GateSummary {
	return GateSummary{ID: gate.ID, Type: gate.Type, Title: gate.Title, Description: gate.Description}
}

func sortedKeys[M ~map[string]V, V any](m M) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func nowISO() string { return nowUTC().Format("2006-01-02T15:04:05.999999999Z07:00") }
