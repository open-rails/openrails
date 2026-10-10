// Package statement keeps period statements on their invoices: a statement
// is paid once the invoices billing its charges are.
package statement

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
)

// Follow passes a payment to invoice on to the statements waiting on its
// charges, in the payment's transaction, right after the payment lands on the
// invoice: an invoice is always locked before its statements.
func Follow(ctx context.Context, q *gen.Queries, merchantID, customerID uuid.UUID, currency string, invoice uuid.UUID, now time.Time) error {
	waiting, err := q.LockStatementsAwaitingInvoice(ctx, gen.LockStatementsAwaitingInvoiceParams{
		MerchantID: merchantID, CustomerID: customerID, Currency: currency, InvoiceID: invoice,
	})
	if err != nil || len(waiting) == 0 {
		return err
	}
	_, err = q.SettleStatementCoverage(ctx, gen.SettleStatementCoverageParams{MerchantID: merchantID, StatementIds: waiting, Now: now})
	return err
}
