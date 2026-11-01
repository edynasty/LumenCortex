package diag

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// plantedSecrets are values no log file may ever contain. Every redaction test asserts the whole
// file text against them, not only the field it looked at.
var plantedSecrets = []string{
	"sk-live-abcdefghijklmnopqrstuvwxyz0123456789",
	"ghp_0123456789abcdefghijklmnopqrstuvwxyz",
	"github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyz",
	"AKIAIOSFODNN7EXAMPLE",
	"xoxb-123456789012-abcdefghijkl",
	"super-secret-bearer-token-value",
	"hunter2-password",
	"client-secret-value-xyz",
	"refresh-token-value-abc",
	"session-cookie-value",
	"pem-private-key-body-AAAA",
}

// logOne writes one record at level and returns the file's only record.
func logOne(t *testing.T, level slog.Level, build func(*slog.Logger)) map[string]any {
	t.Helper()
	l := newDebugLog(t, Options{})
	logger := l.Logger()
	build(logger)
	closeLog(t, l)
	record := onlyRecord(t, l.Dir())
	if got := record["level"]; got != level.String() {
		t.Fatalf("level = %v, want %s", got, level)
	}
	assertNoText(t, l.Dir(), plantedSecrets...)
	return record
}

func TestSecretKeysAreRedactedAtEveryLevel(t *testing.T) {
	keys := []string{
		"authorization", "Authorization", "AUTHORIZATION", "proxy-authorization", "Proxy_Authorization",
		"cookie", "Cookie", "set-cookie", "Set_Cookie", "api_key", "apiKey", "API-KEY", "x-api-key",
		"X-API-Key", "apikey", "token", "access_token", "accessToken", "refresh_token", "id_token",
		"secret", "client_secret", "password", "passwd", "credential", "credentials", "private_key",
		"openai_api_key", "bearer_token", "db_password",
	}
	for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
		for _, key := range keys {
			t.Run(fmt.Sprintf("%s/%s", level, key), func(t *testing.T) {
				record := logOne(t, level, func(logger *slog.Logger) {
					logger.Log(context.Background(), level, "probe", key, "hunter2-password")
				})
				if got := record[key]; got != Redacted {
					t.Fatalf("%s = %v, want %s", key, got, Redacted)
				}
			})
		}
	}
}

func TestSecretKeysAreRedactedInsideGroupsAndWithAttrs(t *testing.T) {
	record := logOne(t, slog.LevelDebug, func(logger *slog.Logger) {
		scoped := logger.With(
			"apiKey", "sk-live-abcdefghijklmnopqrstuvwxyz0123456789",
			"session_id", "s-1",
		)
		grouped := scoped.WithGroup("http").With("authorization", "Bearer super-secret-bearer-token-value")
		grouped.Debug("request",
			slog.Group("headers", "cookie", "session-cookie-value", "accept", "application/json"),
			slog.Group("nested", slog.Group("deeper", "password", "hunter2-password")),
			"note", "plain",
		)
	})
	if record["apiKey"] != Redacted {
		t.Fatalf("With attr apiKey = %v", record["apiKey"])
	}
	http, ok := record["http"].(map[string]any)
	if !ok {
		t.Fatalf("group http missing: %v", record)
	}
	if http["authorization"] != Redacted {
		t.Fatalf("grouped With attr = %v", http["authorization"])
	}
	headers, ok := http["headers"].(map[string]any)
	if !ok {
		t.Fatalf("headers group missing: %v", http)
	}
	if headers["cookie"] != Redacted || headers["accept"] != "application/json" {
		t.Fatalf("headers = %v", headers)
	}
	nested, ok := http["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested group missing: %v", http)
	}
	deeper, ok := nested["deeper"].(map[string]any)
	if !ok || deeper["password"] != Redacted {
		t.Fatalf("nested group = %v", nested)
	}
}

