package protocol

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
)

// Echo is provider data an adapter needs back, unchanged, with the assistant message that produced it
// within the current tool turn: for example reasoning_content or signed thinking blocks (design §4.6).
//
// It is run memory. It never reaches a transcript row, a step record, an event, a log line, Desktop or
// a later run: it has no serialized form (MarshalJSON fails), it prints and logs as its format and size
// only, and its bytes sit behind a pointer so even a reflective dump of an enclosing struct shows an
// address. Only the adapter that produced it reads Data, to put it back on the wire.
type Echo struct {
	format string
	tokens int
	data   *echoData
}

type echoData struct{ b []byte }

// ErrEchoNotSerializable is returned by every attempt to marshal an Echo.
var ErrEchoNotSerializable = errors.New("provider echo is run memory and has no serialized form")

// NewEcho copies data. format is adapter-private (for example "reasoning_content"); tokens is the size
// the adapter reports, which the request window counts toward the echo's row (design §5).
func NewEcho(format string, data []byte, tokens int) Echo {
	if len(data) == 0 {
		return Echo{}
	}
	return Echo{format: format, tokens: max(tokens, 0), data: &echoData{b: bytes.Clone(data)}}
}

// IsZero reports whether the echo holds nothing.
func (e Echo) IsZero() bool { return e.data == nil }

// Kind is the adapter-private format of the echo.
func (e Echo) Kind() string { return e.format }

// Tokens is the size the adapter reported.
func (e Echo) Tokens() int { return e.tokens }

// Data is for the adapter that encodes the echo into its wire format; nothing else reads it.
func (e Echo) Data() []byte {
	if e.data == nil {
		return nil
	}
	return e.data.b
}

// MarshalJSON fails closed: a struct that holds an Echo without `json:"-"` cannot be serialized.
func (Echo) MarshalJSON() ([]byte, error) { return nil, ErrEchoNotSerializable }

// MarshalText fails closed for every text encoder.
func (Echo) MarshalText() ([]byte, error) { return nil, ErrEchoNotSerializable }

func (e Echo) String() string       { return fmt.Sprintf("echo(%s, %d bytes)", e.format, len(e.Data())) }
func (e Echo) GoString() string     { return e.String() }
func (e Echo) LogValue() slog.Value { return slog.StringValue(e.String()) }
