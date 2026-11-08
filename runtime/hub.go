package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/internal/taskstate"
)

// HubTaskSpec seeds the existing work-unit DAG and a normal execution proposal.
// Success, verification and worker identity are never accepted from the caller.
type HubTaskSpec struct {
	ID        string   `json:"id"`
	Goal      string   `json:"goal"`
	DependsOn []string `json:"dependsOn,omitempty"`
	Agent     string   `json:"agent,omitempty"`
}
type HubTask struct {
	ID           string   `json:"id"`
	TaskID       string   `json:"taskId,omitempty"`
	Goal         string   `json:"goal"`
	DependsOn    []string `json:"dependsOn,omitempty"`
	State        string   `json:"state"`
	WorkerID     string   `json:"workerId,omitempty"`
	Agent        string   `json:"agent,omitempty"`
	Verification string   `json:"verification,omitempty"`
	Error        string   `json:"error,omitempty"`
}
type HubSnapshot struct {
	SessionID string         `json:"sessionId"`
	Tasks     []HubTask      `json:"tasks"`
	Workers   []SubagentNode `json:"workers"`
}

// ConfigureHub initializes one task graph while its root is idle. Assignment is
// immutable after creation; standard proposed-task launch owns worktree creation.
func (e *Engine) ConfigureHub(ctx context.Context, root string, specs []HubTaskSpec) error {
	if err := e.runtimeEventAccess(ctx, root); err != nil {
		return err
	}
	if len(specs) == 0 || len(specs) > taskstate.MaxTasks {
		return errors.New("hub needs 1..100 tasks")
	}
	lock, err := e.acquireRun(ctx, root)
	if err != nil {
		return err
	}
	defer lock.Release()
	state, err := e.loadTaskState(ctx, root)
	if err != nil {
		return err
	}
	if len(state.Tasks) != 0 || (state.WorkUnits != nil && len(state.WorkUnits.Order) != 0) {
		return errors.New("hub task graph already initialized")
	}
	before := state.Clone()
	units := make([]taskstate.WorkUnit, 0, len(specs))
	seen := map[string]bool{}
	for _, spec := range specs {
		if spec.ID == "" || len(spec.ID) > 128 || seen[spec.ID] {
			return errors.New("hub task IDs must be unique and bounded")
		}
		seen[spec.ID] = true
		goal, err := taskstate.ValidTaskObjective(spec.Goal)
		if err != nil {
			return err
		}
		who := spec.Agent
		if who == "" {
			who = config.AgentWorker
		}
		if err = e.delegationAgent(who, config.AgentInfo.TaskEligible); err != nil {
			return err
		}
		task, err := state.ProposeTask(goal, who, nil, time.Now().UTC())
		if err != nil {
			return err
		}
		state.Tasks[len(state.Tasks)-1].WorkUnitID = spec.ID
		units = append(units, taskstate.WorkUnit{ID: spec.ID, Goal: task.Objective, DependsOn: spec.DependsOn})
	}
	manager := taskstate.NewWorkUnitManager(nil)
	if err = manager.Seed(units); err != nil {
		return err
	}
	graph := manager.Snapshot()
	state.WorkUnits = &graph
	_, err = e.putTaskState(ctx, root, taskstate.ReasonUser, state, 0, agent.HostStateEdges(before, state))
	return err
}

