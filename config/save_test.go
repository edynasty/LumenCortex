package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func sample(model string) File {
	return File{
		Model: ModelRef(model),
		Providers: map[string]Provider{"p": {
			Settings: &ProviderSettings{BaseURL: "https://p.example", APIKey: "{env:P_KEY}"},
			Models:   map[string]Model{"a": {}, "b": {}},
		}},
	}
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

func TestSaveRoundTripAndDigest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "lumencortex")
	path := filepath.Join(dir, FileName)

	d1, err := Save(path, sample("p/a"), "")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ReadDocument(path)
	if err != nil || doc.Digest != d1 || doc.File.Model != "p/a" {
		t.Fatalf("doc=%+v err=%v digest=%s", doc, err, d1)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
			t.Errorf("mode=%v", st.Mode().Perm())
		}
		if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
			t.Errorf("dir mode=%v", st.Mode().Perm())
		}
	}
	d2, err := Save(path, sample("p/b"), d1)
	if err != nil || d2 == d1 {
		t.Fatalf("second save: %v %s", err, d2)
	}
	if names := dirEntries(t, dir); len(names) != 1 {
		t.Fatalf("leftovers: %v", names)
	}
	// The saved file is what the schema accepts, readable by Load as global config.
	if raw := readFile(t, path); !strings.HasSuffix(raw, "}\n") {
		t.Fatalf("raw=%q", raw)
	}
}

func TestSaveConflictDetection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	base, err := Save(path, sample("p/a"), "")
	if err != nil {
		t.Fatal(err)
	}
	// Another writer changes the file after we read it.
	if _, err := Save(path, sample("p/b"), base); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, path)
	_, err = Save(path, sample("p/a"), base)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale digest: %v", err)
	}
	if readFile(t, path) != before {
		t.Fatal("a conflicting Save overwrote the file")
	}
	// A foreign editor (bypassing Save) also conflicts.
	writeFile(t, path, `{"model": "p/a"}`)
	if _, err := Save(path, sample("p/b"), Digest([]byte(before))); !errors.Is(err, ErrConflict) {
		t.Fatalf("foreign edit: %v", err)
	}
	// "File did not exist" conflicts once it exists, and a stale digest conflicts once it is gone.
	if _, err := Save(path, sample("p/a"), ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected missing but present: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := Save(path, sample("p/a"), base); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected present but removed: %v", err)
	}
	if names := dirEntries(t, dir); len(names) != 0 {
		t.Fatalf("conflicting Save left files: %v", names)
	}
}

// Many writers holding the same digest: exactly one wins, the rest see ErrConflict, and the file
// is always one writer's complete document.
func TestSaveConcurrentWritersSingleWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	base, err := Save(path, sample("p/a"), "")
	if err != nil {
		t.Fatal(err)
	}
	const writers = 16
	var wins, conflicts atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f := sample("p/a")
			f.Failover = []ModelRef{"p/b"}
			f.Providers["p"].Models["w"+string(rune('a'+i))] = Model{}
			<-start
			_, err := Save(path, f, base)
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, ErrConflict):
				conflicts.Add(1)
			default:
				t.Errorf("writer %d: %v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 || conflicts.Load() != writers-1 {
		t.Fatalf("wins=%d conflicts=%d", wins.Load(), conflicts.Load())
	}
	doc, err := ReadDocument(path)
	if err != nil || len(doc.File.Providers["p"].Models) != 3 {
		t.Fatalf("file is not exactly one writer's document: %+v %v", doc.File, err)
	}
	for _, n := range dirEntries(t, filepath.Dir(path)) {
		if n != FileName {
			t.Errorf("leftover %s", n)
		}
	}
}

func TestSaveFailuresLeaveNothingBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	good, err := Save(path, sample("p/a"), "")
	if err != nil {
		t.Fatal(err)
	}
	before := readFile(t, path)

	invalid := sample("p/a")
	invalid.Providers["a/b"] = Provider{}
	if _, err := Save(path, invalid, good); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid: %v", err)
	}
	if readFile(t, path) != before || len(dirEntries(t, dir)) != 1 {
		t.Fatalf("invalid save changed the directory: %v", dirEntries(t, dir))
	}

	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		// A read-only directory: the temporary file cannot be created.
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(dir, 0o700)
		if _, err := Save(path, sample("p/b"), good); err == nil {
			t.Fatal("save into a read-only directory succeeded")
		}
		_ = os.Chmod(dir, 0o700)
		if readFile(t, path) != before || len(dirEntries(t, dir)) != 1 {
			t.Fatalf("failed save left %v", dirEntries(t, dir))
		}
	}

	// The target is a directory: rename fails after the temporary file was written, and the
	// temporary file is removed.
	blocked := filepath.Join(dir, "blocked.json")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(blocked, "keep"), "x")
	if _, err := Save(blocked, sample("p/a"), ""); err == nil {
		t.Fatal("save over a directory succeeded")
	}
	for _, n := range dirEntries(t, dir) {
		if strings.Contains(n, ".tmp-") {
			t.Errorf("temporary file left behind: %s", n)
		}
	}
}

func TestSaveThroughSymlinkReplacesTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	root := canon(t, t.TempDir())
	target := filepath.Join(root, "real.json")
	link := filepath.Join(root, FileName)
	d, err := Save(target, sample("p/a"), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Save(link, sample("p/b"), d); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink was replaced by a file: %v", err)
	}
	if doc, _ := ReadDocument(target); doc.File.Model != "p/b" {
		t.Fatalf("target=%+v", doc.File)
	}
}

// Save rewrites a document Parse produced without losing or adding keys.
func TestSaveKeepsExplicitEmptyFailover(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	f := File{Failover: []ModelRef{}}
	if _, err := Save(path, f, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, path), `"failover": []`) {
		t.Fatalf("explicit empty failover dropped: %s", readFile(t, path))
	}
	doc, err := ReadDocument(path)
	if err != nil || doc.File.Failover == nil {
		t.Fatalf("doc=%+v err=%v", doc.File, err)
	}
}

// The shared temporary directory (/tmp on Linux) is writable by every local user, who can create
// a predictable lock directory first and plant files in it. Save must take its lock somewhere
// private to the user, so a planted lock symlink is never followed and cannot block it.
func TestSaveLockIsNotInTheSharedTempDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	env(t)
	shared := canon(t, t.TempDir())
	t.Setenv("TMPDIR", shared)
	path := filepath.Join(canon(t, t.TempDir()), FileName)

	// What another user can prepare: the directory, world-writable, and a symlink named after the
	// lock of path pointing at a file the victim can create.
	squat := filepath.Join(shared, "lumencortex-config-locks-"+strconv.Itoa(os.Getuid()))
	if err := os.Mkdir(squat, 0o777); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(squat, 0o777)
	sum := sha256.Sum256([]byte(path))
	victim := filepath.Join(canon(t, t.TempDir()), "planted")
	if err := os.Symlink(victim, filepath.Join(squat, hex.EncodeToString(sum[:16])+".lock")); err != nil {
		t.Fatal(err)
	}

	if _, err := Save(path, sample("p/a"), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(victim); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Save followed a lock symlink planted in the shared temp directory: %v", err)
	}
}
