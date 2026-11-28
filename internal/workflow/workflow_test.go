package workflow

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const verifiedCodeFix = `{
  "version": 1,
  "id": "verified-code-fix",
  "title": "Verified code repair",
  "entry": "diagnose",
  "facts": {
    "tests": { "failed": false, "passed": false },
    "implementation": { "changed": false }
  },
  "actions": {
    "diagnose": {
      "allowedTools": ["read_file", "shell"],
      "outcomes": [{
        "id": "baseline-fails",
        "when": { "all": [
          { "tool": "shell" },
          { "arg": "command", "contains": "test" },
          { "result": "exitCode", "notEquals": 0 }
        ] },
        "set": { "tests.failed": true }
      }],
      "routes": [{ "to": "repair", "when": { "fact": "tests.failed", "equals": true } }]
    },
    "repair": {
      "allowedTools": ["read_file", "apply_patch"],
      "outcomes": [{
        "when": { "tool": "apply_patch", "ok": true },
        "set": { "implementation.changed": true }
      }],
      "completeWhen": { "fact": "implementation.changed", "equals": true },
      "routes": [{ "to": "verify" }]
    },
    "verify": {
      "terminal": true,
      "allowedTools": ["read_file", "shell"],
      "outcomes": [{
        "id": "tests-pass",
        "when": { "all": [
          { "tool": "shell" },
          { "arg": "command", "contains": "test" },
          { "result": "exitCode", "equals": 0 }
        ] },
        "set": { "tests.passed": true }
      }],
      "completeWhen": { "fact": "tests.passed", "equals": true }
    }
  }
}`