func TestSecretShapedValuesAreMaskedInsideStrings(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"bearer", "Authorization: Bearer super-secret-bearer-token-value"},
		{"openai", "key sk-live-abcdefghijklmnopqrstuvwxyz0123456789 used"},
		{"github pat", "ghp_0123456789abcdefghijklmnopqrstuvwxyz"},
		{"github fine-grained", "github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyz"},
		{"aws", "AKIAIOSFODNN7EXAMPLE"},
		{"slack", "xoxb-123456789012-abcdefghijkl"},
		{"pem", "-----BEGIN RSA PRIVATE KEY-----\npem-private-key-body-AAAA\n-----END RSA PRIVATE KEY-----"},
		{"url userinfo", "https://alice:hunter2-password@example.com/v1/chat"},
	}
	for _, tc := range cases {
		for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelError} {
			t.Run(fmt.Sprintf("%s/%s", tc.name, level), func(t *testing.T) {
				record := logOne(t, level, func(logger *slog.Logger) {
					logger.Log(context.Background(), level, "probe", "detail", tc.value)
				})
				got, _ := record["detail"].(string)
				if got == "" {
					t.Fatalf("detail missing: %v", record)
				}
				if strings.Contains(got, "hunter2") || strings.Contains(got, "sk-live") ||
					strings.Contains(got, "pem-private-key-body") || strings.Contains(got, "xoxb-") {
					t.Fatalf("secret survived masking: %q", got)
				}
			})
		}
	}
	// The masked forms keep enough context to be useful.
	if got := MaskSecrets("Authorization: Bearer super-secret-bearer-token-value"); got != "Authorization: Bearer "+Redacted {
		t.Fatalf("bearer mask = %q", got)
	}
	if got := MaskSecrets("https://alice:hunter2-password@example.com/x"); got != "https://"+Redacted+"@example.com/x" {
		t.Fatalf("url mask = %q", got)
	}
	if got := MaskSecrets("nothing to mask here"); got != "nothing to mask here" {
		t.Fatalf("clean string changed: %q", got)
	}
}

func TestContentKeysAreReducedToTheirLength(t *testing.T) {
	content := map[string]string{
		"content":           "the model's answer",
		"reasoning":         "hidden chain of thought",
		"reasoning_content": "hidden chain of thought",
		"thinking":          "hidden chain of thought",
		"prompt":            "the whole prompt",
		"input":             "the tool input",
		"final":             "the final answer",
		"message_text":      "a message",
		"chunk":             "tool output chunk",
		"stdout":            "command stdout",
		"stderr":            "command stderr",
		"output":            "tool output",
		"args":              `{"path":"secret.txt"}`,
		"arguments":         `{"path":"secret.txt"}`,
		"command":           "cat secret.txt",
		"text":              "some text",
		"body":              "an http body",
	}
	for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelError} {
		for key, value := range content {
			t.Run(fmt.Sprintf("%s/%s", level, key), func(t *testing.T) {
				l := newDebugLog(t, Options{})
				l.Logger().Log(context.Background(), level, "probe", key, value)
				closeLog(t, l)
				record := onlyRecord(t, l.Dir())
				if level == slog.LevelDebug && debugKeepable[normalizeKey(key)] {
					got, _ := record[key].(string)
					if got != value {
						t.Fatalf("debug %s = %v, want the truncated text", key, record[key])
					}
					return
				}
				reduced, ok := record[key].(map[string]any)
				if !ok {
					t.Fatalf("%s at %s = %v, want a content stub", key, level, record[key])
				}
				if reduced["redacted"] != "content" || reduced["bytes"] != float64(len(value)) {
					t.Fatalf("%s stub = %v, want bytes %d", key, reduced, len(value))
				}
				text := rawLogText(t, l.Dir())
				if strings.Contains(text, value) {
					t.Fatalf("%s leaked its content at %s: %s", key, level, text)
				}
			})
		}
	}
}

func TestDebugKeepsCommandAndArgsTruncated(t *testing.T) {
	long := strings.Repeat("a", 4000)
	l := newDebugLog(t, Options{})
	l.Logger().Debug("tool.start", "command", long, "args", `{"q":"`+long+`"}`, "output", long)
	closeLog(t, l)
	record := onlyRecord(t, l.Dir())

	command, _ := record["command"].(string)
	if len(command) > maxDebugValueBytes || !strings.HasSuffix(command, truncationMarker) {
		t.Fatalf("command kept %d bytes, want at most %d with the marker", len(command), maxDebugValueBytes)
	}
	args, _ := record["args"].(string)
	if len(args) > maxDebugValueBytes || !strings.HasSuffix(args, truncationMarker) {
		t.Fatalf("args kept %d bytes, want at most %d with the marker", len(args), maxDebugValueBytes)
	}
	if _, ok := record["output"].(map[string]any); !ok {
		t.Fatalf("output must stay reduced at debug: %v", record["output"])
	}
	if strings.Contains(rawLogText(t, l.Dir()), long) {
		t.Fatal("debug kept the untruncated text")
	}
}

func TestDebugKeepsCommandMasked(t *testing.T) {
	l := newDebugLog(t, Options{})
	l.Logger().Debug("tool.start", "command", "curl -H 'Authorization: Bearer super-secret-bearer-token-value' https://x")
	closeLog(t, l)
	assertNoText(t, l.Dir(), plantedSecrets...)
	record := onlyRecord(t, l.Dir())
	command, _ := record["command"].(string)
	if !strings.Contains(command, "curl") || !strings.Contains(command, Redacted) {
		t.Fatalf("debug command = %q", command)
	}
}

