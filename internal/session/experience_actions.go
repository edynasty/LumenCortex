package session

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/protocol"
)

func actionDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// canonicalActionDigest reads only the already-answered tool call bound to the
// origin execution. It does not infer executed arguments from summary prose.
func canonicalActionDigest(ctx context.Context, q rowQuerier, origin knowledge.SourceRef, step knowledge.ExperienceStep) (string, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT m.json FROM request_envelopes r JOIN session_messages m ON m.session_id=r.session_id AND m.seq=r.answered_seq WHERE r.session_id=? AND r.run_id=? AND r.request_id=?`, origin.SessionID, origin.RunID, step.RequestID).Scan(&raw)
	if err != nil {
		return "", err
	}
	var message protocol.Message
	if err := json.Unmarshal([]byte(raw), &message); err != nil {
		return "", err
	}
	canonical := func(name string, args json.RawMessage) (string, bool) {
		var obj map[string]any
		if name != step.Tool || json.Unmarshal(args, &obj) != nil {
			return "", false
		}
		encoded, err := json.Marshal(obj)
		if err != nil {
			return "", false
		}
		if actionDigest(args) != step.ArgsDigest && actionDigest(encoded) != step.ArgsDigest {
			return "", false
		}
		return actionDigest(encoded), true
	}
	for _, call := range message.ToolCalls {
		if digest, ok := canonical(call.Name, call.Arguments); ok {
			return digest, nil
		}
		if call.Name == "apply_experience" {
			var nested struct {
				Actions []struct {
					Tool      string          `json:"tool"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"actions"`
			}
			if json.Unmarshal(call.Arguments, &nested) != nil {
				continue
			}
			for _, action := range nested.Actions {
				if digest, ok := canonical(action.Tool, action.Arguments); ok {
					return digest, nil
				}
			}
		}
	}
	return "", ErrNotQualified
}

func matchExperienceActions(ctx context.Context, q rowQuerier, x *knowledge.Experience, actions []knowledge.AppliedAction) error {
	if x.Origin.Trace.Incomplete || x.Origin.Trace.Truncated {
		return ErrNotQualified
	}
	steps := x.Origin.Trace.Steps
	if len(steps) != len(actions) || len(steps) == 0 {
		return invalid("application must match the origin's complete action sequence")
	}
	for i, step := range steps {
		if step.Tool != actions[i].Tool || (step.Outcome != "completed" && step.Outcome != "failed") {
			return ErrNotQualified
		}
		digest, err := canonicalActionDigest(ctx, q, x.Origin.Source, step)
		if err != nil {
			return err
		}
		if digest != actions[i].ArgsDigest {
			return invalid("application action differs from the observed procedure")
		}
	}
	return nil
}

func (s *Store) ExperienceActionExecution(ctx context.Context, id, runID, tool, digest string, after time.Time) (string, error) {
	var execution string
	err := s.db.QueryRowContext(ctx, `SELECT execution_id FROM derived_origins WHERE session_id=? AND run_id=? AND kind='execution' AND tool_name=? AND args_digest=? AND created_at>=? AND outcome IN ('completed','failed') ORDER BY rowid DESC LIMIT 1`, id, runID, tool, digest, stamp(after)).Scan(&execution)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return execution, err
}
