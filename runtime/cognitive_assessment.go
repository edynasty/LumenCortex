package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/edynasty/LumenCortex/internal/evaluator"
	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/internal/threadctx"
)

// CognitiveAssessment describes evidence for exact assertions, not an executable
// permission or the model's hidden reasoning. Unknown and conflict stay explicit.
type CognitiveAssessment struct {
	Items           []CognitiveAssessmentItem `json:"items"`
	Recommendation  string                    `json:"recommendation"`
	RequestedAction string                    `json:"requestedAction,omitempty"`
	SourceSessions  []string                  `json:"sourceSessions"`
	Incomplete      bool                      `json:"incomplete"`
}
type CognitiveAssessmentItem struct {
	ID            string                  `json:"id"`
	Version       int64                   `json:"version"`
	Kind          string                  `json:"kind"`
	Assertion     *CognitiveAssertion     `json:"assertion,omitempty"`
	Applicability knowledge.Applicability `json:"applicability"`
	Reason        string                  `json:"reason"`
	Evidence      []string                `json:"evidence,omitempty"`
}

// AssessCognition is a read-only host view. Model-origin decisions use the same
// predicate, with a separate exact-revision receipt in the session store.
func (e *Engine) AssessCognition(ctx context.Context, readerID, query string, ids []string) (CognitiveAssessment, error) {
	result, _, err := e.assessCognitiveRecords(ctx, readerID, "", query, ids)
	return result, err
}

func (e *Engine) assessCognitiveRecords(ctx context.Context, readerID, runID, query string, ids []string) (CognitiveAssessment, []knowledge.NodeRef, error) {
	result := CognitiveAssessment{Items: []CognitiveAssessmentItem{}, Recommendation: "gather_evidence", SourceSessions: []string{}}
	if len(ids) > 16 || len(query) > 2048 {
		return result, nil, errors.New("cognition assessment exceeds bound")
	}
	reader, err := e.cognitiveReader(ctx, readerID)
	if err != nil {
		return result, nil, err
	}
	workspace, err := e.agentWorkspace(ctx, readerID)
	if err != nil {
		return result, nil, err
	}
	ev := &evaluator.Evaluator{Store: e.store, Workspace: workspace, WorktreeID: "local"}
	scope, err := e.scopeFor(ctx, readerID, runID, workspace, ev)
	if err != nil {
		return result, nil, err
	}
	var selected []knowledge.Node
	if len(ids) == 0 {
		nodes, err := e.store.CognitiveNodesByKind(ctx, reader, "belief")
		if err != nil {
			return result, nil, err
		}
		tokens := threadctx.Tokens(query)
		for _, n := range nodes {
			if cognitiveRelevance(tokens, cognitiveNodeSearchText(n)) > 0 {
				selected = append(selected, n)
			}
		}
		if len(selected) > 16 {
			selected = selected[:16]
			result.Incomplete = true
		}
	} else {
		for _, id := range ids {
			if slices.ContainsFunc(selected, func(n knowledge.Node) bool { return n.ID == id }) {
				continue
			}
			n, err := e.store.CognitiveNode(ctx, id, 0, reader)
			if err != nil {
				return result, nil, err
			}
			selected = append(selected, n)
		}
	}
	// A selected conclusion brings its visible premises and all conflicting current
	// propositions into the assessment; the model cannot hide them by choosing IDs.
	// A premise that was removed, withdrawn or narrowed outside the reader's scope is
	// not disclosed here and does not abort the assessment: CognitiveDependenciesCurrent
	// below marks the dependent proposition stale (dependency_changed).
	for index := 0; index < len(selected); index++ {
		n := selected[index]
		for _, ref := range n.Dependencies {
			if slices.ContainsFunc(selected, func(other knowledge.Node) bool { return other.ID == ref.ID }) {
				continue
			}
			if len(selected) == 16 {
				result.Incomplete = true
				break
			}
			dep, err := e.store.CognitiveNode(ctx, ref.ID, 0, reader)
			if errors.Is(err, session.ErrFenced) || errors.Is(err, session.ErrNotFound) {
				continue
			}
			if err != nil {
				return result, nil, err
			}
			selected = append(selected, dep)
		}
		if n.Assertion != nil {
			peers, err := e.store.CognitiveClaimPeers(ctx, reader, *n.Assertion)
			if err != nil {
				return result, nil, err
			}
			for _, peer := range peers {
				if peer.Assertion == nil || !knowledge.ClaimsConflict(*n.Assertion, *peer.Assertion, n.Scope, peer.Scope, n.Times.Valid, peer.Times.Valid) {
					continue
				}
				if slices.ContainsFunc(selected, func(other knowledge.Node) bool { return other.ID == peer.ID }) {
					continue
				}
				if len(selected) == 16 {
					result.Incomplete = true
					break
				}
				selected = append(selected, peer)
			}
		}
	}
	refs := make([]knowledge.NodeRef, 0, len(selected))
	allCurrent := len(selected) > 0 && !result.Incomplete
	for _, n := range selected {
		item := CognitiveAssessmentItem{ID: n.ID, Version: n.Version, Kind: n.Epistemic, Assertion: n.Assertion, Applicability: knowledge.Unknown, Reason: "unverified_proposition"}
		if item.Kind == "" || item.Kind == "fact" {
			item.Kind = "hypothesis"
		}
		if n.Experience != nil {
			item.Kind = "experience"
			item.Reason = "requires_condition_check"
		}
		if n.Status != "active" {
			item.Applicability = knowledge.Stale
			item.Reason = "lifecycle_" + n.Status
		} else if reason := n.Times.TimeState(scope.now); reason != "" && !knowledge.TimePending(reason) {
			item.Applicability = knowledge.Stale
			item.Reason = reason
		} else {
			changed := false
			for _, src := range n.Sources {
				version, err := e.store.SourceVersion(ctx, src.SessionID)
				if err != nil {
					return result, nil, err
				}
				if version != src.Digest {
					changed = true
				}
			}
			if changed {
				item.Applicability = knowledge.Stale
				item.Reason = "source_version_changed"
			} else if err := e.store.CognitiveDependenciesCurrent(ctx, n, reader, scope.now); err != nil {
				if !errors.Is(err, session.ErrCognitiveRevision) && !errors.Is(err, session.ErrNotFound) && !errors.Is(err, session.ErrFenced) {
					return result, nil, err
				}
				item.Applicability = knowledge.Stale
				item.Reason = "dependency_changed"
			} else if reason != "" {
				// Not yet valid: time will lift the limitation. Unknown, not stale; nothing expired.
				item.Reason = reason
			} else if n.Assertion != nil {
				for index, evidence := range n.Evidence {
					binding, spec, _ := matchBinding(scope.bindings, scope.specList, taskstate.ConstraintUse{Ref: n.Assertion.Ref(), Digest: n.Assertion.ValueDigest(), Scope: session.SourceScopeProject})
					active := binding != nil && binding.BindingDigest == evidence.BindingDigest && evaluator.Approved(spec)
					if active {
						active, err = e.store.CognitiveEvidenceCurrent(ctx, e.projectID(), *n.Assertion, evidence)
						if err != nil {
							return result, nil, err
						}
					}
					version, err := e.store.SourceVersion(ctx, evidence.Source.SessionID)
					if err != nil {
						return result, nil, err
					}
					app, reason := evidence.Applicability(knowledge.EvidenceContext{Reader: reader, At: scope.now, SourceVersion: version, BindingActive: active, Fingerprint: evaluator.Fingerprint(scope.env, spec), RunID: runID, InRun: false})
					item.Evidence = append(item.Evidence, evidence.ID)
					if index == 0 || knowledge.EvidenceLevel(app) > knowledge.EvidenceLevel(item.Applicability) {
						item.Applicability, item.Reason = app, reason
					}
				}
				if item.Applicability == knowledge.Current {
					item.Kind = "fact"
				}
			}
		}
		if item.Applicability != knowledge.Current {
			allCurrent = false
		}
		result.Items = append(result.Items, item)
		refs = append(refs, knowledge.NodeRef{ID: n.ID, Version: n.Version})
		for _, src := range n.Sources {
			if src.SessionID != "" && !slices.Contains(result.SourceSessions, src.SessionID) {
				result.SourceSessions = append(result.SourceSessions, src.SessionID)
			}
		}
		if n.Experience != nil && n.Experience.State == knowledge.ExperienceNeedsRevision && !slices.Contains(result.SourceSessions, n.SessionID) {
			result.SourceSessions = append(result.SourceSessions, n.SessionID)
		}
		owner := n.SessionID
		if n.Experience != nil {
			owner = n.Experience.Origin.Source.SessionID
		}
		if owner != "" && !slices.Contains(result.SourceSessions, owner) {
			result.SourceSessions = append(result.SourceSessions, owner)
		}
	}
	levels := make([]int, len(result.Items))
	for i, item := range result.Items {
		levels[i] = knowledge.EvidenceLevel(item.Applicability)
	}
	for i, n := range selected {
		for j, other := range selected {
			if i == j || levels[i] == 0 || levels[j] < levels[i] || n.Assertion == nil || other.Assertion == nil {
				continue
			}
			if knowledge.ClaimsConflict(*n.Assertion, *other.Assertion, n.Scope, other.Scope, n.Times.Valid, other.Times.Valid) {
				result.Items[i].Applicability = knowledge.Conflict
				result.Items[i].Reason = "conflicting_scoped_propositions"
				if result.Items[i].Kind == "fact" {
					result.Items[i].Kind = "hypothesis"
				}
				allCurrent = false
			}
		}
	}
	if allCurrent {
		result.Recommendation = "continue"
	}
	return result, refs, nil
}

