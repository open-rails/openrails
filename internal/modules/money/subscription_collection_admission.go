package money

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// AdmitDueSubscriptionCollection is an unexposed prototype. No worker or public
// enrollment calls it. Receipt-qualified retries and completion are deliberately
// held until the shared accepted-membership writer and recurring CIT are ready.
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
		if sub.CustomerID != observed.CustomerID || sub.CollectionPolicy != models.CollectionPolicyEngine || sub.Rail != models.RailNMI || sub.RailSubscriptionID != "" {
			return errors.New("subscription is not an engine-owned NMI obligation")
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
		if sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.After(admittedAt) || (sub.Status != models.StatusActive && sub.Status != models.StatusPastDue) || (sub.Status == models.StatusPastDue && (sub.NextRetryAt == nil || sub.NextRetryAt.After(admittedAt))) {
			return errors.New("engine subscription is not due")
		}
		// The prototype cannot infer definite nonexecution from a generic terminal
		// status. Typed recurring decline completion will authorize later attempts.
		_, err = q.GetLatestSubscriptionCollectionForPeriod(ctx, gen.GetLatestSubscriptionCollectionForPeriodParams{MerchantID: mid.UUID(), SubscriptionID: sub.ID, PreviousPeriodEnd: sub.CurrentPeriodEndsAt.UTC()})
		if err == nil {
			return errors.New("engine retry requires qualified terminal completion")
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if sub.PaymentMethodID == nil {
			return errors.New("engine subscription has no saved method")
		}
		method, err := q.GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: mid.UUID(), ID: *sub.PaymentMethodID})
		if err != nil {
			return err
		}
		if method.CustomerID != sub.CustomerID || method.PspID != sub.PspID || method.Rail != "nmi" || method.Custodian != models.CustodianHyperSwitch || method.ParkReason != "" || method.StoredCredentialRecurringRef == "" {
			return errors.New("engine recurring method is not qualified for this obligation")
		}
		binding, err := collectionHyperSwitchBinding(ctx, q, method, s.hyperSwitchDeployment)
		if err != nil {
			return err
		}
		accounts, err := q.GetCollectionCustodianAccountsForShare(ctx, gen.GetCollectionCustodianAccountsForShareParams{MerchantID: mid.UUID(), PspID: method.PspID, CustodianID: *method.CustodianID})
		if err != nil {
			return err
		}
		if accounts.OpenrailsPsp.Archived || accounts.OpenrailsCustodian.Archived {
			return errors.New("archived accounts cannot admit a new engine renewal")
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
		key := subscriptions.SubscriptionCollectionKey(sub.ID, *sub.CurrentPeriodEndsAt, 0)
		payload := subscriptions.SubscriptionCollectionPayload{Renewal: terms, PreviousPeriodEnd: sub.CurrentPeriodEndsAt.UTC(), AcceptedAt: admittedAt, PaymentMethodID: method.ID, Instrument: charge.FreezeInstrument(method), HyperSwitch: binding, AmountMinor: minor, OrderReference: subscriptions.RebillOrderReference(key)}
		accepted, err = intents.NewStore(d).Enqueue(ctx, intents.EnqueueParams{MerchantID: mid.UUID(), Provider: "nmi", IntentType: subscriptions.TypeSubscriptionCollection, SubscriptionID: &sub.ID, PriceID: &terms.PriceID, PspID: method.PspID, CustodianID: *method.CustodianID, Payload: payload, IdempotencyKey: key, NextAttemptAt: admittedAt, Origin: intents.OriginSystem, OriginReason: "accepted engine renewal"})
		if err != nil {
			return err
		}
		_, err = subscriptions.DecodeSubscriptionCollectionPayload(accepted)
		return err
	})
	return accepted, err
}
