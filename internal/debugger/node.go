package debugger

import (
	"bytes"
	"context"
	"encoding/json"
	"net"

	"github.com/edynasty/LumenCortex/internal/debugger/dap"
)

// nodeTarget binds the standalone js-debug root to its one launched target.
// No executable, environment, PID or remote endpoint comes from this request.
func (m *Manager) nodeTarget(ctx context.Context, s *session, msg dap.Message) (err error) {
	if msg.Command != "startDebugging" {
		return ErrDenied
	}
	var args struct {
		Request       string `json:"request"`
		Configuration struct {
			Type   string `json:"type"`
			Name   string `json:"name"`
			Target string `json:"__pendingTargetId"`
		} `json:"configuration"`
	}
	decoder := json.NewDecoder(bytes.NewReader(msg.Arguments))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&args); err != nil {
		return ErrDenied
	}
	if args.Request != "launch" || args.Configuration.Type != "pwa-node" || args.Configuration.Target == "" || len(args.Configuration.Target) > 256 {
		return ErrDenied
	}
	select {
	case s.op <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return ErrState
	}
	defer func() {
		<-s.op
		if err != nil {
			s.cancel()
		}
	}()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// A second target cannot silently replace the selected debuggee.
	if len(s.children) != 0 {
		return ErrDenied
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", s.socket)
	if err != nil {
		return err
	}
	client := dap.New(conn)
	s.children = append(s.children, client)
	m.mu.Lock()
	s.client = client
	m.mu.Unlock()
	go m.events(s, client)
	if err = client.Request(ctx, "initialize", map[string]any{"clientID": "lumencortex", "adapterID": s.adapter.ID, "pathFormat": "path", "linesStartAt1": true, "columnsStartAt1": true, "supportsVariableType": true, "supportsVariablePaging": true, "supportsRunInTerminalRequest": false}, nil); err != nil {
		return err
	}
	launched := make(chan error, 1)
	go func() {
		launched <- client.Request(ctx, "launch", map[string]string{"type": "pwa-node", "__pendingTargetId": args.Configuration.Target}, nil)
	}()
	for path, points := range s.breakpoints {
		if err = client.Request(ctx, "setBreakpoints", map[string]any{"source": map[string]string{"path": path}, "breakpoints": points}, nil); err != nil {
			return err
		}
	}
	if err = client.Request(ctx, "configurationDone", struct{}{}, nil); err != nil {
		return err
	}
	select {
	case err = <-launched:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
