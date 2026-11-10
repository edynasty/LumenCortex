package editing

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

const (
	// maxDiffEdits bounds the edit distance a relocation accepts. Beyond it the edit is refused with
	// "stale" rather than relocated on a guess (Myers is O((N+M)D), so the bound bounds the work).
	maxDiffEdits = 256
	// maxDiffLines bounds the total line count a relocation or a preview diff examines.
	maxDiffLines = 200_000
	// diffContext is how many unchanged lines a preview hunk shows around a change.
	diffContext = 3
)

// MatchLines maps every base line (0-based index) to the current line holding the same text in a
// longest common subsequence, or -1 when the base line has no counterpart. A base line that is absent
// from the map was changed, deleted or moved, so an edit anchored to it cannot be relocated safely.
//
// The algorithm is Myers' O((N+M)D) greedy diff with its edit distance bounded by maxDiffEdits: a file
// that changed more than that is refused instead of relocated.
func MatchLines(base, cur []string) ([]int, bool) {
	n, m := len(base), len(cur)
	mapping := make([]int, n)
	for i := range mapping {
		mapping[i] = -1
	}
	if n == 0 || m == 0 {
		return mapping, true
	}
	if n+m > maxDiffLines {
		return mapping, false
	}
	maxD := n + m
	if maxD > maxDiffEdits {
		maxD = maxDiffEdits
	}
	// v holds the furthest x reached on diagonal k, at index offset+k. offset is one past the widest
	// diagonal so k+1 and k-1 are always in range.
	offset := maxD + 1
	v := make([]int, 2*maxD+3)
	trace := make([][]int, 0, maxD+1)
	for d := 0; d <= maxD; d++ {
		trace = append(trace, append([]int(nil), v...))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1]
			} else {
				x = v[offset+k-1] + 1
			}
			y := x - k
			for x < n && y < m && base[x] == cur[y] {
				x++
				y++
			}
			v[offset+k] = x
			if x >= n && y >= m {
				backtrack(mapping, trace, offset, d, n, m)
				return mapping, true
			}
		}
	}
	return mapping, false
}

// backtrack walks the trace back from (x, y) and records the diagonal steps: those are the matched
// lines. trace[d] is the state before round d, so a step at distance d is read from it.
func backtrack(mapping []int, trace [][]int, offset, d, x, y int) {
	for ; d > 0; d-- {
		v := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := v[offset+prevK]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			mapping[x-1] = y - 1
			x--
			y--
		}
		x, y = prevX, prevY
	}
	for x > 0 && y > 0 {
		mapping[x-1] = y - 1
		x--
		y--
	}
}

// DiffStat counts the changed lines a preview reports.
type DiffStat struct {
	Added   int `json:"added"`
	Removed int `json:"removed"`
}

// UnifiedDiff renders a unified diff between two texts. Identical texts produce no output. When the two
// sides differ beyond the relocation budget, it degrades to "every old line removed, every new line
// added", which still describes the result exactly.
func UnifiedDiff(path string, before, after []byte) string {
	if bytes.Equal(before, after) {
		return ""
	}
	baseLines, nextLines := DecodeDoc(before).Lines, DecodeDoc(after).Lines
	mapping, ok := MatchLines(baseLines, nextLines)
	if !ok {
		mapping = make([]int, len(baseLines))
		for i := range mapping {
			mapping[i] = -1
		}
	}
	ops := diffOps(baseLines, nextLines, mapping)
	changes := make([]int, 0, len(ops))
	for i, op := range ops {
		if !op.equal {
			changes = append(changes, i)
		}
	}
	if len(changes) == 0 {
		return ""
	}
	type span struct{ start, end int }
	var spans []span
	for _, index := range changes {
		start, end := index-diffContext, index+diffContext+1
		if start < 0 {
			start = 0
		}
		if end > len(ops) {
			end = len(ops)
		}
		if len(spans) > 0 && start <= spans[len(spans)-1].end {
			spans[len(spans)-1].end = max(spans[len(spans)-1].end, end)
			continue
		}
		spans = append(spans, span{start: start, end: end})
	}
	// baseAt[i] and nextAt[i] are the 1-based line numbers at which ops[i] starts.
	baseAt, nextAt := make([]int, len(ops)+1), make([]int, len(ops)+1)
	baseAt[0], nextAt[0] = 1, 1
	for i, op := range ops {
		baseAt[i+1], nextAt[i+1] = baseAt[i], nextAt[i]
		if op.equal || op.kind == 'd' {
			baseAt[i+1]++
		}
		if op.equal || op.kind == 'a' {
			nextAt[i+1]++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", path, path)
	for _, s := range spans {
		baseCount, nextCount := 0, 0
		for _, op := range ops[s.start:s.end] {
			switch op.kind {
			case ' ':
				baseCount++
				nextCount++
			case 'd':
				baseCount++
			case 'a':
				nextCount++
			}
		}
		baseFrom, nextFrom := baseAt[s.start], nextAt[s.start]
		if baseCount == 0 {
			baseFrom--
		}
		if nextCount == 0 {
			nextFrom--
		}
		fmt.Fprintf(&b, "@@ -%s +%s @@\n", hunkRange(baseFrom, baseCount), hunkRange(nextFrom, nextCount))
		for _, op := range ops[s.start:s.end] {
			switch op.kind {
			case 'd':
				b.WriteByte('-')
			case 'a':
				b.WriteByte('+')
			default:
				b.WriteByte(' ')
			}
			b.WriteString(op.text)
			b.WriteString("\n")
		}
	}
	return b.String()
}

func hunkRange(start, count int) string {
	if count == 1 {
		return strconv.Itoa(start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}

type diffOp struct {
	kind  byte // ' ', 'd', 'a'
	text  string
	equal bool
}

// diffOps turns the line mapping into an ordered edit script.
func diffOps(base, cur []string, mapping []int) []diffOp {
	ops := make([]diffOp, 0, len(base)+len(cur))
	i, j := 0, 0
	for i < len(base) || j < len(cur) {
		switch {
		case i < len(base) && mapping[i] < 0:
			ops = append(ops, diffOp{kind: 'd', text: base[i]})
			i++
		case i < len(base) && mapping[i] == j:
			ops = append(ops, diffOp{kind: ' ', text: base[i], equal: true})
			i++
			j++
		case i < len(base) && mapping[i] > j, i >= len(base):
			ops = append(ops, diffOp{kind: 'a', text: cur[j]})
			j++
		default:
			ops = append(ops, diffOp{kind: 'd', text: base[i]})
			i++
		}
	}
	return ops
}

// CountDiff reports how many lines a change adds and removes.
func CountDiff(before, after []byte) DiffStat {
	baseLines, nextLines := DecodeDoc(before).Lines, DecodeDoc(after).Lines
	mapping, ok := MatchLines(baseLines, nextLines)
	if !ok {
		return DiffStat{Added: len(nextLines), Removed: len(baseLines)}
	}
	removed := 0
	for _, line := range mapping {
		if line < 0 {
			removed++
		}
	}
	return DiffStat{Added: len(nextLines) - (len(baseLines) - removed), Removed: removed}
}
