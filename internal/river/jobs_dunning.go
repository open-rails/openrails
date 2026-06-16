package riverjobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/analytics"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/processors"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/normalize"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const (
	QueueBilling = "billing"
	KindDunning  = "openrails.dunning"

	dunningAttemptLease = 15 * time.Minute
)

// dunningOutcome classifies what a dunning pass did with one subscription.
type dunningOutcome int

const (
	dunningOutcomeFailed dunningOutcome = iota
	dunningOutcomeSucceeded
	// dunningOutcomeWindowExpired: the missed rebill is older than the dunning
	// window — the subscription was cancelled + downgraded without a charge.
	dunningOutcomeWindowExpired
	// dunningOutcomeMaterialized (#366, mode=limited): the charge decision was
	// recorded as a parked system-origin intent on the ledger instead of
	// executed; the scheduled executor drains it at mode=full.
	dunningOutcomeMaterialized
)

// DunningArgs triggers a dunning run that processes all due past_due subscriptions.
type DunningArgs struct{}

func (DunningArgs) Kind() string { return KindDunning }

// DunningWorker queries all past_due subscriptions where next_retry_at is in the past
// and attempts to rebill them via NMI. It processes each subscription inline and
// updates the database after each attempt for idempotency.
//
// Behavior is controlled by config.FeatureFlags.DunningMode:
//   - "on": Normal dunning - query due subscriptions and attempt charges
//   - "dry_run_only": Query due subscriptions but skip charges (preserves state)
//   - "off": Skip entirely (FailMembership handles immediate cancellation)
type DunningWorker struct {
	river.WorkerDefaults[DunningArgs]
	DB                 *db.DB
	Config             *config.Config
	Clock              clockwork.Clock
	NMIClients         map[string]*nmi.NMIClient
	EventLogService    *analytics.EventLogService
	IdempotencyService *idempotency.IdempotencyService
	// DeferDelete schedules the processor-side delete for terminal
	// cancellations (#344). Threaded into the per-run lifecycle so window
	// expiry and retry exhaustion stop the remote NMI subscription via
	// the ONE scheduled mechanism (kill-switch governed at execution). nil in
	// producer-less wirings/tests: cancellation still happens, the remote sub
	// is left for reconciliation.
	DeferDelete subscriptions.DeferredDeleteScheduler
	// Intents executes the provider-side charge through the intent ledger
	// (#358 phase C): the worker enqueues a manual_rebill intent and runs it
	// synchronously through the identical gate/execute/classify pipeline,
	// driving lifecycle off the returned status. nil builds a Runner over the
	// worker's own dependencies.
	Intents *intents.Runner
}

// intentRunner returns the configured Runner or self-assembles one (direct
// worker constructions in tests). Config is only attached when non-nil — a
// typed-nil ModeView would defeat the gate's nil check.
func (w *DunningWorker) intentRunner() *intents.Runner {
	if w.Intents != nil {
		return w.Intents
	}
	fingerprints := intents.NewRuntimeFingerprints(w.Config, w.NMIClients)
	runner := &intents.Runner{
		Store: intents.NewStore(w.DB).WithFingerprints(fingerprints),
		Registry: intents.NewRegistry(
			intents.NewManualRebillHandler(w.DB, w.Config, w.NMIClients, w.Clock, w.EventLogService),
		),
		Clock:        w.Clock,
		Fingerprints: fingerprints,
	}
	if w.Config != nil {
		runner.Config = w.Config
	}
	return runner
}

func (DunningWorker) Kind() string { return KindDunning }

// now returns the current time from the worker's clock
func (w *DunningWorker) now() time.Time {
	if w.Clock != nil {
		return w.Clock.Now()
	}
	return time.Now()
}

