package session

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// Patch updates the Session columns it names. It never touches metadata_json,
// which changes only through the single-key primitives in metadata.go.
type Patch struct {
	Status     *string
	Provider   *string
	Model      *string
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

// Update changes only the columns the patch names, in one statement, so it
// cannot overwrite a column another writer changed in the meantime.
func (s *Store) Update(ctx context.Context, sessionID string, patch Patch) error {
	sets := []string{"updated_at = ?"}
	args := []any{time.Now().UTC().Format(time.RFC3339Nano)}
	if patch.Status != nil {
		sets = append(sets, "status = ?")
		args = append(args, *patch.Status)
	}
	if patch.Provider != nil {
		sets = append(sets, "provider = NULLIF(?, '')")
		args = append(args, *patch.Provider)
	}
	if patch.Model != nil {
		sets = append(sets, "model = NULLIF(?, '')")
		args = append(args, *patch.Model)
	}
	if patch.ClearFinal {
		sets = append(sets, "final = NULL")
	} else if patch.Final != nil {
		sets = append(sets, "final = ?")
		args = append(args, *patch.Final)
	}
	if patch.Usage != nil {
		var usage any
		if len(*patch.Usage) > 0 {
			usage = string(*patch.Usage)
		}
		sets = append(sets, "usage_json = ?")
		args = append(args, usage)
	}
	if patch.ClearError {
		sets = append(sets, "error_json = NULL")
	} else if patch.Error != nil {
		var errorJSON any
		if len(*patch.Error) > 0 {
			errorJSON = string(*patch.Error)
		}
		sets = append(sets, "error_json = ?")
		args = append(args, errorJSON)
	}
	args = append(args, sessionID)
	result, err := s.db.ExecContext(ctx, "UPDATE sessions SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
