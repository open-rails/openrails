package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

func ownsInitialEnrollment(in gen.OpenrailsRailIntent, user string, price uuid.UUID, fingerprint string) error {
	p, err := subscriptions.DecodeNMIInitialEnrollmentPayload(in)
	if err != nil {
		return err
	}
	if p.UserID != user || p.PriceID != price || p.RequestFingerprint != fingerprint {
		return apperr.Conflictf("checkout key belongs to another accepted enrollment")
	}
	return nil
}

func initialEnrollmentReplayParams(in gen.OpenrailsRailIntent) intents.EnqueueParams {
	return intents.EnqueueParams{MerchantID: in.MerchantID, Provider: in.Rail, PspID: *in.PspID, IntentType: in.IntentType, PriceID: in.PriceID, Payload: json.RawMessage(in.Payload), IdempotencyKey: in.IdempotencyKey, NextAttemptAt: in.NextAttemptAt, Origin: intents.Origin(in.Origin)}
}

func (s *CheckoutService) replayInitialEnrollment(ctx context.Context, req *CheckoutRequest, user *UserIdentity) (*CheckoutResponse, bool, error) {
	if req == nil || user == nil || req.IdempotencyKey == "" || s.SubscriptionService == nil {
		return nil, false, nil
	}
	prior, err := intents.NewStore(s.SubscriptionService.Database()).GetByIdempotencyKey(ctx, NMISubscriptionCreateIdempotencyKey(req.IdempotencyKey))
	if db.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	p, err := subscriptions.DecodeNMIInitialEnrollmentPayload(prior)
	if err != nil {
		return nil, true, err
	}
	target := railTarget{PSP: p.PSP, Rail: "nmi"}
	fingerprint := saleRequestFingerprint(req, user, p.PriceID, target)
	if err := ownsInitialEnrollment(prior, user.ID, p.PriceID, fingerprint); err != nil {
		return nil, true, err
	}
	if s.Intents == nil {
		return nil, true, errors.New("enrollment executor unavailable")
	}
	current, err := s.Intents.EnqueueOwnedAndExecute(ctx, initialEnrollmentReplayParams(prior), func(in gen.OpenrailsRailIntent) error {
		return ownsInitialEnrollment(in, user.ID, p.PriceID, fingerprint)
	})
	if err != nil {
		return nil, true, err
	}
	response, err := nmiSubscriptionResponseFromIntent(current)
	return response, true, err
}

