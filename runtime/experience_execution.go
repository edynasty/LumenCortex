package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

type experienceAction struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}

type cognitiveAccess struct {
	engine           *Engine
	sessionID, runID string
}

func decodeCognitiveInput(args map[string]any, out any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	if len(raw) > 32768 {
		return errors.New("cognitive input exceeds bound")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return errors.New("trailing cognitive input")
	}
	return nil
}

func cognitiveToolResult(v any) (protocol.ToolResult, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	return protocol.ToolResult{OK: true, Content: string(raw)}, nil
}

func (a cognitiveAccess) CognitiveCall(ctx context.Context, name string, args map[string]any) (protocol.ToolResult, error) {
	if a.runID == "" {
		return protocol.ToolResult{}, errors.New("cognition tools require an admitted run")
	}
	if err := a.engine.runtimeEventAccess(ctx, a.sessionID); err != nil {
		return protocol.ToolResult{}, err
	}
	switch name {
	case "recall_experiences":
		var in struct {
			Query string `json:"query"`
		}
		if err := decodeCognitiveInput(args, &in); err != nil {
			return protocol.ToolResult{}, err
		}
		matches, err := a.engine.RecallExperiences(ctx, a.sessionID, in.Query, 8)
		if err != nil {
			return protocol.ToolResult{}, err
		}
		refs := make([]knowledge.NodeRef, len(matches))
		for i, m := range matches {
			refs[i] = knowledge.NodeRef{ID: m.ID, Version: m.Version}
		}
		reader, err := a.engine.cognitiveReader(ctx, a.sessionID)
		if err != nil {
			return protocol.ToolResult{}, err
		}
		if err := a.engine.store.RecordCognitiveAccess(ctx, refs, reader, a.engine.now()); err != nil {
			return protocol.ToolResult{}, err
		}
		sources := make([]string, 0, len(matches))
		for _, m := range matches {
			n, err := a.engine.readExperience(ctx, a.sessionID, m.ID, 0)
			if err != nil {
				return protocol.ToolResult{}, err
			}
			source := n.Experience.Origin.Source.SessionID
			if n.Experience.State == knowledge.ExperienceNeedsRevision {
				source = n.SessionID
			}
			if !slices.Contains(sources, source) {
				sources = append(sources, source)
			}
		}
		return cognitiveToolResult(struct {
			Matches []ExperienceMatch `json:"matches"`
			Sources []string          `json:"sourceSessions"`
		}{matches, sources})
	case "adopt_experience":
		var in struct {
			ID      string `json:"id"`
			Version int64  `json:"version"`
		}
		if err := decodeCognitiveInput(args, &in); err != nil {
			return protocol.ToolResult{}, err
		}
		adoption, err := a.engine.adoptExperience(ctx, a.sessionID, in.ID, in.Version, a.runID)
		if err != nil {
			return protocol.ToolResult{}, err
		}
		n, err := a.engine.readExperience(ctx, a.sessionID, in.ID, 0)
		if err != nil {
			return protocol.ToolResult{}, err
		}
		return cognitiveToolResult(struct {
			ExperienceAdoption
			Sources []string `json:"sourceSessions"`
		}{adoption, []string{n.Experience.Origin.Source.SessionID}})
	case "apply_experience":
		var in struct {
			ID         string             `json:"id"`
			AdoptionID string             `json:"adoptionId"`
			Actions    []experienceAction `json:"actions"`
		}
		if err := decodeCognitiveInput(args, &in); err != nil {
			return protocol.ToolResult{}, err
		}
		return a.apply(ctx, in.ID, in.AdoptionID, in.Actions)
	case "propose_experience":
		var in struct {
			Proposal ExperienceProposal `json:"proposal"`
		}
		if err := decodeCognitiveInput(args, &in); err != nil {
			return protocol.ToolResult{}, err
		}
		id := knowledge.Digest("experience-proposal", a.sessionID, a.runID, in.Proposal.Digest())
		err := a.engine.store.QueueExperienceProposal(ctx, session.ExperienceProposalRecord{ID: id, SessionID: a.sessionID, RunID: a.runID, Proposal: in.Proposal})
		if err != nil {
			return protocol.ToolResult{}, err
		}
		return cognitiveToolResult(map[string]string{"id": id, "state": "pending", "meaning": "candidate only; finalization requires a real terminal run and host-bound checks"})
	case "assess_cognition":
		var in struct {
			Query  string   `json:"query"`
			IDs    []string `json:"ids"`
			Action string   `json:"action"`
		}
		if err := decodeCognitiveInput(args, &in); err != nil {
			return protocol.ToolResult{}, err
		}
		assessment, err := a.engine.assessCognitiveDecision(ctx, a.sessionID, a.runID, in.Query, in.IDs, in.Action)
		if err != nil {
			return protocol.ToolResult{}, err
		}
		return cognitiveToolResult(assessment)
	}
	return protocol.ToolResult{}, errors.New("unknown cognitive operation")
}

