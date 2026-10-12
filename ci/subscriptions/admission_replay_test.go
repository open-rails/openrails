//go:build e2e && integration

package subscriptions_test

import (
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// The HTTP retry key must not freeze a mutable admission at state=open.
// Embedded and remote callers recover the durable admission's current state.
func TestAdmissionRetryReturnsCurrentHold(t *testing.T) {
	w := newWorld(t)
	c := w.newCustomer()
	_, err := createCreditGrant(t.Context(), w.client[embedded], c.cid(), billing.CreateCreditGrantParams{Currency: "USD", Amount: 10_000_000, Source: "test", SourceID: uuid.NewString()})
	require.NoError(t, err)
	for _, tp := range []topology{embedded, remote} {
		for _, terminal := range []billing.AdmissionState{billing.AdmissionReleased, billing.AdmissionCaptured, billing.AdmissionExpired} {
			t.Run(string(tp)+"/"+string(terminal), func(t *testing.T) {
				client := w.client[tp]
				deadline := w.clock.Now().Add(time.Hour)
				params := []billing.AdmitParams{{RequestID: uuid.NewString(), CustomerID: c.cid(), Invoker: c.id, InvokerType: billing.InvokerTypeCustomer, Currency: "USD", EstimatedAmount: 100_000, ExpiresAt: &deadline, Roles: []uuid.UUID{uuid.New(), uuid.New()}}}
				key := openrails.WithIdempotencyKey(uuid.NewString())
				first, err := client.Admit(t.Context(), params, key)
				require.NoError(t, err)
				require.True(t, first[0].Allowed())
				switch terminal {
				case billing.AdmissionReleased:
					_, err = client.ReleaseAdmissions(t.Context(), []string{params[0].RequestID})
				case billing.AdmissionCaptured:
					_, err = captureAdmission(t.Context(), client, params[0].RequestID, 50_000, nil)
				case billing.AdmissionExpired:
					w.clock.Advance(2 * time.Hour)
				}
				require.NoError(t, err)
				// JSON spelling and role order do not change the business terms.
				params[0].Currency = "usd"
				params[0].Roles[0], params[0].Roles[1] = params[0].Roles[1], params[0].Roles[0]
				again, err := client.Admit(t.Context(), params, key)
				require.NoError(t, err)
				require.Equal(t, terminal, *again[0].Admission.State)
				require.True(t, again[0].Admission.Replayed)
				require.False(t, again[0].Allowed())
				params[0].EstimatedAmount++
				changed, err := client.Admit(t.Context(), params, key)
				require.NoError(t, err)
				require.Equal(t, billing.CodeIdempotencyKeyReused, changed[0].Error.Code)
			})
		}
	}
}

// lostResponse delivers a request completely, then loses its successful
// response. Retrying must recover the operation without double charging.
type lostResponse struct {
	drop atomic.Bool
}

func (tr *lostResponse) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil || !tr.drop.Swap(false) {
		return resp, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return nil, io.ErrUnexpectedEOF
}

func TestAdmissionAndCaptureRecoverAfterLostResponses(t *testing.T) {
	w := newWorld(t)
	c := w.newCustomer()
	merchant := w.client[embedded].MerchantID()
	_, err := createCreditGrant(t.Context(), w.client[embedded], c.cid(), billing.CreateCreditGrantParams{Currency: "USD", Amount: 10_000_000, Source: "test", SourceID: uuid.NewString()})
	require.NoError(t, err)
	transport := &lostResponse{}
	remote, err := openrails.NewRemote(w.server.URL+mountPrefix, openrails.RemoteOptions{
		Credentials: map[billing.MerchantID]openrails.RemoteCredential{merchant: {APIKey: w.auth.hostToken(t)}},
		HTTPClient:  &http.Client{Transport: transport},
	})
	require.NoError(t, err)
	client, err := remote.Merchant(merchant)
	require.NoError(t, err)
	deadline := w.clock.Now().Add(time.Hour)
	params := []billing.AdmitParams{{RequestID: uuid.NewString(), CustomerID: c.cid(), Invoker: c.id, InvokerType: billing.InvokerTypeCustomer, Currency: "USD", EstimatedAmount: 100_000, ExpiresAt: &deadline}}
	transport.drop.Store(true)
	_, err = client.Admit(t.Context(), params)
	require.ErrorIs(t, err, billing.ErrUnreachable)
	again, err := client.Admit(t.Context(), params)
	require.NoError(t, err)
	require.True(t, again[0].Admission.Replayed)
	require.True(t, again[0].Allowed())

	transport.drop.Store(true)
	_, err = captureAdmission(t.Context(), client, params[0].RequestID, 50_000, nil)
	require.ErrorIs(t, err, billing.ErrUnreachable)
	receipt, err := captureAdmission(t.Context(), client, params[0].RequestID, 50_000, nil)
	require.NoError(t, err)
	require.True(t, receipt.Replayed)
	require.NotNil(t, receipt.BalanceTransactionID)
	replayed, err := captureAdmission(t.Context(), w.client[embedded], params[0].RequestID, 50_000, nil)
	require.NoError(t, err)
	require.Equal(t, receipt.BalanceTransactionID, replayed.BalanceTransactionID)
	_, err = captureAdmission(t.Context(), client, params[0].RequestID, 50_001, nil)
	require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)
	balance, err := client.GetBalance(t.Context(), c.cid(), "USD")
	require.NoError(t, err)
	require.EqualValues(t, 9_950_000, balance.BalanceAmount)
}
