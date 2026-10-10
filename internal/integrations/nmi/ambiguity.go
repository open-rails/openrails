package nmi

import "errors"

// ErrNotDispatched marks a failure established before the financial HTTP call.
// It cannot be inferred from a transport failure or an empty provider query.
var ErrNotDispatched = errors.New("nmi: financial request was not dispatched")

// TransportAmbiguousError marks a gateway call whose mutation may have
// executed though no usable answer came back (timeout after send, reset, 5xx,
// unreadable body). Never treat it as a decline or retry with a fresh order
// id: verify at the provider first. Clean failures (readonly, unconfigured,
// validation, parsed 4xx envelopes or declines) are not wrapped.
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

// UncertainResponseCode identifies processor communication/duplicate responses
// that do not prove this operation was declined or never submitted.
func UncertainResponseCode(code int) bool {
	switch code {
	case 420, 421, 430:
		return true
	default:
		return false
	}
}

// RequiresVerification includes both lost transport responses and parsed
// responses whose outcome cannot safely authorize another non-idempotent send.
func RequiresVerification(err error) bool {
	if IsTransportAmbiguous(err) {
		return true
	}
	var response *CustomerVaultError
	return errors.As(err, &response) && UncertainResponseCode(response.ResponseCode)
}
