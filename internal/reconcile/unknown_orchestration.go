package reconcile

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/opsmetric"
	"github.com/open-rails/openrails/pkg/merchant"
)

// UnknownReconcileOptions bounds one reconcile pass over the `unknown` cohort (#633).
type UnknownReconcileOptions struct {
	MaxPerRail    int           // cap subscriptions pulled per rail per run (0 -> 500)
	DunningWindow time.Duration // a failed renewal within this of the period end is recoverable (0 -> 14d)
	LookbackCap   time.Duration // never pull a window wider than this (#634 3y bound; 0 -> 3y)
	WindowSlack   time.Duration // pull from (oldest period end - slack) (0 -> 48h)
	// CancelBudget (#834/#837) caps how many of this cohort ONE pass may
	// cancel. Over the cap the rail applies NOTHING. Zero value = defaults.
	CancelBudget CancelBudget
	// Breaker (#834) refuses to read absence from an implausibly small roster.
	// Zero value = defaults.
	Breaker RosterBreaker
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
	Held          int                 // cancellations a pass-level guard withheld (#834)
	Backfilled    int                 // payments imported (#634)
	RailCustomers int                 // rail_customer_accounts materialized from a remote customer id (#635)
	RailErrors    map[Provider]string // rails that could not be pulled (their subs stay unknown; caller backs off)
}

