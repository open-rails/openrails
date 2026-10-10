package billing

import "fmt"

// codedError is a stable machine code that also matches its status class, so a
// host transaction error and a Client StatusError classify identically.
type codedError struct {
	code  string
	class error
}

func (e *codedError) Error() string { return e.code }

// ErrorCode is the wire error code.
func (e *codedError) ErrorCode() string { return e.code }

func (e *codedError) Is(target error) bool { return target == e.class }

func newCodedError(code string, class error) *codedError {
	return &codedError{code: code, class: class}
}

var (
	ErrProviderOperationNotFound           error = newCodedError("provider_operation_not_found", ErrNotFound)
	ErrProviderOperationConflict           error = newCodedError("provider_operation_conflict", ErrConflict)
	ErrProviderOperationNotOpen            error = newCodedError("provider_operation_not_open", ErrConflict)
	ErrProviderOperationHasBillingEvidence error = newCodedError("provider_operation_has_billing_evidence", ErrConflict)
	ErrProviderOperationRefused            error = newCodedError("provider_operation_refused", ErrConflict)
	ErrProviderOperationNotRefused         error = newCodedError("provider_operation_not_refused", ErrConflict)
	ErrProviderBillingObservationConflict  error = newCodedError("provider_billing_observation_conflict", ErrConflict)
)

// ProviderOperationConflict names the term a repeated open, increment, release
// or close changed. Over HTTP the field is StatusError.Param.
type ProviderOperationConflict struct{ Field string }

func (e *ProviderOperationConflict) Error() string {
	return fmt.Sprintf("provider operation repeated with changed %s", e.Field)
}
func (e *ProviderOperationConflict) Unwrap() error { return ErrProviderOperationConflict }

// ProviderBillingObservationConflict names the evidence field a replay changed.
// Over HTTP the field is StatusError.Param.
type ProviderBillingObservationConflict struct{ Field string }

func (e *ProviderBillingObservationConflict) Error() string {
	return fmt.Sprintf("provider billing evidence conflicts on %s", e.Field)
}
func (e *ProviderBillingObservationConflict) Unwrap() error {
	return ErrProviderBillingObservationConflict
}
