package shell

import (
	"context"
	"strings"
	"testing"
)

func TestLargeOutputIsBounded(t *testing.T) {
	r := New(t.TempDir())
	// About 2 MiB without depending on Python/Perl.
	cmd := `i=0; while [ $i -lt 32768 ]; do printf '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef'; i=$((i+1)); done`
	result, err := r.Run(context.Background(), cmd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit=%d", result.ExitCode)
	}
	if result.Stdout.Total < 2<<20 {
		t.Fatalf("expected >=2MiB output, got %d", result.Stdout.Total)
	}
	if len(result.Stdout.Head) > r.HeadBytes || len(result.Stdout.Tail) > r.TailBytes {
		t.Fatalf("retained output exceeded limits: head=%d tail=%d", len(result.Stdout.Head), len(result.Stdout.Tail))
	}
	if !result.Stdout.Truncated {
		t.Fatal("expected bounded/truncated stdout")
	}
	if strings.Contains(string(result.Stdout.Head), "unexpected") {
		t.Fatal("sanity check")
	}
}
