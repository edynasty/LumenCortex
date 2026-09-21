package streambuf

import (
	"bytes"
	"testing"
)

func TestHeadTailRetainsBoundedMemory(t *testing.T) {
	b := New(8*1024, 24*1024)
	chunk := bytes.Repeat([]byte("x"), 64*1024)
	const writes = 256 // 16 MiB total
	for i := 0; i < writes; i++ {
		if _, err := b.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	s := b.Snapshot()
	if got, want := s.Total, int64(len(chunk)*writes); got != want {
		t.Fatalf("total=%d want=%d", got, want)
	}
	if len(s.Head) > 8*1024 || len(s.Tail) > 24*1024 {
		t.Fatalf("retained output exceeded bound: head=%d tail=%d", len(s.Head), len(s.Tail))
	}
	if !s.Truncated {
		t.Fatal("expected truncated snapshot")
	}
}
