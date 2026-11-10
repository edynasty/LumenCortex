package editing

import (
	"fmt"
	"sort"
	"strings"
)

// AnchorOp is a Hashline operation as the model writes it: a line range addressed by "N:hhh" anchors
// of the snapshot named by the edit's revision.
type AnchorOp struct {
	// Op is replace, delete, insert_after or insert_before.
	Op string `json:"op"`
	// Start and End are anchors "N:hhh"; End defaults to Start.
	Start string `json:"start"`
	End   string `json:"end,omitempty"`
	// Text is the replacement or inserted text.
	Text string `json:"text,omitempty"`
}

// opRank orders operations that start on the same line: an insertion before the line applies before a
// replacement of it, which applies before an insertion after it.
func opRank(kind string) int {
	switch kind {
	case OpInsertBefore:
		return 0
	case OpInsertAfter:
		return 2
	}
	return 1
}

// ParseOps resolves anchored operations against a snapshot: every anchor must name a line of the
// snapshot with the hash the model was shown, and a line this session was actually shown. Nothing here
// touches the workspace.
func ParseOps(snap *Snapshot, ops []AnchorOp) ([]LineOp, error) {
	if snap == nil {
		return nil, &Error{Code: CodeUnknownRev, Detail: "the revision was not read in this session"}
	}
	if snap.Doc.Rev == "" {
		return nil, &Error{Code: CodeUnknownRev, Path: snap.Path, Rev: snap.Rev,
			Detail: "that revision was read without anchors, so its lines are not available"}
	}
	if len(ops) == 0 {
		return nil, &Error{Code: CodeInvalid, Path: snap.Path, Detail: "ops must contain at least one operation"}
	}
	out := make([]LineOp, 0, len(ops))
	for i, op := range ops {
		kind := strings.TrimSpace(op.Op)
		switch kind {
		case OpReplace, OpDelete, OpInsertAfter, OpInsertBefore:
		default:
			return nil, &Error{Code: CodeInvalid, Path: snap.Path, Detail: fmt.Sprintf("op %d: unknown op %q", i+1, op.Op)}
		}
		start, err := resolveAnchor(snap, op.Start, i)
		if err != nil {
			return nil, err
		}
		end := start
		if strings.TrimSpace(op.End) != "" {
			if kind == OpInsertAfter || kind == OpInsertBefore {
				return nil, &Error{Code: CodeInvalid, Path: snap.Path, Detail: fmt.Sprintf("op %d: %s addresses one line, not a range", i+1, kind)}
			}
			if end, err = resolveAnchor(snap, op.End, i); err != nil {
				return nil, err
			}
		}
		if end.Line < start.Line {
			return nil, &Error{Code: CodeInvalid, Path: snap.Path, Detail: fmt.Sprintf("op %d: end line %d is before start line %d", i+1, end.Line, start.Line)}
		}
		out = append(out, LineOp{Kind: kind, Start: start.Line, End: end.Line, Text: SplitText(op.Text)})
	}
	if err := checkOverlap(out, snap.Path); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Start != out[j].Start {
			return out[i].Start < out[j].Start
		}
		return opRank(out[i].Kind) < opRank(out[j].Kind)
	})
	return out, nil
}

// resolveAnchor checks one anchor against the snapshot: the line exists, its hash is the one the model
// was shown, and the line was shown at all.
func resolveAnchor(snap *Snapshot, raw string, index int) (Anchor, error) {
	anchor, err := ParseAnchor(raw)
	if err != nil {
		return Anchor{}, &Error{Code: CodeInvalid, Path: snap.Path, Detail: fmt.Sprintf("op %d: %v", index+1, err)}
	}
	if anchor.Line < 1 || anchor.Line > len(snap.Doc.Lines) {
		return Anchor{}, &Error{Code: CodeAnchorMismatch, Path: snap.Path, Rev: snap.Rev,
			Detail: fmt.Sprintf("op %d: line %d is outside the file (%d lines)", index+1, anchor.Line, len(snap.Doc.Lines))}
	}
	if got := LineHash(snap.Doc.Lines[anchor.Line-1]); got != anchor.Hash {
		return Anchor{}, &Error{Code: CodeAnchorMismatch, Path: snap.Path, Rev: snap.Rev,
			Detail: fmt.Sprintf("op %d: line %d hashes %s at rev %s, not %s", index+1, anchor.Line, got, snap.Rev, anchor.Hash)}
	}
	if !snap.SeenLine(anchor.Line) {
		return Anchor{}, &Error{Code: CodeNotSeen, Path: snap.Path, Rev: snap.Rev,
			Detail: fmt.Sprintf("op %d: line %d was never shown to this session", index+1, anchor.Line)}
	}
	return anchor, nil
}

