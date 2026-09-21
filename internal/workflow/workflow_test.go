package workflow

import (
	"encoding/json"
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

func TestVerifiedCodeFixParity(t *testing.T) {
	def, err := Parse([]byte(verifiedCodeFix))
	if err != nil {
		t.Fatal(err)
	}
	rt, err := New(def, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rt.CurrentAction != "diagnose" || !rt.IsToolAllowed("shell") || rt.IsToolAllowed("apply_patch") {
		t.Fatalf("unexpected diagnose state: %#v", rt.Summary())
	}

	_, err = rt.ObserveTool(ToolObservation{
		Tool:   "shell",
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
		Tool: "shell", Args: map[string]any{"command": "go test ./..."},
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
