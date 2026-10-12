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
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// An in-place provider amendment cannot relabel an already accepted receipt.
// New receipts must match the version effective for their proven service cycle.
func TestProviderScheduleVersionedReceiptIdentity(t *testing.T) {
	w := newWorld(t)
	w.waive("evidenced", "normalized schedule receipts test immutable version admission without capturing money")
	l := importLegacy(t, w, "nmi", embedded)
	d, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	ctx := db.WithPSPID(merchant.WithID(t.Context(), w.client[embedded].MerchantID()), w.psp["nmi"].UUID())
	due := l.periodEnd()
	end := due.Add(monthHours * time.Hour)
	original := observed.ScheduleReceipt{TransactionID: "before-amendment", Amount: 9_990_000, Currency: "USD", OccurredAt: due.Add(time.Hour), NextBillingAt: end, Approved: true}
	resolve := func(receipt observed.ScheduleReceipt) (bill observed.Bill, err error) {
		err = d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			txdb := d.NewWithPgxTx(tx)
			sub, err := subscriptions.NewSubscriptionRepo(txdb).GetByIDForUpdate(ctx, l.sub.UUID())
			if err != nil {
				return err
			}
			bill, err = observed.ScheduleBill(ctx, txdb, sub, receipt)
			return err
		})
		return
	}
	accepted, err := resolve(original)
	require.NoError(t, err)
	priceID, productID, subID := w.priceStorageID(l.price.ID), l.price.ProductID.UUID(), l.sub.UUID()
	hours := monthHours
	amendment := observed.Bill{PSPID: w.psp["nmi"].UUID(), CustomerID: uuid.MustParse(l.c.id), SubscriptionID: &subID, Owner: observed.OwnerNMISchedule, ProviderReference: l.railSub, Currency: "USD", Total: 19_990_000, Lines: []observed.Line{{PriceID: &priceID, ProductID: &productID, Description: "Accepted amended subscription", UnitAmount: 19_990_000, Amount: 19_990_000, BillingIntervalHours: &hours}}}
	require.NoError(t, d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return observed.DeclareSchedule(ctx, d.NewWithPgxTx(tx), "verified-amendment", amendment, due, hours, 0, w.clock.Now())
	}))
	replay, err := resolve(original)
	require.NoError(t, err)
	require.Equal(t, accepted.SourceKey, replay.SourceKey)
	require.Equal(t, accepted.Total, replay.Total)
	stale := original
	stale.TransactionID = "new-old-price-receipt"
	_, err = resolve(stale)
	require.ErrorContains(t, err, "exactly one accepted schedule version")
	current := original
	current.TransactionID, current.Amount = "new-amended-receipt", amendment.Total
	revised, err := resolve(current)
	require.NoError(t, err)
	require.NotEqual(t, accepted.SourceKey, revised.SourceKey)
	require.Equal(t, amendment.Total, revised.Total)
	require.True(t, revised.PeriodStart.Equal(due))
	// Two confirmed changes before the same renewal retain their acceptance
	// ordering. Replaying the earlier amendment cannot promote it again.
	latest := amendment
	latest.Lines = append([]observed.Line(nil), amendment.Lines...)
	latest.Total, latest.Lines[0].Amount, latest.Lines[0].UnitAmount = 29_990_000, 29_990_000, 29_990_000
	declare := func(source string, bill observed.Bill) error {
		return d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return observed.DeclareSchedule(ctx, d.NewWithPgxTx(tx), source, bill, due, hours, 0, w.clock.Now())
		})
	}
	require.NoError(t, declare("second-confirmed-amendment", latest))
	require.NoError(t, declare("verified-amendment", amendment))
	firstReplay, err := resolve(current)
	require.NoError(t, err)
	require.Equal(t, revised.SourceKey, firstReplay.SourceKey)
	current.TransactionID = "stale-first-amendment"
	_, err = resolve(current)
	require.ErrorContains(t, err, "exactly one accepted schedule version")
	current.TransactionID, current.Amount = "latest-confirmed-amendment", latest.Total
	last, err := resolve(current)
	require.NoError(t, err)
	require.Equal(t, latest.Total, last.Total)
	require.NotEqual(t, revised.SourceKey, last.SourceKey)
	require.Zero(t, w.engineCharges())
}
