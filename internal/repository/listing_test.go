package repository

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestListDirOrdersDirectoriesFirstAndReportsKinds(t *testing.T) {
	root := t.TempDir()
	write := func(name, content string) {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "readme")
	write("go.mod", "module demo")
	write("Alpha.txt", "a")
	write("zeta.txt", "z")
	write("Zed.txt", "Z")
	write("src/main.go", "package main")
	write("internal/repository/service.go", "package repository")
	write(".env", "TOKEN=1")
	write(".git/config", "[core]")
	if err := os.MkdirAll(filepath.Join(root, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("README.md", filepath.Join(root, "readme-link")); err != nil {
		t.Fatal(err)
	}

	service, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	listing, err := service.ListDir(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if listing.Path != "" || listing.Truncated {
		t.Fatalf("listing=%#v", listing)
	}
	var names []string
	for _, entry := range listing.Entries {
		names = append(names, entry.Name)
	}
	// Directories first (case-insensitive), then files and symlinks (case-insensitive).
	want := []string{".github", "internal", "src", ".env", "Alpha.txt", "go.mod", "readme-link", "README.md", "Zed.txt", "zeta.txt"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("names=%v want=%v", names, want)
	}
	byName := map[string]DirEntry{}
	for _, entry := range listing.Entries {
		byName[entry.Name] = entry
	}
	if byName[".git"].Kind != "" {
		t.Fatalf(".git should be omitted: %#v", byName[".git"])
	}
	if byName[".github"].Kind != DirEntryDir || byName[".github"].Path != ".github" {
		t.Fatalf(".github=%#v", byName[".github"])
	}
	if byName["readme-link"].Kind != DirEntrySymlink {
		t.Fatalf("readme-link=%#v", byName["readme-link"])
	}
	if byName["README.md"].Kind != DirEntryFile || byName["README.md"].Size != int64(len("readme")) {
		t.Fatalf("README.md=%#v", byName["README.md"])
	}
	if byName["internal"].Path != "internal" {
		t.Fatalf("internal=%#v", byName["internal"])
	}

	nested, err := service.ListDir(context.Background(), "internal")
	if err != nil {
		t.Fatal(err)
	}
	if nested.Path != "internal" || len(nested.Entries) != 1 || nested.Entries[0].Path != "internal/repository" {
		t.Fatalf("nested=%#v", nested)
	}
	deep, err := service.ListDir(context.Background(), "internal/repository")
	if err != nil {
		t.Fatal(err)
	}
	if len(deep.Entries) != 1 || deep.Entries[0].Path != "internal/repository/service.go" {
		t.Fatalf("deep=%#v", deep)
	}
}

func TestListDirConfinesToTheWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(root, "loop")); err != nil {
		t.Fatal(err)
	}
	service, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{"..", "../", outside, filepath.Join("dir", "..", "..", "etc")} {
		if _, err := service.ListDir(context.Background(), relative); err == nil {
			t.Fatalf("ListDir(%q) should be refused", relative)
		}
	}
	if _, err := service.ListDir(context.Background(), "escape"); err == nil {
		t.Fatal("a symlink to a directory outside the workspace should be refused")
	}
	listing, err := service.ListDir(context.Background(), "loop")
	if err != nil {
		t.Fatalf("a symlink inside the workspace should list: %v", err)
	}
	if listing.Path != "loop" {
		t.Fatalf("listing=%#v", listing)
	}
	// The escape entry itself is visible as a symlink; listing it is what is refused.
	rootListing, err := service.ListDir(context.Background(), ".")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range rootListing.Entries {
		if entry.Name == "escape" {
			found = true
			if entry.Kind != DirEntrySymlink {
				t.Fatalf("escape=%#v", entry)
			}
		}
	}
	if !found {
		t.Fatal("the escape symlink should be listed as an entry")
	}
	file := filepath.Join(root, "dir", "note.txt")
	if err := os.WriteFile(file, []byte("note"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ListDir(context.Background(), "dir/note.txt"); err == nil {
		t.Fatal("listing a file should fail")
	}
	if _, err := service.ListDir(context.Background(), "missing"); err == nil {
		t.Fatal("listing a missing directory should fail")
	}
}

func TestListDirIsBounded(t *testing.T) {
	root := t.TempDir()
	total := MaxListEntries + 5
	for index := range total {
		name := fmt.Sprintf("file-%04d.txt", index)
		if err := os.WriteFile(filepath.Join(root, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	service, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	listing, err := service.ListDir(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !listing.Truncated || len(listing.Entries) != MaxListEntries {
		t.Fatalf("entries=%d truncated=%v", len(listing.Entries), listing.Truncated)
	}
	if listing.Entries[0].Name != "file-0000.txt" {
		t.Fatalf("first=%#v", listing.Entries[0])
	}
}

func TestListDirHonorsCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory reads differ on windows")
	}
	root := t.TempDir()
	service, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.ListDir(ctx, ""); err == nil {
		t.Fatal("a cancelled context should fail before reading")
	}
}
