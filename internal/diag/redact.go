package diag

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Redaction limits (ADR-0019 §5). Every record is bounded before it is queued, so the queue's
// memory is a count bound times a record bound, never the size of what a caller tried to log.
const (
	// Redacted is the value a secret-named attribute is replaced with.
	Redacted = "[redacted]"

	// maxStringBytes caps one ordinary string attribute.
	maxStringBytes = 2048
	// maxDebugValueBytes caps a command or argument text kept at debug level.
	maxDebugValueBytes = 512
	// maxRecordBytes caps one encoded JSON line; a larger record is replaced by a stub that keeps
	// the timestamp, level and message and says how much was dropped.
	maxRecordBytes = 16 << 10
	// maxSanitizeDepth bounds recursion into maps and slices.
	maxSanitizeDepth = 8
)

// truncationMarker ends every value the redactor cut.
const truncationMarker = "...[truncated]"

// secretKeys are the attribute names whose value is never logged, compared after normalizeKey.
var secretKeys = map[string]bool{
	"authorization":      true,
	"proxyauthorization": true,
	"cookie":             true,
	"setcookie":          true,
	"apikey":             true,
	"xapikey":            true,
	"token":              true,
	"accesstoken":        true,
	"refreshtoken":       true,
	"idtoken":            true,
	"secret":             true,
	"clientsecret":       true,
	"password":           true,
	"passwd":             true,
	"credential":         true,
	"credentials":        true,
	"privatekey":         true,
}

// secretSuffixes are matched against the end of a normalized key, so openai_api_key, bearer_token
// and db_password are secrets too. A plain count such as total_tokens does not end in one.
var secretSuffixes = []string{
	"apikey", "token", "secret", "password", "passwd", "credential", "credentials", "privatekey", "cookie",
}

// contentKeys are the attribute names whose value is reduced to its byte length. Prompts, model
// messages, reasoning, tool output and tool arguments never reach the log.
var contentKeys = map[string]bool{
	"content":          true,
	"reasoning":        true,
	"reasoningcontent": true,
	"thinking":         true,
	"prompt":           true,
	"input":            true,
	"final":            true,
	"messagetext":      true,
	"chunk":            true,
	"stdout":           true,
	"stderr":           true,
	"output":           true,
	"args":             true,
	"arguments":        true,
	"command":          true,
	"text":             true,
	"body":             true,
}

// debugKeepable are the content keys debug level may keep, masked and truncated: a command or the
// arguments of a tool call are what a debugging session needs. Reasoning, prompts and output are
// never kept at any level.
var debugKeepable = map[string]bool{
	"command":   true,
	"args":      true,
	"arguments": true,
}

// normalizedKey is normalizeKey, except that a key already in normal form is returned as it is: the
// common attribute names cost no allocation.
func normalizedKey(key string) string {
	for i := 0; i < len(key); i++ {
		c := key[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			continue
		}
		return normalizeKey(key)
	}
	return key
}

// normalizeKey lowercases a key and drops '_', '-' and spaces, so X-API-Key, x_api_key and
// xApiKey compare equal.
func normalizeKey(key string) string {
	var b strings.Builder
	b.Grow(len(key))
	for _, r := range key {
		switch r {
		case '_', '-', ' ':
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// IsSecretKey reports whether an attribute named key holds a secret and is replaced wholesale.
func IsSecretKey(key string) bool { return isSecretName(normalizedKey(key)) }

// IsContentKey reports whether an attribute named key holds content and is reduced to its length.
func IsContentKey(key string) bool { return contentKeys[normalizedKey(key)] }

// isSecretName is IsSecretKey on an already normalized name.
func isSecretName(norm string) bool {
	if secretKeys[norm] {
		return true
	}
	for _, suffix := range secretSuffixes {
		if len(norm) > len(suffix) && strings.HasSuffix(norm, suffix) {
			return true
		}
	}
	return false
}

// secretPatterns mask secrets that appear inside an otherwise ordinary string: an Authorization
// header value, a provider key, a forge token, an AWS key id, a Slack token, a PEM private key and
// credentials embedded in a URL. They run in this order, so a PEM block is gone before the key
// patterns look at its body.
var secretPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`), "[redacted private key]"},
	{regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{8,}`), "Bearer " + Redacted},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`), "sk-" + Redacted},
	{regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{20,})`), Redacted},
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), Redacted},
	{regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`), Redacted},
	{regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`), "$1" + Redacted + "@"},
}

