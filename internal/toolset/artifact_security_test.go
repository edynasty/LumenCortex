package toolset

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestArtifactTransferPathSwapCannotReachOutside(t *testing.T) {
	for _, write := range []bool{false, true} {
		t.Run(map[bool]string{false: "read", true: "write"}[write], func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			victim := filepath.Join(outside, "victim")
			if err := os.WriteFile(victim, []byte("HOST-SECRET"), 0600); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(root, "file")
			if err := os.WriteFile(file, []byte("SAFE"), 0600); err != nil {
				t.Fatal(err)
			}
			set, err := New(Options{Workspace: root, Policy: PolicyWorkspace})
			if err != nil {
				t.Fatal(err)
			}
			testHookBeforeOpen = func(path string) {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(victim, path); err != nil {
					t.Fatal(err)
				}
			}
			defer func() { testHookBeforeOpen = nil }()
			if write {
				err = set.WriteArtifact(context.Background(), "file", []byte("ATTACK"))
			} else {
				_, err = set.ReadArtifact(context.Background(), "file", 1024)
			}
			if err == nil {
				t.Fatal("replacement symlink accepted")
			}
			b, err := os.ReadFile(victim)
			if err != nil || string(b) != "HOST-SECRET" {
				t.Fatalf("outside changed %q %v", b, err)
			}
		})
	}
}
