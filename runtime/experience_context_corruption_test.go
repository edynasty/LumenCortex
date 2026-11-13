package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/edynasty/LumenCortex/internal/session"
)

// F04 narrows thread-context reads, not the full graph diagnostic. Unrelated
// corrupt payloads are not decoded, but a selected Experience must still pass
// the normal typed validation. These reads must never repair or rewrite storage.
func TestExperienceContextCorruptionBoundary(t *testing.T) {
	for _, mode := range []string{"unrelated belief", "unadopted experience", "owned experience", "adopted experience"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f := newCognitionFixture(t, false)
			n, origin := expValidated(t, f, "context corruption boundary")
			reader := expNewSession(t, f, "context reader")
			selected := mode == "owned experience" || mode == "adopted experience"
			if mode == "owned experience" {
				reader = origin
			}
			if mode == "adopted experience" {
				if _, err := f.engine.AdoptExperience(ctx, reader, n.ID, n.Version); err != nil {
					t.Fatal(err)
				}
			}
			clean, err := f.engine.GetContextGraph(ctx, reader)
			if err != nil {
				t.Fatal(err)
			}
			if _, found := expContextNode(clean, "experience:"+n.ID); found != selected {
				t.Fatalf("fixture selection = %v, want %v", found, selected)
			}

			path := f.engine.store.Path()
			if mode == "unrelated belief" {
				scope := cognitiveMustScope(t, f.engine, origin, "project")
				cognitiveMustPut(t, f.engine, origin, CognitiveNode{ID: "unrelated-corrupt", Kind: "belief", Title: "unrelated", Scope: scope}, 0)
				cognitiveExecFault(t, path, `UPDATE cognition_nodes SET json='{"id":"unrelated-corrupt"}' WHERE id='unrelated-corrupt'`)
			} else {
				// Preserve selection keys while corrupting a required payload field.
				cognitiveExecFault(t, path, fmt.Sprintf(`UPDATE cognition_nodes SET json=json_set(json,'$.experience.definition.problemPattern','') WHERE id='%s'`, n.ID))
			}
			before := cognitiveStoreDump(t, path)
			graph, graphErr := f.engine.GetContextGraph(ctx, reader)
			thread, threadErr := f.engine.ReadThread(ctx, reader)
			if selected {
				if !errors.Is(graphErr, session.ErrCognitiveCorrupt) || !errors.Is(threadErr, session.ErrCognitiveCorrupt) {
					t.Fatalf("selected corrupt Experience: graph=%v thread=%v", graphErr, threadErr)
				}
				if !reflect.DeepEqual(graph, ContextGraphView{}) || !reflect.DeepEqual(thread, ThreadView{}) {
					t.Fatal("selected corrupt Experience returned a partial projection")
				}
			} else {
				if graphErr != nil || threadErr != nil {
					t.Fatalf("unrelated corruption blocked context: graph=%v thread=%v", graphErr, threadErr)
				}
				if _, found := expContextNode(graph, "experience:"+n.ID); found {
					t.Fatal("unadopted Experience leaked into context")
				}
				if thread.Evidence == nil {
					t.Fatal("thread lost its bounded evidence projection")
				}
			}
			if nodes, edges, err := f.engine.nativeGraph(ctx); !errors.Is(err, session.ErrCognitiveCorrupt) || nodes != nil || edges != nil {
				t.Fatalf("full graph must still reject corruption: nodes=%d edges=%d err=%v", len(nodes), len(edges), err)
			}
			if after := cognitiveStoreDump(t, path); after != before {
				t.Fatal("context or diagnostic read changed stored data")
			}
		})
	}
}
