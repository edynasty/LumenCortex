package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

// ErrRunDeadline is the cancellation cause of a run that reached Options.Deadline (ADR-0019 §2).
var ErrRunDeadline = errors.New("the run reached its deadline")

// Bounds of the default deadline reserve: a tenth of the run's budget, within these.
const (
	minDeadlineReserve = 30 * time.Second
	maxDeadlineReserve = 120 * time.Second
	// minAnswerMargin is the least time kept free of tool work before the deadline, for the reply that
	// follows the last tool result.
	minAnswerMargin = 10 * time.Second
)

// jobWaitTool is the job tool whose time is waiting, not tool work.
const jobWaitTool = "job_wait"

// runClock is a run's time budget: the deadline, the reserve in which the model is asked to deliver, and
// where the time went. A zero deadline means none: every method is then a no-op.
//
// The deadline is anchored on the monotonic clock when the run starts, so the remaining time only ever
// decreases, whatever the wall clock does.
type runClock struct {
	deadline time.Time
	reserve  time.Duration
	// model is the time spent in model requests (retries and their backoff included), tools the time
	// spent executing tools other than job_wait, wait the time spent in job_wait.
	model, tools, wait time.Duration
	// longestModel is the longest single model request so far; it sizes the answer margin.
	longestModel time.Duration
	// reserveNoticed: the reserve row has been appended.
	reserveNoticed bool
}

func newRunClock(deadline time.Time, reserve time.Duration, now time.Time) runClock {
	if deadline.IsZero() {
		return runClock{}
	}
	budget := deadline.Sub(now)
	if reserve <= 0 {
		reserve = clampDuration(budget/10, minDeadlineReserve, maxDeadlineReserve)
	}
	if budget > 0 && reserve > budget/2 {
		reserve = budget / 2
	}
	return runClock{deadline: now.Add(budget), reserve: reserve}
}

func (c *runClock) set() bool { return !c.deadline.IsZero() }

func (c *runClock) observeModel(d time.Duration) {
	c.model += d
	if d > c.longestModel {
		c.longestModel = d
	}
}

// clampDuration bounds d to [lo, hi]. (The package's tests declare their own min, so the builtins are
// not used here.)
func clampDuration(d, lo, hi time.Duration) time.Duration {
	if d < lo {
		return lo
	}
	if d > hi {
		return hi
	}
	return d
}

func (c *runClock) observeTool(name string, d time.Duration) {
	if name == jobWaitTool {
		c.wait += d
		return
	}
	c.tools += d
}

// answerMargin is the time before the deadline that tools may not use: one and a half times the longest
// model request so far, at least minAnswerMargin, at most the reserve.
func (c *runClock) answerMargin() time.Duration {
	margin := clampDuration(c.longestModel*3/2, minAnswerMargin, c.reserve)
	if margin > c.reserve { // a reserve below minAnswerMargin wins
		margin = c.reserve
	}
	return margin
}

// toolDeadline is the time by which a tool call must be done, so that a reply still fits before the
// deadline. ok is false when the run has no deadline.
func (c *runClock) toolDeadline() (time.Time, bool) {
	if !c.set() {
		return time.Time{}, false
	}
	return c.deadline.Add(-c.answerMargin()), true
}

func (c *runClock) remaining(now time.Time) time.Duration {
	if left := c.deadline.Sub(now); left > 0 {
		return left
	}
	return 0
}

// deadlineReport is the "deadline" member a tool result carries.
type deadlineReport struct {
	RemainingSec int64 `json:"remainingSec"`
	ReserveSec   int64 `json:"reserveSec"`
	// UsedSec splits the run's elapsed time: model requests, tool execution, and waiting on jobs.
	UsedSec struct {
		Model int64 `json:"model"`
		Tools int64 `json:"tools"`
		Wait  int64 `json:"wait"`
	} `json:"usedSec"`
}

func seconds(d time.Duration) int64 { return int64(math.Floor(d.Seconds())) }

// annotate adds the run's time report to a tool result's content: a "deadline" member when the content is
// a JSON object, a trailing runtime line otherwise. The result row is new to the transcript, so the cached
// prefix is untouched. Without a deadline the content is returned as it is.
func (c *runClock) annotate(content string, now time.Time) string {
	if !c.set() {
		return content
	}
	report := deadlineReport{RemainingSec: seconds(c.remaining(now)), ReserveSec: seconds(c.reserve)}
	report.UsedSec.Model, report.UsedSec.Tools, report.UsedSec.Wait = seconds(c.model), seconds(c.tools), seconds(c.wait)
	raw, _ := json.Marshal(report)
	trimmed := bytes.TrimSpace([]byte(content))
	if len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}' && json.Valid(trimmed) {
		body := bytes.TrimSpace(trimmed[:len(trimmed)-1])
		var b strings.Builder
		b.Grow(len(body) + len(raw) + 16)
		b.Write(body)
		if len(body) > 1 { // not "{": the object has members
			b.WriteByte(',')
		}
		b.WriteString(`"deadline":`)
		b.Write(raw)
		b.WriteByte('}')
		return b.String()
	}
	return content + "\n[runtime] deadline: " + string(raw)
}

// startNotice is the row a run with a deadline appends when it starts.
func (c *runClock) startNotice(now time.Time) protocol.Message {
	return protocol.Message{Role: "user", Origin: protocol.OriginDeadline, Content: fmt.Sprintf(
		"[runtime] This run has a deadline: about %d s from now (%s). Every tool result reports the time left in its \"deadline\" member, with the time used so far by model requests, tools and waiting. Commands are stopped before the deadline so that a final reply still fits, and the run is stopped at the deadline whether or not it has delivered. Get a working result in place early, measure an expensive experiment on a small sample before running it in full, and keep enough time to put the deliverable in place and check it.",
		seconds(c.remaining(now)), c.deadline.UTC().Format(time.RFC3339))}
}

// reserveNotice returns, once, the row appended when the time left first falls within the reserve.
func (c *runClock) reserveNotice(now time.Time) (protocol.Message, bool) {
	if !c.set() || c.reserveNoticed || c.remaining(now) > c.reserve {
		return protocol.Message{}, false
	}
	c.reserveNoticed = true
	return protocol.Message{Role: "user", Origin: protocol.OriginDeadline, Content: fmt.Sprintf(
		"[runtime] About %d s are left before this run's deadline. Use them to deliver: put the deliverable in its final place, run only a quick check of it, cancel any background job you no longer need, and give your final answer, saying what is unfinished.",
		seconds(c.remaining(now)))}, true
}
