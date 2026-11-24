//go:build darwin

package editing

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestOpusKillChild(t *testing.T) {
	mode, root := os.Getenv("LC_OPUS_KILL_MODE"), os.Getenv("LC_OPUS_KILL_ROOT")
	if mode == "" {
		return
	}
	block := func(string) {
		_ = os.WriteFile(filepath.Join(root, "..", "marker"), []byte("1"), 0o644)
		select {}
	}
	e := newTestEngine(t, root)
	orig := []byte("original-content\n")
	e.RegisterRead(testActor(), filepath.Join(root, "f.txt"), RevOf(orig))
	h := PublishFaultHooks{}
	ch := FileChange{Path: "f.txt", BaseRev: RevOf(orig)}
	next := []byte("new-content\n")
	switch mode {
	case "update_before_swap":
		h.BeforePublish = func(d string) error { block(d); return nil }
		ch.Content = &next
	case "update_after_swap":
		h.AfterInPlaceWriteClose = func(d string) error { block(d); return nil }
		ch.Content = &next
	case "delete_before_quarantine":
		h.BeforeDeleteUnlink = func(d string) error { block(d); return nil }
		ch.Delete = true
	case "delete_after_quarantine_unlink":
		h.BeforeFinalExternalCheck = block
		ch.Delete = true
	}
	e.SetPublishFaultHooks(h)
	_, _ = e.Commit(context.Background(), Plan{Actor: testActor(), Tool: "apply_patch", Changes: []FileChange{ch}})
}

func TestOpusKillProbe(t *testing.T) {
	for _, mode := range []string{"update_before_swap", "update_after_swap", "delete_before_quarantine", "delete_after_quarantine_unlink"} {
		t.Run(mode, func(t *testing.T) {
			base, _ := filepath.EvalSymlinks(t.TempDir())
			root := filepath.Join(base, "ws")
			_ = os.MkdirAll(root, 0o755)
			_ = os.WriteFile(filepath.Join(root, "f.txt"), []byte("original-content\n"), 0o644)
			cmd := exec.Command(os.Args[0], "-test.run=^TestOpusKillChild$")
			cmd.Env = append(os.Environ(), "LC_OPUS_KILL_MODE="+mode, "LC_OPUS_KILL_ROOT="+root)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(base, "marker")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					_ = cmd.Process.Kill()
					t.Fatal("child never reached hook")
				}
				time.Sleep(time.Millisecond)
			}
			_ = cmd.Process.Signal(syscall.SIGKILL)
			_ = cmd.Wait()
			ents, _ := os.ReadDir(root)
			for _, en := range ents {
				b, _ := os.ReadFile(filepath.Join(root, en.Name()))
				t.Logf("KILL %s: %s = %q", mode, en.Name(), b)
			}
			if len(ents) == 0 {
				t.Logf("KILL %s: directory empty", mode)
			}
		})
	}
}
