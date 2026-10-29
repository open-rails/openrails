package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"

	"database/sql"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/catalog/providerobjects"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/parking"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/pspimport"
	"github.com/open-rails/openrails/internal/reconcile"
)

// NMIConvergeService fetch-and-converges one NMI subscription: it probes
// provider truth (query.php sale actions by order reference + the v5 recurring
// GET), then
//  1. activation leg: a pending row (signup awaiting settlement) activates
//     only off a fetched settled charge; a fetched decline fails it. The
//     decider does not own pending.
//  2. decider convergence: reconcile.Decide/ApplyDecision over the probe
//     snapshot for active/past_due/unknown rows. NMI's E_NOT_FOUND or a
//     tombstone is provider-confirmed gone; any other refusal retries.
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
	// Objects records the plans the subscriptions converge reads name.
	Objects *providerobjects.Observer
	// Findings notifies an operator of the finding a park raises.
	Findings reconcile.FindingNotifier
	// Adopter lands a live subscription OpenRails does not hold.
	Adopter NMIAdopter
}

// NMIAdopter lands one live NMI subscription OpenRails does not hold, on its
// plan's provider price, for the vault's one owner.
type NMIAdopter interface {
	AdoptNMISubscription(ctx context.Context, pspID uuid.UUID, sub nmi.V5Subscription, customer uuid.UUID, price *models.Price) error
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
			return s.convergeUnheld(ctx, rail, reference, now)
		}
		return uuid.Nil, fmt.Errorf("nmi converge: resolve %q: %w", reference, err)
	}
	return s.convergeHeld(ctx, rail, sub, now)
}

// convergeHeld converges a subscription OpenRails holds to fetched truth.
func (s *NMIConvergeService) convergeHeld(ctx context.Context, rail string, sub *models.Subscription, now time.Time) (uuid.UUID, error) {
	prober := &reconcile.NMISubscriptionProber{Client: s.NMIClient}
	snap, err := prober.ProbeSubscription(ctx, reconcile.ProbeSubject{
		LocalID:            sub.ID,
		RailSubscriptionID: sub.RailSubscriptionID,
		PeriodStart:        sub.CurrentPeriodStartsAt,
		PeriodEnd:          sub.CurrentPeriodEndsAt,
		ObservedAt:         now,
	})
	if err != nil {
		// Provider API down: the queued job IS the dirty mark — retry later.
		return sub.CustomerID, fmt.Errorf("nmi converge: probe %s: %w", sub.RailSubscriptionID, err)
	}
	for _, remote := range snap.Subscriptions {
		if err := observePrice(ctx, s.Objects, remote.PlanID, nil); err != nil {
			log.WithContext(ctx).WithError(err).WithField("plan", remote.PlanID).Warn("nmi converge: recording an unseen plan failed; the next read records it")
		}
	}

	periodEnd, fetched := sub.CurrentPeriodEndsAt, len(snap.Transactions)
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
		"period_end":           periodEnd,
		"fetched_transactions": fetched,
	}).Info("nmi converge: decided from fetched truth")

	// Held now: a park left from before it landed is done.
	return sub.CustomerID, closePark(ctx, s.DB, sub.RailSubscriptionID, billing.ParkConverged, now)
}

