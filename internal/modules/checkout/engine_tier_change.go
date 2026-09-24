package checkout

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Engine-owned subscriptions (OpenRails schedules and charges every period)
// change tier without any provider schedule:
//
//   - Upgrade, effective now: one engine charge of new price − unused credit
//     (QuoteModelBUpgrade) on the subscription's saved card, run as an
//     initial_membership operation for a successor membership that replaces
//     this one. It inherits every engine charge guarantee (admission lock,
//     frozen terms, idempotency, issuer authentication, decline custody,
//     lost-submission recovery). On success the old membership ends and the
//     successor opens a fresh period of the new cadence; on refusal nothing
//     changes. Renewals bill the new price.
//   - Downgrade, effective at period end: the new price is scheduled; access
//     and price stay until the current period ends, the renewal bills the new
//     price for a period of its cadence, and nothing is refunded.

var (
	errTierChangeRenewalDue = &TierChangeError{HTTPStatus: http.StatusConflict, Code: openrails.CodeTierChangeRenewalDue, Message: "the current period has ended or its renewal is unresolved; change tier after the renewal settles"}
	errTierChangeScheduled  = &TierChangeError{HTTPStatus: http.StatusConflict, Code: openrails.CodeTierChangeAlreadyScheduled, Message: "another plan change is already scheduled for the end of this period"}
	errTierChangeMoved      = &TierChangeError{HTTPStatus: http.StatusConflict, Code: openrails.CodeTierChangeRefused, Message: "the subscription changed since the tier change was requested; preview again"}
)

// engineUpgradeQuote prices an engine upgrade at now and freezes the successor
// membership terms. The preview and the charge share it.
func engineUpgradeQuote(sub *models.Subscription, current, target *models.Price, product *models.Product, now time.Time) (subscriptions.InitialMembershipTerms, error) {
	var terms subscriptions.InitialMembershipTerms
	if sub.Status != models.StatusActive || (sub.CurrentPeriodEndsAt != nil && !sub.CurrentPeriodEndsAt.After(now)) {
		return terms, errTierChangeRenewalDue
	}
	if sub.PaymentMethodID == nil {
		return terms, ErrPaymentMethodStale
	}
	quote, err := QuoteModelBUpgrade(modelBUpgradeOf(sub, current, target), now)
	if err != nil {
		return terms, err
	}
	// An engine membership renews only from a paid agreement: the upgrade must
	// charge something.
	if quote.ChargeNow <= 0 {
		return terms, ErrTierChangeCreditExceedsPrice
	}
	benefits := models.CloneEntitlementsSpec(product.EntitlementsSpec)
	if benefits == nil {
		benefits = map[string]*int{}
	}
	terms = subscriptions.InitialMembershipTerms{
		CollectionPolicy: models.CollectionPolicyEngine, SubscriptionID: uuidutil.NewV7(), PaymentID: uuidutil.NewV7(),
		CustomerID: sub.CustomerID, PSPID: sub.PspID, ProductID: product.ID, PriceID: target.ID, PaymentMethodID: *sub.PaymentMethodID,
		ProductName: product.DisplayName, Amount: quote.ChargeNow, RecurringAmount: target.Amount, Currency: target.Currency,
		AcceptedAt: quote.PeriodStart, PeriodStart: quote.PeriodStart, PeriodEnd: quote.PeriodEnd, Entitlements: benefits,
		Replaces: &subscriptions.ReplacedMembership{SubscriptionID: sub.ID, PriceID: sub.PriceID, PeriodEnd: sub.CurrentPeriodEndsAt.UTC(), Credit: quote.Credit},
	}
	return terms, terms.Validate()
}

