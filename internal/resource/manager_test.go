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
