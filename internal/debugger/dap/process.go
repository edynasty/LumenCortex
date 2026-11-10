package dap

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/edynasty/LumenCortex/internal/shell"
	"github.com/edynasty/LumenCortex/internal/streambuf"
)

type pipeConnection struct {
	reader, writer *os.File
	once           sync.Once
}

func (p *pipeConnection) Read(b []byte) (int, error)         { return p.reader.Read(b) }
func (p *pipeConnection) Write(b []byte) (int, error)        { return p.writer.Write(b) }
func (p *pipeConnection) SetWriteDeadline(t time.Time) error { return p.writer.SetWriteDeadline(t) }
func (p *pipeConnection) Close() error {
	var err error
	p.once.Do(func() { err = errors.Join(p.reader.Close(), p.writer.Close()) })
	return err
}

type Process struct {
	Client *Client
	Stderr *streambuf.HeadTail
	owned  *shell.ManagedProcess
	pid    int
	done   chan struct{}
	err    error
}

// Start owns a host-built command. The caller supplies its sandboxed command and
// approved environment; this layer only owns transport and process lifetime.
func Start(ctx context.Context, cmd *exec.Cmd) (*Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	rollback := func() { inR.Close(); inW.Close(); outR.Close(); outW.Close() }
	cmd.Stdin = inR
	cmd.Stdout = outW
	stderr := streambuf.New(4096, 4096)
	cmd.Stderr = stderr
	cmd.WaitDelay = 2 * time.Second
	owned, err := shell.StartManaged(cmd)
	if err != nil {
		rollback()
		return nil, err
	}
	_ = inR.Close()
	_ = outW.Close()
	p := &Process{Client: New(&pipeConnection{reader: outR, writer: inW}), Stderr: stderr, owned: owned, pid: cmd.Process.Pid, done: make(chan struct{})}
	go func() { p.err = owned.Wait(); p.Client.fail(io.EOF); close(p.done) }()
	go func() {
		select {
		case <-ctx.Done():
		case <-p.Client.Done():
		case <-p.done:
			return
		}
		_ = owned.Kill()
	}()
	return p, nil
}
func (p *Process) Done() <-chan struct{} { return p.done }

// PID is the adapter process, reported for host observability only.
func (p *Process) PID() int { return p.pid }
func (p *Process) Close() error {
	_ = p.Client.Close()
	err := p.owned.Kill()
	<-p.done
	// Forced adapter shutdown is expected. Cleanup failures are not.
	if errors.Is(p.err, shell.ErrCleanup) {
		err = errors.Join(err, p.err)
	}
	return err
}
