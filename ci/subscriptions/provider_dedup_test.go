//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"
)

func stripeDedupRequest(t *testing.T, f *stripeFake, ctx context.Context, key string, form url.Values) (int, []byte, error) {
	t.Helper()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.stripe.com/v1/payment_intents", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	r.Header.Set("Idempotency-Key", key)
	response, err := f.RoundTrip(r)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	return response.StatusCode, body, err
}

func stripeDedupForm(method string) url.Values {
	return url.Values{"amount": {"999"}, "currency": {"usd"}, "customer": {"cus_fixture"}, "payment_method": {method}}
}

// These prove the simulator's provider boundary, not Stripe account behavior.
// A result survives a lost response; concurrent requests do not execute twice;
// parameters are part of the key binding; and a key is not a permanent fence.
func TestStripeSimulatorIdempotencyBoundary(t *testing.T) {
	f := newStripeFake()
	clock := clockwork.NewFakeClockAt(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	f.setClock(clock.Now)
	form := stripeDedupForm("pm_fixture")
	gate := f.hold(newGate(func(r *http.Request) bool { return r.URL.Path == "/v1/payment_intents" }, false))
	t.Cleanup(func() { f.unhold() })
	type result struct {
		status int
		body   []byte
		err    error
	}
	winner := make(chan result, 1)
	go func() {
		status, body, err := stripeDedupRequest(t, f, t.Context(), "one-obligation", form)
		winner <- result{status, body, err}
	}()
	select {
	case <-gate.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not reach the provider")
	}
	status, body, err := stripeDedupRequest(t, f, t.Context(), "one-obligation", form)
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, status)
	require.Contains(t, string(body), "idempotency_key_in_use")
	require.Empty(t, f.ledger(""))
	close(gate.release)
	first := <-winner
	require.NoError(t, first.err)
	require.Equal(t, http.StatusOK, first.status)
	f.unhold()

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			status, body, err := stripeDedupRequest(t, f, t.Context(), "one-obligation", form)
			require.NoError(t, err)
			require.Equal(t, first.status, status)
			require.Equal(t, first.body, body)
		})
	}
	wg.Wait()
	require.Len(t, f.ledger(""), 1)
	changed := stripeDedupForm("pm_fixture")
	changed.Set("amount", "1000")
	status, _, err = stripeDedupRequest(t, f, t.Context(), "one-obligation", changed)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, status)
	require.Len(t, f.ledger(""), 1)

	clock.Advance(24*time.Hour - time.Nanosecond)
	status, body, err = stripeDedupRequest(t, f, t.Context(), "one-obligation", form)
	require.NoError(t, err)
	require.Equal(t, first.status, status)
	require.Equal(t, first.body, body)
	clock.Advance(time.Nanosecond)
	status, body, err = stripeDedupRequest(t, f, t.Context(), "one-obligation", form)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.NotEqual(t, first.body, body)
	require.Len(t, f.ledger(""), 2, "the provider may prune a key after 24 hours")
}

func TestStripeSimulatorCachesDeclinesAndKeepsReadback(t *testing.T) {
	f := newStripeFake()
	clock := clockwork.NewFakeClockAt(time.Now())
	f.setClock(clock.Now)
	f.declines["pm_declined"] = "insufficient_funds"
	form := stripeDedupForm("pm_declined")
	status, first, err := stripeDedupRequest(t, f, t.Context(), "attempt-0", form)
	require.NoError(t, err)
	require.Equal(t, http.StatusPaymentRequired, status)
	delete(f.declines, "pm_declined")
	status, replay, err := stripeDedupRequest(t, f, t.Context(), "attempt-0", form)
	require.NoError(t, err)
	require.Equal(t, http.StatusPaymentRequired, status, "replay retains the original HTTP status")
	require.Equal(t, first, replay)
	require.Empty(t, f.ledger(""))
	status, _, err = stripeDedupRequest(t, f, t.Context(), "attempt-1", form)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Len(t, f.ledger(""), 1)

	f.delayIntentVisibility(time.Hour)
	status, paid, err := stripeDedupRequest(t, f, t.Context(), "delayed", form)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	var intent struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(paid, &intent))
	list := func() string {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.stripe.com/v1/payment_intents?customer=cus_fixture", nil)
		require.NoError(t, err)
		response, err := f.RoundTrip(r)
		require.NoError(t, err)
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		return string(body)
	}
	require.NotContains(t, list(), intent.ID)
	clock.Advance(25 * time.Hour)
	require.Contains(t, list(), intent.ID, "persistent payment records outlive idempotency keys")
}

func TestStripeSimulatorDoesNotCacheValidationFailure(t *testing.T) {
	f := newStripeFake()
	form := stripeDedupForm("pm_fixture")
	form.Set("amount", "not-an-integer")
	status, _, err := stripeDedupRequest(t, f, t.Context(), "validate-then-retry", form)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, status)
	require.Empty(t, f.ledger(""))
	form.Set("amount", "999")
	status, _, err = stripeDedupRequest(t, f, t.Context(), "validate-then-retry", form)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status, "a corrected request may reuse a key that never executed")
	require.Len(t, f.ledger(""), 1)
}