func (e *Engine) hubTasks(ctx context.Context, root string, state taskstate.State) ([]HubTask, error) {
	out := []HubTask{}
	byUnit := make(map[string]taskstate.Task, len(state.Tasks))
	for _, task := range state.Tasks {
		if task.WorkUnitID != "" {
			byUnit[task.WorkUnitID] = task
		}
	}
	for _, unit := range state.Units().List() {
		view := HubTask{ID: unit.ID, Goal: unit.Goal, DependsOn: append([]string(nil), unit.DependsOn...), State: "pending"}
		task, linked := byUnit[unit.ID]
		if !linked {
			view.State = string(unit.Status)
			if unit.Status == taskstate.WorkActive {
				view.State = "running"
			}
			out = append(out, view)
			continue
		}
		view.TaskID, view.WorkerID, view.Agent = task.ID, task.SessionID, task.Agent
		if task.SessionID != "" {
			if err := e.runtimeEventAccess(ctx, task.SessionID); err != nil {
				view.State = "blocked"
				view.Goal = ""
				view.Error = "worker source unavailable"
				out = append(out, view)
				continue
			}
			relation, err := e.store.ParentRelation(ctx, task.SessionID)
			if err != nil {
				return nil, err
			}
			if relation.ParentSessionID != root || relation.Kind != RelationExecution {
				return nil, errors.New("invalid Hub worker relation")
			}
			child, err := e.store.Get(ctx, task.SessionID)
			if err != nil {
				return nil, err
			}
			switch child.Status {
			case "created":
				view.State = "assigned"
			case "running":
				view.State = "running"
			case "failed":
				view.State = "failed"
			case "interrupted":
				view.State = "cancelled"
			case "completed":
				result, err := e.taskResult(ctx, state, relation, child)
				if err != nil {
					return nil, err
				}
				view.State = "completed"
				if len(result.OpenUnits) > 0 || result.ChecksError != "" {
					view.State = "blocked"
					view.Error = "worker work or verification remains unresolved"
				}
				for _, check := range result.Checks {
					if check.Required && !check.Satisfied {
						view.State = "blocked"
						view.Error = "required worker check has not passed"
					}
				}
				childState, err := e.loadTaskState(ctx, task.SessionID)
				if err != nil {
					return nil, err
				}
				for _, u := range childState.Units().List() {
					if u.Status != taskstate.WorkCompleted {
						view.State = "blocked"
						view.Error = "worker unit did not complete"
					}
				}
				if childState.Verification != nil {
					view.Verification = childState.Verification.Status
					if view.Verification != "passed" {
						view.State = "blocked"
						view.Error = "worker verification did not pass"
					}
				}
				if result.Delivery != nil && result.Delivery.Status != DeliveryVerified {
					view.State = "blocked"
					view.Error = "worker delivery is not verified"
				}
			default:
				view.State = "blocked"
			}
		} else if unit.Status == taskstate.WorkCancelled {
			view.State = "cancelled"
		} else if unit.Status == taskstate.WorkFailed {
			view.State = "failed"
		}
		out = append(out, view)
	}
	index := make(map[string]string, len(out))
	for _, view := range out {
		index[view.ID] = view.State
	}
	for i := range out {
		if out[i].State != "pending" {
			continue
		}
		out[i].State = "ready"
		for _, dep := range out[i].DependsOn {
			if index[dep] != "completed" {
				out[i].State = "pending"
				if index[dep] == "failed" || index[dep] == "cancelled" || index[dep] == "blocked" {
					out[i].State = "blocked"
				}
				break
			}
		}
	}
	return out, nil
}
func (s *RunSupervisor) Hub(ctx context.Context, root string) (HubSnapshot, error) {
	out := HubSnapshot{SessionID: root}
	if s == nil || s.engine == nil {
		return out, ErrRunSupervisorClosed
	}
	if err := s.engine.runtimeEventAccess(ctx, root); err != nil {
		return out, err
	}
	state, err := s.engine.loadTaskState(ctx, root)
	if err != nil {
		return out, err
	}
	out.Tasks, err = s.engine.hubTasks(ctx, root, state)
	if err != nil {
		return out, err
	}
	out.Workers, err = s.SubagentTree(ctx, root)
	return out, err
}
func (e *Engine) admitHubTask(ctx context.Context, root string, state taskstate.State, task taskstate.Task) error {
	if task.WorkUnitID == "" {
		return nil
	}
	views, err := e.hubTasks(ctx, root, state)
	if err != nil {
		return err
	}
	for _, view := range views {
		if view.ID != task.WorkUnitID {
			continue
		}
		if view.State == "cancelled" || view.State == "failed" {
			return errors.New("Hub task is terminal")
		}
		for _, dep := range view.DependsOn {
			complete := false
			for _, candidate := range views {
				if candidate.ID == dep && candidate.State == "completed" {
					complete = true
					break
				}
			}
			if !complete {
				return fmt.Errorf("Hub task %s waits for verified dependency %s", view.ID, dep)
			}
		}
		return nil
	}
	return errors.New("Hub assignment has no work unit")
}

func (e *Engine) publishHubResult(root, worker string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	state, err := e.loadTaskState(ctx, root)
	if err != nil {
		e.events.publish("worker.task.blocked", root, map[string]any{"workerId": worker, "status": "unavailable"})
		return
	}
	views, err := e.hubTasks(ctx, root, state)
	if err != nil {
		e.events.publish("worker.task.blocked", root, map[string]any{"workerId": worker, "status": "unavailable"})
		return
	}
	for _, view := range views {
		if view.WorkerID != worker {
			continue
		}
		kind := "worker.task.blocked"
		if view.State == "completed" {
			kind = "worker.task.completed"
		}
		data := map[string]any{"workerId": worker, "taskId": view.TaskID, "status": view.State}
		e.events.publish(kind, root, data)
		e.events.publish("worker.result.ready", root, data)
		return
	}
}
