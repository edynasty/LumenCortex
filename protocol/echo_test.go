package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const sentinel = "SENTINEL-REASONING-ECHO"

// §4.6 regression 1 (TestEchoNeverSerialized, S5): echo bytes have no serialized, printed or logged
// form, and request and response JSON never carry them.
func TestEchoNeverSerialized(t *testing.T) {
	e := NewEcho("reasoning_content", []byte(sentinel), 7)
	if string(e.Data()) != sentinel || e.Tokens() != 7 || e.Kind() != "reasoning_content" || e.IsZero() {
		t.Fatalf("echo accessors: %q %d %q %v", e.Data(), e.Tokens(), e.Kind(), e.IsZero())
	}
	if _, err := json.Marshal(struct{ E Echo }{e}); !errors.Is(err, ErrEchoNotSerializable) {
		t.Fatalf("json.Marshal of a struct holding Echo: err=%v, want ErrEchoNotSerializable", err)
	}
	if _, err := json.Marshal(map[string]any{"e": e}); !errors.Is(err, ErrEchoNotSerializable) {
		t.Fatalf("json.Marshal of a map holding Echo: err=%v", err)
	}
	req := ProviderRequest{Messages: []Message{{Role: "assistant", Content: "a"}}, Echo: []Echo{e}}
	resp := ProviderResponse{Message: Message{Role: "assistant", Content: "a"}, Echo: e}
	for _, v := range []any{req, resp, &req, &resp} {
		raw, err := json.Marshal(v)
		if err != nil || bytes.Contains(raw, []byte(sentinel)) {
			t.Fatalf("json of %T: %s %v", v, raw, err)
		}
	}
	type holder struct {
		model string
		echo  Echo
	}
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("x", "echo", e, "req", req)
	slog.New(slog.NewTextHandler(&logged, nil)).Info("x", "echo", e)
	for _, s := range []string{
		fmt.Sprintf("%v %+v %#v %s %q", e, e, e, e, e),
		fmt.Sprintf("%v %+v %#v", holder{"m", e}, holder{"m", e}, holder{"m", e}),
		fmt.Sprintf("%v %+v %#v", []Echo{e}, map[int64]Echo{1: e}, req),
		fmt.Sprintf("%v %+v %#v", resp, resp, &resp),
		logged.String(),
	} {
		if strings.Contains(s, sentinel) {
			t.Fatalf("echo bytes printed: %s", s)
		}
	}
	// The bytes are a copy: changing the adapter's buffer does not change the echo.
	buf := []byte("abc")
	copied := NewEcho("k", buf, 1)
	buf[0] = 'X'
	if string(copied.Data()) != "abc" {
		t.Fatal("NewEcho kept the caller's buffer")
	}
	if !NewEcho("k", nil, 3).IsZero() {
		t.Fatal("an echo of no bytes must be zero")
	}
}
