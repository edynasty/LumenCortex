//go:build darwin

package evalruntime

import (
	"errors"
	"syscall"
	"time"
)

// startForkWatcher makes descendant discovery immediate on macOS: a kqueue
// EVFILT_PROC/NOTE_FORK registration on the worker and on every child it spawns
// triggers a process-table scan the moment a fork happens, instead of waiting for
// the next poll. It is an accelerator, not a containment boundary: if kqueue is
// unavailable the 150ms poller still runs, and neither mechanism can observe a
// descendant that was never recorded. See process for the exact limitation.
func startForkWatcher(p *process) func() {
	kq, err := syscall.Kqueue()
	if err != nil {
		return func() {}
	}
	register := func(pid int) bool {
		if pid <= 0 {
			return false
		}
		changes := []syscall.Kevent_t{{
			Ident:  uint64(pid),
			Filter: syscall.EVFILT_PROC,
			Flags:  syscall.EV_ADD | syscall.EV_ENABLE,
			Fflags: syscall.NOTE_FORK | syscall.NOTE_EXIT,
		}}
		_, err := syscall.Kevent(kq, changes, nil, nil)
		return err == nil
	}
	if !register(p.pid) {
		_ = syscall.Close(kq)
		return func() {}
	}
	registered := map[int]bool{p.pid: true}
	stop := make(chan struct{})
	go func() {
		defer syscall.Close(kq)
		events := make([]syscall.Kevent_t, 16)
		for {
			select {
			case <-p.stopMon:
				return
			case <-stop:
				return
			default:
			}
			timeout := syscall.NsecToTimespec(int64(250 * time.Millisecond))
			n, err := syscall.Kevent(kq, nil, events, &timeout)
			if err != nil {
				if errors.Is(err, syscall.EINTR) {
					continue
				}
				return
			}
			for i := range n {
				event := events[i]
				if event.Fflags&syscall.NOTE_FORK == 0 {
					continue
				}
				p.scanDescendants()
				for _, child := range p.trackedSnapshot() {
					if registered[child] {
						continue
					}
					if register(child) {
						registered[child] = true
					}
				}
			}
		}
	}()
	return func() { close(stop) }
}
