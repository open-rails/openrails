package reconcile

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	repo "github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// UnknownReconcileOptions bounds one reconcile pass over the `unknown` cohort (#633).
type UnknownReconcileOptions struct {
	MaxPerRail    int           // cap subscriptions pulled per rail per run (0 -> 500)
	DunningWindow time.Duration // a failed renewal within this of the period end is recoverable (0 -> 14d)
	LookbackCap   time.Duration // never pull a window wider than this (#634 3y bound; 0 -> 3y)
	WindowSlack   time.Duration // pull from (oldest period end - slack) (0 -> 48h)
}

func (o UnknownReconcileOptions) withDefaults() UnknownReconcileOptions {
	if o.MaxPerRail <= 0 {
		o.MaxPerRail = 500
	}
	if o.DunningWindow <= 0 {
		o.DunningWindow = 14 * 24 * time.Hour
	}
	if o.LookbackCap <= 0 {
		o.LookbackCap = 3 * 365 * 24 * time.Hour
	}
	if o.WindowSlack <= 0 {
		o.WindowSlack = 48 * time.Hour
	}
	return o
}

// UnknownReconcileResult summarizes one pass.
type UnknownReconcileResult struct {
	Renewed      int
	PastDue      int
	Cancelled    int
	StillUnknown int
	Backfilled   int                 // payments imported (#634)
	RailErrors   map[Provider]string // rails that could not be pulled (their subs stay unknown; caller backs off)
}

// railToProvider maps a local subscription rail to its reconcile Provider.
func railToProvider(rail string) (Provider, bool) {
	switch rail {
	case "nmi", "mobius":
		return ProviderNMI, true
	case "ccbill":
		return ProviderCCBill, true
	case "stripe":
		return ProviderStripe, true
	case "solana":
		return ProviderSolana, true
	}
	return "", false
}

