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

// Reason refines an availability failure for logs and the run's final error; the runtime branches on
// Class only. Adapters set it from what they observed, never from model output.
type Reason string

const (
	// ReasonRateLimited: HTTP 429 or a rate-limit stream error.
	ReasonRateLimited Reason = "rate_limited"
	// ReasonOverloaded: HTTP 503, 529 or an overloaded stream error.
	ReasonOverloaded Reason = "overloaded"
	// ReasonServerError: any other 5xx, or a server error event in the stream.
	ReasonServerError Reason = "server_error"
	// ReasonTimeout: HTTP 408, or the adapter's request or idle timeout.
	ReasonTimeout Reason = "timeout"
	// ReasonConflict: HTTP 409.
	ReasonConflict Reason = "conflict"
	// ReasonTransport: the connection failed or broke (refused, reset, EOF, TLS, DNS).
	ReasonTransport Reason = "transport"
	// ReasonStreamIncomplete: the stream ended without its terminal event, or carried an error event.
	ReasonStreamIncomplete Reason = "stream_incomplete"
	// ReasonMalformedResponse: a 2xx body that is not a valid response (bad JSON, tool-call delimiters
	// leaked into text).
	ReasonMalformedResponse Reason = "malformed_response"
)

// ProviderError is what an adapter returns for every failed Complete. One Complete is one HTTP
// attempt: the agent loop owns retrying, so every attempt is admitted, recorded and logged.
type ProviderError struct {
	Class ErrorClass
	// Reason refines Class (set for availability; may be empty otherwise).
	Reason Reason
	// Status is the HTTP status of the failed attempt, 0 when there was none (transport, stream).
	Status int
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
