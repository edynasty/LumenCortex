package runtime

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/edynasty/LumenCortex/internal/evaluator"
	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/taskstate"
)

// Public values are native contracts. The host proposes lifecycle changes; only
// framework-run checks may attach evidence. Model confidence never grants authority.
type CognitiveScope = knowledge.Scope
type CognitiveReader = knowledge.Reader
type CognitiveTimes = knowledge.Times
type CognitiveValidTime = knowledge.ValidTime
type CognitiveAssertion = knowledge.Assertion
type CognitiveNode = knowledge.Node
type CognitiveEdge = knowledge.Edge
type CognitiveEvidence = knowledge.Evidence
type CognitiveNodeRef = knowledge.NodeRef
type CognitiveMemoryPolicy = knowledge.MemoryPolicy

// CognitiveScopeFor derives ownership from the existing Engine binding and the session's actual
// workspace. Global scopes are deliberately unsupported; a local store is not a global authority.
func (e *Engine) CognitiveScopeFor(ctx context.Context, sessionID, kind string) (CognitiveScope, error) {
	reader, err := e.cognitiveReader(ctx, sessionID)
	if err != nil {
		return CognitiveScope{}, err
	}
	s := CognitiveScope{Kind: knowledge.ScopeKind(kind), OwnerID: reader.OwnerID, ProjectID: reader.ProjectID}
	switch s.Kind {
	case knowledge.ScopeProject:
	case knowledge.ScopeWorkspace:
		s.WorkspaceID = reader.WorkspaceID
	case knowledge.ScopeSession:
		s.WorkspaceID, s.SessionID = reader.WorkspaceID, sessionID
	case knowledge.ScopeReader:
		s.WorkspaceID, s.SessionID, s.ReaderID = reader.WorkspaceID, sessionID, reader.ID
	case knowledge.ScopeSource:
		s.WorkspaceID, s.SessionID, s.SourceID = reader.WorkspaceID, sessionID, sessionID
	default:
		return s, errors.New("global or unknown scope has no native store authority")
	}
	return s, s.Validate()
}

func (e *Engine) cognitiveReader(ctx context.Context, id string) (knowledge.Reader, error) {
	workspace := e.workspace
	runID := ""
	if id != "" {
		if _, err := e.store.Get(ctx, id); err != nil {
			return knowledge.Reader{}, err
		}
		var err error
		workspace, err = e.agentWorkspace(ctx, id)
		if err != nil {
			return knowledge.Reader{}, err
		}
		if ref, err := e.store.CognitiveSource(ctx, id); err == nil {
			runID = ref.RunID
		}
	}
	return knowledge.Reader{OwnerID: knowledge.Digest("cognition-owner", e.repoDir), ProjectID: knowledge.Digest("cognition-project", e.projectID()), WorkspaceID: knowledge.Digest("cognition-workspace", workspace), SessionID: id, ID: runID, SourceIDs: []string{id}}, nil
}

