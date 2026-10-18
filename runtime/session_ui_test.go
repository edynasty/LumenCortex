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
			"workflow":     map[string]any{"status": "running"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	title := "Readable title"
	pinned := true
	archived := true
	info, err := engine.UpdateSessionUI(context.Background(), handle.ID, SessionUIPatch{
		Title:    &title,
		Pinned:   &pinned,
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
		Title:    &empty,
		Pinned:   &pinned,
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

// The thread's delegation grant (ADR-0006 D4) is stored in the ui key as the user set it: none is the
// default and is not stored, and an unknown value is refused before anything is written.
func TestUpdateSessionUIStoresTheDelegationGrant(t *testing.T) {
	engine, err := Open(Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	ctx := context.Background()
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "g"})
	if err != nil {
		t.Fatal(err)
	}
	ui := func(info SessionInfo) map[string]any {
		var metadata map[string]any
		if err := json.Unmarshal(info.Metadata, &metadata); err != nil {
			t.Fatal(err)
		}
		out, _ := metadata["ui"].(map[string]any)
		return out
	}
	grant := func(value string) (SessionInfo, error) {
		return engine.UpdateSessionUI(ctx, handle.ID, SessionUIPatch{Delegation: &value})
	}

	info, err := grant("propose")
	if err != nil || ui(info)["delegation"] != "propose" {
		t.Fatalf("propose: ui=%v err=%v", ui(info), err)
	}
	title := "kept"
	if info, err = engine.UpdateSessionUI(ctx, handle.ID, SessionUIPatch{Title: &title}); err != nil || ui(info)["delegation"] != "propose" {
		t.Fatalf("a title patch dropped the grant: ui=%v err=%v", ui(info), err)
	}
	if _, err := grant("full"); err == nil {
		t.Fatal("an unknown grant was accepted")
	}
	if _, info, err = engine.Session(ctx, handle.ID); err != nil || ui(info)["delegation"] != "propose" {
		t.Fatalf("a refused grant changed the stored one: ui=%v err=%v", ui(info), err)
	}
	if info, err = grant("none"); err != nil {
		t.Fatal(err)
	}
	if _, ok := ui(info)["delegation"]; ok || ui(info)["title"] != "kept" {
		t.Fatalf("none is stored: ui=%v", ui(info))
	}
}