func (s *CheckoutService) admitInitialEnrollment(ctx context.Context, req *CheckoutRequest, user *UserIdentity, priceID uuid.UUID, method *models.PaymentMethod, target railTarget, key string) (gen.OpenrailsRailIntent, error) {
	var in gen.OpenrailsRailIntent
	if method == nil || s.PurchaseService == nil || target.Scope == nil {
		return in, errors.New("initial enrollment requires a saved instrument and provider account")
	}
	client, err := s.resolveNMIClient(ctx, target.PSP)
	if err != nil {
		return in, err
	}
	owner, account := client.AccountIdentity()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return in, err
	}
	if owner != mid.UUID() || account != target.Scope.ID {
		return in, errors.New("enrollment client differs from accepted provider account")
	}
	// Provider reads never run under the customer/method transaction. This is an
	// observation of one billing entry, not a lock on remote vault mutations.
	if _, err = client.ReadSingleCardVaultBilling(ctx, method.RailCustomerRef, method.RailMethodRef); err != nil {
		return in, fmt.Errorf("enrollment instrument cannot be qualified: %w", err)
	}
	database := s.SubscriptionService.Database()
	fingerprint := saleRequestFingerprint(req, user, priceID, target)
	err = database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := database.NewWithPgxTx(tx)
		customer, err := customerIDFromUser(user.ID)
		if err != nil {
			return err
		}
		if _, err = d.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: customer}); err != nil {
			return err
		}
		prior, err := intents.NewStore(d).GetByIdempotencyKey(ctx, NMISubscriptionCreateIdempotencyKey(key))
		if err == nil {
			in = prior
			return ownsInitialEnrollment(prior, user.ID, priceID, fingerprint)
		}
		if !db.IsNotFound(err) {
			return err
		}
		purchase := s.PurchaseService.transactionBound(d)
		price, err := purchase.PriceService.GetByID(ctx, priceID)
		if err != nil {
			return err
		}
		product, err := purchase.ProductService.GetByID(ctx, price.ProductID)
		if err != nil {
			return err
		}
		days := price.RecurringCycleDays()
		if days == nil || *days <= 0 || price.TrialUnitAmount != nil || price.TrialDurationHours != nil {
			return errors.New("native enrollment requires declared whole-day recurring terms without a trial")
		}
		plan, err := requireNMIPlanForTarget(price, target)
		if err != nil {
			return err
		}
		saved, err := d.Gen(ctx).GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: mid.UUID(), ID: method.ID})
		if err != nil {
			return err
		}
		if saved.CustomerID != customer || saved.PspID != target.Scope.ID || saved.Custodian != models.CustodianPSP || saved.ParkReason != "" || saved.RailCustomerRef != method.RailCustomerRef || saved.RailMethodRef != method.RailMethodRef {
			return errors.New("enrollment instrument changed during admission")
		}
		coverage, err := purchase.GetUserProductCoverage(ctx, user.ID, product)
		if err != nil {
			return err
		}
		if _, err := purchase.SubscriptionService.GetActiveOrPendingByUserIDAndProductID(ctx, user.ID, product.ID); err == nil {
			return apperr.Conflictf("customer already has a subscription for this product")
		} else if !db.IsNotFound(err) {
			return err
		}
		now := s.now().UTC().Truncate(time.Microsecond)
		startDate, delayed := nmiSubscriptionStartDate(coverage, now)
		start := now
		amount := price.Amount
		if delayed != nil {
			start = delayed.UTC().Truncate(time.Microsecond)
			amount = 0
		}
		end := start.Add(time.Duration(*days) * 24 * time.Hour)
		if delayed == nil {
			startDate = end.Format("20060102")
		}
		benefits := models.CloneEntitlementsSpec(product.EntitlementsSpec)
		if benefits == nil {
			benefits = map[string]*int{}
		}
		paymentID := uuid.Nil
		if amount > 0 {
			paymentID = uuidutil.NewV7()
		}
		terms := subscriptions.InitialMembershipTerms{CollectionPolicy: models.CollectionPolicyProvider, SubscriptionID: uuidutil.NewV7(), PaymentID: paymentID, CustomerID: customer, PSPID: saved.PspID, ProductID: product.ID, PriceID: price.ID, PaymentMethodID: saved.ID, ProductName: product.DisplayName, Amount: amount, RecurringAmount: price.Amount, Currency: price.Currency, AcceptedAt: now, PeriodStart: start, PeriodEnd: end, Pending: delayed != nil, Entitlements: benefits}
		email := req.Email
		if s.Config != nil && s.Config.IsTestMode() {
			email = ""
		}
		payload := NMISubscriptionCreatePayload{Terms: terms, Instrument: charge.FreezeInstrument(saved), RequestFingerprint: fingerprint, DayFrequency: *days, PlanPayments: 0, Provider: "nmi", PSP: target.PSP, PlanID: plan, CustomerVaultID: saved.RailCustomerRef, BillingID: saved.RailMethodRef, AmountMicros: amount, Currency: price.Currency, Email: email, UserID: user.ID, PriceID: price.ID, LocalSubscriptionID: terms.SubscriptionID, PaymentMethodID: &terms.PaymentMethodID, StartDate: startDate, DelayedStart: delayed, StoredCredentialRef: strings.TrimSpace(saved.StoredCredentialRecurringRef), E2ERunID: strings.TrimSpace(req.Metadata["e2e_run_id"]), CheckoutIdempotencyKey: key, FirstName: ResolveCheckoutFirstName(req, user), LastName: ResolveCheckoutLastName(req), Address1: DefaultIfEmpty(req.Address1, "N/A"), City: DefaultIfEmpty(req.City, "N/A"), State: DefaultIfEmpty(req.State, "N/A"), Zip: DefaultIfEmpty(req.Zip, "00000"), Country: DefaultIfEmpty(req.Country, "US")}
		in, err = intents.NewStore(d).Enqueue(ctx, intents.EnqueueParams{MerchantID: mid.UUID(), Provider: "nmi", PspID: saved.PspID, IntentType: TypeNMISubscriptionCreate, PriceID: &price.ID, Payload: payload, IdempotencyKey: NMISubscriptionCreateIdempotencyKey(key), NextAttemptAt: now, Origin: intents.OriginUser, OriginReason: "checkout initial enrollment"})
		if err != nil {
			return err
		}
		return ownsInitialEnrollment(in, user.ID, priceID, fingerprint)
	})
	return in, err
}
