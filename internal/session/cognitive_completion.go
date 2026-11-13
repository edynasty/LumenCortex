package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/edynasty/LumenCortex/internal/knowledge"
)

func experienceApplication(ctx context.Context, q rowQuerier, consumer, adoption string) (knowledge.Application, error) {
	var raw string
	var a knowledge.Application
	err := q.QueryRowContext(ctx, `SELECT json FROM cognition_applications WHERE session_id=? AND adoption_id=?`, consumer, adoption).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	err = json.Unmarshal([]byte(raw), &a)
	return a, err
}

func (s *Store) ExperienceApplication(ctx context.Context, consumer, adoption string) (knowledge.Application, error) {
	var a knowledge.Application
	err := s.readCognition(ctx, func(conn *sql.Conn) error {
		if _, fenced, err := consumerFence(ctx, conn, consumer); err != nil {
			return err
		} else if fenced {
			return ErrFenced
		}
		var err error
		a, err = experienceApplication(ctx, conn, consumer, adoption)
		return err
	})
	return a, err
}

// BeginExperienceApplication is the single admission point. It rechecks the
// definition, reader, source, adoption, and exact freshly executed conditions in
// one transaction. No external action occurs before this transaction commits.
func (s *Store) BeginExperienceApplication(ctx context.Context, a knowledge.Application, expected int64, project string, reader knowledge.Reader) error {
	if a.ID == "" || a.RunID == "" || a.SessionID != reader.SessionID || a.State != "pending" || a.At.IsZero() || len(a.Actions) < 1 || len(a.Actions) > 8 {
		return invalid("invalid experience application")
	}
	return s.writeTx(ctx, func(conn *sql.Conn) error {
		if err := cognitiveReader(ctx, conn, reader); err != nil {
			return err
		}
		n, err := loadCognitiveNode(ctx, conn, a.ExperienceID, 0)
		if err != nil {
			return err
		}
		if n.Version != expected || n.Experience == nil || n.Experience.State != knowledge.ExperienceValidated || n.Status != "active" || n.Times.TimeState(a.At) != "" {
			return ErrCognitiveRevision
		}
		visible, err := cognitiveVisible(ctx, conn, n, reader)
		if err != nil {
			return err
		}
		if !visible {
			return ErrFenced
		}
		if err := sourcesUnchanged(ctx, conn, n.SessionID, n.Sources); err != nil {
			return err
		}
		x := n.Experience
		i := slices.IndexFunc(x.Uses, func(u knowledge.ExperienceUse) bool { return u.ID == a.AdoptionID && u.ConsumerID == a.SessionID })
		if i < 0 {
			return ErrNotFound
		}
		u := x.Uses[i]
		if u.Feedback != nil || u.DefinitionVersion != x.DefinitionVersion || a.DefinitionVersion != x.DefinitionVersion || (u.RunID != "" && u.RunID != a.RunID) {
			return ErrCognitiveRevision
		}
		ref, status, err := experienceSource(ctx, conn, a.SessionID, false)
		if err != nil {
			return err
		}
		if status != "running" || ref.RunID != a.RunID {
			return ErrSourceChanged
		}
		if len(a.Conditions) != len(x.Definition.Preconditions) {
			return ErrNotQualified
		}
		for i, o := range a.Conditions {
			if o.Assertion != x.Definition.Preconditions[i] || o.Evidence.Source != ref || o.Evidence.Verification != "passed" {
				return ErrNotQualified
			}
			if err := cognitiveEvidence(ctx, conn, project, o.Assertion, o.Evidence); err != nil {
				return err
			}
		}
		for _, o := range x.Validation.Conditions {
			if err := cognitiveEvidence(ctx, conn, project, o.Assertion, o.Evidence); err != nil {
				return err
			}
		}
		if x.Validation.Outcome == nil {
			return ErrNotQualified
		}
		if err := cognitiveEvidence(ctx, conn, project, x.Validation.Outcome.Assertion, x.Validation.Outcome.Evidence); err != nil {
			return err
		}
		if err := matchExperienceActions(ctx, conn, x, a.Actions); err != nil {
			return err
		}
		raw, err := json.Marshal(a)
		if err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO cognition_applications(id,experience_id,adoption_id,session_id,run_id,state,created_at,json) VALUES(?,?,?,?,?,?,?,?)`, a.ID, a.ExperienceID, a.AdoptionID, a.SessionID, a.RunID, a.State, stamp(a.At), string(raw))
		return err
	})
}

func (s *Store) FinishExperienceApplication(ctx context.Context, a knowledge.Application) error {
	return s.writeTx(ctx, func(conn *sql.Conn) error {
		old, err := experienceApplication(ctx, conn, a.SessionID, a.AdoptionID)
		if err != nil {
			return err
		}
		if old.ID != a.ID || old.RunID != a.RunID || old.State != "pending" || len(old.Actions) != len(a.Actions) {
			return ErrCognitiveRevision
		}
		if a.State != "completed" && a.State != "failed" && a.State != "unknown" {
			return invalid("invalid application outcome")
		}
		var previousRow int64
		for i, action := range a.Actions {
			if action.Tool != old.Actions[i].Tool || action.ArgsDigest != old.Actions[i].ArgsDigest || (a.State == "completed" && action.Outcome != "completed") {
				return ErrNotQualified
			}
			if action.ExecutionID != "" {
				var outcome string
				var row int64
				err := conn.QueryRowContext(ctx, `SELECT rowid,outcome FROM derived_origins WHERE session_id=? AND run_id=? AND execution_id=? AND kind='execution' AND tool_name=? AND args_digest=? AND created_at>=?`, a.SessionID, a.RunID, action.ExecutionID, action.Tool, action.ArgsDigest, stamp(old.At)).Scan(&row, &outcome)
				if err != nil || row <= previousRow || action.Outcome != outcome || (outcome != "completed" && outcome != "failed" && !(a.State == "unknown" && outcome == "outcome_unknown")) {
					return ErrNotQualified
				}
				previousRow = row
			} else if action.Outcome != "" || a.State == "completed" {
				return ErrNotQualified
			}
		}
		raw, err := json.Marshal(a)
		if err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `UPDATE cognition_applications SET state=?,json=? WHERE id=? AND state='pending'`, a.State, string(raw), a.ID)
		return err
	})
}

// RecoverExperienceApplications never retries a side effect. Called only while
// the caller holds this session's run lock after the previous run ended.
func (s *Store) RecoverExperienceApplications(ctx context.Context, id string, now time.Time) error {
	return s.writeTx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `UPDATE cognition_applications SET state='unknown',json=json_set(json,'$.state','unknown','$.finishedAt',?) WHERE session_id=? AND state='pending'`, stamp(now), id)
		return err
	})
}

type ExperienceProposalRecord struct {
	ID        string
	SessionID string
	RunID     string
	State     string
	NodeID    string
	Proposal  knowledge.ExperienceProposal
}

func (s *Store) QueueExperienceProposal(ctx context.Context, r ExperienceProposalRecord) error {
	if r.Proposal.Validate() != nil || r.ID == "" || r.RunID == "" {
		return invalid("invalid experience proposal")
	}
	return s.writeTx(ctx, func(conn *sql.Conn) error {
		ref, status, err := experienceSource(ctx, conn, r.SessionID, false)
		if err != nil {
			return err
		}
		if status != "running" || ref.RunID != r.RunID {
			return ErrSourceChanged
		}
		var count int
		if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM cognition_proposals WHERE session_id=? AND run_id=?`, r.SessionID, r.RunID).Scan(&count); err != nil {
			return err
		}
		if count >= knowledge.MaxCandidates {
			return invalid("experience proposal limit reached")
		}
		raw, err := json.Marshal(r.Proposal)
		if err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO cognition_proposals(id,session_id,run_id,state,json) VALUES(?,?,?,'pending',?) ON CONFLICT(id) DO NOTHING`, r.ID, r.SessionID, r.RunID, string(raw))
		return err
	})
}

func (s *Store) PendingExperienceProposals(ctx context.Context, id string) ([]ExperienceProposalRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,run_id,json FROM cognition_proposals WHERE session_id=? AND state='pending' ORDER BY rowid LIMIT ?`, id, knowledge.MaxCandidates)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExperienceProposalRecord
	for rows.Next() {
		r := ExperienceProposalRecord{SessionID: id, State: "pending"}
		var raw string
		if err := rows.Scan(&r.ID, &r.RunID, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &r.Proposal); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) SettleExperienceProposal(ctx context.Context, id, nodeID, state string) error {
	if state != "completed" && state != "refused" {
		return invalid("invalid proposal outcome")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE cognition_proposals SET node_id=?,state=? WHERE id=? AND state='pending'`, nodeID, state, id)
	return err
}

func (s *Store) PendingCognitiveSessions(ctx context.Context) ([]string, error) {
	var ids []string
	err := s.readCognition(ctx, func(conn *sql.Conn) error {
		var err error
		ids, err = selectStrings(ctx, conn, `SELECT DISTINCT session_id FROM cognition_proposals WHERE state='pending' UNION SELECT DISTINCT session_id FROM cognition_applications WHERE state='pending' UNION SELECT DISTINCT json_extract(u.value,'$.consumerId') FROM cognition_nodes n,json_each(n.json,'$.experience.uses') u WHERE n.kind='experience' AND n.version=(SELECT max(v.version) FROM cognition_nodes v WHERE v.id=n.id) AND json_type(u.value,'$.feedback') IS NULL`)
		return err
	})
	return ids, err
}
