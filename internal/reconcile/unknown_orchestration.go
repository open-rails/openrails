package reconcile

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
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
		o.DunningWindow = DefaultDunningWindow
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
	Renewed       int
	Adopted       int // remote period end adopted without a charge (#367 doctrine)
	PastDue       int
	Cancelled     int
	StillUnknown  int
	Probed        int                 // per-subscription probe fallbacks attempted (#665)
	Backfilled    int                 // payments imported (#634)
	RailCustomers int                 // rail_customer_accounts materialized from a remote customer id (#635)
	RailErrors    map[Provider]string // rails that could not be pulled (their subs stay unknown; caller backs off)
}

// reconcilableRails returns the rails with provider accounts (registry-driven,
// #669) in stable order; their reconcile Provider is the rail name itself
// (#630: mobius is a provider account on rail nmi, not a rail).
func reconcilableRails() []string {
	var out []string
	for _, d := range rails.All() {
		if d.HasRailMerchantAccounts {
			out = append(out, string(d.Rail))
		}
	}
	return out
}

// ReconcileUnknownCohort resolves the `unknown` subscription cohort (#632) against
// provider truth using ONE windowed bulk fetch PER RAIL (#633) — plus a targeted
// per-subscription probe (#665) ONLY for rows the bulk snapshot could not decide
// (NULL period end, evidence outside the window, non-exhaustive roster). For each
// rail it pulls [oldestPeriodEnd-slack, now] (clamped to LookbackCap), feeds
// every unknown sub to the ONE decider (Decide), applies the transition
// (ApplyDecision), and backfills the provider's missing charges (#634,
// idempotent by transaction id, declines recorded as failed). A rail whose
// fetch fails is recorded in RailErrors and its subs are LEFT unknown — the
// caller (a River job) retries with exponential backoff, and no per-sub probes
// are fanned out against a rail that just failed a bulk read. Must run inside a
// merchant-scoped connection.
func ReconcileUnknownCohort(ctx context.Context, database *db.DB, lc *subscriptions.SubscriptionLifecycleService, fetchers map[Provider]RailFetcher, probers map[Provider]SubscriptionProber, merchantID merchant.ID, now time.Time, opts UnknownReconcileOptions) (UnknownReconcileResult, error) {
	opts = opts.withDefaults()
	res := UnknownReconcileResult{RailErrors: map[Provider]string{}}
	q := database.Gen(ctx)

	for _, rail := range reconcilableRails() {
		provider := Provider(rail)
		fetcher := fetchers[provider]
		prober := probers[provider]
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
		if fetcher == nil && prober == nil {
			// No configured fetcher (missing creds / rail disabled) — leave unknown.
			res.RailErrors[provider] = "no fetcher configured"
			res.StillUnknown += len(rows)
			continue
		}

		// One windowed bulk pull covering every unknown sub of this rail.
		var snap *RemoteSnapshot
		if fetcher != nil {
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
			snap, err = fetcher.Fetch(ctx, FetchParams{Since: since, Until: now})
			if err != nil {
				// Provider unreachable: subs stay unknown, caller backs off.
				res.RailErrors[provider] = err.Error()
				res.StillUnknown += len(rows)
				continue
			}
		}

		for _, r := range rows {
			state := SubscriptionState{
				Status:             string(models.StatusUnknown),
				Rail:               rail,
				RailSubscriptionID: r.RailSubscriptionID,
				PeriodEnd:          r.CurrentPeriodEndsAt,
			}
			decision := Decide(state, EvidenceBundle{Snapshot: snap}, now, opts.DunningWindow)
			if decision.Kind == TransitionNone && prober != nil && r.RailSubscriptionID != "" {
				// #665: the bulk window couldn't decide this row — ONE targeted
				// per-sub probe, fed to the SAME decider. A probe failure keeps
				// the row unknown (retried next pass).
				res.Probed++
				if psnap, perr := prober.ProbeSubscription(ctx, ProbeSubject{
					LocalID: r.ID, RailSubscriptionID: r.RailSubscriptionID, PeriodEnd: r.CurrentPeriodEndsAt,
				}); perr != nil {
					log.WithContext(ctx).WithError(perr).WithFields(log.Fields{
						"subscription_id": r.ID, "rail": rail,
					}).Warn("reconcile unknown: per-subscription probe failed; staying unknown")
				} else {
					decision = Decide(state, EvidenceBundle{Snapshot: psnap}, now, opts.DunningWindow)
				}
			}
			if err := applyUnknownDecision(ctx, database, lc, q, r.ID, decision, now, opts.LookbackCap, &res); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}

// applyUnknownDecision applies one decider transition + its payment backfill.
func applyUnknownDecision(ctx context.Context, database *db.DB, lc *subscriptions.SubscriptionLifecycleService, q *gen.Queries, subID uuid.UUID, d Decision, now time.Time, lookbackCap time.Duration, res *UnknownReconcileResult) error {
	sub, err := subscriptions.NewSubscriptionRepo(database).GetByID(ctx, subID)
	if err != nil {
		return fmt.Errorf("reconcile unknown: load subscription %s: %w", subID, err)
	}

	// Backfill the provider's missing charges first (#634) so a renewed sub's
	// confirming payment exists before/with the status flip, and materialize the
	// provider customer id (#635) — shared with the #684 webhook converge path.
	backfilled, railCustomer, err := applyDecisionSideEffects(ctx, q, sub, d, now, lookbackCap)
	if err != nil {
		return fmt.Errorf("reconcile unknown: %w", err)
	}
	res.Backfilled += backfilled
	if railCustomer {
		res.RailCustomers++
	}

	if _, err := ApplyDecision(ctx, database, lc, sub, d, now); err != nil {
		return err
	}
	switch d.Kind {
	case TransitionRenew:
		res.Renewed++
	case TransitionAdoptPeriodEnd:
		res.Adopted++
	case TransitionPastDue:
		res.PastDue++
	case TransitionCancel:
		res.Cancelled++
	default: // TransitionNone: no conclusive evidence — stays unknown
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
		if currency == "" && sub.Price != nil {
			// #651: don't fabricate "usd". A decline/void carries no currency of its
			// own; fall back to the subscription's real billing currency (truthful).
			currency = sub.Price.Currency
		}
		if currency == "" {
			// Genuinely unknown currency (transaction and subscription both lack one):
			// skip + warn rather than guess.
			log.WithContext(ctx).WithFields(log.Fields{
				"transaction_id":  t.TransactionID,
				"subscription_id": sub.ID,
			}).Warn("reconcile backfill: transaction and subscription both lack a currency; skipping row")
			continue
		}
		subID := sub.ID
		// #684/#671: RemoteTransaction amounts are provider-wire CENTS; the
		// payments ledger is MICROS. Convert at this boundary, never store raw.
		amountMicros := moneyutil.CentsToMicros(t.AmountCents)
		n, err := q.CreatePaymentIfNotExists(ctx, gen.CreatePaymentIfNotExistsParams{
			ID:             uuid.New(),
			MerchantID:     sub.MerchantID,
			PriceID:        sub.PriceID,
			Rail:           string(sub.Rail),
			TransactionID:  t.TransactionID,
			Amount:         amountMicros,
			ListAmount:     amountMicros,
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
