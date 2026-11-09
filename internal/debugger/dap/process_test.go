package dap

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestAdapterProcessDrainsBoundedStderr(t *testing.T) {
	if os.Getenv("LC_DAP_STDERR_CHILD") == "1" {
		os.Stderr.WriteString(strings.Repeat("x", 2<<20))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAdapterProcessDrainsBoundedStderr$")
	cmd.Env = append(os.Environ(), "LC_DAP_STDERR_CHILD=1")
	p, err := Start(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	select {
	case <-p.Done():
	case <-ctx.Done():
		t.Fatal("stderr stalled adapter")
	}
	got := p.Stderr.Snapshot()
	if got.Total != 2<<20 || !got.Truncated || len(got.Head)+len(got.Tail) > 8192 {
		t.Fatalf("unbounded/lost stderr %+v", got)
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
}
