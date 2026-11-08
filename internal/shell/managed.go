package shell

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
)

// ManagedProcess gives a long-lived stdio server the same containment and owner
// crash supervision as shell commands. Wait must be called exactly once.
// Unlike a shell's optional background release, server exit always reaps its tree.
type ManagedProcess struct {
	cmd      *exec.Cmd
	group    *guardedGroup
	mu       sync.Mutex
	finished bool
	killed   bool
	killErr  error
}

// StartManaged starts behind the existing admission gate. No server instruction
// executes before its containment is established; every failed start rolls back.
func StartManaged(cmd *exec.Cmd) (*ManagedProcess, error) {
	group, err := newGuardedGroup(cmd)
	if err != nil {
		return nil, err
	}
	p := &ManagedProcess{cmd: cmd, group: group}
	cmd.Cancel = p.Kill
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(err, group.release(true))
	}
	if err := group.started(cmd); err != nil {
		_ = cmd.Wait()
		return nil, errors.Join(err, group.release(true))
	}
	return p, nil
}

// Kill is serialized with releasing the pinned group leader. After Wait returns
// no stale numeric process group is ever signalled.
func (p *ManagedProcess) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished {
		return nil
	}
	if !p.killed {
		p.killed = true
		p.killErr = p.group.kill()
	}
	return p.killErr
}

// Wait reaps the server, then its complete contained tree, even when a descendant
// kept stdout or stderr open. Callers must set exec.Cmd.WaitDelay to bound pipes.
func (p *ManagedProcess) Wait() error {
	err := p.cmd.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	cleanup := p.group.release(true)
	p.finished = true
	if cleanup != nil {
		return errors.Join(err, fmt.Errorf("%w: %w", ErrCleanup, cleanup))
	}
	return err
}