// convergeUnheld converges a reference that resolves to nothing local: a
// one-time sale or order id (the v5 GET answers E_NOT_FOUND or a tombstone)
// is dropped; a live subscription OpenRails does not hold is adopted when its
// plan is mapped and its vault has exactly one owner, and parked otherwise.
// One naming no plan raises a finding instead. Price is checked before
// customer, and nothing is guessed.
func (s *NMIConvergeService) convergeUnheld(ctx context.Context, rail, reference string, now time.Time) (uuid.UUID, error) {
	remote, found, err := s.NMIClient.GetSubscription(ctx, reference)
	if err != nil {
		return uuid.Nil, fmt.Errorf("nmi converge: read unheld %s: %w", reference, err)
	}
	if !found {
		// One-time sale, order id, or a subscription NMI deleted: the purchase
		// path and the import own those.
		log.WithContext(ctx).WithFields(log.Fields{"rail": rail, "subscription_reference": reference}).
			Info("nmi converge: reference resolves to no local or live provider subscription; nothing to converge")
		return uuid.Nil, closePark(ctx, s.DB, reference, billing.ParkGone, now)
	}
	if book := remote.BookID(); book != "" {
		// An OpenRails book made it: this one (its purchase stamps it) or
		// another sharing the gateway. Neither is adopted.
		log.WithContext(ctx).WithFields(log.Fields{"rail": rail, "subscription_reference": reference, "book": book}).
			Info("nmi converge: unheld subscription carries a book id; an OpenRails purchase made it, nothing to adopt")
		return uuid.Nil, nil
	}
	pspID, err := db.RequirePSPID(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	var plan string
	var stated *providerobjects.Object
	if remote.Plan != nil {
		if obj := providerobjects.NMIPlan(*remote.Plan); obj.Ref != "" {
			obj.Evidence, _ = json.Marshal(map[string]any{"source": "nmi_subscription_plan", "subscription": reference, "plan": remote.Plan})
			plan, stated = obj.Ref, &obj
		}
	}
	vault := strings.TrimSpace(remote.CustomerVaultID)
	subject := parking.Subject{PSPID: pspID, Rail: rail, SubscriptionRef: reference, PriceRef: plan, CustomerRef: vault, Unheld: true,
		Evidence: map[string]any{"plan": plan, "vault": vault, "amount": remote.Amount, "next_billing_date": remote.NextBillingDate}}
	if plan == "" {
		return uuid.Nil, raisePlanless(ctx, s.DB, s.Findings, subject)
	}
	if err := observePrice(ctx, s.Objects, plan, stated); err != nil {
		return uuid.Nil, fmt.Errorf("nmi converge: observe plan %s: %w", plan, err)
	}
	price, err := s.PriceService.GetByNMIPlan(ctx, rail, plan)
	if db.IsNotFound(err) {
		subject.Reason = billing.ParkPriceUnmapped
		return uuid.Nil, parkSubscription(ctx, s.DB, s.Findings, subject, now)
	} else if err != nil {
		return uuid.Nil, fmt.Errorf("nmi converge: plan %s lookup: %w", plan, err)
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	var owners []uuid.UUID
	if vault != "" {
		if owners, err = s.DB.Gen(ctx).ListVaultOwners(ctx, gen.ListVaultOwnersParams{MerchantID: mid.UUID(), PspID: pspID, Vault: vault}); err != nil {
			return uuid.Nil, err
		}
	}
	if len(owners) != 1 {
		subject.Reason, subject.Evidence["vault_owners"] = billing.ParkCustomerUnlinked, len(owners)
		return uuid.Nil, parkSubscription(ctx, s.DB, s.Findings, subject, now)
	}
	if owned, err := parking.ImportOwns(ctx, s.DB.Gen(ctx), pspID, reference); err != nil {
		return uuid.Nil, err
	} else if owned {
		log.WithContext(ctx).WithFields(log.Fields{"rail": rail, "subscription_reference": reference}).
			Info("nmi converge: unheld subscription is its PSP import's while it pulls or stages it")
		return uuid.Nil, nil
	}
	if held, err := s.SubscriptionService.GetActiveOrPendingByUserIDAndProductID(ctx, owners[0].String(), price.ProductID); err == nil && held != nil {
		subject.Reason, subject.HeldBy = billing.ParkProductHeld, &held.ID
		subject.Evidence["customer_id"], subject.Evidence["held_by"] = owners[0], held.ID
		return uuid.Nil, parkSubscription(ctx, s.DB, s.Findings, subject, now)
	} else if err != nil && !db.IsNotFound(err) {
		return uuid.Nil, fmt.Errorf("nmi converge: held product of %s: %w", owners[0], err)
	}
	if s.Adopter == nil {
		return uuid.Nil, fmt.Errorf("nmi converge: adoption is not wired")
	}
	// It lands on the price converge resolved its plan to: one meaning of
	// mapped for parking and adoption.
	if err := s.Adopter.AdoptNMISubscription(ctx, pspID, remote, owners[0], price); errors.Is(err, pspimport.ErrProductHeld) {
		subject.Reason, subject.Evidence["customer_id"] = billing.ParkProductHeld, owners[0]
		return uuid.Nil, parkSubscription(ctx, s.DB, s.Findings, subject, now)
	} else if err != nil {
		return uuid.Nil, fmt.Errorf("nmi converge: adopt %s: %w", reference, err)
	}
	log.WithContext(ctx).WithFields(log.Fields{"rail": rail, "subscription_reference": reference, "customer_id": owners[0]}).
		Info("nmi converge: adopted a live subscription; converging it as held")
	sub, err := resolveNMISubscriptionByReference(ctx, rail, s.SubscriptionService, s.PaymentService, reference)
	if err != nil {
		return uuid.Nil, fmt.Errorf("nmi converge: load adopted %s: %w", reference, err)
	}
	return s.convergeHeld(ctx, rail, sub, now)
}

// resolveNMISubscriptionByReference resolves an NMI event reference to the
// local subscription: rail subscription id first, then the order-id metadata
// stamped at signup, then the signup attempt's payment metadata. Unresolved is
// a db.IsNotFound error.
func resolveNMISubscriptionByReference(ctx context.Context, rail string, subSvc *subscriptions.SubscriptionService, paySvc *payments.PaymentService, reference string) (*models.Subscription, error) {
	if subSvc == nil {
		return nil, fmt.Errorf("subscription service is required")
	}
	ref := strings.TrimSpace(reference)
	if ref == "" {
		return nil, sql.ErrNoRows
	}

	subscription, err := subSvc.GetByPSPSubscriptionID(ctx, rail, ref)
	if err == nil {
		return subscription, nil
	} else if !db.IsNotFound(err) {
		return nil, fmt.Errorf("load subscription by rail subscription ID: %w", err)
	}

	subscription, err = subSvc.GetByGatewayOrder(ctx, rail, ref)
	if err == nil {
		return subscription, nil
	}
	if !db.IsNotFound(err) {
		return nil, fmt.Errorf("load subscription by NMI order metadata: %w", err)
	}

	if paySvc != nil {
		attempt, lookupErr := paySvc.GetByNMISubscriptionOrder(ctx, ref)
		if lookupErr != nil && !db.IsNotFound(lookupErr) {
			return nil, fmt.Errorf("load NMI subscription attempt by order metadata: %w", lookupErr)
		}
		if lookupErr == nil && attempt != nil {
			providerSubID := strings.TrimSpace(fmt.Sprint(attempt.Metadata["provider_subscription_id"]))
			if providerSubID != "" {
				return subSvc.GetByPSPSubscriptionID(ctx, rail, providerSubID)
			}
		}
	}

	return nil, sql.ErrNoRows
}
