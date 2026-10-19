package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
)

// storeSecret is planted in the store; every redaction test asserts it appears in no error, JSON
// blob or fmt dump the store can produce.
const storeSecret = "sk-store-NEVER-PRINT-13579"

func newStore(t *testing.T) (*FileCredentialStore, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, CredentialStoreFileName)
	return NewFileCredentialStore(path), path
}

func TestFileCredentialStoreLifecycle(t *testing.T) {
	s, path := newStore(t)
	// A missing file is an empty store.
	if _, err := s.Get("alpha"); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("missing store: %v", err)
	}
	if s.Has("alpha") || s.Has("not a name") {
		t.Fatal("empty store holds a secret")
	}
	names, err := s.Names()
	if err != nil || len(names) != 0 {
		t.Fatalf("names=%v err=%v", names, err)
	}
	// Set, Get, replace, delete.
	if err := s.Set("alpha", storeSecret); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("beta.two", "k-2"); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Get("alpha"); err != nil || v != storeSecret {
		t.Fatalf("get=%q err=%v", v, err)
	}
	if err := s.Set("alpha", "k-1-replaced"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Get("alpha"); v != "k-1-replaced" {
		t.Fatalf("replace=%q", v)
	}
	names, err = s.Names()
	if err != nil || len(names) != 2 || names[0] != "alpha" || names[1] != "beta.two" {
		t.Fatalf("names=%v err=%v", names, err)
	}
	if !s.Has("alpha") || s.Has("gamma") {
		t.Fatal("Has is wrong")
	}
	// Resolve satisfies CredentialResolver.
	var resolver CredentialResolver = s
	if v, err := resolver.Resolve("beta.two"); err != nil || v != "k-2" {
		t.Fatalf("resolve=%q err=%v", v, err)
	}
	// Delete is idempotent.
	for range 2 {
		if err := s.Delete("alpha"); err != nil {
			t.Fatal(err)
		}
	}
	if s.Has("alpha") {
		t.Fatal("alpha survived the delete")
	}
	// The file on disk is the documented name and mode.
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != CredentialStoreFileName {
		t.Fatalf("file=%s", path)
	}
	// Names carries names only.
	namesJSON, _ := json.Marshal(names)
	if strings.Contains(string(namesJSON), storeSecret) {
		t.Fatal("names carry values")
	}
}

func TestFileCredentialStoreDefaultPath(t *testing.T) {
	_, globalDir := env(t)
	p, err := DefaultCredentialStorePath()
	if err != nil || p != filepath.Join(globalDir, CredentialStoreFileName) {
		t.Fatalf("path=%q err=%v", p, err)
	}
}

func TestFileCredentialStoreValidatesNamesAndValues(t *testing.T) {
	s, _ := newStore(t)
	for _, name := range []string{"", "-x", ".x", "a b", strings.Repeat("b", 129), "a/b"} {
		if err := s.Set(name, "k"); err == nil {
			t.Errorf("accepted name %q", name)
		}
		if _, err := s.Get(name); err == nil {
			t.Errorf("got name %q", name)
		}
		if s.Has(name) {
			t.Errorf("has name %q", name)
		}
		if err := s.Delete(name); err == nil {
			t.Errorf("deleted name %q", name)
		}
	}
	if err := s.Set("alpha", ""); err == nil {
		t.Error("accepted an empty value")
	}
	if err := s.Set("alpha", strings.Repeat("k", maxSecretBytes+1)); err == nil {
		t.Error("accepted an oversize value")
	}
	if names, err := s.Names(); err != nil || len(names) != 0 {
		t.Fatalf("failed Sets wrote something: %v %v", names, err)
	}
}

