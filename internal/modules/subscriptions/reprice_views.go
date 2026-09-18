package subscriptions

import (
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db/models"
)

// RepriceBatchView is a reprice batch header on the wire: typed price ids,
// plain UUIDs for the batch itself.
type RepriceBatchView struct {
	ID                     uuid.UUID          `json:"id"`
	MerchantID             uuid.UUID          `json:"merchant_id"`
	PriceKey               *string            `json:"price_key,omitempty"`
	ToPriceID              openrails.PriceID  `json:"to_price_id"`
	SourcePriceID          *openrails.PriceID `json:"source_price_id,omitempty"`
	EffectiveAt            time.Time          `json:"effective_at"`
	Kind                   models.RepriceKind `json:"kind"`
	FallbackPolicy         string             `json:"fallback_policy,omitempty"`
	SubscriptionsMatched   int                `json:"subscriptions_matched"`
	SubscriptionsScheduled int                `json:"subscriptions_scheduled"`
	SubscriptionsSkipped   int                `json:"subscriptions_skipped"`
	SubscriptionsBlocked   int                `json:"subscriptions_blocked"`
	CreatedAt              time.Time          `json:"created_at"`
}

func RepriceBatchViewOf(b *models.RepriceBatch) RepriceBatchView {
	return RepriceBatchView{
		ID: b.ID, MerchantID: b.MerchantID, PriceKey: b.PriceKey, ToPriceID: openrails.PriceID(b.ToPriceID),
		SourcePriceID: (*openrails.PriceID)(b.SourcePriceID), EffectiveAt: b.EffectiveAt, Kind: b.Kind, FallbackPolicy: b.FallbackPolicy,
		SubscriptionsMatched: b.SubscriptionsMatched, SubscriptionsScheduled: b.SubscriptionsScheduled, SubscriptionsSkipped: b.SubscriptionsSkipped,
		SubscriptionsBlocked: b.SubscriptionsBlocked, CreatedAt: b.CreatedAt,
	}
}

// SubscriptionRepriceView is one scheduled price change on the wire.
type SubscriptionRepriceView struct {
	ID                      uuid.UUID                `json:"id"`
	MerchantID              uuid.UUID                `json:"merchant_id"`
	SubscriptionID          openrails.SubscriptionID `json:"subscription_id"`
	FromPriceID             openrails.PriceID        `json:"from_price_id"`
	ToPriceID               openrails.PriceID        `json:"to_price_id"`
	EffectiveAt             time.Time                `json:"effective_at"`
	Status                  models.RepriceStatus     `json:"status"`
	Kind                    models.RepriceKind       `json:"kind"`
	BlockedReason           string                   `json:"blocked_reason,omitempty"`
	RepriceBatchID          *uuid.UUID               `json:"reprice_batch_id,omitempty"`
	AcknowledgedShortNotice bool                     `json:"acknowledged_short_notice"`
	CreatedAt               time.Time                `json:"created_at"`
	AppliedAt               *time.Time               `json:"applied_at,omitempty"`
	CanceledAt              *time.Time               `json:"canceled_at,omitempty"`
}

func SubscriptionRepriceViewOf(r *models.SubscriptionReprice) SubscriptionRepriceView {
	return SubscriptionRepriceView{
		ID: r.ID, MerchantID: r.MerchantID, SubscriptionID: openrails.SubscriptionID(r.SubscriptionID),
		FromPriceID: openrails.PriceID(r.FromPriceID), ToPriceID: openrails.PriceID(r.ToPriceID), EffectiveAt: r.EffectiveAt,
		Status: r.Status, Kind: r.Kind, BlockedReason: r.BlockedReason, RepriceBatchID: r.RepriceBatchID,
		AcknowledgedShortNotice: r.AcknowledgedShortNotice, CreatedAt: r.CreatedAt, AppliedAt: r.AppliedAt, CanceledAt: r.CanceledAt,
	}
}

func RepriceBatchViews(items []*models.RepriceBatch) []RepriceBatchView {
	out := make([]RepriceBatchView, 0, len(items))
	for _, b := range items {
		out = append(out, RepriceBatchViewOf(b))
	}
	return out
}

func SubscriptionRepriceViews(items []*models.SubscriptionReprice) []SubscriptionRepriceView {
	out := make([]SubscriptionRepriceView, 0, len(items))
	for _, r := range items {
		out = append(out, SubscriptionRepriceViewOf(r))
	}
	return out
}
