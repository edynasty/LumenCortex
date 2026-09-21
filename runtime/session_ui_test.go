package runtime

import (
	"context"
	"encoding/json"
	"testing"
)

func TestUpdateSessionUIPreservesOtherMetadata(t *testing.T) {
	engine, err := Open(Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	handle, err := engine.NewSession(context.Background(), SessionOptions{
		Goal: "original goal",
		Metadata: map[string]any{
			"contextPaths": []string{"src/main.go"},
			"workflow": map[string]any{"status": "running"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	title := "Readable title"
	pinned := true
	archived := true
	info, err := engine.UpdateSessionUI(context.Background(), handle.ID, SessionUIPatch{
		Title: &title,
		Pinned: &pinned,
		Archived: &archived,
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.Goal != "original goal" {
		t.Fatalf("goal=%q", info.Goal)
	}

	var metadata map[string]any
	if err := json.Unmarshal(info.Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	if _, ok := metadata["contextPaths"]; !ok {
		t.Fatalf("contextPaths missing: %#v", metadata)
	}
	if _, ok := metadata["workflow"]; !ok {
		t.Fatalf("workflow missing: %#v", metadata)
	}
	rawUI, ok := metadata["ui"].(map[string]any)
	if !ok {
		t.Fatalf("ui metadata=%#v", metadata["ui"])
	}
	if rawUI["title"] != "Readable title" || rawUI["pinned"] != true || rawUI["archived"] != true {
		t.Fatalf("ui=%#v", rawUI)
	}

	empty := ""
	pinned = false
	archived = false
	info, err = engine.UpdateSessionUI(context.Background(), handle.ID, SessionUIPatch{
		Title: &empty,
		Pinned: &pinned,
		Archived: &archived,
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata = map[string]any{}
	if err := json.Unmarshal(info.Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	if _, ok := metadata["ui"]; ok {
		t.Fatalf("empty ui metadata should be removed: %#v", metadata)
	}
}
