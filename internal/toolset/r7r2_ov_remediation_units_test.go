package toolset

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"testing"

	"github.com/edynasty/LumenCortex/internal/editing"
)

// TestOVOmissionBytesMatchesJSON: omissionBytes is the exact JSON size of the two omission keys added to
// an object that already has keys (OV-04).
func TestOVOmissionBytesMatchesJSON(t *testing.T) {
	states := []editing.PublishState{
		editing.PublishExternalModification, editing.PublishPartial, editing.PublishSucceededWithPostError,
		editing.PublishUncertain, editing.PublishRollbackFailed, editing.PublishNotAttempted,
	}
	rng := rand.New(rand.NewSource(3))
	for trial := 0; trial < 500; trial++ {
		n := rng.Intn(20000)
		byState := map[editing.PublishState]int{}
		for _, s := range states {
			if rng.Intn(2) == 0 {
				byState[s] = rng.Intn(12000)
			}
		}
		// An overflow with omitted rows always has at least one omitted state, so the map is non-empty.
		if n > 0 && len(byState) == 0 {
			byState[editing.PublishPartial] = 1 + rng.Intn(12000)
		}
		base := map[string]any{"error": "x"}
		baseRaw, err := json.Marshal(base)
		if err != nil {
			t.Fatal(err)
		}
		with := map[string]any{"error": "x"}
		if n > 0 {
			with["filesOmitted"] = n
			with["filesOmittedByState"] = byState
		}
		withRaw, err := json.Marshal(with)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := omissionBytes(n, byState), len(withRaw)-len(baseRaw); n > 0 && got != want {
			t.Fatalf("omissionBytes(%d, %v) = %d, want %d (%s)", n, byState, got, want, withRaw)
		}
		if n == 0 && omissionBytes(0, byState) != 0 {
			t.Fatalf("omissionBytes(0, %v) != 0", byState)
		}
	}
}

// ovReferenceSelect is the exhaustive ground truth for selectRows: the largest subset of at most maxK
// candidates that fits by rowBudget.size, and among subsets of that size the lexicographically smallest
// sorted index list.
func ovReferenceSelect(cands []overflowRow, maxK int, b rowBudget) []overflowRow {
	bestSize := 0
	var bestIdx []int
	for mask := 0; mask < 1<<len(cands); mask++ {
		var idx []int
		cost := 0
		var picked []overflowRow
		for i := range cands {
			if mask&(1<<i) != 0 {
				idx = append(idx, i)
				cost += cands[i].cost
				picked = append(picked, cands[i])
			}
		}
		if len(idx) > maxK {
			continue
		}
		if b.size(len(idx), cost, stateCounts(picked)) > b.limit {
			continue
		}
		if len(idx) > bestSize || (len(idx) == bestSize && lessIdx(idx, bestIdx)) {
			bestSize = len(idx)
			bestIdx = idx
		}
	}
	var out []overflowRow
	for _, i := range bestIdx {
		out = append(out, cands[i])
	}
	return out
}

func lessIdx(a, b []int) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// TestOVSelectRowsMatchesExhaustive checks selectRows against exhaustive enumeration on random instances
// with prior rows, shown counts, and budgets around the boundary (OV-06).
func TestOVSelectRowsMatchesExhaustive(t *testing.T) {
	stateSet := []editing.PublishState{
		editing.PublishExternalModification, editing.PublishPartial, editing.PublishSucceededWithPostError,
		editing.PublishUncertain, editing.PublishRollbackFailed, editing.PublishNotAttempted, editing.PublishSucceededAndVerified,
	}
	rng := rand.New(rand.NewSource(11))
	for trial := 0; trial < 320; trial++ {
		n := rng.Intn(15)
		var cands []overflowRow
		keys := map[string]bool{}
		for len(cands) < n {
			key := fmt.Sprintf("k%02d/%03d", rng.Intn(40), rng.Intn(1000))
			if keys[key] {
				continue
			}
			keys[key] = true
			cands = append(cands, overflowRow{at: len(cands), key: key, state: stateSet[rng.Intn(len(stateSet))], cost: 80 + rng.Intn(321)})
		}
		sort.Slice(cands, func(i, j int) bool { return cands[i].key < cands[j].key })
		shown := map[editing.PublishState]int{}
		stateTotal := map[editing.PublishState]int{}
		priorRows, priorBytes := rng.Intn(4), 0
		for i := 0; i < priorRows; i++ {
			s := stateSet[rng.Intn(len(stateSet))]
			shown[s]++
			stateTotal[s]++
			priorBytes += 100 + rng.Intn(200)
		}
		for _, c := range cands {
			stateTotal[c.state]++
		}
		for _, s := range stateSet {
			stateTotal[s] += rng.Intn(3)
		}
		total := 0
		for _, v := range stateTotal {
			total += v
		}
		base := 200 + rng.Intn(300)
		b := rowBudget{base: base, total: total, rows: priorRows, rowBytes: priorBytes, shown: shown, stateTotal: stateTotal}
		b.limit = base + priorBytes + rng.Intn(2600)
		maxK := 1 + rng.Intn(14)
		got := selectRows(cands, maxK, b)
		want := ovReferenceSelect(cands, maxK, b)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("trial %d: selectRows = %v, exhaustive = %v (limit %d, maxK %d)", trial, got, want, b.limit, maxK)
		}
	}
}
