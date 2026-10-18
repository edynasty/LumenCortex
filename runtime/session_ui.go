package runtime

import (
	"context"
	"encoding/json"
	"strings"
)

// SessionUIState is the host's `ui` metadata key. Delegation is the thread's standing grant for
// start_task, as the user set it (ADR-0006 D4): it is presentation state only. A host passes it in
// AgentOptions.Delegation at each Start, and the Start, not this key, is what Core enforces; a change
// never reaches a run or an execution child that already exists.
type SessionUIState struct {
	Title      string `json:"title,omitempty"`
	Pinned     bool   `json:"pinned,omitempty"`
	Archived   bool   `json:"archived,omitempty"`
	Delegation string `json:"delegation,omitempty"`
}

type SessionUIPatch struct {
	Title    *string `json:"title,omitempty"`
	Pinned   *bool   `json:"pinned,omitempty"`
	Archived *bool   `json:"archived,omitempty"`
	// Delegation: "" or none, propose, isolated; any other value is refused before anything is written.
	Delegation *string `json:"delegation,omitempty"`
}

func (e *Engine) UpdateSessionUI(ctx context.Context, sessionID string, patch SessionUIPatch) (SessionInfo, error) {
	var delegation string
	if patch.Delegation != nil {
		var err error
		if delegation, err = normalizeDelegation(strings.TrimSpace(*patch.Delegation)); err != nil {
			return SessionInfo{}, err
		}
	}
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
	if patch.Delegation != nil {
		// none is the default and is not stored.
		ui.Delegation = ""
		if delegation != DelegationNone {
			ui.Delegation = delegation
		}
	}

	if ui == (SessionUIState{}) {
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
