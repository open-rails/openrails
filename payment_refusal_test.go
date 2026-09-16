package openrails

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPaymentRefusalClassification(t *testing.T) {
	declined := &StatusError{Status: http.StatusPaymentRequired, ErrorDetails: ErrorDetails{Type: "card_error", Code: CodeCardDeclined, Message: "anything"}}
	require.ErrorIs(t, declined, ErrPaymentRefused)
	require.ErrorIs(t, declined, ErrCardDeclined)
	require.NotErrorIs(t, declined, ErrPaymentMethodStale)
	require.NotErrorIs(t, declined, ErrInsufficientCredits)
	require.NotErrorIs(t, declined, ErrInvalid)
	require.NotErrorIs(t, declined, ErrInternal)

	stale := &StatusError{Status: http.StatusPaymentRequired, ErrorDetails: ErrorDetails{Type: "card_error", Code: CodePaymentMethodStale}}
	require.ErrorIs(t, stale, ErrPaymentRefused)
	require.ErrorIs(t, stale, ErrPaymentMethodStale)
	require.NotErrorIs(t, stale, ErrCardDeclined)

	rejected := &StatusError{Status: http.StatusBadGateway, ErrorDetails: ErrorDetails{Type: "api_error", Code: CodePaymentProviderRejected}}
	require.ErrorIs(t, rejected, ErrPaymentProviderRejected)
	require.ErrorIs(t, rejected, ErrInternal)
	require.NotErrorIs(t, rejected, ErrPaymentRefused)

	// The coded sentinels classify by their status class without a response.
	require.ErrorIs(t, ErrCardDeclined, ErrPaymentRefused)
	require.ErrorIs(t, ErrPaymentProviderRejected, ErrInternal)
}
