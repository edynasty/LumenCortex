package runtime

import (
	"context"
	"encoding/json"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/threadctx"
	"github.com/edynasty/LumenCortex/protocol"
)

// Bounds of one transcript page read_thread returns (plan T2-S1, DATA_AND_FAILURES §4: a page never
// splits a tool group; one group over the bound is returned as oversize and incomplete). Initial
// mechanism bounds, not tuned values.
const (
	transcriptMaxMessages  = 40
	transcriptMaxBytes     = 32 << 10
	transcriptMessageBytes = 4 << 10
	transcriptArgsBytes    = 1 << 10
	// transcriptGroupScan bounds the rows read further back to find where a group larger than a page
	// starts.
	transcriptGroupScan = 500
	// transcriptOverhead is what one message costs on a page besides its text.
	transcriptOverhead = 64
)

// transcriptRequest is what read_thread asked for: a page (want) of the messages before seq before,
// the newest page when before is negative.
type transcriptRequest struct {
	want   bool
	before int64
}

// threadTranscript is one page of a session's visible messages, oldest first. Only what the provider
// was shown is projected: role, text, tool calls and the tool call a result answers. Nothing else a
// stored row may hold (an echo, reasoning) is read into it.
type threadTranscript struct {
	Messages []transcriptMessage `json:"messages"`
	// NextBefore is the seq to pass as before for the next older page, -1 when there is none.
	NextBefore int64 `json:"nextBefore"`
	HasMore    bool  `json:"hasMore"`
	// Complete is false when a message on the page is cut or the page is part of an oversize group.
	Complete bool `json:"complete"`
	// Oversize marks a page that is one tool group larger than a page: GroupFirst..GroupLast locate the
	// whole group; the page holds its first messages, cut.
	Oversize   bool  `json:"oversize,omitempty"`
	GroupFirst int64 `json:"groupFirst,omitempty"`
	GroupLast  int64 `json:"groupLast,omitempty"`
}

type transcriptMessage struct {
	Seq        int64            `json:"seq"`
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	ToolCalls  []transcriptCall `json:"toolCalls,omitempty"`
	ToolCallID string           `json:"toolCallId,omitempty"`
	Origin     string           `json:"origin,omitempty"`
	// Cut marks a message whose text or arguments were shortened on this page.
	Cut bool `json:"cut,omitempty"`
}

type transcriptCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args,omitempty"`
}

// transcriptPage reads the page of sessionID's messages before before (the newest when negative):
// the newest whole tool groups that fit the bounds. A group starts at any row that is not a tool
// result; the results that follow belong to it.
func (e *Engine) transcriptPage(ctx context.Context, sessionID string, before int64) (threadTranscript, error) {
	rows, err := e.store.MessagesBefore(ctx, sessionID, before, transcriptMaxMessages)
	if err != nil {
		return threadTranscript{}, err
	}
	page := threadTranscript{NextBefore: -1, Complete: true, Messages: []transcriptMessage{}}
	if len(rows) == 0 {
		return page, nil
	}
	groups := groupRows(rows)
	// Take whole groups, newest first, while they fit; a leading partial group (its start is older
	// than the rows read) is never taken: the next page starts with it.
	count, size, first := 0, 0, len(groups)
	for i := len(groups) - 1; i >= 0; i-- {
		g := groups[i]
		if !g.started {
			break
		}
		n, b := len(g.rows), g.bytes(transcriptMessageBytes)
		if count+n > transcriptMaxMessages || size+b > transcriptMaxBytes {
			break
		}
		count, size, first = count+n, size+b, i
	}
	if first == len(groups) {
		// The newest group alone does not fit, or its start is older than a page: return it oversize.
		return e.oversizePage(ctx, sessionID, before, groups[len(groups)-1])
	}
	for _, g := range groups[first:] {
		for _, r := range g.rows {
			m := projectMessage(r, transcriptMessageBytes)
			page.Complete = page.Complete && !m.Cut
			page.Messages = append(page.Messages, m)
		}
	}
	page.NextBefore = page.Messages[0].Seq
	return e.finishPage(ctx, sessionID, page)
}

// oversizePage returns the first messages of the group that ends the rows before before, each cut to
// a share of the page, located by the whole group's first and last seq.
func (e *Engine) oversizePage(ctx context.Context, sessionID string, before int64, newest rowGroup) (threadTranscript, error) {
	g := newest
	if !g.started {
		rows, err := e.store.MessagesBefore(ctx, sessionID, before, transcriptGroupScan)
		if err != nil {
			return threadTranscript{}, err
		}
		groups := groupRows(rows)
		g = groups[len(groups)-1]
	}
	page := threadTranscript{Oversize: true, Complete: false, GroupFirst: g.rows[0].Seq, GroupLast: g.rows[len(g.rows)-1].Seq}
	shown := g.rows[:min(len(g.rows), transcriptMaxMessages)]
	share := max(256, transcriptMaxBytes/len(shown)-transcriptOverhead)
	for _, r := range shown {
		page.Messages = append(page.Messages, projectMessage(r, share))
	}
	page.NextBefore = page.GroupFirst
	return e.finishPage(ctx, sessionID, page)
}

// finishPage sets HasMore, and NextBefore to -1 when nothing is older than the page.
func (e *Engine) finishPage(ctx context.Context, sessionID string, page threadTranscript) (threadTranscript, error) {
	older, err := e.store.MessagesBefore(ctx, sessionID, page.NextBefore, 1)
	if err != nil {
		return threadTranscript{}, err
	}
	page.HasMore = len(older) > 0
	if !page.HasMore {
		page.NextBefore = -1
	}
	return page, nil
}

// rowGroup is one assistant or user row and the tool results after it. started is false for the
// tool results at the head of the rows read, whose own start is older.
type rowGroup struct {
	rows    []session.Message
	started bool
}

func (g rowGroup) bytes(perMessage int) int {
	n := 0
	for _, r := range g.rows {
		n += min(len(r.JSON), perMessage+transcriptArgsBytes) + transcriptOverhead
	}
	return n
}

func groupRows(rows []session.Message) []rowGroup {
	var groups []rowGroup
	for _, r := range rows {
		if r.Role == "tool" && len(groups) > 0 {
			groups[len(groups)-1].rows = append(groups[len(groups)-1].rows, r)
			continue
		}
		groups = append(groups, rowGroup{rows: []session.Message{r}, started: r.Role != "tool"})
	}
	return groups
}

// projectMessage is the visible part of one stored row, its text cut to maxBytes and each call's
// arguments to transcriptArgsBytes. A row that does not decode is shown by role and seq only, cut.
func projectMessage(r session.Message, maxBytes int) transcriptMessage {
	out := transcriptMessage{Seq: r.Seq, Role: r.Role}
	var m protocol.Message
	if err := json.Unmarshal(r.JSON, &m); err != nil {
		out.Cut = true
		return out
	}
	out.Content = threadctx.Cut(m.Content, maxBytes)
	out.Cut = len(out.Content) != len(m.Content)
	out.ToolCallID, out.Origin = m.ToolCallID, m.Origin
	for _, c := range m.ToolCalls {
		args := threadctx.Cut(string(c.Arguments), transcriptArgsBytes)
		out.Cut = out.Cut || len(args) != len(c.Arguments)
		out.ToolCalls = append(out.ToolCalls, transcriptCall{ID: c.ID, Name: c.Name, Args: args})
	}
	return out
}