func (a cognitiveAccess) apply(ctx context.Context, id, adoptionID string, actions []experienceAction) (protocol.ToolResult, error) {
	if len(actions) < 1 || len(actions) > 8 {
		return protocol.ToolResult{}, errors.New("one to eight actions required")
	}
	caller, ok := protocol.ToolCallbackFromContext(ctx)
	if !ok {
		return protocol.ToolResult{}, errors.New("application requires the active run's tool authority")
	}
	n, err := a.engine.readExperience(ctx, a.sessionID, id, 0)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	if err := a.engine.experienceCurrent(ctx, n); err != nil {
		return protocol.ToolResult{}, err
	}
	i := slices.IndexFunc(n.Experience.Uses, func(u knowledge.ExperienceUse) bool { return u.ID == adoptionID && u.ConsumerID == a.sessionID })
	if i < 0 {
		return protocol.ToolResult{}, session.ErrNotFound
	}
	u := n.Experience.Uses[i]
	if u.DefinitionVersion != n.Experience.DefinitionVersion || u.Feedback != nil {
		return protocol.ToolResult{}, session.ErrCognitiveRevision
	}
	source, err := a.engine.store.ExperienceConsumerForRun(ctx, a.sessionID, a.runID)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	conditions, err := a.engine.observeExperienceConditions(ctx, a.sessionID, n.Experience.Definition.Preconditions, n.Scope, source)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	receiptID, err := newCandidateID()
	if err != nil {
		return protocol.ToolResult{}, err
	}
	receipt := knowledge.Application{ID: receiptID, ExperienceID: id, AdoptionID: adoptionID, SessionID: a.sessionID, RunID: a.runID, DefinitionVersion: u.DefinitionVersion, State: "pending", At: a.engine.now(), Conditions: conditions}
	for _, action := range actions {
		if action.Tool == "" || action.Arguments == nil {
			return protocol.ToolResult{}, errors.New("invalid procedure action")
		}
		raw, err := json.Marshal(action.Arguments)
		if err != nil {
			return protocol.ToolResult{}, err
		}
		digest := sha256.Sum256(raw)
		receipt.Actions = append(receipt.Actions, knowledge.AppliedAction{Tool: action.Tool, ArgsDigest: "sha256:" + hex.EncodeToString(digest[:])})
	}
	reader, err := a.engine.cognitiveReader(ctx, a.sessionID)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	if err := a.engine.store.BeginExperienceApplication(ctx, receipt, n.Version, a.engine.projectID(), reader); err != nil {
		return protocol.ToolResult{}, err
	}
	result := protocol.ToolResult{OK: true}
	var results []protocol.ToolResult
	receipt.State = "completed"
	for index, action := range actions {
		if err = a.engine.runtimeEventAccess(ctx, a.sessionID); err == nil {
			err = a.engine.experienceCurrent(ctx, n)
		}
		if err != nil {
			receipt.State = "unknown"
			result.OK = false
			break
		}
		current, checkErr := a.engine.readExperience(ctx, a.sessionID, id, 0)
		if checkErr != nil || current.Experience.DefinitionVersion != u.DefinitionVersion || current.Experience.State != knowledge.ExperienceValidated {
			err = session.ErrCognitiveRevision
			receipt.State = "unknown"
			result.OK = false
			break
		}
		if index > 0 {
			source, checkErr = a.engine.store.ExperienceConsumerForRun(ctx, a.sessionID, a.runID)
			if checkErr == nil {
				_, checkErr = a.engine.observeExperienceConditions(ctx, a.sessionID, current.Experience.Definition.Preconditions, current.Scope, source)
			}
			if checkErr != nil {
				err = checkErr
				receipt.State = "unknown"
				result.OK = false
				break
			}
		}
		nested, callErr := caller.Call(ctx, action.Tool, action.Arguments)
		result.MutatesWorkspace = result.MutatesWorkspace || nested.MutatesWorkspace
		results = append(results, nested)
		if !nested.Denied && !nested.Interrupted && callErr == nil {
			receipt.Actions[index].ExecutionID, callErr = a.engine.store.ExperienceActionExecution(context.WithoutCancel(ctx), a.sessionID, a.runID, action.Tool, receipt.Actions[index].ArgsDigest, receipt.At)
		}
		if callErr != nil || !nested.OK || nested.Denied || nested.Interrupted {
			err = callErr
			result.OK = false
			result.Denied = nested.Denied
			result.Interrupted = nested.Interrupted
			receipt.State = "failed"
			if ctx.Err() != nil || nested.Interrupted {
				receipt.State = "unknown"
			}
			break
		}
		receipt.Actions[index].Completed = true
	}
	receipt.FinishedAt = a.engine.now()
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if settleErr := a.engine.store.FinishExperienceApplication(settleCtx, receipt); settleErr != nil {
		return protocol.ToolResult{Interrupted: true, MutatesWorkspace: result.MutatesWorkspace}, settleErr
	}
	out := struct {
		ApplicationID string                `json:"applicationId"`
		State         string                `json:"state"`
		Results       []protocol.ToolResult `json:"results"`
		Sources       []string              `json:"sourceSessions"`
		Error         string                `json:"error,omitempty"`
	}{receipt.ID, receipt.State, results, []string{n.Experience.Origin.Source.SessionID}, ""}
	if err != nil {
		out.Error = err.Error()
	}
	raw, marshalErr := json.Marshal(out)
	if marshalErr != nil {
		return result, marshalErr
	}
	result.Content = string(raw)
	return result, nil
}

