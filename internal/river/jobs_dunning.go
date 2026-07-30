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
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/open-rails/openrails/internal/shared/normalize"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const (
	QueueBilling = "billing"
	KindDunning  = "openrails.dunning"

	dunningAttemptLease = 15 * time.Minute

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
	IdempotencyService *idempotency.IdempotencyService
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
	_, ok, err := w.NMIResolver.ResolveNMIClient(ctx, sub.MerchantID, sub.PspID)
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

	if w.NMIResolver == nil {
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
	merchantIDs, err := w.DB.GenDirectory().ListDueDunningMerchants(ctx, gen.ListDueDunningMerchantsParams{
		Rails: nmiRails, Now: w.now(), MerchantLimit: dunningMerchantBatch,
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
	if w.DeferDelete != nil {
		// Terminal cancellations (window expiry, retry exhaustion) schedule
		// the remote NMI delete through the shared mechanism (#344).
		lifecycle.SetDeferredDeleteScheduler(w.DeferDelete)
	}
	moneySvc := money.NewMoneyService(w.DB, w.Clock)

	total := 0
	successCount := 0
	failCount := 0
	windowExpiredCount := 0
	materializedCount := 0

	for _, mid := range merchantIDs {
		if mid == nil {
			continue
		}
		merchantID := merchant.ID(*mid)
		// The pin AND the proof it took: every read and write below runs under
		// this merchant's app.merchant_id, exactly as a request would.
		if err := w.DB.RunInMerchantScope(ctx, merchantID, "dunning pass", func(mctx context.Context) error {
			dueSubscriptions, err := subscriptions.NewSubscriptionRepo(w.DB).ListDueDunningSubscriptions(mctx, nmiRails, w.now())
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

			for _, sub := range dueSubscriptions {
				outcome := w.processSubscription(mctx, &sub, lifecycle, priceSvc, moneySvc, materialize)
				// #511 Phase E: re-converge this customer inline after the dunning
				// transition (past_due / grace / terminal cancel / renewal) — already
				// on the merchant-scoped connection, so call Converge directly. Best-
				// effort: a convergence error must not fail the dunning run.
				if _, cerr := converge.AfterMutation(mctx, w.DB, merchant.ID(sub.MerchantID), sub.CustomerID); cerr != nil {
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
			return nil
		}); err != nil {
			// One merchant's failure must not abort the rest of the run.
			log.WithContext(ctx).WithError(err).WithField("merchant_id", merchantID.String()).
				Error("Dunning: merchant pass failed; continuing")
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

	railName := resolveSubscriptionRail(sub)
	providerKey := railName

	if sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.IsZero() {
		logEntry.Warn("Dunning: past_due subscription has no current period end; skipping rebill")
		return dunningOutcomeFailed
	}

	periodEnd := sub.CurrentPeriodEndsAt.UTC()
	rail := models.Rail(railName)

	providerAutoBilled := subscriptionProviderAutoBilled(railName, sub.PaymentMethod)

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
		return w.parkStaleSubscription(ctx, logEntry, sub, lifecycle, periodEnd, window)
	}

	// #635: a provider-auto-billed subscription is charged by the provider itself,
	// not by us. Still let the stale-window check above close truly stale rows,
	// but never manual-rebill a vault-less provider-billed subscription.
	if providerAutoBilled {
		logEntry.WithField("rail", railName).
			Info("Dunning: provider-auto-billed (vault-less) subscription; skipping rebill, awaiting provider-pull reconciliation (#632/#633)")
		return dunningOutcomeFailed
	}

	// #730/#788: the armed rail state is the ONLY arming plane — a merchant
	// whose NMI account can arm (or errors while arming) must reach the
	// rebill intent (the handler re-resolves at charge time; a declared-but-
	// unarmable account parks on the ledger with its fail-closed reason).
	if !w.storeArmsNMI(ctx, sub) {
		logEntry.WithFields(log.Fields{"rail": railName, "provider": providerKey}).Warn("NMI rail is not armed for this merchant; skipping")
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
			logEntry.WithError(err).Warn("Dunning (materialize): failed to load subscription merchant for rebill intent")
			return dunningOutcomeFailed
		}
		attemptOrdinal := 0
		if sub.RetryAttempts != nil {
			attemptOrdinal = *sub.RetryAttempts
		}
		windowEnd := periodEnd.Add(window)
		row, err := w.intentRunner().Store.Enqueue(ctx, intents.EnqueueParams{
			MerchantID:     genSub.MerchantID,
			Provider:       providerKey,
			IntentType:     intents.TypeManualRebill,
			SubscriptionID: &sub.ID,
			PspID:          sub.PspID,
			Payload: intents.ManualRebillPayload{
				SubscriptionID: sub.ID,
				PeriodEnd:      periodEnd,
				Rail:           railName,
				OrderReference: orderReference,
				Attempt:        attemptOrdinal,
			},
			IdempotencyKey: intents.ManualRebillIdempotencyKey(sub.ID, periodEnd, railName, orderReference, attemptOrdinal),
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

	// #840: our own payment-method row is missing or incomplete. This is the
	// ABSENCE OF OUR OWN DATA, not a provider decline — a vault-sync bug, a
	// failed `payment_methods` write or a mis-populated RailMethodRef is
	// indistinguishable here from a dead card. It used to run the failure
	// policy, which counted it as a dunning failure and (DunningMaxFailures == 1
	// for sub-4-day cycles) terminated on the FIRST observation: cancel +
	// entitlement revoke + irreversible NMI SCHEDULE delete, with no forensic
	// `payments` row to show for it.
	//
	// It now PARKS, BEFORE the claim, so it never touches last_retry_at or
	// retry_attempts. Verification against the provider is deliberately NOT
	// attempted inline: the missing field IS the vault handle, so there is
	// nothing to query the rail with. Parking as `unknown` routes the row to the
	// unknown-cohort provider probe, which enumerates the merchant's roster and
	// is the only plane that can verify a subscription whose local refs are gone.
	pm := sub.PaymentMethod
	if pm == nil || pm.RailCustomerRef == "" || pm.RailMethodRef == "" {
		if err := lifecycle.ApplyLocalUnknown(ctx, w.DB, sub); err != nil {
			logEntry.WithError(err).Error("Dunning: failed to park subscription with unusable payment method")
			return dunningOutcomeFailed
		}
		logEntry.WithFields(log.Fields{
			"payment_method_present": pm != nil,
			"rail":                   railName,
		}).Error("Dunning: local payment-method data unusable for rebill (OUR row, not a provider decline); subscription PARKED as unknown — no attempt counted, no cancellation, no provider delete. Operator: reconcile the vault refs")
		return dunningOutcomeFailed
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

	// The provider-side charge flows through the intent ledger (#358 phase C):
	// one system-origin intent per (subscription, period end, attempt
	// ordinal), executed synchronously through the identical pipeline. The
	// enqueue's conflict semantics replace the retired manual_rebill_attempts
	// claim — a crash between charge and lifecycle update re-derives the same
	// key and gets the durable outcome back instead of double-charging.
	genSub, err := w.DB.Gen(ctx).GetSubscriptionByID(ctx, sub.ID)
	if err != nil {
		logEntry.WithError(err).Warn("Dunning: failed to load subscription merchant for rebill intent")
		return dunningOutcomeFailed
	}
	attemptOrdinal := 0
	if sub.RetryAttempts != nil {
		attemptOrdinal = *sub.RetryAttempts
	}
	windowEnd := periodEnd.Add(window)
	intent, err := w.intentRunner().EnqueueAndExecute(ctx, intents.EnqueueParams{
		MerchantID:     genSub.MerchantID,
		Provider:       providerKey,
		IntentType:     intents.TypeManualRebill,
		SubscriptionID: &sub.ID,
		PspID:          sub.PspID,
		Payload: intents.ManualRebillPayload{
			SubscriptionID: sub.ID,
			PeriodEnd:      periodEnd,
			Rail:           railName,
			OrderReference: orderReference,
			Attempt:        attemptOrdinal,
		},
		IdempotencyKey: intents.ManualRebillIdempotencyKey(sub.ID, periodEnd, railName, orderReference, attemptOrdinal),
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
			return w.applySuccessfulRebill(ctx, logEntry, sub, lifecycle, priceSvc, moneySvc, rail, txnID)
		}
		logEntry.Info("Dunning: rebill successful")
		return dunningOutcomeSucceeded

	case intents.StatusFailedTerminal:
		// Declined (now, or durably on an earlier pass — the evidence carries
		// the response code either way): classify and apply the failure
		// policy. The next scheduled retry derives a fresh attempt ordinal.
		return w.applyDeclinedRebill(ctx, logEntry, sub, lifecycle, rail, intent)

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
	rail models.Rail,
	intent gen.OpenrailsRailIntent,
) dunningOutcome {
	reason := normalize.FromPtr(intent.LastFailureReason)
	if reason == "" {
		reason = "rebill declined"
	}
	responseCode := manualRebillEvidenceResponseCode(intent)
	var failureCode *string
	if responseCode != 0 {
		code := fmt.Sprintf("%d", responseCode)
		failureCode = &code
	}

	// or#870: ONE classifier, three outcomes. Unknown codes land in bucket 1.
	outcome := collection.ClassifyDecline(string(rail), normalize.FromPtr(failureCode))

	// #821/#839: name the evidence leg that would justify a terminal outcome.
	// Only bucket 3 does — the issuer has withdrawn the mandate or the
	// instrument is permanently dead. Bucket 2 stops charging WITHOUT
	// terminating, so it names nothing; bucket 1 names nothing either and
	// FailMembership derives the dunning-exhausted leg from recorded attempts
	// if the schedule runs out.
	certainty := ""
	if outcome == collection.DeclineNonRecoverable {
		certainty = collection.CertaintyNonRetryableDecline
	}

	// #836: the operator kill switch, read on this merchant-scoped connection.
	// When it is off, no terminal outcome (local cancel + entitlement revoke +
	// the queued irreversible provider delete) executes — the row parks instead.
	blocked := ""
	if v := destructive.New(w.DB).Check(ctx, sub.MerchantID); !v.Allowed {
		blocked = v.Reason
	}

	declineLog := logEntry.WithFields(log.Fields{
		"response_code":      responseCode,
		"reason":             reason,
		"decline_outcome":    outcome.String(),
		"terminal_certainty": certainty,
		"terminal_blocked":   blocked,
	})
	switch {
	case blocked != "":
		declineLog.Warn("Dunning: decline recorded but destructive actions are gated; no terminal cancellation will execute — " + blocked)
	case outcome == collection.DeclineNonRecoverable:
		declineLog.Error("Dunning: non-recoverable decline (or#870 bucket 3); cancelling the subscription at the rail — the stored payment method is NOT touched")
	case outcome == collection.DeclineFixPaymentMethod:
		declineLog.Warn("Dunning: customer's card needs fixing (or#870 bucket 2); charging STOPS, subscription and entitlements retained, update-payment-method notice sent")
	default:
		declineLog.Warn("Dunning: retryable decline (or#870 bucket 1); will retry on schedule")
	}

	if err := lifecycle.FailMembership(ctx, &subscriptions.FailMembershipParams{
		Rail:                rail,
		SubscriptionID:      &sub.ID,
		FailureReason:       &reason,
		FailureCode:         failureCode,
		Decline:             outcome,
		RecordFailedAttempt: true,
		TerminalCertainty:   certainty,
		TerminalBlocked:     blocked,
	}); err != nil {
		logEntry.WithError(err).Warn("apply failure policy after declined rebill")
	}
	return dunningOutcomeFailed
}

// manualRebillEvidenceString reads one string field off the intent's
// result_evidence.
func manualRebillEvidenceString(intent gen.OpenrailsRailIntent, key string) string {
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
func manualRebillEvidenceResponseCode(intent gen.OpenrailsRailIntent) int {
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

func (w *DunningWorker) applySuccessfulRebill(
	ctx context.Context,
	logEntry *log.Entry,
	sub *models.Subscription,
	lifecycle *subscriptions.SubscriptionLifecycleService,
	priceSvc *catalog.PriceService,
	moneySvc *money.MoneyService,
	rail models.Rail,
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
		Rail:               rail,
		RailSubscriptionID: sub.RailSubscriptionID,
		TransactionID:      transactionID,
		Amount:             amount,
		Currency:           currency,
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

// subscriptionProviderAutoBilled reports whether the provider bills this
// subscription on its own side, so OpenRails must not manual-rebill or terminate
// it (#635). Registry-backed (#669); see rails.Descriptor.AutoBilled.
func subscriptionProviderAutoBilled(rail string, pm *models.PaymentMethod) bool {
	return rails.AutoBilled(models.Rail(rail), pm)
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
