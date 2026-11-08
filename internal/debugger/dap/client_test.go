package dap

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOutOfOrderResponsesAndInterleavedEvent(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := New(a)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	peer := make(chan error, 1)
	go func() {
		r := bufio.NewReader(b)
		messages := make([]Message, 8)
		for i := range messages {
			m, err := readMessage(r)
			if err != nil {
				peer <- err
				return
			}
			messages[i] = m
		}
		for i := len(messages) - 1; i >= 0; i-- {
			m := messages[i]
			if err := writeMessage(b, Message{Type: "event", Event: "stopped", Body: json.RawMessage(`{"reason":"breakpoint"}`)}); err != nil {
				peer <- err
				return
			}
			if err := writeMessage(b, Message{Type: "response", RequestSeq: m.Seq, Command: m.Command, Success: true, Body: m.Arguments}); err != nil {
				peer <- err
				return
			}
		}
		peer <- nil
	}()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(want int) {
			defer wg.Done()
			var got struct {
				Value int `json:"value"`
			}
			if err := c.Request(ctx, "variables", map[string]int{"value": want}, &got); err != nil {
				t.Error(err)
				return
			}
			if got.Value != want {
				t.Errorf("response misrouted got %d want %d", got.Value, want)
			}
		}(i)
	}
	wg.Wait()
	if err := <-peer; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		select {
		case m := <-c.Events():
			if m.Event != "stopped" {
				t.Fatalf("wrong event %+v", m)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func TestInvalidAndTruncatedFrames(t *testing.T) {
	for _, wire := range []string{
		"Content-Length: 4194305\r\n\r\n", "Content-Length: -1\r\n\r\n", "Content-Length: +1\r\n\r\n", "Content-Length: 4\r\nContent-Length: 4\r\n\r\nnull", "Other: x\r\n\r\n", "Content-Length: 2\n\n{}", "Content-Length: 100\r\n\r\n{}", strings.Repeat("x", 8193) + "\n", "Content-Length: 4\r\n\r\nnull",
	} {
		t.Run(fmt.Sprint(len(wire), wire[:min(len(wire), 20)]), func(t *testing.T) {
			if _, err := readMessage(bufio.NewReaderSize(strings.NewReader(wire), maxHeaders)); err == nil {
				t.Fatal("invalid frame accepted")
			}
		})
	}
}
func TestCancelledWriteUnblocksTransport(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := New(a)
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Request(ctx, "launch", map[string]string{"program": "fixture"}, nil) }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancel succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("write ignored cancellation")
	}
}
func TestResponseCommandMismatchFailsClosed(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := New(a)
	defer c.Close()
	go func() {
		m, _ := readMessage(bufio.NewReader(b))
		_ = writeMessage(b, Message{Type: "response", RequestSeq: m.Seq, Command: "evaluate", Success: true})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Request(ctx, "threads", nil, nil); err == nil {
		t.Fatal("mismatched command accepted")
	}
}
