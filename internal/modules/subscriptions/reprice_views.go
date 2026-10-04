package subscriptions

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
)

func repriceBatch(b gen.BillingRepriceBatch, scheduled, applied, canceled, blocked int64) billing.RepriceBatch {
	out := billing.RepriceBatch{
		ID: billing.RepriceBatchID(b.ID), Kind: billing.RepriceKind(b.Kind), PriceKey: b.PriceKey,
		SourcePriceID: (*billing.PriceID)(b.SourcePriceID), ToPriceID: billing.PriceID(b.ToPriceID), EffectiveAt: b.EffectiveAt,
		Matched: int(b.SubscriptionsMatched), Skipped: int(b.SubscriptionsSkipped),
		Scheduled: int(scheduled), Applied: int(applied), Canceled: int(canceled), Blocked: int(blocked),
		CreatedAt: b.CreatedAt,
	}
	if b.FallbackPolicy != "" {
		policy := b.FallbackPolicy
		out.FallbackPolicy = &policy
	}
	return out
}

// Reprice is a reprice row on the wire.
func Reprice(r *models.SubscriptionReprice) billing.Reprice {
	out := billing.Reprice{
		ID: billing.RepriceID(r.ID), SubscriptionID: billing.SubscriptionID(r.SubscriptionID),
		FromPriceID: billing.PriceID(r.FromPriceID), ToPriceID: billing.PriceID(r.ToPriceID), EffectiveAt: r.EffectiveAt,
		Status: r.Status, Kind: r.Kind, RepriceBatchID: (*billing.RepriceBatchID)(r.RepriceBatchID),
		AcknowledgedShortNotice: r.AcknowledgedShortNotice, CreatedAt: r.CreatedAt, AppliedAt: r.AppliedAt, CanceledAt: r.CanceledAt,
	}
	if r.BlockedReason != "" {
		reason := r.BlockedReason
		out.BlockedReason = &reason
	}
	return out
}
