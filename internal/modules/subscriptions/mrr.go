package subscriptions

import (
	"context"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db/gen"
)

// MRRViews reads the MRR of the merchant's subscriptions ids, each with its
// scheduled change. The subscription_mrr trigger keeps one row for every
// subscription with a price; one without has none.
func MRRViews(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]*billing.SubscriptionMRR, error) {
	out := make(map[uuid.UUID]*billing.SubscriptionMRR, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.ListSubscriptionMRR(ctx, gen.ListSubscriptionMRRParams{MerchantID: merchantID, SubscriptionIds: ids})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		view := &billing.SubscriptionMRR{Amount: row.Mrr, Currency: catalog.Currency(row.Currency)}
		if row.ScheduledKind != nil && row.ScheduledAmount != nil && row.ScheduledEffectiveAt != nil {
			view.Scheduled = &billing.ScheduledMRR{Kind: billing.ScheduledMRRKind(*row.ScheduledKind), Amount: *row.ScheduledAmount, EffectiveAt: row.ScheduledEffectiveAt.UTC()}
		}
		out[row.SubscriptionID] = view
	}
	return out, nil
}
