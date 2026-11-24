package agent

// Review probe (throwaway; /tmp only): independent R2-04 measurement with wall AND CPU time, malloc
// counts and SQLite query counts, repeated 3x, on the same seeded workload for 1a68226 and 7d064d0.

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

func zzCPU() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func TestZZR3Profile(t *testing.T) {
	ctx := context.Background()
	rawStore, err := session.Open(filepath.Join(t.TempDir(), "prof.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rawStore.Close() }()
	const sid, maxCtx = "s-zz-prof", 32768
	if err := rawStore.Create(ctx, session.Session{ID: sid, Goal: "prof", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rawStore.BeginRun(ctx, sid, session.RunStart{RunID: "run-r204", Input: &protocol.Message{Role: "user", Content: "long turn"}}); err != nil {
		t.Fatal(err)
	}
	baseStore := realSQLiteAgentStore{s: rawStore}
	cStore := &countingAgentStore{Store: baseStore}
	rLive := &run{}
	if err := rLive.load(ctx, baseStore, sid, nil, maxCtx); err != nil {
		t.Fatal(err)
	}
	all := []HistoryGroupMeta{{Seq: rLive.transcript[0].Seq, LastSeq: rLive.transcript[0].Seq, Role: "user", AnsweredSeq: -1, MutationEvidence: MutationNone}}
	body := strings.Repeat("x", 3000)
	next := 2
	for _, target := range []int{2049, 6000, 12000} {
		seedStart := time.Now()
		seedCPU := zzCPU()
		seedR204RangeOnRealSQLite(t, ctx, baseStore, sid, rLive, &all, next, target, body, maxCtx)
		seedWall, seedCPUd := time.Since(seedStart), zzCPU()-seedCPU
		next = target + 1
		for rep := 0; rep < 3; rep++ {
			runtime.GC()
			var a, b runtime.MemStats
			runtime.ReadMemStats(&a)
			cStore.resetCounts()
			c0, w0 := zzCPU(), time.Now()
			var first RepairContext
			for i := 0; i < 40; i++ {
				rc, err := rLive.deriveRepairContext(ctx, cStore, sid)
				if err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					first = rc
				} else if rc != first {
					t.Fatal("non-deterministic")
				}
			}
			wall, cpu := time.Since(w0), zzCPU()-c0
			runtime.ReadMemStats(&b)
			t.Logf("groups=%d rep=%d 40x derive: wall=%.3fms cpu=%.3fms alloc=%dB mallocs=%d txScans=%d txRows=%d execQ=%d | seeding this range: wall=%.0fms cpu=%.0fms | inMemGroups=%d",
				target, rep, float64(wall.Microseconds())/1000, float64(cpu.Microseconds())/1000, b.TotalAlloc-a.TotalAlloc, b.Mallocs-a.Mallocs,
				cStore.transcriptScans.Load(), cStore.transcriptRows.Load(), cStore.execListQueries.Load(),
				float64(seedWall.Microseconds())/1000, float64(seedCPUd.Microseconds())/1000, len(rLive.historyGroups))
		}
		c0, w0 := zzCPU(), time.Now()
		rCold := &run{}
		cStore.resetCounts()
		if err := rCold.load(ctx, cStore, sid, nil, maxCtx); err != nil {
			t.Fatal(err)
		}
		if _, err := rCold.deriveRepairContext(ctx, cStore, sid); err != nil {
			t.Fatal(err)
		}
		t.Logf("groups=%d cold load+derive: wall=%.1fms cpu=%.1fms txScans=%d execQ=%d", target,
			float64(time.Since(w0).Microseconds())/1000, float64((zzCPU()-c0).Microseconds())/1000, cStore.transcriptScans.Load(), cStore.execListQueries.Load())
	}
}
