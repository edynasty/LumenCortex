package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestRegistryMaxSkillsRootTruncationWarningAndSnapshot(t *testing.T) {
	globalRoot := t.TempDir()
	projectRoot := t.TempDir()
	registry, err := NewRegistry(
		[]Root{{Source: SourceLumenCortex, Path: filepath.Join(globalRoot, "skills"), Writable: true}},
		[]Root{{Source: SourceLumenCortex, Path: filepath.Join(projectRoot, "skills"), Writable: true}},
	)
	if err != nil {
		t.Fatal(err)
	}

	for i := range 135 {
		id := fmt.Sprintf("skill-%03d", i)
		body := fmt.Sprintf("---\nname: %s\ndescription: trigger %03d\n---\nbody-%03d", id, i, i)
		if err := registry.Save(ScopeGlobal, id, body); err != nil {
			t.Fatal(err)
		}
	}

	res, err := registry.Prompt(PromptOptions{IncludeProject: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Eligible) != MaxSkills {
		t.Fatalf("Eligible=%d want %d", len(res.Eligible), MaxSkills)
	}
	if len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "128-skill per-root limit") {
		t.Fatalf("expected MaxSkills truncation warning, got %#v", res.Warnings)
	}
	if res.Eligible[0].ID != "skill-000" || res.Eligible[0].Content == "" || !strings.Contains(res.Eligible[0].Content, "body-000") {
		t.Fatalf("unexpected Eligible[0]: %+v", res.Eligible[0])
	}
	if len(res.CompactText) == 0 || len(res.CompactText) > MaxCompactCatalogBytes {
		t.Fatalf("CompactText len=%d outside (0, %d]", len(res.CompactText), MaxCompactCatalogBytes)
	}
}

func TestRegistryConcurrentMutationsAndReadsRaceSafe(t *testing.T) {
	globalRoot := t.TempDir()
	projectRoot := t.TempDir()
	registry, err := NewRegistry(
		[]Root{{Source: SourceLumenCortex, Path: filepath.Join(globalRoot, "skills"), Writable: true}},
		[]Root{{Source: SourceLumenCortex, Path: filepath.Join(projectRoot, "skills"), Writable: true}},
	)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		id := fmt.Sprintf("shared-%d", i)
		if err := registry.Save(ScopeGlobal, id, "---\ndescription: initial\n---\ninit"); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			target := fmt.Sprintf("shared-%d", worker%4)
			ephemeral := fmt.Sprintf("temp-%d", worker)
			for iter := range 15 {
				_ = registry.Save(ScopeGlobal, target, fmt.Sprintf("---\ndescription: w%d-%d\n---\nbody", worker, iter))
				_ = registry.Save(ScopeProject, ephemeral, "---\ndescription: ephem\n---\nbody")
				_ = registry.SetEnabled(ScopeGlobal, target, iter%2 == 0)
				_, _ = registry.List(ScopeEffective)
				_, _ = registry.Prompt(PromptOptions{IncludeProject: true})
				_ = registry.Delete(ScopeProject, ephemeral)
			}
		}(g)
	}
	wg.Wait()

	// Ensure no leftover temporary files remain in writable directories.
	for _, dir := range []string{globalRoot, filepath.Join(globalRoot, "skills"), projectRoot, filepath.Join(projectRoot, "skills")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".tmp") {
				t.Fatalf("leftover temp file in %s: %s", dir, e.Name())
			}
		}
	}
}
