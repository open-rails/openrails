package webhooks

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/normalize"
)

// StripeConvergeService is the #684 fetch-and-converge implementation for one
// Stripe subscription: GET the subscription (+ latest invoice) through the
// stripeapi choke point, then
//  1. creation leg — no local row yet: create the membership from FETCHED
//     identity (subscription metadata / items / latest invoice), never from a
//     webhook payload;
//  2. mirror facts — fetch-sourced row facts outside the decider's vocabulary
//     (price remap, scheduled-cancel/resume marks);
//  3. decider convergence — reconcile.Decide/ApplyDecision over the fetched
//     snapshot (renewals, failures, provider-confirmed-gone), with payment
//     backfill as the money record.
type StripeConvergeService struct {
	DB                           *db.DB
	Clock                        clockwork.Clock
	Prober                       subscriptions.StripeLivenessProber
	PriceService                 *catalog.PriceService
	ProductService               *catalog.ProductService
	SubscriptionService          *subscriptions.SubscriptionService
	SubscriptionLifecycleService *subscriptions.SubscriptionLifecycleService
	PaymentService               *payments.PaymentService
	MoneyService                 *money.MoneyService
	NotificationService          *subscriptions.NotificationService
	RailCustomerService          *payments.RailCustomerService
	CheckoutSessionService       webhookCheckoutSessionStore
}

func (s *StripeConvergeService) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now()
	}
	return time.Now()
}

// Converge fetches provider truth for one Stripe subscription and converges
// the local row to it. Idempotent; any error is retryable (the job backs off
// and the row keeps its current state — access intact, #664 posture). Returns
// the affected customer id (uuid.Nil when nothing local was touched) so the
// caller can run the inline convergence pass.
func (s *StripeConvergeService) Converge(ctx context.Context, railSubID string) (uuid.UUID, error) {
	railSubID = strings.TrimSpace(railSubID)
	if railSubID == "" {
		return uuid.Nil, fmt.Errorf("stripe converge: rail subscription id is required")
	}
	if s.Prober == nil {
		return uuid.Nil, fmt.Errorf("stripe converge: prober not configured")
	}
	now := s.now().UTC()

	rec, err := s.Prober.ProbeSubscription(ctx, railSubID)
	if err != nil {
		// Provider API down: the queued job IS the dirty mark — retry later.
		return uuid.Nil, fmt.Errorf("stripe converge: fetch %s: %w", railSubID, err)
	}

	sub, err := s.SubscriptionService.GetByRailSubscriptionID(ctx, string(models.RailStripe), railSubID)
	if err != nil {
		if !db.IsNotFound(err) {
			return uuid.Nil, fmt.Errorf("stripe converge: load local subscription: %w", err)
		}
		if !rec.Found {
			log.WithContext(ctx).WithField("rail_subscription_id", railSubID).
				Info("stripe converge: subscription unknown locally and gone at Stripe; nothing to do")
			return uuid.Nil, nil
		}
		return s.createFromFetchedRecord(ctx, railSubID, rec, now)
	}

	if rec.Found {
		if err := s.applyFetchedMirrorFacts(ctx, railSubID, rec, now); err != nil {
			return sub.CustomerID, err
		}
		// Reload: the mirror facts may have moved price/cancel marks.
		if reloaded, rerr := s.SubscriptionService.GetByRailSubscriptionID(ctx, string(models.RailStripe), railSubID); rerr == nil {
			sub = reloaded
		}
	}

	snap := reconcile.StripeSnapshotFromLiveness(railSubID, rec, now)
	res, err := reconcile.ConvergeSubscriptionFromSnapshot(ctx, s.DB, s.SubscriptionLifecycleService, sub, snap, now, 0)
	if err != nil {
		return sub.CustomerID, fmt.Errorf("stripe converge: %w", err)
	}
	log.WithContext(ctx).WithFields(log.Fields{
		"rail_subscription_id": railSubID,
		"transition":           res.Decision.Kind.String(),
		"reason":               res.Decision.Reason,
		"applied":              res.Applied,
		"backfilled":           res.Backfilled,
	}).Info("stripe converge: decided from fetched truth")

	return sub.CustomerID, afterConvergeTransition(ctx, convergeDeps{
		MoneyService:        s.MoneyService,
		SubscriptionService: s.SubscriptionService,
		NotificationService: s.NotificationService,
	}, sub, res, now)
}