// The definition names the command tool by its pre-ADR-0019 name "shell": it admits "bash" (and no
// longer "shell") and its tool conditions match bash calls, so a workflow written before the rename keeps
// working without being edited.
func TestVerifiedCodeFixParity(t *testing.T) {
	def, err := Parse([]byte(verifiedCodeFix))
	if err != nil {
		t.Fatal(err)
	}
	rt, err := New(def, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rt.CurrentAction != "diagnose" || !rt.IsToolAllowed("bash") || rt.IsToolAllowed("shell") || rt.IsToolAllowed("apply_patch") {
		t.Fatalf("unexpected diagnose state: %#v", rt.Summary())
	}

	_, err = rt.ObserveTool(ToolObservation{
		Tool:   "bash",
		Args:   map[string]any{"command": "go test ./..."},
		Result: map[string]any{"ok": false, "content": `{"exitCode":1}`},
		Step:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rt.CurrentAction != "repair" {
		t.Fatalf("action=%s", rt.CurrentAction)
	}

	_, err = rt.ObserveTool(ToolObservation{Tool: "apply_patch", Result: map[string]any{"ok": true}, Step: 2})
	if err != nil {
		t.Fatal(err)
	}
	if rt.CurrentAction != "verify" {
		t.Fatalf("action=%s", rt.CurrentAction)
	}

	_, err = rt.ObserveTool(ToolObservation{
		Tool: "bash", Args: map[string]any{"command": "go test ./..."},
		Result: map[string]any{"ok": true, "content": `{"exitCode":0}`}, Step: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rt.Status != "ready_to_finish" || !rt.CanFinish() {
		t.Fatalf("summary=%#v", rt.Summary())
	}
	passed, _ := getPath(rt.Facts, "tests.passed")
	if passed != true {
		t.Fatalf("tests.passed=%v", passed)
	}
}

func TestEffectiveAllowlistPreservesDenyAll(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action string
		base   []string
		want   []string
	}{
		{name: "unrestricted", action: "null"},
		{name: "base_deny_all", action: "null", base: []string{}, want: []string{}},
		{name: "action_deny_all", action: "[]", want: []string{}},
		{name: "base_only", action: "null", base: []string{"read_file"}, want: []string{"read_file"}},
		{name: "action_only", action: `["read_file"]`, want: []string{"read_file"}},
		{name: "disjoint", action: `["write_file"]`, base: []string{"read_file"}, want: []string{}},
		{name: "intersection", action: `["read_file","write_file"]`, base: []string{"read_file", "recall"}, want: []string{"read_file"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def, err := Parse([]byte(`{"id":"allowlist","entry":"act","actions":{"act":{"terminal":true,"allowedTools":` + tc.action + `}}}`))
			if err != nil {
				t.Fatal(err)
			}
			rt, err := New(def, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := rt.EffectiveAllowlist(tc.base); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("permission set = %#v, want %#v (nil is unrestricted; empty denies all)", got, tc.want)
			}
		})
	}
}

func TestHumanGateWithFalseEqualsAndSnapshotRestore(t *testing.T) {
	raw := []byte(`{
      "id":"approval",
      "facts":{"ready":true},
      "actions":{"done":{"terminal":true,"completeWhen":{"fact":"ready","equals":true},
        "gates":[{"id":"deny","type":"human","fact":"approved","equals":false}]}}
    }`)
	def, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !def.Actions["done"].Gates[0].HasEquals {
		t.Fatal("equals presence lost")
	}
	rt, err := New(def, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Status != "waiting_gate" {
		t.Fatalf("status=%s", rt.Status)
	}
	if _, err := rt.Approve("deny", "tester"); err != nil {
		t.Fatal(err)
	}
	if !rt.CanFinish() {
		t.Fatalf("summary=%#v", rt.Summary())
	}

	snap := rt.Snapshot()
	restored, err := New(snap.Definition, &State{CurrentAction: snap.CurrentAction, Facts: snap.Facts, FactSources: snap.FactSources, History: snap.History, Status: snap.Status})
	if err != nil {
		t.Fatal(err)
	}
	if !restored.CanFinish() {
		t.Fatal("restored runtime lost completion state")
	}
}

// A human gate that does not state "equals" waits for true (the gate's fact set by an approval). It must
// still wait after Validate's clone, after a Snapshot is persisted as JSON and restored, and after the
// runtime is restored from that snapshot; only Approve opens it.
func TestHumanGateWithoutEqualsWaitsThroughCloneAndRestore(t *testing.T) {
	def, err := Parse([]byte(`{
      "id":"approval",
      "facts":{"ready":true},
      "actions":{"done":{"terminal":true,"completeWhen":{"fact":"ready","equals":true},
        "gates":[{"id":"apply","type":"human","fact":"approved"}]}}
    }`))
	if err != nil {
		t.Fatal(err)
	}
	if def.Actions["done"].Gates[0].HasEquals {
		t.Fatal("a gate without equals reads back as one that states it")
	}
	rt, err := New(def, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Status != "waiting_gate" || len(rt.WaitingHumanGates()) != 1 || rt.CanFinish() {
		t.Fatalf("fresh runtime does not wait at the gate: %#v", rt.Summary())
	}

	stored, err := json.Marshal(rt.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var snap Snapshot
	if err := json.Unmarshal(stored, &snap); err != nil {
		t.Fatal(err)
	}
	restored, err := New(snap.Definition, &State{CurrentAction: snap.CurrentAction, Facts: snap.Facts, FactSources: snap.FactSources, History: snap.History, Status: snap.Status})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Status != "waiting_gate" || len(restored.WaitingHumanGates()) != 1 {
		t.Fatalf("restored runtime does not wait at the gate: %#v (stored %s)", restored.Summary(), stored)
	}
	if _, err := restored.Approve("apply", "tester"); err != nil {
		t.Fatal(err)
	}
	if approved, _ := getPath(restored.Facts, "approved"); approved != true || !restored.CanFinish() {
		t.Fatalf("approved=%v summary=%#v", approved, restored.Summary())
	}
}

func TestUnsafePathRejected(t *testing.T) {
	_, err := Parse([]byte(`{"id":"unsafe","actions":{"a":{"terminal":true,"completeWhen":{"fact":"__proto__.x","exists":true}}}}`))
	if err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("err=%v", err)
	}
}

func TestAutomaticCycleFailsClosed(t *testing.T) {
	def, err := Parse([]byte(`{"id":"cycle","actions":{"a":{"routes":[{"to":"b"}]},"b":{"routes":[{"to":"a"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(def, nil)
	if err == nil || !strings.Contains(err.Error(), "transition limit") {
		t.Fatalf("err=%v", err)
	}
}

func TestConditionComparators(t *testing.T) {
	ctx := EvalContext{Facts: map[string]any{"n": 5.0, "tags": []any{"a", "b"}}}
	cases := []string{
		`{"fact":"n","gte":5}`,
		`{"fact":"tags","contains":"b"}`,
		`{"fact":"n","in":[4,5,6]}`,
		`{"fact":"n","notEquals":4}`,
	}
	for _, raw := range cases {
		var cond any
		if err := json.Unmarshal([]byte(raw), &cond); err != nil {
			t.Fatal(err)
		}
		ok, err := Evaluate(cond, ctx)
		if err != nil || !ok {
			t.Fatalf("%s => %v %v", raw, ok, err)
		}
	}
}
