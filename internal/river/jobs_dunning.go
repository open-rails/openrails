package riverjobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/decline"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/open-rails/openrails/internal/shared/cadence"
	"github.com/open-rails/openrails/internal/shared/normalize"
	"github.com/open-rails/openrails/internal/shared/progress"
	"github.com/open-rails/openrails/internal/writeposture"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const (
	QueueBilling = "billing"
	KindDunning  = "openrails.dunning"

	// DuePassInterval is the due pass cadence: the bound on how long an
	// engine renewal waits past its paid-period boundary.
	DuePassInterval = time.Minute

	// dunningMerchantBatch is one page of the merchants with due work. The
	// work queue is indexed on the due-dunning predicate, so a pass costs its
	// ACTIVITY, never the size of the merchant directory.
	dunningMerchantBatch = 500
)

// dunningOutcome classifies what a dunning pass did with one subscription.
type dunningOutcome int

const (
	dunningOutcomeFailed dunningOutcome = iota
	dunningOutcomeSucceeded
	// dunningOutcomeWindowExpired: the dunning window passed. A past_due
	// subscription parks as unknown uncharged; one awaiting a card expires.
	dunningOutcomeWindowExpired
	// dunningOutcomeMaterialized: the charge decision was recorded as an
	// intent (parked under mode=limited) instead of executed inline.
	dunningOutcomeMaterialized
	// dunningOutcomeSettled: another replica's pass or a concurrent command
	// resolved the obligation between this pass's scan and its admission.
	dunningOutcomeSettled
)

// DunningArgs triggers one due pass; a set MerchantID scopes it to that merchant.
type DunningArgs struct {
	MerchantID uuid.UUID `json:"merchant_id,omitempty"`
}

func (DunningArgs) Kind() string { return KindDunning }

// DunningWorker is the due pass: per merchant with due work it retries past_due
// NMI subscriptions through manual_rebill intents, admits due engine renewals
// to collection, and ends memberships whose wait for a card outlived the
// dunning window.
type DunningWorker struct {
	river.WorkerDefaults[DunningArgs]
	DB     *db.DB
	Config *config.Config
	Clock  clockwork.Clock
	// NMIResolver arms store-scoped NMI clients per merchant. Consulted at the
	// charge gate; the rebill handler re-resolves at charge time.
	NMIResolver money.NMIClientResolver
	// DeferDelete schedules the provider-side delete for terminal
	// cancellations through the self-assembled rebill handler. nil leaves the
	// remote subscription for reconciliation.
	DeferDelete subscriptions.ProviderCancelScheduler
	// Intents executes the provider-side charge through the intent ledger: the
	// worker enqueues a manual_rebill intent and runs it synchronously. nil
	// builds a Runner over the worker's own dependencies.
	Intents *intents.Runner
	// EngineCollections admits stored engine obligations; the existing
	// provider-intent fleet performs the accepted charge and receipt recovery.
	EngineCollections *money.MoneyService
}

// intentRunner returns the configured Runner or assembles one (tests). Config
// is attached only when set: a nil ModeView fails closed, so every rebill
// would park silently.
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
		runner.Config = writeposture.View{Config: w.Config, DB: w.DB}
	}
	return runner
}