func TestLongStringsAreCapped(t *testing.T) {
	long := strings.Repeat("y", 9000)
	l := newDebugLog(t, Options{})
	l.Logger().Info("probe", "detail", long, "nested", map[string]any{"detail": long})
	closeLog(t, l)
	record := onlyRecord(t, l.Dir())
	detail, _ := record["detail"].(string)
	if len(detail) > maxStringBytes || !strings.HasSuffix(detail, truncationMarker) {
		t.Fatalf("string kept %d bytes, want at most %d", len(detail), maxStringBytes)
	}
	nested, _ := record["nested"].(map[string]any)
	inner, _ := nested["detail"].(string)
	if len(inner) > maxStringBytes || !strings.HasSuffix(inner, truncationMarker) {
		t.Fatalf("nested string kept %d bytes", len(inner))
	}
}

func TestSanitizeMapRedactsByKey(t *testing.T) {
	l := newDebugLog(t, Options{})
	l.Logger().Info("probe", "data", map[string]any{
		"authorization": "Bearer super-secret-bearer-token-value",
		"note":          "sk-live-abcdefghijklmnopqrstuvwxyz0123456789",
		"nested":        map[string]any{"password": "hunter2-password"},
		"list":          []any{"AKIAIOSFODNN7EXAMPLE", "plain"},
		"count":         3,
	})
	closeLog(t, l)
	record := onlyRecord(t, l.Dir())
	assertNoText(t, l.Dir(), plantedSecrets...)
	data, ok := record["data"].(map[string]any)
	if !ok {
		t.Fatalf("data = %v", record["data"])
	}
	if data["authorization"] != Redacted {
		t.Fatalf("map authorization = %v", data["authorization"])
	}
	if data["count"] != float64(3) {
		t.Fatalf("count = %v", data["count"])
	}
	list, _ := data["list"].([]any)
	if len(list) != 2 || list[1] != "plain" {
		t.Fatalf("list = %v", list)
	}
}

func TestByteStringsBecomeContentStubs(t *testing.T) {
	l := newDebugLog(t, Options{})
	l.Logger().Info("probe", "blob", []byte("binary-secret-bytes"))
	closeLog(t, l)
	record := onlyRecord(t, l.Dir())
	stub, ok := record["blob"].(map[string]any)
	if !ok || stub["bytes"] != float64(len("binary-secret-bytes")) {
		t.Fatalf("blob = %v", record["blob"])
	}
	if strings.Contains(rawLogText(t, l.Dir()), "binary-secret-bytes") {
		t.Fatal("byte string leaked")
	}
}

func TestExportContainsNoSecrets(t *testing.T) {
	l := newDebugLog(t, Options{})
	logger := l.Logger()
	logger.Info("session.start", "session_id", "s-1", "prompt", "the whole prompt with sk-live-abcdefghijklmnopqrstuvwxyz0123456789")
	logger.Error("session.failed", "authorization", "Bearer super-secret-bearer-token-value", "error", "boom")
	logger.Debug("tool.start", "command", "curl -H 'Authorization: Bearer super-secret-bearer-token-value'")
	closeLog(t, l)

	path := filepath.Join(t.TempDir(), "export.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Export(context.Background(), file, map[string]any{"coreVersion": "test"}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	entries := readZip(t, path)
	text := strings.Join(entries, "\n")
	for _, secret := range plantedSecrets {
		if strings.Contains(text, secret) {
			t.Fatalf("export contains %q:\n%s", secret, text)
		}
	}
	if strings.Contains(text, "the whole prompt") {
		t.Fatal("export contains a prompt")
	}
}

// readZip returns the names and contents of a zip's entries, name first.
func readZip(t *testing.T, path string) []string {
	t.Helper()
	out, err := zipContents(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return out
}

func TestInlineGroupAttributesRedacted(t *testing.T) {
	l := newDebugLog(t, Options{})
	l.Logger().Info("probe", slog.Group("", "authorization", "Bearer super-secret-bearer-token-value", "note", "plain"))
	closeLog(t, l)
	record := onlyRecord(t, l.Dir())
	if record["authorization"] != Redacted {
		t.Fatalf("inline group attribute = %v", record["authorization"])
	}
	if record["note"] != "plain" {
		t.Fatalf("inline group attribute note = %v", record["note"])
	}
	assertNoText(t, l.Dir(), plantedSecrets...)
}

func TestMessageIsMasked(t *testing.T) {
	l := newDebugLog(t, Options{})
	l.Logger().Info("failed with key sk-live-abcdefghijklmnopqrstuvwxyz0123456789")
	closeLog(t, l)
	assertNoText(t, l.Dir(), plantedSecrets...)
	if record := onlyRecord(t, l.Dir()); !strings.Contains(record["msg"].(string), Redacted) {
		t.Fatalf("msg = %v", record["msg"])
	}
}
