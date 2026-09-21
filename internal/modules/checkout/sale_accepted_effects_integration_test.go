//go:build integration

package checkout

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/stretchr/testify/require"
)

func TestSaleAcceptedWindowsSurviveCatalogChangeAndDelayedReceipt(t *testing.T) {
	fx := newSaleIntentFixture(t)
	hours := 24
	accepted := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	entitlementStart := accepted.Add(12 * time.Hour)
	ownershipEnd := accepted.Add(24 * time.Hour)
	fx.payload.AcceptedAt, fx.payload.OwnershipStart = accepted, accepted
	fx.payload.EntitlementStart, fx.payload.OwnershipEnd = entitlementStart, &ownershipEnd
	fx.payload.AccessDurationHours = &hours
	fx.payload.Entitlements = map[string]*int{"accepted_feature": nil}
	fx.gateway.saleMode.Store("ambiguous500")
	row := fx.enqueueAndExecute(t, uuid.NewString())
	require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
	_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.products SET entitlements_spec='{"later_feature":null}' WHERE id=$1`, fx.productID)
	require.NoError(t, err)
	_, err = fx.db.Pool().Exec(fx.ctx, `UPDATE billing.prices SET access_duration_hours=720 WHERE id=$1`, fx.priceID)
	require.NoError(t, err)
	out := fx.runner.Registry.Lookup(payments.TypeNMISale).Verify(db.WithPSPID(fx.ctx, *row.PspID), row)
	require.Equal(t, intents.OutcomeSucceeded, out.Class, out.Reason)
	var start, end time.Time
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT starts_at,ends_at FROM billing.grants WHERE customer_id=$1 AND kind='ownership'`, fx.customerID).Scan(&start, &end))
	require.Equal(t, accepted, start.UTC())
	require.Equal(t, ownershipEnd, end.UTC(), "ownership never starts over at recovery")
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT start_at,end_at FROM billing.entitlements WHERE customer_id=$1 AND entitlement='accepted_feature'`, fx.customerID).Scan(&start, &end))
	require.Equal(t, entitlementStart, start.UTC())
	require.Equal(t, entitlementStart.Add(24*time.Hour), end.UTC(), "the accepted stacked entitlement meaning is preserved separately")
	var later int
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.entitlements WHERE customer_id=$1 AND entitlement='later_feature'`, fx.customerID).Scan(&later))
	require.Zero(t, later)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
}

func TestSaleObservedPaymentRepairsOnlyMatchingFrozenBenefits(t *testing.T) {
	for _, contradictory := range []bool{false, true} {
		name := "matching"
		if contradictory {
			name = "contradictory"
		}
		t.Run(name, func(t *testing.T) {
			fx := newSaleIntentFixture(t)
			fx.payload.Entitlements = map[string]*int{"accepted_feature": nil}
			fx.gateway.saleMode.Store("ambiguous500")
			row := fx.enqueueAndExecute(t, uuid.NewString())
			require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
			snapshot := models.CloneEntitlementsSpec(fx.payload.Entitlements)
			if contradictory {
				snapshot = map[string]*int{"another_benefit": nil}
			}
			observed := &models.Payment{ID: uuid.New(), CustomerID: fx.customerID, PriceID: fx.priceID, Rail: "nmi", TransactionID: fx.gateway.txnID, Amount: fx.payload.Amount, ListAmount: fx.payload.ListAmount, Currency: fx.payload.Currency, Status: payments.PaymentStatusCompletedValue, EntitlementsSpecSnapshot: snapshot, MoneyMovement: models.MoneyMovementRail, PurchasedAt: fx.payload.AcceptedAt}
			scoped := db.WithPSPID(fx.ctx, *row.PspID)
			created, err := fx.purchase.PaymentService.CreateIfNotExists(scoped, observed)
			require.NoError(t, err)
			require.True(t, created)
			handler := fx.runner.Registry.Lookup(payments.TypeNMISale)
			out := handler.Verify(scoped, row)
			var access, events int
			require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.grants WHERE customer_id=$1`, fx.customerID).Scan(&access))
			if contradictory {
				require.Equal(t, intents.OutcomeAmbiguous, out.Class)
				require.Zero(t, access)
			} else {
				require.Equal(t, intents.OutcomeSucceeded, out.Class, out.Reason)
				require.Equal(t, observed.ID.String(), out.Evidence["payment_id"])
				require.Positive(t, access)
				again := handler.Verify(scoped, row)
				require.Equal(t, intents.OutcomeSucceeded, again.Class)
			}
			require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.host_outbox WHERE payment_id=$1`, observed.ID).Scan(&events))
			require.Equal(t, 1, events)
			require.Equal(t, 1, fx.paymentCount(t))
			require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
		})
	}
}

