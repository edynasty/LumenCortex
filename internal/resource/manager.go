package resource

import (
	"errors"
	"fmt"
	"sync/atomic"
)

var (
	ErrMemoryBudgetExceeded = errors.New("runtime memory budget exceeded")
	ErrAgentLimitExceeded   = errors.New("runtime agent concurrency limit exceeded")
)

type Budget struct {
	SoftBytes int64 `json:"softBytes"`
	HardBytes int64 `json:"hardBytes"`
	MaxAgents int   `json:"maxAgents"`
}

// Manager accounts the bounded in-process working memory and the running agents of one Engine.
// The agent limit may change while agents run (SetMaxAgents): a lower limit never stops a running
// agent, it only refuses new ones until enough have finished.
type Manager struct {
	budget       Budget
	maxAgents    atomic.Int64
	used         atomic.Int64
	activeAgents atomic.Int64
}

func New(b Budget) *Manager {
	if b.SoftBytes <= 0 {
		b.SoftBytes = 768 << 20
	}
	if b.HardBytes <= 0 {
		b.HardBytes = 1024 << 20
	}
	if b.HardBytes < b.SoftBytes {
		b.HardBytes = b.SoftBytes
	}
	if b.MaxAgents <= 0 {
		b.MaxAgents = 4
	}
	m := &Manager{budget: b}
	m.maxAgents.Store(int64(b.MaxAgents))
	return m
}

// Budget reports the budget with the agent limit currently in force.
func (m *Manager) Budget() Budget {
	b := m.budget
	b.MaxAgents = m.MaxAgents()
	return b
}
func (m *Manager) MaxAgents() int      { return int(m.maxAgents.Load()) }
func (m *Manager) UsedBytes() int64    { return m.used.Load() }
func (m *Manager) ActiveAgents() int   { return int(m.activeAgents.Load()) }
func (m *Manager) UnderPressure() bool { return m.used.Load() >= m.budget.SoftBytes }

// SetMaxAgents changes the agent limit for later AcquireAgent calls; n <= 0 is ignored.
func (m *Manager) SetMaxAgents(n int) {
	if n > 0 {
		m.maxAgents.Store(int64(n))
	}
}

// Reserve accounts bounded in-process working memory. It is deliberately explicit:
// disk-backed session/history bytes should not be reserved here.
func (m *Manager) Reserve(bytes int64) (func(), error) {
	if bytes <= 0 {
		return func() {}, nil
	}
	for {
		current := m.used.Load()
		next := current + bytes
		if next > m.budget.HardBytes {
			return nil, ErrMemoryBudgetExceeded
		}
		if m.used.CompareAndSwap(current, next) {
			var released atomic.Bool
			return func() {
				if released.CompareAndSwap(false, true) {
					m.used.Add(-bytes)
				}
			}, nil
		}
	}
}

// AcquireAgent takes one agent slot or fails at once with ErrAgentLimitExceeded; it never waits.
func (m *Manager) AcquireAgent() (func(), error) {
	for {
		current := m.activeAgents.Load()
		if limit := m.maxAgents.Load(); current >= limit {
			return nil, fmt.Errorf("%w (%d running, limit %d)", ErrAgentLimitExceeded, current, limit)
		}
		if m.activeAgents.CompareAndSwap(current, current+1) {
			var released atomic.Bool
			return func() {
				if released.CompareAndSwap(false, true) {
					m.activeAgents.Add(-1)
				}
			}, nil
		}
	}
}
