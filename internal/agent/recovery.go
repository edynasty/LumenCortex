package agent

import (
	"errors"
	"fmt"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/protocol"
)

// Recovery is how a step waits out availability failures that no failover candidate answered (design
// §4.5; the runtime's execution.retry). After a sweep of the step's candidates ends in availability
// failures, the loop waits and sends the same step again from the first candidate, as a new admitted
// request: the transcript is unchanged and no tool ran, because a failed request returned nothing to
// execute. The wait is an exponential backoff from BaseDelay, doubling up to MaxDelay, scaled by a
// 75-100% jitter so concurrent runs do not retry in lockstep, or the provider's Retry-After when that is
// longer. Recovery sets no request, token or cost limit: the run's deadline and cancellation bound it, and
// MaxOutage bounds one continuous outage.
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
	RecoveryStopDisabled = "retry_disabled"
	RecoveryStopOutage   = "outage_window"
	RecoveryStopDeadline = "deadline"
)

// outage is one step's run of availability failures since its last answer.
type outage struct {
	start    time.Time
	failures int
	retries  int
	// hint is the shortest Retry-After of the current sweep (0 when any candidate gave none): the
	// earliest any candidate said it could take a request again.
	hint     time.Duration
	sweeping bool
}

func (o *outage) fail(sentAt time.Time, retryAfter time.Duration) {
	if o.failures == 0 {
		o.start = sentAt
	}
	o.failures++
	if !o.sweeping || retryAfter < o.hint {
		o.hint = retryAfter
	}
	o.sweeping = true
}

// next returns the wait before the next sweep, or why the step stops instead: retrying is disabled, the
// wait would take the outage past MaxOutage, or it would end at or after the deadline.
func (p Recovery) next(o *outage, now time.Time, deadline time.Time, jitter float64) (time.Duration, string) {
	if !p.enabled() {
		return 0, RecoveryStopDisabled
	}
	wait := max(p.backoff(o.retries+1, jitter), o.hint)
	at := now.Add(wait)
	if p.MaxOutage > 0 && at.Sub(o.start) > p.MaxOutage {
		return wait, RecoveryStopOutage
	}
	if !deadline.IsZero() && !at.Before(deadline) {
		return wait, RecoveryStopDeadline
	}
	return wait, ""
}

// RecoveryError is the final error of a step that stopped recovering from availability failures. It
// wraps the last provider error, so its class and Retry-After stay readable.
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
