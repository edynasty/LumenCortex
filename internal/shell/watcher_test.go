package shell

import (
	"context"
	"runtime"
	"testing"
	"time"
)

func TestRepeatedTimeoutsJoinCleanupWatchers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command fixture")
	}
	r := New(t.TempDir())
	r.Interpreter = System()
	r.Interpreter.Args = []string{"-c"}
	baseline := runtime.NumGoroutine()
	for i := range 8 {
		result, err := r.RunTimeout(context.Background(), "sleep 30", 50*time.Millisecond, nil)
		if err != nil || !result.TimedOut {
			t.Fatalf("iteration%d result=%+v err=%v", i, result, err)
		}
	}
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+2 {
		t.Fatalf("cleanup leaked goroutines: before=%d after=%d", baseline, got)
	}
	result, err := r.Run(context.Background(), "echo slot-reused", nil)
	if err != nil || result.ExitCode != 0 || result.Cancelled {
		t.Fatalf("subsequent command=%+v err=%v", result, err)
	}
}
