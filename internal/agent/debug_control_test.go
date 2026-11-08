package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDebugBreakpointOnceConditionsAndStaleControl(t *testing.T) {
	events := make(chan DebugSnapshot, 8)
	d, err := NewDebugControl("run-a", &DebugOptions{Breakpoints: []DebugBreakpoint{{ID: "only-write", Event: "BeforeToolCall", Enabled: true, Once: true, Tool: "write_file", Agent: "worker", Conditions: map[string]string{"step": "2"}}}}, func(s DebugSnapshot) { events <- s })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = d.Boundary(ctx, DebugBoundary{Event: "BeforeToolCall", Tool: "read_file", Agent: "worker", Step: 2}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- d.Boundary(ctx, DebugBoundary{Event: "BeforeToolCall", Tool: "write_file", Agent: "worker", Step: 2})
	}()
	var s DebugSnapshot
	select {
	case s = <-events:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if s.State != "paused" || s.Reason != "only-write" || s.Breakpoints[0].Enabled {
		t.Fatalf("breakpoint %+v", s)
	}
	// Snapshot mutation is not authority.
	s.Breakpoints[0].Conditions["step"] = "99"
	if d.Snapshot().Breakpoints[0].Conditions["step"] != "2" {
		t.Fatal("caller mutated controller")
	}
	if _, err = d.Command(s.Revision, "continue", nil); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Command(s.Revision, "step_tool", nil); !errors.Is(err, ErrDebugStale) {
		t.Fatalf("stale control %v", err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if err = d.Boundary(ctx, DebugBoundary{Event: "BeforeToolCall", Tool: "write_file", Agent: "worker", Step: 2}); err != nil {
		t.Fatal("once breakpoint fired again", err)
	}
	d.Close()
	if err = d.Boundary(ctx, DebugBoundary{Event: "BeforeModelCall"}); !errors.Is(err, ErrDebugState) {
		t.Fatalf("closed controller %v", err)
	}
}

func TestDebugChildCompletionWaitsForParentBoundary(t *testing.T) {
	paused := make(chan DebugSnapshot, 4)
	d, err := NewDebugControl("parent", &DebugOptions{Breakpoints: []DebugBreakpoint{{ID:"child-done", Event:"AfterSubagentEnd", Enabled:true, Once:true}}}, func(s DebugSnapshot){ paused <- s })
	if err != nil { t.Fatal(err) }
	d.Observe(DebugBoundary{Event:"AfterSubagentEnd",Agent:"worker"})
	if d.Snapshot().State != "running" { t.Fatal("child callback suspended parent outside safe boundary") }
	ctx,cancel := context.WithTimeout(context.Background(),time.Second)
	defer cancel()
	done := make(chan error,1)
	go func(){done <- d.Boundary(ctx,DebugBoundary{Event:"BeforeModelCall"})}()
	select {
	case s := <-paused:
		if s.Reason != "child-done" || s.Boundary.Event != "BeforeModelCall" { t.Fatalf("incorrect async pause %+v",s) }
		if _,err=d.Command(s.Revision,"continue",nil);err!=nil{t.Fatal(err)}
	case <-ctx.Done(): t.Fatal(ctx.Err())
	}
	if err=<-done;err!=nil{t.Fatal(err)}
}
