package intents

import (
	"context"
	"errors"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// EnqueueScheduled accepts one immutable dunning attempt while holding the
// subscription lock. The same operation owns preparation, submission, recovery
// and lifecycle effects; there is no second dunning lease to infer ownership.
func (h *ManualRebillHandler) EnqueueScheduled(ctx context.Context, subscriptionID uuid.UUID) (gen.OpenrailsRailIntent, error) {
	row, _, err := h.enqueueRebill(ctx, subscriptionID, uuid.Nil, "", nil)
	return row, err
}

var (
	ErrRebillNotRetryable = errors.New("subscription is not retryable")
	ErrRebillInProgress   = errors.New("subscription has an unresolved payment operation")
	ErrRebillKeyConflict  = errors.New("retry key belongs to a different accepted request")
	ErrRebillUnsupported  = errors.New("customer-present retry is unsupported for this subscription")
)

// EnqueueCustomer shares the scheduled admission lock and ownership. The HTTP
// command supplies payer only after verifying a payer-scoped customer action.
func (h *ManualRebillHandler) EnqueueCustomer(ctx context.Context, subscriptionID, payer uuid.UUID, clientKey string, method *uuid.UUID) (gen.OpenrailsRailIntent, bool, error) {
	if payer == uuid.Nil || strings.TrimSpace(clientKey) == "" || len(clientKey) > 255 {
		return gen.OpenrailsRailIntent{}, false, errors.New("payer and a 1-255 byte idempotency key required")
	}
	if method != nil && *method == uuid.Nil {
		return gen.OpenrailsRailIntent{}, false, errors.New("payment method is invalid")
	}
	return h.enqueueRebill(ctx, subscriptionID, payer, CustomerPaymentKey(TypeManualRebill, payer, strings.TrimSpace(clientKey)), method)
}

func (h *ManualRebillHandler) enqueueRebill(ctx context.Context, subscriptionID, payer uuid.UUID, customerKey string, requestedMethod *uuid.UUID) (gen.OpenrailsRailIntent, bool, error) {
	var accepted gen.OpenrailsRailIntent
	replayed := false
	customer := customerKey != ""
	now := h.now()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return accepted, replayed, err
	}
	err = h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.DB.NewWithPgxTx(tx)
		sub, err := subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, subscriptionID)
		if err != nil {
			return err
		}
		if customer {
			if sub.CustomerID != payer {
				return pgx.ErrNoRows
			}
			prior, err := d.Gen(ctx).GetRailIntentByIdempotencyKey(ctx, gen.GetRailIntentByIdempotencyKeyParams{MerchantID: mid.UUID(), IdempotencyKey: customerKey})
			if err == nil {
				p, err := DecodeManualRebillPayload(prior)
				if err != nil {
					return err
				}
				if p.Renewal.SubscriptionID != subscriptionID || p.Renewal.CustomerID != payer || p.Initiator != charge.InitiatorCustomer || !sameOptionalID(p.RequestedPaymentMethodID, requestedMethod) {
					return ErrRebillKeyConflict
				}
				accepted, replayed = prior, true
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		// An unresolved attempt retains ownership even if a webhook or an
		// operator has changed the current lifecycle meanwhile.
		active, err := d.Gen(ctx).GetUnresolvedManualRebill(ctx, gen.GetUnresolvedManualRebillParams{MerchantID: mid.UUID(), SubscriptionID: subscriptionID})
		if err == nil {
			if customer {
				return ErrRebillInProgress
			}
			accepted = active
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if sub.Status != models.StatusPastDue || sub.CurrentPeriodEndsAt == nil || (!customer && sub.NextRetryAt != nil && sub.NextRetryAt.After(now)) {
			return ErrRebillNotRetryable
		}
		ordinal := 0
		previous, err := d.Gen(ctx).GetLatestManualRebillForPeriod(ctx, gen.GetLatestManualRebillForPeriodParams{MerchantID: mid.UUID(), SubscriptionID: subscriptionID, PeriodStart: sub.CurrentPeriodEndsAt.UTC()})
		if err == nil {
			prior, err := DecodeManualRebillPayload(previous)
			if err != nil {
				return err
			}
			ordinal = prior.Attempt + 1
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		failures := 0
		if sub.RetryAttempts != nil {
			failures = *sub.RetryAttempts
		}
		key := ManualRebillIdempotencyKey(sub.ID, *sub.CurrentPeriodEndsAt, string(sub.Rail), ordinal)
		initiator, origin, reason := charge.InitiatorMerchant, OriginSystem, "scheduled recurring recovery"
		actor := ""
		if customer {
			key, initiator, origin, reason = customerKey, charge.InitiatorCustomer, OriginUser, "verified customer subscription retry"
			actor = payer.String()
		}
		store := NewStore(d)
		terms, err := subscriptions.PrepareRenewalTerms(ctx, d, sub, now)
		if err != nil {
			return err
		}
		if customer && terms.PeriodEnd.Sub(terms.PeriodStart)%(24*time.Hour) != 0 {
			return ErrRebillUnsupported
		}
		if requestedMethod != nil && (sub.PaymentMethodID == nil || *requestedMethod != *sub.PaymentMethodID) {
			return ErrRebillUnsupported
		}
		if sub.PaymentMethodID == nil {
			return errors.New("rebill has no payment method")
		}
		methodRow, err := d.Gen(ctx).GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: mid.UUID(), ID: *sub.PaymentMethodID})
		if err != nil {
			return err
		}
		method, err := models.PaymentMethodFromGen(methodRow)
		if err != nil {
			return err
		}
		if method.CustomerID != sub.CustomerID || method.PspID != sub.PspID || method.Rail != sub.Rail {
			return errors.New("rebill method is not owned by this customer and provider account")
		}
		if customer && (!rails.IsNMI(sub.Rail) || method.RebillDriver != models.RebillDriverOpenRails || method.Custodian != models.CustodianPSP) {
			return ErrRebillUnsupported
		}
		minor, err := moneyutil.NativeToRailMinorExact(terms.Currency, terms.Amount)
		if err != nil {
			return err
		}
		p := ManualRebillPayload{Initiator: initiator, RequestedPaymentMethodID: requestedMethod, Renewal: terms, PaymentMethodID: method.ID, Instrument: charge.FreezeInstrument(methodRow), Rail: string(sub.Rail), RailSubscriptionID: sub.RailSubscriptionID, OrderReference: rebillOrderReference(key), Attempt: ordinal, FailureCount: failures, AmountMinor: minor}
		windowEnd := terms.PeriodStart.Add(collection.Window(int(terms.PeriodEnd.Sub(terms.PeriodStart) / time.Hour)))
		if !windowEnd.After(now) {
			return ErrRebillNotRetryable
		}
		accepted, err = store.Enqueue(ctx, EnqueueParams{MerchantID: mid.UUID(), Provider: p.Rail, IntentType: TypeManualRebill, SubscriptionID: &sub.ID, PriceID: &terms.PriceID, PspID: sub.PspID, Payload: p, IdempotencyKey: key, NextAttemptAt: now, Origin: origin, OriginReason: reason, Actor: actor, ExpiresAt: &windowEnd})
		if err != nil {
			return err
		}
		canonical, err := DecodeManualRebillPayload(accepted)
		if err != nil {
			return err
		}
		if canonical.Renewal.SubscriptionID != sub.ID || canonical.Renewal.CustomerID != sub.CustomerID || canonical.Initiator != initiator || (customer && !sameOptionalID(canonical.RequestedPaymentMethodID, requestedMethod)) || (!customer && canonical.Attempt != ordinal) || !canonical.Renewal.PeriodStart.Equal(*sub.CurrentPeriodEndsAt) {
			return ErrRebillKeyConflict
		}
		return nil
	})
	return accepted, replayed, err
}

func sameOptionalID(a, b *uuid.UUID) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}
