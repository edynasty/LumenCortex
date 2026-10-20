package taskstate

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Execution tasks (ADR-0006 D4, C2): a task the thread proposed waits for the user's Start; a started
// task is a child session with relation "execution". Its results reach the thread as task_result rows;
// TaskResults is the delivery cursor.
const (
	TaskProposed = "proposed"
	TaskStarted  = "started"
)

const (
	// MaxTaskObjectiveBytes bounds a task's objective, which becomes the child's goal.
	MaxTaskObjectiveBytes = 16 << 10
	// MaxTasks bounds the tasks one thread records.
	MaxTasks = 100
)

var (
	ErrTaskNotFound       = errors.New("execution task proposal not found")
	ErrTaskAlreadyStarted = errors.New("execution task proposal was already started")
	ErrTooManyTasks       = fmt.Errorf("a thread records at most %d execution tasks", MaxTasks)
)

// Task is one execution task of the thread.
type Task struct {
	ID        string `json:"id"`
	Objective string `json:"objective"`
	State     string `json:"state"`
	// Agent is the catalog agent the child runs (ADR-0009 A3), "" in records written before it.
	Agent string `json:"agent,omitempty"`
	// SessionID is the child session, once started.
	SessionID  string     `json:"sessionId,omitempty"`
	ProposedAt *time.Time `json:"proposedAt,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	// Bounds are the proposing run's step, output and tool bounds: the user's later Start of a
	// proposal applies them, as a directly started task inherits them from its parent run.
	Bounds *TaskBounds `json:"bounds,omitempty"`
}

// TaskBounds are the run bounds a proposed task inherits from the run that proposed it.
type TaskBounds struct {
	MaxSteps            int      `json:"maxSteps,omitempty"`
	MaxToolCallsPerStep int      `json:"maxToolCallsPerStep,omitempty"`
	MaxTokens           int      `json:"maxTokens,omitempty"`
	ToolDenylist        []string `json:"toolDenylist,omitempty"`
}

// TaskMark identifies one terminal transition of a child: its newest transcript seq and its status.
// A child's result is delivered when its mark differs from the stored one, so each transition is
// delivered once and a resumed child that ends again is delivered again.
type TaskMark struct {
	Seq    int64  `json:"seq"`
	Status string `json:"status"`
}

// ValidTaskObjective trims and checks an objective.
func ValidTaskObjective(objective string) (string, error) {
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return "", errors.New("task objective is required")
	}
	if len(objective) > MaxTaskObjectiveBytes {
		return "", fmt.Errorf("task objective exceeds %d bytes", MaxTaskObjectiveBytes)
	}
	return objective, nil
}

// ProposeTask records a task that waits for the user's Start, with the agent it will run and the
// proposing run's bounds.
func (s *State) ProposeTask(objective, agent string, bounds *TaskBounds, at time.Time) (Task, error) {
	return s.addTask(objective, agent, TaskProposed, "", bounds, at)
}

// AddStartedTask records a task that was started directly (delegation isolated).
func (s *State) AddStartedTask(objective, agent, sessionID string, at time.Time) (Task, error) {
	return s.addTask(objective, agent, TaskStarted, sessionID, nil, at)
}

func (s *State) addTask(objective, agent, state, sessionID string, bounds *TaskBounds, at time.Time) (Task, error) {
	objective, err := ValidTaskObjective(objective)
	if err != nil {
		return Task{}, err
	}
	if len(s.Tasks) >= MaxTasks {
		return Task{}, ErrTooManyTasks
	}
	at = at.UTC()
	t := Task{ID: fmt.Sprintf("t%d", len(s.Tasks)+1), Objective: objective, Agent: agent, State: state, SessionID: sessionID, Bounds: bounds}
	if state == TaskProposed {
		t.ProposedAt = &at
	} else {
		t.StartedAt = &at
	}
	s.Tasks = append(s.Tasks, t)
	return t, nil
}

// FindTask returns the task with id.
func (s State) FindTask(id string) (Task, bool) {
	for _, t := range s.Tasks {
		if t.ID == id {
			return t, true
		}
	}
	return Task{}, false
}

// StartTask marks a proposed task started as sessionID. An unknown id or a task that is not proposed
// is refused.
func (s *State) StartTask(id, sessionID string, at time.Time) error {
	for i := range s.Tasks {
		if s.Tasks[i].ID != id {
			continue
		}
		if s.Tasks[i].State != TaskProposed {
			return ErrTaskAlreadyStarted
		}
		at = at.UTC()
		s.Tasks[i].State, s.Tasks[i].SessionID, s.Tasks[i].StartedAt = TaskStarted, sessionID, &at
		return nil
	}
	return ErrTaskNotFound
}

// MarkDelivered advances the delivery cursor of child.
func (s *State) MarkDelivered(child string, mark TaskMark) {
	if s.TaskResults == nil {
		s.TaskResults = map[string]TaskMark{}
	}
	s.TaskResults[child] = mark
}

// Delivered reports whether mark of child was already delivered.
func (s State) Delivered(child string, mark TaskMark) bool {
	stored, ok := s.TaskResults[child]
	return ok && stored == mark
}
