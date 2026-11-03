package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/protocol"
)

func TestDeliverySurvivesResetAndResume(t *testing.T) {
	engine, id := notebookEngine(t)
	ctx := context.Background()
	d := &DeliveryContract{Files: []DeliveryFile{{Path: "result.txt", Changed: true}}}
	p := &notebookProvider{responses: []protocol.ProviderResponse{finalResponse("not yet implemented")}}
	first, err := engine.runAgentWith(ctx, id, p, AgentOptions{Policy: "workspace", Delivery: d, MaxSteps: 1})
	if err == nil || first.Status != "interrupted" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	info, err := engine.TaskState(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	baseline := info.State.Delivery.Files[0].BaseBlob
	if !slices.Contains(info.Obligations, "delivery") {
		t.Fatalf("missing obligation: %+v", info)
	}
	if _, err := engine.ResetContext(ctx, id); err != nil {
		t.Fatal(err)
	}
	p = &notebookProvider{responses: []protocol.ProviderResponse{finalResponse("still not implemented"), finalResponse("still not implemented"), finalResponse("still not implemented")}}
	second, secondErr := engine.runAgentWith(ctx, id, p, AgentOptions{Policy: "workspace", Input: "Continue the requested delivery.", MaxSteps: 12})
	if second.Status != "interrupted" || second.Spend.ModelRequests != 3 {
		t.Fatalf("bounded continuation=%+v err=%v", second, secondErr)
	}
	info, err = engine.TaskState(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Delivery.Files[0].BaseBlob != baseline {
		t.Fatal("resume replaced baseline")
	}
	// A question ends its turn but cannot mark the objective done.
	p = &notebookProvider{responses: []protocol.ProviderResponse{finalResponse("No result yet.")}}
	turn, err := engine.runAgentWith(ctx, id, p, AgentOptions{Interactive: true, MaxSteps: 2})
	if err != nil || turn.Status != "completed" {
		t.Fatalf("conversation=%+v %v", turn, err)
	}
	workspace, err := engine.agentWorkspace(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	p = &notebookProvider{responses: []protocol.ProviderResponse{
		toolCallResponse("deliver", "write_file", `{"path":"result.txt","content":"implemented\n"}`),
		finalResponse("Delivered result.txt."),
	}}
	final, err := engine.runAgentWith(ctx, id, p, AgentOptions{Policy: "workspace", MaxSteps: 3})
	if err != nil || final.Status != "completed" {
		t.Fatalf("delivery=%+v %v", final, err)
	}
	info, err = engine.TaskState(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(info.Obligations, "delivery") {
		t.Fatalf("delivered file still blocked: %+v", info)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "result.txt"))
	if err != nil || string(content) != "implemented\n" {
		t.Fatalf("actual delivered content %q: %v", content, err)
	}
}

func TestDeliveryCommitAndBranchAreExplicit(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
	}
	git("init", "-b", "task")
	git("config", "user.email", "fixture@example.invalid")
	git("config", "user.name", "fixture")
	file := filepath.Join(dir, "result.txt")
	if err := os.WriteFile(file, []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "result.txt")
	git("commit", "-m", "baseline")
	repo, err := repository.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	d := &DeliveryContract{Files: []DeliveryFile{{Path: "result.txt", Changed: true}}, Branch: "task", Commit: true}
	if err := repo.CaptureDelivery(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	check := func(want bool) {
		t.Helper()
		missing, err := repo.DeliveryMissing(context.Background(), d)
		if err != nil || (len(missing) == 0) != want {
			t.Fatalf("want satisfied=%t got %v %v", want, missing, err)
		}
	}
	check(false)
	if err := os.WriteFile(file, []byte("after\n"), 0600); err != nil {
		t.Fatal(err)
	}
	check(false) // dirty changes are not a commit
	git("commit", "--allow-empty", "-m", "empty")
	check(false) // moving HEAD without a patch is not delivery
	git("add", "result.txt")
	git("commit", "-m", "deliver")
	check(true)
	git("switch", "-c", "wrong")
	check(false)
	git("switch", "task")
	check(true)
	git("update-index", "--assume-unchanged", "result.txt")
	if err := os.WriteFile(file, []byte("hidden dirty\n"), 0600); err != nil {
		t.Fatal(err)
	}
	check(false) // index flags cannot hide a mismatching delivered blob
	if err := os.WriteFile(file, []byte("after\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("update-index", "--no-assume-unchanged", "result.txt")
	check(true)
	listedFiles := d.Files
	d.Files = nil
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		git("update-index", flag, "result.txt")
		if err := os.WriteFile(file, []byte("hidden whole-workspace change\n"), 0600); err != nil {
			t.Fatal(err)
		}
		check(false)
		if err := os.WriteFile(file, []byte("after\n"), 0600); err != nil {
			t.Fatal(err)
		}
		git("update-index", "--no-assume-unchanged", "--no-skip-worktree", "result.txt")
		check(true)
	}
	d.Files = listedFiles
	// Without commit selected, an actual file change can be delivered dirty.
	d.Commit = false
	if err := os.WriteFile(file, []byte("later\n"), 0600); err != nil {
		t.Fatal(err)
	}
	check(true)
}
