//go:build integration

package reconcile

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/stretchr/testify/require"
)

func TestReconciliationUsesOnlySelectedPSPPriceAndPaymentBindings(t *testing.T) {
	database := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	ctx := dbtest.WithTestMerchant(context.Background())
	accounts := []uuid.UUID{uuid.New(), uuid.New()}
	priceIDs, paymentIDs := map[uuid.UUID]uuid.UUID{}, map[uuid.UUID]uuid.UUID{}
	planRef, transactionRef := "plan-"+uuid.NewString(), "transaction-"+uuid.NewString()
	for _, pspID := range accounts {
		_, err := database.Qx(ctx).Exec(ctx, `INSERT INTO openrails.psps(id,merchant_id,rail,environment,account_id,key) VALUES($1,$2,'nmi','test',$3,$3)`, pspID, dbtest.TestMerchantID.UUID(), uuid.NewString())
		require.NoError(t, err)
		productID, priceID, paymentID := uuid.New(), uuid.New(), uuid.New()
		priceIDs[pspID] = priceID
		paymentIDs[pspID] = paymentID
		customerID := dbtest.EnsureCustomerIDPgx(ctx, t, database.Pool(), uuid.NewString())
		_, err = database.Qx(ctx).Exec(ctx, `INSERT INTO openrails.products(id,merchant_id,key,display_name) VALUES($1,$2,$3,'Reconcile')`, productID, dbtest.TestMerchantID.UUID(), uuid.NewString())
		require.NoError(t, err)
		_, err = database.Qx(ctx).Exec(ctx, `INSERT INTO openrails.prices(id,merchant_id,product_id,amount,currency,auto_renew,access_duration_hours) VALUES($1,$2,$3,10000000,'USD',true,720)`, priceID, dbtest.TestMerchantID.UUID(), productID)
		require.NoError(t, err)
		_, err = database.Qx(ctx).Exec(ctx, `INSERT INTO openrails.price_psp_bindings(merchant_id,price_id,psp_id,plan_id) VALUES($1,$2,$3,$4)`, dbtest.TestMerchantID.UUID(), priceID, pspID, planRef)
		require.NoError(t, err)
		require.NoError(t, payments.NewPaymentService(database).Create(db.WithPSPID(ctx, pspID), &models.Payment{ID: paymentID, CustomerID: customerID, PriceID: priceID, PspID: &pspID, Rail: models.RailNMI, TransactionID: transactionRef, Amount: 10000000, ListAmount: 10000000, Currency: "USD", Status: "completed", MoneyMovement: models.MoneyMovementRail}))
	}
	loader := &PGLocalStateLoader{DB: database}
	for _, pspID := range accounts {
		state, err := loader.Load(ctx, ProviderNMI, pspID)
		require.NoError(t, err)
		require.Len(t, state.Prices, 1)
		require.Equal(t, priceIDs[pspID], state.Prices[0].ID)
		plans := buildPlanIndex(ProviderNMI, state.Prices)
		require.Len(t, plans[planRef], 1)
		require.Equal(t, priceIDs[pspID], plans[planRef][0].price.ID)
		receipts, err := loader.PaymentsByTransactionIDs(ctx, ProviderNMI, pspID, []string{transactionRef})
		require.NoError(t, err)
		require.Len(t, receipts, 1)
		require.Equal(t, paymentIDs[pspID], receipts[0].ID)
	}
}
