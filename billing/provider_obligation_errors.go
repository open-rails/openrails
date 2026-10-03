package openrails

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
	ErrOperationAuthorizationNotFound           error = newCodedError("operation_authorization_not_found", ErrNotFound)
	ErrOperationAuthorizationConflict           error = newCodedError("operation_authorization_conflict", ErrConflict)
	ErrOperationAuthorizationNotOpen            error = newCodedError("operation_authorization_not_open", ErrConflict)
	ErrOperationAuthorizationHasBillingEvidence error = newCodedError("operation_authorization_has_billing_evidence", ErrConflict)
	ErrProviderBillingQualificationNotFound     error = newCodedError("provider_billing_qualification_not_found", ErrNotFound)
	ErrProviderBillingObservationConflict       error = newCodedError("provider_billing_observation_conflict", ErrConflict)
	ErrProviderBillingQualificationRefused      error = newCodedError("provider_billing_qualification_refused", ErrConflict)
)

// OperationAuthorizationConflict names the immutable field an operation id
// reuse changed. Over HTTP the field is StatusError.Param.
type OperationAuthorizationConflict struct{ Field string }

func (e *OperationAuthorizationConflict) Error() string {
	return fmt.Sprintf("operation authorization id reused with changed %s", e.Field)
}
func (e *OperationAuthorizationConflict) Unwrap() error { return ErrOperationAuthorizationConflict }

// ProviderBillingObservationConflict names the evidence field a replay changed.
// Over HTTP the field is StatusError.Param.
type ProviderBillingObservationConflict struct{ Field string }

func (e *ProviderBillingObservationConflict) Error() string {
	return fmt.Sprintf("provider billing evidence conflicts on %s", e.Field)
}
func (e *ProviderBillingObservationConflict) Unwrap() error {
	return ErrProviderBillingObservationConflict
}