func TestFileCredentialStorePerms(t *testing.T) {
	if runtime.GOOS != "windows" {
		s, path := newStore(t)
		if err := s.Set("alpha", storeSecret); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != filePerm {
			t.Fatalf("file mode=%04o want %04o", perm, filePerm)
		}
		dirInfo, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if perm := dirInfo.Mode().Perm(); perm&0077 != 0 {
			t.Fatalf("dir mode=%04o leaks group/other bits", perm)
		}
		// A loosened file fails closed, naming only the path and mode.
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err = s.Get("alpha")
		if err == nil || !strings.Contains(err.Error(), "0644") || !strings.Contains(err.Error(), "0600") {
			t.Fatalf("loose file: %v", err)
		}
		if strings.Contains(err.Error(), storeSecret) {
			t.Fatalf("loose-file error leaks the value: %v", err)
		}
		if s.Has("alpha") {
			t.Fatal("loose file reported as readable")
		}
		if _, err := s.Names(); err == nil {
			t.Fatal("loose file listed")
		}
		// A write refuses the loose file too: it cannot merge what it will not read. The value is
		// intact and the mode is the user's to fix; after chmod 600 everything works again.
		if err := s.Set("beta", "k-2"); err == nil || !strings.Contains(err.Error(), "0644") {
			t.Fatalf("loose file write: %v", err)
		}
		if strings.Contains(err.Error(), storeSecret) {
			t.Fatalf("loose-file write error leaks the value: %v", err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		if v, err := s.Get("alpha"); err != nil || v != storeSecret {
			t.Fatalf("get after repair=%q %v", v, err)
		}
		if err := s.Set("alpha", "k-fixed"); err != nil {
			t.Fatal(err)
		}
		if v, err := s.Get("alpha"); err != nil || v != "k-fixed" {
			t.Fatalf("get after rewrite=%q %v", v, err)
		}
		info, _ = os.Stat(path)
		if perm := info.Mode().Perm(); perm != filePerm {
			t.Fatalf("rewrite mode=%04o", perm)
		}
	}
}

func TestFileCredentialStoreAtomicWriteLeavesNoTempAndKeepsTheOldValue(t *testing.T) {
	if runtime.GOOS != "windows" {
		s, path := newStore(t)
		if err := s.Set("alpha", storeSecret); err != nil {
			t.Fatal(err)
		}
		// A non-writable directory (0500) fails the write; the owner bits are never widened.
		dir := filepath.Dir(path)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		if err := s.Set("beta", "k-2"); err == nil {
			t.Fatal("write into a read-only directory accepted")
		}
		// The old value is intact and no temporary file is left behind.
		if v, err := s.Get("alpha"); err != nil || v != storeSecret {
			t.Fatalf("old value=%q err=%v", v, err)
		}
		left, _ := filepath.Glob(filepath.Join(dir, "."+CredentialStoreFileName+".tmp-*"))
		if len(left) != 0 {
			t.Fatalf("temporary files left: %v", left)
		}
	}
}

func TestFileCredentialStoreCorruptFile(t *testing.T) {
	s, path := newStore(t)
	if err := os.WriteFile(path, []byte("{\"alpha\": \""+storeSecret+"\", brok"), filePerm); err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{
		func() error { _, e := s.Get("alpha"); return e }(),
		func() error { _, e := s.Names(); return e }(),
		func() error { return s.Set("beta", "k-2") }(),
		func() error { return s.Delete("alpha") }(),
	} {
		if err == nil {
			t.Fatal("corrupt file accepted")
		}
		if strings.Contains(err.Error(), storeSecret) || strings.Contains(err.Error(), "brok") {
			t.Fatalf("corrupt-file error carries file content: %v", err)
		}
		if !strings.Contains(err.Error(), "not a valid credentials file") && !strings.Contains(err.Error(), CredentialStoreFileName) {
			t.Fatalf("corrupt-file error does not name the file: %v", err)
		}
	}
	// A JSON document that is not an object of names to strings is corrupt too.
	if err := os.WriteFile(path, []byte("[1,2]"), filePerm); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("alpha"); err == nil || strings.Contains(err.Error(), "[1,2]") {
		t.Fatalf("array accepted: %v", err)
	}
}

func TestFileCredentialStoreNeverLeaksValues(t *testing.T) {
	s, path := newStore(t)
	if err := s.Set("alpha", storeSecret); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("beta", storeSecret); err != nil {
		t.Fatal(err)
	}
	blobs := []string{
		fmt.Sprintf("%v", s), fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s),
	}
	j, _ := json.Marshal(s)
	blobs = append(blobs, string(j))
	names, _ := s.Names()
	blobs = append(blobs, fmt.Sprintf("%v", names))
	// Every failure mode's error, and the stored values never reach one.
	_, notFound := s.Get("gamma")
	_ = os.WriteFile(path, []byte("junk"), 0o600)
	_, corrupt := s.Get("alpha")
	if runtime.GOOS != "windows" {
		_ = os.Chmod(path, 0o644)
		_, loose := s.Get("alpha")
		_ = os.Chmod(path, 0o600)
		blobs = append(blobs, fmt.Sprintf("%v", loose))
	}
	_ = os.WriteFile(path, []byte("{}"), filePerm)
	blobs = append(blobs, fmt.Sprintf("%v", notFound), fmt.Sprintf("%v", corrupt))
	blobs = append(blobs, fmt.Sprintf("%v", s.Set("gamma", "")), fmt.Sprintf("%v", s.Set("gamma", strings.Repeat("k", maxSecretBytes+1))))
	for _, b := range blobs {
		if strings.Contains(b, storeSecret) {
			t.Errorf("store leaked a value: %.200s", b)
		}
	}
}

func TestFileCredentialStoreConcurrentSetsLoseNothing(t *testing.T) {
	s, path := newStore(t)
	// Two store instances on the same path, as two processes would have.
	s2 := NewFileCredentialStore(path)
	const writers, perWriter = 4, 8
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			store := s
			if w%2 == 1 {
				store = s2
			}
			for i := range perWriter {
				name := fmt.Sprintf("w%d.i%d", w, i)
				if err := store.Set(name, "k-"+name); err != nil {
					t.Errorf("set %s: %v", name, err)
				}
			}
		}(w)
	}
	wg.Wait()
	names, err := s.Names()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != writers*perWriter {
		sort.Strings(names)
		t.Fatalf("lost secrets: %d of %d (%v)", len(names), writers*perWriter, names)
	}
	// A concurrent reader always sees a complete file: it gets every name or an empty error, never
	// a torn one.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			if _, err := s2.Names(); err != nil {
				t.Errorf("reader saw a torn store: %v", err)
				return
			}
		}
	}()
	<-done
}
