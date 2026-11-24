package editing

// Review probe (throwaway; /tmp only): create on LC_R3_FSROOT with the pre-R2 engine (a8b4786).
import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestZZR3PrevFSCreate(t *testing.T) {
	fsroot := os.Getenv("LC_R3_FSROOT")
	if fsroot == "" {
		t.Skip("set LC_R3_FSROOT")
	}
	base, err := os.MkdirTemp(fsroot, "zzprev-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	e, err := New(Options{Root: base})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("created by agent\n")
	_, cerr := e.Commit(context.Background(), Plan{Actor: Actor{Session: "s"}, Tool: "write_file",
		Changes: []FileChange{{Path: "new.txt", Content: &body, AllowCreate: true}}})
	got, rerr := os.ReadFile(filepath.Join(base, "new.txt"))
	t.Logf("a8b4786 engine create: err=%v file=%q readErr=%v", cerr, got, rerr)
}
