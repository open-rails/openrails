//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
)

// These are normalized, authenticated receipt fixtures, deliberately below
// provider transport. The writer must retain facts without issuing a charge.
func TestObservedBillMoneyFidelity(t *testing.T) {
	w := newWorld(t)
	w.waive("evidenced", "normalized provider receipt fixtures exercise the local writer without calling a payment gateway")
	c := w.newCustomer()
	price := w.lifetime("observed-test", 100)
	var priceID uuid.UUID
	productID := price.ProductID.UUID()
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT id FROM billing.prices WHERE product_id=$1 AND key=$2`), productID, price.Key).Scan(&priceID))
	d, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	ctx := merchant.WithID(t.Context(), w.client[embedded].MerchantID())
	quantity := 1
	base := observed.Bill{PSPID: w.psp["nmi"].UUID(), CustomerID: uuid.MustParse(c.id), Owner: observed.OwnerProvider, Kind: "purchase", SourceKind: "verified_invoice", SourceKey: "invoice-1", ProviderReference: "invoice-1", Currency: "USD", Total: 100, Lines: []observed.Line{{PriceID: &priceID, ProductID: &productID, Description: "Accepted goods", Quantity: &quantity, UnitAmount: 100, Amount: 100}}}
	capture := func(id string, amount int64) *models.Payment {
		return &models.Payment{ID: uuid.New(), CustomerID: base.CustomerID, Rail: models.RailNMI, PspID: &base.PSPID, TransactionID: id, Amount: amount, ListAmount: 100, Currency: "USD", Status: "succeeded", MoneyMovement: models.MoneyMovementRail}
	}
	record := func(b observed.Bill) (out observed.Result, err error) {
		err = d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var e error
			out, e = observed.Record(ctx, d.NewWithPgxTx(tx), b, w.clock.Now())
			return e
		})
		return
	}
	first := base
	first.Payment = capture("observed-partial-1", 60)
	results := make(chan observed.Result, 8)
	errs := make(chan error, 8)
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() { defer workers.Done(); out, err := record(first); results <- out; errs <- err }()
	}
	workers.Wait()
	close(results)
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
	var order uuid.UUID
	created, paymentCreated := 0, 0
	for out := range results {
		if out.Created {
			created++
		}
		if out.PaymentCreated {
			paymentCreated++
		}
		order = out.Order.ID
		require.False(t, out.Completed)
		require.Equal(t, "open", out.Order.Status)
	}
	require.Equal(t, 1, created)
	require.Equal(t, 1, paymentCreated)
	second := base
	second.Payment = capture("observed-partial-2", 40)
	second.Settled = true
	second.SettlementSource = "cash"
	done, err := record(second)
	require.NoError(t, err)
	require.True(t, done.Completed)
	require.Equal(t, order, done.Order.ID)
	require.Equal(t, "complete", done.Order.Status)
	replay, err := record(second)
	require.NoError(t, err)
	require.False(t, replay.Completed)
	require.False(t, replay.PaymentCreated)
	extra := base
	extra.Payment = capture("observed-genuine-extra", 25)
	extraResult, err := record(extra)
	require.NoError(t, err)
	require.True(t, extraResult.PaymentCreated)
	require.False(t, extraResult.Completed)
	require.EqualValues(t, 100, extraResult.Order.Total)
	var payments, findings, events, grants int
	require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT count(*) FROM billing.payments WHERE order_id=$1`), order).Scan(&payments))
	require.Equal(t, 3, payments)
	require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT count(*) FROM billing.reconciliation_findings WHERE finding_type='provider_bill_overpaid' AND subject_key=$1`), "order:"+order.String()).Scan(&findings))
	require.Equal(t, 1, findings)
	require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT count(*) FROM billing.host_outbox WHERE event_type='order.completed' AND subject_id=$1`), order).Scan(&events))
	require.Equal(t, 1, events)
	require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT count(*) FROM billing.grants WHERE customer_id=$1`), base.CustomerID).Scan(&grants))
	require.Zero(t, grants, "observing money never grants access")
	changed := first
	changed.Payment = capture("observed-partial-1", 61)
	_, err = record(changed)
	require.ErrorIs(t, err, observed.ErrConflict)
	changed = base
	changed.Lines = append([]observed.Line(nil), base.Lines...)
	changed.Total = 101
	changed.Lines[0].Amount = 101
	_, err = record(changed)
	require.ErrorIs(t, err, observed.ErrConflict)
	changed = base
	changed.SourceKey = "different-bill"
	changed.Payment = capture("observed-partial-1", 60)
	_, err = record(changed)
	require.ErrorIs(t, err, observed.ErrConflict, "a transaction cannot settle a second obligation")
	require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT count(*) FROM billing.provider_bills WHERE source_key='different-bill'`)).Scan(&payments))
	require.Zero(t, payments, "rejected replay leaves no partial bill")
}

func TestObservedBillNonCashAndRollback(t *testing.T) {
	w := newWorld(t)
	c := w.newCustomer()
	d, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	ctx := merchant.WithID(t.Context(), w.client[embedded].MerchantID())
	b := observed.Bill{PSPID: w.psp["stripe"].UUID(), CustomerID: uuid.MustParse(c.id), Owner: observed.OwnerProvider, Kind: "purchase", SourceKind: "invoice", SourceKey: "credit-bill", Currency: "USD", Total: 99, Settled: true, SettlementSource: "credit", NonCashAmount: 99, Lines: []observed.Line{{Source: "provider_adjustment", Description: "Verified invoice total", UnitAmount: 99, Amount: 99}}}
	rollback := errors.New("lost local transaction")
	require.ErrorIs(t, d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := observed.Record(ctx, d.NewWithPgxTx(tx), b, w.clock.Now())
		if err != nil {
			return err
		}
		return rollback
	}), rollback)
	var count int
	require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT count(*) FROM billing.provider_bills WHERE source_key='credit-bill'`)).Scan(&count))
	require.Zero(t, count)
	var done observed.Result
	require.NoError(t, d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		done, err = observed.Record(ctx, d.NewWithPgxTx(tx), b, w.clock.Now())
		return err
	}))
	require.True(t, done.Completed)
	require.Nil(t, done.Payment)
	require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT count(*) FROM billing.payments WHERE order_id=$1`), done.Order.ID).Scan(&count))
	require.Zero(t, count, "credit settlement must not manufacture a gateway capture")
	_, err = observed.Record(ctx, d, b, w.clock.Now())
	require.ErrorContains(t, err, "transaction-bound")
}