func TestSaleRecoveryPreservesAnotherPurchasesLaterEntitlement(t *testing.T) {
	productID, priceID := uuid.New(), uuid.New()
	var fx *saleIntentFixture
	t.Cleanup(func() {
		if fx != nil {
			_, _ = fx.db.Pool().Exec(fx.ctx, `DELETE FROM billing.prices WHERE id=$1`, priceID)
			_, _ = fx.db.Pool().Exec(fx.ctx, `DELETE FROM billing.products WHERE id=$1`, productID)
		}
	})
	fx = newSaleIntentFixture(t)
	hours := 24
	accepted := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	end := accepted.Add(24 * time.Hour)
	fx.payload.AcceptedAt, fx.payload.EntitlementStart, fx.payload.OwnershipStart = accepted, accepted, accepted
	fx.payload.AccessDurationHours, fx.payload.OwnershipEnd = &hours, &end
	fx.payload.Entitlements = map[string]*int{"shared_purchase_feature": nil}
	fx.gateway.saleMode.Store("ambiguous500")
	first := fx.enqueueAndExecute(t, uuid.NewString())
	require.Equal(t, intents.StatusUnknownNeedsVerify, first.Status)
	now := time.Now().UTC().Truncate(time.Microsecond)
	insertProductAndPrice(fx.ctx, t, fx.db.Pool(), &models.Product{ID: productID, Key: "later-purchase-" + productID.String(), DisplayName: "Later purchase", EntitlementsSpec: map[string]*int{"shared_purchase_feature": nil}, CreatedAt: now, UpdatedAt: now}, &models.Price{ID: priceID, ProductID: productID, Amount: 5_000_000, Currency: "USD", AccessDurationHours: &hours, CreatedAt: now, UpdatedAt: now})

	// Another real observed purchase completes while the first is provider-unknown.
	// It owns its own later rights on the shared entitlement timeline.
	observed, err := fx.purchase.RegisterPurchase(db.WithPSPID(fx.ctx, *first.PspID), &payments.RegisterPurchaseRequest{UserID: fx.userID, PriceID: priceID, Rail: "nmi", TransactionID: "later-sale-" + uuid.NewString(), Amount: 5_000_000, AmountProvided: true, Currency: "USD"})
	require.NoError(t, err)
	var laterStart, laterEnd time.Time
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT start_at,end_at FROM billing.entitlements WHERE source_id=$1 AND entitlement='shared_purchase_feature'`, observed.PaymentID).Scan(&laterStart, &laterEnd))
	out := fx.runner.Registry.Lookup(payments.TypeNMISale).Verify(db.WithPSPID(fx.ctx, *first.PspID), first)
	require.Equal(t, intents.OutcomeSucceeded, out.Class, out.Reason)
	var originalStart, originalEnd, afterStart, afterEnd time.Time
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT start_at,end_at FROM billing.entitlements WHERE source_id=$1 AND entitlement='shared_purchase_feature'`, fx.payload.PaymentID).Scan(&originalStart, &originalEnd))
	require.True(t, originalStart.Equal(accepted))
	require.True(t, originalEnd.Equal(end))
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT start_at,end_at FROM billing.entitlements WHERE source_id=$1 AND entitlement='shared_purchase_feature'`, observed.PaymentID).Scan(&afterStart, &afterEnd))
	require.True(t, laterStart.Equal(afterStart))
	require.True(t, laterEnd.Equal(afterEnd))
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
}

func TestSaleAcceptedIndefiniteEntitlementRetainsHistoricalStart(t *testing.T) {
	fx := newSaleIntentFixture(t)
	accepted := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	fx.payload.AcceptedAt, fx.payload.EntitlementStart, fx.payload.OwnershipStart = accepted, accepted, accepted
	fx.payload.Entitlements = map[string]*int{"indefinite_accepted_feature": nil}
	fx.gateway.saleMode.Store("ambiguous500")
	row := fx.enqueueAndExecute(t, uuid.NewString())
	require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
	out := fx.runner.Registry.Lookup(payments.TypeNMISale).Verify(db.WithPSPID(fx.ctx, *row.PspID), row)
	require.Equal(t, intents.OutcomeSucceeded, out.Class, out.Reason)
	var start time.Time
	var end *time.Time
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT start_at,end_at FROM billing.entitlements WHERE customer_id=$1 AND entitlement='indefinite_accepted_feature'`, fx.customerID).Scan(&start, &end))
	require.True(t, accepted.Equal(start), "indefinite paid access retains its accepted start, not receipt recovery time")
	require.Nil(t, end)
}
