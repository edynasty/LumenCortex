package taskstate

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/internal/workflow"
)

// MaxBlockBytes bounds the Task State block; the objective takes at most MaxBriefBytes of it.
const MaxBlockBytes = 8192

const maxFactsBytes = 1024

// BlockHeader starts every Task State block, so a reader of a request can tell it from a user row.
const BlockHeader = "[Task State — maintained by the LumenCortex runtime from durable session state; it is not a new user message.]"

// Render builds the Task State block: one user message placed after the system prompt in every
// request. It is deterministic and at most MaxBlockBytes. It shows the objective (the brief, else the
// goal), the references the user included, the workflow, the open work units, and a repeated-failure
// line once progress records the same failure twice. Completed units are counted, not listed. wf is the
// state's workflow runtime, nil when there is none.
func Render(goal string, s State, wf *workflow.Runtime) string {
	var b strings.Builder
	b.WriteString(BlockHeader)
	b.WriteString("\n\nObjective:\n")
	b.WriteString(truncate(strings.TrimSpace(s.Objective(goal)), MaxBriefBytes))
	b.WriteString("\n")

	var included []Reference
	for _, r := range s.References {
		if r.State == RefIncluded {
			included = append(included, r)
		}
	}
	if len(included) > 0 {
		b.WriteString("\nReferences the user included (read them with tools when relevant):\n")
		for _, r := range included {
			fmt.Fprintf(&b, "- %s %s", r.Kind, r.Target)
			if r.Note != "" {
				fmt.Fprintf(&b, " — %s", r.Note)
			}
			b.WriteString("\n")
		}
	}

	if wf != nil {
		renderWorkflow(&b, wf)
	}

	if s.WorkUnits != nil {
		renderUnits(&b, s.Units())
	}

	if len(s.Tasks) > 0 {
		renderTasks(&b, s.Tasks)
	}

	if s.Progress.SameFailureCount >= 2 {
		fmt.Fprintf(&b, "\nRepeated failure: the same tool failure has occurred %d times. Change the approach instead of repeating it.\n", s.Progress.SameFailureCount)
	}
	return truncate(b.String(), MaxBlockBytes)
}

func renderWorkflow(b *strings.Builder, wf *workflow.Runtime) {
	action := wf.Action()
	fmt.Fprintf(b, "\nWorkflow contract %q is active and binding.\n", wf.Definition.Title)
	fmt.Fprintf(b, "Current action: %s", wf.CurrentAction)
	if action.Title != "" {
		fmt.Fprintf(b, " — %s", action.Title)
	}
	if action.Terminal {
		b.WriteString(" (terminal)")
	}
	b.WriteString("\n")
	if action.Description != "" {
		fmt.Fprintf(b, "%s\n", truncate(action.Description, 1024))
	}
	if action.AllowedTools != nil {
		tools := append([]string(nil), (*action.AllowedTools)...)
		sort.Strings(tools)
		fmt.Fprintf(b, "Allowed tools: %s\n", strings.Join(tools, ", "))
	}
	for _, gate := range wf.PendingGates() {
		fmt.Fprintf(b, "Pending gate: %s (%s)\n", gate.ID, gate.Type)
	}
	if wf.CanFinish() {
		b.WriteString("Completion: the workflow can finish.\n")
	} else {
		fmt.Fprintf(b, "Completion: %s.\n", wf.BlockReason())
	}
	if len(wf.Facts) > 0 {
		raw, _ := json.Marshal(wf.Facts) // map keys are sorted
		fmt.Fprintf(b, "Facts: %s\n", truncate(string(raw), maxFactsBytes))
	}
}

func renderUnits(b *strings.Builder, m *WorkUnitManager) {
	counts := map[WorkUnitStatus]int{}
	var open []WorkUnit
	for _, u := range m.List() {
		counts[u.Status]++
		if !terminalWorkStatus(u.Status) {
			open = append(open, u)
		}
	}
	fmt.Fprintf(b, "\nWork units: %d open, %d completed, %d cancelled.\n", len(open), counts[WorkCompleted], counts[WorkCancelled])
	active := m.Current()
	for _, u := range open {
		marker := "-"
		if active != nil && active.ID == u.ID {
			marker = "*"
		}
		fmt.Fprintf(b, "%s %s [%s/%s] %s", marker, u.ID, u.Status, u.Risk, truncate(u.Goal, 512))
		if len(u.Checks) > 0 {
			status := map[string]string{}
			for _, r := range u.CheckResults {
				status[r.Check] = r.Status
			}
			parts := make([]string, 0, len(u.Checks))
			for _, check := range u.Checks {
				s := status[check]
				if s == "" {
					s = "pending"
				}
				parts = append(parts, check+": "+s)
			}
			fmt.Fprintf(b, " (checks: %s)", strings.Join(parts, ", "))
		}
		b.WriteString("\n")
	}
	if len(open) > 0 {
		b.WriteString("Use work_unit_update to record evidence and complete units; completing a unit runs its checks. The active unit is marked *.\n")
	}
}

// maxRenderedTasks bounds the execution tasks the block lists: the newest ones, in order.
const maxRenderedTasks = 10

func renderTasks(b *strings.Builder, tasks []Task) {
	fmt.Fprintf(b, "\nExecution tasks: %d.\n", len(tasks))
	start := max(0, len(tasks)-maxRenderedTasks)
	for _, t := range tasks[start:] {
		state := "proposed, waiting for the user to start it"
		if t.State == TaskStarted {
			state = "started as " + t.SessionID + "; its result arrives as a task_result row"
		}
		fmt.Fprintf(b, "- %s (%s): %s\n", t.ID, state, truncate(t.Objective, 256))
	}
}

// truncate cuts s to at most max bytes at a rune boundary, marking the cut.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const mark = "…[truncated]"
	cut := max - len(mark)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + mark
}
