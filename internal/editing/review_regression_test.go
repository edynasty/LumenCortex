package editing

import (
 "context"
 "testing"
)

func TestReviewDeletePreservesChangedFile(t *testing.T) {
	for _, when := range []string{"before", "validation", "write"} {
		t.Run(when, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, "a", "original")
			e := newTestEngine(t, root)
			change := func() { writeFile(t, root, "a", "new user content") }
			plan := Plan{Actor: testActor(), Changes: []FileChange{{Path: "a", Delete: true, BaseRev: RevOf([]byte("original"))}}}
			switch when {
			case "before":
				change()
			case "validation":
				plan.Validate = []Validator{func(map[string][]byte) []Diagnostic { change(); return nil }}
			case "write":
				e.beforeWrite = func(string) { change() }
			}
			if _, err := e.Commit(context.Background(), plan); err == nil {
				t.Error("delete accepted a changed revision")
			}
			if got := readFileContent(t, root, "a"); got != "new user content" {
				t.Fatalf("new content lost: %q", got)
			}
		})
	}
}
