package riverjobs

import (
	"context"
	"fmt"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/analytics"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/processors"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const KindSubscriptionLiveness = "openrails.subscription_liveness"

// livenessOutcome classifies what a liveness pass did with one subscription.
type livenessOutcome int

const (
	// livenessOutcomeUnreachable: the provider read failed (or no client is
	// configured); state is untouched and the next state-scan pass re-derives
	// the same cohort and retries — the deliberate alternative to a durable
	// read-queue (the intent ledger stays mutations-only by design).
	livenessOutcomeUnreachable livenessOutcome = iota
	// livenessOutcomeRepaired: the provider DID charge the lapsed period (we
	// missed the webhook); the membership was renewed off the verified
	// transaction — entitlements + payment backfilled, never a second charge.
	livenessOutcomeRepaired
	// livenessOutcomeFailed: the provider declined (or the remote record is
	// stalled past-due); the subscription was routed into dunning, which owns
	// it from here (#359 retry schedule, staleness window, window-expiry
	// cancel) — or cancelled immediately on a hard decline.
	livenessOutcomeFailed
	// livenessOutcomeAdopted: no charge was attempted but the remote record
	// is alive with a future next billing date (provider clock misalignment);
	// the remote period end was adopted locally. Period adoption alone never
	// grants access — entitlements extend only through a real charge.
	livenessOutcomeAdopted
	// livenessOutcomeCancelled: the remote subscription no longer exists
	// (NMI deletes cancelled records from the recurring report; Stripe
	// reports canceled) — cancelled locally + entitlements revoked. No remote
	// delete is needed: it is already gone.
	livenessOutcomeCancelled
	// livenessOutcomeSkipped: not probeable this pass (missing period end,
	// stripe prober unavailable, unknown processor).
	livenessOutcomeSkipped
)

// SubscriptionLivenessArgs triggers a liveness pass over the silent-lapsed
// cohort.
type SubscriptionLivenessArgs struct{}

func (SubscriptionLivenessArgs) Kind() string { return KindSubscriptionLiveness }

// SubscriptionLivenessWorker (#367) is the scheduled provider-truth prober
// for SILENT lapsed subscriptions: status='active' rows whose period passed
// with NO provider signal either way (no rebill-success, no rebill-failure
// webhook). It reaches out to the provider for THAT subscription's state and
// converges local state through the normal lifecycle services, regardless of
// how stale the gap is:
//
//	charged   -> RenewMembership repair (payment + entitlement backfill)
//	declined  -> FailMembership (+ #359 schedule) — dunning owns it from here
//	no attempt, remote alive w/ future next billing -> adopt the remote period
//	remote absent/terminal -> cancel locally + revoke entitlements
//
// It is also #368's RESOLVER: every resolution revokes/replaces the renewal
// grace window that bridged the silence (charged-repair via RenewMembership's
// revoke-then-push, the dead outcomes via FailMembership's grace revocation).
//
// STALENESS SAFETY: this worker only READS providers and writes local state —
// it never charges. Charging decisions stay inside dunning, whose derived
// staleness window (#344/#359) cancels months-stale subscriptions instead of
// surprise-charging them.
//
// MODE GATING: probes are reads, convergence is local writes — runs under
// full AND limited (consistent with #366 materialize); SKIPS under readonly
// (forensics boots stay pure observers).
//
// PROVIDERS: NMI-backed (query.php per subscription — the whole point) and
// Stripe (GET /v1/subscriptions/{id} + latest invoice). CCBill is excluded
// (no per-record API; the 6h DataLink bulk worker is its version of this);
// Solana is excluded (the cranker is pull-based; there were never webhooks).
type SubscriptionLivenessWorker struct {
	river.WorkerDefaults[SubscriptionLivenessArgs]
	DB              *db.DB
	Config          *config.Config
	Clock           clockwork.Clock
	NMIClients      map[string]*nmi.NMIClient
	EventLogService *analytics.EventLogService
	// DeferDelete schedules the processor-side delete when a probe outcome
	// terminally cancels an NMI-backed subscription that still exists
	// remotely (hard decline). Routed through the one shared #344 mechanism
	// (intent-ledger governed); nil leaves the remote sub for reconciliation.
	DeferDelete subscriptions.DeferredDeleteScheduler
	// StripeProber probes remote Stripe subscriptions. nil self-assembles
	// from Config when Stripe is configured; injectable for tests.
	StripeProber subscriptions.StripeLivenessProber
}

func (SubscriptionLivenessWorker) Kind() string { return KindSubscriptionLiveness }

func (w *SubscriptionLivenessWorker) now() time.Time {
	if w.Clock != nil {
		return w.Clock.Now()
	}
	return time.Now()
}

// nmiAlignmentSlack widens the charge probe's date window below the local
// period end: NMI bills on its own day boundary, so the renewal sale for the
// next period can land up to a day before our period-end instant.
const nmiAlignmentSlack = 24 * time.Hour

func (w *SubscriptionLivenessWorker) Work(ctx context.Context, job *river.Job[SubscriptionLivenessArgs]) error {
	// Readonly mode is a pure observer everywhere (#346): even though the
	// probes are reads, convergence is local writes — skip entirely.
	if w.Config != nil && w.Config.IsProviderReadOnly() {
		log.WithContext(ctx).Warn("Readonly mode: subscription liveness sync skipped (pure observer; no local convergence)")
		return nil
	}

	// Cohort: NMI-backed + Stripe subscriptions, still 'active', whose period
	// lapsed past the probe slack with no payment recorded for the boundary.
	// past_due rows are dunning's, excluded by the query. Re-derived every
	// pass, so unreachable providers are retried for free.
	cohortProcessors := append(processors.GetNMIBackedProcessorsList(), string(models.ProcessorStripe))
	cutoff := w.now().UTC().Add(-subscriptions.LivenessProbeSlack)
	cohort, err := repo.NewSubscriptionRepo(w.DB).ListSilentLapsed(ctx, cohortProcessors, cutoff)
	if err != nil {
		return fmt.Errorf("query silent lapsed subscriptions: %w", err)
	}
	if len(cohort) == 0 {
		log.WithContext(ctx).Debug("Liveness: no silent lapsed subscriptions")
		return nil
	}

	// Build services once for all subscriptions (same construction block as
	// the dunning worker).
	priceSvc := catalog.NewPriceService(w.DB)
	productSvc := catalog.NewProductService(w.DB)
	entitlementSvc := entitlements.NewEntitlementService(w.DB, w.Clock)
	notifSvc := subscriptions.NewNotificationService(w.DB, nil)
	paymentSvc := payments.NewPaymentService(w.DB, w.Clock)
	lifecycle := subscriptions.NewSubscriptionLifecycleService(w.DB, productSvc, priceSvc, entitlementSvc, notifSvc, paymentSvc, w.EventLogService, w.Clock)
	lifecycle.SetConfig(w.Config)
	if w.DeferDelete != nil {
		// Hard-decline terminal cancellations stop the remote NMI
		// subscription via the shared scheduled mechanism (#344).
		lifecycle.SetDeferredDeleteScheduler(w.DeferDelete)
	}
	// A second lifecycle WITHOUT the delete scheduler for remote-absent
	// outcomes: the remote subscription is already gone, so scheduling an
	// nmi_delete intent for it would only generate verify noise.
	lifecycleRemoteGone := subscriptions.NewSubscriptionLifecycleService(w.DB, productSvc, priceSvc, entitlementSvc, notifSvc, paymentSvc, w.EventLogService, w.Clock)
	lifecycleRemoteGone.SetConfig(w.Config)
	moneySvc := money.NewMoneyService(w.DB, w.Clock)

	deps := &livenessDeps{
		lifecycle:           lifecycle,
		lifecycleRemoteGone: lifecycleRemoteGone,
		priceSvc:            priceSvc,
		productSvc:          productSvc,
		moneySvc:            moneySvc,
	}

	log.WithContext(ctx).WithField("count", len(cohort)).Info("Liveness: probing silent lapsed subscriptions")

	var repaired, failed, adopted, cancelled, unreachable, skipped int
	for i := range cohort {
		// #336: pin the subscription's tenant so the lifecycle convergence
		// writes (renew/fail/adopt) carry the app.tenant_id GUC.
		outcome := livenessOutcomeUnreachable
		if err := w.DB.RunInTenantConn(merchant.WithID(ctx, merchant.ID(cohort[i].MerchantID)), func(tctx context.Context) error {
			outcome = w.processSubscription(tctx, &cohort[i], deps)
			return nil
		}); err != nil {
			log.WithContext(ctx).WithError(err).WithField("subscription_id", cohort[i].ID).
				Error("Liveness: failed to pin tenant connection; counting subscription as unreachable")
		}
		switch outcome {
		case livenessOutcomeRepaired:
			repaired++
		case livenessOutcomeFailed:
			failed++
		case livenessOutcomeAdopted:
			adopted++
		case livenessOutcomeCancelled:
			cancelled++
		case livenessOutcomeSkipped:
			skipped++
		default:
			unreachable++
		}
	}

	// Alert-style pass summary: the silence cohort now has an automated
	// owner, and this line is its heartbeat.
	log.WithContext(ctx).WithFields(log.Fields{
		"cohort":      len(cohort),
		"repaired":    repaired,
		"failed":      failed,
		"adopted":     adopted,
		"cancelled":   cancelled,
		"unreachable": unreachable,
		"skipped":     skipped,
	}).Info("Liveness: pass completed")
	return nil
}

// livenessDeps bundles the per-pass service set.
type livenessDeps struct {
	lifecycle           *subscriptions.SubscriptionLifecycleService
	lifecycleRemoteGone *subscriptions.SubscriptionLifecycleService
	priceSvc            *catalog.PriceService
	productSvc          *catalog.ProductService
	moneySvc            *money.MoneyService
}

func (w *SubscriptionLivenessWorker) processSubscription(ctx context.Context, sub *models.Subscription, deps *livenessDeps) livenessOutcome {
	logEntry := log.WithContext(ctx).WithField("subscription_id", sub.ID)

	if sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.IsZero() {
		logEntry.Warn("Liveness: silent lapsed subscription has no period end; skipping")
		return livenessOutcomeSkipped
	}
	periodEnd := sub.CurrentPeriodEndsAt.UTC()

	if sub.Processor == models.ProcessorStripe {
		return w.probeStripe(ctx, logEntry, sub, periodEnd, deps)
	}

	provider := resolveSubscriptionProcessor(sub)
	if provider == "" || !processors.IsNMIBackedProcessor(models.Processor(provider)) {
		logEntry.WithField("processor", string(sub.Processor)).Warn("Liveness: unsupported processor in cohort; skipping")
		return livenessOutcomeSkipped
	}
	client := w.NMIClients[provider]
	if client == nil {
		logEntry.WithField("processor", provider).Warn("Liveness: NMI client not configured; will retry next pass")
		return livenessOutcomeUnreachable
	}
	return w.probeNMI(ctx, logEntry, sub, models.Processor(provider), client, periodEnd, deps)
}

// probeNMI runs the two read-only NMI lookups and converges:
// (a) the period's sale transactions by order reference (the local
// subscription id, stamped as orderid/ponumber at signup and inherited by
// every recurring rebill), (b) the remote recurring record.
func (w *SubscriptionLivenessWorker) probeNMI(
	ctx context.Context,
	logEntry *log.Entry,
	sub *models.Subscription,
	processor models.Processor,
	client *nmi.NMIClient,
	periodEnd time.Time,
	deps *livenessDeps,
) livenessOutcome {
	probe, err := client.ProbeSalesByOrderID(sub.ID.String(), periodEnd.Add(-nmiAlignmentSlack))
	if err != nil {
		logEntry.WithError(err).Warn("Liveness: NMI transaction probe failed; will retry next pass")
		return livenessOutcomeUnreachable
	}

	if probe.SuccessFound && probe.SuccessTransactionID != "" {
		// (a) Charged: NMI billed the period and we missed the webhook.
		// Repair through the same renewal path as dunning's verified-existing-
		// sale repair: payment backfilled exactly once (CreateIfNotExists on
		// the transaction id), entitlements granted, grace revoked + renewed.
		return w.repairChargedPeriod(ctx, logEntry, sub, deps, &subscriptions.RenewMembershipParams{
			Processor:               processor,
			ProcessorSubscriptionID: sub.ProcessorSubscriptionID,
			TransactionID:           probe.SuccessTransactionID,
		})
	}

	if probe.DeclineFound {
		// (b) Declined: the renewal was attempted and failed; we missed the
		// failure webhook. Route into dunning, which owns it from here — its
		// retry schedule, staleness window and window-expiry cancel all apply
		// unchanged. Hard declines cancel immediately, same as dunning.
		reason := probe.DeclineReason
		if reason == "" {
			reason = "silent lapse: renewal sale declined at provider"
		}
		hardDecline := subscriptions.ClassifyNMIDecline(probe.DeclineResponseCode) == subscriptions.DeclineHard
		var failureCode *string
		if probe.DeclineResponseCode != 0 {
			code := fmt.Sprintf("%d", probe.DeclineResponseCode)
			failureCode = &code
		}
		if err := deps.lifecycle.FailMembership(ctx, &subscriptions.FailMembershipParams{
			Processor:      processor,
			SubscriptionID: &sub.ID,
			FailureReason:  &reason,
			FailureCode:    failureCode,
			HardDecline:    hardDecline,
		}); err != nil {
			logEntry.WithError(err).Error("Liveness: fail-membership after verified decline")
			return livenessOutcomeUnreachable
		}
		logEntry.WithFields(log.Fields{
			"response_code": probe.DeclineResponseCode,
			"hard_decline":  hardDecline,
		}).Warn("Liveness: verified renewal decline; subscription routed into dunning")
		return livenessOutcomeFailed
	}

	// No charge attempt found for the period — ask the recurring report.
	liveness, err := client.GetRecurringLiveness(sub.ProcessorSubscriptionID)
	if err != nil {
		logEntry.WithError(err).Warn("Liveness: NMI recurring probe failed; will retry next pass")
		return livenessOutcomeUnreachable
	}

	if !liveness.Found {
		// Remote absent: NMI deletes cancelled subscriptions from the
		// recurring report entirely, so absence IS terminal. Cancel locally +
		// revoke entitlements (no remote delete needed — it is already gone).
		reason := "silent lapse: remote recurring subscription no longer exists at provider"
		if err := deps.lifecycleRemoteGone.FailMembership(ctx, &subscriptions.FailMembershipParams{
			Processor:      processor,
			SubscriptionID: &sub.ID,
			FailureReason:  &reason,
			Terminal:       true,
		}); err != nil {
			logEntry.WithError(err).Error("Liveness: cancel after remote-absent probe")
			return livenessOutcomeUnreachable
		}
		logEntry.Warn("Liveness: remote subscription absent; cancelled locally and revoked entitlements")
		return livenessOutcomeCancelled
	}

	now := w.now().UTC()
	if !liveness.NextChargeDate.IsZero() && liveness.NextChargeDate.After(now) {
		// Never attempted, remote alive with a FUTURE next billing date: the
		// date-misalignment case (NMI thinks July 1 where we think June 30).
		// Adopt the remote period end so the local expectation re-anchors.
		// Adoption alone never grants access — entitlement windows extend
		// only through the renewal-shaped path when a charge actually exists.
		return w.adoptRemotePeriodEnd(ctx, logEntry, sub, deps, liveness.NextChargeDate)
	}

	// Remote alive but its own charge date is stalled in the past: NMI
	// stopped advancing it (an attempt failed without surfacing, or the
	// record is wedged). Route into dunning — its in-window manual rebill
	// collects what is due, and its staleness window cancels anything older
	// without a charge.
	reason := "silent lapse: remote recurring record stalled (no charge attempt found, next charge date not in the future)"
	if err := deps.lifecycle.FailMembership(ctx, &subscriptions.FailMembershipParams{
		Processor:      processor,
		SubscriptionID: &sub.ID,
		FailureReason:  &reason,
	}); err != nil {
		logEntry.WithError(err).Error("Liveness: fail-membership after stalled remote record")
		return livenessOutcomeUnreachable
	}
	logEntry.WithField("next_charge_date", liveness.NextChargeDate).
		Warn("Liveness: remote recurring record stalled; subscription routed into dunning")
	return livenessOutcomeFailed
}

// probeStripe converges a silent lapsed Stripe subscription off one expanded
// GET. Stripe runs its own dunning and retries webhooks for days, so this
// mostly repairs long outages.
func (w *SubscriptionLivenessWorker) probeStripe(
	ctx context.Context,
	logEntry *log.Entry,
	sub *models.Subscription,
	periodEnd time.Time,
	deps *livenessDeps,
) livenessOutcome {
	prober := w.stripeProber()
	if prober == nil {
		logEntry.Warn("Liveness: stripe prober unavailable (stripe not configured); skipping")
		return livenessOutcomeSkipped
	}
	rec, err := prober.ProbeSubscription(ctx, sub.ProcessorSubscriptionID)
	if err != nil {
		logEntry.WithError(err).Warn("Liveness: stripe probe failed; will retry next pass")
		return livenessOutcomeUnreachable
	}

	switch {
	case !rec.Found, rec.Status == "canceled", rec.Status == "incomplete_expired":
		reason := fmt.Sprintf("silent lapse: stripe subscription terminal at provider (status=%s, found=%t)", rec.Status, rec.Found)
		if err := deps.lifecycleRemoteGone.FailMembership(ctx, &subscriptions.FailMembershipParams{
			Processor:      models.ProcessorStripe,
			SubscriptionID: &sub.ID,
			FailureReason:  &reason,
			Terminal:       true,
		}); err != nil {
			logEntry.WithError(err).Error("Liveness: cancel after terminal stripe probe")
			return livenessOutcomeUnreachable
		}
		logEntry.WithField("stripe_status", rec.Status).Warn("Liveness: stripe subscription terminal; cancelled locally and revoked entitlements")
		return livenessOutcomeCancelled

	case rec.Status == "past_due", rec.Status == "unpaid", rec.Status == "incomplete", rec.Status == "paused":
		// Stripe is dunning it (or it never completed); mirror the failure
		// locally so access converges with provider truth. Stripe owns the
		// retries — the local past_due only gates entitlement renewal.
		reason := fmt.Sprintf("silent lapse: stripe subscription %s at provider", rec.Status)
		if err := deps.lifecycle.FailMembership(ctx, &subscriptions.FailMembershipParams{
			Processor:      models.ProcessorStripe,
			SubscriptionID: &sub.ID,
			FailureReason:  &reason,
		}); err != nil {
			logEntry.WithError(err).Error("Liveness: fail-membership after stripe dunning-state probe")
			return livenessOutcomeUnreachable
		}
		logEntry.WithField("stripe_status", rec.Status).Warn("Liveness: stripe subscription past due at provider; mirrored locally")
		return livenessOutcomeFailed

	case rec.Status == "active" || rec.Status == "trialing":
		if rec.CurrentPeriodEnd.After(periodEnd) && rec.LatestInvoicePaid && rec.LatestInvoiceTransactionID != "" {
			// Charged: Stripe advanced the period on a paid invoice and we
			// missed invoice.paid. Repair with the remote period and the
			// webhook-compatible transaction id, so a late webhook dedupes.
			return w.repairChargedPeriod(ctx, logEntry, sub, deps, &subscriptions.RenewMembershipParams{
				Processor:               models.ProcessorStripe,
				ProcessorSubscriptionID: sub.ProcessorSubscriptionID,
				CurrentPeriodStartsAt:   zeroTimePtrLiveness(rec.CurrentPeriodStart),
				CurrentPeriodEndsAt:     zeroTimePtrLiveness(rec.CurrentPeriodEnd),
				TransactionID:           rec.LatestInvoiceTransactionID,
				Amount:                  rec.LatestInvoiceAmountPaid,
				AmountProvided:          true,
				Currency:                rec.LatestInvoiceCurrency,
			})
		}
		if rec.CurrentPeriodEnd.After(w.now().UTC()) {
			// Alive with a future period end but no paid invoice covering it
			// (clock misalignment / unbilled extension): adopt the remote
			// period end without granting access.
			return w.adoptRemotePeriodEnd(ctx, logEntry, sub, deps, rec.CurrentPeriodEnd)
		}
		logEntry.WithFields(log.Fields{
			"stripe_status":     rec.Status,
			"remote_period_end": rec.CurrentPeriodEnd,
		}).Warn("Liveness: stripe subscription active but remote period also stale; leaving for next pass")
		return livenessOutcomeUnreachable

	default:
		logEntry.WithField("stripe_status", rec.Status).Warn("Liveness: unrecognized stripe status; leaving for next pass")
		return livenessOutcomeUnreachable
	}
}

// repairChargedPeriod renews the membership off a verified provider charge —
// the same path as dunning's verified-existing-sale repair. Amount/currency
// default to the subscription's price when the caller has no invoice
// evidence. Idempotent: RenewMembership's CreateIfNotExists on the
// transaction id makes the payment backfill exactly-once, and a concurrent
// webhook landing first turns this into a no-op.
func (w *SubscriptionLivenessWorker) repairChargedPeriod(
	ctx context.Context,
	logEntry *log.Entry,
	sub *models.Subscription,
	deps *livenessDeps,
	params *subscriptions.RenewMembershipParams,
) livenessOutcome {
	if !params.AmountProvided && params.Amount == 0 {
		params.Currency = subscriptions.CurrencyUSD
		if sub.Price != nil {
			params.Amount = sub.Price.Amount
			params.Currency = sub.Price.Currency
		} else if p, err := deps.priceSvc.GetByID(ctx, sub.PriceID); err == nil {
			params.Amount = p.Amount
			params.Currency = p.Currency
		}
	}

	if err := deps.lifecycle.RenewMembership(ctx, params); err != nil {
		logEntry.WithError(err).Error("Liveness: renew membership after verified provider charge")
		return livenessOutcomeUnreachable
	}

	// Per-renewal credits mirror the dunning worker's post-renew grant:
	// failures are logged, never fatal — the renewal is the money part.
	if deps.moneySvc != nil {
		if updated, err := w.DB.Gen(ctx).GetSubscriptionByID(ctx, sub.ID); err != nil {
			logEntry.WithError(err).Warn("Liveness: load subscription after repair for credit grants")
		} else if updated.CurrentPeriodEndsAt != nil && !updated.CurrentPeriodEndsAt.IsZero() {
			if err := deps.moneySvc.GrantSubscriptionCredits(ctx, money.GrantSubscriptionCreditsParams{
				SubscriptionID: sub.ID,
				PeriodEnd:      updated.CurrentPeriodEndsAt.UTC(),
				Cadence:        models.CreditGrantCadencePerRenewal,
				Source:         "subscription_renewal",
			}); err != nil {
				logEntry.WithError(err).Warn("Liveness: grant subscription credits after repair")
			}
		}
	}

	logEntry.WithField("transaction_id", params.TransactionID).
		Warn("Liveness: provider charge verified for silent period; membership repaired (payment + entitlements backfilled)")
	return livenessOutcomeRepaired
}

// adoptRemotePeriodEnd is the PS-3-equivalent local write: re-anchor
// current_period_ends_at to the provider's own clock so the silence detector
// stops firing until the provider's real billing moment passes. No
// entitlement windows are touched — adoption alone never grants access.
func (w *SubscriptionLivenessWorker) adoptRemotePeriodEnd(
	ctx context.Context,
	logEntry *log.Entry,
	sub *models.Subscription,
	deps *livenessDeps,
	remoteEnd time.Time,
) livenessOutcome {
	subSvc := subscriptions.NewSubscriptionService(w.DB, deps.priceSvc, deps.productSvc, nil, nil, nil, w.Clock)
	fresh, err := subSvc.GetByID(ctx, sub.ID)
	if err != nil {
		logEntry.WithError(err).Error("Liveness: load subscription for period adoption")
		return livenessOutcomeUnreachable
	}
	if fresh.Status != models.StatusActive {
		logEntry.Info("Liveness: subscription moved out of active during pass; skipping period adoption")
		return livenessOutcomeSkipped
	}
	oldEnd := fresh.CurrentPeriodEndsAt
	adopted := remoteEnd.UTC()
	fresh.CurrentPeriodEndsAt = &adopted
	if err := subSvc.Update(ctx, fresh); err != nil {
		logEntry.WithError(err).Error("Liveness: persist adopted remote period end")
		return livenessOutcomeUnreachable
	}
	logEntry.WithFields(log.Fields{
		"old_period_end":     oldEnd,
		"adopted_period_end": adopted,
	}).Warn("Liveness: adopted remote period end (provider clock misalignment); no access granted by adoption")
	return livenessOutcomeAdopted
}

// stripeProber returns the injected prober or self-assembles one from config
// (nil when Stripe is not configured).
func (w *SubscriptionLivenessWorker) stripeProber() subscriptions.StripeLivenessProber {
	if w.StripeProber != nil {
		return w.StripeProber
	}
	if w.Config == nil {
		return nil
	}
	prober, err := subscriptions.NewStripeLivenessProber(w.Config)
	if err != nil {
		return nil
	}
	w.StripeProber = prober
	return prober
}

func zeroTimePtrLiveness(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
