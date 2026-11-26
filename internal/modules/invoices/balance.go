package invoices

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/currency"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/money/books"
	"github.com/open-rails/openrails/internal/modules/money/credits"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// ErrBalanceShort refuses an invoice in a merchant's unit its customer's
// balance does not cover.
var ErrBalanceShort = apperr.New(http.StatusPaymentRequired, billing.CodeInsufficientCredits, "The customer's balance does not cover the invoice.")

// InMerchantUnit reports whether an invoice's currency is a merchant's own
// unit: its balance pays it, never a provider.
func InMerchantUnit(code string) bool { return currency.IsUnitCode(code) }

// payFromBalance pays a locked open invoice in a merchant's unit from its
// customer's balance in that unit, then settles it: each line draws the live
// lots, soonest expiry first, one credit_spend per lot at (invoice,
// invoice_line, line), naming the invoice, and its money moves into the line's
// deferred revenue at each lot's rate. Pending usage and running charges'
// accrual are not spent: they post later from what is left.
func (s *Service) payFromBalance(ctx context.Context, d *db.DB, invoice *Invoice) (Outcome, error) {
	q := d.Gen(ctx)
	now := s.now()
	lots := credits.New(q, invoice.MerchantID, s.now)
	if _, err := lots.ExpireLapsed(ctx, invoice.CustomerID, invoice.Currency); err != nil {
		return "", err
	}
	available, err := availableUnits(ctx, q, invoice, now)
	if err != nil {
		return "", err
	}
	if available < invoice.Total {
		return "", ErrBalanceShort
	}
	for _, line := range invoice.Lines {
		if line.Amount == 0 {
			continue
		}
		coord := ledger.Coord{Operation: ledger.OpInvoice, Source: "order_line", SourceID: line.ID.String()}
		into := &books.Line{Invoice: invoice.ID, InvoiceLine: line.ID, Product: line.ProductID}
		if _, err := lots.SpendInto(ctx, invoice.CustomerID, invoice.Currency, line.Amount, invoice.CustomerID.String(), "order", coord, into); err != nil {
			if errors.Is(err, credits.ErrInsufficientCredits) {
				return "", ErrBalanceShort
			}
			return "", fmt.Errorf("pay invoice line %s from the balance: %w", line.ID, err)
		}
	}
	return s.settle(ctx, d, invoice, nil)
}

// availableUnits is the customer's balance in the invoice's unit less its
// pending usage and running charges' accrual.
func availableUnits(ctx context.Context, q *gen.Queries, invoice *Invoice, now time.Time) (int64, error) {
	l := ledger.New(q, invoice.MerchantID)
	account, found, err := l.CustomerBalanceAccountID(ctx, invoice.CustomerID, invoice.Currency)
	if err != nil || !found {
		return 0, err
	}
	balance, err := l.Balance(ctx, account)
	if err != nil {
		return 0, err
	}
	unposted, err := q.GetUnpostedAmounts(ctx, gen.GetUnpostedAmountsParams{MerchantID: invoice.MerchantID, CustomerID: invoice.CustomerID, Currency: invoice.Currency, AsOf: now})
	if err != nil {
		return 0, err
	}
	return balance - unposted.Pending - unposted.Accrued, nil
}