func (w *DunningWorker) Work(ctx context.Context, job *river.Job[DunningArgs]) error {
	// Check dunning mode from feature flags
	dunningMode := config.DunningModeOn
	if w.Config != nil {
		dunningMode = w.Config.GetDunningMode()
	}

	// Mode handling (#345/#366). Dunning is a PROACTIVE operation, so provider
	// charges never fire outside mode=full — but the SCAN still runs under
	// limited and MATERIALIZES its decisions (#366): window-expired subs are
	// cancelled + downgraded locally NOW (the no-charge path; local writes are
	// limited-legal, and the lifecycle respects the entitlement-expiration
	// kill switch), and in-window charges are enqueued as parked system-origin
	// intents the ledger executor drains at mode=full. That makes a freshly
	// migrated backlog VISIBLE in `billing intents` instead of implicit in
	// subscription rows. Readonly stays a pure observer: demote to
	// dry_run_only exactly as before (forensics boots must not mutate state).
	materialize := false
	if w.Config != nil && dunningMode == config.DunningModeOn {
		switch {
		case w.Config.IsProviderReadOnly():
			log.WithContext(ctx).Warn("Readonly mode: dunning demoted to dry_run_only (no charges, no cancellations, no intents)")
			dunningMode = config.DunningModeDryRunOnly
		case w.Config.IsLimitedMode():
			materialize = true
			log.WithContext(ctx).Warn("Limited mode: dunning materializes decisions — local window-expiry cancellations apply, charge intents enqueue PARKED (no provider writes until mode=full)")
		}
	}

	// If dunning is completely off, skip - FailMembership handles immediate cancellation
	if dunningMode == config.DunningModeOff {
		log.WithContext(ctx).Info("Dunning mode is 'off'; skipping dunning run (FailMembership handles immediate cancellation)")
		return nil
	}

	if w.NMIClients == nil {
		log.WithContext(ctx).Warn("NMI clients not configured; skipping dunning run")
		return nil
	}

	// Query all due past_due NMI-backed subscriptions
	// Use w.now() instead of SQL NOW() to support time mocking in tests
	nmiProcessors := processors.GetNMIBackedProcessorsList()
	dueSubscriptions, err := repo.NewSubscriptionRepo(w.DB).ListDueDunningSubscriptions(ctx, nmiProcessors, w.now())
	if err != nil {
		return fmt.Errorf("query due subscriptions: %w", err)
	}

	if len(dueSubscriptions) == 0 {
		log.WithContext(ctx).Debug("Dunning: no subscriptions due for retry")
		return nil
	}

	// If dry_run_only mode, log the subscriptions but don't process them
	// This preserves retry counts and next_retry_at for when dunning is re-enabled
	if dunningMode == config.DunningModeDryRunOnly {
		log.WithContext(ctx).WithField("count", len(dueSubscriptions)).
			Warn("Dunning mode is 'dry_run_only'; found due subscriptions but skipping charges")
		log.WithContext(ctx).Info("   Subscriptions remain in past_due state with retry counts preserved")
		log.WithContext(ctx).Info("   Set feature_flags.dunning_mode=on to resume charging")
		return nil
	}

	log.WithContext(ctx).WithField("count", len(dueSubscriptions)).Info("Dunning: processing due subscriptions")

	// Build services once for all attempts
	priceSvc := catalog.NewPriceService(w.DB)
	productSvc := catalog.NewProductService(w.DB)
	entitlementSvc := entitlements.NewEntitlementService(w.DB, w.Clock)
	notifSvc := subscriptions.NewNotificationService(w.DB, nil)
	paymentSvc := payments.NewPaymentService(w.DB, w.Clock)
	lifecycle := subscriptions.NewSubscriptionLifecycleService(w.DB, productSvc, priceSvc, entitlementSvc, notifSvc, paymentSvc, w.EventLogService, w.Clock)
	lifecycle.SetConfig(w.Config) // For feature flag access
	if w.DeferDelete != nil {
		// Terminal cancellations (window expiry, retry exhaustion) schedule
		// the remote NMI delete through the shared mechanism (#344).
		lifecycle.SetDeferredDeleteScheduler(w.DeferDelete)
	}
	moneySvc := money.NewMoneyService(w.DB, w.Clock)

	successCount := 0
	failCount := 0
	windowExpiredCount := 0
	materializedCount := 0

	for _, sub := range dueSubscriptions {
		// #336: pin the subscription's tenant so writes in processSubscription
		// (payment inserts, lifecycle updates) carry the app.tenant_id GUC.
		outcome := dunningOutcomeFailed
		if err := w.DB.RunInMerchantConn(merchant.WithID(ctx, merchant.ID(sub.MerchantID)), func(tctx context.Context) error {
			outcome = w.processSubscription(tctx, &sub, lifecycle, priceSvc, moneySvc, materialize)
			return nil
		}); err != nil {
			log.WithContext(ctx).WithError(err).WithField("subscription_id", sub.ID).
				Error("Dunning: failed to pin tenant connection; counting subscription as failed")
		}
		switch outcome {
		case dunningOutcomeSucceeded:
			successCount++
		case dunningOutcomeWindowExpired:
			windowExpiredCount++
		case dunningOutcomeMaterialized:
			materializedCount++
		default:
			failCount++
		}
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"total":          len(dueSubscriptions),
		"success":        successCount,
		"failed":         failCount,
		"window_expired": windowExpiredCount,
		"materialized":   materializedCount,
	}).Info("Dunning: run completed")

	return nil
}

