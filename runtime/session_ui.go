package runtime

import (
	"context"
	"encoding/json"
	"strings"
)

type SessionUIState struct {
	Title    string `json:"title,omitempty"`
	Pinned   bool   `json:"pinned,omitempty"`
	Archived bool   `json:"archived,omitempty"`
}

type SessionUIPatch struct {
	Title    *string `json:"title,omitempty"`
	Pinned   *bool   `json:"pinned,omitempty"`
	Archived *bool   `json:"archived,omitempty"`
}

func (e *Engine) UpdateSessionUI(ctx context.Context, sessionID string, patch SessionUIPatch) (SessionInfo, error) {
	current, err := e.store.Get(ctx, sessionID)
	if err != nil {
		return SessionInfo{}, err
	}

	metadata := map[string]any{}
	if len(current.Metadata) > 0 {
		if err := json.Unmarshal(current.Metadata, &metadata); err != nil {
			return SessionInfo{}, err
		}
	}

	ui := SessionUIState{}
	if rawUI, ok := metadata["ui"]; ok {
		raw, err := json.Marshal(rawUI)
		if err != nil {
			return SessionInfo{}, err
		}
		if err := json.Unmarshal(raw, &ui); err != nil {
			return SessionInfo{}, err
		}
	}
	if patch.Title != nil {
		ui.Title = strings.TrimSpace(*patch.Title)
	}
	if patch.Pinned != nil {
		ui.Pinned = *patch.Pinned
	}
	if patch.Archived != nil {
		ui.Archived = *patch.Archived
	}

	if ui.Title == "" && !ui.Pinned && !ui.Archived {
		err = e.store.RemoveHostMetadataKey(ctx, sessionID, "ui")
	} else {
		var raw []byte
		if raw, err = json.Marshal(ui); err != nil {
			return SessionInfo{}, err
		}
		err = e.store.SetHostMetadataKey(ctx, sessionID, "ui", raw)
	}
	if err != nil {
		return SessionInfo{}, err
	}
	_, info, err := e.Session(ctx, sessionID)
	return info, err
}
