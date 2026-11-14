package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/threadctx"
)

// Experimental Experience is a host-invoked pipeline, not an autonomous model memory service.
// Proposals are untrusted; the framework owns origin, checks, applicability and every transition.
type ExperienceProposal = knowledge.ExperienceProposal
type ExperienceRecord = knowledge.Experience
type ExperienceObservation = knowledge.ExperienceObservation
type ExperienceStatistics = knowledge.ExperienceStatistics
type ExperienceExtraction struct {
	ScopeKind string             `json:"scopeKind"`
	Proposal  ExperienceProposal `json:"proposal"`
	Valid     CognitiveValidTime `json:"valid"`
}
type ExperienceMatch struct {
	ID                string `json:"id"`
	Version           int64  `json:"version"`
	DefinitionVersion int64  `json:"definitionVersion"`
	ProblemPattern    string `json:"problemPattern"`
	State             string `json:"state"`
	Applicability     string `json:"applicability"`
	Confidence        string `json:"confidence"`
}
type ExperienceAdoption struct {
	ID                string                  `json:"id"`
	ExperienceID      string                  `json:"experienceId"`
	Version           int64                   `json:"version"`
	DefinitionVersion int64                   `json:"definitionVersion"`
	Strategy          []string                `json:"strategy"`
	Conditions        []ExperienceObservation `json:"conditions"`
	ExpectedOutcome   CognitiveAssertion      `json:"expectedOutcome"`
	ExpectedResult    string                  `json:"expectedResult"`
	Uncertainty       string                  `json:"uncertainty"`
}

var ErrExperienceNotApplicable = errors.New("experience is not currently applicable")

// ParseExperienceProposal is the strict extractor boundary; unknown authority/reasoning fields fail.
func ParseExperienceProposal(raw string) (ExperienceProposal, error) {
	return knowledge.ParseExperienceProposal(raw)
}

// ExtractExperience records a candidate derived from one real completed/failed run. Proposal text
// is not a final-answer summary and never becomes evidence. Repeated identical extraction is a read.
func (e *Engine) ExtractExperience(ctx context.Context, originID string, request ExperienceExtraction) (CognitiveNode, error) {
	if err := request.Proposal.Validate(); err != nil {
		return CognitiveNode{}, err
	}
	if err := request.Valid.Validate(); err != nil {
		return CognitiveNode{}, err
	}
	lock, err := e.acquireRun(ctx, originID)
	if err != nil {
		return CognitiveNode{}, err
	}
	defer lock.Release()
	return e.extractExperience(ctx, originID, request)
}

func (e *Engine) extractExperience(ctx context.Context, originID string, request ExperienceExtraction) (CognitiveNode, error) {
	if err := request.Proposal.Validate(); err != nil {
		return CognitiveNode{}, err
	}
	if err := request.Valid.Validate(); err != nil {
		return CognitiveNode{}, err
	}
	scope, err := e.CognitiveScopeFor(ctx, originID, request.ScopeKind)
	if err != nil {
		return CognitiveNode{}, err
	}
	reader, err := e.cognitiveReader(ctx, originID)
	if err != nil {
		return CognitiveNode{}, err
	}
	origin, err := e.store.ExperienceOrigin(ctx, originID)
	if err != nil {
		return CognitiveNode{}, err
	}
	files, err := e.experienceFiles(ctx, originID, request.Proposal.CitedPaths)
	if err != nil {
		return CognitiveNode{}, err
	}
	origin.Trace.Files = files
	identity, err := json.Marshal(struct {
		Source     knowledge.SourceRef
		Scope      CognitiveScope
		Definition string
		Valid      CognitiveValidTime
	}{origin.Source, scope, request.Proposal.Digest(), request.Valid})
	if err != nil {
		return CognitiveNode{}, err
	}
	id := "experience_" + strings.TrimPrefix(knowledge.Digest("experience-extraction:v1", string(identity)), "sha256:")
	if existing, err := e.store.CognitiveNode(ctx, id, 0, reader); err == nil {
		return experienceForReader(existing, originID), nil
	} else if !errors.Is(err, session.ErrNotFound) {
		return CognitiveNode{}, err
	}
	now := e.now()
	x := &knowledge.Experience{Origin: origin, Definition: request.Proposal, DefinitionVersion: 1, State: knowledge.ExperienceCandidate, Confidence: "unverified", Uncertainty: knowledge.ExperienceUncertainty, CreatedAt: now}
	n := CognitiveNode{ID: id, Version: 1, SessionID: session.ExperienceOwner(id), Kind: "experience", Title: request.Proposal.ProblemPattern, Status: "active", Scope: scope, Times: CognitiveTimes{ObservedAt: origin.CompletedAt, RecordedAt: now, Valid: request.Valid}, Sources: []knowledge.SourceRef{origin.Source}, Experience: x}
	if err := e.store.CommitExperience(ctx, n, 0, e.projectID(), reader); err != nil {
		return CognitiveNode{}, err
	}
	return n, nil
}

