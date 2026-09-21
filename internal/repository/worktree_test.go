package repository

import "testing"

func TestParseWorktrees(t *testing.T) {
	raw := "worktree /repo\nHEAD abc123\nbranch refs/heads/main\n\nworktree /repo-wt\nHEAD def456\nbranch refs/heads/lumencortex/task\nlocked reason\n"
	items := parseWorktrees(raw)
	if len(items) != 2 {
		t.Fatalf("items=%#v", items)
	}
	if items[0].Path != "/repo" || items[0].Branch != "main" {
		t.Fatalf("first=%#v", items[0])
	}
	if items[1].Branch != "lumencortex/task" || !items[1].Locked {
		t.Fatalf("second=%#v", items[1])
	}
}