// processSubscription attempts a dunning rebill for a single subscription.
func (w *DunningWorker) processSubscription(
	ctx context.Context,
	sub *models.Subscription,
	lifecycle *subscriptions.SubscriptionLifecycleService,
	priceSvc *catalog.PriceService,
	moneySvc *money.MoneyService,
	materialize bool,
) dunningOutcome {
	logEntry := log.WithContext(ctx).WithField("subscription_id", sub.ID)

	provider := resolveSubscriptionProcessor(sub)
	client := w.NMIClients[provider] // may be nil; only required for charging

	if sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.IsZero() {
		logEntry.Warn("Dunning: past_due subscription has no current period end; skipping rebill")
		return dunningOutcomeFailed
	}

	periodEnd := sub.CurrentPeriodEndsAt.UTC()
	processor := models.Processor(provider)

	// Dunning staleness window (#344, #359): charges are only attempted within
	// the window DERIVED from the price's billing cycle (last retry offset +
	// one day of slack — see subscriptions.DunningWindow). Anything
	// older (e.g. months-stale subscriptions imported from a legacy system)
	// must never be surprise-charged — cancel + downgrade instead, and the
	// processor-side subscription is stopped via the scheduled deferred-delete
	// mechanism so NMI quits retrying it. A sub-4-day cycle derives a ZERO
	// window: any past_due daily sub is immediately terminal here.
	cycleDays := subscriptions.BillingCycleDaysOf(sub.Price)
	if cycleDays <= 0 && priceSvc != nil {
		if p, err := priceSvc.GetByID(ctx, sub.PriceID); err == nil {
			cycleDays = subscriptions.BillingCycleDaysOf(p)
		}
	}
	if cycleDays <= 0 {
		logEntry.WithField("price_id", sub.PriceID).
			Warn("Dunning: subscription has no billing cycle (one-time price?); using monthly dunning window")
	}
	window := subscriptions.DunningWindow(cycleDays)
	if w.now().UTC().After(periodEnd.Add(window)) {
		return w.expireWindowedSubscription(ctx, logEntry, sub, lifecycle, processor, periodEnd, window)
	}

	if client == nil {
		logEntry.WithField("processor", provider).Warn("NMI client not configured for provider; skipping")
		return dunningOutcomeFailed
	}

	orderReference := rebillOrderReference(sub)
	if orderReference == "" {
		logEntry.Warn("Dunning: unable to build rebill order reference; skipping rebill")
		return dunningOutcomeFailed
	}

	// Materialize (#366, mode=limited): record the charge decision as a
	// parked intent and stop — no claim (claiming writes last_retry_at, which
	// is dunning FORENSIC evidence and must keep its imported value until a
	// real attempt), no payment-method failure policy, no lifecycle movement.
	// The ledger executor drains the intent at mode=full; its relevance
	// re-check and the ExpiresAt dunning-window bound guarantee a parked
	// charge never fires stale.
	if materialize {
		genSub, err := w.DB.Gen(ctx).GetSubscriptionByID(ctx, sub.ID)
		if err != nil {
			logEntry.WithError(err).Warn("Dunning (materialize): failed to load subscription tenant for rebill intent")
			return dunningOutcomeFailed
		}
		attemptOrdinal := 0
		if sub.RetryAttempts != nil {
			attemptOrdinal = *sub.RetryAttempts
		}
		windowEnd := periodEnd.Add(window)
		row, err := w.intentRunner().Store.Enqueue(ctx, intents.EnqueueParams{
			MerchantID:     genSub.MerchantID,
			Provider:       provider,
			IntentType:     intents.TypeManualRebill,
			SubscriptionID: &sub.ID,
			Payload: intents.ManualRebillPayload{
				SubscriptionID: sub.ID,
				PeriodEnd:      periodEnd,
				Processor:      provider,
				OrderReference: orderReference,
				Attempt:        attemptOrdinal,
			},
			IdempotencyKey: intents.ManualRebillIdempotencyKey(sub.ID, periodEnd, provider, orderReference, attemptOrdinal),
			NextAttemptAt:  w.now().UTC(),
			Origin:         intents.OriginSystem,
			OriginReason:   "dunning rebill attempt (materialized under mode=limited)",
			ExpiresAt:      &windowEnd,
		})
		if err != nil {
			logEntry.WithError(err).Warn("Dunning (materialize): failed to enqueue rebill intent")
			return dunningOutcomeFailed
		}
		logEntry.WithFields(log.Fields{
			"intent_id":     row.ID,
			"intent_status": row.Status,
			"window_end":    windowEnd,
		}).Info("Dunning (materialize): charge intent on the ledger; executor drains it when the mode allows")
		return dunningOutcomeMaterialized
	}

	claimed, err := w.claimDunningAttempt(ctx, sub, w.now())
	if err != nil {
		logEntry.WithError(err).Warn("Dunning: failed to claim subscription for rebill")
		return dunningOutcomeFailed
	}
	if !claimed {
		logEntry.Info("Dunning: subscription was already claimed or no longer due")
		return dunningOutcomeFailed
	}

	// Validate payment method before involving the ledger: nothing chargeable
	// exists, so no provider mutation is recorded — straight to the failure
	// policy, exactly as before.
	pm := sub.PaymentMethod
	if pm == nil || pm.VaultID == "" || pm.BillingID == nil || *pm.BillingID == "" {
		reason := "payment method unavailable for rebill"
		if err := lifecycle.FailMembership(ctx, &subscriptions.FailMembershipParams{
			Processor:      processor,
			SubscriptionID: &sub.ID,
			FailureReason:  &reason,
		}); err != nil {
			logEntry.WithError(err).Warn("fail-membership after missing payment method")
		}
		return dunningOutcomeFailed
	}

	// The provider-side charge flows through the intent ledger (#358 phase C):
	// one system-origin intent per (subscription, period end, attempt
	// ordinal), executed synchronously through the identical pipeline. The
	// enqueue's conflict semantics replace the retired manual_rebill_attempts
	// claim — a crash between charge and lifecycle update re-derives the same
	// key and gets the durable outcome back instead of double-charging.
	genSub, err := w.DB.Gen(ctx).GetSubscriptionByID(ctx, sub.ID)
	if err != nil {
		logEntry.WithError(err).Warn("Dunning: failed to load subscription tenant for rebill intent")
		return dunningOutcomeFailed
	}
	attemptOrdinal := 0
	if sub.RetryAttempts != nil {
		attemptOrdinal = *sub.RetryAttempts
	}
	windowEnd := periodEnd.Add(window)
	intent, err := w.intentRunner().EnqueueAndExecute(ctx, intents.EnqueueParams{
		MerchantID:     genSub.MerchantID,
		Provider:       provider,
		IntentType:     intents.TypeManualRebill,
		SubscriptionID: &sub.ID,
		Payload: intents.ManualRebillPayload{
			SubscriptionID: sub.ID,
			PeriodEnd:      periodEnd,
			Processor:      provider,
			OrderReference: orderReference,
			Attempt:        attemptOrdinal,
		},
		IdempotencyKey: intents.ManualRebillIdempotencyKey(sub.ID, periodEnd, provider, orderReference, attemptOrdinal),
		NextAttemptAt:  w.now().UTC(),
		Origin:         intents.OriginSystem,
		OriginReason:   "dunning rebill attempt",
		ExpiresAt:      &windowEnd,
	})
	if err != nil {
		logEntry.WithError(err).Warn("Dunning: failed to enqueue durable manual rebill intent")
		return dunningOutcomeFailed
	}
	logEntry = logEntry.WithField("intent_id", intent.ID)

	switch intent.Status {
	case intents.StatusSucceeded:
		// The handler's finalize renews the membership; if the renewal raced
		// or this is a durable success from an earlier crashed pass (enqueue
		// conflict), repair the local lifecycle here.
		txnID := manualRebillEvidenceString(intent, "transaction_id")
		if refreshed, rerr := w.DB.Gen(ctx).GetSubscriptionByID(ctx, sub.ID); rerr == nil &&
			models.SubscriptionStatus(refreshed.Status) == models.StatusPastDue && txnID != "" {
			logEntry.Warn("Dunning: repairing local lifecycle from durable successful rebill intent")
			return w.applySuccessfulRebill(ctx, logEntry, sub, lifecycle, priceSvc, moneySvc, processor, txnID)
		}
		logEntry.Info("Dunning: rebill successful")
		return dunningOutcomeSucceeded

	case intents.StatusFailedTerminal:
		// Declined (now, or durably on an earlier pass — the evidence carries
		// the response code either way): classify and apply the failure
		// policy. The next scheduled retry derives a fresh attempt ordinal.
		return w.applyDeclinedRebill(ctx, logEntry, sub, lifecycle, processor, intent)

	case intents.StatusUnknownNeedsVerify:
		// Exactly the old markManualRebillUnknown posture: no lifecycle
		// change, no next retry scheduled for this attempt; the intent
		// verifier resolves it via the NMI Query API, and on late-confirmed
		// success the handler's finalize repairs the lifecycle.
		logEntry.Warn("Dunning: manual rebill status unknown; verifier will resolve via provider reads (no further automatic charge for this attempt)")
		return dunningOutcomeFailed

	case intents.StatusPending, intents.StatusInFlight, intents.StatusFailedRetryable:
		// Parked (mode gate, unconfigured client) or owned by another
		// executor; the scheduled executor drains it — or the relevance
		// window expires it if the blocker outlasts the dunning window.
		logEntry.WithField("reason", normalize.FromPtr(intent.LastFailureReason)).
			Info("Dunning: rebill intent not executed this pass; ledger executor will drain it")
		return dunningOutcomeFailed

	default: // superseded, expired
		logEntry.WithField("intent_status", intent.Status).
			Info("Dunning: rebill intent no longer applicable")
		return dunningOutcomeFailed
	}
}

