package money

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/shared/moneyutil"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ServiceUsageRollupRow is one grouped spend bucket: the dimension value, the
// number of usage events, and the summed host-priced amount.
type ServiceUsageRollupRow struct {
	Key         string `json:"key"`
	Currency    string `json:"currency"`
	EventCount  int64  `json:"event_count"`
	TotalAmount int64  `json:"total_amount"`
}

// serviceUsageGroupKeys is the allowlist of group_by selectors. "invoker" and
// "resource" read the typed attribution columns; the rest read the long-tail
// string dimensions stashed in metadata at capture time. The SQL-side mapping
// lives in the ServiceUsageRollup query's CASE expression.
var serviceUsageGroupKeys = map[string]bool{
	"resource": true,
	"invoker":  true,
	"function": true,
	"tier":     true,
}

// ServiceUsageRollup returns per-dimension-VALUE spend for a merchant subject over
// [from, to), grouped by group_by. Service-scoped (any payer), for the platform
// usage/revenue surfaces — NOT the hot admission path.
func (s *MoneyService) ServiceUsageRollup(ctx context.Context, payer identity.CustomerID, currency string, from, to time.Time, groupBy string) ([]ServiceUsageRollupRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	cur := normalizeCurrency(currency)
	if err := moneyutil.ValidateCurrency(cur); err != nil {
		return nil, err
	}
	groupBy = strings.TrimSpace(groupBy)
	if !serviceUsageGroupKeys[groupBy] {
		return nil, fmt.Errorf("invalid group_by %q (want resource|invoker|function|tier)", groupBy)
	}
	var out []ServiceUsageRollupRow
	err := s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		tid, terr := merchant.Require(ctx)
		if terr != nil {
			return terr
		}
		rows, err := s.db.Gen(ctx).ServiceUsageRollup(ctx, gen.ServiceUsageRollupParams{
			MerchantID: tid.UUID(),
			CustomerID: payer.UUID(),
			Currency:   cur,
			GroupBy:    groupBy,
			FromAt:     from.UTC(),
			ToAt:       to.UTC(),
		})
		if err != nil {
			return err
		}
		out = make([]ServiceUsageRollupRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, ServiceUsageRollupRow{Key: r.Key, Currency: r.Currency, EventCount: r.EventCount, TotalAmount: r.TotalAmount})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ResourceRevenueDailyRow is one day's revenue for a resource in internal units.
type ResourceRevenueDailyRow struct {
	Date     string `json:"date"`
	Currency string `json:"currency"`
	Amount   int64  `json:"amount"`
}

// ResourceRevenueDaily returns per-day revenue (sum of captured usage_event
// amounts; older rows used a USD-specific internal unit conversion) for
// a resource (typed attribution column), across ALL payers in the merchant, over
// [from, to). Powers endpoint revenue analytics (#410).
func (s *MoneyService) ResourceRevenueDaily(ctx context.Context, resource, currency string, from, to time.Time) ([]ResourceRevenueDailyRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	resource = strings.TrimSpace(resource)
	if resource == "" {
		return nil, fmt.Errorf("resource required")
	}
	cur := normalizeCurrency(currency)
	if err := moneyutil.ValidateCurrency(cur); err != nil {
		return nil, err
	}
	var out []ResourceRevenueDailyRow
	err := s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		tid, terr := merchant.Require(ctx)
		if terr != nil {
			return terr
		}
		rows, err := s.db.Gen(ctx).ResourceRevenueDaily(ctx, gen.ResourceRevenueDailyParams{
			MerchantID: tid.UUID(),
			Resource:   &resource,
			Currency:   cur,
			FromAt:     from.UTC(),
			ToAt:       to.UTC(),
		})
		if err != nil {
			return err
		}
		out = make([]ResourceRevenueDailyRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, ResourceRevenueDailyRow{Date: r.Date, Currency: r.Currency, Amount: r.Amount})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// nilIfEmpty returns nil for "", else a pointer to s (nullable text columns).
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