// ReconcileUnknownCohort resolves the `unknown` subscription cohort (#632) against
// provider truth using ONE windowed bulk fetch PER RAIL (#633) — never a
// per-subscription fan-out. For each rail it pulls [oldestPeriodEnd-slack, now]
// (clamped to LookbackCap), matches every unknown sub locally
// (ResolveUnknownFromSnapshot), applies the resolution (ResolveUnknownSubscription),
// and backfills the provider's missing charges (#634, idempotent by transaction id,
// declines recorded as failed). A rail whose fetch fails is recorded in RailErrors
// and its subs are LEFT unknown — the caller (a River job) retries with exponential
// backoff. Must run inside a merchant-scoped connection.
func ReconcileUnknownCohort(ctx context.Context, database *db.DB, lc *subscriptions.SubscriptionLifecycleService, fetchers map[Provider]RailFetcher, merchantID merchant.ID, now time.Time, opts UnknownReconcileOptions) (UnknownReconcileResult, error) {
	opts = opts.withDefaults()
	res := UnknownReconcileResult{RailErrors: map[Provider]string{}}
	q := database.Gen(ctx)

	for _, rail := range []string{"nmi", "mobius", "ccbill", "stripe", "solana"} {
		provider, ok := railToProvider(rail)
		if !ok {
			continue
		}
		fetcher := fetchers[provider]
		railArg := rail
		rows, err := q.ListUnknownSubscriptions(ctx, gen.ListUnknownSubscriptionsParams{
			MerchantID: merchantID.UUID(), Rail: &railArg, MaxRows: int32(opts.MaxPerRail),
		})
		if err != nil {
			return res, fmt.Errorf("reconcile unknown: list %s cohort: %w", rail, err)
		}
		if len(rows) == 0 {
			continue
		}
		if fetcher == nil {
			// No configured fetcher (missing creds / rail disabled) — leave unknown.
			res.RailErrors[provider] = "no fetcher configured"
			res.StillUnknown += len(rows)
			continue
		}

		// One windowed bulk pull covering every unknown sub of this rail.
		since := now
		for _, r := range rows {
			if r.CurrentPeriodEndsAt != nil && r.CurrentPeriodEndsAt.Before(since) {
				since = *r.CurrentPeriodEndsAt
			}
		}
		since = since.Add(-opts.WindowSlack)
		if floor := now.Add(-opts.LookbackCap); since.Before(floor) {
			since = floor
		}
		snap, err := fetcher.Fetch(ctx, FetchParams{Since: since, Until: now})
		if err != nil {
			// Provider unreachable: subs stay unknown, caller backs off.
			res.RailErrors[provider] = err.Error()
			res.StillUnknown += len(rows)
			continue
		}

		for _, r := range rows {
			periodEnd := now
			if r.CurrentPeriodEndsAt != nil {
				periodEnd = *r.CurrentPeriodEndsAt
			}
			verdict := ResolveUnknownFromSnapshot(
				UnknownSubject{RailSubscriptionID: r.RailSubscriptionID, PeriodEnd: periodEnd},
				snap, now, opts.DunningWindow,
			)
			if err := applyUnknownVerdict(ctx, database, lc, q, r.ID, verdict, now, opts.LookbackCap, &res); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}

// applyUnknownVerdict applies one resolution + its payment backfill.
func applyUnknownVerdict(ctx context.Context, database *db.DB, lc *subscriptions.SubscriptionLifecycleService, q *gen.Queries, subID uuid.UUID, v UnknownVerdict, now time.Time, lookbackCap time.Duration, res *UnknownReconcileResult) error {
	sub, err := repo.NewSubscriptionRepo(database).GetByID(ctx, subID)
	if err != nil {
		return fmt.Errorf("reconcile unknown: load subscription %s: %w", subID, err)
	}

	// Backfill the provider's missing charges first (#634) so a renewed sub's
	// confirming payment exists before/with the status flip.
	backfilled, err := backfillSubscriptionPayments(ctx, q, sub, v.Backfill, now, lookbackCap)
	if err != nil {
		return fmt.Errorf("reconcile unknown: backfill %s: %w", subID, err)
	}
	res.Backfilled += backfilled

	switch v.Outcome {
	case UnknownOutcomeRenewed:
		if err := lc.ResolveUnknownSubscription(ctx, database, sub, subscriptions.ResolveRenewed, v.NewPeriodEnd, now); err != nil {
			return err
		}
		res.Renewed++
	case UnknownOutcomePastDue:
		grace := now
		if sub.CurrentPeriodEndsAt != nil {
			grace = *sub.CurrentPeriodEndsAt
		}
		if err := lc.ResolveUnknownSubscription(ctx, database, sub, subscriptions.ResolvePastDue, nil, grace); err != nil {
			return err
		}
		res.PastDue++
	case UnknownOutcomeCancelled:
		if err := lc.ResolveUnknownSubscription(ctx, database, sub, subscriptions.ResolveCancelled, nil, now); err != nil {
			return err
		}
		res.Cancelled++
	default: // UnknownOutcomeUnreachable
		res.StillUnknown++
	}
	return nil
}

// backfillSubscriptionPayments imports a subscription's provider-observed charges
// that we are missing (#634), idempotent by transaction id (CreatePaymentIfNotExists
// ON CONFLICT DO NOTHING). Successful charges land as `completed`, declines/voids as
// `failed` — the true attempt history, so dunning/analytics see reality. Bounded to
// the LookbackCap (3y); older charges are out of recoverable scope. Returns the
// count newly inserted.
func backfillSubscriptionPayments(ctx context.Context, q *gen.Queries, sub *models.Subscription, txns []RemoteTransaction, now time.Time, lookbackCap time.Duration) (int, error) {
	floor := now.Add(-lookbackCap)
	inserted := 0
	for i := range txns {
		t := txns[i]
		if t.TransactionID == "" || t.OccurredAt.Before(floor) {
			continue
		}
		status := "completed"
		if !t.Success {
			status = "failed"
		}
		currency := t.Currency
		if currency == "" {
			currency = "usd"
		}
		subID := sub.ID
		n, err := q.CreatePaymentIfNotExists(ctx, gen.CreatePaymentIfNotExistsParams{
			ID:             uuid.New(),
			MerchantID:     sub.MerchantID,
			PriceID:        sub.PriceID,
			Rail:           string(sub.Rail),
			TransactionID:  t.TransactionID,
			Amount:         t.AmountCents,
			ListAmount:     t.AmountCents,
			Currency:       currency,
			Status:         status,
			SubscriptionID: &subID,
			PurchasedAt:    t.OccurredAt,
			CustomerID:     sub.CustomerID,
		})
		if err != nil {
			return inserted, fmt.Errorf("backfill payment %s: %w", t.TransactionID, err)
		}
		inserted += int(n)
	}
	return inserted, nil
}
