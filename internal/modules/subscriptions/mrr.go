package subscriptions

import (
	"context"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db/gen"
)

// MRRView is a subscription's MRR and its risk.
type MRRView struct {
	MRR  *billing.SubscriptionMRR
	Risk *billing.MRRRisk
}

// MRRViews reads the MRR of the merchant's subscriptions ids, each with its
// scheduled change and its open risk case. The subscription_mrr trigger keeps
// one row for every subscription with a price; one without has none.
func MRRViews(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]MRRView, error) {
	out := make(map[uuid.UUID]MRRView, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.ListSubscriptionMRR(ctx, gen.ListSubscriptionMRRParams{MerchantID: merchantID, SubscriptionIds: ids})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		view := MRRView{MRR: &billing.SubscriptionMRR{Amount: row.Mrr, Currency: catalog.Currency(row.Currency)}}
		if row.ScheduledKind != nil && row.ScheduledAmount != nil && row.ScheduledEffectiveAt != nil {
			view.MRR.Scheduled = &billing.ScheduledMRR{Kind: billing.ScheduledMRRKind(*row.ScheduledKind), Amount: *row.ScheduledAmount, EffectiveAt: row.ScheduledEffectiveAt.UTC()}
		}
		if len(row.Risk) > 0 && row.RiskEntryReason != nil && row.RiskSince != nil {
			risk := &billing.MRRRisk{EntryReason: billing.MRRRiskReason(*row.RiskEntryReason), Since: row.RiskSince.UTC()}
			for _, reason := range row.Risk {
				risk.Reasons = append(risk.Reasons, billing.MRRRiskReason(reason))
			}
			view.Risk = risk
		}
		out[row.SubscriptionID] = view
	}
	return out, nil
}