// createFromFetchedRecord is the signup/first-invoice leg: a Stripe checkout
// subscription has no local row until its first paid invoice. All facts come
// from the FETCHED record (metadata stamped by checkout, latest invoice).
func (s *StripeConvergeService) createFromFetchedRecord(ctx context.Context, railSubID string, rec subscriptions.StripeLivenessRecord, now time.Time) (uuid.UUID, error) {
	userID := normalize.FirstNonEmpty(rec.Metadata["user_id"], rec.Metadata["userId"], rec.Metadata["uid"])
	if userID == "" && s.RailCustomerService != nil && rec.CustomerID != "" {
		if uid, err := s.RailCustomerService.GetUserIDByCustomerID(ctx, string(models.RailStripe), rec.CustomerID); err == nil {
			userID = strings.TrimSpace(uid)
		}
	}
	if userID == "" {
		// Retryable: checkout.session.completed records the rail-customer
		// mapping; redelivery/retry wins once it lands.
		return uuid.Nil, fmt.Errorf("stripe converge: cannot attribute subscription %s to a user (no metadata user_id, no rail customer mapping)", railSubID)
	}
	if s.RailCustomerService != nil && rec.CustomerID != "" {
		_ = s.RailCustomerService.Upsert(ctx, userID, string(models.RailStripe), rec.CustomerID)
	}

	priceID, price, err := s.resolveFetchedPrice(ctx, rec)
	if err != nil {
		return uuid.Nil, fmt.Errorf("stripe converge: %s: %w", railSubID, err)
	}

	status := strings.ToLower(strings.TrimSpace(rec.Status))
	if !rec.LatestInvoicePaid || (status != "active" && status != "trialing") {
		// Nothing settled yet (incomplete signup, unpaid first invoice) — the
		// row stays uncreated; the next event/pull re-wakes us.
		log.WithContext(ctx).WithFields(log.Fields{
			"rail_subscription_id": railSubID, "status": rec.Status, "invoice_paid": rec.LatestInvoicePaid,
		}).Info("stripe converge: fetched subscription has no settled paid invoice; not creating membership")
		return uuid.Nil, nil
	}
	if err := validateStripeFetchedInvoicePrice(rec, price); err != nil {
		return uuid.Nil, fmt.Errorf("stripe converge: %s: %w", railSubID, err)
	}

	transactionID := rec.LatestInvoiceTransactionID
	if recorded, err := s.fetchedInvoicePaymentAlreadyRecorded(ctx, rec); err != nil {
		return uuid.Nil, err
	} else if recorded {
		// Cross-key dedup (#675): the payment already exists under another key
		// (backfill/charge id vs invoice id) — extend without a second row.
		transactionID = ""
	}

	var purchasedAt *time.Time
	if !rec.LatestInvoiceCreated.IsZero() {
		t := rec.LatestInvoiceCreated
		purchasedAt = &t
	}
	paymentMetadata := map[string]any{}
	if rec.LatestInvoiceID != "" {
		paymentMetadata["stripe_invoice_id"] = rec.LatestInvoiceID
	}

	sub, err := s.SubscriptionLifecycleService.CreateMembership(ctx, &subscriptions.CreateMembershipParams{
		UserID:                userID,
		PriceID:               priceID,
		Rail:                  models.RailStripe,
		RailSubscriptionID:    &railSubID,
		CurrentPeriodStartsAt: zeroTimePtr(rec.CurrentPeriodStart),
		CurrentPeriodEndsAt:   zeroTimePtr(rec.CurrentPeriodEnd),
		TransactionID:         transactionID,
		Amount:                moneyutil.CentsToMicros(rec.LatestInvoiceAmountPaid),
		AmountProvided:        true,
		Currency:              rec.LatestInvoiceCurrency,
		PurchasedAt:           purchasedAt,
		PaymentMetadata:       paymentMetadata,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("stripe converge: create membership %s: %w", railSubID, err)
	}
	log.WithContext(ctx).WithFields(log.Fields{
		"rail_subscription_id": railSubID, "subscription_id": sub.ID, "user_id": userID,
	}).Info("stripe converge: membership created from fetched record")

	// Initial credit lot (idempotent per (subscription, label, period_end)).
	if s.MoneyService != nil && sub.CurrentPeriodEndsAt != nil && !sub.CurrentPeriodEndsAt.IsZero() {
		if err := s.MoneyService.GrantSubscriptionCredits(ctx, money.GrantSubscriptionCreditsParams{
			SubscriptionID: sub.ID,
			PeriodEnd:      sub.CurrentPeriodEndsAt.UTC(),
			Cadence:        models.CreditGrantCadenceOnce,
			Source:         "subscription_initial",
		}); err != nil {
			return sub.CustomerID, fmt.Errorf("stripe converge: grant initial subscription credits: %w", err)
		}
	}

	s.markCheckoutSessionSucceeded(ctx, rec, userID, priceID, transactionID, sub.ID)
	return sub.CustomerID, nil
}

// resolveFetchedPrice maps the fetched record to a local price: metadata
// internal_price_id (checkout-stamped) first, then the Stripe price id.
func (s *StripeConvergeService) resolveFetchedPrice(ctx context.Context, rec subscriptions.StripeLivenessRecord) (uuid.UUID, *models.Price, error) {
	if idStr := normalize.FirstNonEmpty(rec.Metadata["internal_price_id"], rec.Metadata["price_id"]); idStr != "" {
		if priceID, err := uuid.Parse(idStr); err == nil {
			price, err := s.PriceService.GetByID(ctx, priceID)
			if err != nil {
				return uuid.Nil, nil, fmt.Errorf("price lookup failed: %w", err)
			}
			return priceID, price, nil
		}
	}
	if rec.PriceID != "" {
		price, err := s.PriceService.GetByStripePriceID(ctx, rec.PriceID)
		if err != nil {
			return uuid.Nil, nil, fmt.Errorf("stripe price %s not mapped", rec.PriceID)
		}
		return price.ID, price, nil
	}
	return uuid.Nil, nil, fmt.Errorf("unable to resolve price from fetched subscription")
}

// validateStripeFetchedInvoicePrice ports validateStripeInvoicePrice onto the
// fetched record: exact list-price match except for proration invoices
// (billing_reason=subscription_update — Model B upgrades) and settled
// zero-amount invoices (trials).
func validateStripeFetchedInvoicePrice(rec subscriptions.StripeLivenessRecord, price *models.Price) error {
	if price == nil {
		return fmt.Errorf("fetched invoice price is required for validation")
	}
	if !strings.EqualFold(strings.TrimSpace(rec.LatestInvoiceCurrency), strings.TrimSpace(price.Currency)) {
		return fmt.Errorf("fetched invoice currency mismatch: got %s, want %s", rec.LatestInvoiceCurrency, price.Currency)
	}
	if strings.EqualFold(rec.LatestInvoiceBillingReason, "subscription_update") {
		if rec.LatestInvoiceAmountPaid <= 0 {
			return fmt.Errorf("fetched proration invoice has non-positive amount_paid: %d", rec.LatestInvoiceAmountPaid)
		}
		return nil
	}
	amountPaidMicros := moneyutil.CentsToMicros(rec.LatestInvoiceAmountPaid)
	if amountPaidMicros != price.Amount {
		if rec.LatestInvoiceAmountPaid == 0 {
			return nil // settled zero-amount invoice (trial / no_payment_required)
		}
		return fmt.Errorf("fetched invoice amount mismatch: got %d cents (%d micros), want %d micros", rec.LatestInvoiceAmountPaid, amountPaidMicros, price.Amount)
	}
	return nil
}

// fetchedInvoicePaymentAlreadyRecorded is the cross-key dedup for the fetched
// latest invoice: a completed payment may already exist under the charge /
// payment_intent / invoice id, or via stripe_invoice_id metadata.
func (s *StripeConvergeService) fetchedInvoicePaymentAlreadyRecorded(ctx context.Context, rec subscriptions.StripeLivenessRecord) (bool, error) {
	if s.PaymentService == nil {
		return false, nil
	}
	for _, candidate := range []string{rec.LatestInvoiceTransactionID, rec.LatestInvoiceID} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		existing, err := s.PaymentService.GetByTransactionID(ctx, models.RailStripe, candidate)
		if err != nil {
			if db.IsNotFound(err) {
				continue
			}
			return false, fmt.Errorf("stripe converge: dedup fetched invoice payment: %w", err)
		}
		if existing != nil && existing.Status != payments.PaymentStatusFailedValue {
			return true, nil
		}
	}
	if invoiceID := strings.TrimSpace(rec.LatestInvoiceID); invoiceID != "" {
		existing, err := s.PaymentService.GetByMetadataValue(ctx, "stripe_invoice_id", invoiceID)
		if err != nil {
			if db.IsNotFound(err) {
				return false, nil
			}
			return false, fmt.Errorf("stripe converge: dedup fetched invoice payment by metadata: %w", err)
		}
		if existing != nil && existing.Status != payments.PaymentStatusFailedValue {
			return true, nil
		}
	}
	return false, nil
}

