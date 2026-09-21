//go:build integration

package checkout

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/stretchr/testify/require"
)

func TestReviewSaleRequiresExactPaymentReceipt(t *testing.T) {
	for _, candidateOnly := range []bool{false, true} {
		t.Run(fmt.Sprint(candidateOnly), func(t *testing.T) {
			fx := newSaleIntentFixture(t)
			fx.payload.Amount = 10_000_000 // Actual fixture transaction read is only 5 USD.
			fx.gateway.saleMode.Store("ambiguous500")
			row := fx.enqueueAndExecute(t, uuid.NewString())
			require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
			require.Zero(t, fx.paymentCount(t))
			if candidateOnly {
				fx.gateway.hidden.Store(true)
				require.NoError(t, intents.NewStore(fx.db).RecordProgress(fx.ctx, row.ID, map[string]any{"transaction_id": fx.gateway.txnID}))
				var err error
				row, err = intents.NewStore(fx.db).Get(fx.ctx, row.ID)
				require.NoError(t, err)
			}
			out := fx.runner.Registry.Lookup(payments.TypeNMISale).Verify(db.WithPSPID(fx.ctx, *row.PspID), row)
			require.Equal(t, intents.OutcomeAmbiguous, out.Class, "candidate/order alone cannot settle frozen 10 USD when provider only reports 5 USD")
			require.Zero(t, fx.paymentCount(t))
			require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
		})
	}
}
