//go:build integration

package checkout

import (
	"context"
	"fmt"
	"github.com/open-rails/openrails/internal/modules/payments"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/productaccess"
	"github.com/stretchr/testify/require"
)

func TestReviewSaleRequiresExactPaymentReceipt(t *testing.T) {
	for _, candidateOnly := range []bool{false, true} {
		t.Run(fmt.Sprint(candidateOnly), func(t *testing.T) {
			fx := newSaleIntentFixture(t)
			fx.payload.Amount = 10_000_000 // Actual fixture transaction read is only5USD.
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
			require.Equal(t, intents.OutcomeAmbiguous, out.Class, "candidate/order alone cannot settle frozen10USD when provider onlyreports5USD")
			require.Zero(t, fx.paymentCount(t))
			require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
		})
	}
}

func TestReviewSaleTerminalFailureRollsBackPurchase(t *testing.T) {
	fx := newSaleIntentFixture(t)
	fx.purchase.SetProductAccessService(productaccess.NewService(fx.db))
	ddl := dbtest.SharedSuperuserPGXPool(t)
	name := "review_sale_terminal_" + uuid.NewString()[:8]
	_, err := ddl.Exec(fx.ctx, fmt.Sprintf(`CREATE FUNCTION billing.%s() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'injected sale terminal failure'; END$$; CREATE TRIGGER %s BEFORE UPDATE ON billing.rail_intents FOR EACH ROW WHEN (NEW.price_id='%s'::uuid AND NEW.intent_type='nmi_sale' AND NEW.status='succeeded') EXECUTE FUNCTION billing.%s()`, name, name, fx.priceID, name))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := ddl.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON billing.rail_intents; DROP FUNCTION IF EXISTS billing.%s()`, name, name))
		require.NoError(t, err)
	})
	row := fx.enqueueAndExecute(t, uuid.NewString())
	require.NotEqual(t, intents.StatusSucceeded, row.Status)
	require.Zero(t, fx.paymentCount(t), "purchase row must roll back with terminal failure")
	var grants int
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.grants WHERE customer_id=$1 AND product_id=$2`, fx.customerID, fx.productID).Scan(&grants))
	require.Zero(t, grants)
	fx.advanceClock(2 * time.Minute)
}
