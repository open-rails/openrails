package reconcile

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/modules/analytics"
)

// HistoryEvent is one OpenRails analytics event used as the THIRD dunning
// evidence source (decision 2026-06-11): alongside the provider-pulled
// transaction timeline ("provider") and the local retry fields ("local"), the
// ClickHouse payment/subscription events ("history") carry deep history the
// provider APIs cannot return — including, for migrated tenants, the imported
// legacy rebill/scheduler events (doujins #387).
type HistoryEvent struct {
	// Table is the originating ClickHouse table (payment_events |
	// subscription_events).
	Table     string
	EventType string
	Processor string
	// SubscriptionID is the local billing.subscriptions uuid when known.
	SubscriptionID *uuid.UUID
	// ProcessorSubscriptionID correlates when no local id was stamped.
	ProcessorSubscriptionID string
	ProcessorTransactionID  string
	Status                  string
	Amount                  *float64
	OccurredAt              time.Time
}

// HistoryEventSource supplies the analytics-event evidence for the dunning
// forensics. Implementations must be read-only and FAIL SOFT: the engine
// turns any error into a note in the report, never a run failure.
type HistoryEventSource interface {
	// Configured reports whether the source can be queried at all (e.g.
	// ClickHouse present in the config). Unconfigured sources are noted in
	// the report and skipped.
	Configured() bool
	// ListEvents returns the tenant's events for the given local processor
	// names, oldest first. Zero since/until = unbounded on that side.
	ListEvents(ctx context.Context, processorNames []string, since, until time.Time) ([]HistoryEvent, error)
}

// analyticsHistorySource adapts the analytics module's ClickHouse forensics
// reader onto the engine's HistoryEventSource.
type analyticsHistorySource struct {
	svc *analytics.DunningHistoryService
}

// NewAnalyticsHistorySource wraps the ClickHouse dunning-history reader.
func NewAnalyticsHistorySource(svc *analytics.DunningHistoryService) HistoryEventSource {
	return &analyticsHistorySource{svc: svc}
}

func (s *analyticsHistorySource) Configured() bool {
	return s != nil && s.svc.Configured()
}

func (s *analyticsHistorySource) ListEvents(ctx context.Context, processorNames []string, since, until time.Time) ([]HistoryEvent, error) {
	rows, err := s.svc.ListDunningHistory(ctx, processorNames, since, until, 0)
	if err != nil {
		return nil, err
	}
	out := make([]HistoryEvent, 0, len(rows))
	for _, r := range rows {
		ev := HistoryEvent{
			Table:          r.Table,
			EventType:      r.EventType,
			Processor:      r.Processor,
			SubscriptionID: r.SubscriptionID,
			Status:         r.Status,
			Amount:         r.Amount,
			OccurredAt:     r.Timestamp,
		}
		if r.ProcessorSubscriptionID != nil {
			ev.ProcessorSubscriptionID = *r.ProcessorSubscriptionID
		}
		if r.ProcessorTransactionID != nil {
			ev.ProcessorTransactionID = *r.ProcessorTransactionID
		}
		out = append(out, ev)
	}
	return out, nil
}
