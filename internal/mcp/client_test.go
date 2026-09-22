package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestHelperMCPServer(t *testing.T) {
	if os.Getenv("LCX_MCP_HELPER") != "1" {
		return
	}
	reader := bufio.NewReader(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)

	write := func(value any) {
		raw, _ := json.Marshal(value)
		_, _ = writer.Write(raw)
		_, _ = writer.WriteString("\n")
		_ = writer.Flush()
	}
	errorResponse := func(id any, message string) {
		write(map[string]any{
			"jsonrpc": "2.0",
			"id": id,
			"error": map[string]any{"code": -32602, "message": message},
		})
	}

	for {
		raw, err := readJSONLine(reader)
		if err != nil {
			os.Exit(0)
		}
		var msg envelope
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}
		if msg.Method == "notifications/initialized" || msg.Method == "notifications/cancelled" {
			continue
		}
		if len(msg.ID) == 0 {
			continue
		}

		var id any
		_ = json.Unmarshal(msg.ID, &id)
		var params map[string]any
		_ = json.Unmarshal(msg.Params, &params)

		modern := os.Getenv("LCX_MCP_MODERN") == "1"
		if modern {
			meta, ok := params["_meta"].(map[string]any)
			if !ok || meta["io.modelcontextprotocol/protocolVersion"] != ProtocolModern {
				errorResponse(id, "missing modern protocol metadata")
				continue
			}
		}

		switch msg.Method {
		case "initialize":
			write(map[string]any{
				"jsonrpc": "2.0",
				"id": id,
				"result": map[string]any{
					"protocolVersion": ProtocolLegacy,
					"capabilities": map[string]any{"tools": map[string]any{}},
					"serverInfo": map[string]any{"name": "helper", "version": "1"},
				},
			})
		case "server/discover":
			write(map[string]any{
				"jsonrpc": "2.0",
				"id": id,
				"result": map[string]any{
					"protocolVersion": ProtocolModern,
					"capabilities": map[string]any{"tools": map[string]any{}},
					"serverInfo": map[string]any{"name": "helper", "version": "1"},
				},
			})
		case "tools/list":
			write(map[string]any{
				"jsonrpc": "2.0",
				"id": id,
				"result": map[string]any{
					"tools": []map[string]any{{
						"name": "echo",
						"description": "Echo text",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{"text": map[string]any{"type": "string"}},
						},
					}},
				},
			})
		case "tools/call":
			name, _ := params["name"].(string)
			args, _ := params["arguments"].(map[string]any)
			if name == "huge" {
				write(map[string]any{
					"jsonrpc": "2.0",
					"id": id,
					"result": map[string]any{
						"content": []map[string]any{{"type": "text", "text": strings.Repeat("x", MaxToolResultBytes+1024)}},
					},
				})
				continue
			}
			text, _ := args["text"].(string)
			write(map[string]any{
				"jsonrpc": "2.0",
				"id": id,
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": text}},
				},
			})
		case "slow":
			time.Sleep(2 * time.Second)
			write(map[string]any{
				"jsonrpc": "2.0",
				"id": id,
				"result": map[string]any{"late": true},
			})
		default:
			write(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
		}
	}
}

func startHelperMCP(t *testing.T, mode ProtocolMode) *Client {
	t.Helper()
	t.Setenv("LCX_MCP_HELPER", "1")
	if mode == ModeModern {
		t.Setenv("LCX_MCP_MODERN", "1")
	} else {
		t.Setenv("LCX_MCP_MODERN", "0")
	}
	client, err := Start(context.Background(), Config{
		ID: "helper",
		Name: "Helper",
		Command: os.Args[0],
		Args: []string{"-test.run=TestHelperMCPServer"},
		Workspace: t.TempDir(),
		ProtocolMode: mode,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestLegacyMCPDiscoveryAndToolInvocation(t *testing.T) {
	client := startHelperMCP(t, ModeLegacy)

	status := client.Status()
	if !status.Running || status.PID == 0 || status.ProtocolVersion != ProtocolLegacy || status.Tools != 1 {
		t.Fatalf("status=%#v", status)
	}
	tools := client.Tools()
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools=%#v", tools)
	}

	result, err := client.CallTool(context.Background(), "echo", map[string]any{"text": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "hello" {
		t.Fatalf("result=%#v", result)
	}
}

func TestModernMCPAddsProtocolMetadata(t *testing.T) {
	client := startHelperMCP(t, ModeModern)
	status := client.Status()
	if status.ProtocolMode != ModeModern || status.ProtocolVersion != ProtocolModern {
		t.Fatalf("status=%#v", status)
	}
	result, err := client.CallTool(context.Background(), "echo", map[string]any{"text": "modern"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "modern" {
		t.Fatalf("result=%#v", result)
	}
}

func TestMCPToolResultIsBounded(t *testing.T) {
	client := startHelperMCP(t, ModeLegacy)
	_, err := client.CallTool(context.Background(), "huge", map[string]any{})
	if !errors.Is(err, ErrToolResultTooLarge) {
		t.Fatalf("err=%v", err)
	}
}

func TestMCPRequestCancellationReleasesPending(t *testing.T) {
	client := startHelperMCP(t, ModeLegacy)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	var result map[string]any
	err := client.Request(ctx, "slow", map[string]any{}, &result)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if got := client.Status().PendingRequests; got != 0 {
		t.Fatalf("pending=%d", got)
	}
}

func TestMCPReadLineRejectsOversizedMessage(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader(strings.Repeat("x", MaxMessageBytes+1) + "\n"))
	if _, err := readJSONLine(reader); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("err=%v", err)
	}
}

func TestMCPPendingMapLimit(t *testing.T) {
	client := startHelperMCP(t, ModeLegacy)
	client.pendingMu.Lock()
	for i := 0; i < MaxPendingRequests; i++ {
		client.pending[int64(1000+i)] = make(chan response, 1)
	}
	client.pendingMu.Unlock()
	defer func() {
		client.pendingMu.Lock()
		for id := range client.pending {
			if id >= 1000 {
				delete(client.pending, id)
			}
		}
		client.pendingMu.Unlock()
	}()

	var result map[string]any
	err := client.Request(context.Background(), "ping", map[string]any{}, &result)
	if !errors.Is(err, ErrTooManyPending) {
		t.Fatalf("err=%v", err)
	}
}

func TestMCPProtocolConstantsAreDistinct(t *testing.T) {
	if ProtocolLegacy == ProtocolModern {
		t.Fatal(fmt.Sprintf("protocol versions must differ: %s", ProtocolLegacy))
	}
}
