package money

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
)

// PricingPeriod is the billing period instant at falls in, as usage orders
// close it, read without writing. A currency's period is its usage-billing
// anniversary, or the merchant's boundary anchored on the customer's first
// activity in it; with no currency, or no activity yet, the boundary is
// anchored on the customer's creation.
func (s *MoneyService) PricingPeriod(ctx context.Context, q *gen.Queries, merchantID, customerID uuid.UUID, currency string, at time.Time) (time.Time, time.Time, error) {
	at = at.UTC()
	if currency != "" {
		cfg, cur, err := s.usageBillingConfig(ctx, currency)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		if cur != nil {
			// The anchor ensureUsageBillingAccount would give a new account.
			anchor := time.Date(2000, time.January, cfg.BillingDay, 0, 0, 0, 0, time.UTC)
			row, err := q.GetMoneyAccountSettings(ctx, gen.GetMoneyAccountSettingsParams{MerchantID: merchantID, CustomerID: customerID, Currency: currency})
			switch {
			case err == nil && row.UsageBillingAnchor != nil:
				anchor = *row.UsageBillingAnchor
			case err != nil && !errors.Is(err, pgx.ErrNoRows):
				return time.Time{}, time.Time{}, err
			}
			start, err := CurrentBillingPeriodStart(at, anchor, BillingBoundaryAnniversary)
			return start, NextUsageBillingDate(at, anchor), err
		}
	}
	boundary, err := s.billingPeriodBoundary(ctx)
	if err != nil && !errors.Is(err, merchantconfig.ErrNoSettings) {
		return time.Time{}, time.Time{}, err
	}
	var anchor time.Time
	if boundary == BillingBoundaryAnniversary {
		if anchor, err = s.periodAnchor(ctx, q, merchantID, customerID, currency); err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	start, err := CurrentBillingPeriodStart(at, anchor, boundary)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	switch boundary {
	case BillingBoundaryCalendarMonth:
		return start, start.AddDate(0, 1, 0), nil
	case BillingBoundaryAnniversary:
		if anchor.IsZero() {
			anchor = time.Unix(0, 0).UTC()
		}
		return start, NextUsageBillingDate(at, anchor), nil
	case BillingBoundaryFixedInterval:
		return start, start.Add(fixedBillingInterval), nil
	}
	return time.Time{}, time.Time{}, fmt.Errorf("invalid billing_period_boundary %q", boundary)
}

// periodAnchor is what an anniversary period is anchored on: the customer's
// first activity in the currency, else its creation.
func (s *MoneyService) periodAnchor(ctx context.Context, q *gen.Queries, merchantID, customerID uuid.UUID, currency string) (time.Time, error) {
	if currency != "" {
		_, err := q.GetLedgerAccount(ctx, gen.GetLedgerAccountParams{MerchantID: merchantID, AccountType: string(ledger.CustomerUnits), Currency: currency, CustomerID: &customerID})
		switch {
		case err == nil:
			return q.GetCustomerPeriodAnchor(ctx, gen.GetCustomerPeriodAnchorParams{MerchantID: merchantID, CustomerID: customerID, Currency: currency})
		case !errors.Is(err, pgx.ErrNoRows):
			return time.Time{}, err
		}
	}
	customer, err := q.GetCustomer(ctx, gen.GetCustomerParams{MerchantID: merchantID, ID: customerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	return customer.CreatedAt, err
}
