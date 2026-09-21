package money

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// AdmitDueSubscriptionCollection freezes one due engine obligation. Existing
// accepted work is recovered before a hold can refuse a new admission.
func (s *MoneyService) AdmitDueSubscriptionCollection(ctx context.Context, subscriptionID uuid.UUID, admittedAt time.Time) (gen.OpenrailsRailIntent, error) {
	var accepted gen.OpenrailsRailIntent
	mid, err := merchant.Require(ctx)
	if err != nil {
		return accepted, err
	}
	if admittedAt.IsZero() {
		return accepted, errors.New("engine admission time is required")
	}
	admittedAt = admittedAt.UTC().Truncate(time.Microsecond)
	repo := subscriptions.NewSubscriptionRepo(s.db)
	observed, err := repo.GetByID(ctx, subscriptionID)
	if err != nil {
		return accepted, err
	}
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		q := d.Gen(ctx)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: observed.CustomerID}); err != nil {
			return err
		}
		sub, err := subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, subscriptionID)
		if err != nil {
			return err
		}
		if sub.CustomerID != observed.CustomerID || sub.CollectionPolicy != models.CollectionPolicyEngine || (sub.Rail != models.RailNMI && sub.Rail != models.RailStripe) || sub.RailSubscriptionID != "" {
			return errors.New("subscription is not an engine-owned card obligation")
		}
		current, err := q.GetUnresolvedSubscriptionCollection(ctx, gen.GetUnresolvedSubscriptionCollectionParams{MerchantID: mid.UUID(), SubscriptionID: sub.ID})
		if err == nil {
			if _, err := subscriptions.DecodeSubscriptionCollectionPayload(current); err != nil {
				return err
			}
			accepted = current
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if s.EngineAdmissionHold {
			return errors.New("new engine payment admission is held")
		}
		if sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.After(admittedAt) || (sub.Status != models.StatusActive && sub.Status != models.StatusPastDue) || (sub.Status == models.StatusPastDue && (sub.NextRetryAt == nil || sub.NextRetryAt.After(admittedAt))) {
			return errors.New("engine subscription is not due")
		}
		attempt := 0
		previous, err := q.GetLatestSubscriptionCollectionForPeriod(ctx, gen.GetLatestSubscriptionCollectionForPeriodParams{MerchantID: mid.UUID(), SubscriptionID: sub.ID, PreviousPeriodEnd: sub.CurrentPeriodEndsAt.UTC()})
		if err == nil {
			if previous.Status != intents.StatusFailedTerminal {
				return errors.New("previous engine obligation has not been released")
			}
			if err := intents.ValidateSubscriptionCollectionTerminal(previous); err != nil {
				return err
			}
			old, err := subscriptions.DecodeSubscriptionCollectionPayload(previous)
			if err != nil {
				return err
			}
			if old.Attempt >= math.MaxInt32 {
				return errors.New("engine attempt ordinal exceeds ledger integer range")
			}
			attempt = old.Attempt + 1
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if sub.PaymentMethodID == nil {
			return errors.New("engine subscription has no saved method")
		}
		observedMethod, err := q.GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: mid.UUID(), ID: *sub.PaymentMethodID})
		if err != nil {
			return err
		}
		if observedMethod.CustodianID != nil {
			handle := paymentmethods.CustodianHandle{Custodian: *observedMethod.CustodianID, Method: observedMethod.RailMethodRef}
			if err := paymentmethods.LockCustodianHandles(ctx, q, mid.UUID(), handle); err != nil {
				return err
			}
			if err := paymentmethods.RequireCustodianHandleAvailable(ctx, q, mid.UUID(), handle); err != nil {
				return err
			}
		}
		method, err := q.GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: mid.UUID(), ID: *sub.PaymentMethodID})
		if err != nil {
			return err
		}
		if err := charge.FreezeInstrument(observedMethod).Matches(method, charge.AgreementRecurring); err != nil {
			return err
		}
		if method.CustomerID != sub.CustomerID || method.PspID != sub.PspID || method.Rail != string(sub.Rail) || method.ParkReason != "" {
			return errors.New("engine recurring method is not qualified for this obligation")
		}
		account, err := q.GetPSP(ctx, gen.GetPSPParams{MerchantID: mid.UUID(), ID: method.PspID})
		if err != nil {
			return err
		}
		if account.Archived {
			return errors.New("archived account cannot admit a new engine renewal")
		}
		binding, err := engineCollectionBinding(ctx, q, method, s.hyperSwitchDeployment)
		if err != nil {
			return err
		}
		if method.CustodianID != nil {
			custodian, err := q.GetCustodian(ctx, gen.GetCustodianParams{MerchantID: mid.UUID(), ID: *method.CustodianID})
			if err != nil {
				return err
			}
			if custodian.Archived {
				return errors.New("archived custodian cannot admit a new engine renewal")
			}
		}
		if err := charge.ValidateEngineInstrument(method.Rail, charge.FreezeInstrument(method), engineHyperSwitchPointer(method.Custodian, binding), true); err != nil {
			return err
		}
		terms, err := subscriptions.PrepareRenewalTerms(ctx, d, sub, admittedAt)
		if err != nil {
			return err
		}
		terms, err = subscriptions.SelectEngineRenewalPeriod(terms, admittedAt)
		if err != nil {
			return err
		}
		minor, err := moneyutil.NativeToRailMinorExact(terms.Currency, terms.Amount)
		if err != nil {
			return err
		}
		key := subscriptions.SubscriptionCollectionKey(sub.ID, *sub.CurrentPeriodEndsAt, attempt)
		failures := 0
		if sub.RetryAttempts != nil {
			failures = *sub.RetryAttempts
		}
		payload := subscriptions.SubscriptionCollectionPayload{Attempt: attempt, FailureCount: failures, Renewal: terms, PreviousPeriodEnd: sub.CurrentPeriodEndsAt.UTC(), AcceptedAt: admittedAt, PaymentMethodID: method.ID, Instrument: charge.FreezeInstrument(method), HyperSwitch: binding, AmountMinor: minor, OrderReference: subscriptions.RebillOrderReference(key)}
		accepted, err = intents.NewStore(d).Enqueue(ctx, intents.EnqueueParams{MerchantID: mid.UUID(), Provider: method.Rail, IntentType: subscriptions.TypeSubscriptionCollection, SubscriptionID: &sub.ID, PriceID: &terms.PriceID, PspID: method.PspID, CustodianID: engineCustodianID(method.CustodianID), Payload: payload, IdempotencyKey: key, NextAttemptAt: admittedAt, Origin: intents.OriginSystem, OriginReason: "accepted engine renewal"})
		if err != nil {
			return err
		}
		_, err = subscriptions.DecodeSubscriptionCollectionPayload(accepted)
		return err
	})
	return accepted, err
}
