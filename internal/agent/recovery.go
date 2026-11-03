package agent

import (
	"errors"
	"fmt"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/protocol"
)

// Recovery is how a step waits out availability failures that no failover candidate answered (design
// §4.5; the runtime's execution.retry). Once the step's failover chain has ended without an answer and
// at least one candidate failed with availability, every further failure is followed by a wait and a new
// admitted request of the same step: the transcript is unchanged and no tool ran, because a failed
// request returned nothing to execute. Only candidates that failed with availability are retried, each
// no earlier than its own Retry-After; a candidate that capacity rejected for good is not sent again.
// The wait is an exponential backoff from BaseDelay, doubling up to MaxDelay, scaled by a 75-100% jitter
// so concurrent runs do not retry in lockstep, or the chosen candidate's remaining Retry-After when that
// is longer. Recovery sets no request, token or cost limit: the run's deadline and cancellation bound it,
// and MaxOutage bounds one continuous outage.
//
// The zero value never retries.
type Recovery struct {
	BaseDelay time.Duration
	MaxDelay  time.Duration
	// MaxOutage is the longest continuous outage, from the first failed request to the next send; 0
	// leaves only the deadline and cancellation.
	MaxOutage time.Duration
}

// RecoveryFrom is the policy of execution.retry for a run that has a deadline or not. A run without a
// deadline gets a finite outage window even when none is configured: nothing else would end it.
func RecoveryFrom(r config.Retry, hasDeadline bool) Recovery {
	if r.Disabled {
		return Recovery{}
	}
	base := time.Duration(config.DefaultRetryBaseDelayMs) * time.Millisecond
	if r.BaseDelayMs > 0 {
		base = time.Duration(r.BaseDelayMs) * time.Millisecond
	}
	maxDelay := time.Duration(config.DefaultRetryMaxDelayMs) * time.Millisecond
	if r.MaxDelayMs > 0 {
		maxDelay = time.Duration(r.MaxDelayMs) * time.Millisecond
	}
	if maxDelay < base {
		maxDelay = base
	}
	outage := time.Duration(r.MaxOutageSeconds) * time.Second
	if outage == 0 && !hasDeadline {
		outage = config.DefaultRetryMaxOutageNoDeadlineS * time.Second
	}
	return Recovery{BaseDelay: base, MaxDelay: maxDelay, MaxOutage: outage}
}

func (p Recovery) enabled() bool { return p.BaseDelay > 0 }

// backoff is the wait before retry n (1-based): BaseDelay doubled n-1 times, at most MaxDelay, scaled to
// 75-100% by jitter in [0, 1).
func (p Recovery) backoff(n int, jitter float64) time.Duration {
	d := p.BaseDelay
	for i := 1; i < n && d < p.MaxDelay; i++ {
		d *= 2
	}
	if d > p.MaxDelay {
		d = p.MaxDelay
	}
	return time.Duration(float64(d) * (0.75 + 0.25*jitter))
}

// Why a step stopped recovering.
const (
	RecoveryStopDisabled    = "retry_disabled"
	RecoveryStopNoCandidate = "no_candidate"
	RecoveryStopOutage      = "outage_window"
	RecoveryStopDeadline    = "deadline"
)

// stopFor returns why the step stops instead of waiting wait before it sends candidate pick again:
// retrying is disabled, no candidate is left to retry (pick < 0), the wait would take the outage past
// MaxOutage, or it would end at or after the deadline. "" means wait.
func (p Recovery) stopFor(pick int, wait time.Duration, start, now, deadline time.Time) string {
	switch at := now.Add(wait); {
	case !p.enabled():
		return RecoveryStopDisabled
	case pick < 0:
		return RecoveryStopNoCandidate
	case p.MaxOutage > 0 && at.Sub(start) > p.MaxOutage:
		return RecoveryStopOutage
	case !deadline.IsZero() && !at.Before(deadline):
		return RecoveryStopDeadline
	}
	return ""
}

// outage is one step's run of availability failures since its last answer.
type outage struct {
	start    time.Time
	failures int
	retries  int
	// ready is when each candidate (by index) that failed with availability may be sent again: its
	// failure time plus its Retry-After.
	ready map[int]time.Time
	// last is the newest availability error.
	last error
}

func (o *outage) fail(i int, sentAt, failedAt time.Time, retryAfter time.Duration, err error) {
	if o.failures == 0 {
		o.start = sentAt
	}
	o.failures++
	if o.ready == nil {
		o.ready = map[int]time.Time{}
	}
	o.ready[i] = failedAt.Add(retryAfter)
	o.last = err
}

// readyIn lists, per candidate index, how long until it may be retried (0 when it may now), or -1 when
// it is not retried: it never failed with availability, or capacity rejected it for good.
func (o *outage) readyIn(n int, exhausted map[int]bool, now time.Time) []time.Duration {
	out := make([]time.Duration, n)
	for i := range out {
		t, ok := o.ready[i]
		switch {
		case !ok || exhausted[i]:
			out[i] = -1
		case t.After(now):
			out[i] = t.Sub(now)
		}
	}
	return out
}

