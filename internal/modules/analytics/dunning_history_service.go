package analytics

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/pkg/tenant"
)

// ARCHITECTURAL BOUNDARY (issue #232 — Postgres is the system of record):
//
// DunningHistoryService is a FORENSICS/DISPLAY-ONLY read surface over the
// derived, tenant-scoped ClickHouse event sink (payment_events +
// subscription_events). It exists for exactly one consumer: the reconcile
// engine's dunning-forensics report (#107), which merges these events as a
// third evidence source alongside the provider-pulled transaction timeline and
// the local retry fields. For migrated tenants the sink also holds the
// imported legacy history (doujins #387 normalizes users_logs rebill attempts,
// mobius_schedulers scheduler events and payment_settings gateway state into
// these tables), so this is what answers "did legacy dunning run and when did
// it die" for periods the provider APIs cannot serve.
//
// It must NEVER be consulted for a money / entitlement / authorization
// decision — the reconcile DIFF and its enforce appliers read Postgres and the
// provider only; this source feeds the report sections exclusively.
// ClickHouse is OPTIONAL: when unconfigured/unavailable the caller degrades to
// a note in the report, never an error.
type DunningHistoryService struct {
	cfg *config.ClickHouseConfig
}

func NewDunningHistoryService(cfg *config.ClickHouseConfig) *DunningHistoryService {
	return &DunningHistoryService{cfg: cfg}
}

// Configured reports whether ClickHouse is configured at all (same gate as the
// event writer: presence of HTTPAddr signals intent to use ClickHouse).
func (s *DunningHistoryService) Configured() bool {
	return s != nil && s.cfg != nil && s.cfg.HTTPAddr != ""
}

// DunningHistoryEvent is one analytics event relevant to dunning forensics.
type DunningHistoryEvent struct {
	// Table is the source table: payment_events or subscription_events.
	Table     string
	EventType string
	Processor string
	// SubscriptionID is the LOCAL openrails.subscriptions uuid when the writer
	// knew it (imported legacy events carry the migrated subscription's id).
	SubscriptionID          *uuid.UUID
	ProcessorSubscriptionID *string
	ProcessorTransactionID  *string
	Status                  string
	Amount                  *float64
	Timestamp               time.Time
}

// defaultDunningHistoryLimit bounds one forensics read; deep history is the
// point, so the cap is generous but finite.
const defaultDunningHistoryLimit = 100000

// ListDunningHistory returns the tenant's payment + subscription events for
// the given processors, oldest first. Zero since/until leave the window
// unbounded on that side (legacy history predates any reconcile window).
func (s *DunningHistoryService) ListDunningHistory(ctx context.Context, processors []string, since, until time.Time, limit int) ([]DunningHistoryEvent, error) {
	if !s.Configured() {
		return nil, fmt.Errorf("clickhouse not configured")
	}
	if len(processors) == 0 {
		return nil, nil
	}
	if limit <= 0 || limit > defaultDunningHistoryLimit {
		limit = defaultDunningHistoryLimit
	}
	conn, err := initClickHouseConnection(ctx, s.cfg)
	if err != nil {
		return nil, fmt.Errorf("connect clickhouse: %w", err)
	}
	defer func() { _ = conn.Close() }()

	tenantID := tenant.FromContextOrDefault(ctx).String()

	window := ""
	args := []any{tenantID, processors, tenantID, processors}
	// The window predicates are appended symmetrically to both UNION branches.
	var windowArgs []any
	if !since.IsZero() {
		window += " AND timestamp >= ?"
		windowArgs = append(windowArgs, since.UTC())
	}
	if !until.IsZero() {
		window += " AND timestamp <= ?"
		windowArgs = append(windowArgs, until.UTC())
	}

	query := `
        SELECT source, event_type, processor, subscription_id,
               processor_subscription_id, processor_transaction_id, status,
               amount, timestamp
        FROM (
            SELECT 'payment_events' AS source,
                   event_type, processor, subscription_id,
                   CAST(NULL, 'Nullable(String)') AS processor_subscription_id,
                   processor_transaction_id,
                   '' AS status,
                   toFloat64(amount) AS amount,
                   timestamp
            FROM payment_events
            WHERE tenant_id = ? AND processor IN (?)` + window + `
            UNION ALL
            SELECT 'subscription_events' AS source,
                   event_type, processor, subscription_id,
                   processor_subscription_id,
                   processor_transaction_id,
                   status,
                   toNullable(toFloat64(price_amount)) AS amount,
                   timestamp
            FROM subscription_events
            WHERE tenant_id = ? AND processor IN (?)` + window + `
        )
        ORDER BY timestamp ASC
        LIMIT ?`

	// Interleave: tenant+processors (+window) for branch 1, same for branch 2.
	allArgs := make([]any, 0, len(args)+2*len(windowArgs)+1)
	allArgs = append(allArgs, args[0], args[1])
	allArgs = append(allArgs, windowArgs...)
	allArgs = append(allArgs, args[2], args[3])
	allArgs = append(allArgs, windowArgs...)
	allArgs = append(allArgs, limit)

	rows, err := conn.Query(ctx, query, allArgs...)
	if err != nil {
		return nil, fmt.Errorf("query dunning history: %w", err)
	}
	defer rows.Close()

	var out []DunningHistoryEvent
	for rows.Next() {
		var ev DunningHistoryEvent
		if err := rows.Scan(&ev.Table, &ev.EventType, &ev.Processor,
			&ev.SubscriptionID, &ev.ProcessorSubscriptionID,
			&ev.ProcessorTransactionID, &ev.Status, &ev.Amount,
			&ev.Timestamp); err != nil {
			return nil, fmt.Errorf("scan dunning history row: %w", err)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read dunning history rows: %w", err)
	}
	return out, nil
}
