package nmi

import "errors"

// TransportAmbiguousError marks a gateway call whose MUTATION MAY HAVE
// EXECUTED even though we got no usable answer: the request was (or may have
// been) sent and the response was lost (timeout after send, connection reset,
// 5xx, unreadable/undecodable body). Callers must NEVER treat it as a decline
// or blind-retry with a fresh order id — verify at the provider first (#674).
//
// Clean failures (read-only guard, unconfigured client, request validation,
// parsed 4xx envelopes, parsed declines) are deliberately NOT wrapped.
type TransportAmbiguousError struct{ Err error }

func (e *TransportAmbiguousError) Error() string {
	return "nmi: outcome unknown (transport failure after send): " + e.Err.Error()
}

func (e *TransportAmbiguousError) Unwrap() error { return e.Err }

// ambiguous wraps err as transport-ambiguous. nil-safe.
func ambiguous(err error) error {
	if err == nil {
		return nil
	}
	return &TransportAmbiguousError{Err: err}
}

// IsTransportAmbiguous reports whether err carries a TransportAmbiguousError
// anywhere in its chain.
func IsTransportAmbiguous(err error) bool {
	var t *TransportAmbiguousError
	return errors.As(err, &t)
}