// PutCognitiveNode creates or revises a host proposal; verified evidence is added only by
// VerifyCognitiveNode. Supplying Evidence/VerifiedAt is refused rather than trusted or silently stripped.
func (e *Engine) PutCognitiveNode(ctx context.Context, sessionID string, proposed CognitiveNode, expected int64) (CognitiveNode, error) {
	lock, err := e.acquireRun(ctx, sessionID)
	if err != nil {
		return CognitiveNode{}, err
	}
	defer lock.Release()
	if proposed.Kind == "experience" || proposed.Experience != nil {
		return CognitiveNode{}, errors.New("experience records use the Experimental Experience API")
	}
	if len(proposed.Evidence) > 0 || !proposed.Times.VerifiedAt.IsZero() {
		return CognitiveNode{}, errors.New("only framework verification can attach evidence")
	}
	reader, err := e.cognitiveReader(ctx, sessionID)
	if err != nil {
		return CognitiveNode{}, err
	}
	if !proposed.Scope.VisibleTo(reader) {
		return CognitiveNode{}, session.ErrFenced
	}
	proposed.SessionID = sessionID
	proposed.Version = expected + 1
	proposed.Times.RecordedAt = e.now()
	if proposed.RevisionReason == "" {
		proposed.RevisionReason = "host_proposal"
	}
	if proposed.Epistemic == "fact" {
		return CognitiveNode{}, errors.New("only framework verification can establish a fact")
	}
	if proposed.ID == "" {
		proposed.ID, err = newCandidateID()
		if err != nil {
			return CognitiveNode{}, err
		}
	}
	if proposed.Status == "" {
		proposed.Status = "active"
	}
	if proposed.Status == "invalid" && proposed.Times.InvalidatedAt.IsZero() {
		proposed.Times.InvalidatedAt = proposed.Times.RecordedAt
	}
	for _, ref := range proposed.Dependencies {
		dep, err := e.store.CognitiveNode(ctx, ref.ID, ref.Version, reader)
		if err != nil {
			return CognitiveNode{}, err
		}
		if !dep.Scope.Contains(proposed.Scope) {
			return CognitiveNode{}, session.ErrFenced
		}
		source, err := e.store.CognitiveSource(ctx, dep.SessionID)
		if err != nil {
			return CognitiveNode{}, err
		}
		for _, src := range append([]knowledge.SourceRef{source}, dep.Sources...) {
			if !slices.Contains(proposed.Sources, src) {
				proposed.Sources = append(proposed.Sources, src)
			}
		}
	}
	for i, src := range proposed.Sources {
		actual, err := e.store.CognitiveSource(ctx, src.SessionID)
		if err != nil {
			return CognitiveNode{}, err
		}
		if src != actual {
			return CognitiveNode{}, session.ErrSourceChanged
		}
		proposed.Sources[i] = actual
	}
	if err := e.store.PutCognitiveNode(ctx, proposed, expected, e.projectID()); err != nil {
		return CognitiveNode{}, err
	}
	return proposed, nil
}

func (e *Engine) ReadCognitiveNode(ctx context.Context, sessionID, id string, version int64) (CognitiveNode, error) {
	n, err := e.readCognitiveNode(ctx, sessionID, id, version)
	return experienceForReader(n, sessionID), err
}

// Internal transitions need the complete immutable record, not the public feedback projection.
func (e *Engine) readCognitiveNode(ctx context.Context, sessionID, id string, version int64) (CognitiveNode, error) {
	r, err := e.cognitiveReader(ctx, sessionID)
	if err != nil {
		return CognitiveNode{}, err
	}
	return e.store.CognitiveNode(ctx, id, version, r)
}

func (e *Engine) PutCognitiveEdge(ctx context.Context, sessionID string, edge CognitiveEdge) error {
	lock, err := e.acquireRun(ctx, sessionID)
	if err != nil {
		return err
	}
	defer lock.Release()
	r, err := e.cognitiveReader(ctx, sessionID)
	if err != nil {
		return err
	}
	edge.SessionID = sessionID
	return e.store.PutCognitiveEdge(ctx, edge, r)
}

// VerifyCognitiveNode runs the current host-bound check in the record owner's workspace. Evidence
// promotion is exact-assertion only. A failed closed episode is retained as contradicting evidence.
func (e *Engine) VerifyCognitiveNode(ctx context.Context, sessionID, id string, expected int64) (CognitiveNode, error) {
	lock, err := e.acquireRun(ctx, sessionID)
	if err != nil {
		return CognitiveNode{}, err
	}
	defer lock.Release()
	n, err := e.ReadCognitiveNode(ctx, sessionID, id, 0)
	if err != nil {
		return CognitiveNode{}, err
	}
	if n.SessionID != sessionID || n.Version != expected || n.Assertion == nil {
		return CognitiveNode{}, session.ErrCognitiveRevision
	}
	source, err := e.store.CognitiveSource(ctx, sessionID)
	if err != nil {
		return CognitiveNode{}, err
	}
	ev, err := e.verifyAssertion(ctx, sessionID, *n.Assertion, n.Scope, source)
	if err != nil {
		return CognitiveNode{}, err
	}
	n.Version++
	n.Times.RecordedAt = e.now()
	n.Evidence = []knowledge.Evidence{ev}
	n.RevisionReason = "framework_verification"
	if !slices.Contains(n.Sources, source) {
		n.Sources = append(n.Sources, source)
	}
	if ev.Verification == "passed" {
		n.Times.VerifiedAt = ev.Times.VerifiedAt
		n.Epistemic = "fact"
	} else {
		n.Status = "stale"
		if n.Times.InvalidatedAt.IsZero() {
			n.Times.InvalidatedAt = n.Times.RecordedAt
		}
	}
	if err := e.store.PutCognitiveNode(ctx, n, expected, e.projectID()); err != nil {
		return CognitiveNode{}, err
	}
	return n, nil
}

