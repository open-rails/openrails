// Package owed settles a customer's debt: from newly funded credit, and
// again from a usage order payment's won dispute.
package owed

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/money/books"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/modules/orders/usage"
)

// Repay applies min(owed, funded) of a just-funded credit lot to what the
// customer owes, in the caller's transaction and under its customer lock.
// Every leg is an owed_repayment transfer on the lot at the funding's
// coordinate, so the funding's own once-only deposit makes the repayment
// once-only too; the general ledger repays the receivable at face
// (books.RepayFromCredit). It returns the amount repaid.
func Repay(ctx context.Context, d *db.DB, merchantID uuid.UUID, lot gen.BillingCreditGrant, funded int64, now time.Time) (int64, error) {
	q := d.Gen(ctx)
	currency, customer := lot.Unit, lot.CustomerID
	l := ledger.New(q, merchantID)
	gl := books.New(q, merchantID)
	// The funding coordinate credits.Lots.Materialize deposits the lot at.
	coord := ledger.Coord{Operation: ledger.OpDeposit, Source: "grant", SourceID: lot.ID.String()}
	return settle(ctx, d, merchantID, customer, currency, funded, nil, now, func(amount int64, order *uuid.UUID) error {
		legCoord := coord
		if order != nil {
			legCoord.SourceID = lot.ID.String() + ":" + order.String()
		}
		// The repayment moves no money: its transfer is the order's record.
		leg, err := l.RepayOwed(ctx, customer, currency, amount, legCoord, lot.ID, order)
		if err != nil {
			return err
		}
		current, err := q.GetCreditLot(ctx, gen.GetCreditLotParams{MerchantID: merchantID, ID: lot.ID})
		if err != nil {
			return err
		}
		return gl.RepayFromCredit(ctx, books.CreditLotOf(current), leg.Amount,
			books.Coord{Operation: books.Operation(leg.Operation), Source: leg.Source, SourceID: leg.SourceID}, leg.EffectiveAt)
	})
}

// settle applies up to amount to what the customer owes: first to the usage
// orders that claim the debt, oldest first, then to debt no order claims
// yet. An order whose collection is in flight keeps its claim and its
// amount. leg posts each part, naming the order it pays (nil for debt no
// order claims); payment is what paid a completed order. It returns the
// amount applied.
func settle(ctx context.Context, d *db.DB, merchantID, customer uuid.UUID, currency string, amount int64, payment *uuid.UUID, now time.Time,
	leg func(amount int64, order *uuid.UUID) error) (int64, error) {
	if amount <= 0 {
		return 0, nil
	}
	q := d.Gen(ctx)
	owed, err := ledger.New(q, merchantID).OutstandingOwed(ctx, customer, currency)
	if err != nil || owed <= 0 {
		return 0, err
	}
	claims, err := q.ListOwedOrderClaims(ctx, gen.ListOwedOrderClaimsParams{MerchantID: merchantID, CustomerID: customer, Currency: currency})
	if err != nil {
		return 0, err
	}
	var billed, inFlight int64
	for _, c := range claims {
		billed += c.AmountDue
		if c.Status == "processing" {
			inFlight += c.AmountDue
		}
	}
	remaining := min(amount, owed-inFlight)
	if remaining <= 0 {
		return 0, nil
	}
	applied := int64(0)
	for _, c := range claims {
		if remaining == 0 {
			break
		}
		if c.Status == "processing" {
			continue
		}
		part := min(remaining, c.AmountDue)
		order := c.ID
		if err := leg(part, &order); err != nil {
			return 0, err
		}
		if _, err := usage.Pay(ctx, d, merchantID, order, part, payment, nil, now); err != nil {
			return 0, fmt.Errorf("owed settlement: order %s: %w", order, err)
		}
		remaining -= part
		applied += part
	}
	if part := min(remaining, max(owed-billed, 0)); part > 0 {
		if err := leg(part, nil); err != nil {
			return 0, err
		}
		applied += part
	}
	return applied, nil
}
