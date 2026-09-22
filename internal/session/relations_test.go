package session

import (
	"context"
	"encoding/json"
	"testing"
)

func TestRelationsAndCheckpointsPersistAndCascade(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	if err := store.Create(ctx, Session{ID: "parent", Goal: "parent", Status: "created"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, Session{ID: "child", Goal: "child", Status: "created"}); err != nil {
		t.Fatal(err)
	}

	if err := store.AddRelation(ctx, Relation{
		ParentSessionID: "parent",
		ChildSessionID:  "child",
		Kind:            "subagent",
		Metadata:        json.RawMessage(`{"role":"research"}`),
	}); err != nil {
		t.Fatal(err)
	}

	parent, err := store.ParentRelation(ctx, "child")
	if err != nil {
		t.Fatal(err)
	}
	if parent.ParentSessionID != "parent" || parent.ChildSessionID != "child" || parent.Kind != "subagent" {
		t.Fatalf("relation=%#v", parent)
	}

	children, err := store.ChildRelations(ctx, "parent", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].ChildSessionID != "child" {
		t.Fatalf("children=%#v", children)
	}
	count, err := store.CountChildren(ctx, "parent", "subagent")
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("count=%d", count)
	}

	first, err := store.AppendCheckpoint(ctx, "parent", "before-subagent", map[string]any{"step": 1})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AppendCheckpoint(ctx, "parent", "after-subagent", map[string]any{"step": 2})
	if err != nil {
		t.Fatal(err)
	}
	if first.Seq != 0 || second.Seq != 1 {
		t.Fatalf("checkpoint seq=%d,%d", first.Seq, second.Seq)
	}
	checkpoints, err := store.Checkpoints(ctx, "parent", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoints) != 2 || checkpoints[0].Seq != 1 || checkpoints[1].Seq != 0 {
		t.Fatalf("checkpoints=%#v", checkpoints)
	}

	if _, err := store.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, "parent"); err != nil {
		t.Fatal(err)
	}
	var relations int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_relations`).Scan(&relations); err != nil {
		t.Fatal(err)
	}
	if relations != 0 {
		t.Fatalf("relations after parent delete=%d", relations)
	}
	var checkpointCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_checkpoints WHERE session_id = ?`, "parent").Scan(&checkpointCount); err != nil {
		t.Fatal(err)
	}
	if checkpointCount != 0 {
		t.Fatalf("checkpoints after parent delete=%d", checkpointCount)
	}
}

func TestRelationRejectsSelfReference(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Create(context.Background(), Session{ID: "same", Goal: "same", Status: "created"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddRelation(context.Background(), Relation{
		ParentSessionID: "same",
		ChildSessionID:  "same",
		Kind:            "subagent",
	}); err == nil {
		t.Fatal("expected self relation rejection")
	}
}
