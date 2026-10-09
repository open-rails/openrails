package checkout

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// initialMembershipQuoteFingerprint excludes newly allocated IDs and acceptance
// instants. A replay compares the customer's displayed commercial decision.
func initialMembershipQuoteFingerprint(terms subscriptions.InitialMembershipTerms) string {
	duration := terms.PeriodEnd.Sub(terms.PeriodStart)
	// Older accepted quotes implied access for exactly one billing period.
	// Keep that equivalent decision's original hash while distinguishing both
	// indefinite access and independently sized access windows on new orders.
	var access json.RawMessage
	if terms.AccessDurationHours == nil || duration%time.Hour != 0 || time.Duration(*terms.AccessDurationHours) != duration/time.Hour {
		access, _ = json.Marshal(terms.AccessDurationHours)
	}
	legacy := terms.LegacyEntitlements
	terms.LegacyEntitlements = nil
	terms.SubscriptionID, terms.PaymentID = uuid.Nil, uuid.Nil
	terms.AcceptedAt, terms.PeriodStart, terms.PeriodEnd = time.Time{}, time.Time{}, time.Time{}
	type fingerprintTerms struct {
		subscriptions.InitialMembershipTerms
		Entitlements        any                               `json:"entitlements"`
		Replaces            *subscriptions.ReplacedMembership `json:"replaces,omitempty"`
		AccessDurationHours json.RawMessage                   `json:"access_duration_hours,omitempty"`
	}
	raw, _ := json.Marshal(struct {
		Terms    fingerprintTerms
		Duration time.Duration
	}{fingerprintTerms{InitialMembershipTerms: terms, Entitlements: grants.AcceptedEntitlementValue(terms.Entitlements, legacy), Replaces: terms.Replaces, AccessDurationHours: access}, duration})
	digest := sha256.Sum256(raw)
	return fmt.Sprintf("%x", digest)
}

