package riverjobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/shared/cadence"
	"github.com/open-rails/openrails/internal/shared/normalize"
	"github.com/open-rails/openrails/internal/writeposture"
)

const (
	QueueBilling = "billing"
	KindDunning  = "openrails.dunning"

	// dueRecheckMin is the shortest wait of a row the pass left unchanged.
	dueRecheckMin = time.Minute

	// dunningMerchantBatch is one page of the merchants with due work. The
	// work queue is indexed on the due-dunning predicate, so a pass costs its
	// ACTIVITY, never the size of the merchant directory.
	dunningMerchantBatch = 500

	// dueRecheckMax bounds how long a row the pass left unchanged waits before
	// it is looked at again: an operator's switch or a fixed configuration
	// takes effect within it.
	dueRecheckMax = time.Hour
)

// dunningOutcome classifies what a dunning pass did with one subscription.
type dunningOutcome int

const (
	dunningOutcomeFailed dunningOutcome = iota
	dunningOutcomeSucceeded
	// dunningOutcomeWindowExpired: the dunning window passed. A past_due
	// subscription parks as unknown uncharged; one awaiting a card expires;
	// a provider-billed one whose grace ended unanswered is read by id.
	dunningOutcomeWindowExpired
	// dunningOutcomeMaterialized: the charge was queued as a provider
	// operation, which its own job runs (parked under a limited write posture).
	dunningOutcomeMaterialized
	// dunningOutcomeSettled: another replica's pass or a concurrent command
	// resolved the obligation between this pass's scan and its admission.
	dunningOutcomeSettled
	// dunningOutcomeRefused: the row was left as it was (held, refused,
	// unarmed); it waits for its recheck.
	dunningOutcomeRefused
)