// reconcilableRails returns the rails with PSPs (registry-driven,
// #669) in stable order; their reconcile Provider is the rail name itself
// (#630: mobius is a PSP on rail nmi, not a rail).
func reconcilableRails() []string {
	var out []string
	for _, d := range rails.All() {
		if d.HasPSPs {
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
	// #835: nothing this cohort holds that predates the deployment's first pull
	// may cancel anybody — an unknown row on an imported book carries inherited
	// history by definition.
	floor := EvidenceFloorFor(ctx, database, merchantID.UUID())

	for _, rail := range reconcilableRails() {
		provider := Provider(rail)
		fetcher := fetchers[provider]
		prober := probers[provider]
		railArg := rail
		rows, err := q.ListUnknownSubscriptions(ctx, gen.ListUnknownSubscriptionsParams{
			MerchantID: merchantID.UUID(), Rail: &railArg,
			MaxRows: int32(opts.MaxPerRail), // #nosec G115 -- withDefaults() clamps to 500 when <=0; only caller is the internal River worker, no HTTP path sets this
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

		// #834: the pass-level brakes this path never had. It had ONLY a 500-row
		// FETCH cap: every unknown row absent from the snapshot hit
		// `absent_from_exhaustive_roster` -> cancel + entitlement revoke, and
		// because those cancels carry RemoteGone=true they create NO provider
		// intent, so the #679 volume breaker could not see — let alone stop — a
		// single one of them.
		localLive, cerr := q.CountLiveLinkedSubscriptionsForRail(ctx, gen.CountLiveLinkedSubscriptionsForRailParams{
			MerchantID: merchantID.UUID(), Rail: rail,
		})
		if cerr != nil {
			return res, fmt.Errorf("reconcile unknown: count live %s book: %w", rail, cerr)
		}
		if snap != nil {
			remoteLive := len(snap.Subscriptions)
			tripped, reason := opts.Breaker.Implausible(provider, remoteLive, int(localLive))
			opsmetric.Emit(ctx, opsmetric.MetricRosterRatio, log.Fields{
				"provider": string(provider), "merchant_id": merchantID.String(), "rail": rail,
				"remote_live": remoteLive, "local_live": localLive,
				"ratio": ratioOf(remoteLive, int(localLive)), "tripped": tripped,
			})
			if tripped {
				// The roster is not believable, so it proves nothing about absence.
				// Strip the proof rather than abort: the rows stay `unknown`,
				// entitlements stay intact, and a per-sub probe can still resolve
				// them individually on real evidence.
				snap.Coverage.SubscriptionsExhaustive = false
				log.WithContext(ctx).WithFields(log.Fields{
					"merchant_id": merchantID.String(), "rail": rail,
					"remote_live": remoteLive, "local_live": localLive,
				}).Error("reconcile unknown: roster ratio breaker tripped; absence will not be read as cancellation")
				recordGuardFinding(ctx, q, merchantID, provider, "roster_ratio", reason)
			}
		}

		// Decide the WHOLE cohort before applying any of it, so the cancellation
		// cap sees the pass as a whole rather than one row at a time.
		type pendingDecision struct {
			id       uuid.UUID
			decision Decision
		}
		decisions := make([]pendingDecision, 0, len(rows))
		cancels := 0
		for _, r := range rows {
			state := SubscriptionState{
				Status:             string(models.StatusUnknown),
				Rail:               rail,
				RailSubscriptionID: r.RailSubscriptionID,
				PeriodEnd:          r.CurrentPeriodEndsAt,
			}
			decision := Decide(state, EvidenceBundle{Snapshot: snap, EvidenceFloor: floor}, now, opts.DunningWindow)
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
					decision = Decide(state, EvidenceBundle{Snapshot: psnap, EvidenceFloor: floor}, now, opts.DunningWindow)
				}
			}
			if decision.Kind == TransitionCancel {
				cancels++
			}
			if decision.EvidenceFloored {
				recordEvidenceStaleFinding(ctx, q, merchantID, provider, r.ID.String(), decision.Reason)
			}
			decisions = append(decisions, pendingDecision{id: r.ID, decision: decision})
		}

		// #837 all-or-nothing cap on the LOCAL cancel + entitlement revoke.
		exceeded, reason := opts.CancelBudget.Exceeded(cancels, int(localLive))
		opsmetric.Emit(ctx, opsmetric.MetricCancellationsPerPass, log.Fields{
			"provider": string(provider), "merchant_id": merchantID.String(), "rail": rail,
			"planned_cancellations": cancels, "local_live": localLive,
			"allowed": opts.CancelBudget.Limit(int(localLive)), "capped": exceeded,
			"path": "unknown_cohort",
		})
		if exceeded {
			log.WithContext(ctx).WithFields(log.Fields{
				"merchant_id": merchantID.String(), "rail": rail,
				"planned_cancellations": cancels, "local_live": localLive,
			}).Error("reconcile unknown: cancellation cap exceeded; applied nothing for this rail")
			recordGuardFinding(ctx, q, merchantID, provider, "cancellation_cap", reason)
			res.Held += cancels
			res.StillUnknown += len(rows)
			continue
		}

		for _, d := range decisions {
			if err := applyUnknownDecision(ctx, database, lc, q, d.id, d.decision, now, opts.LookbackCap, &res); err != nil {
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
		// CUR-6: this is a provider INGESTION boundary — Stripe reports currency
		// lower-case on the wire — and the value lands in payments.currency, so
		// it must be canonicalised here, not left as the rail wrote it.
		currency := money.NormalizeCurrency(t.Currency)
		// or#864 / CUR-9: a decline or void carries no currency of its own, and
		// the previous code borrowed the subscription's under a comment claiming
		// it did not fabricate. Both halves of that were wrong: the borrow IS a
		// substitution, and it was invisible on the row afterwards.
		//
		// The borrow stays — the roster reports transactions FOR this
		// subscription, so its billing currency is the one this attempt was
		// denominated in; that is a real relationship, not a guess — but it is
		// an INFERENCE, so the row says so. Metadata carries the provenance and
		// the log names it, which is the difference between an inference and a
		// fabrication: a reader can tell which one they are looking at.
		currencyInherited := false
		if currency == "" && sub.Price != nil {
			currency = money.NormalizeCurrency(sub.Price.Currency)
			currencyInherited = currency != ""
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
		amountMicros := int64(moneyutil.CentsToMicros(moneyutil.Cents(t.AmountCents)))
		params := gen.CreatePaymentIfNotExistsParams{
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
			// or#893: a charge belongs to the account that took it, which is the
			// account that owns the subscription it renewed.
			PspID: &sub.PspID,
			// or#827: a mirrored success IS money the rail moved; a mirrored
			// decline moved nothing and must never reach the host feed.
			MoneyMovement: string(models.MoneyMovementNone),
		}
		if t.Success {
			params.MoneyMovement = string(models.MoneyMovementRail)
		}
		if currencyInherited {
			params.Metadata = []byte(`{"currency_provenance":"inherited_from_subscription_price"}`)
			log.WithContext(ctx).WithFields(log.Fields{
				"transaction_id":  t.TransactionID,
				"subscription_id": sub.ID,
				"currency":        currency,
			}).Warn("reconcile backfill: transaction reported no currency; denominating the attempt in the subscription's billing currency and recording the inheritance as provenance (CUR-9)")
		}
		// #796: backfilled declines carry the rail's code VERBATIM so
		// approval_rate's failure_reason dimension sees them (attempt_kind
		// stays NULL — the mirror cannot distinguish initial vs renewal).
		if !t.Success {
			if code := strings.TrimSpace(t.DeclineCode); code != "" {
				reason := payments.NormalizeFailureReason(string(sub.Rail), code)
				params.FailureCode = &code
				params.FailureReason = &reason
			}
		}
		// Provider-driven: NMI can only rebill a card IT holds, so the custody
		// fact here is stated, not guessed (or#879).
		if tt := payments.DefaultTokenType(string(sub.Rail), models.CustodianPSP); tt != "" {
			params.TokenType = &tt
		}
		n, err := q.CreatePaymentIfNotExists(ctx, params)
		if err != nil {
			return inserted, fmt.Errorf("backfill payment %s: %w", t.TransactionID, err)
		}
		inserted += int(n)
	}
	return inserted, nil
}

// evidenceStaleAction is the operator prose for a floored cancel.
func evidenceStaleAction(reason string) string {
	return "the evidence for this terminal cancel (" + reason + ") predates this deployment's first pull of the merchant, or carries no date at all, " +
		"so nothing we observed corroborates it — it arrived with the imported book. The subscription is parked as `unknown` with its access intact. " +
		"Verify it against the provider (a per-subscription probe re-decides it on evidence THIS deployment observed) before cancelling anything"
}

// recordEvidenceStaleFinding persists the operator-facing record of a cancel the
// #835 staleness floor withheld. Best-effort, like every guard finding: the
// floor has already done its job in memory, and failing to write the record
// must never turn a SAFE outcome into an error that retries into an unsafe one.
func recordEvidenceStaleFinding(ctx context.Context, q *gen.Queries, merchantID merchant.ID, provider Provider, subscriptionID, reason string) {
	action := evidenceStaleAction(reason)
	if _, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
		MerchantID:        merchantID.UUID(),
		FindingType:       string(FindingEvidenceStale),
		SubjectKey:        evidenceStaleSubjectKey(provider, subscriptionID),
		Severity:          string(SeverityHigh),
		Status:            string(FindingStatusRequiresReview),
		RecommendedAction: &action,
	}); err != nil {
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"merchant_id": merchantID.String(), "rail": string(provider), "subscription_id": subscriptionID,
		}).Error("reconcile: could not persist the evidence-staleness finding (the floor still held; the row is parked `unknown`)")
	}
}

// evidenceStaleSubjectKey is the stable identity of a floored cancel: one row
// per (provider, subscription), so re-runs update rather than pile up.
func evidenceStaleSubjectKey(provider Provider, subscriptionID string) string {
	return string(provider) + ":subscription:" + subscriptionID
}

// recordGuardFinding persists the operator-facing record of a pass-level guard
// that withheld cancellations (#834). Best-effort: the guard has already done
// its job in memory, and failing to write the finding must never turn a SAFE
// outcome into an error that retries into an unsafe one.
func recordGuardFinding(ctx context.Context, q *gen.Queries, merchantID merchant.ID, provider Provider, subject, reason string) {
	if _, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
		MerchantID:        merchantID.UUID(),
		FindingType:       string(FindingCancellationCapped),
		SubjectKey:        string(provider) + ":unknown_cohort:" + subject,
		Severity:          string(SeverityCritical),
		Status:            string(FindingStatusRequiresReview),
		RecommendedAction: &reason,
	}); err != nil {
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"merchant_id": merchantID.String(), "rail": string(provider), "guard": subject,
		}).Error("reconcile unknown: could not persist the guard finding (the guard still held; investigate the roster)")
	}
}
