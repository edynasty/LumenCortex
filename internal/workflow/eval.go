package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

func Evaluate(condition any, ctx EvalContext) (bool, error) {
	if condition == nil {
		return true, nil
	}
	if b, ok := condition.(bool); ok {
		return b, nil
	}
	m, ok := toMap(condition)
	if !ok {
		return false, nil
	}
	if raw, ok := m["all"]; ok {
		arr, _ := toSlice(raw)
		for _, item := range arr {
			value, err := Evaluate(item, ctx)
			if err != nil || !value {
				return value, err
			}
		}
		return true, nil
	}
	if raw, ok := m["any"]; ok {
		arr, _ := toSlice(raw)
		for _, item := range arr {
			value, err := Evaluate(item, ctx)
			if err != nil {
				return false, err
			}
			if value {
				return true, nil
			}
		}
		return false, nil
	}
	if raw, ok := m["not"]; ok {
		value, err := Evaluate(raw, ctx)
		return !value, err
	}

	checks := []bool{}
	if raw, ok := m["tool"]; ok {
		tools := []string{fmt.Sprint(raw)}
		if arr, yes := toSlice(raw); yes {
			tools = tools[:0]
			for _, value := range arr {
				tools = append(tools, fmt.Sprint(value))
			}
		}
		found := false
		for _, tool := range tools {
			found = found || tool == ctx.Tool
		}
		checks = append(checks, found)
	}
	if raw, ok := m["ok"]; ok {
		checks = append(checks, truthy(ctx.Result["ok"]) == truthy(raw))
	}
	if raw, ok := m["fact"]; ok {
		value, _ := getPath(ctx.Facts, fmt.Sprint(raw))
		checks = append(checks, compareValue(value, m))
	}
	if raw, ok := m["arg"]; ok {
		value, _ := getPath(ctx.Args, fmt.Sprint(raw))
		checks = append(checks, compareValue(value, m))
	}
	if raw, ok := m["result"]; ok {
		path := fmt.Sprint(raw)
		value, exists := getPath(ctx.ResultData, path)
		if !exists {
			value, _ = getPath(ctx.Result, path)
		}
		checks = append(checks, compareValue(value, m))
	}
	if len(checks) == 0 {
		return false, nil
	}
	for _, check := range checks {
		if !check {
			return false, nil
		}
	}
	return true, nil
}

func validateCondition(condition any, label string) error {
	if condition == nil {
		return nil
	}
	if _, ok := condition.(bool); ok {
		return nil
	}
	m, ok := toMap(condition)
	if !ok {
		return fmt.Errorf("invalid workflow condition at %s", label)
	}
	for _, key := range []string{"all", "any"} {
		if raw, exists := m[key]; exists {
			arr, yes := toSlice(raw)
			if !yes {
				return fmt.Errorf("%s.%s must be an array", label, key)
			}
			for i, item := range arr {
				if err := validateCondition(item, fmt.Sprintf("%s.%s[%d]", label, key, i)); err != nil {
					return err
				}
			}
			return nil
		}
	}
	if raw, ok := m["not"]; ok {
		return validateCondition(raw, label+".not")
	}
	selectors := map[string]bool{}
	for _, key := range []string{"fact", "tool", "ok", "arg", "result"} {
		if _, ok := m[key]; ok {
			selectors[key] = true
		}
	}
	if len(selectors) == 0 {
		return fmt.Errorf("condition at %s has no selector", label)
	}
	for _, key := range []string{"fact", "arg", "result"} {
		if raw, ok := m[key]; ok {
			if _, err := validatePath(fmt.Sprint(raw)); err != nil {
				return fmt.Errorf("%s.%s: %w", label, key, err)
			}
		}
	}
	allowed := map[string]bool{"equals": true, "notEquals": true, "exists": true, "contains": true, "matches": true, "in": true, "gt": true, "gte": true, "lt": true, "lte": true}
	for key := range m {
		if !selectors[key] && !allowed[key] {
			return fmt.Errorf("unknown workflow condition key %s at %s", key, label)
		}
	}
	return nil
}

