//go:build darwin || linux

package safefs

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func rootFixture(t *testing.T) (*Root, string, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(base, "inside")
	outside := filepath.Join(base, "outside")
	for _, p := range []string{inside, outside} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Open(inside)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, inside, outside
}
func TestRejectsSymlinkReadsWritesAndTraversal(t *testing.T) {
	r, inside, outside := rootFixture(t)
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("HOST-SECRET"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, target string }{{"secret", secret}, {"dangling", filepath.Join(outside, "new")}, {"nested", outside}} {
		if err := os.Symlink(tc.target, filepath.Join(inside, tc.name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"secret", "dangling", "nested/secret", "../outside/secret", "nested/../secret", "/etc/passwd", "a/../../outside/secret"} {
		if data, err := r.ReadFile(name, 1024); err == nil {
			t.Fatalf("unsafe read %s returned %q", name, data)
		}
		if err := r.WriteFile(name, []byte("ATTACK"), 0600); err == nil {
			t.Fatalf("unsafe write %s succeeded", name)
		}
	}
	b, err := os.ReadFile(secret)
	if err != nil || string(b) != "HOST-SECRET" {
		t.Fatalf("outside changed: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside created: %v", err)
	}
}
func TestSpecialFilesRejectWithoutBlocking(t *testing.T) {
	inside, err := os.MkdirTemp("/tmp", "lc-sf-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(inside) })
	inside, err = filepath.EvalSymlinks(inside)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Open(inside)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := unix.Mkfifo(filepath.Join(inside, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	socket, err := net.Listen("unix", filepath.Join(inside, "socket"))
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	if err := os.Mkdir(filepath.Join(inside, "directory"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"fifo", "socket", "directory"} {
		done := make(chan error, 1)
		go func() { _, err := r.ReadFile(name, 1024); done <- err }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("accepted %s", name)
			}
		case <-time.After(time.Second):
			t.Fatalf("blocked on %s", name)
		}
	}
}
func TestValidatedDescriptorSurvivesPathSwap(t *testing.T) {
	r, inside, outside := rootFixture(t)
	os.WriteFile(filepath.Join(inside, "pending"), []byte("SAFE"), 0600)
	os.WriteFile(filepath.Join(outside, "secret"), []byte("HOST-SECRET"), 0600)
	f, _, err := r.OpenRegular("pending", 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Rename(filepath.Join(inside, "pending"), filepath.Join(inside, "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(inside, "pending")); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(f)
	if err != nil || string(data) != "SAFE" {
		t.Fatalf("descriptor changed %q %v", data, err)
	}
	if data, err := r.ReadFile("pending", 1024); err == nil {
		t.Fatalf("replacement accepted %q", data)
	}
}
func TestDirectoryReplacementRaceStaysInside(t *testing.T) {
	r, inside, outside := rootFixture(t)
	os.Mkdir(filepath.Join(inside, "dir"), 0700)
	os.WriteFile(filepath.Join(inside, "dir", "value"), []byte("SAFE"), 0600)
	os.WriteFile(filepath.Join(outside, "value"), []byte("HOST-SECRET"), 0600)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if os.Rename(filepath.Join(inside, "dir"), filepath.Join(inside, "saved")) != nil {
				continue
			}
			os.Symlink(outside, filepath.Join(inside, "dir"))
			os.Remove(filepath.Join(inside, "dir"))
			os.Rename(filepath.Join(inside, "saved"), filepath.Join(inside, "dir"))
		}
	}()
	for i := 0; i < 500; i++ {
		data, err := r.ReadFile("dir/value", 1024)
		if err == nil && string(data) != "SAFE" && string(data) != "LOCAL" {
			close(stop)
			wg.Wait()
			t.Fatalf("outside bytes returned %q", data)
		}
		_ = r.WriteFile("dir/value", []byte("LOCAL"), 0600)
	}
	close(stop)
	wg.Wait()
	data, err := os.ReadFile(filepath.Join(outside, "value"))
	if err != nil || string(data) != "HOST-SECRET" {
		t.Fatalf("outside changed %q %v", data, err)
	}
}
func TestBoundsAndRevokedHandle(t *testing.T) {
	r, _, _ := rootFixture(t)
	if err := r.WriteFile("large", bytes.Repeat([]byte("x"), 4096), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadFile("large", 64); !errors.Is(err, ErrLimit) {
		t.Fatalf("size bound %v", err)
	}
	if n, err := r.Bytes(10, 10); err != nil || n != 4096 {
		t.Fatalf("size accounting %d %v", n, err)
	}
	r.Close()
	if err := r.WriteFile("after", nil, 0600); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("revoked write %v", err)
	}
}
func TestRemovalDoesNotFollowDirectorySymlink(t *testing.T) {
	r, inside, outside := rootFixture(t)
	os.WriteFile(filepath.Join(outside, "keep"), []byte("KEEP"), 0600)
	os.Symlink(outside, filepath.Join(inside, "link"))
	if err := r.RemoveAll("link"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(outside, "keep"))
	if err != nil || string(data) != "KEEP" {
		t.Fatalf("outside removal %q %v", data, err)
	}
}
