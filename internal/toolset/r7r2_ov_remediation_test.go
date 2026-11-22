package toolset

import (
	"fmt"
	"math/rand"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"github.com/edynasty/LumenCortex/internal/editing"
)

// ovBruteOwners computes the owners of a basename group for one root by a plain HasPrefix filter.
func ovBruteOwners(p *failurePathIndex, base, root string) map[string]string {
	owners := map[string]string{}
	for _, full := range p.groups[base] {
		if root != "" && !strings.HasPrefix(full, root+"/") {
			continue
		}
		eachPathParent(full, func(start, end int) {
			name := full[start:end]
			if owner, ok := owners[name]; !ok {
				owners[name] = full
			} else if owner != full {
				owners[name] = ""
			}
		})
	}
	return owners
}

// ovBruteSuffix is the pairwise ground truth for ownedSuffix: the longest common suffix with any other
// name whose owner differs from the name's owner.
func ovBruteSuffix(owners map[string]string, name string) int {
	best := 0
	for other, owner := range owners {
		if owner != owners[name] {
			best = max(best, commonSuffixBytes(name, other))
		}
	}
	return best
}

// TestOVOwnedSuffixMatchesPairwise checks the cached owner-suffix table against the pairwise loop for
// every owned component of several groups, including long suffixes, case and Unicode variants, and a
// group of more than sixty paths.
func TestOVOwnedSuffixMatchesPairwise(t *testing.T) {
	long := strings.Repeat("s", 1100)
	var big []string
	for i := 0; i < 80; i++ {
		big = append(big, fmt.Sprintf("g%d/h%d/k%d/f.kt", i%5, i%7, i%3))
	}
	batches := map[string][]string{
		"unique": func() []string {
			var out []string
			for i := 0; i < 70; i++ {
				out = append(out, fmt.Sprintf("d%03d/x%02d/f.kt", i, i%7))
			}
			return out
		}(),
		"shared":     {"a/shared/f.kt", "b/shared/f.kt", "c/shared/deep/f.kt", "c/other/shared/f.kt"},
		"same_owner": {"same/only/one/f.kt", "same/only/two/f.kt", "same/only/two/deeper/g.kt"},
		"long_suffix": {
			"p/" + long + "00/f.kt", "p/" + long + "01/f.kt", "q/" + long + "00/f.kt", "q/" + long + "x/f.kt",
		},
		"unicode_case": {"Ünï/Case/f.kt", "ünï/case/f.kt", "Ünï/cASE/f.kt", "αβ/γ/f.kt", "ΑΒ/γ/f.kt", "αβ/Γ/f.kt"},
		"large":        big,
	}
	set := astraSet(t)
	display := set.displayPaths()
	for name, paths := range batches {
		index := newFailurePathIndex()
		for _, p := range paths {
			index.add(p, display)
		}
		for base := range index.groups {
			roots := map[string]bool{"": true}
			for _, full := range index.groups[base] {
				if first, _, ok := strings.Cut(full, "/"); ok {
					roots[first] = true
				}
			}
			for root := range roots {
				owners := index.parentOwners(base, root)
				key := [2]string{base, root}
				for owned, owner := range owners {
					if owner == "" {
						continue
					}
					if got, want := index.ownedSuffix(key, owners, owned), ovBruteSuffix(owners, owned); got != want {
						t.Fatalf("%s: ownedSuffix(%q, %q) = %d, want %d", name, base, owned, got, want)
					}
				}
			}
		}
	}
}

// TestOVParentOwnersByRoot checks the root-indexed owner maps against a plain HasPrefix filter for
// every root of random batches.
func TestOVParentOwnersByRoot(t *testing.T) {
	pool := []string{"a", "b", "bb", "é", "É", "c"}
	set := astraSet(t)
	display := set.displayPaths()
	for seed := int64(1); seed <= 3; seed++ {
		rng := rand.New(rand.NewSource(seed))
		var paths []string
		for i := 0; i < 120; i++ {
			depth := 1 + rng.Intn(4)
			var parts []string
			for d := 0; d < depth; d++ {
				parts = append(parts, pool[rng.Intn(len(pool))])
			}
			base := []string{"f.kt", "g.kt"}[rng.Intn(2)]
			paths = append(paths, strings.Join(parts, "/")+"/"+base+fmt.Sprintf("%03d", i%5))
		}
		index := newFailurePathIndex()
		for _, p := range paths {
			index.add(p, display)
		}
		for base := range index.groups {
			for _, root := range append([]string{""}, pool...) {
				if got, want := index.parentOwners(base, root), ovBruteOwners(index, base, root); !reflect.DeepEqual(got, want) {
					t.Fatalf("seed %d: parentOwners(%q, %q) = %v, want %v", seed, base, root, got, want)
				}
			}
		}
	}
}

// ovInComponentMatch is a reader that takes each "…" as one or more non-slash characters and every
// other character, including "{" and "}", literally. "./" is exact.
func ovInComponentMatch(label, path string) bool {
	if strings.HasPrefix(label, "./") {
		return path == label[2:]
	}
	var b strings.Builder
	b.WriteString("^")
	for i, part := range strings.Split(label, pathEllipsis) {
		if i > 0 {
			b.WriteString(`[^/]+`)
		}
		b.WriteString(regexp.QuoteMeta(part))
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String()).MatchString(path)
}

