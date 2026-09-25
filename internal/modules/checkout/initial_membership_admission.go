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
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

func ownsInitialMembership(in gen.OpenrailsRailIntent, user string, price uuid.UUID, fingerprint string, sessionID *uuid.UUID) error {
	p, err := subscriptions.DecodeInitialMembershipPayload(in)
	if err != nil {
		return err
	}
	if (p.CheckoutSessionID == nil) != (sessionID == nil) || (sessionID != nil && *p.CheckoutSessionID != *sessionID) {
		return apperr.Conflictf("checkout key belongs to another session binding")
	}
	if p.Terms.CustomerID.String() != user || p.Terms.PriceID != price || p.RequestFingerprint != fingerprint {
		return apperr.Conflictf("checkout key belongs to another accepted enrollment")
	}
	return nil
}

func initialMembershipReplayParams(in gen.OpenrailsRailIntent) intents.EnqueueParams {
	return intents.EnqueueParams{MerchantID: in.MerchantID, Provider: in.Rail, PspID: *in.PspID, IntentType: in.IntentType, PriceID: in.PriceID, Payload: json.RawMessage(in.Payload), IdempotencyKey: in.IdempotencyKey, NextAttemptAt: in.NextAttemptAt, Origin: intents.Origin(in.Origin)}
}

func (s *CheckoutService) replayInitialMembership(ctx context.Context, req *CheckoutRequest, user *UserIdentity) (*CheckoutResponse, bool, error) {
	if req == nil || user == nil || req.IdempotencyKey == "" || s.SubscriptionService == nil {
		return nil, false, nil
	}
	prior, err := intents.NewStore(s.SubscriptionService.Database()).GetByIdempotencyKey(ctx, InitialMembershipIdempotencyKey(req.IdempotencyKey))
	if db.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	p, err := subscriptions.DecodeInitialMembershipPayload(prior)
	if err != nil {
		return nil, true, err
	}
	target := railTarget{PSP: p.PSP, Rail: "nmi"}
	fingerprint := saleRequestFingerprint(req, user, p.Terms.PriceID, target)
	if err := ownsInitialMembership(prior, user.ID, p.Terms.PriceID, fingerprint, nil); err != nil {
		return nil, true, err
	}
	if s.Intents == nil {
		return nil, true, errors.New("enrollment executor unavailable")
	}
	current, err := s.Intents.EnqueueOwnedAndExecute(ctx, initialMembershipReplayParams(prior), func(in gen.OpenrailsRailIntent) error {
		return ownsInitialMembership(in, user.ID, p.Terms.PriceID, fingerprint, nil)
	})
	if err != nil {
		return nil, true, err
	}
	response, err := initialMembershipResponseFromIntent(current)
	return response, true, err
}

func (s *CheckoutService) admitInitialMembership(ctx context.Context, req *CheckoutRequest, user *UserIdentity, priceID uuid.UUID, method *models.PaymentMethod, target railTarget, key string) (gen.OpenrailsRailIntent, error) {
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
		prior, err := intents.NewStore(d).GetByIdempotencyKey(ctx, InitialMembershipIdempotencyKey(key))
		if err == nil {
			in = prior
			return ownsInitialMembership(prior, user.ID, priceID, fingerprint, nil)
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
		terms := subscriptions.InitialMembershipTerms{CollectionPolicy: models.CollectionPolicyNMISchedule, SubscriptionID: uuidutil.NewV7(), PaymentID: paymentID, CustomerID: customer, PSPID: saved.PspID, ProductID: product.ID, PriceID: price.ID, PaymentMethodID: saved.ID, ProductName: product.DisplayName, Amount: amount, RecurringAmount: price.Amount, Currency: price.Currency, AcceptedAt: now, PeriodStart: start, PeriodEnd: end, Pending: delayed != nil, Entitlements: benefits}
		email := req.Email
		if s.Config != nil && s.Config.IsTestMode() {
			email = ""
		}
		payload := InitialMembershipPayload{Terms: terms, Instrument: charge.FreezeInstrument(saved), RequestFingerprint: fingerprint, CheckoutIdempotencyKey: key, PSP: target.PSP, Email: email, E2ERunID: strings.TrimSpace(req.Metadata["e2e_run_id"]), NativeSchedule: &subscriptions.NMIInitialScheduleTerms{PlanID: plan, StartDate: startDate, DayFrequency: *days, PlanPayments: 0, Card: nmi.CardUserData{FirstName: ResolveCheckoutFirstName(req, user), LastName: ResolveCheckoutLastName(req), Address1: DefaultIfEmpty(req.Address1, "N/A"), City: DefaultIfEmpty(req.City, "N/A"), State: DefaultIfEmpty(req.State, "N/A"), Zip: DefaultIfEmpty(req.Zip, "00000"), Country: DefaultIfEmpty(req.Country, "US")}}}
		in, err = intents.NewStore(d).Enqueue(ctx, intents.EnqueueParams{MerchantID: mid.UUID(), Provider: "nmi", PspID: saved.PspID, IntentType: TypeInitialMembership, PriceID: &price.ID, Payload: payload, IdempotencyKey: InitialMembershipIdempotencyKey(key), NextAttemptAt: now, Origin: intents.OriginUser, OriginReason: "checkout initial enrollment"})
		if err != nil {
			return err
		}
		return ownsInitialMembership(in, user.ID, priceID, fingerprint, nil)
	})
	return in, err
}
