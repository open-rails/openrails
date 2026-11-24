package books

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// AccrueUsage posts usage a customer owes beyond its balance as revenue
// earned and receivable, DR usage_receivable / CR usage_revenue
// (usage_accrued), at the usage posting's coordinate, dated as its owed is.
// Owed in a merchant's own unit (#1379 refuses it) has no money and posts
// nothing. Credit repays it at face (RepayFromCredit); its collection,
// waiver and allowance are #1380 B5's. Its one caller is the usage posting
// (money.postPeriod); owed that running charges leave is B5's.
func (b *Books) AccrueUsage(ctx context.Context, customer uuid.UUID, currency string, amount int64, coord Coord, at time.Time, order *uuid.UUID) error {
	if amount <= 0 || CheckCurrency(currency) != nil {
		return nil
	}
	_, _, err := b.Post(ctx, Posting{
		Debit: Ref{Type: UsageReceivable, Customer: &customer}, Credit: Ref{Type: UsageRevenue},
		Amount: amount, Currency: currency, Type: UsageAccrued, Coord: coord, EffectiveAt: at,
		Customer: &customer, Order: order,
	})
	return err
}

// ReverseUsage posts a usage period whose price fell below what it posted,
// given back as a lot (usage_reversed, at the posting's coordinate): the
// revenue taken back, DR revenue_adjustments / CR deferred_revenue_credit for
// the lot's units at face, so the lot releases it as it is spent. l is the
// lot after the units book credited it. A lot in a merchant's own unit has
// no money and posts nothing.
func (b *Books) ReverseUsage(ctx context.Context, l CreditLot, coord Coord, at time.Time) error {
	if l.Units <= 0 || CheckCurrency(l.Unit) != nil {
		return nil
	}
	return b.postLot(ctx, l, Ref{Type: RevenueAdjustments}, l.deferredRef(), l.Units, l.Unit, UsageReversed, coord, at)
}