// ConfirmInitialMembership consumes accepted terms and a verified interactive
// payer. Only the validated self-session adapter supplies sessionID; other
// callers leave their opaque replay key unbound. Provider routing/credentials
// never come from the quoted payload.
func (s *CheckoutService) ConfirmInitialMembership(ctx context.Context, accepted subscriptions.InitialMembershipTerms, key string, principal billingauth.Payer, sessionID *uuid.UUID) (*CheckoutResponse, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if billingauth.ValidatePayer(&principal) != nil || principal.CredentialClass != billingauth.CredentialClassUserSession || principal.Invoker != "" || principal.MerchantID != mid || principal.SubjectID != accepted.CustomerID.String() {
		return nil, apperr.New(403, "customer_session_required", "initial membership requires its interactive customer session")
	}
	if accepted.CollectionPolicy != models.CollectionPolicyEngine || accepted.Amount <= 0 || accepted.Amount != accepted.RecurringAmount || accepted.Pending {
		return nil, apperr.Conflictf("engine initial membership requires the displayed positive fixed-price agreement")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, errors.New("initial membership requires its persisted session key")
	}
	if s.SubscriptionService == nil || s.Intents == nil {
		return nil, errors.New("initial membership services unavailable")
	}
	database := s.SubscriptionService.Database()
	fingerprint := initialMembershipQuoteFingerprint(accepted)
	prior, err := intents.NewStore(database).GetByIdempotencyKey(ctx, InitialMembershipIdempotencyKey(key))
	if err == nil {
		if err := ownsInitialMembership(prior, accepted.CustomerID.String(), accepted.PriceID, fingerprint, sessionID); err != nil {
			return nil, err
		}
		current, err := s.Intents.EnqueueOwnedAndExecute(ctx, initialMembershipReplayParams(prior), func(in gen.BillingProviderIntent) error {
			return ownsInitialMembership(in, accepted.CustomerID.String(), accepted.PriceID, fingerprint, sessionID)
		})
		if err != nil {
			return nil, err
		}
		return initialMembershipResponseFromIntent(current)
	}
	if !db.IsNotFound(err) {
		return nil, err
	}
	if s.Config != nil && s.Config.EngineAdmissionHold {
		return nil, apperr.Conflictf("engine payment admission is held")
	}
	if err := accepted.Validate(); err != nil {
		return nil, err
	}
	if s.Config == nil {
		return nil, errors.New("engine initial membership custody is not configured")
	}
	var operation gen.BillingProviderIntent
	err = database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := database.NewWithPgxTx(tx)
		if _, err := d.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: accepted.CustomerID}); err != nil {
			return err
		}
		prior, err := intents.NewStore(d).GetByIdempotencyKey(ctx, InitialMembershipIdempotencyKey(key))
		if err == nil {
			operation = prior
			return ownsInitialMembership(prior, accepted.CustomerID.String(), accepted.PriceID, fingerprint, sessionID)
		}
		if !db.IsNotFound(err) {
			return err
		}
		method, err := d.Gen(ctx).GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: mid.UUID(), ID: accepted.PaymentMethodID})
		if err != nil {
			return err
		}
		if method.CustomerID != accepted.CustomerID || !charge.ChargeableOn(method, accepted.PSPID) || method.ParkReason != nil {
			return charge.ErrInstrumentChanged
		}
		if sessionID != nil {
			if *sessionID == uuid.Nil {
				return ErrCheckoutAttemptValidation
			}
			// Lock the validated persisted quote while accepting its binding.
			if _, err := d.Gen(ctx).LockCheckoutAttemptForShare(ctx, gen.LockCheckoutAttemptForShareParams{MerchantID: mid.UUID(), ID: *sessionID}); err != nil {
				return err
			}
			session, err := NewCheckoutAttemptRepo(d).GetByID(ctx, *sessionID)
			if err != nil {
				return err
			}
			quote, err := acceptedInitialMembershipQuote(ctx, session, principal, s.now())
			if err != nil {
				return err
			}
			if session.Rail != models.Rail(method.Rail) || quote.SubscriptionID != accepted.SubscriptionID || quote.PaymentID != accepted.PaymentID || initialMembershipQuoteFingerprint(quote) != fingerprint {
				return ErrCheckoutAttemptConflict
			}
		}
		var binding *charge.HyperSwitchBinding
		if method.Custodian == models.CustodianHyperSwitch {
			if s.Config.HyperSwitch == nil {
				return errors.New("engine HyperSwitch custody is not configured")
			}
			frozen, err := charge.FreezeHyperSwitchBinding(ctx, d.Gen(ctx), method, accepted.PSPID, s.Config.HyperSwitch.APIBaseURL)
			if err != nil {
				return err
			}
			binding = &frozen
		}
		if err := charge.ValidateEngineInstrument(method.Rail, charge.FreezeInstrument(method, accepted.PSPID), binding, false); err != nil {
			return err
		}
		price, err := d.Gen(ctx).GetPriceByID(ctx, gen.GetPriceByIDParams{MerchantID: mid.UUID(), ID: accepted.PriceID})
		if err != nil {
			return err
		}
		if price.ProductID != accepted.ProductID {
			return errors.New("quoted initial membership has another catalog identity")
		}
		psp, err := d.Gen(ctx).GetPSPForCutoverWrite(ctx, gen.GetPSPForCutoverWriteParams{MerchantID: mid.UUID(), ID: accepted.PSPID})
		if err != nil {
			return err
		}
		if psp.Archived || psp.Rail != method.Rail || psp.Environment != config.ExpectedProviderEnvironment(config.IsTestMode(s.Config)) {
			return errors.New("new membership provider account is no longer available")
		}
		if method.CustodianID != nil {
			custodian, err := d.Gen(ctx).GetCustodian(ctx, gen.GetCustodianParams{MerchantID: mid.UUID(), ID: *method.CustodianID})
			if err != nil {
				return err
			}
			if custodian.Archived {
				return errors.New("new membership custodian is archived")
			}
		}
		label := psp.Key
		payload := subscriptions.InitialMembershipPayload{CheckoutAttemptID: sessionID, Terms: accepted, Instrument: charge.FreezeInstrument(method, accepted.PSPID), RequestFingerprint: fingerprint, CheckoutIdempotencyKey: key, HyperSwitch: binding, PSP: label, Email: principal.Email}
		operation, err = intents.NewStore(d).Enqueue(ctx, intents.EnqueueParams{MerchantID: mid.UUID(), Provider: method.Rail, PspID: accepted.PSPID, IntentType: subscriptions.TypeInitialMembership, PriceID: &accepted.PriceID, Payload: payload, IdempotencyKey: InitialMembershipIdempotencyKey(key), NextAttemptAt: accepted.AcceptedAt, Origin: intents.OriginUser, Actor: principal.SubjectID, OriginReason: "customer confirmed initial membership"})
		return err
	})
	if err != nil {
		return nil, err
	}
	current, err := s.Intents.EnqueueOwnedAndExecute(ctx, initialMembershipReplayParams(operation), func(in gen.BillingProviderIntent) error {
		return ownsInitialMembership(in, accepted.CustomerID.String(), accepted.PriceID, fingerprint, sessionID)
	})
	if err != nil {
		return nil, err
	}
	return initialMembershipResponseFromIntent(current)
}
