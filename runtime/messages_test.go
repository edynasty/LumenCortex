package runtime

import (
	"context"
	"fmt"
	"testing"
)

func TestMessagePageLoadsNewestThenOlderWithoutOverlap(t *testing.T) {
	engine, err := Open(Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	handle, err := engine.NewSession(context.Background(), SessionOptions{Goal: "paging"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 250; i++ {
		if _, err := handle.AppendMessage(context.Background(), "user", map[string]string{
			"content": fmt.Sprintf("message-%03d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	first, err := engine.MessagePage(context.Background(), handle.ID, -1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Messages) != 100 || !first.HasMore {
		t.Fatalf("first=%#v", first)
	}
	if first.Messages[0].Seq != 150 || first.Messages[99].Seq != 249 {
		t.Fatalf("first seq range=%d..%d", first.Messages[0].Seq, first.Messages[99].Seq)
	}

	second, err := engine.MessagePage(context.Background(), handle.ID, first.NextBefore, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Messages) != 100 || !second.HasMore {
		t.Fatalf("second=%#v", second)
	}
	if second.Messages[0].Seq != 50 || second.Messages[99].Seq != 149 {
		t.Fatalf("second seq range=%d..%d", second.Messages[0].Seq, second.Messages[99].Seq)
	}

	third, err := engine.MessagePage(context.Background(), handle.ID, second.NextBefore, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Messages) != 50 || third.HasMore {
		t.Fatalf("third=%#v", third)
	}
	if third.Messages[0].Seq != 0 || third.Messages[49].Seq != 49 {
		t.Fatalf("third seq range=%d..%d", third.Messages[0].Seq, third.Messages[49].Seq)
	}
}
