package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHelperLSPServer(t *testing.T) {
	if os.Getenv("LCX_LSP_HELPER") != "1" {
		return
	}
	reader := bufio.NewReader(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)
	write := func(value any) {
		raw, _ := json.Marshal(value)
		_, _ = fmt.Fprintf(writer, "Content-Length: %d\r\n\r\n", len(raw))
		_, _ = writer.Write(raw)
		_ = writer.Flush()
	}
	for {
		raw, err := readFrame(reader)
		if err != nil {
			os.Exit(0)
		}
		var msg envelope
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}
		if msg.Method == "exit" {
			os.Exit(0)
		}
		if msg.Method == "textDocument/didOpen" {
			var params struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
			}
			_ = json.Unmarshal(msg.Params, &params)
			write(map[string]any{
				"jsonrpc": "2.0",
				"method": "textDocument/publishDiagnostics",
				"params": map[string]any{
					"uri": params.TextDocument.URI,
					"diagnostics": []map[string]any{{
						"range": map[string]any{
							"start": map[string]any{"line": 0, "character": 0},
							"end": map[string]any{"line": 0, "character": 4},
						},
						"severity": 2,
						"source": "helper",
						"message": "helper diagnostic",
					}},
				},
			})
			continue
		}
		if len(msg.ID) == 0 {
			continue
		}
		var id any
		_ = json.Unmarshal(msg.ID, &id)
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"capabilities": map[string]any{"hoverProvider": true}}
		case "shutdown":
			result = nil
		case "textDocument/hover":
			result = map[string]any{"contents": map[string]any{"kind": "plaintext", "value": "hover-ok"}}
		case "textDocument/definition":
			result = []any{}
		case "textDocument/references":
			result = []any{}
		case "textDocument/documentSymbol":
			result = []any{map[string]any{"name": "main", "kind": 12}}
		case "workspace/symbol":
			result = []any{map[string]any{"name": "main", "kind": 12}}
		case "textDocument/rename":
			result = map[string]any{"changes": map[string]any{}}
		case "slow":
			time.Sleep(2 * time.Second)
			result = map[string]any{"late": true}
		default:
			result = nil
		}
		write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
}

func TestClientManagedProcessAndLanguageFeatures(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	if err := os.WriteFile(path, []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LCX_LSP_HELPER", "1")

	client, err := Start(context.Background(), Config{
		Name: "helper",
		Command: os.Args[0],
		Args: []string{"-test.run=TestHelperLSPServer"},
		LanguageID: "go",
		Workspace: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	hover, err := client.Hover(context.Background(), "main.go", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hover), "hover-ok") {
		t.Fatalf("hover=%s", hover)
	}

	symbols, err := client.DocumentSymbols(context.Background(), "main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(symbols), "main") {
		t.Fatalf("symbols=%s", symbols)
	}

	rename, err := client.Rename(context.Background(), "main.go", 1, 5, "renamed")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rename), "changes") {
		t.Fatalf("rename=%s", rename)
	}

	deadline := time.Now().Add(time.Second)
	var diagnostics []Diagnostic
	for len(diagnostics) == 0 && time.Now().Before(deadline) {
		diagnostics, err = client.Diagnostics(context.Background(), "main.go")
		if err != nil {
			t.Fatal(err)
		}
		if len(diagnostics) == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if len(diagnostics) != 1 || diagnostics[0].Message != "helper diagnostic" {
		t.Fatalf("diagnostics=%#v", diagnostics)
	}

	status := client.Status()
	if !status.Running || status.PID == 0 || status.PendingRequests != 0 {
		t.Fatalf("status=%#v", status)
	}
}

func TestClientRequestCancellationRemovesPendingRequest(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LCX_LSP_HELPER", "1")
	client, err := Start(context.Background(), Config{
		Command: os.Args[0],
		Args: []string{"-test.run=TestHelperLSPServer"},
		Workspace: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	var result json.RawMessage
	err = client.Request(ctx, "slow", map[string]any{}, &result)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if got := client.Status().PendingRequests; got != 0 {
		t.Fatalf("pending=%d", got)
	}
}

func TestReadFrameRejectsOversizedMessage(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader(fmt.Sprintf("Content-Length: %d\r\n\r\n", MaxMessageBytes+1)))
	if _, err := readFrame(reader); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("err=%v", err)
	}
}
