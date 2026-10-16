// Package taskstate is the durable task memory beside the transcript (design §4.4, ADR-0006 D1′/D2):
// the user's brief and references, the thread resolution, the host workflow, work units, progress and
// the window start. It holds pure state and rules; internal/session stores the versions and the inbox.
//
// Task State never holds provider reasoning: every field has a runtime or user writer named here, and
// none of them reads a model's hidden reasoning.
package taskstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/edynasty/LumenCortex/internal/workflow"
)

// Reasons recorded with a Task State version.
const (
	ReasonMigrated     = "migrated"
	ReasonStart        = "start"
	ReasonStep         = "step"
	ReasonUser         = "user"
	ReasonContextReset = "context_reset"
	ReasonApply        = "apply"
	// ReasonToolPrefix is followed by the tool whose result changed the state.
	ReasonToolPrefix = "tool:"
)

// MaxBriefBytes bounds the brief, which replaces the goal in the Task State block.
const MaxBriefBytes = 4096

// State is one version of a session's Task State. The goal is not part of it: sessions.goal, written
// once when the session is created, is its only store; Brief overrides it in the Task State block.
type State struct {
	// Version is the stored version this state was read as (0: none stored yet). It is the base of
	// the next write and is not part of the JSON.
	Version    int64       `json:"-"`
	Brief      string      `json:"brief,omitempty"`
	References []Reference `json:"references,omitempty"`
	Resolution Resolution  `json:"resolution"`
	// WorkUnits is nil until the host seeds units or the model creates one.
	WorkUnits *WorkUnitState `json:"workUnits,omitempty"`
	// Workflow is the host workflow's snapshot, nil when the session has none.
	Workflow *workflow.Snapshot `json:"workflow,omitempty"`
	Progress Progress           `json:"progress"`
	// WindowStartSeq is the lowest transcript seq a request may use. Only migration 0003 and a
	// context reset move it, and never backwards.
	WindowStartSeq int64 `json:"windowStartSeq"`
}

// Decode reads one stored version. Empty raw is the empty state of a session without a version.
func Decode(version int64, raw []byte) (State, error) {
	var s State
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &s); err != nil {
			return State{}, fmt.Errorf("decode task state v%d: %w", version, err)
		}
	}
	s.Version = version
	if s.Resolution.State == "" {
		s.Resolution.State = ResolutionOpen
	}
	return s, nil
}

// Encode is the stored JSON of s.
func (s State) Encode() (json.RawMessage, error) {
	return json.Marshal(s)
}

// Clone returns a deep copy of s.
func (s State) Clone() State {
	raw, err := s.Encode()
	if err != nil {
		panic(fmt.Sprintf("task state is not serializable: %v", err))
	}
	out, err := Decode(s.Version, raw)
	if err != nil {
		panic(fmt.Sprintf("task state does not round-trip: %v", err))
	}
	return out
}

// SameContent reports whether a and b store the same JSON, ignoring Version.
func SameContent(a, b State) bool {
	left, errA := a.Encode()
	right, errB := b.Encode()
	return errA == nil && errB == nil && bytes.Equal(left, right)
}

// Objective is the text the Task State block leads with: the brief when the user set one, else the goal.
func (s State) Objective(goal string) string {
	if s.Brief != "" {
		return s.Brief
	}
	return goal
}

// RestoreWorkflow rebuilds the host workflow runtime from the snapshot; nil when there is none.
func (s State) RestoreWorkflow() (*workflow.Runtime, error) {
	if s.Workflow == nil {
		return nil, nil
	}
	if s.Workflow.Definition == nil {
		return nil, errors.New("stored workflow has no definition")
	}
	return workflow.New(s.Workflow.Definition, &workflow.State{
		CurrentAction: s.Workflow.CurrentAction,
		Facts:         s.Workflow.Facts,
		FactSources:   s.Workflow.FactSources,
		History:       s.Workflow.History,
		Status:        s.Workflow.Status,
	})
}

// SetWorkflow stores the runtime's snapshot; nil leaves the state without a workflow.
func (s *State) SetWorkflow(wf *workflow.Runtime) {
	if wf == nil {
		return
	}
	snap := wf.Snapshot()
	s.Workflow = &snap
}

// Units returns a work-unit manager over the stored units (empty when there are none).
func (s State) Units() *WorkUnitManager {
	return NewWorkUnitManager(s.WorkUnits)
}

// SetUnits stores the manager's units. A session that never had units keeps none while the manager is
// empty, so a run that creates no unit writes no unit state.
func (s *State) SetUnits(m *WorkUnitManager) {
	if m == nil || (s.WorkUnits == nil && len(m.State.Order) == 0) {
		return
	}
	snap := m.Snapshot()
	s.WorkUnits = &snap
}

// Seed applies a Start's workflow and work-unit seeds (design §4.4): each only while the state holds
// none. A supplied workflow whose definition differs from the stored one is an error; supplied units are
// otherwise ignored. It reports whether s changed.
func (s *State) Seed(workflowJSON, workUnitsJSON []byte) (bool, error) {
	changed := false
	if len(workflowJSON) > 0 {
		def, err := workflow.Parse(workflowJSON)
		if err != nil {
			return false, err
		}
		if s.Workflow != nil {
			if s.Workflow.Definition == nil {
				return false, errors.New("stored workflow has no definition")
			}
			left, _ := json.Marshal(s.Workflow.Definition)
			right, _ := json.Marshal(def)
			if !bytes.Equal(left, right) {
				return false, errors.New("workflow definition differs from the contract persisted in this session")
			}
		} else {
			wf, err := workflow.New(def, nil)
			if err != nil {
				return false, err
			}
			s.SetWorkflow(wf)
			changed = true
		}
	} else if _, err := s.RestoreWorkflow(); err != nil {
		return false, err
	}
	if len(workUnitsJSON) > 0 && s.WorkUnits == nil {
		units, err := ParseWorkUnits(workUnitsJSON)
		if err != nil {
			return false, err
		}
		manager := NewWorkUnitManager(nil)
		if err := manager.Seed(units); err != nil {
			return false, err
		}
		snap := manager.Snapshot()
		s.WorkUnits = &snap
		changed = true
	}
	return changed, nil
}