// DunningWorker is the due pass: per merchant with due work it queues past_due
// NMI retries as manual_rebill operations, admits due engine renewals to
// collection, ends memberships whose wait for a card outlived the dunning
// window, and asks the provider about a provider-billed row whose grace
// ended unanswered or whose renewal is overdue. It never waits on a provider:
// each operation's own job charges.
type DunningWorker struct {
	DB     *db.DB
	Config *config.Config
	Clock  clockwork.Clock
	// NMIResolver arms store-scoped NMI clients per merchant. Consulted at the
	// charge gate; the rebill handler re-resolves at charge time.
	NMIResolver railresolve.NMIClientResolver
	// EngineCollections admits stored engine obligations; the existing
	// provider-intent fleet performs the accepted charge and receipt recovery.
	EngineCollections *money.MoneyService
	Lifecycle         *subscriptions.SubscriptionLifecycleService
	Log               *slog.Logger
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

// now returns the current time from the worker's clock
func (w *DunningWorker) now() time.Time {
	if w.Clock != nil {
		return w.Clock.Now()
	}
	return time.Now()
}

// Pass runs the due work, then asks the provider about overdue renewals.
func (w *DunningWorker) Pass(ctx context.Context) error {
	return errors.Join(w.due(ctx), w.overdueRenewals(ctx, w.now().UTC()))
}

func (w *DunningWorker) due(ctx context.Context) error {
	// Charges fire only under a merchant's full write posture. Under limited
	// the scan still materializes its decisions: stale subscriptions park as
	// unknown and in-window charges enqueue as parked system-origin intents,
	// visible in `openrails intents`. Nothing on this path cancels without a
	// charge. Readonly (an exported or restored merchant, a copied book) only
	// observes.
	postures := writeposture.View{DB: w.DB}
	if w.NMIResolver == nil && w.EngineCollections == nil {
		w.Log.WarnContext(ctx, "NMI client resolver not configured; skipping dunning run")
		return nil
	}

	// Merchants with due work come from the dunning work queue (ids only); the
	// scan and charge run inside each merchant's scope. w.now(), not SQL NOW(),
	// so tests can mock time. Held admissions leave engine renewals unselected:
	// they wait, untouched, until the hold lifts.
	nmiRails, includeEngine, admitEngine := w.dueScope()
	now := w.now()
	var merchantIDs []uuid.UUID
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
	if len(merchantIDs) == 0 {
		w.Log.DebugContext(ctx, "Dunning: no subscriptions due for retry")
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
				w.Log.WarnContext(mctx, "Readonly: dunning observes due subscriptions only (no charges, no cancellations, no intents)", "count", len(dueSubscriptions), "reason", posture.Reason)
				return nil
			}
			materialize := posture.Limited()
			if materialize {
				w.Log.WarnContext(mctx, "Limited: dunning materializes decisions — stale subscriptions park as unknown, charge intents enqueue parked", "reason", posture.Reason)
			}

			var merchantErr error
			// Page by (due instant, id): a row this pass leaves due is behind
			// the cursor, so it never hides later due work.
			for len(dueSubscriptions) > 0 {
				total += len(dueSubscriptions)
				w.Log.InfoContext(mctx, "Dunning: processing due subscriptions", "count", len(dueSubscriptions), "merchant_id", merchantID.String())
				for _, sub := range dueSubscriptions {
					outcome, processErr := w.processSubscription(mctx, &sub, lifecycle, priceSvc, materialize)
					// One subscription's refusal never fails the pass: it becomes a
					// standing operator finding, and the row waits for its recheck.
					if err := w.recordSubscriptionOutcome(mctx, &sub, processErr); err != nil {
						merchantErr = errors.Join(merchantErr, fmt.Errorf("subscription %s: %w", sub.ID, err))
					}
					if outcome == dunningOutcomeRefused || processErr != nil {
						if err := w.deferDue(mctx, &sub, now); err != nil {
							merchantErr = errors.Join(merchantErr, fmt.Errorf("subscription %s: %w", sub.ID, err))
						}
						failCount++
						continue
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
			w.Log.ErrorContext(ctx, "Dunning: merchant pass failed; continuing", logx.Err(err), "merchant_id", merchantID.String())
			workErr = errors.Join(workErr, fmt.Errorf("merchant %s: %w", merchantID, err))
		}
	}

	w.Log.InfoContext(ctx, "Dunning: run completed", "merchants", len(merchantIDs), "total", total, "success", successCount, "failed", failCount, "window_expired", windowExpiredCount, "materialized", materializedCount, "settled", settledCount)

	return workErr
}

// dueScope is what the pass selects: the rails it collects, and whether it
// ends and admits engine renewals.
func (w *DunningWorker) dueScope() (rails []string, includeEngine, admitEngine bool) {
	rails = []string{string(models.RailNMI)}
	if w.EngineCollections != nil {
		rails = append(rails, string(models.RailStripe))
	}
	includeEngine = w.EngineCollections != nil
	admitEngine = includeEngine && (w.Config == nil || !w.Config.EngineAdmissionHold)
	return rails, includeEngine, admitEngine
}

// DueBy reports a subscription the pass would select at at.
func (w *DunningWorker) DueBy(ctx context.Context, at time.Time) (bool, error) {
	rails, includeEngine, admitEngine := w.dueScope()
	ids, err := w.DB.GenDirectory().ListDueDunningMerchants(ctx, gen.ListDueDunningMerchantsParams{
		Rails: rails, Now: at, MerchantLimit: 1, IncludeEngine: includeEngine, AdmitEngine: admitEngine, AfterMerchantID: uuid.Nil,
	})
	return len(ids) > 0, err
}

// overdueRenewals moves each provider-billed renewal still unconfirmed
// reconcile.PeriodGrace after its period to unverified: a clock reading is
// not evidence, so it only asks the provider. The row leaves the due set.
func (w *DunningWorker) overdueRenewals(ctx context.Context, now time.Time) error {
	if w.Lifecycle == nil {
		return errors.New("overdue renewals: lifecycle not wired")
	}
	before := now.Add(-reconcile.PeriodGrace)
	postures := writeposture.View{DB: w.DB}
	var workErr error
	after := uuid.Nil
	for {
		merchantIDs, err := w.DB.GenDirectory().ListOverdueRenewalMerchants(ctx, gen.ListOverdueRenewalMerchantsParams{
			OverdueBefore: before, AfterMerchantID: after, MerchantLimit: dunningMerchantBatch,
		})
		if err != nil {
			return errors.Join(workErr, fmt.Errorf("overdue renewals: list merchants: %w", err))
		}
		for _, mid := range merchantIDs {
			if err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(mid), "overdue renewals", func(ctx context.Context) error {
				if p := postures.Posture(ctx, mid); p.ReadOnly() {
					return nil
				}
				return w.overdueMerchantRenewals(ctx, mid, before)
			}); err != nil {
				workErr = errors.Join(workErr, fmt.Errorf("merchant %s: %w", mid, err))
			}
		}
		if len(merchantIDs) < dunningMerchantBatch {
			return workErr
		}
		after = merchantIDs[len(merchantIDs)-1]
	}
}

func (w *DunningWorker) overdueMerchantRenewals(ctx context.Context, mid uuid.UUID, before time.Time) error {
	var merchantErr error
	var afterAt time.Time
	afterID := uuid.Nil
	for {
		rows, err := w.DB.Gen(ctx).ListOverdueRenewals(ctx, gen.ListOverdueRenewalsParams{
			MerchantID: mid, OverdueBefore: before, AfterDueAt: afterAt, AfterID: afterID, RowLimit: subscriptions.DueDunningBatch,
		})
		if err != nil {
			return errors.Join(merchantErr, err)
		}
		for _, row := range rows {
			if _, err := w.Lifecycle.AskProviderOverdueRenewal(ctx, w.DB, row.ID, *row.CurrentPeriodEndsAt); err != nil {
				merchantErr = errors.Join(merchantErr, fmt.Errorf("subscription %s: %w", row.ID, err))
			}
		}
		if len(rows) < subscriptions.DueDunningBatch {
			return merchantErr
		}
		last := rows[len(rows)-1]
		afterAt, afterID = *last.CurrentPeriodEndsAt, last.ID
	}
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
	w.Log.ErrorContext(ctx, "Dunning: subscription refused; recorded for review, the pass continues", logx.Err(processErr), "subscription_id", sub.ID)
	evidence, _ := json.Marshal(map[string]any{"subscription_id": sub.ID, "collection_policy": sub.CollectionPolicy, "rail": sub.Rail, "status": sub.Status, "error": processErr.Error()})
	action := fmt.Sprintf("the scheduled due pass could not renew or retry subscription %s (%s). It retries every pass; until the cause is fixed this member is neither charged nor extended: %s", sub.ID, sub.CollectionPolicy, processErr.Error())
	_, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
		MerchantID: sub.MerchantID, FindingType: FindingDuePassRefused, SubjectKey: sub.ID.String(),
		Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: evidence,
	})
	return err
}

