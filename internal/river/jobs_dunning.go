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
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/replaycache"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/open-rails/openrails/internal/shared/normalize"
	"github.com/open-rails/openrails/internal/shared/progress"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const (
	QueueBilling = "billing"
	KindDunning  = "openrails.dunning"

	// DuePassInterval is the due pass cadence: the bound on how long an
	// engine renewal waits past its paid-period boundary.
	DuePassInterval = time.Minute

	// dunningMerchantBatch caps how many merchants one pass fans out to. The
	// work queue is indexed on the due-dunning predicate, so this bounds a pass
	// by ACTIVITY, never by the size of the merchant directory.
	dunningMerchantBatch = 500
)

// dunningOutcome classifies what a dunning pass did with one subscription.
type dunningOutcome int

const (
	dunningOutcomeFailed dunningOutcome = iota
	dunningOutcomeSucceeded
	// dunningOutcomeWindowExpired: the missed rebill is older than the dunning
	// window — the charge was skipped and the subscription PARKED as unknown
	// for provider verification (#839; it is never cancelled here).
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
type DunningWorker struct {
	river.WorkerDefaults[DunningArgs]
	DB     *db.DB
	Config *config.Config
	Clock  clockwork.Clock
	// NMIResolver arms store-scoped NMI clients per merchant (#730/#788, the
	// ONLY credential plane). Consulted at the charge gate; the rebill
	// handler re-resolves at charge time (no caching).
	NMIResolver        money.NMIClientResolver
	IdempotencyService *replaycache.Store
	// DeferDelete schedules the rail-side delete for terminal cancellations
	// (#344). Threaded into the per-run lifecycle so an evidence-backed terminal
	// decline stops the remote NMI subscription via the ONE scheduled mechanism
	// (kill-switch governed at execution). nil in producer-less wirings/tests:
	// cancellation still happens, the remote sub is left for reconciliation.
	DeferDelete subscriptions.DeferredDeleteScheduler
	// Intents executes the provider-side charge through the intent ledger
	// (#358 phase C): the worker enqueues a manual_rebill intent and runs it
	// synchronously through the identical gate/execute/classify pipeline,
	// driving lifecycle off the returned status. nil builds a Runner over the
	// worker's own dependencies.
	Intents *intents.Runner
	// EngineCollections admits stored engine obligations; the existing
	// provider-intent fleet performs the accepted charge and receipt recovery.
	EngineCollections *money.MoneyService
}

// intentRunner returns the configured Runner or self-assembles one (direct
// worker constructions in tests). Config is only attached when non-nil: since
// or#865 a nil ModeView fails CLOSED, and a typed-nil interface would read as
// readonly rather than panic — either way every rebill would park silently.
func (w *DunningWorker) intentRunner() *intents.Runner {
	if w.Intents != nil {
		return w.Intents
	}
	handler := intents.NewManualRebillHandler(w.DB, w.Config, w.NMIResolver, w.Clock)
	handler.DeferDelete = w.DeferDelete
	runner := &intents.Runner{
		Store:    intents.NewStore(w.DB),
		Registry: intents.NewRegistry(handler),
		Clock:    w.Clock,
	}
	if w.Config != nil {
		runner.Config = w.Config
	}
	return runner
}

// storeArmsNMI reports whether the merchant-secrets store can arm an NMI
// client for the subscription's account (#730). Resolver errors count as
// armable: a declared-but-unarmable account must reach the ledger and park
// with its loud fail-closed reason, not vanish in a silent skip.
func (w *DunningWorker) storeArmsNMI(ctx context.Context, sub *models.Subscription) bool {
	if w.NMIResolver == nil {
		return false
	}
	_, ok, err := w.NMIResolver.ResolveNMIClient(ctx, sub.MerchantID, &sub.PspID)
	return ok || err != nil
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
	// Mode handling (#345/#366). Dunning is a PROACTIVE operation, so provider
	// charges never fire outside mode=full — but the SCAN still runs under
	// limited and MATERIALIZES its decisions (#366): stale subs are parked as
	// `unknown` locally NOW, and in-window charges are enqueued as parked
	// system-origin intents the ledger executor drains at mode=full. That makes
	// a freshly migrated backlog VISIBLE in `openrails intents` instead of
	// implicit in subscription rows. #839: limited mode no longer performs LOCAL
	// terminal cancellations either — nothing on this path cancels without a
	// charge. Readonly stays a pure observer: no charges, no state moves, no
	// intents.
	materialize := false
	observeOnly := false
	if w.Config != nil {
		switch {
		case w.Config.IsProviderReadOnly():
			observeOnly = true
			log.WithContext(ctx).Warn("Readonly mode: dunning observes due subscriptions only (no charges, no cancellations, no intents)")
		case w.Config.IsLimitedMode():
			materialize = true
			log.WithContext(ctx).Warn("Limited mode: dunning materializes decisions — stale subscriptions park as unknown (no local cancellations), charge intents enqueue PARKED (no provider writes until mode=full)")
		}
	}

	if w.NMIResolver == nil && w.EngineCollections == nil {
		log.WithContext(ctx).Warn("NMI client resolver not configured; skipping dunning run")
		return nil
	}

	// or#877 B5: the due scan used to run on the job's BARE context.
	// subscriptions FORCEs RLS, so under openrails_app it matched
	// `merchant_id = NULL` — an empty slice, a "no subscriptions due" debug line
	// and a successful return, every four hours since the worker shipped. The
	// per-subscription RunInMerchantConn below it never executed because the
	// loop it lived in never had a row. Scheduled dunning — retries, #839
	// staleness parking, #840 terminal handling — had therefore never run at
	// all. Enumerate the merchants with due work through migration 0023's
	// SECURITY DEFINER work queue (ids only; it RAISES if its definer cannot
	// bypass RLS), then scan and charge inside each merchant's own scope.
	// Use w.now() instead of SQL NOW() to support time mocking in tests.
	nmiRails := []string{string(models.RailNMI)}
	if w.EngineCollections != nil {
		nmiRails = append(nmiRails, string(models.RailStripe))
	}
	merchantIDs, err := w.DB.GenDirectory().ListDueDunningMerchants(ctx, gen.ListDueDunningMerchantsParams{
		Rails: nmiRails, Now: w.now(), MerchantLimit: dunningMerchantBatch, IncludeEngine: w.EngineCollections != nil,
	})
	if err != nil {
		return fmt.Errorf("query merchants with due subscriptions: %w", err)
	}
	if len(merchantIDs) == 0 {
		log.WithContext(ctx).Debug("Dunning: no subscriptions due for retry")
		return nil
	}

	// Build services once for all attempts
	priceSvc := catalog.NewPriceService(w.DB)
	productSvc := catalog.NewProductService(w.DB)
	entitlementSvc := entitlements.NewEntitlementService(w.DB, w.Clock)
	notifSvc := subscriptions.NewNotificationService(w.DB, nil)
	paymentSvc := payments.NewPaymentService(w.DB, w.Clock)
	lifecycle := subscriptions.NewSubscriptionLifecycleService(w.DB, productSvc, priceSvc, entitlementSvc, notifSvc, paymentSvc, w.Clock)
	lifecycle.SetConfig(w.Config)

	total := 0
	successCount := 0
	failCount := 0
	windowExpiredCount := 0
	materializedCount := 0
	var workErr error

	for _, mid := range merchantIDs {
		if mid == nil {
			continue
		}
		merchantID := merchant.ID(*mid)
		progress.Mark(ctx, "dunning merchant "+merchantID.String())
		// The pin AND the proof it took: every read and write below runs under
		// this merchant's app.merchant_id, exactly as a request would.
		if err := w.DB.RunInMerchantScope(ctx, merchantID, "dunning pass", func(mctx context.Context) error {
			dueSubscriptions, err := subscriptions.NewSubscriptionRepo(w.DB).ListDueDunningSubscriptions(mctx, nmiRails, w.now(), w.EngineCollections != nil)
			if err != nil {
				return fmt.Errorf("query due subscriptions: %w", err)
			}
			total += len(dueSubscriptions)
			if len(dueSubscriptions) == 0 {
				return nil
			}
			if observeOnly {
				log.WithContext(mctx).WithField("count", len(dueSubscriptions)).
					Warn("Readonly mode: found due subscriptions but skipping dunning mutations")
				return nil
			}
			log.WithContext(mctx).WithFields(log.Fields{
				"count": len(dueSubscriptions), "merchant_id": merchantID.String(),
			}).Info("Dunning: processing due subscriptions")

			var merchantErr error
			for _, sub := range dueSubscriptions {
				progress.Mark(mctx, "dunning subscription "+sub.ID.String())
				outcome, processErr := w.processSubscription(mctx, &sub, lifecycle, priceSvc, materialize)
				// One subscription's refusal never fails the pass: it becomes a
				// standing operator finding, and the pass (every minute) tries
				// it again with everything else.
				if err := w.recordSubscriptionOutcome(mctx, &sub, processErr); err != nil {
					merchantErr = errors.Join(merchantErr, fmt.Errorf("subscription %s: %w", sub.ID, err))
				}
				// #511 Phase E: re-converge this customer inline after the dunning
				// transition (past_due / grace / terminal cancel / renewal) — already
				// on the merchant-scoped connection, so call Converge directly. Best-
				// effort: a convergence error must not fail the dunning run.
				if _, cerr := converge.AfterMutation(mctx, w.DB, merchant.ID(sub.MerchantID), sub.CustomerID, w.Clock); cerr != nil {
					log.WithContext(mctx).WithError(cerr).WithField("subscription_id", sub.ID).
						Warn("Dunning: inline converge failed; the sweep will reconcile")
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
			return merchantErr
		}); err != nil {
			// One merchant's failure must not abort the rest of the run.
			log.WithContext(ctx).WithError(err).WithField("merchant_id", merchantID.String()).
				Error("Dunning: merchant pass failed; continuing")
			workErr = errors.Join(workErr, fmt.Errorf("merchant %s: %w", merchantID, err))
		}
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"merchants":      len(merchantIDs),
		"total":          total,
		"success":        successCount,
		"failed":         failCount,
		"window_expired": windowExpiredCount,
		"materialized":   materializedCount,
	}).Info("Dunning: run completed")

	return workErr
}

// FindingDuePassRefused is a subscription the due pass could not process.
const FindingDuePassRefused = "life.due_pass.refused"

// recordSubscriptionOutcome raises or resolves the subscription's standing
// due-pass finding. Only a failure to record it is an error of the pass.
func (w *DunningWorker) recordSubscriptionOutcome(ctx context.Context, sub *models.Subscription, processErr error) error {
	q := w.DB.Gen(ctx)
	if processErr == nil {
		_, err := q.ResolveStandingFinding(ctx, gen.ResolveStandingFindingParams{MerchantID: sub.MerchantID, FindingType: FindingDuePassRefused, SubjectKey: sub.ID.String()})
		return err
	}
	log.WithContext(ctx).WithError(processErr).WithField("subscription_id", sub.ID).
		Error("Dunning: subscription refused; recorded for review, the pass continues")
	evidence, _ := json.Marshal(map[string]any{"subscription_id": sub.ID, "collection_policy": sub.CollectionPolicy, "rail": sub.Rail, "status": sub.Status, "error": processErr.Error()})
	action := fmt.Sprintf("the scheduled due pass could not renew or retry subscription %s (%s). It retries every pass; until the cause is fixed this member is neither charged nor extended: %s", sub.ID, sub.CollectionPolicy, processErr.Error())
	_, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
		MerchantID: sub.MerchantID, FindingType: FindingDuePassRefused, SubjectKey: sub.ID.String(),
		Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: evidence,
	})
	return err
}

