package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/internal/taskstate"
)

// ExperienceOrigin reads a bounded terminal episode projection, never message/argument/output text.
// The caller holds the origin run lock. Its version is rechecked when the candidate commits.
func (s *Store) ExperienceOrigin(ctx context.Context, id string) (knowledge.ExperienceOrigin, error) {
	var origin knowledge.ExperienceOrigin
	err := s.readCognition(ctx, func(conn *sql.Conn) error {
		var err error
		origin.Source, origin.Outcome, err = experienceSource(ctx, conn, id, true)
		if err != nil {
			return err
		}
		if origin.Source.RunID == "" {
			return invalid("experience origin has no runtime run")
		}
		var purpose, updated string
		if err := conn.QueryRowContext(ctx, `SELECT COALESCE(json_extract(metadata_json,'$.purpose'),''),updated_at FROM sessions WHERE id=?`, id).Scan(&purpose, &updated); err != nil {
			return err
		}
		if purpose == PurposeMaintenance {
			return invalid("maintenance output is not an experience origin")
		}
		origin.CompletedAt = parseTime(updated)
		state, err := latestTaskState(ctx, conn, id)
		if err != nil {
			return err
		}
		origin.TaskVersion = state.Version
		origin.TaskRef = fmt.Sprintf("task_state:%s@%d", id, state.Version)
		decoded, err := taskstate.Decode(state.Version, state.JSON)
		if err != nil {
			return err
		}
		if decoded.Workflow != nil {
			raw, err := json.Marshal(decoded.Workflow)
			if err != nil {
				return err
			}
			origin.WorkflowDigest = knowledge.Digest("experience-workflow:v1", string(raw))
		}
		rows, err := conn.QueryContext(ctx, `SELECT d.execution_id,d.source_ref,d.tool_name,d.args_digest,d.outcome,COALESCE(d.coverage,''),d.lineage_complete
			FROM derived_origins d WHERE d.session_id=? AND d.run_id=? AND d.kind='execution' ORDER BY d.rowid LIMIT ?`, id, origin.Source.RunID, knowledge.MaxExperienceSteps+1)
		if err != nil {
			return err
		}
		for rows.Next() {
			var step knowledge.ExperienceStep
			var complete int
			if err := rows.Scan(&step.ExecutionID, &step.RequestID, &step.Tool, &step.ArgsDigest, &step.Outcome, &step.Coverage, &complete); err != nil {
				_ = rows.Close()
				return err
			}
			step.LineageComplete = complete == 1
			if len(origin.Trace.Steps) == knowledge.MaxExperienceSteps {
				origin.Trace.Truncated = true
				continue
			}
			if !step.LineageComplete || step.Outcome == "admitted" || step.Outcome == "outcome_unknown" {
				origin.Trace.Incomplete = true
			}
			if step.Outcome == "admitted" {
				step.Outcome = "outcome_unknown"
			}
			origin.Trace.Steps = append(origin.Trace.Steps, step)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		rows, err = conn.QueryContext(ctx, `SELECT r.seq,r.check_id,r.status,COALESCE(r.spec_digest,''),r.fingerprint,COALESCE(r.episode_id,''),COALESCE(r.approval_epoch,0),(`+qualifiedEvalSQL+`)
			FROM eval_results r WHERE r.session_id=? ORDER BY r.seq DESC LIMIT ?`, id, knowledge.MaxExperienceChecks+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var check knowledge.ExperienceCheck
			var qualified int
			if err := rows.Scan(&check.ResultSeq, &check.CheckID, &check.Outcome, &check.SpecDigest, &check.Fingerprint, &check.EpisodeID, &check.ApprovalEpoch, &qualified); err != nil {
				return err
			}
			check.Qualified = qualified == 1
			if len(origin.Trace.Checks) == knowledge.MaxExperienceChecks {
				origin.Trace.Truncated = true
				continue
			}
			origin.Trace.Checks = append(origin.Trace.Checks, check)
		}
		return rows.Err()
	})
	if err != nil {
		return knowledge.ExperienceOrigin{}, err
	}
	return origin, nil
}

// ExperienceConsumer can describe a created/paused consumer before a new run. It grants no evidence;
// only a terminal later SourceRef may be used as final feedback.
func (s *Store) ExperienceConsumer(ctx context.Context, id string) (knowledge.SourceRef, error) {
	return s.ExperienceConsumerForRun(ctx, id, "")
}

// ExperienceConsumerForRun accepts an active source only for its already admitted
// main request. The run identity comes from the runtime, not a tool argument.
func (s *Store) ExperienceConsumerForRun(ctx context.Context, id, runID string) (knowledge.SourceRef, error) {
	var ref knowledge.SourceRef
	err := s.readCognition(ctx, func(conn *sql.Conn) error {
		var status string
		var err error
		ref, status, err = experienceSource(ctx, conn, id, false)
		if err == nil && ((status == "running" && (runID == "" || ref.RunID != runID)) || (runID != "" && (status != "running" || ref.RunID != runID))) {
			return invalid("experience consumer run does not match")
		}
		return err
	})
	return ref, err
}

func experienceSource(ctx context.Context, conn *sql.Conn, id string, terminal bool) (knowledge.SourceRef, string, error) {
	var ref knowledge.SourceRef
	if _, fenced, err := sessionFence(ctx, conn, id); err != nil {
		return ref, "", err
	} else if fenced {
		return ref, "", ErrFenced
	}
	var status string
	err := conn.QueryRowContext(ctx, `SELECT status,`+sourceRunSQL+` FROM sessions s WHERE s.id=?`, id).Scan(&status, &ref.RunID)
	if errors.Is(err, sql.ErrNoRows) {
		return ref, "", ErrNotFound
	}
	if err != nil {
		return ref, "", err
	}
	if terminal && status != "completed" && status != "failed" {
		return ref, status, invalid("experience source is not terminal")
	}
	ref.Digest, err = sourceVersion(ctx, conn, id)
	if err != nil {
		return ref, status, err
	}
	ref.Kind, ref.ID, ref.SessionID, ref.Scope, ref.Sensitivity, ref.Provenance = knowledge.KindSession, id, id, SourceScopeProject, knowledge.SensitivitySession, knowledge.ProvenanceRuntime
	return ref, status, nil
}

// ExperienceOwner is the session that owns the content of the Experience identity id. The identity
// names its owner because a fence outlives the session it names: a withdrawal of the owner, the
// deletion that removes it with the record included, is still found when the identity is committed
// again (CommitExperience).
func ExperienceOwner(id string) string {
	return "session_" + strings.TrimPrefix(knowledge.Digest("experience-owner:v1", id), "sha256:")[:24]
}

// CommitExperience adds one immutable revision. A first extraction also creates its dedicated
// maintenance owner in this same transaction. No source content, transcript or other DB is created.
func (s *Store) CommitExperience(ctx context.Context, n knowledge.Node, expected int64, project string, reader knowledge.Reader) error {
	if n.Kind != "experience" || n.Experience == nil {
		return invalid("missing experience payload")
	}
	if err := n.Validate(); err != nil {
		return err
	}
	return s.writeTx(ctx, func(conn *sql.Conn) error {
		if err := cognitiveReader(ctx, conn, reader); err != nil {
			return err
		}
		if !n.Scope.VisibleTo(reader) {
			return ErrFenced
		}
		var old knowledge.Node
		if expected == 0 {
			if n.Experience.State != knowledge.ExperienceCandidate || n.Experience.DefinitionVersion != 1 || len(n.Experience.Uses) != 0 || !reflect.DeepEqual(n.Experience.Validation, knowledge.ExperienceValidation{}) {
				return invalid("new experience must be an unverified candidate")
			}
			if reader.SessionID != n.Experience.Origin.Source.SessionID {
				return ErrFenced
			}
			if n.SessionID != ExperienceOwner(n.ID) {
				return invalid("experience owner does not name its identity")
			}
			if _, err := loadCognitiveNode(ctx, conn, n.ID, 0); err == nil {
				return ErrCognitiveRevision
			} else if !errors.Is(err, ErrNotFound) {
				return err
			}
			// An identity names one origin episode and one definition. A withdrawal that erased its
			// record leaves its fence on the owner the identity names: the privacy deletion of a
			// consumer removes the whole combined record, with every invalidation and counterexample it
			// held, and the deletion of the owner removes the owner as well. The same episode does not
			// yield the identity again: a deletion is not weaker than the revocation it includes, which
			// hides the record and refuses this extraction.
			if _, withdrawn, err := sessionFence(ctx, conn, n.SessionID); err != nil {
				return err
			} else if withdrawn {
				return ErrFenced
			}
			meta, _ := json.Marshal(map[string]string{MetadataKeyPurpose: PurposeMaintenance})
			if err := insertSession(ctx, conn, Session{ID: n.SessionID, CreatedAt: n.Times.RecordedAt, UpdatedAt: n.Times.RecordedAt, Status: "completed", Goal: "Experimental experience record", Metadata: meta}); err != nil {
				return err
			}
		} else {
			var err error
			old, err = loadCognitiveNode(ctx, conn, n.ID, 0)
			if err != nil {
				return err
			}
			if old.Version != expected || old.Kind != "experience" {
				return ErrCognitiveRevision
			}
			visible, err := cognitiveVisible(ctx, conn, old, reader)
			if err != nil {
				return err
			}
			if !visible {
				return ErrFenced
			}
			if err := knowledge.ExperienceTransition(*old.Experience, *n.Experience); err != nil {
				return err
			}
		}
		if err := experienceEvidenceCommit(ctx, conn, old, n, project, reader); err != nil {
			return err
		}
		if err := putCognitiveRevision(ctx, conn, n, expected, project, expected > 0 && experienceSettlement(old, n)); err != nil {
			return err
		}
		if expected == 0 {
			if err := experienceLineage(ctx, conn, n.SessionID, n.Experience.Origin.Source.SessionID, n.ID); err != nil {
				return err
			}
		}
		if expected > 0 && len(n.Experience.Uses) > len(old.Experience.Uses) {
			u := n.Experience.Uses[len(n.Experience.Uses)-1]
			if err := experienceLineage(ctx, conn, n.SessionID, u.ConsumerID, n.ID); err != nil {
				return err
			}
			// The consumer receives the strategy only after this admission/provenance commit. What it
			// receives is the origin's definition and its own observations, so its lineage names the
			// origin: an edge to the record's owner would make it a reader of every other consumer the
			// record ever holds, and one consumer's withdrawal would fence them all.
			if err := experienceLineage(ctx, conn, u.ConsumerID, n.Experience.Origin.Source.SessionID, n.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

// experienceSettlement reports a revision that only closes one consumer's open use: the same
// lifecycle status, the same sources, the same definition and uses, one new feedback.
// ExperienceTransition has already refused every other change of the payload such a revision could
// carry. The sources are the stored ones, so a settlement cannot attach a source it never compared.
func experienceSettlement(old, next knowledge.Node) bool {
	a, b := old.Experience, next.Experience
	if next.Status != old.Status || !reflect.DeepEqual(next.Sources, old.Sources) || a.DefinitionVersion != b.DefinitionVersion || len(a.Uses) != len(b.Uses) {
		return false
	}
	for i := range a.Uses {
		if a.Uses[i].Feedback == nil && b.Uses[i].Feedback != nil {
			return true
		}
	}
	return false
}

func experienceLineage(ctx context.Context, conn *sql.Conn, owner, source, id string) error {
	if _, fenced, err := sessionFence(ctx, conn, source); err != nil {
		return err
	} else if fenced {
		return ErrFenced
	}
	var seq int64
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM session_messages WHERE session_id=?`, source).Scan(&seq); err != nil {
		return err
	}
	return insertEdges(ctx, conn, owner, "", "", []DerivedEdge{{Kind: "artifact", TargetRef: "cognition:" + id, SourceKind: SourceSession, SourceRef: source, OriginClass: OriginAgentGen, DerivedFrom: fmt.Sprintf("session:%s@%d", source, seq), LineageComplete: true}})
}

func experienceEvidenceCommit(ctx context.Context, conn *sql.Conn, old, n knowledge.Node, project string, reader knowledge.Reader) error {
	x := n.Experience
	if len(n.Sources) != 1 || n.Sources[0] != x.Origin.Source {
		return invalid("experience source lineage differs from origin")
	}
	if x.Origin.Source.SessionID == n.SessionID {
		return invalid("experience owner must be independent of its origin")
	}
	for _, u := range x.Uses {
		if err := sessionExists(ctx, conn, u.ConsumerID); err != nil {
			return err
		}
		if _, fenced, err := sessionFence(ctx, conn, u.ConsumerID); err != nil {
			return err
		} else if fenced {
			return ErrFenced
		}
	}
	if old.Experience == nil {
		return nil
	}
	before := old.Experience
	newObservation := func(o knowledge.ExperienceObservation) error {
		ref, _, err := experienceSource(ctx, conn, o.Evidence.SessionID, false)
		if err != nil {
			return err
		}
		if ref != o.Evidence.Source {
			return ErrSourceChanged
		}
		return cognitiveEvidence(ctx, conn, project, o.Assertion, o.Evidence)
	}
	if !reflect.DeepEqual(before.Validation, x.Validation) {
		if reader.SessionID != x.Origin.Source.SessionID {
			return ErrFenced
		}
		for _, o := range x.Validation.Conditions {
			if o.Evidence.SessionID != reader.SessionID {
				return ErrNotQualified
			}
			if err := newObservation(o); err != nil {
				return err
			}
		}
		if x.Validation.Outcome != nil {
			if x.Validation.Outcome.Evidence.SessionID != reader.SessionID {
				return ErrNotQualified
			}
			if err := newObservation(*x.Validation.Outcome); err != nil {
				return err
			}
		}
	}
	if x.DefinitionVersion != before.DefinitionVersion && reader.SessionID != x.Origin.Source.SessionID {
		return ErrFenced
	}
	if len(x.Uses) > len(before.Uses) {
		u := x.Uses[len(x.Uses)-1]
		if u.ConsumerID != reader.SessionID {
			return ErrFenced
		}
		ref, status, err := experienceSource(ctx, conn, u.ConsumerID, false)
		if err != nil {
			return err
		}
		if (status == "running" && (u.RunID == "" || u.RunID != ref.RunID)) || (u.RunID != "" && (status != "running" || u.RunID != ref.RunID)) || ref.Digest != u.BaselineDigest || ref.RunID != u.PreviousRunID {
			return ErrSourceChanged
		}
		for _, prior := range before.Uses {
			if prior.ConsumerID == u.ConsumerID && prior.BaselineDigest == u.BaselineDigest {
				return invalid("consumer already adopted at this source version")
			}
		}
		// Historical validation is not a present process result, but its original approval must still
		// stand. A revoked binding or a privacy epoch cannot silently confer adoption authority.
		for _, o := range before.Validation.Conditions {
			if err := cognitiveEvidence(ctx, conn, project, o.Assertion, o.Evidence); err != nil {
				return err
			}
		}
		if before.Validation.Outcome == nil {
			return ErrNotQualified
		}
		if err := cognitiveEvidence(ctx, conn, project, before.Validation.Outcome.Assertion, before.Validation.Outcome.Evidence); err != nil {
			return err
		}
		for _, o := range u.Conditions {
			if err := newObservation(o); err != nil {
				return err
			}
		}
	}
	for i := range before.Uses {
		if before.Uses[i].Feedback != nil || x.Uses[i].Feedback == nil {
			continue
		}
		u := x.Uses[i]
		if reader.SessionID != u.ConsumerID {
			return ErrFenced
		}
		ref, status, err := experienceSource(ctx, conn, u.ConsumerID, true)
		if err != nil {
			return err
		}
		f := u.Feedback
		if ref != f.Source {
			return ErrSourceChanged
		}
		if f.Outcome == "success" && status != "completed" {
			return ErrNotQualified
		}
		var observed *knowledge.ExperienceObservation
		if f.Used {
			application, err := experienceApplication(ctx, conn, u.ConsumerID, u.ID)
			if err != nil || application.ID != f.ApplicationID || !application.Executed() || application.RunID != f.Source.RunID {
				return ErrNotQualified
			}
			// A success credits the strategy: the receipt must show the origin's procedure, not an
			// action that failed while the expected outcome happened to hold already.
			if f.Outcome == "success" && !application.Reproduced(x.Origin.Trace.Steps) {
				return ErrNotQualified
			}
			observed = application.Outcome
		}
		// The outcome of a use is the one its receipt observed when the application ended. A check
		// made when the run ended, or later, also shows what the rest of the run and anyone else did
		// to the workspace. Its approval must still stand, as for any evidence that enters a record.
		if f.Observation != nil {
			if !reflect.DeepEqual(f.Observation, observed) {
				return ErrNotQualified
			}
			if err := cognitiveEvidence(ctx, conn, project, observed.Assertion, observed.Evidence); err != nil {
				return err
			}
		}
	}
	return nil
}

// ExperienceReferences uses the same scoped native graph projection; it never bypasses privacy to
// recover a historical strategy. Consumers are bounded by MaxExperienceUses on each record.
func experienceConsumerIDs(n knowledge.Node) []string {
	if n.Experience == nil {
		return nil
	}
	ids := make([]string, 0, len(n.Experience.Uses))
	for _, use := range n.Experience.Uses {
		if !slices.Contains(ids, use.ConsumerID) {
			ids = append(ids, use.ConsumerID)
		}
	}
	return ids
}
