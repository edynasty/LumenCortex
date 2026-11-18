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

// TestQGR02_PromptCatalogContiguousOffsetAndMaxBytes verifies Finding QGR-02:
// when a longer skill description at the inline catalog boundary does not fit while subsequent skills have
// shorter descriptions, inline inclusion stops at the first omitted skill so offset=len(Included) (and
// compact offset) never skips an omitted skill, and len(PromptResult.Text) <= MaxCatalogBytes (32 KiB).
func TestQGR02_PromptCatalogContiguousOffsetAndMaxBytes(t *testing.T) {
	globalRoot := t.TempDir()
	projectRoot := t.TempDir()
	registry, err := NewRegistry(
		[]Root{{Source: SourceLumenCortex, Path: filepath.Join(globalRoot, "skills"), Writable: true}},
		[]Root{{Source: SourceLumenCortex, Path: filepath.Join(projectRoot, "skills"), Writable: true}},
	)
	if err != nil {
		t.Fatal(err)
	}

	longDesc := strings.Repeat("L", MaxDescriptionBytes)
	for i := range 33 {
		id := fmt.Sprintf("s-%03d", i)
		if err := registry.Save(ScopeGlobal, id, "---\ndescription: "+longDesc+"\n---\nbody"); err != nil {
			t.Fatal(err)
		}
	}
	// After ~31-32 ~1KB skills cross the 32 KiB boundary, add tiny skills that WOULD fit if non-contiguous!
	for i := 33; i < 38; i++ {
		id := fmt.Sprintf("s-%03d", i)
		if err := registry.Save(ScopeGlobal, id, "---\ndescription: tiny\n---\nbody"); err != nil {
			t.Fatal(err)
		}
	}

	res, err := registry.Prompt(PromptOptions{IncludeProject: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Text) > MaxCatalogBytes {
		t.Fatalf("len(Text) = %d > MaxCatalogBytes (%d)", len(res.Text), MaxCatalogBytes)
	}
	if len(res.CompactText) > MaxCompactCatalogBytes {
		t.Fatalf("len(CompactText) = %d > MaxCompactCatalogBytes (%d)", len(res.CompactText), MaxCompactCatalogBytes)
	}
	// Included MUST be the exact contiguous prefix Eligible[0 : len(Included)]!
	for i, inc := range res.Included {
		if inc.ID != res.Eligible[i].ID {
			t.Fatalf("non-contiguous Included[%d]=%s != Eligible[%d]=%s (omitted skill was skipped!)", i, inc.ID, i, res.Eligible[i].ID)
		}
	}
	wantFooter := fmt.Sprintf("offset=%d", len(res.Included))
	if !strings.Contains(res.Text, wantFooter) {
		t.Fatalf("Text missing contiguous %s footer: %q", wantFooter, res.Text[max(0, len(res.Text)-140):])
	}
}