// ovAssertResolves requires the label to name exactly want among the attempted paths under the two
// component readings, and requires that the in-component reader never names another path. A counted
// label cannot match its own file under that reader, so the own-path rule is not applied to it there.
func ovAssertResolves(t *testing.T, label, want string, attempted []string) {
	t.Helper()
	for _, loose := range []bool{false, true} {
		if got := astraResolves(label, attempted, loose); len(got) != 1 || got[0] != want {
			t.Fatalf("label %q resolves to %v (loose=%v), want only %q", label, got, loose, want)
		}
	}
	var inComponent []string
	for _, a := range attempted {
		if ovInComponentMatch(label, a) {
			inComponent = append(inComponent, a)
		}
	}
	for _, got := range inComponent {
		if got != want {
			t.Fatalf("label %q resolves to %v under the in-component reader, which names another path than %q", label, inComponent, want)
		}
	}
}

// TestOVCountedGapNeverReadsAsBraceLiteral: a counted gap must not be emitted when another attempted
// path would match it with "{n}" read as literal text. The batch adds braced literal names to the
// archived 40 fixture. Shown labels and the compact label of every attempted path must each name exactly
// one path under every reader.
func TestOVCountedGapNeverReadsAsBraceLiteral(t *testing.T) {
	items := astraFailed(astraArchived40(), astraCycle)
	items = append(items, astraFailed([]string{"r00/q{7}/f.kt", "r01/{15}/f.kt", "r02/mm{7}/f.kt"}, astraCycle)...)
	set := astraSet(t)
	content, p := astraRun(t, set, items, syscall.EIO)
	if len(p.Files) == 0 {
		t.Fatalf("no rows shown: %s", content)
	}
	attempted := make([]string, 0, len(items))
	for _, it := range items {
		attempted = append(attempted, it.disp)
	}
	target := items[0].disp
	if p.Files[0].Path == "r00/…{7}/f.kt" {
		t.Fatalf("target %q is shown as the counted gap r00/…{7}/f.kt", target)
	}
	for i, row := range p.Files {
		ovAssertResolves(t, row.Path, items[i].disp, attempted)
	}
	index := newFailurePathIndex()
	display := set.displayPaths()
	for _, it := range items {
		index.add(it.raw, display)
	}
	for _, it := range items {
		ovAssertResolves(t, index.label(it.raw, 0), it.disp, attempted)
	}
}

// ovBuildFailed builds the receipt and result of an all-failed commit, as astraRun does.
func ovBuildFailed(items []astraItem) (editing.CommitReceipt, editing.Result) {
	rec := editing.CommitReceipt{DirtyFailure: true, State: editing.CommitStatePartiallyPublished}
	res := editing.Result{CommitState: editing.CommitStatePartiallyPublished}
	for _, it := range items {
		rec.Files = append(rec.Files, editing.FileCommitReceipt{Path: it.raw, PublishState: it.state, Published: true, Wrote: true, Error: "input/output error"})
		res.Files = append(res.Files, editing.FileResult{Path: it.raw, PublishState: it.state, Published: true, Error: "input/output error"})
	}
	return rec, res
}

// BenchmarkOVRenderScale renders all-failed batches of growing size, with the same shapes used to
// measure the rescue path. Inputs are built before the timer starts.
func BenchmarkOVRenderScale(b *testing.B) {
	set, err := New(Options{Workspace: b.TempDir(), Policy: PolicyWorkspace, SessionID: "s-ov-scale"})
	if err != nil {
		b.Fatal(err)
	}
	fill := strings.Repeat(strings.Repeat("m", 170)+"/", 6)
	cases := []struct {
		name  string
		sizes []int
		path  func(i int) string
		extra []string
	}{
		{"unique_deep", []int{32, 100, 1024, 4096}, func(i int) string {
			return fmt.Sprintf("src/main/kotlin/com/example/platform/payments/gateway/adapters/stripe/Adapter%05d.kt", i)
		}, nil},
		{"samebase_modules", []int{32, 100, 1024, 4096}, func(i int) string {
			return fmt.Sprintf("services/m%05d/src/main/resources/config/application.yaml", i)
		}, nil},
		{"samebase_longsuffix", []int{32, 100, 1024, 4096}, func(i int) string {
			return fmt.Sprintf("r/x%05d/", i) + fill + "common/f.kt"
		}, nil},
		{"longname_middle", []int{32, 100, 1024, 4096}, func(i int) string {
			return "pkg/" + strings.Repeat("L", 100) + fmt.Sprintf("%05d", i) + strings.Repeat("L", 100) + ".kt"
		}, nil},
		{"blocked_literal", []int{32, 100, 1024, 4096}, func(i int) string {
			return fmt.Sprintf("r%05d…r", i) + "/" + strings.Repeat(strings.Repeat("&", 162)+"/", 4) + "f.kt"
		}, []string{"good.kt"}},
		{"blocked_ordinary_oversize", []int{32, 128, 512, 1024, 4096}, func(i int) string {
			return fmt.Sprintf("r/u%05d%s/f.kt", i, strings.Repeat("z", 3900))
		}, []string{"good.kt"}},
	}
	for _, c := range cases {
		for _, n := range c.sizes {
			b.Run(fmt.Sprintf("%s/%d", c.name, n), func(b *testing.B) {
				var paths []string
				for i := 0; i < n; i++ {
					paths = append(paths, c.path(i))
				}
				paths = append(paths, c.extra...)
				rec, res := ovBuildFailed(astraFailed(paths, astraCycle))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if out, _ := set.commitOutcome(res, rec, syscall.EIO); out.Content == "" {
						b.Fatal("empty payload")
					}
				}
			})
		}
	}
}
