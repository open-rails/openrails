package webhooks

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"

	"database/sql"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// NMIConvergeService is the #684 fetch-and-converge implementation for one NMI
// subscription: probe provider truth (query.php sale actions by order
// reference + the v5 recurring GET — the #665 SubscriptionProber sources), then
//  1. activation leg — a PENDING row (signup awaiting settlement) activates
//     only off a FETCHED settled charge; a fetched decline fails it. The
//     decider deliberately does not own pending (signup path doctrine).
//  2. decider convergence — reconcile.Decide/ApplyDecision over the probe
//     snapshot for active/past_due/unknown rows. An NMI v5 404 is
//     provider-confirmed-gone (#679 certainty).
type NMIConvergeService struct {
	DB                           *db.DB
	Clock                        clockwork.Clock
	Rail                         string
	NMIClient                    *nmi.NMIClient
	PriceService                 *catalog.PriceService
	SubscriptionService          *subscriptions.SubscriptionService
	SubscriptionLifecycleService *subscriptions.SubscriptionLifecycleService
	PaymentService               *payments.PaymentService
	MoneyService                 *money.MoneyService
	NotificationService          *subscriptions.NotificationService
}

func (s *NMIConvergeService) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now()
	}
	return time.Now()
}

// Converge resolves the reference to a local subscription and converges it to
// fetched provider truth. Returns the affected customer id (uuid.Nil when
// nothing local was touched) so the caller can run the inline convergence pass.
func (s *NMIConvergeService) Converge(ctx context.Context, reference string) (uuid.UUID, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return uuid.Nil, fmt.Errorf("nmi converge: subscription reference is required")
	}
	if s.NMIClient == nil {
		return uuid.Nil, fmt.Errorf("nmi converge: nmi client not configured")
	}
	rail := strings.TrimSpace(strings.ToLower(s.Rail))
	if rail == "" {
		return uuid.Nil, fmt.Errorf("nmi converge: rail is required")
	}
	now := s.now().UTC()

	sub, err := resolveNMISubscriptionByReference(ctx, rail, s.SubscriptionService, s.PaymentService, reference)
	if err != nil {
		if db.IsNotFound(err) {
			// One-time sale, foreign object, or the checkout row hasn't landed
			// yet. The webhook was never a state source for those — the
			// checkout path and the pull sweep own them.
			log.WithContext(ctx).WithFields(log.Fields{
				"rail": rail, "subscription_reference": reference,
			}).Info("nmi converge: reference resolves to no local subscription; nothing to converge")
			return uuid.Nil, nil
		}
		return uuid.Nil, fmt.Errorf("nmi converge: resolve %q: %w", reference, err)
	}

	if sub.Status == models.StatusPending {
		return sub.CustomerID, s.activatePendingFromProbe(ctx, rail, sub, now)
	}

	prober := &reconcile.NMISubscriptionProber{Client: s.NMIClient}
	snap, err := prober.ProbeSubscription(ctx, reconcile.ProbeSubject{
		LocalID:            sub.ID,
		RailSubscriptionID: sub.RailSubscriptionID,
		PeriodEnd:          sub.CurrentPeriodEndsAt,
	})
	if err != nil {
		// Provider API down: the queued job IS the dirty mark — retry later.
		return sub.CustomerID, fmt.Errorf("nmi converge: probe %s: %w", sub.RailSubscriptionID, err)
	}

	res, err := reconcile.ConvergeSubscriptionFromSnapshot(ctx, s.DB, s.SubscriptionLifecycleService, sub, snap, now, 0)
	if err != nil {
		return sub.CustomerID, fmt.Errorf("nmi converge: %w", err)
	}
	log.WithContext(ctx).WithFields(log.Fields{
		"rail":                 rail,
		"rail_subscription_id": sub.RailSubscriptionID,
		"transition":           res.Decision.Kind.String(),
		"reason":               res.Decision.Reason,
		"applied":              res.Applied,
		"backfilled":           res.Backfilled,
	}).Info("nmi converge: decided from fetched truth")

	return sub.CustomerID, afterConvergeTransition(ctx, convergeDeps{
		MoneyService:        s.MoneyService,
		SubscriptionService: s.SubscriptionService,
		NotificationService: s.NotificationService,
	}, sub, res, now)
}