// CognitionCompletion reports the secondary memory phase separately from the
// Agent task's result. A storage/check failure is observable and recoverable.
type CognitionCompletion struct {
	Extracted int      `json:"extracted"`
	Feedback  int      `json:"feedback"`
	Errors    []string `json:"errors,omitempty"`
}

func (e *Engine) finalizeCognitiveRun(ctx context.Context, id string) CognitionCompletion {
	report := CognitionCompletion{}
	if err := e.store.RecoverExperienceApplications(ctx, id, e.now()); err != nil {
		report.Errors = append(report.Errors, err.Error())
		return report
	}
	row, err := e.store.Get(ctx, id)
	if err != nil {
		report.Errors = append(report.Errors, err.Error())
		return report
	}
	if row.Status != "completed" && row.Status != "failed" {
		return report
	}
	proposals, err := e.store.PendingExperienceProposals(ctx, id)
	if err != nil {
		report.Errors = append(report.Errors, err.Error())
		return report
	}
	for _, p := range proposals {
		source, err := e.store.CognitiveSource(ctx, id)
		if err != nil {
			report.Errors = append(report.Errors, err.Error())
			continue
		}
		if p.RunID != source.RunID {
			if err := e.store.SettleExperienceProposal(ctx, p.ID, "", "refused"); err != nil {
				report.Errors = append(report.Errors, err.Error())
			}
			continue
		}
		n, err := e.extractExperience(ctx, id, ExperienceExtraction{ScopeKind: string(knowledge.ScopeProject), Proposal: p.Proposal})
		if err == nil && (n.Experience.State == knowledge.ExperienceCandidate || n.Experience.State == knowledge.ExperienceNeedsEvidence) {
			n, err = e.validateExperience(ctx, id, n.ID, n.Version)
		}
		if err != nil {
			report.Errors = append(report.Errors, err.Error())
			continue
		}
		if err := e.store.SettleExperienceProposal(ctx, p.ID, n.ID, "completed"); err != nil {
			report.Errors = append(report.Errors, err.Error())
			continue
		}
		report.Extracted++
	}
	reader, err := e.cognitiveReader(ctx, id)
	if err != nil {
		report.Errors = append(report.Errors, err.Error())
		return report
	}
	nodes, err := e.store.ExperienceContext(ctx, reader)
	if err != nil {
		report.Errors = append(report.Errors, err.Error())
		return report
	}
	for _, n := range nodes {
		for _, u := range n.Experience.Uses {
			if u.ConsumerID != id || u.Feedback != nil {
				continue
			}
			if _, err := e.recordExperienceFeedback(ctx, id, n.ID, u.ID); err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", n.ID, err))
			} else {
				report.Feedback++
			}
		}
	}
	return report
}

func (e *Engine) recoverCognitiveRuns(ctx context.Context) error {
	ids, err := e.store.PendingCognitiveSessions(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		lock, err := e.acquireRun(ctx, id)
		if errors.Is(err, ErrRunHeld) || errors.Is(err, session.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		report := e.finalizeCognitiveRun(ctx, id)
		lock.Release()
		if len(report.Errors) > 0 {
			e.events.publish("cognition.recovery.failed", id, map[string]any{"errors": report.Errors})
		}
	}
	return nil
}