func (s *CheckoutService) processEngineUpgrade(ctx context.Context, req *TierChangeRequest, user *UserIdentity, newPrice *models.Price, newProduct *models.Product, existingSub *models.Subscription, currentPrice *models.Price) (*TierChangeResponse, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if s.Intents == nil || s.Lifecycle == nil || s.Config == nil || s.SubscriptionService == nil {
		return nil, errors.New("engine tier change services unavailable")
	}
	if s.Config.EngineAdmissionHold {
		return nil, &TierChangeError{HTTPStatus: http.StatusConflict, Code: openrails.CodeTierChangeRefused, Message: "engine payment admission is held"}
	}
	ctx = db.WithPSPID(ctx, existingSub.PspID)
	terms, err := engineUpgradeQuote(existingSub, currentPrice, newPrice, newProduct, s.now().UTC().Truncate(time.Microsecond))
	if err != nil {
		return nil, err
	}
	key := tierChangeIdempotencyKey(req.IdempotencyKey)
	requested := strings.TrimSpace(req.PriceID)
	fingerprint := sha256.Sum256([]byte(strings.Join([]string{terms.CustomerID.String(), existingSub.ID.String(), newPrice.ID.String(), key}, "\x00")))
	database := s.SubscriptionService.Database()
	var operation gen.OpenrailsRailIntent
	err = database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := database.NewWithPgxTx(tx)
		if _, err := d.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: terms.CustomerID}); err != nil {
			return err
		}
		method, err := d.Gen(ctx).GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: mid.UUID(), ID: terms.PaymentMethodID})
		if err != nil {
			return err
		}
		if method.CustomerID != terms.CustomerID || method.PspID != terms.PSPID || method.Rail != string(existingSub.Rail) || method.ParkReason != "" {
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
		psp, err := d.Gen(ctx).GetPSPForCutoverWrite(ctx, gen.GetPSPForCutoverWriteParams{MerchantID: mid.UUID(), ID: terms.PSPID})
		if err != nil {
			return err
		}
		if psp.Archived || psp.Rail != method.Rail || psp.Environment != config.ExpectedProviderEnvironment(s.Config.IsTestMode()) {
			return &TierChangeError{HTTPStatus: http.StatusConflict, Code: openrails.CodeTierChangeRefused, Message: "the subscription's payment provider account is no longer available"}
		}
		label := psp.ID.String()
		if psp.Key != nil && strings.TrimSpace(*psp.Key) != "" {
			label = *psp.Key
		}
		email := ""
		if user.Email != nil {
			email = strings.TrimSpace(*user.Email)
		}
		payload := subscriptions.InitialMembershipPayload{Terms: terms, Instrument: charge.FreezeInstrument(method), RequestFingerprint: fmt.Sprintf("%x", fingerprint), CheckoutIdempotencyKey: key, HyperSwitch: binding, PSP: label, Email: email, RequestedPrice: requested}
		operation, err = intents.NewStore(d).Enqueue(ctx, intents.EnqueueParams{MerchantID: mid.UUID(), Provider: method.Rail, PspID: terms.PSPID, IntentType: TypeInitialMembership, SubscriptionID: &existingSub.ID, PriceID: &terms.PriceID, Payload: payload, IdempotencyKey: InitialMembershipIdempotencyKey(key), NextAttemptAt: terms.AcceptedAt, Origin: intents.OriginUser, Actor: terms.CustomerID.String(), OriginReason: "customer tier upgrade"})
		return err
	})
	var conflict *pgconn.PgError
	if errors.As(err, &conflict) && conflict.Code == "23505" && conflict.ConstraintName == tierChangeSubjectConstraint {
		return nil, s.tierChangeInFlight(ctx, existingSub.ID)
	}
	if err != nil {
		return nil, s.engineUpgradeRefusal(ctx, err, existingSub)
	}
	subject := tierChangeSubject{UserID: terms.CustomerID.String(), SubscriptionID: existingSub.ID, RequestedPrice: requested, PriceID: newPrice.ID}
	current, err := s.Intents.EnqueueOwnedAndExecute(ctx, initialMembershipReplayParams(operation), func(in gen.OpenrailsRailIntent) error { return tierChangeOwnedBy(in, subject) })
	if err != nil {
		return nil, err
	}
	return tierChangeResponse(current)
}