// markCheckoutSessionSucceeded closes the open checkout session that started
// this subscription (creation leg only — renewals have no open session).
func (s *StripeConvergeService) markCheckoutSessionSucceeded(ctx context.Context, rec subscriptions.StripeLivenessRecord, userID string, priceID uuid.UUID, transactionID string, subscriptionID uuid.UUID) {
	if s.CheckoutSessionService == nil {
		return
	}
	sessionID := parseCheckoutSessionID(rec.Metadata)
	if sessionID == uuid.Nil {
		if session, err := s.CheckoutSessionService.FindOpenByUserPriceRail(ctx, userID, priceID, models.RailStripe); err == nil && session != nil {
			sessionID = session.ID
		}
	}
	if sessionID == uuid.Nil {
		return
	}
	paymentID := uuid.Nil
	if s.PaymentService != nil && transactionID != "" {
		if payment, err := s.PaymentService.GetByTransactionID(ctx, models.RailStripe, transactionID); err == nil {
			paymentID = payment.ID
		}
	}
	if err := s.CheckoutSessionService.MarkSucceededWithSubscription(ctx, sessionID, paymentID, transactionID, subscriptionID); err != nil {
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"checkout_session_id": sessionID, "transaction_id": transactionID,
		}).Warn("stripe converge: failed to update checkout session")
	}
}