// applyDeclinedRebill applies the failure policy for a terminally-failed
// rebill intent, classifying hard/soft off the decline evidence recorded on
// the ledger.
func (w *DunningWorker) applyDeclinedRebill(
	ctx context.Context,
	logEntry *log.Entry,
	sub *models.Subscription,
	lifecycle *subscriptions.SubscriptionLifecycleService,
	processor models.Processor,
	intent gen.OpenrailsProviderIntent,
) dunningOutcome {
	reason := normalize.FromPtr(intent.LastFailureReason)
	if reason == "" {
		reason = "rebill declined"
	}
	responseCode := manualRebillEvidenceResponseCode(intent)
	hardDecline := subscriptions.ClassifyNMIDecline(responseCode) == subscriptions.DeclineHard
	var failureCode *string
	if responseCode != 0 {
		code := fmt.Sprintf("%d", responseCode)
		failureCode = &code
	}

	declineLog := logEntry.WithFields(log.Fields{
		"response_code": responseCode,
		"reason":        reason,
		"hard_decline":  hardDecline,
	})
	if hardDecline {
		// Emit a high-visibility signal: a hard decline permanently terminates
		// the subscription rather than retrying.
		declineLog.Error("Dunning: hard decline; terminating subscription without further retries")
	} else {
		declineLog.Warn("Dunning: soft decline; will retry on schedule")
	}

	if err := lifecycle.FailMembership(ctx, &subscriptions.FailMembershipParams{
		Processor:      processor,
		SubscriptionID: &sub.ID,
		FailureReason:  &reason,
		FailureCode:    failureCode,
		HardDecline:    hardDecline,
	}); err != nil {
		logEntry.WithError(err).Warn("apply failure policy after declined rebill")
	}
	return dunningOutcomeFailed
}

