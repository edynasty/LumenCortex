package resource

import (
	"errors"
	"testing"
)

func TestReserveHonorsHardLimitAndRelease(t *testing.T) {
	m := New(Budget{SoftBytes: 100, HardBytes: 120, MaxAgents: 1})
	release, err := m.Reserve(100)
	if err != nil {
		t.Fatal(err)
	}
	if !m.UnderPressure() {
		t.Fatal("expected soft pressure")
	}
	if _, err := m.Reserve(21); !errors.Is(err, ErrMemoryBudgetExceeded) {
		t.Fatalf("expected hard limit error, got %v", err)
	}
	release()
	if got := m.UsedBytes(); got != 0 {
		t.Fatalf("used=%d want=0", got)
	}
}


func TestAcquireAgentHonorsLimitAndReleasesExactlyOnce(t *testing.T) {
	m := New(Budget{SoftBytes: 100, HardBytes: 120, MaxAgents: 2})
	releaseA, err := m.AcquireAgent()
	if err != nil {
		t.Fatal(err)
	}
	releaseB, err := m.AcquireAgent()
	if err != nil {
		t.Fatal(err)
	}
	if got := m.ActiveAgents(); got != 2 {
		t.Fatalf("active=%d want=2", got)
	}
	if _, err := m.AcquireAgent(); !errors.Is(err, ErrAgentLimitExceeded) {
		t.Fatalf("expected agent limit error, got %v", err)
	}
	releaseA()
	releaseA()
	if got := m.ActiveAgents(); got != 1 {
		t.Fatalf("active after release=%d want=1", got)
	}
	releaseC, err := m.AcquireAgent()
	if err != nil {
		t.Fatal(err)
	}
	releaseB()
	releaseC()
	if got := m.ActiveAgents(); got != 0 {
		t.Fatalf("active=%d want=0", got)
	}
}
