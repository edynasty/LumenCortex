package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	lcx "github.com/edynasty/LumenCortex/runtime"
)

func TestParseDowngradeTarget(t *testing.T) {
	good := map[string]int{"--to 0": 0, "--to 3": 3, "--to=2": 2}
	for in, want := range good {
		got, err := parseDowngradeTarget(strings.Fields(in))
		if err != nil || got != want {
			t.Errorf("%q: got %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "--to", "--to x", "--to -1", "--to 1 --to 2", "1", "--to 1 extra", "--from 1"} {
		if n, err := parseDowngradeTarget(strings.Fields(in)); err == nil {
			t.Errorf("%q accepted as %d", in, n)
		}
	}
}

// stateHash fingerprints the database, its WAL and the backups directory listing.
func stateHash(t *testing.T, stateDir string) [32]byte {
	t.Helper()
	h := sha256.New()
	for _, name := range []string{"lumencortex.db", "lumencortex.db-wal"} {
		raw, err := os.ReadFile(filepath.Join(stateDir, name))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		h.Write([]byte(name))
		h.Write(raw)
	}
	entries, err := os.ReadDir(filepath.Join(stateDir, "backups"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, e := range entries {
		h.Write([]byte(e.Name()))
	}
	return [32]byte(h.Sum(nil))
}

// TestDowngradeToZeroIsRefusedWithoutChanges: version 1 has no down migration, so `db downgrade
// --to 0` fails from version 2 before the backup, and the database, WAL and backups are
// byte-for-byte what they were.
func TestDowngradeToZeroIsRefusedWithoutChanges(t *testing.T) {
	root := t.TempDir()
	engine, err := lcx.Open(lcx.Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.NewSession(context.Background(), lcx.SessionOptions{Goal: "keep me"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, ".lumencortex")
	before := stateHash(t, stateDir)

	err = runDBCommand(context.Background(), lcx.Options{Workspace: root}, []string{"downgrade", "--to", "0"})
	if err == nil {
		t.Fatal("downgrade --to 0 succeeded; version 1 has no down migration")
	}
	if stateHash(t, stateDir) != before {
		t.Error("the refused downgrade changed the database, WAL or backups")
	}
	// The workspace still opens at the latest version.
	reopened, err := lcx.Open(lcx.Options{Workspace: root})
	if err != nil {
		t.Fatalf("Open after a refused downgrade: %v", err)
	}
	reopened.Close()
}

// TestDowngradeNoopAtCurrentVersion: --to equal to the current version (2) changes nothing.
func TestDowngradeNoopAtCurrentVersion(t *testing.T) {
	root := t.TempDir()
	engine, err := lcx.Open(lcx.Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	engine.Close()
	stateDir := filepath.Join(root, ".lumencortex")
	before := stateHash(t, stateDir)
	from, now, err := lcx.DowngradeSchema(context.Background(), lcx.Options{Workspace: root}, 2)
	if err != nil || from != 2 || now != 2 {
		t.Fatalf("from=%d now=%d err=%v", from, now, err)
	}
	if stateHash(t, stateDir) != before {
		t.Error("a no-op downgrade changed the database")
	}
}

// TestDowngradeToOneDropsModelRef: `db downgrade --to 1` removes sessions.model_ref and the
// workspace then opens again (and re-upgrades) with the pins gone.
func TestDowngradeToOneThenReopenReupgrades(t *testing.T) {
	root := t.TempDir()
	engine, err := lcx.Open(lcx.Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	h, err := engine.NewSession(context.Background(), lcx.SessionOptions{Goal: "keep me"})
	if err != nil {
		t.Fatal(err)
	}
	engine.Close()

	if err := runDBCommand(context.Background(), lcx.Options{Workspace: root}, []string{"downgrade", "--to", "1"}); err != nil {
		t.Fatalf("downgrade --to 1: %v", err)
	}
	reopened, err := lcx.Open(lcx.Options{Workspace: root})
	if err != nil {
		t.Fatalf("Open after the downgrade: %v", err)
	}
	defer reopened.Close()
	if _, info, err := reopened.Session(context.Background(), h.ID); err != nil || info.Goal != "keep me" || info.ModelRef != "" {
		t.Fatalf("session after the cycle: %+v %v", info, err)
	}
}

// TestDBCommandDispatchesBeforeOpen runs the real binary: `db downgrade` must reach the schema
// administration path without an ordinary Open first creating, locking or migrating the workspace.
// On an empty workspace Open would create .lumencortex/lumencortex.db; the command must not.
func TestDBCommandDispatchesBeforeOpen(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "lcx-go")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	empty := t.TempDir()
	cmd := exec.Command(bin, "db", "downgrade", "--to", "0")
	cmd.Dir = empty
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("db downgrade on a workspace without a database succeeded")
	}
	if _, err := os.Stat(filepath.Join(empty, ".lumencortex")); !os.IsNotExist(err) {
		t.Errorf("db downgrade created the state directory (stat: %v); an ordinary Open ran first", err)
	}

	// A migrated workspace that no process holds open: refused, unchanged, exit status non-zero.
	root := t.TempDir()
	engine, err := lcx.Open(lcx.Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	engine.Close()
	before := stateHash(t, filepath.Join(root, ".lumencortex"))
	cmd = exec.Command(bin, "db", "downgrade", "--to", "0")
	cmd.Dir = root
	stderr.Reset()
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("db downgrade --to 0 succeeded")
	}
	if !strings.Contains(stderr.String(), "db downgrade") {
		t.Errorf("stderr=%q, want an error naming the command", stderr.String())
	}
	if stateHash(t, filepath.Join(root, ".lumencortex")) != before {
		t.Error("the refused downgrade changed the workspace")
	}

	cmd = exec.Command(bin, "db")
	cmd.Dir = root
	if err := cmd.Run(); err == nil {
		t.Error("`db` without a subcommand succeeded")
	}
}
