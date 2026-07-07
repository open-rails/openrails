package models

import (
	"time"

	"github.com/google/uuid"
)

// RepriceStatus is the lifecycle of one SubscriptionReprice row.
type RepriceStatus string

const (
	RepriceStatusScheduled RepriceStatus = "scheduled"
	RepriceStatusApplied   RepriceStatus = "applied"
	RepriceStatusCanceled  RepriceStatus = "canceled"
)

// SubscriptionReprice (#773) is one subscription's scheduled/applied/canceled
// price move. At most one status=scheduled row exists per subscription at a
// time, so the renewal boundary always has an unambiguous due reprice (if
// any) to pick up. Applied at the subscription's first renewal on/after
// EffectiveAt — v1 has no proration or mid-cycle application.
type SubscriptionReprice struct {
	ID             uuid.UUID     `json:"id"`
	MerchantID     uuid.UUID     `json:"merchant_id"`
	SubscriptionID uuid.UUID     `json:"subscription_id"`
	FromPriceID    uuid.UUID     `json:"from_price_id"`
	ToPriceID      uuid.UUID     `json:"to_price_id"`
	EffectiveAt    time.Time     `json:"effective_at"`
	Status         RepriceStatus `json:"status"`
	RepriceBatchID *uuid.UUID    `json:"reprice_batch_id,omitempty"`
	CreatedAt      time.Time     `json:"created_at"`
	AppliedAt      *time.Time    `json:"applied_at,omitempty"`
	CanceledAt     *time.Time    `json:"canceled_at,omitempty"`
	// AcknowledgedShortNotice (#781) is the audit trail for the escape hatch:
	// true when this INCREASE reprice's effective_at was inside the
	// merchant's configured notice window and was scheduled anyway via the
	// request's explicit acknowledge_short_notice override.
	AcknowledgedShortNotice bool `json:"acknowledged_short_notice"`
}

// IsDue reports whether this scheduled reprice should be applied at the
// subscription's renewal happening "now" — v1's ONLY effective moment: the
// subscription's first renewal on/after EffectiveAt.
func (r *SubscriptionReprice) IsDue(now time.Time) bool {
	return r != nil && r.Status == RepriceStatusScheduled && !r.EffectiveAt.After(now)
}

// RepriceBatch (#773) is the header row for one bulk reprice operation
// (reprice_all_prior_versions or a single ad-hoc reprice), giving callers one
// handle to inspect per-subscription progress.
type RepriceBatch struct {
	ID                     uuid.UUID `json:"id"`
	MerchantID             uuid.UUID `json:"merchant_id"`
	PriceKey               *string   `json:"price_key,omitempty"`
	ToPriceID              uuid.UUID `json:"to_price_id"`
	EffectiveAt            time.Time `json:"effective_at"`
	SubscriptionsMatched   int       `json:"subscriptions_matched"`
	SubscriptionsScheduled int       `json:"subscriptions_scheduled"`
	SubscriptionsSkipped   int       `json:"subscriptions_skipped"`
	CreatedAt              time.Time `json:"created_at"`
}
