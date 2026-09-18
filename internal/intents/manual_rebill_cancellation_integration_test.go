//go:build integration

package intents

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/stretchr/testify/require"
)

// Close runs after the adapter has consumed and parsed the response. Cancelling
// here deterministically models a lost caller with the provider result in hand.
type rebillResponseClose struct {
	io.ReadCloser
	close func()
}

func (b rebillResponseClose) Close() error { err := b.ReadCloser.Close(); b.close(); return err }

type rebillReceiptTransport struct {
	base  http.RoundTripper
	match func(*http.Request) bool
	close func()
	once  sync.Once
}

func (t *rebillReceiptTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err == nil && t.match(req) {
		resp.Body = rebillResponseClose{resp.Body, func() { t.once.Do(t.close) }}
	}
	return resp, err
}

func TestManualRebillReceiptSurvivesCanceledClosedRequest(t *testing.T) {
	for _, stage := range []string{"candidate", "qualified receipt"} {
		t.Run(stage, func(t *testing.T) {
			fx := seedPastDueSubscription(t)
			fake, boot := newFakeNMIRebillGateway(t)
			base := dbtest.WithTestMerchant(context.Background())
			run := dbtest.OpenOneConnAppDB(t)
			pinned, release, err := run.WithMerchantConn(base)
			require.NoError(t, err)
			defer release()
			ctx, cancel := context.WithCancel(pinned)
			defer cancel()
			// A public zero-value client uses the standard transport, allowing this
			// serial test to cancel precisely after a parsed loopback response.
			client := &nmi.NMIClient{SecurityKey: boot.SecurityKey, DirectPostURL: boot.DirectPostURL, QueryURL: boot.QueryURL, V5BaseURL: boot.V5BaseURL, TestMode: true}
			original := http.DefaultTransport
			defer func() { http.DefaultTransport = original }()
			closed := false
			http.DefaultTransport = &rebillReceiptTransport{
				base: original,
				match: func(req *http.Request) bool {
					if stage == "qualified receipt" {
						return req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/payments/"+fake.txnID)
					}
					return req.Method == http.MethodPost && req.URL.String() == client.DirectPostURL
				},
				close: func() {
					cancel()
					// Interrupted BEGIN closes pgx's pinned connection. The detached custody
					// write must release the dead pool slot before re-pinning the only slot.
					err := run.MerchantTx(ctx, func(context.Context, pgx.Tx) error { return nil })
					require.ErrorIs(t, err, context.Canceled)
					closed = true
				},
			}
			handler := NewManualRebillHandler(run, fullModeConfig(), fakeNMIResolver{client: client}, nil)
			runner := &Runner{Store: NewStore(run), Registry: NewRegistry(handler), Config: fullModeConfig()}
			params := fx.enqueueParams(1)
			_, err = runner.EnqueueAndExecute(ctx, params)
			require.ErrorIs(t, err, context.Canceled)
			require.True(t, closed)
			release()
			require.Zero(t, run.Pool().Stat().AcquiredConns())
			require.GreaterOrEqual(t, run.Pool().Stat().NewConnsCount(), int64(2), "detached write replaced the closed connection in a one-connection pool")
			row, err := fx.store.GetByIdempotencyKey(base, params.IdempotencyKey)
			require.NoError(t, err)
			require.Equal(t, StatusUnknownNeedsVerify, row.Status)
			require.Equal(t, fake.txnID, EvidenceString(row, rebillCandidateKey))
			require.Zero(t, fx.paymentsFor(t, fake.txnID))
			if stage == "qualified receipt" {
				require.Contains(t, string(row.ResultEvidence), rebillReceiptKey)
				fake.charged.Store(false) // provider disappears; durable qualification suffices
			}
			reads := fake.queryCalls.Load()
			_, err = fx.db.Pool().Exec(base, `UPDATE openrails.rail_intents SET next_attempt_at=now() WHERE id=$1`, row.ID)
			require.NoError(t, err)
			require.NoError(t, run.RunInMerchantConn(base, func(ctx context.Context) error { _, err := runner.RunVerifyOnce(ctx); return err }))
			got := fx.intentByID(t, row.ID)
			require.Equal(t, StatusSucceeded, got.Status)
			require.Contains(t, string(got.ResultEvidence), rebillReceiptKey)
			require.Equal(t, 1, fx.paymentsFor(t, fake.txnID))
			require.EqualValues(t, 1, fake.saleCalls.Load())
			if stage == "qualified receipt" {
				require.Equal(t, reads, fake.queryCalls.Load(), "saved qualification completes without another provider read")
			}
		})
	}
}
