package evalruntime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRPCFrameLimit: a peer that writes past the frame bound is failed, never
// buffered without limit.
func TestRPCFrameLimit(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	conn := newRPCConn(inR, outW, nil, nil)
	conn.start()
	defer conn.close(errConnClosed)
	go func() {
		_, _ = io.Copy(io.Discard, outR)
	}()
	huge := strings.Repeat("x", maxFrame+1024)
	_, _ = inW.Write([]byte(huge + "\n"))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-conn.done:
			if !errors.Is(conn.connErr(), errFrameTooLarge) {
				t.Fatalf("close reason = %v", conn.connErr())
			}
			return
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
	t.Fatalf("connection did not close on an oversized frame")
}

// TestRPCCorrelation answers two calls out of order and checks each reply lands
// on its own caller.
func TestRPCCorrelation(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	conn := newRPCConn(inR, outW, nil, nil)
	conn.start()
	defer conn.close(errConnClosed)

	type reply struct {
		id     string
		result json.RawMessage
		err    error
	}
	requests := make(chan string, 2)
	go func() {
		reader := bufio.NewReader(outR)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			var msg rpcMessage
			if json.Unmarshal(line, &msg) != nil {
				continue
			}
			requests <- string(msg.ID)
		}
	}()

	results := make(chan reply, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			raw, err := conn.call(context.Background(), "ping", map[string]int{"index": index})
			results <- reply{id: string(raw), result: raw, err: err}
		}(i)
	}
	first := <-requests
	second := <-requests
	// Reply in reverse order.
	for _, id := range []string{second, first} {
		msg := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": "value-" + id}
		data, _ := json.Marshal(msg)
		_, _ = inW.Write(append(data, '\n'))
	}
	wg.Wait()
	close(results)
	seen := map[string]bool{}
	for reply := range results {
		if reply.err != nil {
			t.Fatalf("call failed: %v", reply.err)
		}
		seen[string(reply.result)] = true
	}
	if len(seen) != 2 {
		t.Fatalf("responses were not correlated: %v", seen)
	}
}

// TestRPCInboundRequestsAndNotifications exercises the worker -> host direction.
func TestRPCInboundRequestsAndNotifications(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	notifications := make(chan string, 4)
	conn := newRPCConn(inR, outW, func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		if method != "tool/call" {
			return nil, errors.New("unsupported")
		}
		return map[string]any{"ok": true}, nil
	}, func(method string, params json.RawMessage) {
		notifications <- method
	})
	conn.start()
	defer conn.close(errConnClosed)

	responses := make(chan map[string]any, 1)
	go func() {
		reader := bufio.NewReader(outR)
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var msg map[string]any
		_ = json.Unmarshal(line, &msg)
		responses <- msg
	}()

	_, _ = inW.Write([]byte(`{"jsonrpc":"2.0","id":"w1","method":"tool/call","params":{"name":"x"}}` + "\n"))
	select {
	case msg := <-responses:
		result, _ := msg["result"].(map[string]any)
		if result["ok"] != true {
			t.Fatalf("response = %+v", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no response to an inbound request")
	}
	_, _ = inW.Write([]byte(`{"jsonrpc":"2.0","method":"event","params":{"type":"display"}}` + "\n"))
	select {
	case method := <-notifications:
		if method != "event" {
			t.Fatalf("notification = %q", method)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no notification delivered")
	}
}

func TestRPCErrorPropagates(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	conn := newRPCConn(inR, outW, nil, nil)
	conn.start()
	defer conn.close(errConnClosed)
	go func() {
		reader := bufio.NewReader(outR)
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var msg rpcMessage
		if json.Unmarshal(line, &msg) != nil {
			return
		}
		response := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(msg.ID),
			"error": map[string]any{"code": -32000, "message": "worker said no"}}
		data, _ := json.Marshal(response)
		_, _ = inW.Write(append(data, '\n'))
	}()
	_, err := conn.call(context.Background(), "ping", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "worker said no") {
		t.Fatalf("error = %v", err)
	}
}

func TestRPCContextCancellation(t *testing.T) {
	inR, _ := io.Pipe()
	outR, outW := io.Pipe()
	conn := newRPCConn(inR, outW, nil, nil)
	conn.start()
	defer conn.close(errConnClosed)
	go func() { _, _ = io.Copy(io.Discard, outR) }()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := conn.call(ctx, "slow", map[string]any{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call error = %v", err)
	}
}
