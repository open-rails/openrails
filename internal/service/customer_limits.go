package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// setLimitOverrides writes a customer's validated overrides; each names a
// declared limit.
func setLimitOverrides(ctx context.Context, q *gen.Queries, merchantID, customerID uuid.UUID, overrides []billing.LimitOverride) error {
	for j, o := range overrides {
		if _, err := limits.Get(ctx, q, merchantID, o.Limit); err != nil {
			var refusal *apperr.Error
			if errors.As(err, &refusal) {
				return refusal.WithParam(fmt.Sprintf("limits[%d].limit", j))
			}
			return err
		}
	}
	return limits.SetOverrides(ctx, q, merchantID, customerID, overrides)
}

// limitReader reads one customer's limits on q at now.
func (s *Service) limitReader(q *gen.Queries, merchantID, customerID uuid.UUID, now time.Time) limits.Customer {
	return limits.Customer{Q: q, MerchantID: merchantID, CustomerID: customerID, Now: now,
		Period: func(ctx context.Context, unit string, at time.Time) (time.Time, time.Time, error) {
			return s.moneyService().PricingPeriod(ctx, q, merchantID, customerID, unit, at)
		}}
}

// ListCustomerLimits is where the customer stands on every declared limit,
// by key, in one page.
func (s *Service) ListCustomerLimits(ctx context.Context, customer identity.CustomerID) (billing.ListPage[billing.CustomerLimit], error) {
	var out billing.ListPage[billing.CustomerLimit]
	if customer.IsZero() {
		return out, apperr.Invalidf("customer_id is required").WithParam("customer_id")
	}
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return out, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return out, err
	}
	database := s.rt.DB
	err = database.RunInMerchantConn(ctx, func(ctx context.Context) error {
		items, err := s.limitReader(database.Gen(ctx), mid.UUID(), customer.UUID(), s.now()).List(ctx)
		out.Items = items
		return err
	})
	return out, err
}

// AdjustCustomerLimit adds to the capacity of the customer's current window
// of one limit and answers where the customer stands on it after.
func (s *Service) AdjustCustomerLimit(ctx context.Context, customer identity.CustomerID, params billing.AdjustLimitParams) (*billing.CustomerLimit, error) {
	if customer.IsZero() {
		return nil, apperr.Invalidf("customer_id is required").WithParam("customer_id")
	}
	key := strings.TrimSpace(params.Limit)
	if key == "" {
		return nil, apperr.Invalidf("limit is required").WithParam("limit")
	}
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var out billing.CustomerLimit
	err = s.rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		def, err := limits.Get(ctx, q, mid.UUID(), key)
		if err != nil {
			var refusal *apperr.Error
			if errors.As(err, &refusal) {
				return refusal.WithParam("limit")
			}
			return err
		}
		if err := db.EnsureCustomerRowQ(ctx, q, mid.UUID(), customer.UUID()); err != nil {
			return err
		}
		// The spend lock serializes the window a first adjustment opens.
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{ID: customer.UUID(), MerchantID: mid.UUID()}); err != nil {
			return err
		}
		out, err = s.limitReader(q, mid.UUID(), customer.UUID(), s.now()).Adjust(ctx, def, params.Amount)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