// activatePendingFromProbe is the signup leg: a pending NMI subscription
// activates ONLY off a fetched settled charge for its order reference (the
// local id, stamped as orderid/ponumber at signup). A fetched decline fails
// the membership; nothing fetched yet snoozes the job (ErrConvergeRetryLater).
func (s *NMIConvergeService) activatePendingFromProbe(ctx context.Context, rail string, sub *models.Subscription, now time.Time) error {
	if delayedStart := nmiFutureDelayedStart(sub.Metadata, now); delayedStart != nil {
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id": sub.ID, "delayed_start": delayedStart.UTC().Format(time.RFC3339),
		}).Info("nmi converge: checkout delayed_start is in the future; keeping pending")
		return nil
	}

	since := sub.CreatedAt.UTC().Add(-24 * time.Hour)
	probe, err := s.NMIClient.ProbeSalesByOrderID(sub.ID.String(), since)
	if err != nil {
		return fmt.Errorf("nmi converge: probe signup charge for %s: %w", sub.ID, err)
	}

	switch {
	case probe.SuccessFound && probe.SuccessTransactionID != "":
		return s.activateFromSettledCharge(ctx, rail, sub, probe)
	case probe.DeclineFound:
		return s.failPendingFromDecline(ctx, rail, sub, probe)
	default:
		// No charge attempt visible yet — settlement lag. Snooze and re-fetch.
		return fmt.Errorf("%w: pending nmi subscription %s has no fetched charge attempt yet", ErrConvergeRetryLater, sub.ID)
	}
}

func (s *NMIConvergeService) activateFromSettledCharge(ctx context.Context, rail string, sub *models.Subscription, probe nmi.SaleProbeResult) error {
	price, err := s.PriceService.GetByID(ctx, sub.PriceID)
	if err != nil {
		return fmt.Errorf("nmi converge: load price for activation: %w", err)
	}

	// Fetched amount, verbatim; the subscription's price is the declared
	// fallback when the report omitted it (#651: never fabricate).
	amountMicros := price.Amount
	if raw := strings.TrimSpace(probe.SuccessAmount); raw != "" {
		cents, perr := moneyutil.ParseDecimalToCents(raw)
		if perr != nil {
			return fmt.Errorf("nmi converge: unparseable fetched charge amount %q: %w", raw, perr)
		}
		if !nmiAmountMatchesExpected(price.Currency, cents, moneyutil.Micros(price.Amount)) {
			return fmt.Errorf("nmi converge: fetched charge amount %d cents does not match expected price %d micros", cents, price.Amount)
		}
		amountMicros = int64(moneyutil.CentsToMicros(cents))
	}
	currency := normalizeNMICurrencyValue(probe.SuccessCurrency, price.Currency)

	if s.DB != nil {
		removed, err := subscriptions.RemoveCancelledSubscriptionsForActivation(ctx, s.DB, sub.CustomerID.String(), sub.ProductID, sub.ID)
		if err != nil {
			return fmt.Errorf("nmi converge: cleanup cancelled subscriptions before activation: %w", err)
		}
		if removed > 0 {
			log.WithContext(ctx).WithFields(log.Fields{
				"user_id": sub.CustomerID.String(), "removed": removed,
			}).Info("nmi converge: removed cancelled subscriptions before activation")
		}
	}

	var purchasedAt *time.Time
	if !probe.SuccessAt.IsZero() {
		t := probe.SuccessAt
		purchasedAt = &t
	}
	if _, err := s.SubscriptionLifecycleService.CreateMembership(ctx, &subscriptions.CreateMembershipParams{
		PriceID:            sub.PriceID,
		UserID:             sub.CustomerID.String(),
		Rail:               models.Rail(rail),
		RailSubscriptionID: &sub.RailSubscriptionID,
		UserEmail:          sub.UserEmail,
		TransactionID:      probe.SuccessTransactionID,
		Amount:             amountMicros,
		AmountProvided:     true,
		Currency:           currency,
		PurchasedAt:        purchasedAt,
	}); err != nil {
		return fmt.Errorf("nmi converge: activate pending subscription: %w", err)
	}
	log.WithContext(ctx).WithFields(log.Fields{
		"subscription_id":      sub.ID,
		"rail_subscription_id": sub.RailSubscriptionID,
		"transaction_id":       probe.SuccessTransactionID,
	}).Info("nmi converge: pending subscription activated from fetched settled charge")

	if s.MoneyService != nil && s.SubscriptionService != nil {
		updated, err := s.SubscriptionService.GetByID(ctx, sub.ID)
		if err != nil {
			return fmt.Errorf("nmi converge: load subscription for initial credit grants: %w", err)
		}
		if updated.CurrentPeriodEndsAt != nil && !updated.CurrentPeriodEndsAt.IsZero() {
			if err := s.MoneyService.GrantSubscriptionCredits(ctx, money.GrantSubscriptionCreditsParams{
				SubscriptionID: updated.ID,
				PeriodEnd:      updated.CurrentPeriodEndsAt.UTC(),
				Cadence:        models.CreditGrantCadenceOnce,
				Source:         "subscription_initial",
			}); err != nil {
				return fmt.Errorf("nmi converge: grant initial subscription credits: %w", err)
			}
		}
	}
	return nil
}

