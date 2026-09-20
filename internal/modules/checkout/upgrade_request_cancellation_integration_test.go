//go:build integration

package checkout

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type closeHookBody struct {
	io.ReadCloser
	closed func()
}

func (b closeHookBody) Close() error {
	err := b.ReadCloser.Close()
	b.closed()
	return err
}

// cancelAfterProviderReceipt cancels the request once the n-th provider write
// response has been read in full: the exact receipt is in hand, then the request
// is gone. A literal NMIClient uses http.DefaultTransport, which this swaps for
// the duration of the test.
func cancelAfterProviderReceipt(t *testing.T, fake *nmi.NMIClient, n int64, cancel context.CancelFunc) *nmi.NMIClient {
	t.Helper()
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	var writes atomic.Int64
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := original.RoundTrip(req)
		if err != nil || req.Method != http.MethodPost {
			return resp, err
		}
		resp.Body = closeHookBody{resp.Body, func() {
			if writes.Add(1) == n {
				cancel()
			}
		}}
		return resp, nil
	})
	return &nmi.NMIClient{
		SecurityKey: fake.SecurityKey, WebhookSecret: fake.WebhookSecret, TestMode: true,
		DirectPostURL: fake.DirectPostURL, QueryURL: fake.QueryURL, V5BaseURL: fake.V5BaseURL,
	}
}

// The upgrade runs as a request does: on a connection the request pins, from a
// one-connection app-role pool. The request is cancelled right after a provider
// receipt arrives; the cancelled local work then closes the pinned connection.
// Every receipt in hand and the unknown mark must still be durable, and the
// verifier must finish from them without resending.
func TestUpgradeReceiptsSurviveCanceledRequestOnPinnedConnection(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cancelAfter int64
	}{{"successor receipt", 1}, {"proration receipt", 2}} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newUpgradeAdoptFixture(t)
			fx.positiveProration()
			fake, err := fx.svc.ResolveNMIClientOverride(fx.ctx, "nmi")
			require.NoError(t, err)

			app := dbtest.OpenOneConnAppDB(t)
			pinned, release, err := app.WithMerchantConn(fx.ctx)
			require.NoError(t, err)
			defer release()
			ctx, cancel := context.WithCancel(pinned)
			defer cancel()
			svc := newUpgradeCheckoutService(app, fx.svc.Clock(), cancelAfterProviderReceipt(t, fake, tc.cancelAfter, cancel))

			_, err = svc.processUpgrade(ctx, fx.req, fx.user, fx.newPrice, fx.newProduct, fx.existingSub, fx.target)
			require.Error(t, err)
			require.ErrorIs(t, ctx.Err(), context.Canceled)
			release()
			require.Zero(t, app.Pool().Stat().AcquiredConns(), "the request connection is returned")

			row := fx.operation(t)
			require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status, "unknown mark must be durable despite the cancelled request")
			var progress nmiUpgradeProgress
			require.NoError(t, json.Unmarshal(row.ResultEvidence, &progress))
			require.NotNil(t, progress.Successor)
			require.NotNil(t, progress.Successor.Enrollment)
			require.Equal(t, fx.gateway.subID, progress.Successor.Enrollment.SubscriptionID)
			if tc.cancelAfter == 2 {
				require.NotNil(t, progress.Proration)
				require.NotNil(t, progress.Proration.Sale)
				require.Equal(t, fx.gateway.saleTxn, progress.Proration.Sale.TransactionID)
			} else {
				require.Zero(t, fx.gateway.saleCalls.Load(), "a cancelled request never submits the next step")
			}

			_, err = fx.db.Qx(fx.ctx).Exec(fx.ctx, `UPDATE billing.rail_intents SET next_attempt_at = 'epoch' WHERE id = $1`, row.ID)
			require.NoError(t, err)
			require.NoError(t, app.RunInMerchantConn(fx.ctx, func(ctx context.Context) error {
				_, err := svc.Intents.(*intents.Runner).RunVerifyOnce(ctx)
				return err
			}))
			recovered := fx.operation(t)
			if tc.cancelAfter == 2 {
				require.Equal(t, intents.StatusSucceeded, recovered.Status)
			} else {
				require.Equal(t, intents.StatusFailedRetryable, recovered.Status, "the unsent proration goes back to the executor")
			}
			require.EqualValues(t, 1, fx.gateway.createCalls.Load(), "the successor is never re-created")
			require.EqualValues(t, tc.cancelAfter-1, fx.gateway.saleCalls.Load(), "the proration is never resent")
		})
	}
}
