package owed

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/money/books"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// SyncUsageReversals books, once each and in the order they were stored, the
// chargebacks and won disputes of a usage order payment, in the caller's
// transaction. A chargeback reopens the owed the payment paid: the usage was
// delivered and the network took the cash back, so the debt stands again,
// owed again in the units book (owed_reopened) and billed on the customer's
// next usage order, and receivable again in the general ledger. Its won
// dispute settles the reopened owed again, as funding would: the orders that
// claim the customer's debt first. A merchant's refund reopens nothing: the
// refund is the usage's contra revenue. A payment that settled no owed (not a
// usage order's) has nothing to sync.
func SyncUsageReversals(ctx context.Context, d *db.DB, merchantID, paymentID uuid.UUID, now time.Time) error {
	q := d.Gen(ctx)
	p, err := q.GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: merchantID, ID: paymentID})
	if err != nil || p.LedgerTransferID == nil || p.RefundedPaymentID != nil || books.CheckCurrency(p.Currency) != nil {
		return err
	}
	paid, err := q.GetUsageRecoveryLedgerTransfer(ctx, gen.GetUsageRecoveryLedgerTransferParams{MerchantID: merchantID, TransferID: *p.LedgerTransferID})
	if err != nil || paid.TransferType != string(ledger.OwedPayment) {
		return err
	}
	reversals, err := q.ListUsagePaymentReversals(ctx, gen.ListUsagePaymentReversalsParams{MerchantID: merchantID, PaymentID: p.ID})
	if err != nil {
		return err
	}
	l, gl := ledger.New(q, merchantID), books.New(q, merchantID)
	source := "usage_payment:" + p.ID.String()
	for _, r := range reversals {
		if posted, err := q.PaymentReversalPosted(ctx, gen.PaymentReversalPostedParams{MerchantID: merchantID, ReversalID: r.ID.String()}); err != nil || posted {
			if err != nil {
				return err
			}
			continue
		}
		state, err := q.GetUsagePaymentReopened(ctx, gen.GetUsagePaymentReopenedParams{MerchantID: merchantID, Source: source})
		if err != nil {
			return err
		}
		reopened := state.Reopened - state.Resettled
		switch {
		case r.ReversalKind != nil && *r.ReversalKind == "chargeback" && r.Amount < 0:
			if err := collectBeforeReopening(ctx, q, gl, p, paid.Amount); err != nil {
				return err
			}
			owed := min(-r.Amount, max(paid.Amount-reopened, 0))
			if owed > 0 {
				if _, err := l.ReopenOwed(ctx, p.CustomerID, p.Currency, owed, ledger.Coord{Operation: books.OpChargeback, Source: source, SourceID: r.ID.String()}, p.OrderID); err != nil {
					return err
				}
				meta, err := json.Marshal(map[string]any{"source": "chargeback", "payment_id": p.ID.String(), "chargeback_id": r.ID.String()})
				if err != nil {
					return err
				}
				if err := q.InsertPendingOwedItem(ctx, gen.InsertPendingOwedItemParams{ID: uuidutil.NewV7(), MerchantID: merchantID, CustomerID: p.CustomerID,
					Currency: p.Currency, SourceType: string(ledger.OwedReopened), SourceID: r.ID.String(), AccruedAt: now, Amount: owed, Metadata: meta,
					CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
			}
			if err := gl.ReopenUsage(ctx, books.UsageReversal{Reversal: r, Original: p, Owed: owed}); err != nil {
				return err
			}
		case r.ReversalKind != nil && *r.ReversalKind == "dispute_reversal" && r.Amount > 0:
			recovery := r.ID
			settled, err := settle(ctx, d, merchantID, p.CustomerID, p.Currency, min(r.Amount, reopened), &recovery, now, func(amount int64, order *uuid.UUID) error {
				coord := ledger.Coord{Operation: books.OpDisputeWonReopened, Source: source, SourceID: r.ID.String()}
				if order != nil {
					coord.SourceID += ":" + order.String()
				}
				_, err := l.PayOwed(ctx, p.CustomerID, p.Currency, amount, coord, order)
				return err
			})
			if err != nil {
				return err
			}
			if err := gl.ResettleUsage(ctx, books.UsageReversal{Reversal: r, Original: p, Owed: settled}); err != nil {
				return err
			}
		}
	}
	return nil
}

// collectBeforeReopening books the collection of a payment whose cash the
// general ledger received but applied to nothing (collected before the
// receivable's postings existed), so its chargeback reopens what it
// collected.
func collectBeforeReopening(ctx context.Context, q *gen.Queries, gl *books.Books, p gen.BillingPayment, owed int64) error {
	merchantID := p.MerchantID
	posted, err := q.UsageCollectionPosted(ctx, gen.UsageCollectionPostedParams{MerchantID: merchantID, PaymentID: p.ID.String()})
	if err != nil || posted {
		return err
	}
	cash, err := q.GetPaymentCashApplication(ctx, gen.GetPaymentCashApplicationParams{MerchantID: merchantID, PaymentID: p.ID.String()})
	if err != nil || cash.Received <= 0 {
		return err
	}
	return gl.CollectUsage(ctx, books.UsageCollection{Payment: p.ID, Customer: p.CustomerID, Order: p.OrderID, Currency: p.Currency,
		Amount: min(owed, cash.Received-cash.Applied), At: p.PurchasedAt})
}