// MaskSecrets returns s with every secret-shaped value replaced. It does not truncate.
func MaskSecrets(s string) string {
	for _, p := range secretPatterns {
		if p.re.MatchString(s) {
			s = p.re.ReplaceAllString(s, p.repl)
		}
	}
	return s
}

// truncateUTF8 cuts s to at most max bytes at a rune boundary and appends the truncation marker,
// so the result never exceeds max bytes.
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max - len(truncationMarker)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncationMarker
}

// redactString masks and truncates an ordinary string attribute.
func redactString(s string) string { return truncateUTF8(MaskSecrets(s), maxStringBytes) }

// contentValue is the stand-in for a content attribute: how much there was, never the bytes.
func contentValue(n int) map[string]any {
	return map[string]any{"redacted": "content", "bytes": n}
}

// valueText renders a slog value as text, for the byte count of a content attribute and for the
// text debug level may keep.
func valueText(v slog.Value) string {
	v = v.Resolve()
	if v.Kind() == slog.KindString {
		return v.String()
	}
	if b, err := json.Marshal(v.Any()); err == nil {
		return string(b)
	}
	return fmt.Sprint(v.Any())
}

// sanitizeValue returns the value to log for the attribute whose normalized name is norm.
func sanitizeValue(level slog.Level, norm string, v slog.Value, depth int) any {
	v = v.Resolve()
	if isSecretName(norm) {
		return Redacted
	}
	if contentKeys[norm] {
		// Debug keeps a command or the arguments of a call, masked and truncated; every other
		// content value is reduced to its length at every level.
		if level < slog.LevelInfo && debugKeepable[norm] {
			return truncateUTF8(MaskSecrets(valueText(v)), maxDebugValueBytes)
		}
		return contentValue(len(valueText(v)))
	}
	switch v.Kind() {
	case slog.KindString:
		return redactString(v.String())
	case slog.KindGroup:
		n := &node{}
		for _, a := range v.Group() {
			addAttr(n, nil, a, level, depth+1)
		}
		return n
	case slog.KindAny:
		return sanitizeAny(level, v.Any(), depth+1)
	default:
		return v.Any()
	}
}

// sanitizeAny redacts a value that is not a plain slog kind: a map, a slice, a byte string or an
// error a caller attached with slog.Any.
func sanitizeAny(level slog.Level, v any, depth int) any {
	if depth > maxSanitizeDepth {
		return "[depth limit]"
	}
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		return redactString(t)
	case []byte:
		return contentValue(len(t))
	case error:
		return redactString(t.Error())
	case map[string]any:
		return sanitizeMap(level, t, depth)
	case []any:
		out := make([]any, 0, len(t))
		for _, item := range t {
			out = append(out, sanitizeAny(level, item, depth+1))
		}
		return out
	case json.RawMessage:
		var parsed any
		if err := json.Unmarshal(t, &parsed); err != nil {
			return redactString(string(t))
		}
		return sanitizeAny(level, parsed, depth+1)
	case fmt.Stringer:
		return redactString(t.String())
	}
	b, err := json.Marshal(v)
	if err != nil {
		return redactString(fmt.Sprint(v))
	}
	var parsed any
	if err := json.Unmarshal(b, &parsed); err != nil {
		return redactString(string(b))
	}
	return sanitizeAny(level, parsed, depth+1)
}

// sanitizeMap redacts a map value key by key, in sorted key order so a record is reproducible.
func sanitizeMap(level slog.Level, m map[string]any, depth int) *node {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	n := &node{}
	for _, k := range keys {
		n.set(k, sanitizeValue(level, normalizedKey(k), slog.AnyValue(m[k]), depth+1))
	}
	return n
}
