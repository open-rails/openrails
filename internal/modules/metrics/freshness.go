package metrics

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/reconcile"
)

// Every read reports each PSP's known-through. A stale PSP's cash and
// payments after it are unknown, not zero: a time bucket starting at or after
// it reads null wherever no current PSP adds to it. Rebills it could not see
// are left out in SQL (pastKnownThrough); subscriptions keep their last known
// state, so MRR carries forward.

// freshness attaches the merchant's known-throughs to res and marks what is
// missing.
func (s *Service) freshness(ctx context.Context, plan *Plan, res *Result) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		list, complete, through, err := reconcile.KnownThroughs(ctx, s.db.NewWithPgxTx(tx).Gen(ctx), mid.UUID(), plan.AsOf)
		if err != nil {
			return err
		}
		res.Freshness, res.CompleteThrough, res.KnownThrough = list, utcPtr(complete), utcPtr(through)
		missing(plan, res.Rows, list)
		missing(plan, res.CompareRows, list)
		return nil
	})
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.UTC()
	return &v
}

// missing nulls the payment measures of time buckets past the known-through
// of every PSP with data their row covers: its own PSP's with psp, its
// rail's PSPs' with rail, else every PSP's. A bucket a current PSP adds to
// keeps its value.
func missing(plan *Plan, rows [][]any, list []billing.PSPKnownThrough) {
	if !plan.HasTime || len(rows) == 0 {
		return
	}
	var cols []int
	for i, m := range plan.Measures {
		if paymentsOnly(m) {
			cols = append(cols, 1+len(plan.Dims)+i)
		}
	}
	if len(cols) == 0 {
		return
	}
	pspCol, railCol := slices.Index(plan.Dims, "psp"), slices.Index(plan.Dims, "rail")
	for _, row := range rows {
		bucket, err := time.Parse(time.RFC3339, row[0].(string))
		if err != nil {
			continue
		}
		covers := func(f billing.PSPKnownThrough) bool {
			switch {
			case pspCol >= 0:
				return row[1+pspCol] == f.PSPID.String()
			case railCol >= 0:
				return row[1+railCol] == string(f.Rail)
			}
			return true
		}
		if unknownFrom(list, covers, bucket) {
			for _, c := range cols {
				row[c] = nil
			}
		}
	}
}

// unknownFrom reports whether every PSP with data the row covers was past its
// known-through at bucket, and at least one is.
func unknownFrom(list []billing.PSPKnownThrough, covers func(billing.PSPKnownThrough) bool, bucket time.Time) bool {
	stale := false
	for _, f := range list {
		if !f.HasData || !covers(f) {
			continue
		}
		if f.Current || f.KnownThrough != nil && bucket.Before(*f.KnownThrough) {
			return false
		}
		stale = true
	}
	return stale
}

// paymentsOnly reports whether every component of m reads payments.
func paymentsOnly(m *Measure) bool {
	for _, leaf := range m.components() {
		if leaf.Family != FamPayments {
			return false
		}
	}
	return true
}

// pastKnownThrough is SQL true when the row's instant at falls after the
// known-through of its PSP, stale: what OpenRails could not see. Its evidence
// is reconcile.KnownThroughs'.
func pastKnownThrough(merchant, psp, at string) string {
	return `EXISTS (SELECT 1 FROM billing.psp_freshness kt
		LEFT JOIN billing.webhook_health kw ON kw.merchant_id = kt.merchant_id AND kw.psp_id = kt.psp_id
		WHERE kt.merchant_id = ` + merchant + ` AND kt.psp_id = ` + psp + ` AND kt.stale_since IS NOT NULL
		AND ` + at + ` > COALESCE(greatest(kt.read_at, kt.imported_through, kw.last_accepted_at, kw.last_pull_at), '-infinity'::timestamptz))`
}
