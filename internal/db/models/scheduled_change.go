package models

import (
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
)

// ScheduledChangeStatus is where a scheduled change stands.
type ScheduledChangeStatus string

const (
	ScheduledChangeScheduled ScheduledChangeStatus = "scheduled"
	ScheduledChangeApplied   ScheduledChangeStatus = "applied"
	ScheduledChangeCanceled  ScheduledChangeStatus = "canceled"
	// ScheduledChangeBlocked is a migration's move its provider could not take.
	ScheduledChangeBlocked ScheduledChangeStatus = "blocked"
)

// ScheduledChange is one subscription's change waiting for its renewal. At
// most one is scheduled per subscription.
type ScheduledChange struct {
	ID               uuid.UUID
	MerchantID       uuid.UUID
	SubscriptionID   uuid.UUID
	FromPriceID      uuid.UUID
	PriceID          uuid.UUID
	Quantity         *int
	EffectiveAt      time.Time
	Source           billing.ScheduledChangeSource
	PriceMigrationID *uuid.UUID
	Status           ScheduledChangeStatus
	BlockedReason    string
	// AcknowledgedShortNotice records a price increase scheduled inside the
	// merchant's notice window under an explicit acknowledgement.
	AcknowledgedShortNotice bool
	CreatedAt               time.Time
	AppliedAt               *time.Time
	CanceledAt              *time.Time
}

// IsDue reports whether the renewal happening at now applies it: the
// subscription's first renewal on or after EffectiveAt.
func (c *ScheduledChange) IsDue(now time.Time) bool {
	return c != nil && c.Status == ScheduledChangeScheduled && !c.EffectiveAt.After(now)
}

// View is the change on the wire.
func (c *ScheduledChange) View() *billing.ScheduledChange {
	if c == nil {
		return nil
	}
	out := &billing.ScheduledChange{
		PriceID: billing.PriceID(c.PriceID), Quantity: c.Quantity, EffectiveAt: c.EffectiveAt.UTC(),
		Source: c.Source, CreatedAt: c.CreatedAt.UTC(),
	}
	if c.PriceMigrationID != nil {
		id := billing.PriceMigrationID(*c.PriceMigrationID)
		out.PriceMigrationID = &id
	}
	return out
}

// ScheduledChangeFromGen maps a generated scheduled_changes row.
func ScheduledChangeFromGen(r gen.BillingScheduledChange) *ScheduledChange {
	out := &ScheduledChange{
		ID: r.ID, MerchantID: r.MerchantID, SubscriptionID: r.SubscriptionID, FromPriceID: r.FromPriceID,
		PriceID: r.PriceID, EffectiveAt: r.EffectiveAt, Source: billing.ScheduledChangeSource(r.Source),
		PriceMigrationID: r.PriceMigrationID, Status: ScheduledChangeStatus(r.Status), BlockedReason: DerefStr(r.BlockedReason),
		AcknowledgedShortNotice: r.AcknowledgedShortNotice, CreatedAt: r.CreatedAt, AppliedAt: r.AppliedAt, CanceledAt: r.CanceledAt,
	}
	if r.Quantity != nil {
		q := int(*r.Quantity)
		out.Quantity = &q
	}
	return out
}

func ScheduledChangesFromGen(rows []gen.BillingScheduledChange) []*ScheduledChange {
	out := make([]*ScheduledChange, 0, len(rows))
	for _, r := range rows {
		out = append(out, ScheduledChangeFromGen(r))
	}
	return out
}
