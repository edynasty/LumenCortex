// Package dap implements bounded Debug Adapter Protocol framing and correlation.
package dap

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const MaxMessage = 4 << 20
const maxHeaders = 8 << 10

var ErrProtocol = errors.New("invalid DAP protocol")

type Message struct {
	Seq        int64           `json:"seq"`
	Type       string          `json:"type"`
	RequestSeq int64           `json:"request_seq,omitempty"`
	Command    string          `json:"command,omitempty"`
	Event      string          `json:"event,omitempty"`
	Success    bool            `json:"success"`
	Message    string          `json:"message,omitempty"`
	Body       json.RawMessage `json:"body,omitempty"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
}

func readMessage(r *bufio.Reader) (Message, error) {
	var result Message
	length, total := -1, 0
	for {
		line, err := r.ReadSlice('\n')
		if err != nil {
			return result, fmt.Errorf("DAP header: %w", err)
		}
		total += len(line)
		if total > maxHeaders || len(line) < 2 || line[len(line)-2] != '\r' {
			return result, ErrProtocol
		}
		if len(line) == 2 {
			break
		}
		key, value, ok := strings.Cut(string(line[:len(line)-2]), ":")
		if !ok || strings.TrimSpace(key) != key || key == "" {
			return result, ErrProtocol
		}
		if strings.EqualFold(key, "Content-Length") {
			value = strings.TrimSpace(value)
			if length >= 0 || value == "" || strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
				return result, ErrProtocol
			}
			length, err = strconv.Atoi(value)
			if err != nil || length <= 0 || length > MaxMessage {
				return result, ErrProtocol
			}
		}
	}
	if length < 1 {
		return result, ErrProtocol
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return result, fmt.Errorf("DAP body: %w", err)
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return result, fmt.Errorf("%w: JSON", ErrProtocol)
	}
	switch result.Type {
	case "response":
		if result.RequestSeq <= 0 || result.Command == "" {
			return result, ErrProtocol
		}
	case "event":
		if result.Event == "" {
			return result, ErrProtocol
		}
	case "request":
		if result.Seq <= 0 || result.Command == "" {
			return result, ErrProtocol
		}
	default:
		return result, ErrProtocol
	}
	return result, nil
}

func writeMessage(w io.Writer, m Message) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(body) > MaxMessage {
		return ErrProtocol
	}
	header := strconv.AppendInt([]byte("Content-Length: "), int64(len(body)), 10)
	header = append(header, '\r', '\n', '\r', '\n')
	for _, part := range [][]byte{header, body} {
		for len(part) > 0 {
			n, err := w.Write(part)
			if err != nil {
				return err
			}
			if n == 0 {
				return io.ErrShortWrite
			}
			part = part[n:]
		}
	}
	return nil
}
