package editing

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewByteEvictionKeepsNewestViewFence(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a", "R1")
	e := newTestEngine(t, root)
	actor := testActor()
	e.RegisterRead(actor, filepath.Join(root, "a"), RevOf([]byte("R1")))
	big := func(tag string) []byte { return []byte(tag + strings.Repeat("x", 3<<20)) }
	r2 := big("R2")
	if _, err := e.Commit(context.Background(), Plan{Actor: actor, Changes: []FileChange{{Path: "a", Content: &r2, RejectStaleRead: true}}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		e.RegisterAnchoredRead(actor, filepath.Join(root, fmt.Sprintf("o%d", i)), DecodeDoc(big(fmt.Sprint(i))), 1, 1)
	}
	writeFile(t, root, "a", "R1")
	stale := []byte("computed from R2")
	if _, err := e.Commit(context.Background(), Plan{Actor: actor, Changes: []FileChange{{Path: "a", Content: &stale, RejectStaleRead: true}}}); err == nil {
		t.Error("stale write accepted after newest view evicted")
	}
	if got := readFileContent(t, root, "a"); got != "R1" {
		t.Fatalf("reverted content lost: %q", got)
	}
}
