package resource

import (
	"errors"
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

type Manager struct {
	budget       Budget
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
		b.MaxAgents = 3
	}
	return &Manager{budget: b}
}

func (m *Manager) Budget() Budget      { return m.budget }
func (m *Manager) UsedBytes() int64    { return m.used.Load() }
func (m *Manager) ActiveAgents() int   { return int(m.activeAgents.Load()) }
func (m *Manager) UnderPressure() bool { return m.used.Load() >= m.budget.SoftBytes }

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


func (m *Manager) AcquireAgent() (func(), error) {
	for {
		current := m.activeAgents.Load()
		if current >= int64(m.budget.MaxAgents) {
			return nil, ErrAgentLimitExceeded
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