func (e *Engine) verifyAssertion(ctx context.Context, sessionID string, a knowledge.Assertion, scope knowledge.Scope, source knowledge.SourceRef) (knowledge.Evidence, error) {
	specs, _, _, err := e.approvedChecks(ctx)
	if err != nil {
		return knowledge.Evidence{}, err
	}
	bindings, err := e.store.ActiveConstraintBindings(ctx, e.projectID())
	if err != nil {
		return knowledge.Evidence{}, err
	}
	binding, spec, _ := matchBinding(bindings, specs, taskstate.ConstraintUse{Ref: a.Ref(), Digest: a.ValueDigest(), Scope: session.SourceScopeProject})
	if binding == nil {
		return knowledge.Evidence{}, session.ErrNotQualified
	}
	verdicts, err := e.decideForCognition(ctx, sessionID, specs, []string{binding.CheckID}, maintenanceVerifyBudget)
	if err != nil {
		return knowledge.Evidence{}, err
	}
	if len(verdicts) != 1 || verdicts[0].Result.Seq == 0 {
		return knowledge.Evidence{}, session.ErrNotQualified
	}
	return e.cognitiveCheckEvidence(ctx, sessionID, verdicts[0].Result.Seq, a, scope, source, *binding, spec)
}

func (e *Engine) cognitiveCheckEvidence(ctx context.Context, sessionID string, seq int64, a knowledge.Assertion, scope knowledge.Scope, source knowledge.SourceRef, binding session.ApprovalRecord, spec evaluator.Spec) (knowledge.Evidence, error) {
	q, err := e.store.QualifiedResult(ctx, sessionID, seq)
	if err != nil {
		return knowledge.Evidence{}, err
	}
	if q.SpecDigest != binding.SpecDigest || q.CheckID != binding.CheckID {
		return knowledge.Evidence{}, session.ErrNotQualified
	}
	now := e.now()
	ev := knowledge.Evidence{ID: fmt.Sprintf("%s:%d", sessionID, seq), Source: source, Type: "check", Provenance: knowledge.ProvenanceRuntime, Trust: "framework", Verification: q.Outcome, Scope: scope, Times: knowledge.Times{ObservedAt: now, RecordedAt: now, VerifiedAt: now}, Freshness: checkFreshness(spec), Fingerprint: q.Fingerprint, BindingDigest: binding.BindingDigest, SpecDigest: q.SpecDigest, ApprovalEpoch: q.ApprovalEpoch, SessionID: sessionID, ResultSeq: seq}
	if q.Outcome == evaluator.StatusPassed {
		ev.SupportingClaims = []string{a.ValueDigest()}
	} else {
		ev.ContradictingClaims = []string{a.ValueDigest()}
	}
	return ev, ev.Validate()
}

// NativeGraph is a host diagnostic snapshot, not a grant to execute from a historical process PASS.
func (e *Engine) nativeGraph(ctx context.Context) ([]knowledge.Node, []knowledge.Edge, error) {
	r, err := e.cognitiveReader(ctx, "")
	if err != nil {
		return nil, nil, err
	}
	return e.store.CognitiveGraph(ctx, r)
}