func (e *Engine) assessCognitiveDecision(ctx context.Context, id, runID, query string, ids []string, action string) (CognitiveAssessment, error) {
	if !slices.Contains([]string{"continue", "gather_evidence", "ask", "defer", "stop"}, action) {
		return CognitiveAssessment{}, errors.New("invalid cognitive decision")
	}
	result, refs, err := e.assessCognitiveRecords(ctx, id, runID, query, ids)
	if err != nil {
		return result, err
	}
	result.RequestedAction = action
	if action != "continue" {
		result.Recommendation = action
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return result, err
	}
	reader, err := e.cognitiveReader(ctx, id)
	if err != nil {
		return result, err
	}
	receipt, err := newCandidateID()
	if err != nil {
		return result, err
	}
	return result, e.store.CommitCognitiveDecision(ctx, receipt, runID, refs, string(raw), reader, e.now())
}

func cognitiveNodeSearchText(n knowledge.Node) string {
	text := n.Title + " " + n.Body
	if len(n.Tags) > 0 {
		text += " " + strings.Join(n.Tags, " ")
	}
	if n.Assertion != nil {
		text += " " + string(n.Assertion.Kind) + " " + n.Assertion.Target + " " + n.Assertion.Key + " " + n.Assertion.Value
	}
	return text
}

func cognitiveRelevance(query []string, text string) int {
	if len(query) == 0 {
		return 0
	}
	tokens := threadctx.Tokens(text)
	score := 0
	for _, token := range query {
		if slices.Contains(tokens, token) {
			score++
		}
	}
	return score
}
