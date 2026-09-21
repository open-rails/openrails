//go:build integration

package webhooks

import (
	"github.com/open-rails/openrails/pkg/merchant"

	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
)

func TestNMIRefundWebhookIsolatesArchivedPSPWithCollidingReferences(t *testing.T) {
	database := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	ctx := dbtest.WithTestMerchant(context.Background())
	pspA, pspB := uuid.New(), uuid.New()
	accountA, accountB := "account-a-"+uuid.NewString(), "account-b-"+uuid.NewString()
	for id, account := range map[uuid.UUID]string{pspA: accountA, pspB: accountB} {
		_, err := database.Qx(ctx).Exec(ctx, `INSERT INTO billing.psps(id,merchant_id,rail,environment,account_id,key,archived) VALUES($1,$2,'nmi','test',$3,$3,$4)`, id, dbtest.TestMerchantID.UUID(), account, id == pspB)
		require.NoError(t, err)
	}
	priceService, productService := catalog.NewPriceService(database), catalog.NewProductService(database)
	paymentService := payments.NewPaymentService(database)
	entitlementService := entitlements.NewEntitlementService(database)
	lifecycle := subscriptions.NewSubscriptionLifecycleService(database, productService, priceService, entitlementService, subscriptions.NewNotificationService(database, nil), paymentService)
	subService := subscriptions.NewSubscriptionService(database, priceService, productService, nil, nil)
	productID, priceID := uuid.New(), uuid.New()
	_, err := database.Qx(ctx).Exec(ctx, `INSERT INTO billing.products(id,merchant_id,key,display_name) VALUES($1,$2,$3,'Collision')`, productID, dbtest.TestMerchantID.UUID(), uuid.NewString())
	require.NoError(t, err)
	_, err = database.Qx(ctx).Exec(ctx, `INSERT INTO billing.prices(id,merchant_id,product_id,amount,currency,auto_renew,access_duration_hours) VALUES($1,$2,$3,10000000,'USD',true,720)`, priceID, dbtest.TestMerchantID.UUID(), productID)
	require.NoError(t, err)
	subRef, chargeRef, refundRef := "sub-"+uuid.NewString(), "charge-"+uuid.NewString(), "refund-"+uuid.NewString()
	subIDs := map[uuid.UUID]uuid.UUID{}
	paymentIDs := map[uuid.UUID]uuid.UUID{}
	for _, pspID := range []uuid.UUID{pspA, pspB} {
		scoped := db.WithPSPID(ctx, pspID)
		customerID := dbtest.EnsureCustomerIDPgx(ctx, t, database.Pool(), uuid.NewString())
		subID, paymentID := uuid.New(), uuid.New()
		subIDs[pspID] = subID
		paymentIDs[pspID] = paymentID
		start, end := time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(29*24*time.Hour)
		_, err = database.Qx(ctx).Exec(ctx, `INSERT INTO billing.subscriptions(id,merchant_id,customer_id,product_id,price_id,rail,psp_id,rail_subscription_id,status,current_period_starts_at,current_period_ends_at) VALUES($1,$2,$3,$4,$5,'nmi',$6,$7,'active',$8,$9)`, subID, dbtest.TestMerchantID.UUID(), customerID, productID, priceID, pspID, subRef, start, end)
		require.NoError(t, err)
		require.NoError(t, paymentService.Create(scoped, &models.Payment{ID: paymentID, CustomerID: customerID, PriceID: priceID, SubscriptionID: &subID, Rail: models.RailNMI, PspID: &pspID, TransactionID: chargeRef, Amount: 10000000, ListAmount: 10000000, Currency: "USD", Status: "completed", MoneyMovement: models.MoneyMovementRail}))
	}
	// Account resolution is the inbound authentication boundary's directory
	// lookup. An archived PSP still routes to its captured immutable identity.
	merchantService, err := merchants.NewService(db.WrapPool(database.Pool(), ""), merchants.NewMemorySecretStore(), "test")
	require.NoError(t, err)
	identity, found, err := merchantService.ResolvePSPByIdentity(ctx, "nmi", "test", accountB)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, pspB, identity.ID)
	scoped := db.WithPSPID(ctx, identity.ID)
	service := &NMIWebhookService{DB: database, Rail: "nmi", PriceService: priceService, ProductService: productService, PaymentService: paymentService, SubscriptionService: subService, SubscriptionLifecycleService: lifecycle,
		Data: NMIWebhookEvent{EventID: uuid.NewString(), EventType: EventTypeNMIRefundSuccess, EventBody: mustJSON(t, map[string]any{"transaction_id": refundRef, "amount": "10.00", "subscription": map[string]string{"subscription_id": subRef}, "transaction": map[string]string{"transaction_id": chargeRef}})}}
	dedup, err := NewDeduplicationService(nil, database)
	require.NoError(t, err)
	service.DeduplicationService = dedup
	require.NoError(t, service.HandleNMIWebhook(scoped))
	require.NoError(t, service.HandleNMIWebhook(scoped), "duplicate delivery does not duplicate reversal")
	refund, err := paymentService.GetByPSPTransactionID(scoped, models.RailNMI, refundRef)
	require.NoError(t, err)
	require.Equal(t, paymentIDs[pspB], *refund.RefundedPaymentID)
	for _, pspID := range []uuid.UUID{pspA, pspB} {
		sub, err := subService.GetByPSPSubscriptionID(db.WithPSPID(ctx, pspID), "nmi", subRef)
		require.NoError(t, err)
		require.Equal(t, subIDs[pspID], sub.ID)
		if pspID == pspA {
			require.Equal(t, models.StatusActive, sub.Status)
		} else {
			require.Equal(t, models.StatusCancelled, sub.Status)
		}
	}
	_, err = paymentService.GetByPSPTransactionID(db.WithPSPID(ctx, pspA), models.RailNMI, refundRef)
	require.True(t, db.IsNotFound(err))
	// The sibling's independently authenticated delivery has the same event ID.
	// It must execute once under A, rather than hit B's completed-event mark.
	require.NoError(t, service.HandleNMIWebhook(db.WithPSPID(ctx, pspA)))
	refundA, err := paymentService.GetByPSPTransactionID(db.WithPSPID(ctx, pspA), models.RailNMI, refundRef)
	require.NoError(t, err)
	require.Equal(t, paymentIDs[pspA], *refundA.RefundedPaymentID)
}

