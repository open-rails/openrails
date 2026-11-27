package metrics

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money/books"
	"github.com/open-rails/openrails/internal/shared/calendarutil"
)

// LiveSchedules reads a merchant's revenue schedules still to recognise, by
// revenue_schedules_live_idx, with what the runoff splits by.
const LiveSchedules = `SELECT s.currency, s.revenue_account, COALESCE('prod_' || s.product_id::text, ''), s.funded, s.recognized_net,
	s.service_starts_at, s.service_ends_at, s.service_months, s.ended_at, s.recognized_through_at, s.reinstatements, s.forfeited
FROM billing.revenue_schedules s
WHERE s.merchant_id = $1 AND s.next_recognition_at IS NOT NULL`

// runRunoff folds the plan's runoff measure into groups: each live schedule's
// recognitions still to post, dated in the range, as the recognition job will
// post them in the accounting timezone.
func (s *Service) runRunoff(ctx context.Context, plan *Plan, groups map[string]*group) error {
	var leaf *Measure
	for _, m := range plan.Measures {
		for _, l := range m.components() {
			if l.Family == FamRunoff {
				leaf = l
			}
		}
	}
	if leaf == nil {
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, LiveSchedules, mid.UUID())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var row gen.BillingRevenueSchedule
			var product string
			if err := rows.Scan(&row.Currency, &row.RevenueAccount, &product, &row.Funded, &row.RecognizedNet, &row.ServiceStartsAt, &row.ServiceEndsAt,
				&row.ServiceMonths, &row.EndedAt, &row.RecognizedThroughAt, &row.Reinstatements, &row.Forfeited); err != nil {
				return err
			}
			dims, ok := usageDimsOf(plan, map[string]string{"currency": row.Currency, "account": row.RevenueAccount, "product_id": product})
			if !ok {
				continue
			}
			runoff, err := books.RunoffOf(ctx, row, plan.zone(), plan.To)
			if err != nil {
				return err
			}
			for _, r := range runoff {
				if r.Dated.Before(plan.From) || !r.Dated.Before(plan.To) {
					continue
				}
				var bucket time.Time
				if plan.HasTime {
					bucket = calendarutil.BucketStart(r.Dated, plan.Grain, plan.zone())
				}
				g := ensureGroup(groups, bucket, dims)
				v := g.vals[leaf.Name]
				v.n += r.Amount
				g.vals[leaf.Name] = v
			}
		}
		return rows.Err()
	})
}
