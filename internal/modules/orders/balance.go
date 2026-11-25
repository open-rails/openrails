package orders

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
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/open-rails/openrails/internal/modules/money/books"
	"github.com/open-rails/openrails/internal/modules/money/credits"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// ErrBalanceShort refuses an order in a merchant's unit its customer's
// balance does not cover.
var ErrBalanceShort = apperr.New(http.StatusPaymentRequired, billing.CodeInsufficientCredits, "The customer's balance does not cover the order.")

// InMerchantUnit reports whether an order's currency is a merchant's own
// unit: its balance pays it, never a provider.
func InMerchantUnit(code string) bool { return currency.IsUnitCode(code) }

// payFromBalance pays a locked open order in a merchant's unit from its
// customer's balance in that unit, then settles it: each line draws the live
// lots, soonest expiry first, one credit_spend per lot at (order,
// order_line, line), naming the order, and its money moves into the line's
// deferred revenue at each lot's rate. Usage the customer accrued draws the
// balance first; what holds and pending refunds keep is not spent.
func (s *Service) payFromBalance(ctx context.Context, d *db.DB, order *Order) (Outcome, error) {
	q := d.Gen(ctx)
	now := s.now()
	lots := credits.New(q, order.MerchantID, s.now)
	if _, err := lots.ExpireLapsed(ctx, order.CustomerID, order.Currency); err != nil {
		return "", err
	}
	if _, err := lots.SettleRunningCharges(ctx, order.CustomerID, order.Currency, now); err != nil {
		return "", err
	}
	available, err := availableUnits(ctx, q, order, now)
	if err != nil {
		return "", err
	}
	if available < order.Total {
		return "", ErrBalanceShort
	}
	for _, line := range order.Lines {
		if line.Amount == 0 {
			continue
		}
		coord := ledger.Coord{Operation: ledger.OpOrder, Source: "order_line", SourceID: line.ID.String()}
		into := &books.Line{Order: order.ID, OrderLine: line.ID, Product: line.ProductID}
		if _, err := lots.SpendInto(ctx, order.CustomerID, order.Currency, line.Amount, order.CustomerID.String(), "order", coord, into); err != nil {
			if errors.Is(err, credits.ErrInsufficientCredits) {
				return "", ErrBalanceShort
			}
			return "", fmt.Errorf("pay order line %s from the balance: %w", line.ID, err)
		}
	}
	return s.settle(ctx, d, order, nil)
}

// availableUnits is the customer's balance in the order's unit less what
// admissions, usage not yet posted and pending refunds hold of it.
func availableUnits(ctx context.Context, q *gen.Queries, order *Order, now time.Time) (int64, error) {
	l := ledger.New(q, order.MerchantID)
	account, found, err := l.CustomerBalanceAccountID(ctx, order.CustomerID, order.Currency)
	if err != nil || !found {
		return 0, err
	}
	balance, err := l.Balance(ctx, account)
	if err != nil {
		return 0, err
	}
	held, err := q.GetFinancialHeldAmount(ctx, gen.GetFinancialHeldAmountParams{MerchantID: order.MerchantID, CustomerID: order.CustomerID, Currency: order.Currency,
		AsOf: now, LeaseLiveSince: limits.LeaseLiveSince(now)})
	if err != nil {
		return 0, err
	}
	return balance - held.Held, nil
}
