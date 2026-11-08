//go:build !linux

package evalruntime

import (
	"errors"
	"syscall"
)

// Only Linux bounds a worker with a cgroup. Elsewhere the pool is never created
// and memory is host-monitored (see platformLimits).
type cgroupPool struct{}

type cgroupLeaf struct{}

type workerGroup struct{}

func newCgroupPool() (*cgroupPool, error) {
	return nil, errors.New("cgroups are a Linux facility")
}

func (p *cgroupPool) newLeaf(int64) (*cgroupLeaf, error) {
	return nil, errors.New("cgroups are a Linux facility")
}
func (p *cgroupPool) liveLeaves() []string { return nil }
func (p *cgroupPool) close() error         { return nil }

func (l *cgroupLeaf) admit(int) error   { return errors.New("cgroups are a Linux facility") }
func (l *cgroupLeaf) kill() error       { return nil }
func (l *cgroupLeaf) populated() bool   { return false }
func (l *cgroupLeaf) procs() []int      { return nil }
func (l *cgroupLeaf) memoryPeak() int64 { return 0 }
func (l *cgroupLeaf) memoryMax() string { return "" }
func (l *cgroupLeaf) oomKills() int64   { return 0 }
func (l *cgroupLeaf) cpuMillis() int64  { return 0 }
func (l *cgroupLeaf) remove()           {}

func findWorkerGroup(*cgroupLeaf, int) (*workerGroup, error) {
	return nil, errors.New("eval: no sandbox worker group on this platform")
}
func (g *workerGroup) signal(syscall.Signal) error { return errors.New("eval: unsupported") }
func (g *workerGroup) close()                      {}

const cgroupRequired = false
