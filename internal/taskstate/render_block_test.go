package taskstate

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/workflow"
)

func TestRenderBlockExposesOnlyRenderedItemsBeforeCutoff(t *testing.T) {
	mgr := NewWorkUnitManager(nil)
	completed := WorkCompleted
	cancelled := WorkCancelled
	for i := range 50 {
		u, err := mgr.Add(WorkUnit{ID: fmt.Sprintf("comp-%02d", i), Goal: fmt.Sprintf("completed-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := mgr.Update(u.ID, WorkUnitPatch{Status: &completed}); err != nil {
			t.Fatal(err)
		}
	}
	uOpen1, err := mgr.Add(WorkUnit{ID: "open-1", Goal: "open-unit-1"})
	if err != nil {
		t.Fatal(err)
	}
	uCancel, err := mgr.Add(WorkUnit{ID: "cancel-1", Goal: "cancelled-unit"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Update(uCancel.ID, WorkUnitPatch{Status: &cancelled}); err != nil {
		t.Fatal(err)
	}

	var notes []Note
	for i := range 20 {
		notes = append(notes, Note{
			ID:   fmt.Sprintf("n%02d", i),
			By:   ByAgent,
			Text: strings.Repeat("x", 220),
		})
	}

	var tasks []Task
	for i := range 100 {
		tasks = append(tasks, Task{
			ID:        fmt.Sprintf("t%03d", i),
			Objective: fmt.Sprintf("task objective %d", i),
		})
	}

	wf, err := workflow.New(&workflow.Definition{
		ID:    "wf-1",
		Title: "Test Workflow",
		Entry: "act",
		Actions: map[string]*workflow.Action{
			"act": {Title: "Act", Terminal: true},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	s := State{
		Brief: "Implement context space",
		References: []Reference{
			{ID: "r-inc", Kind: RefPath, Target: "a.go", State: RefIncluded},
			{ID: "r-sug", Kind: RefPath, Target: "b.go", State: RefSuggested},
		},
	}
	s.SetUnits(mgr)
	s.Notes = notes
	s.Tasks = tasks

	rendered := RenderBlock("goal", s, wf)
	if rendered.Text != Render("goal", s, wf) {
		t.Fatal("Render and RenderBlock.Text diverged")
	}
	if !slices.Contains(rendered.ItemRefs, "ts:brief") ||
		!slices.Contains(rendered.ItemRefs, "ts:ref:r-inc") ||
		!slices.Contains(rendered.ItemRefs, "ts:workflow") ||
		!slices.Contains(rendered.ItemRefs, "ts:unit:"+uOpen1.ID) {
		t.Fatalf("missing expected rendered refs: %v", rendered.ItemRefs)
	}
	if slices.Contains(rendered.ItemRefs, "ts:ref:r-sug") || slices.Contains(rendered.ItemRefs, "ts:unit:"+uCancel.ID) {
		t.Fatalf("included non-rendered ref or cancelled unit: %v", rendered.ItemRefs)
	}
	for _, ref := range rendered.ItemRefs {
		if strings.HasPrefix(ref, "ts:unit:") && ref != "ts:unit:"+uOpen1.ID {
			t.Fatalf("completed unit leaked into ItemRefs: %s", ref)
		}
		if strings.HasPrefix(ref, "ts:task:") {
			id := strings.TrimPrefix(ref, "ts:task:")
			if id < "t090" {
				t.Fatalf("older unrendered task leaked into ItemRefs: %s", ref)
			}
		}
	}
	// Oldest notes beyond notesRenderBytes must not appear in ItemRefs.
	if slices.Contains(rendered.ItemRefs, "ts:note:n00") {
		t.Fatalf("hidden note n00 leaked into ItemRefs: %v", rendered.ItemRefs)
	}

	// Force MaxBlockBytes (10240B) truncation and verify items starting past cutoff are excluded.
	hugeMgr := NewWorkUnitManager(nil)
	for i := range 40 {
		if _, err := hugeMgr.Add(WorkUnit{
			ID:   fmt.Sprintf("open-%02d", i),
			Goal: strings.Repeat("goal-", 80),
		}); err != nil {
			t.Fatal(err)
		}
	}
	sHuge := State{
		Brief: strings.Repeat("B", MaxBriefBytes),
		Tasks: []Task{{ID: "tail-task", Objective: "past 10 KiB"}},
	}
	sHuge.SetUnits(hugeMgr)
	rbHuge := RenderBlock("goal", sHuge, nil)
	if len(rbHuge.Text) > MaxBlockBytes {
		t.Fatalf("RenderBlock exceeded MaxBlockBytes: %d", len(rbHuge.Text))
	}
	if slices.Contains(rbHuge.ItemRefs, "ts:task:tail-task") {
		t.Fatalf("truncated tail task leaked into ItemRefs: %v", rbHuge.ItemRefs)
	}
}
