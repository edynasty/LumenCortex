package agent

import (
	"context"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/window"
	"github.com/edynasty/LumenCortex/protocol"
)

// runTranscript is the source of a run's request windows (design §5).
//
// transcript holds the rows from the Task State's windowStartSeq on, oldest first, with their seqs. It
// is read once when the run starts and extended with every row the run appends. While a run holds the
// session's run lock nothing else appends rows or moves windowStartSeq (design §4.2, §4.4), so it
// stays equal to the stored transcript. Leading groups that no window can include are dropped from
// memory; trimmed counts them, so dropped-group counts still cover everything from windowStartSeq.
//
// transcriptCap is the transcript budget a capacity retry left for a model; it holds until the run
// ends (design §5).
type runTranscript struct {
	transcript    []window.Entry
	trimmed       int
	transcriptCap map[config.ModelRef]int
}

// load reads the transcript from the run's windowStartSeq.
func (r *run) load(ctx context.Context, store Store, sessionID string) error {
	return store.Transcript(ctx, sessionID, r.ts.WindowStartSeq, func(seq int64, m protocol.Message) error {
		r.appendRow(seq, m)
		return nil
	})
}

// appendRow records a row the run wrote, so the next window sees it.
func (r *run) appendRow(seq int64, m protocol.Message) {
	r.transcript = append(r.transcript, window.Entry{Seq: seq, Msg: m})
}

// groups splits the transcript into window groups after dropping the leading ones no window can
// include. Selection takes groups newest first and stops at the first one that does not fit, and no
// budget exceeds maxContext, so the groups older than the first group at which the newest groups'
// cost exceeds maxContext are never sent. That first group is kept so a newest group larger than
// every context still overflows instead of leaving an empty transcript.
func (r *run) groups(maxContext int) ([][]window.Entry, error) {
	groups, err := window.Groups(r.transcript)
	if err != nil {
		return nil, err
	}
	keep, used := 0, 0
	for k := len(groups) - 1; k >= 0; k-- {
		used += window.GroupTokens(groups[k])
		if used > maxContext {
			keep = k
			break
		}
	}
	if keep == 0 {
		return groups, nil
	}
	rows := 0
	for _, g := range groups[:keep] {
		rows += len(g)
	}
	n := copy(r.transcript, r.transcript[rows:])
	clear(r.transcript[n:])
	r.transcript = r.transcript[:n]
	r.trimmed += keep
	return window.Groups(r.transcript)
}

// budget is the window budget of one request to c: its context, its output reservation, and its
// margin, raised to keep the transcript within a capacity retry's limit for c once one was taken.
func (r *run) budget(c Candidate, maxTokens, fixed int) window.Budget {
	b := window.Budget{Context: c.Context, Output: OutputBudget(maxTokens, c), Margin: Margin(c.Context)}
	if limit, ok := r.transcriptCap[c.Ref]; ok {
		b.Margin = max(b.Margin, b.Context-b.Output-fixed-limit)
	}
	return b
}

// capTranscript keeps the transcript of later requests to c within the budget a capacity retry chose.
func (r *run) capTranscript(ref config.ModelRef, retry window.Budget, fixed int) {
	if r.transcriptCap == nil {
		r.transcriptCap = map[config.ModelRef]int{}
	}
	r.transcriptCap[ref] = retry.Context - retry.Output - retry.Margin - fixed
}

// maxContext is the largest context of the run's models: no window holds more transcript tokens.
func (l *Loop) maxContext() int {
	largest := 0
	for _, m := range l.Models {
		largest = max(largest, m.Context)
	}
	return largest
}
