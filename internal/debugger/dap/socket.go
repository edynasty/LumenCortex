package dap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"time"

	"github.com/edynasty/LumenCortex/internal/shell"
	"github.com/edynasty/LumenCortex/internal/streambuf"
)

// StartUnix connects only to the socket in the manager's newly-created private
// directory. It never attaches to an arbitrary caller-supplied network server.
func StartUnix(ctx context.Context, cmd *exec.Cmd, socket string) (*Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stderr := streambuf.New(4096, 4096)
	cmd.Stdout = stderr
	cmd.Stderr = stderr
	cmd.WaitDelay = 2 * time.Second
	owned, err := shell.StartManaged(cmd)
	if err != nil {
		return nil, err
	}
	p := &Process{Stderr: stderr, owned: owned, done: make(chan struct{})}
	go func() { p.err = owned.Wait(); close(p.done) }()
	connect, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		conn, err := (&net.Dialer{}).DialContext(connect, "unix", socket)
		if err == nil {
			p.Client = New(conn)
			go func() {
				select {
				case <-ctx.Done():
				case <-p.Client.Done():
				case <-p.done:
					p.Client.fail(ErrClosed)
					return
				}
				_ = owned.Kill()
			}()
			return p, nil
		}
		select {
		case <-connect.Done():
			_ = owned.Kill()
			<-p.done
			return nil, errors.Join(connect.Err(), p.err)
		case <-p.done:
			return nil, fmt.Errorf("adapter exited before Unix socket ready: %w; %s", p.err, string(stderr.Snapshot().Head))
		case <-time.After(10 * time.Millisecond):
		}
	}
}
