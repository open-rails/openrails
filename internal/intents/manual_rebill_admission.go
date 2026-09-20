package intents

import (
	"context"
	"errors"
	"fmt"
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
	var accepted gen.OpenrailsRailIntent
	now := h.now()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return accepted, err
	}
	err = h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.DB.NewWithPgxTx(tx)
		sub, err := subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, subscriptionID)
		if err != nil {
			return err
		}
		// An unresolved attempt retains ownership even if a webhook or an
		// operator has changed the current lifecycle meanwhile.
		active, err := d.Gen(ctx).GetUnresolvedManualRebill(ctx, gen.GetUnresolvedManualRebillParams{MerchantID: mid.UUID(), SubscriptionID: subscriptionID})
		if err == nil {
			accepted = active
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if sub.Status != models.StatusPastDue || sub.CurrentPeriodEndsAt == nil || (sub.NextRetryAt != nil && sub.NextRetryAt.After(now)) {
			return errors.New("subscription has no due rebill attempt")
		}
		ordinal := 0
		previous, err := d.Gen(ctx).GetLatestManualRebillForPeriod(ctx, gen.GetLatestManualRebillForPeriodParams{MerchantID: mid.UUID(), SubscriptionID: subscriptionID, PeriodStart: sub.CurrentPeriodEndsAt.UTC()})
		if err == nil {
			prior, err := subscriptions.DecodeManualRebillPayload(previous)
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
		key := subscriptions.ManualRebillIdempotencyKey(sub.ID, *sub.CurrentPeriodEndsAt, string(sub.Rail), ordinal)
		store := NewStore(d)
		terms, err := subscriptions.PrepareRenewalTerms(ctx, d, sub, now)
		if err != nil {
			return err
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
		minor, err := moneyutil.NativeToRailMinorExact(terms.Currency, terms.Amount)
		if err != nil {
			return err
		}
		p := subscriptions.ManualRebillPayload{Renewal: terms, PaymentMethodID: method.ID, Instrument: charge.FreezeInstrument(methodRow), Rail: string(sub.Rail), RailSubscriptionID: sub.RailSubscriptionID, OrderReference: subscriptions.RebillOrderReference(key), Attempt: ordinal, FailureCount: failures, AmountMinor: minor}
		windowEnd := terms.PeriodStart.Add(collection.Window(int(terms.PeriodEnd.Sub(terms.PeriodStart) / time.Hour)))
		if !windowEnd.After(now) {
			return errors.New("rebill is outside its collection window")
		}
		accepted, err = store.Enqueue(ctx, EnqueueParams{MerchantID: mid.UUID(), Provider: p.Rail, IntentType: subscriptions.TypeManualRebill, SubscriptionID: &sub.ID, PriceID: &terms.PriceID, PspID: sub.PspID, Payload: p, IdempotencyKey: key, NextAttemptAt: now, Origin: OriginSystem, OriginReason: "scheduled recurring recovery", ExpiresAt: &windowEnd})
		if err != nil {
			return err
		}
		canonical, err := subscriptions.DecodeManualRebillPayload(accepted)
		if err != nil {
			return err
		}
		if canonical.Renewal.SubscriptionID != sub.ID || canonical.Renewal.CustomerID != sub.CustomerID || canonical.Attempt != ordinal || !canonical.Renewal.PeriodStart.Equal(*sub.CurrentPeriodEndsAt) {
			return fmt.Errorf("rebill key belongs to another accepted request")
		}
		return nil
	})
	return accepted, err
}
