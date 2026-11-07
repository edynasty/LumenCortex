package evalruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
)

func TestOpusImportAndMaterializeRejectSymlinkEscape(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	s := h.create("python", "owner", "owner")
	source := h.create("python", "owner", "owner")
	victim := filepath.Join(h.root, "host-victim")
	h.expectCompleted(s, "owner", fmt.Sprintf("import os\nos.symlink(%q, 'attack.txt')", victim))
	if _, err := h.m.PutArtifact(context.Background(), s.ID, "owner", "attack.txt", "text/plain", []byte("ESCAPE")); err == nil {
		t.Fatal("symlink import accepted")
	}
	a, err := h.m.PutArtifact(context.Background(), source.ID, "owner", "source.txt", "text/plain", []byte("PAYLOAD"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.m.Execute(context.Background(), evalapi.Request{ID: "materialize-symlink", SessionID: s.ID, AgentID: "owner", Code: "print('must not run')", Inputs: []evalapi.Input{{SessionID: source.ID, ArtifactID: a.ID, Name: "attack.txt"}}}, nil)
	if err == nil {
		t.Fatal("symlink materialization accepted")
	}
	if _, err := os.Stat(victim); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside file created: %v", err)
	}
	for _, name := range []string{"../escape", "nested/name", "/absolute", ".."} {
		if _, err := h.m.PutArtifact(context.Background(), s.ID, "owner", name, "text/plain", []byte("x")); err == nil {
			t.Fatalf("path name accepted %q", name)
		}
	}
}
func TestOpusResetCannotClobberHostProfileTarget(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	s := h.create("python", "owner", "owner")
	victim := filepath.Join(h.root, "host-original")
	os.WriteFile(victim, []byte("ORIGINAL"), 0600)
	h.expectCompleted(s, "owner", fmt.Sprintf("import os\nos.symlink(%q, os.path.join(os.path.dirname(os.getcwd()),'sandbox.sb'))", victim))
	internal, _ := h.m.session(s.ID)
	old := internal.dataFS
	if err := h.m.Reset(context.Background(), s.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := old.ReadFile("work/anything", 1024); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("old generation handle live: %v", err)
	}
	b, err := os.ReadFile(victim)
	if err != nil || string(b) != "ORIGINAL" {
		t.Fatalf("host clobbered %q %v", b, err)
	}
	h.expectCompleted(s, "owner", "print('reset works')")
	r := h.exec(s, "owner", fmt.Sprintf("open(%q,'w').write('FORGED')", internal.profilePath), evalapi.Budget{})
	if r.Status == evalapi.ResultStatusCompleted {
		t.Fatal("worker wrote host-private profile")
	}
}
func TestOpusPendingRejectsSymlinkAndFIFO(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	s := h.create("python", "owner", "owner")
	secret := filepath.Join(h.root, "synthetic-secret")
	os.WriteFile(secret, []byte("HOST-SECRET"), 0600)
	r := h.exec(s, "owner", fmt.Sprintf("import os\np=os.environ['LC_EVAL_ARTIFACTS']\nos.symlink(%q,p+'/linked')\nos.mkfifo(p+'/fifo')", secret), evalapi.Budget{})
	if !r.Succeeded() && !strings.Contains(r.Error, "pending artifact") {
		t.Fatalf("unexpected setup failure: %+v", r)
	}
	internal, _ := h.m.session(s.ID)
	for _, name := range []string{"linked", "fifo"} {
		done := make(chan error, 1)
		go func() { _, err := internal.adoptPendingArtifacts("attack", []wireArtifact{{Name: name}}); done <- err }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("unsafe adoption %s", name)
			}
		case <-time.After(time.Second):
			t.Fatalf("adoption blocked %s", name)
		}
	}
	if len(internal.artifactsSnapshot()) != 0 {
		t.Fatal("unsafe artifact indexed")
	}
}
func TestOpusArtifactHostStoreNotWorkerWritable(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	s := h.create("python", "owner", "owner")
	a, err := h.m.PutArtifact(context.Background(), s.ID, "owner", "hello.txt", "text/plain", []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	internal, _ := h.m.session(s.ID)
	r := h.exec(s, "owner", fmt.Sprintf("import os\nos.remove(%q)", filepath.Join(internal.artifactDir, a.ID)), evalapi.Budget{})
	if r.Status == evalapi.ResultStatusCompleted {
		t.Fatal("worker removed immutable host artifact")
	}
	_, b, err := h.m.Artifact(context.Background(), s.ID, "owner", a.ID)
	if err != nil || string(b) != "hello" {
		t.Fatalf("artifact changed %q %v", b, err)
	}
}
func TestOpusPendingSwapNeverReturnsHostSecret(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	s := h.create("python", "owner", "owner")
	secret := filepath.Join(h.root, "synthetic-race-secret")
	os.WriteFile(secret, []byte("HOST-SECRET"), 0600)
	r := h.exec(s, "owner", fmt.Sprintf(`import os,threading
stop_race=threading.Event()
def race():
 p=os.environ['LC_EVAL_ARTIFACTS']
 while not stop_race.is_set():
  try:
   with open(p+'/safe.tmp','w') as f: f.write('SAFE')
   os.replace(p+'/safe.tmp',p+'/race.bin')
   try: os.unlink(p+'/link.tmp')
   except FileNotFoundError: pass
   os.symlink(%q,p+'/link.tmp');os.replace(p+'/link.tmp',p+'/race.bin')
  except OSError: pass
race_thread=threading.Thread(target=race,daemon=True)
race_thread.start()`, secret), evalapi.Budget{})
	if !r.Succeeded() && !strings.Contains(r.Error, "pending artifact") {
		t.Fatalf("unexpected race setup failure: %+v", r)
	}
	internal, _ := h.m.session(s.ID)
	for i := 0; i < 200; i++ {
		artifacts, _ := internal.adoptPendingArtifacts("race", []wireArtifact{{Name: "race.bin"}})
		for _, a := range artifacts {
			_, b, err := h.m.Artifact(context.Background(), s.ID, "owner", a.ID)
			if err == nil && strings.Contains(string(b), "HOST-SECRET") {
				t.Fatal("host bytes leaked")
			}
		}
	}
	_ = h.exec(s, "owner", "stop_race.set(); race_thread.join()", evalapi.Budget{})
}
