package subscriptions

import (
	"context"
	"time"

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

// MRRViews reads the MRR of the merchant's subscriptions ids at at, each with
// its scheduled change and its risk. The subscription_mrr trigger keeps one
// row for every subscription with a price; one without has none. A
// cancellation at period end adds its MRR until its end, scheduled churn till
// then; risk's reasons are state's, its case dates them, else state does.
func MRRViews(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, ids []uuid.UUID, at time.Time) (map[uuid.UUID]MRRView, error) {
	out := make(map[uuid.UUID]MRRView, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.ListSubscriptionMRR(ctx, gen.ListSubscriptionMRRParams{MerchantID: merchantID, SubscriptionIds: ids, At: at})
	if err != nil {
		return nil, err
	}
	changes, err := q.ListScheduledMRRChanges(ctx, gen.ListScheduledMRRChangesParams{MerchantID: merchantID, SubscriptionIds: ids})
	if err != nil {
		return nil, err
	}
	risks, err := q.ListMRRRiskNow(ctx, gen.ListMRRRiskNowParams{MerchantID: merchantID, At: at, SubscriptionIds: ids})
	if err != nil {
		return nil, err
	}
	scheduled := make(map[uuid.UUID]*billing.ScheduledMRR, len(changes))
	for _, c := range changes {
		kind := billing.ScheduledMRRExpansion
		if c.Amount < 0 {
			kind = billing.ScheduledMRRContraction
		}
		scheduled[c.SubscriptionID] = &billing.ScheduledMRR{Kind: kind, Amount: c.Amount, EffectiveAt: c.EffectiveAt.UTC()}
	}
	risky := make(map[uuid.UUID]gen.ListMRRRiskNowRow, len(risks))
	for _, r := range risks {
		risky[r.SubscriptionID] = r
	}
	for _, row := range rows {
		ending := row.EndsAt != nil && row.EndsAt.After(at)
		amount := row.Mrr
		if row.EndsAt != nil && !ending {
			amount = 0
		}
		view := MRRView{MRR: &billing.SubscriptionMRR{Amount: amount, Currency: catalog.Currency(row.Currency)}}
		if ending && row.Mrr > 0 {
			view.MRR.Scheduled = &billing.ScheduledMRR{Kind: billing.ScheduledMRRChurn, Amount: -row.Mrr, EffectiveAt: row.EndsAt.UTC()}
		} else if s, ok := scheduled[row.SubscriptionID]; ok {
			view.MRR.Scheduled = s
		}
		if r, ok := risky[row.SubscriptionID]; ok && len(r.Reasons) > 0 {
			risk := &billing.MRRRisk{EntryReason: billing.MRRRiskReason(r.Reasons[0]), Since: r.Since.UTC()}
			if row.RiskEntryReason != nil && row.RiskSince != nil {
				risk.EntryReason, risk.Since = billing.MRRRiskReason(*row.RiskEntryReason), row.RiskSince.UTC()
			}
			for _, reason := range r.Reasons {
				risk.Reasons = append(risk.Reasons, billing.MRRRiskReason(reason))
			}
			view.Risk = risk
		}
		out[row.SubscriptionID] = view
	}
	return out, nil
}
