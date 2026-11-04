package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeWakeups records the wake-ups a tool created.
type fakeWakeups struct {
	scheduled []string
	delays    []int
	cancelled []string
	err       error
}

func (f *fakeWakeups) ScheduleWakeup(_ context.Context, delaySec int, message string) (Wakeup, error) {
	if f.err != nil {
		return Wakeup{}, f.err
	}
	f.delays = append(f.delays, delaySec)
	f.scheduled = append(f.scheduled, message)
	return Wakeup{ID: "schedule_1", FireAt: "2026-11-01T10:00:00Z"}, nil
}

func (f *fakeWakeups) CancelWakeup(_ context.Context, id string) error {
	if f.err != nil {
		return f.err
	}
	f.cancelled = append(f.cancelled, id)
	return nil
}

func TestWakeupToolsAreRegisteredOnlyWhenBound(t *testing.T) {
	unbound, err := New(Options{Workspace: t.TempDir(), Policy: PolicyWorkspace})
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range unbound.Specs(nil) {
		if strings.HasPrefix(spec.Name, "wakeup_") {
			t.Fatalf("unbound set offers %s", spec.Name)
		}
	}
	if _, err := unbound.Execute(context.Background(), "wakeup_schedule", map[string]any{"delay_sec": 60.0, "message": "x"}, nil); err == nil {
		t.Fatal("an unbound set executed wakeup_schedule")
	}

	bound, err := New(Options{Workspace: t.TempDir(), Policy: PolicyWorkspace, Wakeups: &fakeWakeups{}})
	if err != nil {
		t.Fatal(err)
	}
	perms := map[string]string{}
	for _, spec := range bound.Specs(nil) {
		if strings.HasPrefix(spec.Name, "wakeup_") {
			perms[spec.Name] = spec.Permission
		}
	}
	// A wake-up schedules future work, so it is a write; a read-only run is offered neither tool.
	for _, name := range []string{"wakeup_schedule", "wakeup_cancel"} {
		if perms[name] != "write" {
			t.Fatalf("%s permission = %q, want write", name, perms[name])
		}
	}
	readOnly, err := New(Options{Workspace: t.TempDir(), Policy: PolicyReadOnly, Wakeups: &fakeWakeups{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range readOnly.Specs(nil) {
		if strings.HasPrefix(spec.Name, "wakeup_") {
			t.Fatalf("a read-only run was offered %s", spec.Name)
		}
	}
}

func TestWakeupScheduleTool(t *testing.T) {
	controller := &fakeWakeups{}
	set, err := New(Options{Workspace: t.TempDir(), Policy: PolicyWorkspace, Wakeups: controller})
	if err != nil {
		t.Fatal(err)
	}
	result, err := set.Execute(context.Background(), "wakeup_schedule", map[string]any{
		"delay_sec": 300.0, "message": "  check whether the build finished  ",
	}, nil)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	var out struct {
		WakeupID string `json:"wakeupId"`
		FireAt   string `json:"fireAt"`
	}
	if err := json.Unmarshal([]byte(result.Content), &out); err != nil {
		t.Fatalf("content %q: %v", result.Content, err)
	}
	if out.WakeupID != "schedule_1" || out.FireAt != "2026-11-01T10:00:00Z" {
		t.Fatalf("content = %s", result.Content)
	}
	if len(controller.delays) != 1 || controller.delays[0] != 300 {
		t.Fatalf("delays = %v", controller.delays)
	}
	if controller.scheduled[0] != "check whether the build finished" {
		t.Fatalf("message = %q, want it trimmed", controller.scheduled[0])
	}

	// Out-of-range and empty arguments are refused before the runtime is asked.
	for name, args := range map[string]map[string]any{
		"zero delay":  {"delay_sec": 0.0, "message": "x"},
		"too long":    {"delay_sec": 86401.0, "message": "x"},
		"fractional":  {"delay_sec": 1.5, "message": "x"},
		"missing":     {"message": "x"},
		"empty text":  {"delay_sec": 60.0, "message": "   "},
		"huge text":   {"delay_sec": 60.0, "message": strings.Repeat("x", MaxWakeupMessage+1)},
		"missing key": {"delay_sec": 60.0},
	} {
		if _, err := set.Execute(context.Background(), "wakeup_schedule", args, nil); err == nil {
			t.Errorf("%s: executed, want an error", name)
		}
	}
	if len(controller.delays) != 1 {
		t.Fatalf("a refused call reached the runtime: %v", controller.delays)
	}
}

func TestWakeupCancelTool(t *testing.T) {
	controller := &fakeWakeups{}
	set, err := New(Options{Workspace: t.TempDir(), Policy: PolicyWorkspace, Wakeups: controller})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.Execute(context.Background(), "wakeup_cancel", map[string]any{"wakeup_id": " schedule_1 "}, nil); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if len(controller.cancelled) != 1 || controller.cancelled[0] != "schedule_1" {
		t.Fatalf("cancelled = %v", controller.cancelled)
	}
	if _, err := set.Execute(context.Background(), "wakeup_cancel", map[string]any{}, nil); err == nil {
		t.Fatal("cancel without an id executed")
	}
	controller.err = errors.New("schedule not found")
	if _, err := set.Execute(context.Background(), "wakeup_cancel", map[string]any{"wakeup_id": "schedule_9"}, nil); err == nil {
		t.Fatal("a runtime error was swallowed")
	}
}
