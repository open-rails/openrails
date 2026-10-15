package intents

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

const TypeNMIRecurringConfirmation = "nmi_recurring_confirmation"

// confirmationTerms identifies one missing-agreement episode. Request keys do
// not create new attempts for the same card and unpaid period.
type confirmationTerms struct {
	SubscriptionID  uuid.UUID               `json:"subscription_id"`
	CustomerID      uuid.UUID               `json:"customer_id"`
	PaymentMethodID uuid.UUID               `json:"payment_method_id"`
	VersionID       uuid.UUID               `json:"version_id"`
	MandateID       uuid.UUID               `json:"mandate_id"`
	Instrument      charge.FrozenInstrument `json:"instrument"`
	Rail            string                  `json:"rail"`
	Schedule        string                  `json:"schedule"`
	PeriodEnd       time.Time               `json:"period_end"`
}
type recurringConfirmation struct {
	confirmationTerms
	OrderReference string    `json:"order_reference"`
	AcceptedAt     time.Time `json:"accepted_at"`
}

// EnqueueCustomer admits only an explicit confirmation of the effective current
// card. A nil result leaves ordinary method selection with its existing owner.
func (h *NMIRecurringConfirmationHandler) EnqueueCustomer(ctx context.Context, subID, payer uuid.UUID, requested *uuid.UUID) (*gen.BillingProviderIntent, error) {
	var accepted *gen.BillingProviderIntent
	err := h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.DB.NewWithPgxTx(tx)
		sub, err := subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, subID)
		if err != nil {
			return err
		}
		if payer == uuid.Nil || sub.CustomerID != payer {
			return pgx.ErrNoRows
		}
		p, eligible, err := confirmationSnapshot(ctx, d, sub)
		if err != nil || !eligible {
			return err
		}
		// null is a change to following the default, not explicit same-card consent.
		if requested == nil || *requested != p.PaymentMethodID {
			return nil
		}
		raw, err := json.Marshal(p)
		if err != nil {
			return err
		}
		episode := uuid.NewSHA1(sub.MerchantID, raw).String()
		key := TypeNMIRecurringConfirmation + ":" + episode
		store := NewStore(d)
		prior, err := store.GetByIdempotencyKey(ctx, key)
		if err == nil {
			accepted = &prior
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := subscriptions.RefuseOwnedRebillTerms(ctx, d, sub); err != nil {
			return err
		}
		now := h.Clock.Now().UTC()
		row, err := store.Enqueue(ctx, EnqueueParams{
			MerchantID: sub.MerchantID, Provider: string(sub.Rail), IntentType: TypeNMIRecurringConfirmation,
			SubscriptionID: &sub.ID, PspID: sub.PspID,
			Payload: recurringConfirmation{p, "pmr-" + episode, now}, IdempotencyKey: key,
			NextAttemptAt: now, Origin: OriginUser, OriginReason: "customer confirmed current card for recurring recovery", Actor: payer.String(), Inline: true,
		})
		if err == nil {
			accepted = &row
		}
		return err
	})
	return accepted, err
}

// confirmationSnapshot excludes revoked/reconsent agreements and unrelated
// holds. It is read while holding the subscription and card locks.
func confirmationSnapshot(ctx context.Context, d *db.DB, sub *models.Subscription) (confirmationTerms, bool, error) {
	var p confirmationTerms
	if sub.CollectionPolicy != models.CollectionPolicyNMISchedule || !rails.IsNMI(sub.Rail) || sub.Status != models.StatusAwaitingMethod || sub.CanceledAt != nil || sub.CurrentPeriodEndsAt == nil || sub.RailSubscriptionID == "" {
		return p, false, nil
	}
	methodID, err := subscriptions.PaymentMethodOf(ctx, d.Gen(ctx), sub)
	if err != nil || methodID == nil {
		return p, false, err
	}
	method, err := d.Gen(ctx).GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: sub.MerchantID, ID: *methodID})
	if err != nil {
		return p, false, err
	}
	if method.CustomerID != sub.CustomerID || method.Custodian != models.CustodianPSP || !paymentmethods.Chargeable(method) || !charge.ChargeableOn(method, sub.PspID) || method.Rail != string(sub.Rail) {
		return p, false, nil
	}
	instrument := charge.FreezeInstrument(method, sub.PspID)
	if instrument.RailCustomerRef == "" {
		return p, false, nil
	}
	agreement, err := d.Gen(ctx).GetLiveRecurringMandateForShare(ctx, gen.GetLiveRecurringMandateForShareParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		existed, err := d.Gen(ctx).HasSubscriptionMandate(ctx, gen.HasSubscriptionMandateParams{MerchantID: sub.MerchantID, CustomerID: sub.CustomerID, SubscriptionID: sub.ID})
		if err != nil || existed {
			return p, false, err
		}
	case err != nil:
		return p, false, err
	default:
		if agreement.Status != mandates.StatusActive || agreement.CustomerID != sub.CustomerID || agreement.PaymentMethodID == nil || *agreement.PaymentMethodID != method.ID || agreement.PspID != sub.PspID || agreement.InitialTransactionID != nil {
			return p, false, nil
		}
		p.MandateID = agreement.ID
	}
	version, err := d.Gen(ctx).LatestPaymentMethodVersionID(ctx, gen.LatestPaymentMethodVersionIDParams{MerchantID: sub.MerchantID, PaymentMethodID: method.ID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return p, false, err
	}
	p.SubscriptionID, p.CustomerID, p.PaymentMethodID, p.VersionID = sub.ID, sub.CustomerID, method.ID, version
	p.Instrument, p.Rail, p.Schedule, p.PeriodEnd = instrument, string(sub.Rail), sub.RailSubscriptionID, sub.CurrentPeriodEndsAt.UTC()
	return p, true, nil
}
