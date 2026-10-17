package agent

import (
	"context"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/window"
	"github.com/edynasty/LumenCortex/protocol"
)

// runTranscript is the source of a run's request windows (design §5) and its run memory (§4.6).
//
// transcript holds the rows from the Task State's windowStartSeq on, oldest first, with their seqs. It
// is read once when the run starts and extended with every row the run appends. While a run holds the
// session's run lock nothing else appends rows or moves windowStartSeq (design §4.2, §4.4), so it
// stays equal to the stored transcript. Leading groups that no window can include are dropped from
// memory; trimmed counts them, so dropped-group counts still cover everything from windowStartSeq.
//
// transcriptCap is the transcript budget a capacity retry left for a model; it holds until the run
// ends (design §5).
//
// turn is the run memory of the current turn: for each assistant row this run produced after the
// last user row, the model that produced it and the echo the adapter returned with it. It is never
// written anywhere and goes when the run returns. A user row ends the turn and empties it, so it holds
// at most one turn of the transcript in memory; the window counts an entry's echo toward its row
// (Entry.EchoTokens) only while the entry is in it.
type runTranscript struct {
	transcript    []window.Entry
	trimmed       int
	transcriptCap map[config.ModelRef]int
	turn          map[int64]turnEntry
}

type turnEntry struct {
	model config.ModelRef
	echo  protocol.Echo
}

// load reads the transcript from the run's windowStartSeq.
func (r *run) load(ctx context.Context, store Store, sessionID string) error {
	return store.Transcript(ctx, sessionID, r.ts.WindowStartSeq, func(seq int64, m protocol.Message) error {
		r.appendRow(seq, m)
		return nil
	})
}

// appendRow records a row, read at load or written by the run, so the next window sees it. A user row
// opens a new turn: the previous turn's echo is dropped and no longer counts toward its rows.
func (r *run) appendRow(seq int64, m protocol.Message) {
	if m.Role == "user" && len(r.turn) > 0 {
		for i := len(r.transcript) - 1; i >= 0 && r.transcript[i].Msg.Role != "user"; i-- {
			r.transcript[i].EchoTokens = 0
		}
		clear(r.turn)
	}
	r.transcript = append(r.transcript, window.Entry{Seq: seq, Msg: m})
}

// appendAssistant records an assistant row this run received from model, with its echo.
func (r *run) appendAssistant(seq int64, m protocol.Message, model config.ModelRef, echo protocol.Echo) {
	r.transcript = append(r.transcript, window.Entry{Seq: seq, Msg: m, EchoTokens: echo.Tokens()})
	if r.turn == nil {
		r.turn = map[int64]turnEntry{}
	}
	r.turn[seq] = turnEntry{model: model, echo: echo}
}

// turnRows is what the turn rule reads: the rows after the last user row, each assistant row with
// the model that produced it in this run, or "" when this run did not.
func (r *run) turnRows() []TurnRow {
	start := len(r.transcript)
	for start > 0 && r.transcript[start-1].Msg.Role != "user" {
		start--
	}
	if start == len(r.transcript) {
		return nil
	}
	rows := make([]TurnRow, 0, len(r.transcript)-start)
	for _, e := range r.transcript[start:] {
		row := TurnRow{Role: e.Msg.Role}
		if e.Msg.Role == "assistant" {
			row.Producer = r.turn[e.Seq].model
		}
		rows = append(rows, row)
	}
	return rows
}

// requestEcho is ProviderRequest.Echo for built, a window of the transcript: aligned with its
// messages, it holds the echo of each current-turn assistant row target produced in this run. The
// window's transcript rows are the contiguous suffix from built.FirstSeq and end its messages (I4).
// nil when there is none.
func (r *run) requestEcho(built window.Result, target config.ModelRef) []protocol.Echo {
	if len(r.turn) == 0 || built.FirstSeq < 0 {
		return nil
	}
	first := len(r.transcript)
	for first > 0 && r.transcript[first-1].Seq >= built.FirstSeq {
		first--
	}
	offset := len(built.Messages) - (len(r.transcript) - first)
	var out []protocol.Echo
	for k, e := range r.transcript[first:] {
		t, ok := r.turn[e.Seq]
		if !ok || t.model != target || t.echo.IsZero() {
			continue
		}
		if out == nil {
			out = make([]protocol.Echo, len(built.Messages))
		}
		out[offset+k] = t.echo
	}
	return out
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
	for seq := range r.turn {
		if n == 0 || seq < r.transcript[0].Seq {
			delete(r.turn, seq)
		}
	}
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