// applyFetchedMirrorFacts writes the fetch-sourced row facts that are OUTSIDE
// the decider's transition vocabulary: the price mapping (Model B upgrades),
// scheduled-cancellation marks (cancel_at_period_end from Stripe's portal),
// and a portal resume clearing them. These are mirror writes of CURRENT
// provider truth — no event ordering, no watermarks.
func (s *StripeConvergeService) applyFetchedMirrorFacts(ctx context.Context, railSubID string, rec subscriptions.StripeLivenessRecord, now time.Time) error {
	var (
		updatedSub          *models.Subscription
		oldEntitlementsSpec map[string]*int
		newEntitlementsSpec map[string]*int
	)
	status := strings.ToLower(strings.TrimSpace(rec.Status))
	remoteAlive := status == "active" || status == "trialing"

	if err := s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		subRepo := subscriptions.NewSubscriptionRepo(db.NewWithPgxTx(tx))
		sub, err := subRepo.GetByRailSubscriptionIDForUpdate(ctx, string(models.RailStripe), railSubID)
		if err != nil {
			if db.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("stripe converge: load subscription for mirror facts: %w", err)
		}
		if _, terminal := subscriptions.TerminalCancelReason(sub); terminal {
			return nil // terminal locally (chargeback etc.): mirror facts never resurrect
		}

		changed := false

		// Price remap (fetch-sourced): the subscription's Stripe price moved.
		if rec.PriceID != "" {
			if price, perr := s.PriceService.GetByStripePriceID(ctx, rec.PriceID); perr == nil {
				if price.ID != sub.PriceID {
					oldEntitlementsSpec = models.CloneEntitlementsSpec(sub.EntitlementsSpecSnapshot)
					if len(oldEntitlementsSpec) == 0 && s.ProductService != nil && sub.ProductID != uuid.Nil {
						if oldProduct, oerr := s.ProductService.GetByID(ctx, sub.ProductID); oerr == nil {
							oldEntitlementsSpec = oldProduct.EntitlementsSpec
						}
					}
					sub.PriceID = price.ID
					sub.ProductID = price.ProductID
					sub.ScheduledPriceID = nil
					// #813: if a scheduled reprice/plan-change row targeted
					// exactly this price, the provider flip IS its
					// application — mark it applied in the same tx so batch
					// progress reflects provider truth. Idempotent by
					// predicate; 0 rows on organic price moves.
					if _, aerr := db.NewWithPgxTx(tx).Gen(ctx).ApplyScheduledRepriceForSubscriptionPrice(ctx, gen.ApplyScheduledRepriceForSubscriptionPriceParams{
						SubscriptionID: sub.ID,
						ToPriceID:      price.ID,
					}); aerr != nil {
						return fmt.Errorf("stripe converge: mark scheduled reprice applied: %w", aerr)
					}
					if s.ProductService != nil {
						if product, perr2 := s.ProductService.GetByID(ctx, price.ProductID); perr2 == nil {
							sub.EntitlementsSpecSnapshot = models.CloneEntitlementsSpec(product.EntitlementsSpec)
							sub.CreditsSpecSnapshot = models.CloneCreditsSpec(product.CreditsSpec)
							newEntitlementsSpec = product.EntitlementsSpec
						}
					}
					changed = true
				}
			} else {
				log.WithContext(ctx).WithFields(log.Fields{
					"stripe_price_id": rec.PriceID, "subscription_id": sub.ID,
				}).Warn("stripe converge: fetched price not mapped locally")
			}
		}

		// Scheduled cancellation (Stripe portal): remote alive but flagged to
		// cancel at period end — mirror the marks, keep the paid-through owner.
		if remoteAlive && rec.CancelAtPeriodEnd && sub.CancelledAt == nil {
			cancelType := models.CancelTypeUser
			sub.CancelType = &cancelType
			ts := now
			if !rec.CanceledAt.IsZero() {
				ts = rec.CanceledAt
			}
			sub.CancelledAt = &ts
			endAt := now
			if sub.CurrentPeriodEndsAt != nil && sub.CurrentPeriodEndsAt.After(now) {
				endAt = *sub.CurrentPeriodEndsAt
			}
			sub.EndedAt = &endAt
			changed = true
		}

		// Portal resume: remote alive, no scheduled cancel, local row still
		// carries cancellation marks from a prior scheduled cancel.
		if remoteAlive && !rec.CancelAtPeriodEnd &&
			(sub.CancelledAt != nil || sub.Status == models.StatusCancelled) {
			sub.CancelledAt = nil
			sub.CancelType = nil
			sub.EndedAt = nil
			if sub.Status == models.StatusCancelled {
				sub.Status = models.StatusActive
			}
			changed = true
		}

		if !changed {
			return nil
		}
		if err := subRepo.UpdateAt(ctx, sub, s.now()); err != nil {
			return fmt.Errorf("stripe converge: write mirror facts: %w", err)
		}
		updatedSub = sub
		return nil
	}); err != nil {
		return err
	}

	// Downgrade revoke: entitlements the old product had that the new one lost.
	if updatedSub != nil && len(oldEntitlementsSpec) > 0 {
		if err := s.revokeDowngradedEntitlements(ctx, updatedSub, oldEntitlementsSpec, newEntitlementsSpec); err != nil {
			return err
		}
	}
	return nil
}

func (s *StripeConvergeService) revokeDowngradedEntitlements(ctx context.Context, sub *models.Subscription, oldSpec, newSpec map[string]*int) error {
	entSvc := entitlements.NewEntitlementService(s.DB, s.Clock)
	oldEnts := stripeEntitlementSet(oldSpec)
	newEnts := stripeEntitlementSet(newSpec)
	sourceType := models.EntitlementSourceSubscription
	sourceID := sub.ID
	for entName := range oldEnts {
		if newEnts[entName] {
			continue
		}
		if err := entSvc.RevokeExistingEntitlement(ctx, entitlements.RevokeExistingEntitlementParams{
			UserID:      sub.CustomerID.String(),
			Entitlement: entName,
			SourceType:  &sourceType,
			SourceID:    &sourceID,
			Reason:      models.EntitlementRevokeDowngrade,
		}); err != nil {
			return fmt.Errorf("stripe converge: revoke downgraded entitlement %s: %w", entName, err)
		}
	}
	return nil
}
