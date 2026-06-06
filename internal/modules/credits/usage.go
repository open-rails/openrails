package credits

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/tenant"
)

// RecordUsageParams is one metered, host-priced usage event (issue #289). The
// host supplies the final Amount (in the credit type's smallest unit); OpenRails
// records the event AND debits the ledger atomically.
type RecordUsageParams struct {
	// Payer is the payer org BILLED for this usage (the payer). When nil it is
	// resolved from InvokerID (self-hosted/personal case), never synthesized.
	Payer      *identity.TenantSubjectID
	InvokerID  string // invoker (attribution only)
	CreditType string
	EventType  string // metered endpoint / model, e.g. "gpt-4o"
	// Dimensions are per-dimension counts (input_tokens, output_tokens,
	// cached_input_tokens, requests, ...). Used for reporting + #298 throughput.
	Dimensions map[string]int64
	// Amount is the host-priced cost (>= 0). 0 records a free/zero-cost event
	// (still metered for dimensions) without a ledger debit.
	Amount   int64
	Source   string // idempotency: namespace
	SourceID string // idempotency: typically the request id
	Metadata map[string]any
	// OccurredAt defaults to now when zero.
	OccurredAt time.Time
}

// RecordUsage durably records a metered usage event AND debits the credit ledger
// in ONE transaction (issue #289). Idempotent on
// (tenant, payer, event_type, source, source_id): a replayed request returns the
// existing event and never double-charges. Concurrency-safe: the balance row is
// locked FOR UPDATE before the idempotency check, so two concurrent identical
// records serialize and the second sees the first's event.
//
// This is the DURABLE side. In FAST mode (#298) it runs write-behind, off the
// per-request hot path; the synchronous admission decision is the Redis headroom
// op, not this write.
func (s *CreditsService) RecordUsage(ctx context.Context, params RecordUsageParams) (*models.UsageEvent, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
	}
	params.EventType = strings.TrimSpace(params.EventType)
	params.Source = strings.TrimSpace(params.Source)
	params.SourceID = strings.TrimSpace(params.SourceID)
	if params.EventType == "" {
		return nil, fmt.Errorf("event_type required")
	}
	if params.Source == "" || params.SourceID == "" {
		return nil, fmt.Errorf("source and source_id required for usage idempotency")
	}
	if params.Amount < 0 {
		return nil, fmt.Errorf("amount must be >= 0")
	}
	ct, err := s.GetCreditTypeByName(ctx, params.CreditType)
	if err != nil {
		return nil, err
	}
	if !ct.IsActive {
		return nil, ErrCreditTypeInactive
	}
	payer, err := resolvePayer(params.Payer, params.InvokerID)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTenantTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	ownerID := payer.UUID()
	now := s.now()

	// Serialize per (tenant, payer, credit_type) so the idempotency check below
	// can't race a concurrent identical record into a double charge.
	if _, err := s.lockBalance(ctx, tx, payer, params.InvokerID, ct.ID); err != nil {
		return nil, err
	}

	existing := new(models.UsageEvent)
	err = tx.NewSelect().Model(existing).
		Where("tenant_id = ? AND tenant_subject_id = ?", tenantID, ownerID).
		Where("event_type = ?", params.EventType).
		Where("source = ? AND source_id = ?", params.Source, params.SourceID).
		Limit(1).
		Scan(ctx)
	if err == nil {
		return existing, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	// Debit the ledger for the host-priced amount (skip for zero-cost events).
	// Unified credit line (#302): draw prepaid balance first, then accrue to
	// owed up to the credit line. Prepay-only accounts (no line) deny when the
	// amount exceeds available balance.
	var debitID *uuid.UUID
	if params.Amount > 0 {
		balID, owedID, derr := s.spendBalanceThenOwedTx(ctx, tx, ct, payer, params.InvokerID, params.Source, params.SourceID, params.Amount)
		if derr != nil {
			return nil, derr
		}
		if balID != nil {
			debitID = balID
		} else {
			debitID = owedID
		}
	}

	occurred := params.OccurredAt.UTC()
	if params.OccurredAt.IsZero() {
		occurred = now
	}
	ev := &models.UsageEvent{
		ID:                  uuidutil.NewV7(),
		TenantID:            tenantID,
		TenantSubjectID:     ownerID,
		InvokerID:           params.InvokerID,
		CreditTypeID:        ct.ID,
		EventType:           params.EventType,
		Dimensions:          params.Dimensions,
		Amount:              params.Amount,
		Source:              params.Source,
		SourceID:            params.SourceID,
		CreditTransactionID: debitID,
		Metadata:            params.Metadata,
		OccurredAt:          occurred,
		CreatedAt:           now,
	}
	if _, err := tx.NewInsert().Model(ev).Exec(ctx); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ev, nil
}