func (e *Engine) experienceFiles(ctx context.Context, sessionID string, paths []string) ([]knowledge.ExperienceFile, error) {
	workspace, err := e.agentWorkspace(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	repo, err := repository.New(workspace)
	if err != nil {
		return nil, err
	}
	blobs, err := repo.FileBlobs(ctx, paths)
	if err != nil {
		return nil, err
	}
	files := make([]knowledge.ExperienceFile, 0, len(paths))
	for _, path := range paths {
		if blobs[path] == "" {
			return nil, fmt.Errorf("%w: experience citation is not a bounded workspace file", session.ErrSourceChanged)
		}
		files = append(files, knowledge.ExperienceFile{Path: path, Blob: blobs[path]})
	}
	return files, nil
}

// ReadExperience reads exact history subject to today's scope and source/consumer revocation.
// Uses contains only this reader's private observations, including for historical versions.
// The shared state is historical, not permission to use its strategy without AdoptExperience.
func (e *Engine) ReadExperience(ctx context.Context, readerID, id string, version int64) (CognitiveNode, error) {
	n, err := e.readExperience(ctx, readerID, id, version)
	return experienceForReader(n, readerID), err
}

// The Store returns a newly decoded node. Compact its owned slice without copying the record or
// leaving another consumer's details in the returned slice's spare capacity. Never persist this view.
func experienceForReader(n CognitiveNode, readerID string) CognitiveNode {
	if n.Experience != nil {
		n.Experience.Uses = slices.DeleteFunc(n.Experience.Uses, func(u knowledge.ExperienceUse) bool {
			return u.ConsumerID != readerID
		})
	}
	return n
}

func (e *Engine) readExperience(ctx context.Context, readerID, id string, version int64) (CognitiveNode, error) {
	n, err := e.readCognitiveNode(ctx, readerID, id, version)
	if err != nil {
		return CognitiveNode{}, err
	}
	if n.Kind != "experience" || n.Experience == nil {
		return CognitiveNode{}, session.ErrNotFound
	}
	return n, nil
}

// ValidateExperience runs the exact currently approved checks. Closed failures remain negative
// observations. An unknown/error is not a counterexample and never promotes a candidate.
func (e *Engine) ValidateExperience(ctx context.Context, originID, id string, expected int64) (CognitiveNode, error) {
	lock, err := e.acquireRun(ctx, originID)
	if err != nil {
		return CognitiveNode{}, err
	}
	defer lock.Release()
	return e.validateExperience(ctx, originID, id, expected)
}

func (e *Engine) validateExperience(ctx context.Context, originID, id string, expected int64) (CognitiveNode, error) {
	n, err := e.readExperience(ctx, originID, id, 0)
	if err != nil {
		return CognitiveNode{}, err
	}
	if n.Version != expected {
		return CognitiveNode{}, session.ErrCognitiveRevision
	}
	x := n.Experience
	if x.Origin.Source.SessionID != originID {
		return CognitiveNode{}, session.ErrFenced
	}
	if x.State == knowledge.ExperienceNeedsRevision {
		return CognitiveNode{}, errors.New("experience counterexample requires a revised definition")
	}
	if err := e.experienceCurrent(ctx, n); err != nil {
		return CognitiveNode{}, err
	}
	x.Validation = knowledge.ExperienceValidation{Method: knowledge.ExperienceMethod}
	x.State = knowledge.ExperienceNeedsEvidence
	if len(x.Origin.Trace.Steps) == 0 || x.Origin.Trace.Incomplete || x.Origin.Trace.Truncated {
		x.Validation.Reason = "unconfirmed_origin_actions"
	} else if x.Origin.Outcome == "failed" && x.Definition.ExpectedResult == "passed" {
		x.State, x.Validation.Reason = knowledge.ExperienceRejected, "failed_origin_is_not_success"
	} else if x.Origin.Trace.Failed() && x.Definition.ExpectedResult == "passed" {
		x.State, x.Validation.Reason = knowledge.ExperienceRejected, "failed_origin_action_is_not_success"
	} else {
		assertions := append(slices.Clone(x.Definition.Preconditions), x.Definition.ExpectedOutcome)
		observations, checkErr := e.observeExperienceAssertions(ctx, originID, assertions, n.Scope, x.Origin.Source)
		if checkErr != nil && !errors.Is(checkErr, session.ErrNotQualified) {
			return CognitiveNode{}, checkErr
		}
		if checkErr != nil {
			x.Validation.Reason = "outcome_not_qualified"
		} else {
			x.Validation.Conditions = observations[:len(x.Definition.Preconditions)]
			x.Validation.Outcome = &observations[len(x.Definition.Preconditions)]
			x.State, x.Validation.Reason = knowledge.ExperienceValidated, "conditions_and_outcome_verified"
			for _, o := range x.Validation.Conditions {
				if o.Evidence.Verification != "passed" {
					x.State, x.Validation.Reason = knowledge.ExperienceRejected, "conditions_not_verified"
				}
			}
			if x.Validation.Outcome.Evidence.Verification != x.Definition.ExpectedResult {
				x.State, x.Validation.Reason = knowledge.ExperienceRejected, "outcome_counterexample"
			}
			if x.State == knowledge.ExperienceValidated {
				x.LastVerifiedAt = e.now()
			}
		}
	}
	return e.commitExperienceRevision(ctx, originID, n, expected)
}

func (e *Engine) observeExperienceConditions(ctx context.Context, consumerID string, conditions []CognitiveAssertion, scope CognitiveScope, source knowledge.SourceRef) ([]ExperienceObservation, error) {
	observations, err := e.observeExperienceAssertions(ctx, consumerID, conditions, scope, source)
	if err != nil {
		return nil, err
	}
	for _, o := range observations {
		if o.Evidence.Verification != "passed" {
			return observations, ErrExperienceNotApplicable
		}
	}
	return observations, nil
}

func (e *Engine) experienceCurrent(ctx context.Context, n CognitiveNode) error {
	if n.Status != "active" || n.Times.TimeState(e.now()) != "" {
		return ErrExperienceNotApplicable
	}
	reader, err := e.cognitiveReader(ctx, n.Experience.Origin.Source.SessionID)
	if err != nil {
		return err
	}
	if err := e.store.CognitiveDependenciesCurrent(ctx, n, reader, e.now()); err != nil {
		return err
	}
	x := n.Experience
	actual, err := e.store.CognitiveSource(ctx, x.Origin.Source.SessionID)
	if err != nil {
		return err
	}
	if actual != x.Origin.Source {
		return session.ErrSourceChanged
	}
	files, err := e.experienceFiles(ctx, x.Origin.Source.SessionID, x.Definition.CitedPaths)
	if err != nil {
		return err
	}
	if !slices.Equal(files, x.Origin.Trace.Files) {
		return session.ErrSourceChanged
	}
	return nil
}

func (e *Engine) commitExperienceRevision(ctx context.Context, readerID string, n CognitiveNode, expected int64) (CognitiveNode, error) {
	reader, err := e.cognitiveReader(ctx, readerID)
	if err != nil {
		return CognitiveNode{}, err
	}
	n.Version, n.Times.RecordedAt = expected+1, e.now()
	n.RevisionReason = "framework_experience_transition"
	n.Experience.Confidence = n.Experience.ConditionalConfidence()
	n.Times.VerifiedAt = n.Experience.LastVerifiedAt
	if err := e.store.CommitExperience(ctx, n, expected, e.projectID(), reader); err != nil {
		return CognitiveNode{}, err
	}
	return experienceForReader(n, readerID), nil
}

// RecallExperiences ranks scoped locators using the same lexical tokens as
// thread recall. A matching warning gets budget before positive examples.
func (e *Engine) RecallExperiences(ctx context.Context, readerID, pattern string, limit int) ([]ExperienceMatch, error) {
	if !knowledge.SafeExperienceText(pattern, 120) || limit < 1 || limit > 20 {
		return nil, errors.New("invalid experience retrieval query")
	}
	reader, err := e.cognitiveReader(ctx, readerID)
	if err != nil {
		return nil, err
	}
	nodes, err := e.store.CognitiveNodesByKind(ctx, reader, "experience")
	if err != nil {
		return nil, err
	}
	tokens := threadctx.Tokens(pattern)
	scores := make(map[string]int, len(nodes))
	for _, n := range nodes {
		scores[n.ID] = cognitiveRelevance(tokens, n.Title+" "+n.Experience.Definition.ProblemType+" "+n.Experience.Definition.Context)
	}
	slices.SortStableFunc(nodes, func(a, b knowledge.Node) int {
		if scores[a.ID] == 0 || scores[b.ID] == 0 {
			return scores[b.ID] - scores[a.ID]
		}
		if a.ProtectedMemory() != b.ProtectedMemory() {
			if a.ProtectedMemory() {
				return -1
			}
			return 1
		}
		return scores[b.ID] - scores[a.ID]
	})
	matches := make([]ExperienceMatch, 0, limit)
	for _, n := range nodes {
		if n.Experience == nil || scores[n.ID] == 0 {
			continue
		}
		if err := e.experienceCurrent(ctx, n); err != nil {
			if errors.Is(err, session.ErrSourceChanged) || errors.Is(err, session.ErrFenced) || errors.Is(err, session.ErrNotFound) || errors.Is(err, ErrExperienceNotApplicable) {
				continue
			}
			return nil, err
		}
		x := n.Experience
		// A record that can take no further use accepts no adoption, whatever its conditions.
		applicability := "requires_condition_check"
		if !n.AdmitsExperienceUse() {
			applicability = "use_limit_reached"
		}
		matches = append(matches, ExperienceMatch{ID: n.ID, Version: n.Version, DefinitionVersion: x.DefinitionVersion, ProblemPattern: x.Definition.ProblemPattern, State: x.State, Applicability: applicability, Confidence: x.Confidence})
		if len(matches) == limit {
			break
		}
	}
	return matches, nil
}

// ReviseExperience creates a new candidate definition, never erases a counterexample and never
// relabels an old observation. A changed source/citation requires extraction from a new real run.
func (e *Engine) ReviseExperience(ctx context.Context, originID, id string, expected int64, proposal ExperienceProposal) (CognitiveNode, error) {
	if err := proposal.Validate(); err != nil {
		return CognitiveNode{}, err
	}
	lock, err := e.acquireRun(ctx, originID)
	if err != nil {
		return CognitiveNode{}, err
	}
	defer lock.Release()
	n, err := e.readExperience(ctx, originID, id, 0)
	if err != nil {
		return CognitiveNode{}, err
	}
	if n.Version != expected {
		return CognitiveNode{}, session.ErrCognitiveRevision
	}
	if n.Experience.Origin.Source.SessionID != originID {
		return CognitiveNode{}, session.ErrFenced
	}
	if err := e.experienceCurrent(ctx, n); err != nil {
		return CognitiveNode{}, err
	}
	x := n.Experience
	x.SupersedesVersion, x.DefinitionVersion = x.DefinitionVersion, x.DefinitionVersion+1
	x.Definition, x.State, x.Validation = proposal, knowledge.ExperienceCandidate, knowledge.ExperienceValidation{}
	x.LastVerifiedAt = time.Time{}
	n.Title = proposal.ProblemPattern
	return e.commitExperienceRevision(ctx, originID, n, expected)
}

func experienceMissing(id string) error {
	return fmt.Errorf("%w: adoption %s", session.ErrNotFound, id)
}
