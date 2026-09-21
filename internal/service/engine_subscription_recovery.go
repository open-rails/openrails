package service

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// This is a view of the existing engine operation, never authority to submit.
// Admission repeats payer, lifecycle, released-period and instrument checks.
func (s *Service) engineSubscriptionRecovery(ctx context.Context, sub *models.Subscription) (*openrails.PaymentRecovery, error) {
	out := &openrails.PaymentRecovery{}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	q := s.rt.DB.Gen(ctx)
	current, err := q.GetUnresolvedSubscriptionCollection(ctx, gen.GetUnresolvedSubscriptionCollectionParams{MerchantID: mid.UUID(), SubscriptionID: sub.ID})
	if err == nil {
		p, err := subscriptions.DecodeSubscriptionCollectionPayload(current)
		if err != nil {
			return nil, err
		}
		if p.Renewal.CustomerID != sub.CustomerID || p.Renewal.SubscriptionID != sub.ID {
			return nil, errors.New("engine recovery operation scope mismatch")
		}
		out.Operation = &openrails.PaymentOperation{ID: current.ID, Status: current.Status}
		out.BlockedReason = "payment_in_progress"
		if intents.EvidenceString(current, "stripe_payment_intent_id") != "" {
			out.BlockedReason = "authentication_required"
		}
		return out, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if sub.Status != models.StatusPastDue || sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.After(s.now().UTC()) {
		out.BlockedReason = "subscription_not_retryable"
		return out, nil
	}
	if s.rt.Config == nil || s.rt.Config.IsProviderReadOnly() {
		out.BlockedReason = "provider_writes_disabled"
		return out, nil
	}
	if s.rt.Config.EngineAdmissionHold {
		out.BlockedReason = "engine_payment_admission_held"
		return out, nil
	}
	latest, err := q.GetLatestSubscriptionCollectionForPeriod(ctx, gen.GetLatestSubscriptionCollectionForPeriodParams{MerchantID: mid.UUID(), SubscriptionID: sub.ID, PreviousPeriodEnd: *sub.CurrentPeriodEndsAt})
	if errors.Is(err, pgx.ErrNoRows) {
		out.BlockedReason = "subscription_not_retryable"
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if latest.Status != intents.StatusFailedTerminal {
		out.BlockedReason = "payment_in_progress"
		out.Operation = &openrails.PaymentOperation{ID: latest.ID, Status: latest.Status}
		return out, nil
	}
	if err := intents.ValidateSubscriptionCollectionTerminal(latest); err != nil {
		return nil, err
	}
	accepted, err := subscriptions.DecodeSubscriptionCollectionPayload(latest)
	if err != nil {
		return nil, err
	}
	if accepted.Renewal.CustomerID != sub.CustomerID || !accepted.PreviousPeriodEnd.Equal(*sub.CurrentPeriodEndsAt) {
		return nil, errors.New("engine refusal does not match current period")
	}
	var refusal *CustomerPaymentRefusal
	err = customerPaymentRefusal(latest)
	if errors.As(err, &refusal) {
		if accepted.Instrument.PSPID == sub.PspID && latest.Rail == string(sub.Rail) {
			out.LastFailureReason = payments.NormalizeFailureReason(latest.Rail, refusal.Code)
		}
	} else if err != nil && !errors.Is(err, intents.ErrRebillNotRetryable) {
		return nil, err
	}
	if sub.PaymentMethodID == nil || (sub.Rail != models.RailStripe && sub.Rail != models.RailNMI) {
		out.BlockedReason = "customer_payment_unsupported"
		return out, nil
	}
	method, err := q.GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: mid.UUID(), ID: *sub.PaymentMethodID})
	if err != nil {
		return nil, err
	}
	if method.CustomerID != sub.CustomerID || method.PspID != sub.PspID || method.Rail != string(sub.Rail) || method.ParkReason != "" || method.StoredCredentialRecurringRef == "" || method.RailCustomerRef == "" || method.RailMethodRef == "" || (method.Custodian != models.CustodianPSP && !(method.Custodian == models.CustodianHyperSwitch && sub.Rail == models.RailNMI)) {
		out.BlockedReason = "customer_payment_unsupported"
		return out, nil
	}
	out.Retryable = true
	return out, nil
}