// manualRebillEvidenceString reads one string field off the intent's
// result_evidence.
func manualRebillEvidenceString(intent gen.OpenrailsProviderIntent, key string) string {
	if len(intent.ResultEvidence) == 0 {
		return ""
	}
	var evidence map[string]any
	if err := json.Unmarshal(intent.ResultEvidence, &evidence); err != nil {
		return ""
	}
	s, _ := evidence[key].(string)
	return s
}

// manualRebillEvidenceResponseCode reads the gateway decline code off the
// intent's result_evidence (0 when absent — classified soft).
func manualRebillEvidenceResponseCode(intent gen.OpenrailsProviderIntent) int {
	if len(intent.ResultEvidence) == 0 {
		return 0
	}
	var evidence map[string]any
	if err := json.Unmarshal(intent.ResultEvidence, &evidence); err != nil {
		return 0
	}
	if code, ok := evidence["response_code"].(float64); ok {
		return int(code)
	}
	return 0
}

// expireWindowedSubscription handles a past_due subscription whose missed rebill
// is older than the dunning window: terminal cancellation + entitlement
// revocation WITHOUT a charge. The processor-side delete is NOT performed
// inline — FailMembership(Terminal) persists the deletion marker and schedules
// the deferred NMI delete through the one shared mechanism (#344), which the
// deletion kill switch governs at execution time.
func (w *DunningWorker) expireWindowedSubscription(
	ctx context.Context,
	logEntry *log.Entry,
	sub *models.Subscription,
	lifecycle *subscriptions.SubscriptionLifecycleService,
	processor models.Processor,
	periodEnd time.Time,
	window time.Duration,
) dunningOutcome {
	reason := fmt.Sprintf("dunning window expired: rebill was due %s, window is %s", periodEnd.Format(time.RFC3339), window)
	if err := lifecycle.FailMembership(ctx, &subscriptions.FailMembershipParams{
		Processor:      processor,
		SubscriptionID: &sub.ID,
		FailureReason:  &reason,
		Terminal:       true,
	}); err != nil {
		logEntry.WithError(err).Error("Dunning: failed to cancel window-expired subscription")
		return dunningOutcomeFailed
	}

	logEntry.WithFields(log.Fields{
		"period_end": periodEnd,
		"window":     window.String(),
	}).Warn("Dunning: window expired; subscription cancelled and downgraded without charge")
	return dunningOutcomeWindowExpired
}

