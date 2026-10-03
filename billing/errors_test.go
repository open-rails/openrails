package openrails

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

var statusClasses = []error{ErrInvalid, ErrUnauthorized, ErrPaymentRefused, ErrDenied, ErrNotFound, ErrConflict, ErrInternal, ErrUnreachable}

func matchingClasses(err error) []error {
	var out []error
	for _, class := range statusClasses {
		if errors.Is(err, class) {
			out = append(out, class)
		}
	}
	return out
}

// Classification uses only Status and Code; every status maps to exactly one class family.
func TestStatusErrorClassification(t *testing.T) {
	want := map[int][]error{
		400: {ErrInvalid}, 413: {ErrInvalid}, 422: {ErrInvalid},
		401: {ErrUnauthorized},
		402: {ErrPaymentRefused},
		403: {ErrDenied}, 429: {ErrDenied},
		404: {ErrNotFound},
		409: {ErrConflict},
		500: {ErrInternal, ErrUnreachable}, 502: {ErrInternal, ErrUnreachable}, 503: {ErrInternal, ErrUnreachable},
	}
	for status, classes := range want {
		err := &StatusError{Status: status, ErrorDetails: ErrorDetails{Message: "insufficient_credits idempotency_key_reused"}}
		require.Equal(t, classes, matchingClasses(err), "status %d", status)
		require.NotErrorIs(t, err, ErrInsufficientCredits, "messages never classify")
		require.NotErrorIs(t, err, ErrIdempotencyKeyReused, "messages never classify")
	}
	require.ErrorIs(t, &StatusError{Status: 402, ErrorDetails: ErrorDetails{Code: "insufficient_credits"}}, ErrInsufficientCredits)
	require.NotErrorIs(t, &StatusError{Status: 402, ErrorDetails: ErrorDetails{Code: "payment_failed"}}, ErrInsufficientCredits)
	require.Equal(t, "openrails: status=503 Service Unavailable", (&StatusError{Status: 503}).Error())
	require.Equal(t, "openrails: status=409 resource_conflict", (&StatusError{Status: 409, ErrorDetails: ErrorDetails{Code: "resource_conflict"}}).Error())
}

// A host-side coded sentinel and the Client's StatusError for the same code
// classify identically, and codes never match one another.
func TestCodedSentinelsClassifyAcrossTransports(t *testing.T) {
	cases := []struct {
		sentinel error
		status   int
		class    error
	}{
		{ErrCardDeclined, http.StatusPaymentRequired, ErrPaymentRefused},
		{ErrPaymentMethodStale, http.StatusPaymentRequired, ErrPaymentRefused},
		{ErrPaymentProviderRejected, http.StatusBadGateway, ErrInternal},
		{ErrRequestBodyTooLarge, http.StatusRequestEntityTooLarge, ErrInvalid},
		{ErrOperationAuthorizationNotFound, http.StatusNotFound, ErrNotFound},
		{ErrProviderBillingQualificationNotFound, http.StatusNotFound, ErrNotFound},
		{ErrOperationAuthorizationConflict, http.StatusConflict, ErrConflict},
		{ErrOperationAuthorizationNotOpen, http.StatusConflict, ErrConflict},
		{ErrOperationAuthorizationHasBillingEvidence, http.StatusConflict, ErrConflict},
		{ErrProviderBillingObservationConflict, http.StatusConflict, ErrConflict},
		{ErrProviderBillingQualificationRefused, http.StatusConflict, ErrConflict},
	}
	for _, tc := range cases {
		code := tc.sentinel.(interface{ ErrorCode() string }).ErrorCode()
		remote := &StatusError{Status: tc.status, ErrorDetails: ErrorDetails{Code: code}}
		require.Equal(t, []error{tc.class}, matchingClasses(tc.sentinel), code)
		require.Contains(t, matchingClasses(remote), tc.class, code)
		require.ErrorIs(t, remote, tc.sentinel)
		for _, other := range cases {
			if other.sentinel != tc.sentinel {
				require.NotErrorIs(t, remote, other.sentinel, code)
				require.NotErrorIs(t, tc.sentinel, other.sentinel, code)
			}
		}
	}
	for _, typed := range []error{&OperationAuthorizationConflict{Field: "authorization_body"}, &ProviderBillingObservationConflict{Field: "cost"}} {
		require.ErrorIs(t, typed, ErrConflict)
	}
	require.ErrorIs(t, &OperationAuthorizationConflict{}, ErrOperationAuthorizationConflict)
	require.ErrorIs(t, &ProviderBillingObservationConflict{}, ErrProviderBillingObservationConflict)
}
