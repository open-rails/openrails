package credits

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/tenant"
)

// CaptureUsageEventParams records an analytics usage_event linked to an EXISTING
// capture credit_transaction WITHOUT a second ledger debit (the capture already
// debited the ledger). This is distinct from RecordUsage, which debits. It powers
// the service usage rollup (#311) so the tensorhub platform's /budget-usage +
// revenue analytics can be served from OpenRails as the billing source of truth.
type CaptureUsageEventParams struct {
	TenantSubjectID uuid.UUID
	// Actor is the caller-supplied principal string attributable to this usage
	// (opaque to OpenRails). Required.
	Actor        string
	CreditTypeID uuid.UUID
	EventType    string // the metered event kind, e.g. "owner/endpoint"
	Amount       int64  // host-priced captured amount (>= 0)
	// Resource is the caller-supplied free-form string for what was metered
	// (opaque to OpenRails; e.g. tensorhub maps its endpoint slug here). Optional.
	Resource            string
	Dimensions          map[string]int64
	Metadata            map[string]any // string long-tail dims (function_name, tier, ...)
	Source              string
	SourceID            string
	CreditTransactionID *uuid.UUID
}

// InsertCaptureUsageEvent appends a usage_event for analytics, idempotent on
// (tenant, payer, event_type, source, source_id). No ledger debit happens here.
func (s *CreditsService) InsertCaptureUsageEvent(ctx context.Context, p CaptureUsageEventParams) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("credits service not initialized")
	}
	p.EventType = strings.TrimSpace(p.EventType)
	p.Source = strings.TrimSpace(p.Source)
	p.SourceID = strings.TrimSpace(p.SourceID)
	p.Actor = strings.TrimSpace(p.Actor)
	p.Resource = strings.TrimSpace(p.Resource)
	if p.EventType == "" || p.Source == "" || p.SourceID == "" {
		return fmt.Errorf("event_type, source, source_id required for usage event")
	}
	if p.Actor == "" {
		return fmt.Errorf("actor required for usage event")
	}
	if p.Amount < 0 {
		return fmt.Errorf("amount must be >= 0")
	}
	now := s.now()
	return s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		if err := ensureTenantSubject(ctx, s.db.Q(ctx), tenant.FromContextOrDefault(ctx).UUID(), p.TenantSubjectID); err != nil {
			return err
		}
		ev := &models.UsageEvent{
			ID:                  uuidutil.NewV7(),
			TenantID:            tenant.FromContextOrDefault(ctx).UUID(),
			TenantSubjectID:     p.TenantSubjectID,
			Actor:               p.Actor,
			Resource:            nilIfEmpty(p.Resource),
			CreditTypeID:        p.CreditTypeID,
			EventType:           p.EventType,
			Dimensions:          p.Dimensions,
			Amount:              p.Amount,
			Source:              p.Source,
			SourceID:            p.SourceID,
			CreditTransactionID: p.CreditTransactionID,
			Metadata:            p.Metadata,
			OccurredAt:          now,
			CreatedAt:           now,
		}
		_, err := s.db.Q(ctx).NewInsert().Model(ev).
			On("CONFLICT (tenant_id, tenant_subject_id, event_type, source, source_id) DO NOTHING").
			Exec(ctx)
		return err
	})
}

// ServiceUsageRollupRow is one grouped spend bucket: the dimension value, the
// number of usage events, and the summed host-priced amount.
type ServiceUsageRollupRow struct {
	Key         string `json:"key"`
	EventCount  int64  `json:"event_count" bun:"event_count"`
	TotalAmount int64  `json:"total_amount" bun:"total_amount"`
}

// serviceUsageGroupExpr maps a group_by selector to the SQL key expression.
// "actor" and "resource" read the typed attribution columns; the rest read the
// long-tail string dimensions stashed in metadata at capture time.
var serviceUsageGroupExpr = map[string]string{
	"resource": "ue.resource",
	"actor":    "ue.actor",
	"function": "ue.metadata->>'function_name'",
	"tier":     "ue.metadata->>'availability_tier'",
}

// ServiceUsageRollup returns per-dimension-VALUE spend for a tenant subject over
// [from, to), grouped by group_by. Service-scoped (any payer), for the platform
// usage/revenue surfaces — NOT the hot admission path.
func (s *CreditsService) ServiceUsageRollup(ctx context.Context, payer identity.TenantSubjectID, from, to time.Time, groupBy string) ([]ServiceUsageRollupRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	keyExpr, ok := serviceUsageGroupExpr[strings.TrimSpace(groupBy)]
	if !ok {
		return nil, fmt.Errorf("invalid group_by %q (want resource|actor|function|tier)", groupBy)
	}
	var out []ServiceUsageRollupRow
	err := s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		return s.db.Q(ctx).NewSelect().
			TableExpr("billing.usage_events AS ue").
			ColumnExpr("COALESCE("+keyExpr+", '') AS key").
			ColumnExpr("COUNT(*) AS event_count").
			ColumnExpr("COALESCE(SUM(ue.amount), 0) AS total_amount").
			Where("ue.tenant_id = ?", tenant.FromContextOrDefault(ctx).UUID()).
			Where("ue.tenant_subject_id = ?", payer.UUID()).
			Where("ue.occurred_at >= ? AND ue.occurred_at < ?", from.UTC(), to.UTC()).
			GroupExpr("COALESCE("+keyExpr+", '')").
			OrderExpr("total_amount DESC").
			Scan(ctx, &out)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ResourceRevenueDailyRow is one day's revenue for a resource (millicents).
type ResourceRevenueDailyRow struct {
	Date             string `json:"date" bun:"date"`
	AmountMillicents int64  `json:"amount_millicents" bun:"amount_millicents"`
}

// ResourceRevenueDaily returns per-day revenue (sum of captured usage_event
// amounts; the ledger unit is micro-dollars, so /10 -> millicents) for
// a resource (typed attribution column), across ALL payers in the tenant, over
// [from, to). Powers e.g. tensorhub endpoint revenue analytics (#410).
func (s *CreditsService) ResourceRevenueDaily(ctx context.Context, resource string, from, to time.Time) ([]ResourceRevenueDailyRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
	}
	resource = strings.TrimSpace(resource)
	if resource == "" {
		return nil, fmt.Errorf("resource required")
	}
	var out []ResourceRevenueDailyRow
	err := s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		return s.db.Q(ctx).NewSelect().
			TableExpr("billing.usage_events AS ue").
			ColumnExpr("to_char(date_trunc('day', ue.occurred_at AT TIME ZONE 'UTC'), 'YYYY-MM-DD') AS date").
			ColumnExpr("(COALESCE(SUM(ue.amount), 0) + 9) / 10 AS amount_millicents").
			Where("ue.tenant_id = ?", tenant.FromContextOrDefault(ctx).UUID()).
			Where("ue.resource = ?", resource).
			Where("ue.occurred_at >= ? AND ue.occurred_at < ?", from.UTC(), to.UTC()).
			GroupExpr("1").
			OrderExpr("1").
			Scan(ctx, &out)
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