// checkOverlap refuses a batch in which one operation touches a line another operation already covers.
// Two insertions at the same line do not conflict: their order is fixed by opRank.
func checkOverlap(ops []LineOp, path string) error {
	sorted := append([]LineOp(nil), ops...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Start != sorted[j].Start {
			return sorted[i].Start < sorted[j].Start
		}
		return opRank(sorted[i].Kind) < opRank(sorted[j].Kind)
	})
	for i := 1; i < len(sorted); i++ {
		prev, cur := sorted[i-1], sorted[i]
		if cur.IsInsert() {
			// An insertion inside a replaced or deleted range would be applied to lines that no longer
			// exist: refuse it rather than guess.
			if !prev.IsInsert() && cur.Start >= prev.Start && cur.Start <= prev.End {
				return &Error{Code: CodeOverlap, Path: path,
					Detail: fmt.Sprintf("op %s %d falls inside %s %d-%d", cur.Kind, cur.Start, prev.Kind, prev.Start, prev.End)}
			}
			continue
		}
		end := prev.End
		if prev.IsInsert() {
			end = prev.Start
			if prev.Kind == OpInsertAfter {
				end = prev.Start + 1
			}
		}
		if cur.Start <= end {
			return &Error{Code: CodeOverlap, Path: path,
				Detail: fmt.Sprintf("ops overlap at line %d (%s %d-%d and %s %d-%d)", cur.Start, prev.Kind, prev.Start, prev.End, cur.Kind, cur.Start, cur.End)}
		}
	}
	return nil
}

// lineCheckRange is the set of snapshot lines an op depends on: the lines it touches plus one
// neighbour on each side, so lines inserted at the boundary also refuse the relocation.
func lineCheckRange(op LineOp, total int) []int {
	lo, hi := op.Start, op.End
	switch op.Kind {
	case OpInsertBefore:
		hi = op.Start - 1
	case OpInsertAfter:
		lo = op.Start + 1
	}
	if lo < 1 {
		lo = 1
	}
	if hi > total {
		hi = total
	}
	if hi < lo {
		return nil
	}
	out := make([]int, 0, hi-lo+3)
	for line := lo - 1; line <= hi+1; line++ {
		if line >= 1 && line <= total {
			out = append(out, line)
		}
	}
	return out
}

// rebaseLineOps relocates ops from a snapshot to the current document. Every line an op depends on,
// and its neighbours, must still map to the current file contiguously; otherwise the file changed in
// or next to the edited range and the edit is refused instead of guessed.
func rebaseLineOps(base, cur Doc, ops []LineOp) ([]LineOp, map[int]int, error) {
	mapping, ok := MatchLines(base.Lines, cur.Lines)
	if !ok {
		return nil, nil, fmt.Errorf("the file changed too much to relocate an edit made at rev %s", base.Rev)
	}
	moved := map[int]int{}
	out := make([]LineOp, 0, len(ops))
	for _, op := range ops {
		check := lineCheckRange(op, len(base.Lines))
		previous := -1
		for _, line := range check {
			target := mapping[line-1]
			if target < 0 {
				return nil, nil, fmt.Errorf("snapshot line %d changed since rev %s", line, base.Rev)
			}
			if previous >= 0 && target != previous+1 {
				return nil, nil, fmt.Errorf("lines were inserted or moved inside the edited range %d-%d since rev %s", op.Start, op.End, base.Rev)
			}
			previous = target
		}
		relocated := op
		relocated.Start = mapping[op.Start-1] + 1
		relocated.End = mapping[op.End-1] + 1
		if relocated.Start != op.Start {
			moved[op.Start] = relocated.Start
		}
		out = append(out, relocated)
	}
	return out, moved, nil
}
