package subscriptions

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// RenewalCharger drives an ENGINE-INITIATED recurring MIT charge through the
// #297 Phase A stored-credential seam (internal/modules/payments/charge), for
// a subscription whose payment method already carries a recurring stored-
// credential anchor (payment_methods.stored_credential_recurring_ref).
//
// This is the "renewal job checks for a due scheduled reprice at the
// boundary, re-pins the subscription, then charges" mechanism #773 asks for,
// made concrete for rails where OpenRails' own engine decides the renewal
// amount and initiates the charge — as opposed to NMI's/Stripe's own native
// recurring-plan engines, which OpenRails only OBSERVES via webhook/converge
// (see RenewMembership's normal-renewal fallback and the Solana crank for
// those). The #297 Phase A MIT context rides along UNCHANGED: same
// stored-credential anchor, only the amount differs — permitted when
// disclosed, which RepriceService's schedule-time notification is for.
type RenewalCharger struct {
	Charger   charge.Charger
	Reprice   *RepriceService
	Lifecycle *SubscriptionLifecycleService
	Clock     clockwork.Clock
	// DB, when set, persists a captured stored-credential reference (#297
	// write-once backfill) after a successful charge on a legacy/never-
	// anchored instrument. Nil is fine for tests that construct a PaymentMethod
	// in memory with no backing row.
	DB *db.DB
}

func (rc *RenewalCharger) now() time.Time {
	return timeutil.FirstClock(rc.Clock).Now().UTC()
}

// ChargeRenewal resolves the subscription's effective price (applying any due
// #773 reprice), charges it via a recurring MIT against pm's stored-credential
// anchor, and records the renewal through RenewMembership on approval. A
// declined or transport-failed charge returns the error without touching
// lifecycle state (the subscription's existing dunning path handles retries).
func (rc *RenewalCharger) ChargeRenewal(ctx context.Context, sub *models.Subscription, pm *models.PaymentMethod) (charge.Result, error) {
	if rc.Charger == nil || rc.Reprice == nil || rc.Lifecycle == nil {
		return charge.Result{}, fmt.Errorf("renewal charger: not fully configured")
	}
	price, _, err := rc.Reprice.ResolveEffectivePrice(ctx, sub.ID)
	if err != nil {
		return charge.Result{}, fmt.Errorf("renewal charge: resolve effective price: %w", err)
	}
	amountCents, err := moneyutil.MicrosToCentsExact(price.Amount)
	if err != nil {
		return charge.Result{}, fmt.Errorf("renewal charge: %w", err)
	}
	priorRef := strings.TrimSpace(pm.StoredCredentialRecurringRef)
	now := rc.now()
	res, err := rc.Charger.Charge(ctx, charge.Request{
		Instrument: charge.Instrument{
			PaymentMethodID: pm.ID,
			Rail:            string(pm.Rail),
			CustomerRef:     strings.TrimSpace(pm.RailCustomerRef),
			MethodRef:       strings.TrimSpace(pm.RailMethodRef),
		},
		AmountMinor: moneyutil.Cents(amountCents),
		Currency:    price.Currency,
		Description: "subscription renewal",
		OrderRef:    "sub-renewal-" + sub.ID.String() + "-" + now.Format("20060102"),
		Context:     charge.RecurringMIT(priorRef),
	})
	if err != nil {
		return charge.Result{}, fmt.Errorf("renewal charge: %w", err)
	}
	if res.Declined {
		msg := "declined"
		if res.FailureMessage != nil {
			msg = *res.FailureMessage
		}
		return res, fmt.Errorf("renewal charge declined: %s", msg)
	}
	// #297: a successful charge on an instrument with no recurring stored-
	// credential reference anchors its sequence — persist write-once.
	// Best-effort: the charge itself succeeded and must never fail on this.
	if rc.DB != nil {
		if ref := strings.TrimSpace(res.CapturedRef); ref != "" {
			if _, cerr := rc.DB.Gen(ctx).CaptureStoredCredentialRef(ctx, gen.CaptureStoredCredentialRefParams{
				MerchantID: sub.MerchantID,
				ID:         pm.ID,
				Agreement:  string(charge.AgreementRecurring),
				Ref:        ref,
			}); cerr != nil {
				log.WithContext(ctx).WithError(cerr).WithField("payment_method_id", pm.ID).
					Warn("failed to persist captured stored-credential reference (#297); next charge re-captures")
			}
		}
	}
	if err := rc.Lifecycle.RenewMembership(ctx, &RenewMembershipParams{
		Rail:                  models.Rail(pm.Rail),
		RailSubscriptionID:    sub.RailSubscriptionID,
		TransactionID:         res.TransactionID,
		Amount:                price.Amount,
		AmountProvided:        true,
		Currency:              price.Currency,
		PurchasedAt:           &now,
		CurrentPeriodStartsAt: &now,
	}); err != nil {
		return res, fmt.Errorf("renewal charge: record renewal: %w", err)
	}
	return res, nil
}
