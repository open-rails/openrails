package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/opsmetric"
)

// UnknownReconcileOptions bounds one reconcile pass over the `unknown` cohort.
type UnknownReconcileOptions struct {
	MaxPerRail    int           // cap subscriptions pulled per rail per run (0 -> 500)
	DunningWindow time.Duration // a failed renewal within this of the period end is recoverable (0 -> 14d)
	LookbackCap   time.Duration // never pull a window wider than this (0 -> 3y)
	WindowSlack   time.Duration // pull from (oldest period end - slack) (0 -> 48h)
	// CancelBudget caps how many of this cohort one pass may cancel. Over the
	// cap the rail applies nothing. Zero value = defaults.
	CancelBudget CancelBudget
	// Breaker refuses to read absence from an implausibly small roster. Zero
	// value = defaults.
	Breaker RosterBreaker
	// SkipRails are rails another plane verifies (the NMI Verifier).
	SkipRails []string
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
	Adopted       int // remote period end adopted without a charge
	PastDue       int
	Canceled      int
	StillUnknown  int
	Probed        int                 // per-subscription probe fallbacks attempted
	Held          int                 // cancellations a pass-level guard withheld
	Backfilled    int                 // payments imported
	RailCustomers int                 // psp_customers materialized from a remote customer id
	RailErrors    map[Provider]string // rails that could not be pulled (their subs stay unknown; caller backs off)
}

// reconcilableRails returns the rails with PSPs in stable order; each rail
// name is its reconcile Provider.
func reconcilableRails() []string {
	var out []string
	for _, d := range rails.All() {
		if d.HasPSPs {
			out = append(out, string(d.Rail))
		}
	}
	return out
}

// ReconcileUnknownCohort resolves the `unknown` subscription cohort against
// provider truth with one windowed bulk fetch per rail, probing a single
// subscription only when the bulk snapshot cannot decide it. Per rail it pulls
// [oldest period end - slack, now] (clamped to LookbackCap), runs each row
// through Decide and ApplyDecision, and backfills missing charges idempotently.
// A rail whose fetch fails is recorded in RailErrors and its rows stay unknown:
// the River caller backs off, and no probes fan out against that rail. Must
// run merchant-scoped.
func ReconcileUnknownCohort(ctx context.Context, database *db.DB, lc *subscriptions.SubscriptionLifecycleService, fetchers map[Provider]RailFetcher, probers map[Provider]SubscriptionProber, merchantID billing.MerchantID, now time.Time, opts UnknownReconcileOptions) (UnknownReconcileResult, error) {
	opts = opts.withDefaults()
	res := UnknownReconcileResult{RailErrors: map[Provider]string{}}
	q := database.Gen(ctx)
	// Nothing this cohort holds that predates the deployment's first pull may
	// cancel anybody: an unknown row on an imported book carries inherited
	// history by definition.
	floor := EvidenceFloorFor(ctx, database, merchantID.UUID())

	for _, rail := range reconcilableRails() {
		if slices.Contains(opts.SkipRails, rail) {
			continue
		}
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
			snap, err = fetcher.Fetch(ctx, FetchParams{Since: since, Until: now, ObservedAt: now})
			if err != nil {
				// Provider unreachable: subs stay unknown, caller backs off.
				res.RailErrors[provider] = err.Error()
				res.StillUnknown += len(rows)
				continue
			}
		}

		// Pass-level brakes: an unknown row absent from an exhaustive snapshot
		// cancels with RemoteGone and creates no provider intent, so the
		// intents volume breaker never sees it.
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
				Status:             string(models.StatusUnverified),
				Rail:               rail,
				RailSubscriptionID: models.DerefStr(r.RailSubscriptionID),
				PeriodStart:        r.CurrentPeriodStartsAt,
				PeriodEnd:          r.CurrentPeriodEndsAt,
			}
			decision := Decide(state, EvidenceBundle{Snapshot: snap, EvidenceFloor: floor}, now, opts.DunningWindow)
			// NMI's bulk transaction report names no schedule, so the bulk
			// snapshot can adopt a boundary but never prove its renewal
			// charge; any non-terminal NMI answer is confirmed by the
			// per-subscription probe, which reads charges by schedule id.
			needsProbe := decision.Kind == TransitionNone || (provider == ProviderNMI && decision.Kind == TransitionAdoptPeriodEnd)
			if needsProbe && prober != nil && r.RailSubscriptionID != nil {
				// The bulk window couldn't decide this row: one targeted probe,
				// fed to the same decider. A probe failure keeps the row
				// unknown (retried next pass).
				res.Probed++
				if psnap, perr := prober.ProbeSubscription(ctx, ProbeSubject{
					LocalID: r.ID, RailSubscriptionID: models.DerefStr(r.RailSubscriptionID), PeriodStart: r.CurrentPeriodStartsAt, PeriodEnd: r.CurrentPeriodEndsAt, ObservedAt: now,
				}); perr != nil {
					log.WithContext(ctx).WithError(perr).WithFields(log.Fields{
						"subscription_id": r.ID, "rail": rail,
					}).Warn("reconcile unknown: per-subscription probe failed; staying unknown")
					res.StillUnknown++
					continue
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

		// All-or-nothing cap on the local cancel + entitlement revoke.
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

	// Backfill the provider's missing charges first, so a renewed sub's
	// confirming payment lands with the status flip, and materialize the
	// provider customer id.
	backfilled, railCustomer, err := applyDecisionSideEffects(ctx, database, sub, d, now, lookbackCap)
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
		res.Canceled++
	default: // TransitionNone: no conclusive evidence — stays unknown
		res.StillUnknown++
	}
	return nil
}

