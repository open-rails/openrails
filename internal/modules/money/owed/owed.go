// Package owed repays a customer's debt from newly funded credit.
package owed

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/modules/orders/usage"
)

// Repay applies min(owed, funded) of a just-funded credit lot to what the
// customer owes, in the caller's transaction and under its customer lock:
// first to the usage orders that claim the debt, oldest first, then to debt
// no order claims yet. An order whose collection is in flight keeps its
// claim and its amount. Every leg is an owed_repayment transfer on the lot at
// the funding's coordinate, so the funding's own once-only deposit makes the
// repayment once-only too. It returns the amount repaid.
func Repay(ctx context.Context, d *db.DB, merchantID uuid.UUID, lot gen.BillingGrant, funded int64, now time.Time) (int64, error) {
	q := d.Gen(ctx)
	if lot.Currency == nil || funded <= 0 {
		return 0, nil
	}
	currency, customer := *lot.Currency, lot.CustomerID
	l := ledger.New(q, merchantID)
	owed, err := l.OutstandingOwed(ctx, customer, currency)
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
	remaining := min(funded, owed-inFlight)
	if remaining <= 0 {
		return 0, nil
	}
	repaid := int64(0)
	// The funding coordinate grants.MaterializeGrant deposits the lot at.
	coord := ledger.Coord{Operation: ledger.OpDeposit, Source: "grant", SourceID: lot.ID.String()}
	for _, c := range claims {
		if remaining == 0 {
			break
		}
		if c.Status == "processing" {
			continue
		}
		amount := min(remaining, c.AmountDue)
		order := c.ID
		legCoord := coord
		legCoord.SourceID = lot.ID.String() + ":" + order.String()
		// The repayment moves no money: its transfer is the order's record.
		if _, err := l.RepayOwed(ctx, customer, currency, amount, legCoord, lot.ID, &order); err != nil {
			return 0, err
		}
		if _, err := usage.Pay(ctx, d, merchantID, order, amount, nil, nil, now); err != nil {
			return 0, fmt.Errorf("owed repayment: order %s: %w", order, err)
		}
		remaining -= amount
		repaid += amount
	}
	if amount := min(remaining, max(owed-billed, 0)); amount > 0 {
		if _, err := l.RepayOwed(ctx, customer, currency, amount, coord, lot.ID, nil); err != nil {
			return 0, err
		}
		repaid += amount
	}
	return repaid, nil
}
