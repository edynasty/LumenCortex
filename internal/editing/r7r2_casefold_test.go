package editing

import "testing"

// TestR7R2CaseCollisionFollowsFilesystemSemantics pins the decision to the volume, not to a global
// lowercase rule: with case folding reported off, distinct spellings are distinct targets.
func TestR7R2CaseCollisionFollowsFilesystemSemantics(t *testing.T) {
	root := t.TempDir()
	e := newTestEngine(t, root)
	up, low := []byte("UPPER\n"), []byte("lower\n")
	changes := []FileChange{
		{Path: "N.txt", Content: &up, Create: true},
		{Path: "n.txt", Content: &low, Create: true},
	}
	prep := make([]*prepared, len(changes))
	for i, change := range changes {
		abs, rel, err := e.host.Resolve(change.Path, true)
		if err != nil {
			t.Fatal(err)
		}
		prep[i] = &prepared{change: change, abs: abs, rel: rel, key: canonicalPath(abs)}
	}
	e.foldsCase = func(string) bool { return false }
	if later, _ := e.duplicateTarget(prep); later != -1 {
		t.Fatalf("case-sensitive volume treated distinct names as one target (change %d)", later+1)
	}
	e.foldsCase = func(string) bool { return true }
	if later, earlier := e.duplicateTarget(prep); later != 1 || earlier != 0 {
		t.Fatalf("case-folding volume missed the collision: later=%d earlier=%d", later, earlier)
	}
}

// TestR7R2CaseProbeReadsTheVolume checks the engine's read-only probe against a created spelling.
func TestR7R2CaseProbeReadsTheVolume(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "probe-case.txt", "x\n")
	if got, want := probeFoldsCase(root), caseFolds(t, root); got != want {
		t.Fatalf("probeFoldsCase=%v, volume folds case=%v", got, want)
	}
}
