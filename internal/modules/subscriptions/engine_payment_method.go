package subscriptions

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/pkg/merchant"
)

// UpdateEnginePaymentMethod selects an already qualified recurring instrument.
// It never changes schedule ownership or issues a provider schedule mutation.
// Accepted uncertain payments keep their frozen instrument until resolved.
func (s *SubscriptionLifecycleService) UpdateEnginePaymentMethod(ctx context.Context, subscriptionID, customer, methodID uuid.UUID) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.DB.NewWithPgxTx(tx)
		q := d.Gen(ctx)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: customer}); err != nil {
			return err
		}
		sub, err := NewSubscriptionRepo(d).GetByIDForUpdate(ctx, subscriptionID)
		if err != nil {
			return err
		}
		if sub.CustomerID != customer {
			return pgx.ErrNoRows
		}
		if sub.CollectionPolicy != models.CollectionPolicyEngine || (sub.Rail != models.RailNMI && sub.Rail != models.RailStripe) || (sub.Status != models.StatusActive && sub.Status != models.StatusPastDue && sub.Status != models.StatusAwaitingMethod) {
			return apperr.Conflictf("subscription cannot select an engine payment method")
		}
		if err := RefuseOwnedRebillTerms(ctx, d, sub); err != nil {
			return err
		}
		observed, err := q.GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: mid.UUID(), ID: methodID})
		if err != nil {
			return err
		}
		if observed.CustodianID != nil {
			handle := paymentmethods.CustodianHandle{Custodian: *observed.CustodianID, Method: observed.RailMethodRef}
			if err := paymentmethods.LockCustodianHandles(ctx, q, mid.UUID(), handle); err != nil {
				return err
			}
			if err := paymentmethods.RequireCustodianHandleAvailable(ctx, q, mid.UUID(), handle); err != nil {
				return err
			}
		}
		method, err := q.GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: mid.UUID(), ID: methodID})
		if err != nil {
			return err
		}
		if method.CustomerID != customer {
			// Another customer's method is indistinguishable from a missing one.
			return apperr.New(http.StatusNotFound, "not_found", "payment method not found")
		}
		if method.PspID != sub.PspID || method.Rail != string(sub.Rail) || method.ParkReason != "" || method.ChargeVia != "pan_proxy" {
			return charge.ErrInstrumentChanged
		}
		if err := charge.FreezeInstrument(observed).Matches(method, charge.AgreementRecurring); err != nil {
			return err
		}
		psp, err := q.GetPSP(ctx, gen.GetPSPParams{MerchantID: mid.UUID(), ID: method.PspID})
		if err != nil {
			return err
		}
		if psp.Archived || psp.Rail != method.Rail {
			return apperr.Conflictf("payment account is archived")
		}
		var binding *charge.HyperSwitchBinding
		if method.Custodian == models.CustodianHyperSwitch {
			if s.Config == nil || s.Config.HyperSwitch == nil {
				return errors.New("HyperSwitch deployment is unavailable")
			}
			frozen, err := charge.FreezeHyperSwitchBinding(ctx, q, method, s.Config.HyperSwitch.APIBaseURL)
			if err != nil {
				return err
			}
			binding = &frozen
			custodian, err := q.GetCustodian(ctx, gen.GetCustodianParams{MerchantID: mid.UUID(), ID: *method.CustodianID})
			if err != nil {
				return err
			}
			if custodian.Archived {
				return apperr.Conflictf("payment custodian is archived")
			}
		}
		if err := charge.ValidateEngineInstrument(method.Rail, charge.FreezeInstrument(method), binding, true); err != nil {
			return apperr.Conflictf("replacement card requires a qualified recurring agreement")
		}
		if method.Rail == "stripe" && !stripeEngineID(method.StoredCredentialRecurringRef, "pi_") && !stripeEngineID(method.StoredCredentialRecurringRef, "seti_") {
			return apperr.Conflictf("replacement Stripe card requires a qualified recurring agreement")
		}
		now := s.now()
		sub.PaymentMethodID = &methodID
		// A delinquent membership retries on the new card at the next due
		// pass instead of waiting out the old card's schedule; one waiting
		// for a card resumes dunning.
		if sub.Status == models.StatusPastDue || sub.Status == models.StatusAwaitingMethod {
			sub.Status = models.StatusPastDue
			sub.NextRetryAt = &now
		}
		return NewSubscriptionRepo(d).UpdateAt(ctx, sub, now)
	})
}