// backfillSubscriptionPayments imports a subscription's provider-observed
// successful charges we are missing, idempotent by transaction id (ON CONFLICT
// DO NOTHING). Declines are attempts, not payments; refunds and chargebacks
// are never charge rows. Bounded to lookbackCap. Returns the count inserted.
func backfillSubscriptionPayments(ctx context.Context, database *db.DB, sub *models.Subscription, txns []RemoteTransaction, now time.Time, lookbackCap time.Duration) (int, error) {
	inserted := 0
	// Oldest first: schedule receipts settle cycles in the order they ran.
	order := make([]int, len(txns))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		x, y := txns[order[a]], txns[order[b]]
		if !x.OccurredAt.Equal(y.OccurredAt) {
			return x.OccurredAt.Before(y.OccurredAt)
		}
		return x.TransactionID < y.TransactionID
	})
	for _, i := range order {
		t := &txns[i]
		if t.TransactionID == "" || t.OccurredAt.Before(now.Add(-lookbackCap)) {
			continue
		}
		if t.StripeBill == nil && !chargeAttempt(t.Type) {
			if t.Type == TransactionTypeRefund || t.Type == TransactionTypeChargeback {
				recordUnlinkedReversalFinding(ctx, database.Gen(ctx), sub, *t)
			}
			continue
		}
		// A Stripe decline is attempt history; its open invoice is the bill.
		if t.StripeBill == nil && t.Type == TransactionTypeDecline && sub.Rail == models.RailStripe {
			continue
		}
		var result observed.Result
		err := database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			d := database.NewWithPgxTx(tx)
			current, err := subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, sub.ID)
			if err != nil {
				return err
			}
			if t.StripeBill != nil {
				bill, _, err := subscriptions.StripeBillObservation(ctx, d, current, *t.StripeBill)
				if err != nil {
					return err
				}
				result, err = subscriptions.RecordStripeBill(ctx, d, current, *t.StripeBill, now)
				if err == nil {
					t.ObservedBill = &bill
				}
				return err
			}
			currency := money.NormalizeCurrency(t.Currency)
			if currency == "" {
				return fmt.Errorf("provider receipt %s has no billed currency", t.TransactionID)
			}
			amount, err := t.nativeIn(currency)
			if err != nil {
				return err
			}
			var next time.Time
			if t.NextBillingAt != nil {
				next = *t.NextBillingAt
			}
			bill, err := observed.ScheduleBill(ctx, d, current, observed.ScheduleReceipt{TransactionID: t.TransactionID, Amount: amount,
				Currency: currency, OccurredAt: t.OccurredAt, NextBillingAt: next, Approved: t.Success})
			if err != nil {
				return err
			}
			if t.Success {
				bill.Payment = &models.Payment{CustomerID: sub.CustomerID, SubscriptionID: &sub.ID, PspID: &sub.PspID, Rail: sub.Rail,
					TransactionID: t.TransactionID, Amount: amount, ListAmount: amount, Currency: currency, Status: payments.PaymentStatusSucceededValue,
					MoneyMovement: models.MoneyMovementRail, PurchasedAt: t.OccurredAt, CreatedAt: now, Metadata: t.PaymentMetadata}
				bill.Settled, bill.SettlementSource = true, "cash"
			}
			result, err = observed.Record(ctx, d, bill, now)
			if err == nil {
				t.ObservedBill = &bill
			}
			return err
		})
		if err != nil {
			unresolved := &UnresolvedReceiptError{TransactionID: t.TransactionID, Err: err}
			if findingErr := recordUnresolvedReceipt(ctx, database.Gen(ctx), sub, unresolved); findingErr != nil {
				return inserted, fmt.Errorf("retain unresolved provider bill: %w", findingErr)
			}
			return inserted, unresolved
		}
		if result.PaymentCreated {
			inserted++
		}
	}
	return inserted, nil
}

