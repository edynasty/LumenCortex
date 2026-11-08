package evalruntime

import "testing"

// A tracked descendant is only ever signalled while its pid still names the
// process that was recorded: once the pid is reused by an unrelated process
// (different start time), it is neither killed nor reported as residual.
func TestTrackedDescendantIgnoresReusedPID(t *testing.T) {
	p := &process{pid: 100, tracked: map[int]string{200: "Sat Nov  8 03:00:00 2026", 300: "Sat Nov  8 03:00:01 2026"}}
	table := []procEntry{
		{pid: 200, ppid: 1, pgid: 200, start: "Sat Nov  8 03:00:00 2026"}, // same process
		{pid: 300, ppid: 1, pgid: 300, start: "Sat Nov  8 04:12:09 2026"}, // pid reused
	}
	got := p.liveTracked(table)
	if len(got) != 1 || got[0] != 200 {
		t.Fatalf("liveTracked = %v, want only the recorded process 200", got)
	}
}