func (w *DunningWorker) applySuccessfulRebill(
	ctx context.Context,
	logEntry *log.Entry,
	sub *models.Subscription,
	lifecycle *subscriptions.SubscriptionLifecycleService,
	priceSvc *catalog.PriceService,
	moneySvc *money.MoneyService,
	processor models.Processor,
	transactionID string,
) dunningOutcome {
	var amount int64
	currency := subscriptions.CurrencyUSD
	if sub.Price != nil {
		amount = sub.Price.Amount
		currency = sub.Price.Currency
	} else if p, err := priceSvc.GetByID(ctx, sub.PriceID); err == nil {
		amount = p.Amount
		currency = p.Currency
	}

	// Success: renew membership window and persist payment in the lifecycle flow.
	if err := lifecycle.RenewMembership(ctx, &subscriptions.RenewMembershipParams{
		Processor:               processor,
		ProcessorSubscriptionID: sub.ProcessorSubscriptionID,
		TransactionID:           transactionID,
		Amount:                  amount,
		Currency:                currency,
	}); err != nil {
		logEntry.WithError(err).Error("renew membership after successful rebill")
		return dunningOutcomeFailed
	}

	if moneySvc != nil {
		if updated, err := w.DB.Gen(ctx).GetSubscriptionByID(ctx, sub.ID); err != nil {
			logEntry.WithError(err).Warn("load subscription after rebill for credit grants")
		} else if updated.CurrentPeriodEndsAt != nil && !updated.CurrentPeriodEndsAt.IsZero() {
			if err := moneySvc.GrantSubscriptionCredits(ctx, money.GrantSubscriptionCreditsParams{
				SubscriptionID: sub.ID,
				PeriodEnd:      updated.CurrentPeriodEndsAt.UTC(),
				Cadence:        models.CreditGrantCadencePerRenewal,
				Source:         "subscription_renewal",
			}); err != nil {
				logEntry.WithError(err).Warn("grant subscription credits after successful rebill")
			}
		}
	}

	logEntry.Info("Dunning: rebill successful")
	return dunningOutcomeSucceeded
}

