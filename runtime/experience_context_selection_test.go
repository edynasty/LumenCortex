package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/edynasty/LumenCortex/internal/session"
)

// Ownership comes from committed provenance, not a payload whose corruption is
// precisely what the read must detect. Unrelated malformed JSON must stay local.
func TestExperienceContextCorruptSelection(t *testing.T) {
	for _, mode := range []string{"unadopted", "owned", "adopted"} {
		for _, fault := range []string{"malformed_json", "missing_ownership"} {
			t.Run(mode+"/"+fault, func(t *testing.T) {
				ctx := context.Background()
				f := newCognitionFixture(t, false)
				n, origin := expValidated(t, f, "corrupt selection")
				reader := expNewSession(t, f, "context consumer")
				if mode == "owned" {
					reader = origin
				}
				if mode == "adopted" {
					if _, err := f.engine.AdoptExperience(ctx, reader, n.ID, n.Version); err != nil {
						t.Fatal(err)
					}
				}
				path := f.engine.store.Path()
				mutation := `json='{'`
				if fault == "missing_ownership" {
					mutation = `json=json_remove(json,'$.experience.origin','$.experience.uses')`
				}
				cognitiveExecFault(t, path, fmt.Sprintf(`PRAGMA ignore_check_constraints=ON; UPDATE cognition_nodes SET %s WHERE id='%s'; PRAGMA ignore_check_constraints=OFF`, mutation, n.ID))
				before := cognitiveStoreDump(t, path)
				graph, graphErr := f.engine.GetContextGraph(ctx, reader)
				thread, threadErr := f.engine.ReadThread(ctx, reader)
				if mode == "unadopted" {
					if graphErr != nil || threadErr != nil {
						t.Fatalf("unrelated malformed experience blocked context: graph=%v thread=%v", graphErr, threadErr)
					}
					if _, found := expContextNode(graph, "experience:"+n.ID); found {
						t.Fatal("unadopted experience leaked into context")
					}
				} else {
					if !errors.Is(graphErr, session.ErrCognitiveCorrupt) || !errors.Is(threadErr, session.ErrCognitiveCorrupt) {
						t.Fatalf("selected corrupt experience was hidden or misclassified: graph=%v thread=%v", graphErr, threadErr)
					}
					if !reflect.DeepEqual(graph, ContextGraphView{}) || !reflect.DeepEqual(thread, ThreadView{}) {
						t.Fatal("selected corrupt experience returned a partial projection")
					}
				}
				if nodes, edges, err := f.engine.nativeGraph(ctx); !errors.Is(err, session.ErrCognitiveCorrupt) || nodes != nil || edges != nil {
					t.Fatalf("full diagnostic lost corruption: nodes=%d edges=%d err=%v", len(nodes), len(edges), err)
				}
				if cognitiveStoreDump(t, path) != before {
					t.Fatal("corruption read changed storage")
				}
			})
		}
	}
}