// UsageRollupRow is a per-event_type aggregate of usage over a window: total
// host-priced amount, event count, and summed per-dimension counts. Powers
// usage reporting (GET /v1/me/usage) and #303 invoice line items.
type UsageRollupRow struct {
	EventType   string           `json:"event_type"`
	TotalAmount int64            `json:"total_amount"`
	EventCount  int64            `json:"event_count"`
	Dimensions  map[string]int64 `json:"dimensions"`
}

// AggregateUsage rolls up an payer's usage_events over [from, to) grouped by
// event_type, with summed dimensions. RLS-scoped to the request tenant. This is
// the rollup layer — it is NEVER called on the per-request admission hot path.
func (s *CreditsService) AggregateUsage(ctx context.Context, payer identity.TenantSubjectID, from, to time.Time) ([]UsageRollupRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	ownerID := payer.UUID()

	type totalRow struct {
		EventType   string `bun:"event_type"`
		TotalAmount int64  `bun:"total_amount"`
		EventCount  int64  `bun:"event_count"`
	}
	type dimRow struct {
		EventType string `bun:"event_type"`
		Key       string `bun:"key"`
		Total     int64  `bun:"total"`
	}

	rows := map[string]*UsageRollupRow{}
	err := s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		var totals []totalRow
		if err := s.db.Q(ctx).NewSelect().
			Model((*models.UsageEvent)(nil)).
			ColumnExpr("event_type").
			ColumnExpr("COALESCE(SUM(amount), 0) AS total_amount").
			ColumnExpr("COUNT(*) AS event_count").
			Where("tenant_id = ?", tenantID).
			Where("tenant_subject_id = ?", ownerID).
			Where("occurred_at >= ? AND occurred_at < ?", from.UTC(), to.UTC()).
			GroupExpr("event_type").
			Scan(ctx, &totals); err != nil {
			return err
		}
		for _, t := range totals {
			rows[t.EventType] = &UsageRollupRow{
				EventType:   t.EventType,
				TotalAmount: t.TotalAmount,
				EventCount:  t.EventCount,
				Dimensions:  map[string]int64{},
			}
		}

		var dims []dimRow
		if err := s.db.Q(ctx).NewSelect().
			TableExpr("billing.usage_events AS ue").
			ColumnExpr("ue.event_type AS event_type").
			ColumnExpr("d.key AS key").
			ColumnExpr("COALESCE(SUM((d.value)::bigint), 0) AS total").
			Join("CROSS JOIN LATERAL jsonb_each_text(ue.dimensions) AS d").
			Where("ue.tenant_id = ?", tenantID).
			Where("ue.tenant_subject_id = ?", ownerID).
			Where("ue.occurred_at >= ? AND ue.occurred_at < ?", from.UTC(), to.UTC()).
			GroupExpr("ue.event_type, d.key").
			Scan(ctx, &dims); err != nil {
			return err
		}
		for _, d := range dims {
			if r, ok := rows[d.EventType]; ok {
				r.Dimensions[d.Key] = d.Total
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	out := make([]UsageRollupRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	return out, nil
}