func compareValue(actual any, c map[string]any) bool {
	if value, ok := c["exists"]; ok {
		return (actual != nil) == truthy(value)
	}
	if value, ok := c["equals"]; ok {
		return same(actual, value)
	}
	if value, ok := c["notEquals"]; ok {
		return !same(actual, value)
	}
	if value, ok := c["contains"]; ok {
		switch typed := actual.(type) {
		case string:
			return strings.Contains(typed, fmt.Sprint(value))
		case []any:
			for _, item := range typed {
				if same(item, value) {
					return true
				}
			}
		}
		return false
	}
	if value, ok := c["matches"]; ok {
		re, err := regexp.Compile(fmt.Sprint(value))
		return err == nil && re.MatchString(fmt.Sprint(actual))
	}
	if value, ok := c["in"]; ok {
		arr, _ := toSlice(value)
		for _, item := range arr {
			if same(actual, item) {
				return true
			}
		}
		return false
	}
	actualFloat, actualOK := toFloat(actual)
	for _, op := range []string{"gt", "gte", "lt", "lte"} {
		if value, ok := c[op]; ok {
			expected, expectedOK := toFloat(value)
			if !actualOK || !expectedOK {
				return false
			}
			switch op {
			case "gt":
				return actualFloat > expected
			case "gte":
				return actualFloat >= expected
			case "lt":
				return actualFloat < expected
			case "lte":
				return actualFloat <= expected
			}
		}
	}
	return truthy(actual)
}

func validatePath(path string) ([]string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("invalid path")
	}
	parts := strings.Split(path, ".")
	forbidden := map[string]bool{"__proto__": true, "prototype": true, "constructor": true}
	for _, part := range parts {
		if part == "" || forbidden[part] {
			return nil, fmt.Errorf("unsafe path: %s", path)
		}
	}
	return parts, nil
}

func getPath(target any, path string) (any, bool) {
	parts, err := validatePath(path)
	if err != nil {
		return nil, false
	}
	current := target
	for _, part := range parts {
		m, ok := toMap(current)
		if !ok {
			return nil, false
		}
		value, ok := m[part]
		if !ok {
			return nil, false
		}
		current = value
	}
	return current, true
}

func setPath(target map[string]any, path string, value any) error {
	parts, err := validatePath(path)
	if err != nil {
		return err
	}
	current := target
	for _, part := range parts[:len(parts)-1] {
		value, ok := current[part]
		if !ok {
			next := map[string]any{}
			current[part] = next
			current = next
			continue
		}
		next, ok := value.(map[string]any)
		if !ok {
			next = map[string]any{}
			current[part] = next
		}
		current = next
	}
	current[parts[len(parts)-1]] = value
	return nil
}

func parseResultContent(value any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	if m, ok := toMap(value); ok {
		return m
	}
	if text, ok := value.(string); ok {
		var parsed any
		if json.Unmarshal([]byte(text), &parsed) == nil {
			if m, ok := toMap(parsed); ok {
				return m
			}
			return map[string]any{"content": parsed}
		}
		return map[string]any{"content": text}
	}
	return map[string]any{"content": value}
}

func truthy(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case nil:
		return false
	case string:
		return typed != "" && typed != "0" && typed != "false"
	case float64:
		return typed != 0
	case int:
		return typed != 0
	case int64:
		return typed != 0
	default:
		return true
	}
}

func toFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		value, err := typed.Float64()
		return value, err == nil
	case string:
		value, err := strconv.ParseFloat(typed, 64)
		return value, err == nil
	default:
		return 0, false
	}
}

func toMap(value any) (map[string]any, bool) {
	if m, ok := value.(map[string]any); ok {
		return m, true
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		return nil, false
	}
	return out, true
}

func toSlice(value any) ([]any, bool) {
	if arr, ok := value.([]any); ok {
		return arr, true
	}
	rv := reflect.ValueOf(value)
	if !rv.IsValid() || rv.Kind() != reflect.Slice {
		return nil, false
	}
	out := make([]any, rv.Len())
	for i := range out {
		out[i] = rv.Index(i).Interface()
	}
	return out, true
}
