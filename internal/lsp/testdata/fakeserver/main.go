// Command fakeserver is a tiny language server for the LSP manager tests: Content-Length framed
// JSON-RPC over stdio, no dependencies, deterministic answers. It is testdata, so the go tool does not
// build it with ./...; a test builds it once into a temporary directory.
//
// Environment:
//
//	FAKE_LSP_START_LOG    append "<pid> <unix nanos>" when the server starts
//	FAKE_LSP_SLOW_MS      delay every request by this many milliseconds
//	FAKE_LSP_CRASH_METHOD exit(3) instead of answering this method
//	FAKE_LSP_CANCEL_LOG   append "<id>" when $/cancelRequest names that request
//	FAKE_LSP_RENAME_JSON  the workspace edit textDocument/rename answers with, verbatim
//	FAKE_LSP_IGNORE_EXIT  never exit on the "exit" notification (tests the kill path)
//
// Flags override the environment for one server: -slow, -crash, -start-log, -cancel-log,
// -rename-file, -ignore-exit. -rename-file is read on every rename, so a test can change the edit it
// answers with while the server runs.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func main() {
	slowFlag := flag.Int("slow", 0, "delay every request by this many milliseconds")
	crashFlag := flag.String("crash", "", "exit(3) instead of answering this method")
	crashOnceFlag := flag.String("crash-once", "", "crash on the method above only while this file exists, and remove it")
	startLogFlag := flag.String("start-log", "", "append this process's pid here on start")
	cancelLogFlag := flag.String("cancel-log", "", "append a cancelled request id here")
	renameFileFlag := flag.String("rename-file", "", "file holding the workspace edit a rename answers with")
	ignoreExitFlag := flag.Bool("ignore-exit", false, "never exit on the exit notification")
	flag.Parse()

	startLog := firstNonEmpty(*startLogFlag, os.Getenv("FAKE_LSP_START_LOG"))
	cancelLog := firstNonEmpty(*cancelLogFlag, os.Getenv("FAKE_LSP_CANCEL_LOG"))
	renameFile := firstNonEmpty(*renameFileFlag, os.Getenv("FAKE_LSP_RENAME_FILE"))
	if startLog != "" {
		appendLine(startLog, fmt.Sprintf("%d %d", os.Getpid(), time.Now().UnixNano()))
	}
	reader := bufio.NewReader(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)
	write := func(value any) {
		raw, err := json.Marshal(value)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(writer, "Content-Length: %d\r\n\r\n", len(raw))
		_, _ = writer.Write(raw)
		_ = writer.Flush()
	}
	slow := *slowFlag
	if value, err := strconv.Atoi(os.Getenv("FAKE_LSP_SLOW_MS")); err == nil && slow == 0 {
		slow = value
	}
	crashMethod := firstNonEmpty(*crashFlag, os.Getenv("FAKE_LSP_CRASH_METHOD"))
	ignoreExit := *ignoreExitFlag || os.Getenv("FAKE_LSP_IGNORE_EXIT") == "1"

	for {
		raw, err := readFrame(reader)
		if err != nil {
			return
		}
		var msg envelope
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}
		switch msg.Method {
		case "exit":
			if !ignoreExit {
				return
			}
			continue
		case "$/cancelRequest":
			if cancelLog != "" {
				appendLine(cancelLog, strings.TrimSpace(string(msg.Params)))
			}
			continue
		case "textDocument/didOpen":
			var params struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
			}
			_ = json.Unmarshal(msg.Params, &params)
			write(map[string]any{
				"jsonrpc": "2.0",
				"method":  "textDocument/publishDiagnostics",
				"params": map[string]any{
					"uri": params.TextDocument.URI,
					"diagnostics": []map[string]any{{
						"range": map[string]any{
							"start": map[string]any{"line": 0, "character": 0},
							"end":   map[string]any{"line": 0, "character": 4},
						},
						"severity": 2,
						"source":   "fake",
						"message":  "fake diagnostic",
					}},
				},
			})
			continue
		}
		if len(msg.ID) == 0 {
			continue
		}
		if slow > 0 {
			time.Sleep(time.Duration(slow) * time.Millisecond)
		}
		if crashMethod != "" && msg.Method == crashMethod && crashApplies(*crashOnceFlag) {
			os.Exit(3)
		}
		var id any
		_ = json.Unmarshal(msg.ID, &id)
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"capabilities": map[string]any{
				"hoverProvider": true, "definitionProvider": true, "referencesProvider": true,
				"documentSymbolProvider": true, "renameProvider": true,
			}}
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
			raw := os.Getenv("FAKE_LSP_RENAME_JSON")
			if renameFile != "" {
				if content, err := os.ReadFile(renameFile); err == nil {
					raw = string(content)
				}
			}
			if raw != "" {
				var edit json.RawMessage
				if json.Unmarshal([]byte(raw), &edit) == nil {
					write(map[string]any{"jsonrpc": "2.0", "id": id, "result": edit})
					continue
				}
			}
			result = map[string]any{"changes": map[string]any{}}
		default:
			result = nil
		}
		write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
}

// crashApplies reports whether a configured crash is still due: without a marker file it always is,
// with one it is due while the file exists, and that file is removed so the restart answers normally.
func crashApplies(marker string) bool {
	if marker == "" {
		return true
	}
	if _, err := os.Stat(marker); err != nil {
		return false
	}
	_ = os.Remove(marker)
	return true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func appendLine(path, line string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(f, line)
	_ = f.Close()
}

func readFrame(reader *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(key), "Content-Length") {
			if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
				length = n
			}
		}
	}
	if length < 0 {
		return nil, io.ErrUnexpectedEOF
	}
	raw := make([]byte, length)
	if _, err := io.ReadFull(reader, raw); err != nil {
		return nil, err
	}
	return raw, nil
}