func (w *DunningWorker) claimDunningAttempt(ctx context.Context, sub *models.Subscription, now time.Time) (bool, error) {
	if w == nil || w.DB == nil || sub == nil {
		return false, errors.New("dunning worker database and subscription are required")
	}

	claimedAt := now.UTC()
	leaseUntil := claimedAt.Add(dunningAttemptLease)
	rowsAffected, err := w.DB.Gen(ctx).ClaimDunningAttempt(ctx, gen.ClaimDunningAttemptParams{
		ID: sub.ID, LeaseUntil: leaseUntil, ClaimedAt: claimedAt,
	})
	if err != nil {
		return false, fmt.Errorf("read dunning claim result: %w", err)
	}
	if rowsAffected == 0 {
		return false, nil
	}

	sub.LastRetryAt = &claimedAt
	sub.NextRetryAt = &leaseUntil
	return true, nil
}

func rebillOrderReference(sub *models.Subscription) string {
	if sub == nil || sub.CurrentPeriodEndsAt == nil {
		return ""
	}
	return fmt.Sprintf("rebill-%s-%d", sub.ID, sub.CurrentPeriodEndsAt.UTC().Unix())
}

func resolveSubscriptionProcessor(sub *models.Subscription) string {
	if sub == nil {
		return ""
	}

	// Use processor field directly
	if p := normalizeProcessor(sub.Processor); p != "" {
		return p
	}
	if sub.PaymentMethod != nil {
		if p := normalizeProcessor(sub.PaymentMethod.Processor); p != "" {
			return p
		}
	}
	return ""
}

func normalizeProcessor(value interface{}) string {
	switch v := value.(type) {
	case *string:
		if v == nil {
			return ""
		}
		return normalizeProcessor(*v)
	case string:
		return normalize.Lower(v)
	case models.Processor:
		// Subscription.Processor and PaymentMethod.Processor are the named type
		// models.Processor (type Processor string), which does NOT match `case
		// string` in a Go type switch. Without this case resolveSubscriptionProcessor
		// returns "" for every subscription, so NMIClients[""] is nil and NMI/Mobius
		// dunning rebills are silently skipped (caught by the dunning integration test).
		return normalize.Lower(string(v))
	default:
		return ""
	}
}
