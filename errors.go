package openrails

import (
	"errors"
	"fmt"
	"net/http"
)

// Sentinel errors classify the shared client response by status and machine code.
var (
	// ErrUnreachable marks transport failures and server failures. It never proves
	// that a write did not commit; retry with the same operation identity.
	ErrUnreachable = errors.New("openrails: unreachable")

	// ErrInvalid is a malformed/rejected request (HTTP 400 family).
	ErrInvalid = errors.New("openrails: invalid request")
	// ErrUnauthorized is a missing/invalid credential (HTTP 401).
	ErrUnauthorized = errors.New("openrails: unauthorized")
	// ErrDenied is an authorization/scope denial (HTTP 403/429).
	ErrDenied = errors.New("openrails: denied")
	// ErrNotFound is a missing resource (HTTP 404).
	ErrNotFound = errors.New("openrails: not found")
	// ErrConflict is a state conflict (HTTP 409).
	ErrConflict = errors.New("openrails: conflict")
	// ErrInternal is an engine-side failure (HTTP 5xx, or the embedded
	// equivalent: a service error the handlers would have mapped to 500).
	ErrInternal = errors.New("openrails: internal error")

	// ErrInsufficientCredits is the payer-balance deny (HTTP 402 /
	// "insufficient_credits"). Message kept identical to go-client's sentinel.
	ErrInsufficientCredits = errors.New("insufficient_credits")

	// ErrIdempotencyKeyReused is the money-write refusal (HTTP 409 /
	// "idempotency_key_reused"): the key already committed and THIS retry
	// carries different charging terms (or#891). A caller bug, not an engine
	// fault — retrying it unchanged will refuse again. The engine-side twin is
	// pkg/service.ErrIdempotencyKeyReused; the StatusError's Message carries the
	// detail (which field, committed vs retried).
	ErrIdempotencyKeyReused = errors.New("idempotency_key_reused")
)

// ErrorDetails is the canonical HTTP error payload. Metadata numbers decode as
// json.Number so identifiers and monetary values retain their exact precision.
type ErrorDetails struct {
	Type      string         `json:"type"`
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id,omitempty"`
	Param     *string        `json:"param,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// StatusError preserves the full server error and relevant response headers.
// Human messages are diagnostic; classification uses only Status and Code.
type StatusError struct {
	Status int
	ErrorDetails
	RetryAfter string
}

func (e *StatusError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = e.Code
	}
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("openrails: status=%d %s", e.Status, msg)
}

// Is classifies this response without inspecting its human message.
func (e *StatusError) Is(target error) bool {
	if coded, ok := target.(*codedError); ok {
		return e.Code == coded.code
	}
	switch target {
	case ErrInsufficientCredits:
		return e.Code == "insufficient_credits"
	case ErrIdempotencyKeyReused:
		return e.Code == "idempotency_key_reused"
	case ErrUnauthorized:
		return e.Status == http.StatusUnauthorized
	case ErrDenied:
		return e.Status == http.StatusForbidden || e.Status == http.StatusTooManyRequests
	case ErrNotFound:
		return e.Status == http.StatusNotFound
	case ErrConflict:
		return e.Status == http.StatusConflict
	case ErrInternal, ErrUnreachable:
		return e.Status >= 500
	case ErrInvalid:
		return e.Status >= 400 && e.Status < 500 && e.Status != http.StatusUnauthorized &&
			e.Status != http.StatusPaymentRequired && e.Status != http.StatusForbidden &&
			e.Status != http.StatusTooManyRequests && e.Status != http.StatusNotFound && e.Status != http.StatusConflict
	default:
		return false
	}
}
