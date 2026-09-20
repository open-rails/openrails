//go:build integration

package intents

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/stretchr/testify/require"
)

func TestCustomerRebillSharesOwnershipAndReplaysAcceptedBody(t *testing.T) {
	for _, first := range []string{"customer", "worker"} {
		t.Run(first, func(t *testing.T) {
			fx := seedPastDueSubscription(t)
			gateway, client := newFakeNMIRebillGateway(t, fx)
			ctx := fx.handlerCtx()
			h := NewManualRebillHandler(fx.db, fullModeConfig(), fakeNMIResolver{client: client}, nil)
			key := uuid.NewString()
			payer := fx.payload.Renewal.CustomerID
			if first == "worker" {
				row, err := h.EnqueueScheduled(ctx, fx.subID)
				require.NoError(t, err)
				_, _, err = h.EnqueueCustomer(ctx, fx.subID, payer, key, nil)
				require.ErrorIs(t, err, ErrRebillInProgress)
				require.EqualValues(t, 0, gateway.saleCalls.Load())
				got, err := fx.rebillRunner(client, fullModeConfig()).ExecuteByID(ctx, row.ID)
				require.NoError(t, err)
				require.Equal(t, StatusSucceeded, got.Status)
				form := gateway.saleForm.Load().(url.Values)
				require.Equal(t, "merchant", form.Get("initiated_by"))
				return
			}
			// Equal customer requests serialize on the same subscription lock. The
			// scheduled producer must reuse that accepted operation as well.
			start := make(chan struct{})
			type result struct {
				id  uuid.UUID
				err error
			}
			done := make(chan result, 2)
			for range 2 {
				go func() {
					<-start
					row, _, err := h.EnqueueCustomer(ctx, fx.subID, payer, key, nil)
					done <- result{row.ID, err}
				}()
			}
			close(start)
			a, b := <-done, <-done
			require.NoError(t, a.err)
			require.NoError(t, b.err)
			require.Equal(t, a.id, b.id)
			same, err := h.EnqueueScheduled(ctx, fx.subID)
			require.NoError(t, err)
			require.Equal(t, a.id, same.ID)
			terms, err := DecodeManualRebillPayload(same)
			require.NoError(t, err)
			require.Equal(t, charge.InitiatorCustomer, terms.Initiator)
			_, _, err = h.EnqueueCustomer(ctx, fx.subID, payer, uuid.NewString(), nil)
			require.ErrorIs(t, err, ErrRebillInProgress)
			_, _, err = h.EnqueueCustomer(ctx, fx.subID, uuid.New(), key, nil)
			require.ErrorIs(t, err, pgx.ErrNoRows)
			method := terms.PaymentMethodID
			_, _, err = h.EnqueueCustomer(ctx, fx.subID, payer, key, &method)
			require.ErrorIs(t, err, ErrRebillKeyConflict, "absent and supplied are different accepted bodies")
			gateway.saleStatus.Store(http.StatusBadGateway)
			row, err := fx.rebillRunner(client, fullModeConfig()).ExecuteByID(ctx, a.id)
			require.NoError(t, err)
			require.Equal(t, StatusUnknownNeedsVerify, row.Status)
			same, replayed, err := h.EnqueueCustomer(ctx, fx.subID, payer, key, nil)
			require.NoError(t, err)
			require.True(t, replayed)
			require.Equal(t, row.ID, same.ID)
			require.EqualValues(t, 1, gateway.saleCalls.Load())
			gateway.charged.Store(true)
			require.Equal(t, OutcomeSucceeded, h.Verify(ctx, row).Class)
			same, replayed, err = h.EnqueueCustomer(ctx, fx.subID, payer, key, nil)
			require.NoError(t, err)
			require.True(t, replayed)
			require.Equal(t, StatusSucceeded, same.Status)
			form := gateway.saleForm.Load().(url.Values)
			require.Equal(t, "customer", form.Get("initiated_by"))
			require.Equal(t, "used", form.Get("stored_credential_indicator"))
			require.Equal(t, terms.Instrument.StoredCredentialRecurringRef, form.Get("initial_transaction_id"))
			require.EqualValues(t, 1, gateway.saleCalls.Load())
			require.Equal(t, 1, fx.paymentsFor(t, gateway.txnID))
			require.Equal(t, "active", string(fx.subscription(t).Status))
		})
	}
}
