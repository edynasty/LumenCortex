package cognition

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type WorkUnitStatus string

const (
	WorkPending   WorkUnitStatus = "pending"
	WorkActive    WorkUnitStatus = "active"
	WorkBlocked   WorkUnitStatus = "blocked"
	WorkVerifying WorkUnitStatus = "verifying"
	WorkCompleted WorkUnitStatus = "completed"
	WorkFailed    WorkUnitStatus = "failed"
	WorkCancelled WorkUnitStatus = "cancelled"
)

type EvidenceRef struct {
	Requirement string `json:"requirement"`
	Ref         string `json:"ref,omitempty"`
	Summary     string `json:"summary,omitempty"`
}

type VerificationResult struct {
	Check  string `json:"check"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type WorkUnit struct {
	ID                  string               `json:"id"`
	Goal                string               `json:"goal"`
	Description         string               `json:"description,omitempty"`
	Status              WorkUnitStatus       `json:"status"`
	Risk                string               `json:"risk"`
	RequiredEvidence    []string             `json:"requiredEvidence,omitempty"`
	Verification        []string             `json:"verification,omitempty"`
	DependsOn           []string             `json:"dependsOn,omitempty"`
	Evidence            []EvidenceRef        `json:"evidence,omitempty"`
	VerificationResults []VerificationResult `json:"verificationResults,omitempty"`
	Summary             string               `json:"summary,omitempty"`
	CreatedAt           time.Time            `json:"createdAt"`
	UpdatedAt           time.Time            `json:"updatedAt"`
	CompletedAt         *time.Time           `json:"completedAt,omitempty"`
}

type WorkUnitState struct {
	Version  int                 `json:"version"`
	Order    []string            `json:"order"`
	Items    map[string]WorkUnit `json:"items"`
	ActiveID string              `json:"activeId,omitempty"`
}

type WorkUnitManager struct {
	State WorkUnitState
	now   func() time.Time
}

func NewWorkUnitManager(state *WorkUnitState) *WorkUnitManager {
	m := &WorkUnitManager{now: time.Now}
	if state == nil {
		m.State = WorkUnitState{Version: 1, Items: map[string]WorkUnit{}}
	} else {
		m.State = cloneWorkUnitState(*state)
		if m.State.Version == 0 {
			m.State.Version = 1
		}
		if m.State.Items == nil {
			m.State.Items = map[string]WorkUnit{}
		}
	}
	return m
}

func ParseWorkUnits(raw []byte) ([]WorkUnit, error) {
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	var rows []any
	switch value := payload.(type) {
	case []any:
		rows = value
	case map[string]any:
		list, ok := value["workUnits"].([]any)
		if !ok {
			return nil, errors.New("work unit payload must contain workUnits array")
		}
		rows = list
	default:
		return nil, errors.New("work unit payload must be an array or object")
	}
	out := make([]WorkUnit, 0, len(rows))
	for _, row := range rows {
		obj, ok := row.(map[string]any)
		if !ok {
			return nil, errors.New("work unit entry must be an object")
		}
		for _, forbidden := range []string{"model", "models", "provider", "category", "reasoningEffort", "reasoning_effort"} {
			if _, exists := obj[forbidden]; exists {
				return nil, fmt.Errorf("work units cannot select %s", forbidden)
			}
		}
		normalized, err := json.Marshal(normalizeWorkUnitJSON(obj))
		if err != nil {
			return nil, err
		}
		var unit WorkUnit
		if err := json.Unmarshal(normalized, &unit); err != nil {
			return nil, err
		}
		out = append(out, unit)
	}
	return out, nil
}

func (m *WorkUnitManager) Seed(units []WorkUnit) error {
	if len(m.State.Order) != 0 {
		return errors.New("work units are already initialized")
	}
	seen := map[string]bool{}
	now := m.now().UTC()
	for i := range units {
		unit := units[i]
		if strings.TrimSpace(unit.ID) == "" {
			unit.ID = fmt.Sprintf("wu_%d", i+1)
		}
		if strings.TrimSpace(unit.Goal) == "" {
			return errors.New("work unit goal is required")
		}
		if seen[unit.ID] {
			return fmt.Errorf("duplicate work unit id: %s", unit.ID)
		}
		seen[unit.ID] = true
		if unit.Status == "" {
			unit.Status = WorkPending
		}
		if unit.Risk == "" {
			unit.Risk = "medium"
		}
		if !validWorkStatus(unit.Status) {
			return fmt.Errorf("invalid work unit status: %s", unit.Status)
		}
		if !validRisk(unit.Risk) {
			return fmt.Errorf("invalid work unit risk: %s", unit.Risk)
		}
		if unit.CreatedAt.IsZero() {
			unit.CreatedAt = now
		}
		if unit.UpdatedAt.IsZero() {
			unit.UpdatedAt = now
		}
		units[i] = unit
	}
	for _, unit := range units {
		for _, dep := range uniqueStrings(unit.DependsOn) {
			if dep == unit.ID {
				return fmt.Errorf("work unit %s cannot depend on itself", unit.ID)
			}
			if !seen[dep] {
				return fmt.Errorf("work unit %s depends on unknown unit: %s", unit.ID, dep)
			}
		}
	}
	if err := detectWorkCycles(units); err != nil {
		return err
	}
	for _, unit := range units {
		unit.DependsOn = uniqueStrings(unit.DependsOn)
		unit.RequiredEvidence = uniqueStrings(unit.RequiredEvidence)
		unit.Verification = uniqueStrings(unit.Verification)
		m.State.Order = append(m.State.Order, unit.ID)
		m.State.Items[unit.ID] = unit
	}
	_, _ = m.EnsureActive()
	return nil
}

func (m *WorkUnitManager) EnsureActive() (*WorkUnit, error) {
	if m.State.ActiveID != "" {
		if unit, ok := m.State.Items[m.State.ActiveID]; ok && !terminalWorkStatus(unit.Status) {
			copy := unit
			return &copy, nil
		}
		m.State.ActiveID = ""
	}
	for _, id := range m.State.Order {
		unit := m.State.Items[id]
		if unit.Status != WorkPending {
			continue
		}
		ready := true
		for _, dep := range unit.DependsOn {
			if m.State.Items[dep].Status != WorkCompleted {
				ready = false
				break
			}
		}
		if ready {
			unit.Status = WorkActive
			unit.UpdatedAt = m.now().UTC()
			m.State.Items[id] = unit
			m.State.ActiveID = id
			copy := unit
			return &copy, nil
		}
	}
	return nil, nil
}

func (m *WorkUnitManager) Update(id string, patch WorkUnitPatch) (WorkUnit, error) {
	unit, ok := m.State.Items[id]
	if !ok {
		return WorkUnit{}, fmt.Errorf("unknown work unit: %s", id)
	}
	if patch.Summary != nil {
		unit.Summary = strings.TrimSpace(*patch.Summary)
	}
	if patch.Evidence != nil {
		unit.Evidence = append([]EvidenceRef(nil), (*patch.Evidence)...)
	}
	if patch.VerificationResults != nil {
		unit.VerificationResults = append([]VerificationResult(nil), (*patch.VerificationResults)...)
	}
	if patch.Status != nil && *patch.Status != unit.Status {
		if !allowedWorkTransition(unit.Status, *patch.Status) {
			return WorkUnit{}, fmt.Errorf("work unit %s cannot transition %s -> %s", id, unit.Status, *patch.Status)
		}
		if *patch.Status == WorkCompleted {
			if err := completionReady(unit); err != nil {
				return WorkUnit{}, err
			}
			now := m.now().UTC()
			unit.CompletedAt = &now
		}
		unit.Status = *patch.Status
		if terminalWorkStatus(unit.Status) && m.State.ActiveID == id {
			m.State.ActiveID = ""
		}
	}
	unit.UpdatedAt = m.now().UTC()
	m.State.Items[id] = unit
	if m.State.ActiveID == "" {
		_, _ = m.EnsureActive()
	}
	return unit, nil
}

func (m *WorkUnitManager) Incomplete() []WorkUnit {
	out := []WorkUnit{}
	for _, id := range m.State.Order {
		unit := m.State.Items[id]
		if !terminalWorkStatus(unit.Status) {
			out = append(out, unit)
		}
	}
	return out
}

func (m *WorkUnitManager) Snapshot() WorkUnitState {
	return cloneWorkUnitState(m.State)
}

type WorkUnitPatch struct {
	Status              *WorkUnitStatus
	Summary             *string
	Evidence            *[]EvidenceRef
	VerificationResults *[]VerificationResult
}

func completionReady(unit WorkUnit) error {
	evidence := map[string]bool{}
	for _, item := range unit.Evidence {
		evidence[strings.TrimSpace(item.Requirement)] = true
	}
	for _, required := range unit.RequiredEvidence {
		if !evidence[required] {
			return fmt.Errorf("work unit %s missing required evidence: %s", unit.ID, required)
		}
	}
	passed := map[string]bool{}
	for _, item := range unit.VerificationResults {
		if item.Status == "passed" {
			passed[strings.TrimSpace(item.Check)] = true
		}
	}
	for _, check := range unit.Verification {
		if !passed[check] {
			return fmt.Errorf("work unit %s missing passed verification: %s", unit.ID, check)
		}
	}
	return nil
}

func allowedWorkTransition(from, to WorkUnitStatus) bool {
	allowed := map[WorkUnitStatus]map[WorkUnitStatus]bool{
		WorkPending:   {WorkActive: true, WorkBlocked: true, WorkCancelled: true},
		WorkActive:    {WorkBlocked: true, WorkVerifying: true, WorkCompleted: true, WorkFailed: true, WorkCancelled: true},
		WorkBlocked:   {WorkPending: true, WorkActive: true, WorkFailed: true, WorkCancelled: true},
		WorkVerifying: {WorkActive: true, WorkCompleted: true, WorkFailed: true, WorkCancelled: true},
		WorkFailed:    {WorkPending: true, WorkActive: true, WorkCancelled: true},
	}
	return allowed[from][to]
}

func validWorkStatus(status WorkUnitStatus) bool {
	switch status {
	case WorkPending, WorkActive, WorkBlocked, WorkVerifying, WorkCompleted, WorkFailed, WorkCancelled:
		return true
	default:
		return false
	}
}

func terminalWorkStatus(status WorkUnitStatus) bool {
	return status == WorkCompleted || status == WorkCancelled
}

func validRisk(risk string) bool {
	switch risk {
	case "low", "medium", "high", "critical":
		return true
	default:
		return false
	}
}

func detectWorkCycles(units []WorkUnit) error {
	byID := map[string]WorkUnit{}
	for _, unit := range units {
		byID[unit.ID] = unit
	}
	visiting := map[string]bool{}
	visited := map[string]bool{}
	var visit func(string) error
	visit = func(id string) error {
		if visited[id] {
			return nil
		}
		if visiting[id] {
			return fmt.Errorf("work unit dependency cycle detected at %s", id)
		}
		visiting[id] = true
		for _, dep := range byID[id].DependsOn {
			if err := visit(dep); err != nil {
				return err
			}
		}
		delete(visiting, id)
		visited[id] = true
		return nil
	}
	for id := range byID {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func normalizeWorkUnitJSON(obj map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range obj {
		switch key {
		case "required_evidence":
			out["requiredEvidence"] = value
		case "depends_on":
			out["dependsOn"] = value
		case "verification_results":
			out["verificationResults"] = value
		default:
			out[key] = value
		}
	}
	return out
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func cloneWorkUnitState(state WorkUnitState) WorkUnitState {
	raw, _ := json.Marshal(state)
	var out WorkUnitState
	_ = json.Unmarshal(raw, &out)
	if out.Items == nil {
		out.Items = map[string]WorkUnit{}
	}
	return out
}