// storeArmsNMI reports whether the merchant-secrets store can arm an NMI
// client for the subscription's account. Resolver errors count as armable: a
// declared-but-unarmable account must reach the ledger and park with its loud
// fail-closed reason, not vanish in a silent skip.
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
	// Charges fire only in mode=full. Under limited the scan still materializes
	// its decisions: stale subscriptions park as unknown and in-window charges
	// enqueue as parked system-origin intents, visible in `openrails intents`.
	// Nothing on this path cancels without a charge. Readonly only observes.
	// Each merchant's write posture can lower the process's mode (an exported
	// or restored merchant, a copied book).
	postures := writeposture.View{Config: w.Config, DB: w.DB}
	if w.NMIResolver == nil && w.EngineCollections == nil {
		log.WithContext(ctx).Warn("NMI client resolver not configured; skipping dunning run")
		return nil
	}

	// Merchants with due work come from the dunning work queue (ids only); the
	// scan and charge run inside each merchant's scope. w.now(), not SQL NOW(),
	// so tests can mock time. Held admissions leave engine renewals unselected:
	// they wait, untouched, until the hold lifts.
	nmiRails := []string{string(models.RailNMI)}
	if w.EngineCollections != nil {
		nmiRails = append(nmiRails, string(models.RailStripe))
	}
	includeEngine := w.EngineCollections != nil
	admitEngine := includeEngine && (w.Config == nil || !w.Config.EngineAdmissionHold)
	now := w.now()
	var merchantIDs []uuid.UUID
	if job.Args.MerchantID != uuid.Nil {
		merchantIDs = []uuid.UUID{job.Args.MerchantID}
	} else {
		after := uuid.Nil
		for {
			page, err := w.DB.GenDirectory().ListDueDunningMerchants(ctx, gen.ListDueDunningMerchantsParams{
				Rails: nmiRails, Now: now, MerchantLimit: dunningMerchantBatch, IncludeEngine: includeEngine, AdmitEngine: admitEngine, AfterMerchantID: after,
			})
			if err != nil {
				return fmt.Errorf("query merchants with due subscriptions: %w", err)
			}
			merchantIDs = append(merchantIDs, page...)
			if len(page) < dunningMerchantBatch {
				break
			}
			after = page[len(page)-1]
		}
	}
	if len(merchantIDs) == 0 {
		log.WithContext(ctx).Debug("Dunning: no subscriptions due for retry")
		return nil
	}

	// Build services once for all attempts
	priceSvc := catalog.NewPriceService(w.DB)
	productSvc := catalog.NewProductService(w.DB)
	entitlementSvc := entitlements.NewEntitlementService(w.DB, w.Clock)
	notifSvc := subscriptions.NewNotificationService(w.DB)
	paymentSvc := payments.NewPaymentService(w.DB, w.Clock)
	lifecycle := subscriptions.NewSubscriptionLifecycleService(w.DB, productSvc, priceSvc, entitlementSvc, notifSvc, paymentSvc, w.Clock)
	lifecycle.SetConfig(w.Config)

	total := 0
	successCount := 0
	failCount := 0
	windowExpiredCount := 0
	materializedCount := 0
	settledCount := 0
	var workErr error

	for _, mid := range merchantIDs {
		merchantID := billing.MerchantID(mid)
		progress.Mark(ctx, "dunning merchant "+merchantID.String())
		// The pin AND the proof it took: every read and write below runs under
		// this merchant's openrails.merchant_id, exactly as a request would.
		if err := w.DB.RunInMerchantScope(ctx, merchantID, "dunning pass", func(mctx context.Context) error {
			repo := subscriptions.NewSubscriptionRepo(w.DB)
			dueSubscriptions, err := repo.ListDueDunningSubscriptions(mctx, nmiRails, now, includeEngine, admitEngine, nil)
			if err != nil {
				return fmt.Errorf("query due subscriptions: %w", err)
			}
			if len(dueSubscriptions) == 0 {
				return nil
			}
			posture := postures.Posture(mctx, mid)
			if posture.ReadOnly() {
				total += len(dueSubscriptions)
				log.WithContext(mctx).WithFields(log.Fields{"count": len(dueSubscriptions), "reason": posture.Reason}).
					Warn("Readonly: dunning observes due subscriptions only (no charges, no cancellations, no intents)")
				return nil
			}
			materialize := posture.Limited()
			if materialize {
				log.WithContext(mctx).WithField("reason", posture.Reason).
					Warn("Limited: dunning materializes decisions — stale subscriptions park as unknown, charge intents enqueue parked")
			}

			var merchantErr error
			// Page by (due instant, id): a row this pass leaves due is behind
			// the cursor, so it never hides later due work.
			for len(dueSubscriptions) > 0 {
				total += len(dueSubscriptions)
				log.WithContext(mctx).WithFields(log.Fields{
					"count": len(dueSubscriptions), "merchant_id": merchantID.String(),
				}).Info("Dunning: processing due subscriptions")
				for _, sub := range dueSubscriptions {
					progress.Mark(mctx, "dunning subscription "+sub.ID.String())
					outcome, processErr := w.processSubscription(mctx, &sub, lifecycle, priceSvc, materialize)
					// One subscription's refusal never fails the pass: it becomes a
					// standing operator finding, and the pass (every minute) tries
					// it again with everything else.
					if err := w.recordSubscriptionOutcome(mctx, &sub, processErr); err != nil {
						merchantErr = errors.Join(merchantErr, fmt.Errorf("subscription %s: %w", sub.ID, err))
					}
					// Re-converge this customer after the dunning transition.
					// Best-effort: a convergence error must not fail the run.
					if _, cerr := converge.AfterMutation(mctx, w.DB, billing.MerchantID(sub.MerchantID), sub.CustomerID, w.Clock); cerr != nil {
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
					case dunningOutcomeSettled:
						settledCount++
					default:
						failCount++
					}
				}
				if len(dueSubscriptions) < subscriptions.DueDunningBatch {
					break
				}
				cursor := subscriptions.DueDunningCursorOf(&dueSubscriptions[len(dueSubscriptions)-1])
				if dueSubscriptions, err = repo.ListDueDunningSubscriptions(mctx, nmiRails, now, includeEngine, admitEngine, &cursor); err != nil {
					return errors.Join(merchantErr, fmt.Errorf("query due subscriptions: %w", err))
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
		"settled":        settledCount,
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
	if sub.Status == models.StatusAwaitingMethod {
		// The wait for a new card outlived its dunning window. Limited mode
		// never ends it locally; the operator sees the held outcome.
		blocked := ""
		if materialize {
			blocked = "mode=limited holds local terminal cancellations"
		} else if gate := destructive.New(w.DB).Check(ctx, sub.MerchantID); !gate.Allowed {
			blocked = gate.Reason
		}
		if _, err := lifecycle.ExpireAwaitingMethod(ctx, w.DB, sub, blocked); err != nil {
			return dunningOutcomeFailed, err
		}
		return dunningOutcomeWindowExpired, nil
	}
	if sub.CollectionPolicy == models.CollectionPolicyEngine {
		// The due query selects no engine renewal while admissions are held.
		if w.EngineCollections == nil {
			return dunningOutcomeFailed, nil
		}
		_, err := w.EngineCollections.AdmitDueSubscriptionCollection(ctx, sub.ID, w.now())
		if errors.Is(err, subscriptions.ErrRenewalHeldByUpgrade) {
			return dunningOutcomeFailed, nil
		}
		if errors.Is(err, money.ErrEngineMethodUnusable) {
			// Only the member can fix this: their renewal is an open order
			// they pay with a usable card, on the dunning clock, under the
			// merchant's dunning access policy.
			log.WithContext(ctx).WithError(err).WithField("subscription_id", sub.ID).Warn("renewal: the payment method cannot be charged; the member pays the renewal order")
			opened := w.EngineCollections.OpenDueRenewalOrder(ctx, sub.ID, w.now())
			reason, code := "payment_method_unusable", "payment_method_unusable"
			if errors.Is(err, charge.ErrAgreementRequired) {
				code = charge.AgreementRequiredCode
			}
			if err := lifecycle.FailMembership(ctx, &subscriptions.FailMembershipParams{Rail: sub.Rail, SubscriptionID: &sub.ID, FailureReason: &reason, FailureCode: &code, Decline: decline.FixPaymentMethod}); err != nil {
				return dunningOutcomeFailed, fmt.Errorf("await a payment method: %w", err)
			}
			if opened != nil {
				return dunningOutcomeFailed, fmt.Errorf("open the renewal order: %w", opened)
			}
			return dunningOutcomeFailed, nil
		}
		if errors.Is(err, money.ErrEngineCollectionNotDue) {
			// Settled by another writer after this pass read it: nothing to do.
			return dunningOutcomeSettled, nil
		}
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

	// Charges are attempted only within the window derived from the price's
	// billing cycle: an older rebill (e.g. a months-stale legacy import) is
	// never surprise-charged by a catch-up run. Expiry skips the charge and
	// parks the row as `unknown` (access intact, out of the dunning queue) for
	// the unknown-cohort provider probe: NMI rebills forever, so a lapsed date
	// is evidence of nothing.
	cycleHours := collection.BillingCycleHoursOf(sub.Price)
	if cycleHours <= 0 && priceSvc != nil {
		if p, err := priceSvc.GetByID(ctx, sub.PriceID); err == nil {
			cycleHours = collection.BillingCycleHoursOf(p)
		}
	}
	policy, err := subscriptions.CasePolicy(ctx, w.DB, sub)
	if err != nil {
		return dunningOutcomeFailed, err
	}
	window, err := policy.Window(cycleHours)
	if err != nil {
		// Fail closed: no charge on a guessed cadence; the operator decides.
		logEntry.WithError(err).WithField("price_id", sub.PriceID).Error("Dunning: subscription has no billing cycle; refusing to rebill")
		if ferr := collection.RecordUnknownCycle(ctx, w.DB.Gen(ctx), sub.MerchantID, sub.ID.String(), "subscription", err); ferr != nil {
			return dunningOutcomeFailed, errors.Join(err, ferr)
		}
		return dunningOutcomeFailed, nil
	}
	if w.now().UTC().After(periodEnd.Add(window)) {
		return w.parkStaleSubscription(ctx, logEntry, sub, lifecycle, periodEnd, window), nil
	}

	// The provider charges a provider-auto-billed subscription itself: never
	// manual-rebill it.
	if providerAutoBilled {
		logEntry.WithField("rail", railName).
			Info("Dunning: provider-auto-billed (vault-less) subscription; skipping rebill, awaiting provider-pull reconciliation (#632/#633)")
		return dunningOutcomeFailed, nil
	}

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
	if errors.Is(err, charge.ErrAgreementRequired) {
		// Only the member can fix this: wait for a card they agree to.
		reason, code := "payment_method_unusable", charge.AgreementRequiredCode
		if err := lifecycle.FailMembership(ctx, &subscriptions.FailMembershipParams{Rail: sub.Rail, SubscriptionID: &sub.ID, FailureReason: &reason, FailureCode: &code, Decline: decline.FixPaymentMethod}); err != nil {
			return dunningOutcomeFailed, fmt.Errorf("await a payment method: %w", err)
		}
		return dunningOutcomeFailed, nil
	}
	if err != nil {
		logEntry.WithError(err).Warn("Dunning: subscription has no admissible recurring attempt")
		return dunningOutcomeFailed, nil
	}
	if materialize {
		logEntry.WithField("intent_id", intent.ID).Info("Dunning: accepted intent is parked until provider writes are enabled")
		return dunningOutcomeMaterialized, nil
	}
	intent, err = w.intentRunner().ExecuteFirstByID(ctx, intent.ID)
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
		// No lifecycle change and no next retry for this attempt: the intent
		// verifier resolves it via the NMI Query API, and a late-confirmed
		// success repairs the lifecycle in the handler's finalize.
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

// parkStaleSubscription skips the charge of a past_due subscription whose
// missed rebill is older than the dunning window and parks it as `unknown`:
// access intact, out of the dunning queue, no provider delete. It never
// terminates; the unknown-cohort probe (ProviderRefreshWorker) resolves it
// against the provider.
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
		"window":     cadence.FormatDuration(window),
	}).Warn("Dunning: rebill is older than the staleness window; charge skipped and subscription PARKED as unknown (access intact) for provider verification")
	return dunningOutcomeWindowExpired
}

// subscriptionProviderAutoBilled reports whether the provider bills this
// subscription itself, so OpenRails must not manual-rebill or terminate it
// (see rails.Descriptor.AutoBilled).
func subscriptionProviderAutoBilled(rail string, sub *models.Subscription) bool {
	return rails.AutoBilled(models.Rail(rail), sub)
}

func resolveSubscriptionRail(sub *models.Subscription) string {
	if sub == nil {
		return ""
	}

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
		// models.Rail does not match `case string` in a type switch; without
		// this case every subscription resolves to no rail and is never
		// rebilled.
		return normalize.Lower(string(v))
	default:
		return ""
	}
}
