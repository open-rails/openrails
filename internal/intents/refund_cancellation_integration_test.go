//go:build integration

package intents

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// cancelingStripe confirms the refund and cancels the caller as the response
// arrives: the exact receipt is in hand and the request is gone. Its list never
// shows the refund, so recovery can only come from the saved receipt.
type cancelingStripe struct {
	cancel  context.CancelFunc
	creates atomic.Int64
}

func (f *cancelingStripe) CreateRefund(context.Context, subscriptions.RefundParams) (*subscriptions.RefundResult, error) {
	f.creates.Add(1)
	f.cancel()
	return &subscriptions.RefundResult{ID: "re_exact", Status: "succeeded"}, nil
}

func (f *cancelingStripe) FindRefundByIdempotencyKey(context.Context, string, string) (*subscriptions.RefundResult, bool, error) {
	return nil, false, nil
}

// The request-pinned arm is the production shape at its tightest: a
// one-connection app-role pool whose only connection the request pins, and
// which the cancelled local finalize closes (pgx closes an interrupted BEGIN).
func TestStripeRefundReceiptSurvivesCanceledRequest(t *testing.T) {
	for _, arm := range []string{"unpinned", "request-pinned one-connection pool"} {
		t.Run(arm, func(t *testing.T) {
			fx := seedRefundablePayment(t, 500)
			base := dbtest.WithTestMerchant(context.Background())
			params := fx.stripeEnqueueParams(t, 500)

			run, reqCtx, release := fx.db, base, func() {}
			inScope := func(fn func(context.Context) error) error { return fn(base) }
			if arm != "unpinned" {
				run = dbtest.OpenOneConnAppDB(t)
				var err error
				reqCtx, release, err = run.WithMerchantConn(base)
				require.NoError(t, err)
				inScope = func(fn func(context.Context) error) error { return run.RunInMerchantConn(base, fn) }
			}
			ctx, cancel := context.WithCancel(reqCtx)
			defer cancel()
			stripe := &cancelingStripe{cancel: cancel}
			handler := NewStripeRefundHandler(run, fullModeConfig(), stripeTestRails(), nil)
			handler.Stripe = stripe
			runner := &Runner{Store: NewStore(run), Registry: NewRegistry(handler), Config: fullModeConfig()}

			_, err := runner.EnqueueAndExecute(ctx, params)
			require.ErrorIs(t, err, context.Canceled)
			release()
			if arm != "unpinned" {
				require.Zero(t, run.Pool().Stat().AcquiredConns(), "the request connection is returned")
			}

			row, err := fx.store.GetByIdempotencyKey(base, params.IdempotencyKey)
			require.NoError(t, err)
			require.Equal(t, StatusUnknownNeedsVerify, row.Status, "unknown mark must be durable despite the cancelled request")
			require.Equal(t, "re_exact", EvidenceString(row, "provider_refund_id"))
			_, _, metadata := fx.reservation(t)
			require.Equal(t, "re_exact", metadata["provider_refund_id"], "the receipt write lands before the cancelled finalize")

			_, err = fx.db.Pool().Exec(base, "UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
			require.NoError(t, err)
			require.NoError(t, inScope(func(ctx context.Context) error {
				_, err := runner.RunVerifyOnce(ctx)
				return err
			}))
			require.Equal(t, StatusSucceeded, fx.intentByID(t, row.ID).Status)
			status, txn, _ := fx.reservation(t)
			require.Equal(t, "completed", status)
			require.Equal(t, "re_exact", txn)
			require.EqualValues(t, 1, stripe.creates.Load(), "the saved receipt settles it; the refund is never re-created")
		})
	}
}