// processSubscription attempts a dunning rebill for a single subscription.
func (w *DunningWorker) processSubscription(
	ctx context.Context,
	sub *models.Subscription,
	lifecycle *subscriptions.SubscriptionLifecycleService,
	priceSvc *catalog.PriceService,
	materialize bool,
) (dunningOutcome, error) {
	if sub.CollectionPolicy == models.CollectionPolicyEngine {
		if w.EngineCollections == nil || (w.Config != nil && w.Config.EngineAdmissionHold) {
			return dunningOutcomeFailed, nil
		}
		_, err := w.EngineCollections.AdmitDueSubscriptionCollection(ctx, sub.ID, w.now())
		return dunningOutcomeMaterialized, err
	}
	ctx = db.WithPSPID(ctx, sub.PspID)
	logEntry := log.WithContext(ctx).WithField("subscription_id", sub.ID)

	railName := resolveSubscriptionRail(sub)
	providerKey := railName

	if sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.IsZero() {
		logEntry.Warn("Dunning: past_due subscription has no current period end; skipping rebill")
		return dunningOutcomeFailed, nil
	}

	periodEnd := sub.CurrentPeriodEndsAt.UTC()

	providerAutoBilled := subscriptionProviderAutoBilled(railName, sub)

	// Dunning staleness window (#344, #359): charges are only attempted within
	// the window DERIVED from the price's billing cycle (last retry offset +
	// one day of slack — see collection.Window). Anything older (e.g.
	// months-stale subscriptions imported from a legacy system) must never be
	// surprise-charged by a catch-up run.
	//
	// #839: expiry SKIPS THE CHARGE and PARKS. It used to cancel + revoke
	// entitlements + queue the irreversible NMI SCHEDULE delete, with a date
	// comparison as its only evidence and zero charge attempts — and because a
	// sub-4-day cycle derived a ZERO window, `now > periodEnd + 0` was true by
	// construction, so a daily subscription was destroyed on its first dunning
	// touch having never been billed once. A clock reading is not a death
	// certificate: NMI rebills forever, so a lapsed date is the NORMAL state of
	// a dunning customer. The row parks as `unknown` — access intact, out of the
	// dunning queue — and the unknown-cohort provider probe resolves it against
	// provider truth.
	cycleHours := collection.BillingCycleHoursOf(sub.Price)
	if cycleHours <= 0 && priceSvc != nil {
		if p, err := priceSvc.GetByID(ctx, sub.PriceID); err == nil {
			cycleHours = collection.BillingCycleHoursOf(p)
		}
	}
	if cycleHours <= 0 {
		logEntry.WithField("price_id", sub.PriceID).
			Warn("Dunning: subscription has no billing cycle (one-time price?); using monthly dunning window")
	}
	window := collection.Window(cycleHours)
	if w.now().UTC().After(periodEnd.Add(window)) {
		return w.parkStaleSubscription(ctx, logEntry, sub, lifecycle, periodEnd, window), nil
	}

	// #635: a provider-auto-billed subscription is charged by the provider itself,
	// not by us. Still let the stale-window check above close truly stale rows,
	// but never manual-rebill a vault-less provider-billed subscription.
	if providerAutoBilled {
		logEntry.WithField("rail", railName).
			Info("Dunning: provider-auto-billed (vault-less) subscription; skipping rebill, awaiting provider-pull reconciliation (#632/#633)")
		return dunningOutcomeFailed, nil
	}

	// #730/#788: the armed rail state is the ONLY arming plane — a merchant
	// whose NMI account can arm (or errors while arming) must reach the
	// rebill intent (the handler re-resolves at charge time; a declared-but-
	// unarmable account parks on the ledger with its fail-closed reason).
	if !w.storeArmsNMI(ctx, sub) {
		logEntry.WithFields(log.Fields{"rail": railName, "provider": providerKey}).Warn("NMI rail is not armed for this merchant; skipping")
		return dunningOutcomeFailed, nil
	}

	if !materialize {
		pm := sub.PaymentMethod
		if pm == nil || pm.RailCustomerRef == "" || pm.RailMethodRef == "" {
			if err := lifecycle.ApplyLocalUnknown(ctx, w.DB, sub); err != nil {
				logEntry.WithError(err).Error("Dunning: failed to park subscription with unusable payment method")
				return dunningOutcomeFailed, nil
			}
			logEntry.WithFields(log.Fields{
				"payment_method_present": pm != nil,
				"rail":                   railName,
			}).Error("Dunning: local payment-method data unusable for rebill (OUR row, not a provider decline); subscription PARKED as unknown — no attempt counted, no cancellation, no provider delete. Operator: reconcile the vault refs")
			return dunningOutcomeFailed, nil
		}

	}
	producer := intents.NewManualRebillHandler(w.DB, w.Config, w.NMIResolver, w.Clock)
	intent, err := producer.EnqueueScheduled(ctx, sub.ID)
	if err != nil {
		logEntry.WithError(err).Warn("Dunning: subscription has no admissible recurring attempt")
		return dunningOutcomeFailed, nil
	}
	if materialize {
		logEntry.WithField("intent_id", intent.ID).Info("Dunning: accepted intent is parked until provider writes are enabled")
		return dunningOutcomeMaterialized, nil
	}
	intent, err = w.intentRunner().ExecuteByID(ctx, intent.ID)
	if err != nil {
		return dunningOutcomeFailed, err
	}
	logEntry = logEntry.WithField("intent_id", intent.ID)

	switch intent.Status {
	case intents.StatusSucceeded:
		// The handler's finalize owns the renewal: a succeeded intent has its
		// payment, period and access effects committed exactly once.
		logEntry.Info("Dunning: rebill successful")
		return dunningOutcomeSucceeded, nil

	case intents.StatusFailedTerminal:
		// Refusal and dunning policy committed with this terminal operation.
		return dunningOutcomeFailed, nil

	case intents.StatusUnknownNeedsVerify:
		// Exactly the old markManualRebillUnknown posture: no lifecycle
		// change, no next retry scheduled for this attempt; the intent
		// verifier resolves it via the NMI Query API, and on late-confirmed
		// success the handler's finalize repairs the lifecycle.
		logEntry.Warn("Dunning: manual rebill status unknown; verifier will resolve via provider reads (no further automatic charge for this attempt)")
		return dunningOutcomeFailed, nil

	case intents.StatusPending, intents.StatusInFlight, intents.StatusFailedRetryable:
		// Parked (mode gate, unconfigured client) or owned by another
		// executor; the scheduled executor drains it — or the relevance
		// window expires it if the blocker outlasts the dunning window.
		logEntry.WithField("reason", normalize.FromPtr(intent.LastFailureReason)).
			Info("Dunning: rebill intent not executed this pass; ledger executor will drain it")
		return dunningOutcomeFailed, nil

	default: // superseded, expired
		logEntry.WithField("intent_status", intent.Status).
			Info("Dunning: rebill intent no longer applicable")
		return dunningOutcomeFailed, nil
	}
}