// engineUpgradeRefusal types an admission refusal; nothing was charged.
func (s *CheckoutService) engineUpgradeRefusal(ctx context.Context, err error, sub *models.Subscription) error {
	var typed *TierChangeError
	switch {
	case errors.As(err, &typed):
		return err
	case errors.Is(err, subscriptions.ErrUpgradeRenewalDue):
		return errTierChangeRenewalDue
	case errors.Is(err, subscriptions.ErrUpgradeReplacedChanged):
		return errTierChangeMoved
	case errors.Is(err, charge.ErrInstrumentChanged):
		return ErrPaymentMethodStale
	}
	var inFlight *TierChangeInFlightError
	if owner := s.refuseTierChangeInFlight(ctx, sub); errors.As(owner, &inFlight) {
		return owner
	}
	return err
}

// processEngineDowngrade schedules the target price for the end of the
// current period. Repeating it for the same target answers the same result.
func (s *CheckoutService) processEngineDowngrade(ctx context.Context, newPrice *models.Price, newProduct *models.Product, existingSub *models.Subscription) (*TierChangeResponse, error) {
	if err := s.engineDowngradeAdmissible(ctx, newPrice, existingSub); err != nil {
		return nil, err
	}
	if existingSub.ScheduledPriceID == nil {
		scheduled, err := subscriptions.NewSubscriptionRepo(s.SubscriptionService.Database()).SchedulePriceChange(ctx, existingSub.ID, existingSub.PriceID, newPrice.ID)
		switch {
		case errors.Is(err, subscriptions.ErrRebillTermsCommitted):
			return nil, errTierChangeRenewalDue
		case errors.Is(err, subscriptions.ErrRepriceAlreadyScheduled):
			if scheduled, err = s.SubscriptionService.GetByID(ctx, existingSub.ID); err != nil {
				return nil, err
			}
			if scheduled.ScheduledPriceID == nil || *scheduled.ScheduledPriceID != newPrice.ID || scheduled.PriceID != existingSub.PriceID {
				return nil, errTierChangeScheduled
			}
		case err != nil:
			return nil, err
		}
		existingSub = scheduled
	}
	end := *existingSub.CurrentPeriodEndsAt
	subID := openrails.SubscriptionID(existingSub.ID)
	return &TierChangeResponse{
		Object: "tier_change", Status: "succeeded", Mode: "tier_change", Action: "downgrade", Effective: "period_end",
		PriceID: openrails.PriceID(newPrice.ID).String(), Payment: CheckoutSessionPaymentResponse{Rail: string(existingSub.Rail)}, SubscriptionID: &subID,
		Message:      fmt.Sprintf("Downgrade to %s scheduled. Your current plan stays active until the period ends.", newProduct.DisplayName),
		DelayedStart: &end, Currency: newPrice.Currency, NextChargeAmount: newPrice.Amount, NextChargeDate: &end,
	}, nil
}

