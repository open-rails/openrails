package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrHostLifecycleEventNotFound indicates the event id does not exist under the
// given merchant scope.
var ErrHostLifecycleEventNotFound = errors.New("controlplane: host lifecycle event not found for merchant")

// HostLifecycleEvent is one durable signal the embedding host must act on
// (or#878). Today the only kind is an arrears delinquency transition:
//
//	delinquency.grace     — overdue, still inside the merchant's grace window
//	delinquency.entered   — past grace: OpenRails has stopped admitting new
//	                        spend, and the host should shut off what it runs
//	delinquency.cleared   — the debt is settled; restore whatever was shut off
//
// The feed is durable and acked rather than pushed, because both directions of
// a lost message are expensive: a missed cut-off is a revenue leak, and a missed
// restore is an outage for someone who has already paid. A host acks only after
// its own idempotent processing succeeds.
//
// OpenRails never performs the shutoff. It does not know what the host runs, and
// modelling the operator's resources is outside its business.
type HostLifecycleEvent struct {
	ID          uuid.UUID
	MerchantID  merchant.ID
	EventType   string
	SubjectType string
	SubjectID   uuid.UUID
	Currency    string
	OccurredAt  time.Time
	Data        map[string]any
}

// ListPendingHostLifecycleEvents returns the oldest unacknowledged lifecycle
// events for one merchant. This is a host seam; callers must authorize the
// merchant ID before use.
//
// Merchant-scoped through MerchantTx from the start (the or#861 lesson: this
// same feed shape, run on the bare pool against an RLS-forced table, silently
// listed NOTHING and every test missed it because the tests held a GUC-bearing
// connection).
func (c *ControlPlane) ListPendingHostLifecycleEvents(ctx context.Context, merchantID merchant.ID, limit int) ([]HostLifecycleEvent, error) {
	if merchantID.IsZero() {
		return nil, errors.New("control plane: list host lifecycle events: merchant id is required")
	}
	if limit <= 0 {
		limit = 100
	}
	var rows []gen.ListPendingHostLifecycleEventsRow
	err := c.pool.MerchantTx(ctx, merchantID, func(ctx context.Context, tx pgx.Tx) error {
		var qErr error
		rows, qErr = gen.New(tx).ListPendingHostLifecycleEvents(ctx, gen.ListPendingHostLifecycleEventsParams{
			MerchantID: merchantID.UUID(),
			RowLimit:   int64(limit),
		})
		return qErr
	})
	if err != nil {
		return nil, fmt.Errorf("control plane: list host lifecycle events: %w", err)
	}

	events := make([]HostLifecycleEvent, 0, len(rows))
	for _, row := range rows {
		event := HostLifecycleEvent{
			ID:          row.ID,
			MerchantID:  merchant.ID(row.MerchantID),
			EventType:   row.EventType,
			SubjectType: row.SubjectType,
			SubjectID:   row.SubjectID,
			OccurredAt:  row.OccurredAt,
		}
		if row.Currency != nil {
			event.Currency = *row.Currency
		}
		if len(row.Data) > 0 {
			if err := json.Unmarshal(row.Data, &event.Data); err != nil {
				return nil, fmt.Errorf("control plane: decode host lifecycle event %s: %w", row.ID, err)
			}
		}
		events = append(events, event)
	}
	return events, nil
}

// AcknowledgeHostLifecycleEvent removes an event from the pending feed. The id
// must belong to the given merchant; acking another merchant's event returns
// ErrHostLifecycleEventNotFound.
func (c *ControlPlane) AcknowledgeHostLifecycleEvent(ctx context.Context, merchantID merchant.ID, id uuid.UUID) error {
	if merchantID.IsZero() {
		return errors.New("control plane: acknowledge host lifecycle event: merchant id is required")
	}
	var n int64
	err := c.pool.MerchantTx(ctx, merchantID, func(ctx context.Context, tx pgx.Tx) error {
		var qErr error
		n, qErr = gen.New(tx).AcknowledgeHostLifecycleEvent(ctx, gen.AcknowledgeHostLifecycleEventParams{
			MerchantID: merchantID.UUID(),
			ID:         id,
			Now:        time.Now().UTC(),
		})
		return qErr
	})
	if err != nil {
		return fmt.Errorf("control plane: acknowledge host lifecycle event: %w", err)
	}
	if n == 0 {
		return ErrHostLifecycleEventNotFound
	}
	return nil
}