// parkStaleSubscription handles a past_due subscription whose missed rebill is
// older than the dunning window (#839). The charge is SKIPPED — a rebill that
// went stale months ago is never fired by a catch-up run — and the row is
// PARKED as `unknown`: entitlements intact, out of the dunning queue, no
// provider delete queued. It never terminates. The window is a clock reading,
// and a clock reading is not evidence a subscription is dead; the
// unknown-cohort provider probe (ProviderRefreshWorker) is what verifies it
// against provider truth and may then resolve it either way.
func (w *DunningWorker) parkStaleSubscription(
	ctx context.Context,
	logEntry *log.Entry,
	sub *models.Subscription,
	lifecycle *subscriptions.SubscriptionLifecycleService,
	periodEnd time.Time,
	window time.Duration,
) dunningOutcome {
	if err := lifecycle.ApplyLocalUnknown(ctx, w.DB, sub); err != nil {
		logEntry.WithError(err).Error("Dunning: failed to park stale subscription as unknown")
		return dunningOutcomeFailed
	}
	logEntry.WithFields(log.Fields{
		"period_end": periodEnd,
		"window":     window.String(),
	}).Warn("Dunning: rebill is older than the staleness window; charge skipped and subscription PARKED as unknown (access intact) for provider verification")
	return dunningOutcomeWindowExpired
}

