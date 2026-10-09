// Package owed repays a customer's debt from newly funded credit.
package owed

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// balanceChannel is the invoice_payments channel of a payment from the
// customer's funded balance.
const balanceChannel = "balance"

// Repay applies min(owed, funded) of a just-funded credit lot to what the
// customer owes, in the caller's transaction and under its customer lock:
// first to the invoices that claim the debt, oldest first, as balance
// payments, then to debt no invoice claims yet. An invoice whose collection is
// in flight keeps its claim and its amount. Every leg is an owed_repayment
// transfer on the lot at the funding's coordinate, so the funding's own
// once-only deposit makes the repayment once-only too. It returns the amount
// repaid.
func Repay(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, lot gen.BillingGrant, funded int64, now time.Time) (int64, error) {
	if lot.Currency == nil || funded <= 0 {
		return 0, nil
	}
	currency, customer := *lot.Currency, lot.CustomerID
	l := ledger.New(q, merchantID)
	owed, err := l.OutstandingOwed(ctx, customer, currency)
	if err != nil || owed <= 0 {
		return 0, err
	}
	claims, err := q.ListOwedInvoiceClaims(ctx, gen.ListOwedInvoiceClaimsParams{MerchantID: merchantID, CustomerID: customer, Currency: currency})
	if err != nil {
		return 0, err
	}
	var invoiced, inFlight int64
	for _, c := range claims {
		invoiced += c.AmountDue
		if c.CollectionIntentID != nil {
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
		if c.CollectionIntentID != nil {
			continue
		}
		amount := min(remaining, c.AmountDue)
		invoice := c.ID
		legCoord := coord
		legCoord.SourceID = lot.ID.String() + ":" + invoice.String()
		tr, err := l.RepayOwed(ctx, customer, currency, amount, legCoord, lot.ID, &invoice)
		if err != nil {
			return 0, err
		}
		n, err := q.ApplyInvoiceBalancePayment(ctx, gen.ApplyInvoiceBalancePaymentParams{
			MerchantID: merchantID, CustomerID: customer, InvoiceID: invoice, Amount: amount, Now: now,
		})
		if err != nil {
			return 0, err
		}
		if n != 1 {
			return 0, fmt.Errorf("owed repayment: invoice %s changed under the customer lock", invoice)
		}
		if err := q.InsertInvoicePayment(ctx, gen.InsertInvoicePaymentParams{
			ID: uuidutil.NewV7(), MerchantID: merchantID, CustomerID: customer, InvoiceID: invoice,
			LedgerTransferID: &tr.ID, Currency: currency, Amount: amount, Status: "settled", Channel: balanceChannel,
			AttemptedAt: now, SettledAt: &now, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return 0, err
		}
		remaining -= amount
		repaid += amount
	}
	if amount := min(remaining, max(owed-invoiced, 0)); amount > 0 {
		if _, err := l.RepayOwed(ctx, customer, currency, amount, coord, lot.ID, nil); err != nil {
			return 0, err
		}
		repaid += amount
	}
	return repaid, nil
}