// failPendingFromDecline records the fetched decline and fails the pending
// membership (FailMembership owns the retry/terminal decision inline).
func (s *NMIConvergeService) failPendingFromDecline(ctx context.Context, rail string, sub *models.Subscription, probe nmi.SaleProbeResult) error {
	if s.PaymentService != nil && probe.DeclineTransactionID != "" {
		existing, err := s.PaymentService.GetByTransactionID(ctx, models.Rail(rail), probe.DeclineTransactionID)
		if err != nil && !db.IsNotFound(err) {
			return fmt.Errorf("nmi converge: lookup fetched decline payment: %w", err)
		}
		if existing == nil || db.IsNotFound(err) {
			amountMicros := int64(0)
			if raw := strings.TrimSpace(probe.DeclineAmount); raw != "" {
				if cents, perr := moneyutil.ParseDecimalToCents(raw); perr == nil {
					amountMicros = int64(moneyutil.CentsToMicros(cents))
				}
			}
			if amountMicros == 0 && sub.Price != nil {
				amountMicros = sub.Price.Amount
			}
			currency := normalizeNMICurrencyValue(probe.DeclineCurrency)
			if currency == "" && sub.Price != nil {
				currency = strings.ToLower(strings.TrimSpace(sub.Price.Currency))
			}
			purchasedAt := s.now()
			if !probe.DeclineAt.IsZero() {
				purchasedAt = probe.DeclineAt
			}
			subID := sub.ID
			failed := &models.Payment{
				ID:             uuidutil.NewV7(),
				CustomerID:     sub.CustomerID,
				PriceID:        sub.PriceID,
				SubscriptionID: &subID,
				Rail:           models.Rail(rail),
				TransactionID:  probe.DeclineTransactionID,
				Amount:         amountMicros,
				ListAmount:     amountMicros,
				Currency:       currency,
				Status:         "failed",
				AttemptKind:    func() *string { k := payments.AttemptInitial; return &k }(),
				PurchasedAt:    purchasedAt,
			}
			if probe.DeclineResponseCode != 0 {
				code := strconv.Itoa(probe.DeclineResponseCode)
				reason := payments.NormalizeFailureReason(rail, code)
				failed.FailureCode = &code
				failed.FailureReason = &reason
			}
			if tt := payments.DefaultTokenTypeForRail(rail); tt != "" {
				failed.TokenType = &tt
			}
			if _, err := s.PaymentService.CreateIfNotExists(ctx, failed); err != nil {
				return fmt.Errorf("nmi converge: record fetched decline: %w", err)
			}
		}
	}

	var failureReason, failureCode *string
	if r := strings.TrimSpace(probe.DeclineReason); r != "" {
		failureReason = &r
	}
	if probe.DeclineResponseCode != 0 {
		code := strconv.Itoa(probe.DeclineResponseCode)
		failureCode = &code
	}
	if err := s.SubscriptionLifecycleService.FailMembership(ctx, &subscriptions.FailMembershipParams{
		Rail:           models.Rail(rail),
		SubscriptionID: &sub.ID,
		FailureReason:  failureReason,
		FailureCode:    failureCode,
	}); err != nil {
		return fmt.Errorf("nmi converge: fail pending membership: %w", err)
	}
	log.WithContext(ctx).WithFields(log.Fields{
		"subscription_id": sub.ID, "decline_transaction_id": probe.DeclineTransactionID,
	}).Info("nmi converge: pending subscription failed from fetched decline")
	return nil
}

// resolveNMISubscriptionByReference resolves an NMI event reference to the
// local subscription: rail subscription id first, then the order-id metadata
// stamped at signup, then the signup attempt's payment metadata. Identity
// resolution only — shared by the converge path and the payload-apply money
// handlers (refund/void). Returns db.IsNotFound-style error when unresolved.
func resolveNMISubscriptionByReference(ctx context.Context, rail string, subSvc *subscriptions.SubscriptionService, paySvc *payments.PaymentService, reference string) (*models.Subscription, error) {
	if subSvc == nil {
		return nil, fmt.Errorf("subscription service is required")
	}
	ref := strings.TrimSpace(reference)
	if ref == "" {
		return nil, sql.ErrNoRows
	}

	subscription, err := subSvc.GetByRailSubscriptionID(ctx, rail, ref)
	if err == nil {
		return subscription, nil
	} else if !db.IsNotFound(err) {
		return nil, fmt.Errorf("load subscription by rail subscription ID: %w", err)
	}

	subscription, err = subSvc.GetByRailMetadataValue(ctx, rail, "order_id", ref)
	if err == nil {
		return subscription, nil
	}
	if !db.IsNotFound(err) {
		return nil, fmt.Errorf("load subscription by NMI order metadata: %w", err)
	}

	if paySvc != nil {
		attempt, lookupErr := paySvc.GetByMetadataValue(ctx, "nmi_subscription_order_id", ref)
		if lookupErr != nil && !db.IsNotFound(lookupErr) {
			return nil, fmt.Errorf("load NMI subscription attempt by order metadata: %w", lookupErr)
		}
		if lookupErr == nil && attempt != nil {
			providerSubID := strings.TrimSpace(fmt.Sprint(attempt.Metadata["provider_subscription_id"]))
			if providerSubID != "" {
				return subSvc.GetByRailSubscriptionID(ctx, rail, providerSubID)
			}
		}
	}

	return nil, sql.ErrNoRows
}
