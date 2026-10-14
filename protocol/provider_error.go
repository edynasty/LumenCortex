package protocol

import (
	"fmt"
	"time"
)

// ErrorClass says what a failed provider request means for the run (design §4.5, A5). The adapter
// that made the request classifies it; the agent loop never inspects provider-specific errors.
type ErrorClass string

const (
	// ClassAuth: the credential was refused (401, 403). The run stops; failover would only send the
	// transcript to another endpoint with the same mistake.
	ClassAuth ErrorClass = "auth"
	// ClassBadRequest: the provider rejected the request itself (other 4xx). The run stops.
	ClassBadRequest ErrorClass = "bad_request"
	// ClassCapacity: the request does not fit the model's context window. Only a candidate with a
	// larger context can answer it.
	ClassCapacity ErrorClass = "capacity"
	// ClassAvailability: the provider could not answer now (408, 409, 429, 5xx, transport errors,
	// timeouts, malformed streams). The next candidate may.
	ClassAvailability ErrorClass = "availability"
	// ClassCancelled: the run's context ended.
	ClassCancelled ErrorClass = "cancelled"
)

// ProviderError is what an adapter returns for every failed Complete.
type ProviderError struct {
	Class ErrorClass
	// RetryAfter is the delay the provider asked for, 0 when it did not ask.
	RetryAfter time.Duration
	// Err is the adapter's own error. It may carry provider detail and is for display and
	// diagnostics; Class is the only part the runtime branches on.
	Err error
}

func (e *ProviderError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("provider error (%s)", e.Class)
	}
	return e.Err.Error()
}

func (e *ProviderError) Unwrap() error { return e.Err }
