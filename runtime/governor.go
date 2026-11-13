package runtime

import (
	"context"
	"errors"

	"github.com/edynasty/LumenCortex/internal/cognition"
	"github.com/edynasty/LumenCortex/internal/knowledge"
)

type GovernorPlanResult struct {
	Analysis     cognition.GovernorAnalysis   `json:"analysis"`
	Plan         cognition.GovernorPlan       `json:"plan"`
	Validation   cognition.GovernorValidation `json:"validation"`
	Curator      bool                         `json:"curator"`
	CuratorModel string                       `json:"curatorModel,omitempty"`
}

// Public aliases expose the already-existing read-only Governor contract to hosts outside Core.
type GovernorAnalyzeOptions = cognition.GovernorAnalyzeOptions
type GovernorAnalysis = cognition.GovernorAnalysis
type GovernorPlan = cognition.GovernorPlan
type GovernorValidation = cognition.GovernorValidation

// CognitiveGraphSnapshot reads a native Go graph through the Engine store. Scope/fence filtering
// happens before projection; historical evidence grades are not a current-use authorization.
func (e *Engine) CognitiveGraphSnapshot(ctx context.Context) (cognition.GraphState, map[string]cognition.StorageTelemetry, error) {
	if e == nil || e.store == nil {
		return cognition.GraphState{}, nil, errors.New("runtime store is not initialized")
	}
	nodes, edges, err := e.nativeGraph(ctx)
	if err != nil {
		return cognition.GraphState{}, nil, err
	}
	graph := cognition.GraphState{Version: 1, Nodes: make(map[string]cognition.GraphNode, len(nodes)), Edges: make(map[string]cognition.GraphEdge, len(edges)), Metadata: map[string]any{"authority": "go-native", "readOnly": true}}
	for _, n := range nodes {
		status, grade, trust := n.Status, "hypothesis", "model_inferred"
		if n.Kind == "entity" || n.Kind == "task" {
			grade, trust = "static", "user_provided"
		}
		limitation := n.Times.TimeState(e.now())
		for _, source := range n.Sources {
			version, err := e.store.SourceVersion(ctx, source.SessionID)
			if err != nil {
				return cognition.GraphState{}, nil, err
			}
			if source.Digest != version {
				limitation = "source_version_changed"
			}
		}
		// Not yet valid is pending, not stale: the stored lifecycle stands and the limitation says
		// why the record is not usable now, so the analyzer never proposes archiving it.
		pending := limitation == "not_yet_valid" || limitation == "future_observation"
		if limitation != "" && !pending && status != "invalid" && status != "archived" {
			status = "stale"
		}
		for _, evidence := range n.Evidence {
			if evidence.Verification == "passed" {
				grade, trust = "runtime", "runtime_verified"
			}
		}
		graph.Nodes[n.ID] = cognition.GraphNode{ID: n.ID, Kind: n.Kind, Title: n.Title, Body: n.Body, Tags: n.Tags, Status: status, Grade: grade, TrustZone: trust, Metadata: map[string]any{"version": n.Version, "scope": n.Scope, "times": n.Times, "evidence": n.Evidence, "useLimitation": limitation, "applicability": knowledge.Unknown}}
		node := graph.Nodes[n.ID]
		node.Metadata["protectedMemory"] = n.ProtectedMemory()
		node.Metadata["memoryClass"] = n.MemoryClass()
		node.Metadata["memory"] = n.Memory
		node.Metadata["dependencies"] = n.Dependencies
		graph.Nodes[n.ID] = node
	}
	for _, edge := range edges {
		graph.Edges[edge.ID] = cognition.GraphEdge{ID: edge.ID, From: edge.From, To: edge.To, Type: edge.Type, Weight: edge.Weight}
	}
	return graph, map[string]cognition.StorageTelemetry{}, nil
}

func (e *Engine) GovernorAnalyze(ctx context.Context, options cognition.GovernorAnalyzeOptions) (cognition.GovernorAnalysis, error) {
	graph, storage, err := e.CognitiveGraphSnapshot(ctx)
	if err != nil {
		return cognition.GovernorAnalysis{}, err
	}
	if options.StorageByNode == nil {
		options.StorageByNode = storage
	}
	return (cognition.GovernorAnalyzer{}).Analyze(graph, options), nil
}

// ErrGovernorCuratorDisabled is returned when a Governor plan is asked for with the model curator.
// A curator proposal is a model request, and every model request of a run has to be admitted to
// request_envelopes before it is sent (ADR-0018). The curator path has no admission, so it fails
// closed instead of sending an unrecorded request; the deterministic plan stays available.
var ErrGovernorCuratorDisabled = errors.New("the Governor's model curator is disabled: a curator request has no request envelope (ADR-0018); use the deterministic plan")

// GovernorPlan analyses the graph and returns a validated plan. With a curator it refuses before
// reading the graph or calling a provider: the deterministic plan is the only plan this runtime
// produces.
func (e *Engine) GovernorPlan(ctx context.Context, curator *cognition.LLMGovernorCurator, options cognition.GovernorAnalyzeOptions) (GovernorPlanResult, error) {
	if curator != nil {
		return GovernorPlanResult{}, ErrGovernorCuratorDisabled
	}
	graph, storage, err := e.CognitiveGraphSnapshot(ctx)
	if err != nil {
		return GovernorPlanResult{}, err
	}
	if options.StorageByNode == nil {
		options.StorageByNode = storage
	}
	analysis := (cognition.GovernorAnalyzer{}).Analyze(graph, options)
	plan := cognition.DeterministicGovernorPlan(analysis)
	validation := cognition.ValidateGovernorPlan(plan, graph)
	return GovernorPlanResult{Analysis: analysis, Plan: validation.Normalized, Validation: validation}, nil
}

func (e *Engine) GovernorValidate(ctx context.Context, plan cognition.GovernorPlan) (cognition.GovernorValidation, error) {
	graph, _, err := e.CognitiveGraphSnapshot(ctx)
	if err != nil {
		return cognition.GovernorValidation{}, err
	}
	return cognition.ValidateGovernorPlan(plan, graph), nil
}