func TestCustodianWebhookAndUpdaterIsolateCollidingAccounts(t *testing.T) {
	fx := newBTWebhookFixture(t)
	ctx := fx.ctx
	custodianA := db.CustodianIDFromContext(ctx)
	custodianB := dbtest.EnsureTestCustodian(ctx, t, fx.dbi.Pool(), dbtest.TestMerchantID.UUID())
	pspB, methodB := uuid.New(), uuid.New()
	_, err := fx.dbi.Qx(ctx).Exec(ctx, `INSERT INTO billing.psps(id,merchant_id,rail,environment,account_id,key,custodian_id) VALUES($1,$2,'nmi','test',$3,$3,$4)`, pspB, dbtest.TestMerchantID.UUID(), uuid.NewString(), custodianB)
	require.NoError(t, err)
	_, err = fx.dbi.Gen(ctx).CreatePaymentMethod(ctx, gen.CreatePaymentMethodParams{
		ID: methodB, MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: fx.customerID,
		Rail: "nmi", PspID: pspB, Custodian: models.CustodianBasisTheory, CustodianID: &custodianB,
		RailMethodRef: fx.tokenID, NetworkTokenID: fx.ntID, NetworkTokenStatus: "active",
	})
	require.NoError(t, err)
	// Rename/archive the authenticated account after the instrument was stored.
	_, err = fx.dbi.Qx(ctx).Exec(ctx, `UPDATE billing.custodians SET key=$2,archived=true WHERE id=$1`, custodianA, uuid.NewString())
	require.NoError(t, err)
	eventID := uuid.NewString()
	require.NoError(t, fx.deliver(t, eventID, basistheory.EventTokenExpired, map[string]any{"token": map[string]any{"id": fx.tokenID}}))
	require.Equal(t, "bt_token_expired", fx.methodRow(t).ParkReason)
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	require.NoError(t, scopeErr)
	other, err := fx.dbi.Gen(ctx).GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: scopeMerchantID.UUID(), ID: methodB})
	require.NoError(t, err)
	require.Empty(t, other.ParkReason)

	stats, err := FoldAccountUpdaterResults(ctx, fx.dbi.Gen(ctx), []basistheory.AccountUpdaterResultRow{{Token: fx.tokenID, NewToken: "rotated-" + fx.tokenID, ResultCode: "UPD_EXP_DATE", NewExpirationMonth: "11", NewExpirationYear: "2032"}})
	require.NoError(t, err)
	require.Equal(t, 1, stats.Rotated)
	require.Empty(t, fx.methodRow(t).ParkReason)
	other, err = fx.dbi.Gen(ctx).GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: scopeMerchantID.UUID(), ID: methodB})
	require.NoError(t, err)
	require.Equal(t, fx.tokenID, other.RailMethodRef)

	// Provider job references are also local to the custodian account.
	jobRef := uuid.NewString()
	for _, cid := range []uuid.UUID{custodianA, custodianB} {
		_, err := fx.dbi.Qx(ctx).Exec(ctx, `INSERT INTO billing.account_updater_batches(merchant_id,custodian_id,instruments,status,job_ref) VALUES($1,$2,'[]','submitted',$3)`, dbtest.TestMerchantID.UUID(), cid, jobRef)
		require.NoError(t, err)
	}
	require.NoError(t, CloseAccountUpdaterBatch(ctx, fx.dbi.Gen(ctx), jobRef, stats))
	var state string
	require.NoError(t, fx.dbi.Qx(ctx).QueryRow(ctx, `SELECT status FROM billing.account_updater_batches WHERE custodian_id=$1 AND job_ref=$2`, custodianB, jobRef).Scan(&state))
	require.Equal(t, "submitted", state)

	// Reusing A's event ID under B still applies B's own independent event.
	fx.ctx = db.WithCustodianID(ctx, custodianB)
	require.NoError(t, fx.deliver(t, eventID, basistheory.EventTokenExpired, map[string]any{"token": map[string]any{"id": fx.tokenID}}))
	other, err = fx.dbi.Gen(ctx).GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: scopeMerchantID.UUID(), ID: methodB})
	require.NoError(t, err)
	require.Equal(t, "bt_token_expired", other.ParkReason)
}
