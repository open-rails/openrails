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
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

// initialMembershipQuoteFingerprint excludes newly allocated IDs and acceptance
// instants. A replay compares the customer's displayed commercial decision.
func initialMembershipQuoteFingerprint(terms subscriptions.InitialMembershipTerms) string {
	duration := terms.PeriodEnd.Sub(terms.PeriodStart)
	terms.SubscriptionID, terms.PaymentID = uuid.Nil, uuid.Nil
	terms.AcceptedAt, terms.PeriodStart, terms.PeriodEnd = time.Time{}, time.Time{}, time.Time{}
	raw, _ := json.Marshal(struct {
		Terms    subscriptions.InitialMembershipTerms
		Duration time.Duration
	}{terms, duration})
	digest := sha256.Sum256(raw)
	return fmt.Sprintf("%x", digest)
}

// ConfirmInitialMembership consumes a priced session's accepted terms and its
// already-verified interactive payer. The self-session adapter owns quote expiry
// and display agreement; neither a merchant confirm nor a body can mint this
// principal. Provider routing/credentials never come from the quoted payload.
func (s *CheckoutService) ConfirmInitialMembership(ctx context.Context, accepted subscriptions.InitialMembershipTerms, key string, principal billingauth.DelegatedPrincipal) (*CheckoutResponse, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if principal.Validate() != nil || principal.CredentialClass != billingauth.CredentialClassUserSession || principal.Invoker != "" || principal.MerchantID != mid.String() || principal.SubjectID != accepted.CustomerID.String() {
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
		if err := ownsInitialMembership(prior, accepted.CustomerID.String(), accepted.PriceID, fingerprint); err != nil {
			return nil, err
		}
		current, err := s.Intents.EnqueueOwnedAndExecute(ctx, initialMembershipReplayParams(prior), func(in gen.OpenrailsRailIntent) error {
			return ownsInitialMembership(in, accepted.CustomerID.String(), accepted.PriceID, fingerprint)
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
	var operation gen.OpenrailsRailIntent
	err = database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := database.NewWithPgxTx(tx)
		if _, err := d.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: accepted.CustomerID}); err != nil {
			return err
		}
		prior, err := intents.NewStore(d).GetByIdempotencyKey(ctx, InitialMembershipIdempotencyKey(key))
		if err == nil {
			operation = prior
			return ownsInitialMembership(prior, accepted.CustomerID.String(), accepted.PriceID, fingerprint)
		}
		if !db.IsNotFound(err) {
			return err
		}
		method, err := d.Gen(ctx).GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: mid.UUID(), ID: accepted.PaymentMethodID})
		if err != nil {
			return err
		}
		if method.CustomerID != accepted.CustomerID || method.PspID != accepted.PSPID || method.ParkReason != "" {
			return charge.ErrInstrumentChanged
		}
		var binding *charge.HyperSwitchBinding
		if method.Custodian == models.CustodianHyperSwitch {
			if s.Config.HyperSwitch == nil {
				return errors.New("engine HyperSwitch custody is not configured")
			}
			frozen, err := charge.FreezeHyperSwitchBinding(ctx, d.Gen(ctx), method, s.Config.HyperSwitch.APIBaseURL)
			if err != nil {
				return err
			}
			binding = &frozen
		}
		if err := charge.ValidateEngineInstrument(method.Rail, charge.FreezeInstrument(method), binding, false); err != nil {
			return err
		}
		price, err := d.Gen(ctx).GetPriceByID(ctx, gen.GetPriceByIDParams{MerchantID: mid.UUID(), ID: accepted.PriceID})
		if err != nil {
			return err
		}
		if price.ProductID != accepted.ProductID {
			return errors.New("quoted initial membership has another catalog identity")
		}
		psp, err := d.Gen(ctx).GetPSP(ctx, gen.GetPSPParams{MerchantID: mid.UUID(), ID: accepted.PSPID})
		if err != nil {
			return err
		}
		label := psp.ID.String()
		if psp.Key != nil && strings.TrimSpace(*psp.Key) != "" {
			label = *psp.Key
		}
		payload := subscriptions.InitialMembershipPayload{Terms: accepted, Instrument: charge.FreezeInstrument(method), RequestFingerprint: fingerprint, CheckoutIdempotencyKey: key, HyperSwitch: binding, PSP: label, Email: principal.Email}
		operation, err = intents.NewStore(d).Enqueue(ctx, intents.EnqueueParams{MerchantID: mid.UUID(), Provider: method.Rail, PspID: accepted.PSPID, IntentType: subscriptions.TypeInitialMembership, PriceID: &accepted.PriceID, Payload: payload, IdempotencyKey: InitialMembershipIdempotencyKey(key), NextAttemptAt: accepted.AcceptedAt, Origin: intents.OriginUser, Actor: principal.SubjectID, OriginReason: "customer confirmed initial membership"})
		return err
	})
	if err != nil {
		return nil, err
	}
	current, err := s.Intents.EnqueueOwnedAndExecute(ctx, initialMembershipReplayParams(operation), func(in gen.OpenrailsRailIntent) error {
		return ownsInitialMembership(in, accepted.CustomerID.String(), accepted.PriceID, fingerprint)
	})
	if err != nil {
		return nil, err
	}
	return initialMembershipResponseFromIntent(current)
}
