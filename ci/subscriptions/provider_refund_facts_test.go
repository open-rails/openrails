//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/reconcile"
)

// Late provider observations preserve when money moved while accounting and
// review run now. A partial refund never blocks a later legitimate partial.
func TestObservedRefundRetainsHistoryAndAccessReview(t *testing.T) {
	w := newWorld(t)
	w.waive("evidenced", "normalized refund observations exercise local persistence without a provider capture")
	c := w.newCustomer()
	price := w.lifetime("content:refund-facts", 10_000_000)
	priceID, productID := w.priceStorageID(price.ID), price.ProductID.UUID()
	customerID := uuid.MustParse(c.id)
	d, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	ctx := merchant.WithID(t.Context(), w.client[embedded].MerchantID())
	psp := w.psp["nmi"].UUID()
	now, chargedAt := w.clock.Now(), w.clock.Now().Add(-60*day)
	payment := &models.Payment{ID: uuid.New(), CustomerID: customerID, Rail: models.RailNMI, PspID: &psp,
		TransactionID: "historical-sale", Amount: 10_000_000, ListAmount: 10_000_000, Currency: "USD", Status: "succeeded", MoneyMovement: models.MoneyMovementRail, PurchasedAt: chargedAt}
	bill := observed.Bill{PSPID: psp, CustomerID: customerID, Owner: observed.OwnerProvider, Kind: "purchase", SourceKind: "verified_invoice", SourceKey: "refund-bill", ProviderReference: "refund-bill", Currency: "USD", Total: payment.Amount,
		Payment: payment, Settled: true, SettlementSource: "cash", Lines: []observed.Line{{PriceID: &priceID, ProductID: &productID, Description: "Accepted purchase", UnitAmount: payment.Amount, Amount: payment.Amount}}}
	var orderID uuid.UUID
	require.NoError(t, d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		result, err := observed.Record(ctx, d.NewWithPgxTx(tx), bill, now)
		if err != nil {
			return err
		}
		orderID = result.Order.ID
		ledger := grants.New(d.NewWithPgxTx(tx).Gen(ctx), w.client[embedded].MerchantID().UUID())
		ledger.SetClock(w.clock.Now)
		grant, _, err := ledger.GrantAccessOnce(ctx, grants.GrantInput{Customer: customerID, Product: &productID, Kind: grants.Access, Source: grants.Purchase, SourceID: payment.ID.String(), Payment: &payment.ID, StartsAt: chargedAt})
		if err != nil {
			return err
		}
		return ledger.MaterializeGrant(ctx, grant)
	}))
	writer := &reconcile.PGLocalWriter{DB: d, Now: w.clock.Now}
	refundedAt := now.Add(-32 * day)
	action := reconcile.RecordRefundAction{PspID: &psp, Rail: "nmi", TransactionID: "first-partial",
		AmountCents: 200, Currency: "USD", PurchasedAt: refundedAt, RefundedPaymentID: &payment.ID, CustomerID: customerID,
		Metadata: map[string]any{"reconcile_refund_backfill": true, "provider": "nmi", "provider_transaction_id": "first-partial"}}
	changed, err := writer.RecordRefund(ctx, action)
	require.NoError(t, err)
	require.True(t, changed)
	svc := payments.NewPaymentService(d, w.clock)
	recorded, err := svc.GetByPSPTransactionID(db.WithPSPID(ctx, psp), models.RailNMI, action.TransactionID)
	require.NoError(t, err)
	require.Equal(t, orderID, *recorded.OrderID)
	require.True(t, refundedAt.Equal(recorded.PurchasedAt))
	require.True(t, now.Equal(recorded.CreatedAt))
	require.Equal(t, action.Metadata, recorded.Metadata)
	changed, err = writer.RecordRefund(ctx, action)
	require.NoError(t, err)
	require.False(t, changed)
	conflicting := action
	conflicting.AmountCents++
	_, err = writer.RecordRefund(ctx, conflicting)
	require.ErrorContains(t, err, "different payment facts")
	// Later discovery metadata cannot rewrite the first accepted evidence.
	rediscovered := action
	rediscovered.Metadata = map[string]any{"provider": "nmi", "later_discovery": true}
	changed, err = writer.RecordRefund(ctx, rediscovered)
	require.NoError(t, err)
	require.False(t, changed)
	retained, err := svc.GetByID(ctx, recorded.ID)
	require.NoError(t, err)
	require.True(t, refundedAt.Equal(retained.PurchasedAt))
	require.Equal(t, recorded.Metadata, retained.Metadata)
	action.TransactionID, action.PurchasedAt = "second-partial", refundedAt.Add(time.Hour)
	changed, err = writer.RecordRefund(ctx, action)
	require.NoError(t, err)
	require.True(t, changed)
	original, err := svc.GetByID(ctx, payment.ID)
	require.NoError(t, err)
	require.Equal(t, "succeeded", original.Status)
	total, err := svc.GetRefundTotalByPaymentID(ctx, payment.ID)
	require.NoError(t, err)
	require.Equal(t, int64(4_000_000), total)
	w.converge()
	require.NotEmpty(t, w.openFindings("derive.grant.excess"), "live access backed by refunded money needs human review")
	require.True(t, c.entitled("content:refund-facts"), "a review finding never revokes access automatically")
	require.Equal(t, float64(2), w.metricTotal(refundedAt.Add(-day), "refund_count", nil))
	require.Zero(t, w.metricTotal(now.Add(-day), "refund_count", nil), "late discovery is not current-period refund activity")
	require.Zero(t, w.engineCharges())
}
