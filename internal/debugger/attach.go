package debugger

import (
	"context"
	"time"
)

// Attach never accepts a PID from the caller. The ID names a target started by
// this manager; its parent keeps the child unreaped for the entire session.
func (m *Manager) Attach(ctx context.Context, c Control) (Snapshot, error) {
	s, unlock, err := m.lock(ctx, c)
	if err != nil {
		return Snapshot{}, err
	}
	defer unlock()
	m.mu.Lock()
	state := s.snapshot.State
	pid := s.snapshot.TargetPID
	m.mu.Unlock()
	if !s.opts.AllowAttach || state != "prepared" || pid <= 1 || s.adapter.AttachPIDField == "" {
		return Snapshot{}, ErrDenied
	}
	args := make(map[string]any, len(s.adapter.Attach)+3)
	for k, v := range s.adapter.Attach {
		args[k] = v
	}
	args[s.adapter.AttachPIDField] = pid
	args["program"] = s.opts.Program
	args["cwd"] = s.opts.Workspace
	launched := make(chan error, 1)
	go func() { launched <- s.client.Request(s.life, "attach", args, nil) }()
	wait, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	consumed := false
	select {
	case <-s.initialized:
	case err = <-launched:
		consumed = true
		if err != nil {
			s.cancel()
			return Snapshot{}, err
		}
		select {
		case <-s.initialized:
		case <-wait.Done():
			s.cancel()
			return Snapshot{}, wait.Err()
		}
	case <-wait.Done():
		s.cancel()
		return Snapshot{}, wait.Err()
	}
	if !consumed {
		go func() {
			select {
			case err := <-launched:
				if err != nil {
					s.cancel()
				}
			case <-s.life.Done():
			}
		}()
	}
	m.mu.Lock()
	if s.snapshot.State == "prepared" {
		s.snapshot.State = "configuring"
	}
	snapshot := s.snapshot
	m.mu.Unlock()
	m.emit(s, "debug.session.attached", map[string]any{"status": snapshot.State})
	return snapshot, nil
}
