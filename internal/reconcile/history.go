package reconcile

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// HistoryEvent is one Postgres history row used as the THIRD dunning evidence
// source: alongside the provider-pulled transaction timeline ("provider") and
// the local retry fields ("local"), the imported legacy dunning history
// (doujins #387) and failed-payment rows carry history the provider APIs
// cannot return.
type HistoryEvent struct {
	// Table is the originating Postgres table (imported_dunning_history |
	// payments).
	Table     string
	EventType string
	Rail      string
	// SubscriptionID is the local openrails.subscriptions uuid when known.
	SubscriptionID *uuid.UUID
	// RailSubscriptionID correlates when no local id was stamped.
	RailSubscriptionID string
	RailTransactionID  string
	Status             string
	// AmountMicros is the raw row amount in micros; nil when the row has none.
	// Display/forensics only — format at the edge, never compute on it.
	AmountMicros *int64
	OccurredAt   time.Time
}

// HistoryEventSource supplies the history evidence for the dunning forensics.
// Implementations must be read-only and FAIL SOFT: the engine turns any error
// into a note in the report, never a run failure.
type HistoryEventSource interface {
	// Configured reports whether the source can be queried at all.
	// Unconfigured sources are noted in the report and skipped.
	Configured() bool
	// ListEvents returns the merchant's events for the given local rail
	// names, oldest first. Zero since/until = unbounded on that side.
	ListEvents(ctx context.Context, railNames []string, since, until time.Time) ([]HistoryEvent, error)
}

// historyEventLimit bounds one forensics read; deep history is the point, so
// the cap is generous but finite.
const historyEventLimit = 100000

// PGHistorySource reads dunning history from Postgres (#735):
// imported_dunning_history (the one-time legacy import) ∪ payments rows with
// status='failed'. Structured so #733's subscription_status_transitions can be
// added as another branch later. Display/forensics only — never a decision
// input.
type PGHistorySource struct {
	DB *db.DB
}

// NewPGHistorySource wraps the Postgres dunning-history reader.
func NewPGHistorySource(d *db.DB) HistoryEventSource {
	return &PGHistorySource{DB: d}
}

func (s *PGHistorySource) Configured() bool {
	return s != nil && s.DB != nil
}

func (s *PGHistorySource) ListEvents(ctx context.Context, railNames []string, since, until time.Time) ([]HistoryEvent, error) {
	if len(railNames) == 0 {
		return nil, nil
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var sincePtr, untilPtr *time.Time
	if !since.IsZero() {
		t := since.UTC()
		sincePtr = &t
	}
	if !until.IsZero() {
		t := until.UTC()
		untilPtr = &t
	}
	rows, err := s.DB.Gen(ctx).ListDunningHistoryEvents(ctx, gen.ListDunningHistoryEventsParams{
		MerchantID: tid.UUID(),
		Rails:      railNames,
		Since:      sincePtr,
		Until:      untilPtr,
		LimitRows:  historyEventLimit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]HistoryEvent, 0, len(rows))
	for _, r := range rows {
		ev := HistoryEvent{
			Table:          r.SourceTable,
			EventType:      r.EventType,
			Rail:           r.Rail,
			SubscriptionID: r.SubscriptionID,
			Status:         r.Status,
			OccurredAt:     r.OccurredAt,
		}
		if r.RailSubscriptionID != nil {
			ev.RailSubscriptionID = *r.RailSubscriptionID
		}
		if r.RailTransactionID != nil {
			ev.RailTransactionID = *r.RailTransactionID
		}
		if r.AmountMicros != nil {
			micros := *r.AmountMicros
			ev.AmountMicros = &micros
		}
		out = append(out, ev)
	}
	return out, nil
}
