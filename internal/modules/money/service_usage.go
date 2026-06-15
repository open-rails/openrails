package money

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// CaptureUsageEventParams records an analytics usage_event linked to an EXISTING
// capture credit_transaction WITHOUT a second ledger debit (the capture already
// debited the ledger). This is distinct from RecordUsage, which debits. It powers
// the service usage rollup (#311) so the tensorhub platform's /budget-usage +
// revenue analytics can be served from OpenRails as the billing source of truth.
type CaptureUsageEventParams struct {
	CustomerID uuid.UUID
	// Invoker is the caller-supplied principal string attributable to this usage
	// (opaque to OpenRails). Required.
	Invoker   string
	Currency  string
	EventType string // the metered event kind, e.g. "owner/endpoint"
	Amount    int64  // host-priced captured amount (>= 0)
	// Resource is the caller-supplied free-form string for what was metered
	// (opaque to OpenRails; e.g. tensorhub maps its endpoint slug here). Optional.
	Resource           string
	Dimensions         map[string]int64
	Metadata           map[string]any // string long-tail dims (function_name, tier, ...)
	Source             string
	SourceID           string
	MoneyTransactionID *uuid.UUID
}

// InsertCaptureUsageEvent appends a usage_event for analytics, idempotent on
// (tenant, payer, event_type, source, source_id). No ledger debit happens here.
func (s *MoneyService) InsertCaptureUsageEvent(ctx context.Context, p CaptureUsageEventParams) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	p.EventType = strings.TrimSpace(p.EventType)
	p.Source = strings.TrimSpace(p.Source)
	p.SourceID = strings.TrimSpace(p.SourceID)
	p.Invoker = strings.TrimSpace(p.Invoker)
	p.Resource = strings.TrimSpace(p.Resource)
	if p.EventType == "" || p.Source == "" || p.SourceID == "" {
		return fmt.Errorf("event_type, source, source_id required for usage event")
	}
	if p.Invoker == "" {
		return fmt.Errorf("invoker required for usage event")
	}
	if p.Amount < 0 {
		return fmt.Errorf("amount must be >= 0")
	}
	cur := normalizeCurrency(p.Currency)
	if err := ValidateCurrency(cur); err != nil {
		return err
	}
	now := s.now()
	return s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		q := s.db.Gen(ctx)
		tid, terr := merchant.Require(ctx)
		if terr != nil {
			return terr
		}
		if err := ensureCustomer(ctx, q, tid.UUID(), p.CustomerID); err != nil {
			return err
		}
		ev := &models.UsageEvent{
			ID:                 uuidutil.NewV7(),
			MerchantID:         tid.UUID(),
			CustomerID:         p.CustomerID,
			Invoker:            p.Invoker,
			Currency:           cur,
			Resource:           nilIfEmpty(p.Resource),
			EventType:          p.EventType,
			Dimensions:         p.Dimensions,
			Amount:             p.Amount,
			Source:             p.Source,
			SourceID:           p.SourceID,
			MoneyTransactionID: p.MoneyTransactionID,
			Metadata:           p.Metadata,
			OccurredAt:         now,
			CreatedAt:          now,
		}
		dims, err := toJSONBC(ev.Dimensions)
		if err != nil {
			return err
		}
		meta, err := toJSONBC(ev.Metadata)
		if err != nil {
			return err
		}
		return q.InsertUsageEventIfAbsent(ctx, gen.InsertUsageEventIfAbsentParams{
			ID:                 ev.ID,
			MerchantID:         ev.MerchantID,
			CustomerID:         ev.CustomerID,
			InvokerID:          ev.Invoker,
			Currency:           ev.Currency,
			Resource:           ev.Resource,
			EventType:          ev.EventType,
			Dimensions:         dims,
			Amount:             ev.Amount,
			Source:             ev.Source,
			SourceID:           ev.SourceID,
			MoneyTransactionID: ev.MoneyTransactionID,
			Metadata:           meta,
			OccurredAt:         ev.OccurredAt,
			CreatedAt:          ev.CreatedAt,
		})
	})
}

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

// ServiceUsageRollup returns per-dimension-VALUE spend for a tenant subject over
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
	if err := ValidateCurrency(cur); err != nil {
		return nil, err
	}
	groupBy = strings.TrimSpace(groupBy)
	if !serviceUsageGroupKeys[groupBy] {
		return nil, fmt.Errorf("invalid group_by %q (want resource|invoker|function|tier)", groupBy)
	}
	var out []ServiceUsageRollupRow
	err := s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
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
// a resource (typed attribution column), across ALL payers in the tenant, over
// [from, to). Powers e.g. tensorhub endpoint revenue analytics (#410).
func (s *MoneyService) ResourceRevenueDaily(ctx context.Context, resource, currency string, from, to time.Time) ([]ResourceRevenueDailyRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	resource = strings.TrimSpace(resource)
	if resource == "" {
		return nil, fmt.Errorf("resource required")
	}
	cur := normalizeCurrency(currency)
	if err := ValidateCurrency(cur); err != nil {
		return nil, err
	}
	var out []ResourceRevenueDailyRow
	err := s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
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
