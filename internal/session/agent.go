package session

import (
	"context"
	"encoding/json"
	"time"
)

// Patch updates Session metadata without loading or rewriting its message/step history.
type Patch struct {
	Status     *string
	Provider   *string
	Model      *string
	Metadata   *json.RawMessage
	Final      *string
	ClearFinal bool
	Usage      *json.RawMessage
	Error      *json.RawMessage
	ClearError bool
}

func (s *Store) NextStep(ctx context.Context, sessionID string) (int64, error) {
	var step int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(step), 0) + 1 FROM agent_steps WHERE session_id = ?`, sessionID,
	).Scan(&step)
	return step, err
}

func (s *Store) Update(ctx context.Context, sessionID string, patch Patch) error {
	current, err := s.Get(ctx, sessionID)
	if err != nil {
		return err
	}
	if patch.Status != nil {
		current.Status = *patch.Status
	}
	if patch.Provider != nil {
		current.Provider = *patch.Provider
	}
	if patch.Model != nil {
		current.Model = *patch.Model
	}
	if patch.Metadata != nil {
		current.Metadata = append(json.RawMessage(nil), (*patch.Metadata)...)
	}
	if patch.ClearFinal {
		current.Final = nil
	} else if patch.Final != nil {
		value := *patch.Final
		current.Final = &value
	}
	if patch.Usage != nil {
		current.Usage = append(json.RawMessage(nil), (*patch.Usage)...)
	}
	if patch.ClearError {
		current.Error = nil
	} else if patch.Error != nil {
		current.Error = append(json.RawMessage(nil), (*patch.Error)...)
	}

	current.UpdatedAt = time.Now().UTC()
	var final any
	if current.Final != nil {
		final = *current.Final
	}
	var usage any
	if len(current.Usage) > 0 {
		usage = string(current.Usage)
	}
	var errorJSON any
	if len(current.Error) > 0 {
		errorJSON = string(current.Error)
	}
	_, err = s.db.ExecContext(ctx, `
UPDATE sessions SET
  updated_at = ?, status = ?, provider = NULLIF(?, ''), model = NULLIF(?, ''),
  metadata_json = ?, final = ?, usage_json = ?, error_json = ?
WHERE id = ?
`, current.UpdatedAt.Format(time.RFC3339Nano), current.Status, current.Provider, current.Model,
		normalizeJSON(current.Metadata, "{}"), final, usage, errorJSON, sessionID)
	return err
}