// engineDowngradeAdmissible refuses what the period-end renewal could not
// honour. A schedule already naming this target is the same request.
func (s *CheckoutService) engineDowngradeAdmissible(ctx context.Context, newPrice *models.Price, sub *models.Subscription) error {
	if cycle := newPrice.RecurringCycleHours(); cycle == nil || *cycle <= 0 {
		return ErrTierChangeCycleUnknown
	}
	if sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.IsZero() {
		return ErrTierChangePeriodUnknown
	}
	if sub.ScheduledPriceID != nil && *sub.ScheduledPriceID != newPrice.ID {
		return errTierChangeScheduled
	}
	if _, err := subscriptions.NewRepriceRepo(s.SubscriptionService.Database()).GetScheduledForSubscription(ctx, sub.ID); err == nil {
		return errTierChangeScheduled
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return nil
}

// previewEngineTierChange quotes exactly what processEngineUpgrade charges and
// when processEngineDowngrade takes effect.
func (s *CheckoutService) previewEngineTierChange(ctx context.Context, resp *TierChangePreviewResponse, sub *models.Subscription, current, target *models.Price, product *models.Product, downgrade bool) (*TierChangePreviewResponse, error) {
	if downgrade {
		if err := s.engineDowngradeAdmissible(ctx, target, sub); err != nil {
			return nil, err
		}
		end := *sub.CurrentPeriodEndsAt
		resp.Action, resp.Effective, resp.NextChargeDate = "downgrade", "period_end", &end
		resp.Message = fmt.Sprintf("No charge now. Your plan changes to %s at the end of the current period, then renews at %s.", product.DisplayName, formatMinorAmount(target.Amount, target.Currency))
		return resp, nil
	}
	terms, err := engineUpgradeQuote(sub, current, target, product, s.now().UTC().Truncate(time.Microsecond))
	if err != nil {
		return nil, err
	}
	end := terms.PeriodEnd
	resp.Action, resp.Effective, resp.AmountDueNow, resp.NextChargeDate = "upgrade", "now", terms.Amount, &end
	resp.Message = fmt.Sprintf("You'll be charged %s now and %s on %s.", formatMinorAmount(terms.Amount, target.Currency), formatMinorAmount(target.Amount, target.Currency), end.Format("January 2, 2006"))
	return resp, nil
}

// engineUpgradeTierChangeResponse renders an engine upgrade: the replaced
// membership while unresolved, the successor once paid.
func engineUpgradeTierChangeResponse(in gen.OpenrailsRailIntent) (*TierChangeResponse, error) {
	p, err := subscriptions.DecodeInitialMembershipPayload(in)
	if err != nil {
		return nil, err
	}
	if !p.Upgrade() {
		return nil, fmt.Errorf("intent %s is not a tier upgrade", in.ID)
	}
	replaced := openrails.SubscriptionID(p.Terms.Replaces.SubscriptionID)
	end := p.Terms.PeriodEnd
	resp := &TierChangeResponse{
		Object: "tier_change", Mode: "tier_change", Action: "upgrade", Effective: "now", PriceID: openrails.PriceID(p.Terms.PriceID).String(),
		Payment: CheckoutSessionPaymentResponse{Rail: in.Rail}, SubscriptionID: &replaced,
		Currency: p.Terms.Currency, AmountDueNow: p.Terms.Amount, NextChargeAmount: p.Terms.RecurringAmount, NextChargeDate: &end,
		OperationID: in.ID.String(),
	}
	switch in.Status {
	case intents.StatusSucceeded:
		if err := intents.ValidateInitialMembershipTerminal(in); err != nil {
			return nil, err
		}
		successor := openrails.SubscriptionID(p.Terms.SubscriptionID)
		resp.Status, resp.SubscriptionID = "succeeded", &successor
		resp.Payment.TransactionID = intents.EvidenceString(in, "transaction_id")
		resp.Message = intents.EvidenceString(in, "message")
		return resp, nil
	case intents.StatusFailedTerminal, intents.StatusExpired, intents.StatusSuperseded:
		var evidence struct {
			Declined bool `json:"declined"`
		}
		_ = json.Unmarshal(in.ResultEvidence, &evidence)
		if failure := operationFailure(in); evidence.Declined && failure != nil {
			return nil, tierChangeRefused(in, http.StatusPaymentRequired, failure.Reason)
		}
		return nil, tierChangeRefused(in, 0, "")
	default:
		if authenticationRequired(in) {
			resp.Status = "requires_action"
			resp.NextAction = &CheckoutSessionNextAction{Type: "payment_authentication"}
			resp.Message = "The card issuer requires authentication; authenticate operation " + in.ID.String() + " to complete the upgrade"
			return resp, nil
		}
		return tierChangeProcessing(resp)
	}
}

// effectiveOf is when a tier change takes effect: an upgrade now, a
// downgrade at the end of the current period.
func effectiveOf(action string) string {
	if action == "downgrade" {
		return "period_end"
	}
	return "now"
}
