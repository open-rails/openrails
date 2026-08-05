package openrails

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Sentinel errors shared by BOTH transports (#338). The contract is
// bidirectional and structural:
//
//   - the REMOTE client maps HTTP status codes + error-code payloads onto these
//     sentinels (see remote.go statusError);
//   - the EMBEDDED client (openrails/embed) maps pkg/service errors onto the
//     SAME sentinels by transcribing the exact status mapping the HTTP handlers
//     perform (internal/http/handlers/service_*.go are the authoritative map).
//
// errors.Is therefore behaves identically whether the engine is in-process or
// behind HTTP. Every sentinel is carried by a *StatusError, so callers can also
// recover the HTTP-ish status code and detail message on either transport.
var (
	// ErrUnreachable wraps transport/timeout failures AND 5xx responses on the
	// REMOTE transport only — the caller's fail-policy (FailOpen vs FailClosed,
	// #248) keys off it. The embedded transport never produces ErrUnreachable
	// (there is no wire to lose); an embedded engine fault is ErrInternal only.
	// A clean deny (allowed=false) is NOT an ErrUnreachable and is always
	// honored regardless of policy.
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

// StatusError is the concrete error both transports return for any non-OK
// engine outcome. Status is the HTTP status the standalone server returns for
// this failure (the embedded transport reports the SAME code by transcribing
// the handler mapping); Code/Message carry the wire error payload.
//
// errors.Is(err, <sentinel>) works through StatusError on both transports.
type StatusError struct {
	Status  int
	Code    string
	Message string

	sentinels []error
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

// Unwrap exposes the mapped sentinels so errors.Is matches them.
func (e *StatusError) Unwrap() []error { return e.sentinels }

// NewStatusError builds the canonical cross-transport error for an HTTP-ish
// status + error payload. Both the remote client (from a real HTTP response)
// and the embedded adapter (from the handler-transcribed mapping) construct
// errors through here, which is what keeps errors.Is identical across
// transports.
func NewStatusError(status int, code, message string) *StatusError {
	return newStatusError(status, code, message)
}

func newStatusError(status int, code, message string, extra ...error) *StatusError {
	e := &StatusError{Status: status, Code: code, Message: message}

	// Code/message-specific sentinels first: the handlers signal these as
	// payload strings ("insufficient_credits" on 402 — see
	// internal/http/handlers/service_credits.go).
	switch {
	case signals(code, message, "insufficient_credits"):
		e.sentinels = append(e.sentinels, ErrInsufficientCredits)
	case signals(code, message, "idempotency_key_reused"):
		e.sentinels = append(e.sentinels, ErrIdempotencyKeyReused)
	}

	// Status-class sentinel.
	switch {
	case status == http.StatusUnauthorized:
		e.sentinels = append(e.sentinels, ErrUnauthorized)
	case status == http.StatusPaymentRequired:
		if !errorsContain(e.sentinels, ErrInsufficientCredits) {
			e.sentinels = append(e.sentinels, ErrInsufficientCredits)
		}
	case status == http.StatusForbidden, status == http.StatusTooManyRequests:
		e.sentinels = append(e.sentinels, ErrDenied)
	case status == http.StatusNotFound:
		e.sentinels = append(e.sentinels, ErrNotFound)
	case status == http.StatusConflict:
		e.sentinels = append(e.sentinels, ErrConflict)
	case status >= 500:
		e.sentinels = append(e.sentinels, ErrInternal)
	default: // 400 and any other 4xx
		e.sentinels = append(e.sentinels, ErrInvalid)
	}

	e.sentinels = append(e.sentinels, extra...)
	return e
}

// signals reports whether the wire payload names `want`. The standalone error
// envelope's "code" is a coarse api.Code* class (invalid_param,
// resource_conflict, ...), so the discriminating string is normally the
// MESSAGE. A message may carry detail after the token
// ("idempotency_key_reused: spend replayed key (…) with amount=…"), which is
// worth keeping on the wire, so the token is matched as a prefix too.
func signals(code, message, want string) bool {
	if strings.TrimSpace(code) == want {
		return true
	}
	msg := strings.TrimSpace(message)
	return msg == want || strings.HasPrefix(msg, want+":")
}

func errorsContain(errs []error, target error) bool {
	for _, e := range errs {
		if errors.Is(e, target) {
			return true
		}
	}
	return false
}