// recoveryPick is the candidate a recovery request goes to after cands[failed] failed, and the wait
// before it. Candidates are taken in failover order starting after the one that failed, so retries
// rotate over the unavailable candidates: the first one ready within the backoff is sent after the
// backoff; when none is, the one ready soonest is sent once its Retry-After has passed. pick is -1 when
// no candidate is retried (readyIn < 0 for all).
func recoveryPick(readyIn []time.Duration, failed int, backoff time.Duration) (pick int, wait time.Duration) {
	pick = -1
	for k := 1; k <= len(readyIn); k++ {
		i := (failed + k) % len(readyIn)
		d := readyIn[i]
		if d < 0 {
			continue
		}
		if d <= backoff {
			return i, backoff
		}
		if pick < 0 || d < readyIn[pick] {
			pick = i
		}
	}
	if pick < 0 {
		return -1, 0
	}
	return pick, readyIn[pick]
}

type recoveryInputs struct {
	Candidates []candidateRef      `json:"candidates"`
	Failed     int                 `json:"failed"`
	Class      protocol.ErrorClass `json:"class"`
	ReadyInMs  []int64             `json:"readyInMs"`
	BackoffMs  int64               `json:"backoffMs"`
}

type recoveryOutput struct {
	Next   int   `json:"next"`
	WaitMs int64 `json:"waitMs"`
}

// RecoveryDecision records a recovery move: after cands[failed] failed with class, the step waits and
// sends to next (-1: no candidate is left). The inputs are recoveryPick's in whole milliseconds, rounded
// up so neither a Retry-After nor the backoff is cut short, so the decision replays (I7); being a
// failover entry, it moves the step's model like any failover.
func RecoveryDecision(cands []Candidate, failed int, class protocol.ErrorClass, readyIn []time.Duration, backoff time.Duration) (int, time.Duration, Decision) {
	ms := make([]int64, len(readyIn))
	for i, d := range readyIn {
		ms[i] = -1
		if d >= 0 {
			ms[i] = ceilMs(d)
		}
	}
	backoffMs := ceilMs(backoff)
	next, waitMs := replayRecoveryPick(ms, failed, backoffMs)
	return next, time.Duration(waitMs) * time.Millisecond, newDecision(DecisionFailover, RuleFailoverRecovery,
		recoveryInputs{Candidates: candidateRefs(cands), Failed: failed, Class: class, ReadyInMs: ms, BackoffMs: backoffMs},
		recoveryOutput{Next: next, WaitMs: waitMs})
}

// ceilMs is d in whole milliseconds, rounded up so a recorded wait is never shorter than d.
func ceilMs(d time.Duration) int64 {
	return int64((d + time.Millisecond - 1) / time.Millisecond)
}

func replayRecoveryPick(readyInMs []int64, failed int, backoffMs int64) (int, int64) {
	in := make([]time.Duration, len(readyInMs))
	for i, ms := range readyInMs {
		in[i] = time.Duration(ms) * time.Millisecond
	}
	pick, wait := recoveryPick(in, failed, time.Duration(backoffMs)*time.Millisecond)
	return pick, wait.Milliseconds()
}

// RecoveryError is the final error of a step that stopped recovering from availability failures. It
// wraps the last availability error, so its class and Retry-After stay readable.
type RecoveryError struct {
	// Failures counts the failed requests of the outage, Retries the waits taken.
	Failures int
	Retries  int
	Outage   time.Duration
	// Stop is one of the RecoveryStop* reasons; Wait is the wait that was refused.
	Stop string
	Wait time.Duration
	Last error
}

func (e *RecoveryError) Error() string {
	var why string
	switch e.Stop {
	case RecoveryStopDisabled:
		why = "retries are disabled"
	case RecoveryStopNoCandidate:
		why = "no unavailable candidate is left to retry"
	case RecoveryStopOutage:
		why = fmt.Sprintf("the next retry in %s would pass the outage window", e.Wait.Round(time.Millisecond))
	case RecoveryStopDeadline:
		why = fmt.Sprintf("the next retry in %s would pass the run's deadline", e.Wait.Round(time.Millisecond))
	default:
		why = e.Stop
	}
	return fmt.Sprintf("provider unavailable for %s: %d failed requests, %d retries; stopped because %s; last error: %v",
		e.Outage.Round(time.Millisecond), e.Failures, e.Retries, why, e.Last)
}

func (e *RecoveryError) Unwrap() error { return e.Last }

// failureDetail is the reason and HTTP status a provider error carries, for logs.
func failureDetail(err error) (protocol.Reason, int) {
	var pe *protocol.ProviderError
	if errors.As(err, &pe) {
		return pe.Reason, pe.Status
	}
	return "", 0
}