// subscriptionProviderAutoBilled reports whether the provider bills this
// subscription on its own side, so OpenRails must not manual-rebill or terminate
// it (#635). Registry-backed (#669); see rails.Descriptor.AutoBilled.
func subscriptionProviderAutoBilled(rail string, sub *models.Subscription) bool {
	return rails.AutoBilled(models.Rail(rail), sub)
}

func resolveSubscriptionRail(sub *models.Subscription) string {
	if sub == nil {
		return ""
	}

	// Use rail field directly
	if p := normalizeRail(sub.Rail); p != "" {
		return p
	}
	if sub.PaymentMethod != nil {
		if p := normalizeRail(sub.PaymentMethod.Rail); p != "" {
			return p
		}
	}
	return ""
}

func normalizeRail(value interface{}) string {
	switch v := value.(type) {
	case *string:
		if v == nil {
			return ""
		}
		return normalizeRail(*v)
	case string:
		return normalize.Lower(v)
	case models.Rail:
		// Subscription.Rail and PaymentMethod.Rail are the named type
		// models.Rail (type Rail string), which does NOT match `case
		// string` in a Go type switch. Without this case resolveSubscriptionRail
		// returns "" for every subscription, so NMIClients[""] is nil and NMI
		// dunning rebills are silently skipped (caught by the dunning integration test).
		return normalize.Lower(string(v))
	default:
		return ""
	}
}