// deferDue takes a row the pass left unchanged out of the due set until its
// recheck: half the time it has been due, at least one pass interval and at
// most dueRecheckMax.
func (w *DunningWorker) deferDue(ctx context.Context, sub *models.Subscription, now time.Time) error {
	due := subscriptions.DueDunningCursorOf(sub).DueAt
	if due.IsZero() {
		due = now
	}
	wait := min(max(now.Sub(due)/2, dueRecheckMin), dueRecheckMax)
	return w.DB.Gen(ctx).DeferDueSubscription(ctx, gen.DeferDueSubscriptionParams{MerchantID: sub.MerchantID, ID: sub.ID, RecheckAt: now.Add(wait)})
}

// processSubscription takes one due subscription's next step.
func (w *DunningWorker) processSubscription(
	ctx context.Context,
	sub *models.Subscription,
	lifecycle *subscriptions.SubscriptionLifecycleService,
	priceSvc *catalog.PriceService,
	materialize bool,
) (dunningOutcome, error) {
	if sub.AdviceDeferred() {
		// The network's advice allowed no retry before the next renewal: the
		// case ends there. A limited write posture never ends it locally.
		blocked := ""
		if materialize {
			blocked = "a limited write posture holds local terminal cancellations"
		} else if gate := destructive.New(w.DB).Check(ctx, sub.MerchantID); !gate.Allowed {
			blocked = gate.Reason
		}
		if _, err := lifecycle.EndAdviceDeferred(ctx, w.DB, sub, blocked); err != nil {
			return dunningOutcomeFailed, err
		}
		if blocked != "" {
			return dunningOutcomeRefused, nil
		}
		return dunningOutcomeWindowExpired, nil
	}
	if sub.Status == models.StatusAwaitingMethod {
		// The wait for a new card outlived its dunning window. A limited write
		// posture never ends it locally; the operator sees the held outcome.
		blocked := ""
		if materialize {
			blocked = "a limited write posture holds local terminal cancellations"
		} else if gate := destructive.New(w.DB).Check(ctx, sub.MerchantID); !gate.Allowed {
			blocked = gate.Reason
		}
		if _, err := lifecycle.ExpireAwaitingMethod(ctx, w.DB, sub, blocked); err != nil {
			return dunningOutcomeFailed, err
		}
		if blocked != "" {
			return dunningOutcomeRefused, nil
		}
		return dunningOutcomeWindowExpired, nil
	}
	if sub.Status == models.StatusPastDue && sub.NextRetryAt == nil && sub.CollectionPolicy != models.CollectionPolicyEngine {
		// Grace ended and the provider never said renewed or canceled: ask it.
		if _, err := lifecycle.AskProviderPastGrace(ctx, w.DB, sub, w.now()); err != nil {
			return dunningOutcomeFailed, err
		}
		return dunningOutcomeWindowExpired, nil
	}
	if sub.CollectionPolicy == models.CollectionPolicyEngine {
		// The due query selects no engine renewal while admissions are held.
		if w.EngineCollections == nil {
			return dunningOutcomeRefused, nil
		}
		_, err := w.EngineCollections.AdmitDueSubscriptionCollection(ctx, sub.ID, w.now())
		if errors.Is(err, subscriptions.ErrRenewalHeldByUpgrade) {
			return dunningOutcomeRefused, nil
		}
		if errors.Is(err, money.ErrEngineMethodUnusable) {
			// Only the member can fix this: their renewal is an open order
			// they pay with a usable card, on the dunning clock, under the
			// merchant's dunning access policy.
			w.Log.WarnContext(ctx, "renewal: the payment method cannot be charged; the member pays the renewal order", logx.Err(err), "subscription_id", sub.ID)
			opened := w.EngineCollections.OpenDueRenewalOrder(ctx, sub.ID, w.now())
			reason, code := "payment_method_unusable", "payment_method_unusable"
			if errors.Is(err, charge.ErrAgreementRequired) {
				code = charge.AgreementRequiredCode
			}
			if err := lifecycle.FailMembership(ctx, &subscriptions.FailMembershipParams{Rail: sub.Rail, SubscriptionID: &sub.ID, FailureID: subscriptions.RenewalFailureID(sub, reason),
				FailureReason: &reason, FailureCode: &code, Decline: decline.FixPaymentMethod}); err != nil {
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
	logEntry := w.Log.With("subscription_id", sub.ID)

	railName := resolveSubscriptionRail(sub)
	providerKey := railName

	if sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.IsZero() {
		logEntry.WarnContext(ctx, "Dunning: past_due subscription has no current period end; skipping rebill")
		return dunningOutcomeRefused, nil
	}

	periodEnd := sub.CurrentPeriodEndsAt.UTC()

	providerAutoBilled := subscriptionProviderAutoBilled(railName, sub)

	// Charges are attempted only within the window derived from the price's
	// billing cycle: an older rebill (e.g. a months-stale legacy import) is
	// never surprise-charged by a catch-up run. Expiry skips the charge and
	// parks the row as `unverified` (access intact, out of the dunning queue)
	// for a targeted provider read. A lapsed date alone gives no evidence of
	// whether NMI renewed or ended the subscription.
	cycleHours := collection.BillingCycleHoursOf(sub.Price)
	if cycleHours <= 0 && priceSvc != nil {
		if p, err := priceSvc.GetByID(ctx, sub.PriceID); err == nil {
			cycleHours = collection.BillingCycleHoursOf(p)
		}
	}
	policy, err := subscriptions.CasePolicy(ctx, w.DB, sub)
	if err != nil {
		return dunningOutcomeRefused, err
	}
	window, err := policy.Window(cycleHours)
	if err != nil {
		// Fail closed: no charge on a guessed cadence; the operator decides.
		logEntry.ErrorContext(ctx, "Dunning: subscription has no billing cycle; refusing to rebill", logx.Err(err), "price_id", sub.PriceID)
		if ferr := collection.RecordUnknownCycle(ctx, w.DB.Gen(ctx), sub.MerchantID, sub.ID.String(), "subscription", err); ferr != nil {
			return dunningOutcomeRefused, errors.Join(err, ferr)
		}
		return dunningOutcomeRefused, nil
	}
	caseEnd, err := subscriptions.CaseEnd(ctx, w.DB, policy, sub, periodEnd, cycleHours)
	if err != nil {
		return dunningOutcomeRefused, err
	}
	if w.now().UTC().After(caseEnd) {
		return w.parkStaleSubscription(ctx, logEntry, sub, lifecycle, periodEnd, window), nil
	}

	// The provider charges a provider-auto-billed subscription itself: never
	// manual-rebill it.
	if providerAutoBilled {
		logEntry.InfoContext(ctx, "Dunning: provider-auto-billed (vault-less) subscription; skipping rebill, awaiting provider-pull reconciliation", "rail", railName)
		return dunningOutcomeRefused, nil
	}

	if !w.storeArmsNMI(ctx, sub) {
		logEntry.WarnContext(ctx, "NMI rail is not armed for this merchant; skipping", "rail", railName, "provider", providerKey)
		return dunningOutcomeRefused, nil
	}

	if !materialize {
		pm := sub.PaymentMethod
		if pm == nil || pm.RailCustomerRef == "" || pm.RailMethodRef == "" {
			if err := lifecycle.ApplyLocalUnknown(ctx, w.DB, sub); err != nil {
				logEntry.ErrorContext(ctx, "Dunning: failed to park subscription with unusable payment method", logx.Err(err))
				return dunningOutcomeFailed, nil
			}
			logEntry.ErrorContext(ctx, "Dunning: local payment-method data unusable for rebill (OUR row, not a provider decline); subscription PARKED as unknown — no attempt counted, no cancellation, no provider delete. Operator: reconcile the vault refs", "payment_method_present", pm != nil, "rail", railName)
			return dunningOutcomeFailed, nil
		}

	}
	producer := intents.NewManualRebillHandler(w.DB, w.Config, w.NMIResolver, w.Clock)
	intent, err := producer.EnqueueScheduled(ctx, sub.ID)
	if errors.Is(err, charge.ErrAgreementRequired) {
		// Only the member can fix this: wait for a card they agree to.
		reason, code := "payment_method_unusable", charge.AgreementRequiredCode
		if err := lifecycle.FailMembership(ctx, &subscriptions.FailMembershipParams{Rail: sub.Rail, SubscriptionID: &sub.ID, FailureID: subscriptions.RenewalFailureID(sub, reason),
			FailureReason: &reason, FailureCode: &code, Decline: decline.FixPaymentMethod}); err != nil {
			return dunningOutcomeFailed, fmt.Errorf("await a payment method: %w", err)
		}
		return dunningOutcomeFailed, nil
	}
	if err != nil {
		logEntry.WarnContext(ctx, "Dunning: subscription has no admissible recurring attempt", logx.Err(err))
		return dunningOutcomeRefused, nil
	}
	if materialize {
		logEntry.InfoContext(ctx, "Dunning: accepted intent is parked until provider writes are enabled", "intent_id", intent.ID)
	}
	return dunningOutcomeMaterialized, nil
}

// parkStaleSubscription skips the charge of a past_due subscription whose
// missed rebill is older than the dunning window. The unverified transition
// queues a read of the subscription, keeping the customer's access while the
// provider result is unknown.
func (w *DunningWorker) parkStaleSubscription(
	ctx context.Context,
	logEntry *slog.Logger,
	sub *models.Subscription,
	lifecycle *subscriptions.SubscriptionLifecycleService,
	periodEnd time.Time,
	window time.Duration,
) dunningOutcome {
	if err := lifecycle.ApplyLocalUnknown(ctx, w.DB, sub); err != nil {
		logEntry.ErrorContext(ctx, "Dunning: failed to park stale subscription as unknown", logx.Err(err))
		return dunningOutcomeFailed
	}
	logEntry.WarnContext(ctx, "Dunning: rebill is older than the staleness window; charge skipped and subscription PARKED as unknown (access intact) for provider verification", "period_end", periodEnd, "window", cadence.FormatDuration(window))
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
