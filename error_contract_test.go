package openrails

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Exercise the actual HTTP client decoder, including exact metadata integers and
// proxy headers. Error messages are deliberately unrelated to the machine code.
func TestClientErrorContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", "header-request")
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"idempotency_key_reused","message":"These terms differ","param":"amount","metadata":{"original_amount":9223372036854775807,"nested":{"minimum":-9223372036854775808}}}}`))
	}))
	defer server.Close()
	client, err := NewRemote(server.URL, WithAPIKey("contract-test"), WithDefaultMerchant("fixture"))
	require.NoError(t, err)
	_, err = client.GetMerchantSettings(context.Background())
	require.ErrorIs(t, err, ErrConflict)
	require.ErrorIs(t, err, ErrIdempotencyKeyReused)
	var status *StatusError
	require.ErrorAs(t, err, &status)
	require.Equal(t, "invalid_request_error", status.Type)
	require.Equal(t, "header-request", status.RequestID)
	require.Equal(t, "12", status.RetryAfter)
	require.Equal(t, "amount", *status.Param)
	require.Equal(t, json.Number("9223372036854775807"), status.Metadata["original_amount"])
	require.Equal(t, json.Number("-9223372036854775808"), status.Metadata["nested"].(map[string]any)["minimum"])

	for _, message := range []string{"idempotency_key_reused", "idempotency_key_reused: diagnostic", "insufficient_credits"} {
		err := &StatusError{Status: http.StatusConflict, ErrorDetails: ErrorDetails{Code: "resource_conflict", Message: message}}
		require.NotErrorIs(t, err, ErrIdempotencyKeyReused)
		require.NotErrorIs(t, err, ErrInsufficientCredits)
	}
	require.NotErrorIs(t, &StatusError{Status: 402, ErrorDetails: ErrorDetails{Code: "payment_failed", Message: "Card declined"}}, ErrInsufficientCredits)
	require.ErrorIs(t, &StatusError{Status: 402, ErrorDetails: ErrorDetails{Code: "insufficient_credits", Message: "Balance too low"}}, ErrInsufficientCredits)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.GetMerchantSettings(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, ErrUnreachable)
	require.False(t, errors.As(err, &status), "no server response is not a rejected operation")

	ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err = client.GetMerchantSettings(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