// UnresolvedReceiptError is a provider receipt no accepted bill can hold. It
// stays a durable finding; no collection or entitlement is inferred from it.
type UnresolvedReceiptError struct {
	TransactionID string
	Err           error
}

func (e *UnresolvedReceiptError) Error() string {
	return fmt.Sprintf("provider bill receipt %s unresolved: %v", e.TransactionID, e.Err)
}

func (e *UnresolvedReceiptError) Unwrap() error { return e.Err }

func recordUnresolvedReceipt(ctx context.Context, q *gen.Queries, sub *models.Subscription, e *UnresolvedReceiptError) error {
	evidence, _ := json.Marshal(map[string]any{"transaction_id": e.TransactionID, "subscription_id": billing.SubscriptionID(sub.ID).String(), "error": e.Err.Error()})
	action := "Resolve the accepted provider bill or cycle evidence, then replay this observation. No collection or entitlement was inferred."
	_, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{MerchantID: sub.MerchantID,
		PspID: &sub.PspID, FindingType: "pull.bill.unresolved", SubjectKey: "receipt:" + e.TransactionID,
		Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: evidence})
	return err
}

// chargeAttempt reports whether a mirrored transaction is a charge attempt
// (a sale, or its decline) — the only kinds a payment row records.
func chargeAttempt(t TransactionType) bool {
	return t == "" || t == TransactionTypeSale || t == TransactionTypeDecline
}

// recordUnlinkedReversalFinding surfaces a refund or chargeback the mirror
// could not attach to its original sale. Best-effort, like every finding.
func recordUnlinkedReversalFinding(ctx context.Context, q *gen.Queries, sub *models.Subscription, t RemoteTransaction) {
	action := fmt.Sprintf("a %s of %s (transaction %s) was reported for subscription %s without its original sale; record it against the sale it reverses", t.Type, t.formatAmount(), t.TransactionID, billing.SubscriptionID(sub.ID).String())
	if _, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
		MerchantID:        sub.MerchantID,
		FindingType:       string(FindingReversalUnlinked),
		SubjectKey:        string(sub.Rail) + ":" + t.TransactionID,
		Severity:          string(SeverityHigh),
		Status:            string(FindingStatusRequiresReview),
		RecommendedAction: &action,
	}); err != nil {
		log.WithContext(ctx).WithError(err).WithField("transaction_id", t.TransactionID).Error("reconcile backfill: could not persist the unlinked reversal finding")
	}
}

// evidenceStaleAction is the operator prose for a floored cancel.
func evidenceStaleAction(reason string) string {
	return "the evidence for this terminal cancel (" + reason + ") predates this deployment's first pull of the merchant, or carries no date at all, " +
		"so nothing we observed corroborates it — it arrived with the imported book. The subscription is parked as `unknown` with its access intact. " +
		"Verify it against the provider (a per-subscription probe re-decides it on evidence THIS deployment observed) before cancelling anything"
}

// recordEvidenceStaleFinding persists the operator-facing record of a cancel
// the staleness floor withheld. Best-effort: the floor already acted in
// memory, and a failed write must never turn a safe outcome into an error that
// retries into an unsafe one.
func recordEvidenceStaleFinding(ctx context.Context, q *gen.Queries, merchantID billing.MerchantID, provider Provider, subscriptionID, reason string) {
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
// that withheld cancellations. Best-effort: the guard already acted in memory,
// and a failed write must never turn a safe outcome into an error that retries
// into an unsafe one.
func recordGuardFinding(ctx context.Context, q *gen.Queries, merchantID billing.MerchantID, provider Provider, subject, reason string) {
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
