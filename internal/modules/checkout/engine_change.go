package checkout

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// Engine-owned subscriptions (OpenRails schedules and charges every period)
// change without any provider schedule:
//
//   - A tier upgrade, effective now: one engine charge of new price × seats −
//     unused credit (QuoteModelBUpgrade) on the subscription's saved card, run
//     as an initial_membership operation for a successor membership that
//     replaces this one and opens a fresh period of the new cadence.
//   - More seats, effective now: one engine charge of the added seats for the
//     rest of the period, run as an initial_membership operation that changes
//     this membership in place; its period and price stay.
//   - Fewer seats, a tier downgrade, and every change by staff, effective at
//     period end: the price and seats are scheduled, nothing is charged or
//     refunded, and the renewal bills them.
//
// A charge inherits every engine charge guarantee (admission lock, frozen
// terms, idempotency, issuer authentication, decline custody, lost-submission
// recovery); on refusal nothing changes.

var (
	errTierChangeRenewalDue     = &TierChangeError{Code: billing.CodeSubscriptionChangeRenewalDue, Message: "the current period has ended or its renewal is unresolved; change after the renewal settles"}
	errTierChangeScheduled      = &TierChangeError{Code: billing.CodeSubscriptionChangeAlreadyScheduled, Message: "another plan change is already scheduled for the end of this period"}
	errTierChangeMoved          = &TierChangeError{Code: billing.CodeSubscriptionChangeRefused, Message: "the subscription changed since the change was requested; preview again"}
	errStoredCredentialRequired = &TierChangeError{Code: billing.CodeStoredCredentialRequired, Message: "the card has no active recurring agreement for a staff charge; the customer makes this change"}
)

// engineUpgradeQuote prices an engine upgrade to target at quantity seats at
// now and freezes the successor membership terms. The preview and the charge
// share it.
func engineUpgradeQuote(sub *models.Subscription, current, target *models.Price, product *models.Product, quantity *int, now time.Time) (subscriptions.InitialMembershipTerms, error) {
	var terms subscriptions.InitialMembershipTerms
	if sub.Status != models.StatusActive || (sub.CurrentPeriodEndsAt != nil && !sub.CurrentPeriodEndsAt.After(now)) {
		return terms, errTierChangeRenewalDue
	}
	if sub.PaymentMethodID == nil {
		return terms, ErrPaymentMethodStale
	}
	upgrade, err := modelBUpgradeOf(sub, current, target, quantity)
	if err != nil {
		return terms, err
	}
	quote, err := QuoteModelBUpgrade(upgrade, now)
	if err != nil {
		return terms, err
	}
	// An engine membership renews only from a paid agreement: the upgrade must
	// charge something.
	if quote.ChargeNow <= 0 {
		return terms, ErrTierChangeCreditExceedsPrice
	}
	terms = subscriptions.InitialMembershipTerms{
		CollectionPolicy: models.CollectionPolicyEngine, SubscriptionID: uuidutil.NewV7(), PaymentID: uuidutil.NewV7(),
		CustomerID: sub.CustomerID, PSPID: sub.PspID, ProductID: product.ID, PriceID: target.ID, PaymentMethodID: *sub.PaymentMethodID,
		ProductName: product.DisplayName, Quantity: subscriptions.CloneQuantity(quantity), Amount: quote.ChargeNow, RecurringAmount: upgrade.New.Micros, Currency: target.Currency, AccessDurationHours: target.AccessDurationHours,
		AcceptedAt: quote.PeriodStart, PeriodStart: quote.PeriodStart, PeriodEnd: quote.PeriodEnd,
		Replaces: &subscriptions.ReplacedMembership{SubscriptionID: sub.ID, PriceID: sub.PriceID, PeriodEnd: sub.CurrentPeriodEndsAt.UTC(), Credit: quote.Credit},
	}
	return terms, terms.Validate()
}

// seatIncreaseQuote prices more seats on the current price for the rest of the
// period at now and freezes the terms. Amount zero (the period is ending) adds
// them free.
func seatIncreaseQuote(sub *models.Subscription, current *models.Price, product *models.Product, quantity *int, now time.Time) (subscriptions.InitialMembershipTerms, error) {
	var terms subscriptions.InitialMembershipTerms
	if sub.Status != models.StatusActive || sub.CurrentPeriodEndsAt == nil || !sub.CurrentPeriodEndsAt.After(now) {
		return terms, errTierChangeRenewalDue
	}
	if sub.CurrentPeriodStartsAt == nil || !sub.CurrentPeriodEndsAt.After(*sub.CurrentPeriodStartsAt) {
		return terms, ErrTierChangePeriodUnknown
	}
	if sub.PaymentMethodID == nil {
		return terms, ErrPaymentMethodStale
	}
	if sub.Quantity == nil || quantity == nil || *quantity <= *sub.Quantity {
		return terms, errors.New("a seat increase needs more seats than the subscription has")
	}
	added, err := seatPriceAmount(current, *quantity-*sub.Quantity)
	if err != nil {
		return terms, err
	}
	charge, err := QuoteSeatIncrease(added, *sub.CurrentPeriodStartsAt, *sub.CurrentPeriodEndsAt, now)
	if err != nil {
		return terms, err
	}
	recurring, err := seatPrice(current, quantity)
	if err != nil {
		return terms, err
	}
	terms = subscriptions.InitialMembershipTerms{
		CollectionPolicy: models.CollectionPolicyEngine, SubscriptionID: sub.ID,
		CustomerID: sub.CustomerID, PSPID: sub.PspID, ProductID: product.ID, PriceID: current.ID, PaymentMethodID: *sub.PaymentMethodID,
		ProductName: product.DisplayName, Quantity: subscriptions.CloneQuantity(quantity), Amount: charge, RecurringAmount: recurring, Currency: current.Currency, AccessDurationHours: sub.AccessDurationHoursSnapshot,
		AcceptedAt: now, PeriodStart: now, PeriodEnd: sub.CurrentPeriodEndsAt.UTC(),
		Adds: &subscriptions.AddedSeats{FromQuantity: *sub.Quantity},
	}
	if charge == 0 {
		return terms, nil
	}
	terms.PaymentID = uuidutil.NewV7()
	return terms, terms.Validate()
}

func (s *CheckoutService) processEngineUpgrade(ctx context.Context, req *SubscriptionChangeRequest, user *UserIdentity, c *changeTarget) (*TierChangeResponse, error) {
	terms, err := engineUpgradeQuote(c.sub, c.currentPrice, c.price, c.product, c.quantity, s.now().UTC().Truncate(time.Microsecond))
	if err != nil {
		return nil, err
	}
	return s.enqueueEngineChange(ctx, req, user, c.sub, terms)
}

// addEngineSeats charges more seats now. Seats that cost nothing (the period
// is ending) are added without a charge.
func (s *CheckoutService) addEngineSeats(ctx context.Context, req *SubscriptionChangeRequest, user *UserIdentity, c *changeTarget) (*TierChangeResponse, error) {
	terms, err := seatIncreaseQuote(c.sub, c.currentPrice, c.product, c.quantity, s.now().UTC().Truncate(time.Microsecond))
	if err != nil {
		return nil, err
	}
	if terms.Amount > 0 {
		return s.enqueueEngineChange(ctx, req, user, c.sub, terms)
	}
	database := s.SubscriptionService.Database()
	err = database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := database.NewWithPgxTx(tx)
		if _, err := d.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: c.sub.MerchantID, ID: c.sub.CustomerID}); err != nil {
			return err
		}
		if _, err := subscriptions.LockSeatMembership(ctx, d, terms, c.sub.Rail, true); err != nil {
			return err
		}
		return s.Lifecycle.AddSeatsTx(ctx, d, terms, c.sub.Rail, "", "", staffChange(req))
	})
	if err != nil {
		return nil, s.engineUpgradeRefusal(ctx, err, c.sub)
	}
	subID := billing.SubscriptionID(c.sub.ID)
	end := terms.PeriodEnd
	return &TierChangeResponse{
		Status: "succeeded", Effective: "now", PriceID: billing.PriceID(terms.PriceID), Quantity: subscriptions.CloneQuantity(terms.Quantity), Rail: string(c.sub.Rail), SubscriptionID: &subID,
		Currency: terms.Currency, NextChargeAmount: terms.RecurringAmount, NextChargeDate: &end,
		Message: fmt.Sprintf("Now %d seats; nothing is due before %s.", *terms.Quantity, end.UTC().Format("January 2, 2006")),
	}, nil
}

// enqueueEngineChange runs an accepted engine charge as the durable operation
// the request's Idempotency-Key names.
func (s *CheckoutService) enqueueEngineChange(ctx context.Context, req *SubscriptionChangeRequest, user *UserIdentity, sub *models.Subscription, terms subscriptions.InitialMembershipTerms) (*TierChangeResponse, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if s.Intents == nil || s.Lifecycle == nil || s.Config == nil || s.SubscriptionService == nil {
		return nil, errors.New("engine subscription change services unavailable")
	}
	if s.Config.EngineAdmissionHold {
		return nil, &TierChangeError{Code: billing.CodeSubscriptionChangeRefused, Message: "engine payment admission is held"}
	}
	ctx = db.WithPSPID(ctx, sub.PspID)
	key := tierChangeIdempotencyKey(tierChangeCustomer(user), req.IdempotencyKey)
	requested := strings.TrimSpace(req.PriceID)
	fingerprint := sha256.Sum256([]byte(strings.Join([]string{terms.CustomerID.String(), sub.ID.String(), terms.PriceID.String(), strconv.Itoa(subscriptions.SeatCount(terms.Quantity)), key}, "\x00")))
	origin, actor, reason := intents.OriginUser, terms.CustomerID.String(), "customer tier upgrade"
	if terms.Adds != nil {
		reason = "customer seat increase"
	}
	staff := staffChange(req)
	if staff != nil {
		origin, actor, reason = intents.OriginAdmin, staff.Invoker, strings.Replace(reason, "customer", "staff", 1)+": "+staff.Reason
	}
	database := s.SubscriptionService.Database()
	var operation gen.BillingProviderIntent
	err = database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := database.NewWithPgxTx(tx)
		if _, err := d.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: terms.CustomerID}); err != nil {
			return err
		}
		method, err := d.Gen(ctx).GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: mid.UUID(), ID: terms.PaymentMethodID})
		if err != nil {
			return err
		}
		if method.CustomerID != terms.CustomerID || !charge.ChargeableOn(method, terms.PSPID) || method.Rail != string(sub.Rail) || method.ParkReason != nil {
			return charge.ErrInstrumentChanged
		}
		var binding *charge.HyperSwitchBinding
		if method.Custodian == models.CustodianHyperSwitch {
			if s.Config.HyperSwitch == nil {
				return errors.New("engine HyperSwitch custody is not configured")
			}
			frozen, err := charge.FreezeHyperSwitchBinding(ctx, d.Gen(ctx), method, terms.PSPID, s.Config.HyperSwitch.APIBaseURL)
			if err != nil {
				return err
			}
			binding = &frozen
		}
		if err := charge.ValidateEngineInstrument(method.Rail, charge.FreezeInstrument(method, terms.PSPID), binding); err != nil {
			return err
		}
		psp, err := d.Gen(ctx).GetPSPForCutoverWrite(ctx, gen.GetPSPForCutoverWriteParams{MerchantID: mid.UUID(), ID: terms.PSPID})
		if err != nil {
			return err
		}
		if psp.Archived || psp.Rail != method.Rail || psp.Environment != config.ExpectedProviderEnvironment(config.IsTestMode(s.Config)) {
			return &TierChangeError{Code: billing.CodeSubscriptionChangeRefused, Message: "the subscription's payment provider account is no longer available"}
		}
		instrument, err := enrollmentInstrument(ctx, d.Gen(ctx), method, terms.PSPID)
		if err != nil {
			return err
		}
		// Staff charge merchant-initiated, under the subscription's recurring
		// agreement on this card; without one the customer makes the change.
		if staff != nil {
			if instrument.Mandate, err = mandates.ForSubscription(ctx, d.Gen(ctx), mid.UUID(), terms.CustomerID, sub.ID, method.ID, terms.PSPID); errors.Is(err, mandates.ErrMissing) || errors.Is(err, mandates.ErrNotActive) {
				return errStoredCredentialRequired
			} else if err != nil {
				return err
			}
		}
		email := ""
		if user.Email != nil {
			email = strings.TrimSpace(*user.Email)
		}
		payload := subscriptions.InitialMembershipPayload{Terms: terms, Instrument: instrument, RequestFingerprint: fmt.Sprintf("%x", fingerprint), CheckoutIdempotencyKey: key, HyperSwitch: binding, PSP: psp.Key, Email: email, RequestedPrice: requested, Staff: staff}
		operation, err = intents.NewStore(d).Enqueue(ctx, intents.EnqueueParams{MerchantID: mid.UUID(), Provider: method.Rail, PspID: terms.PSPID, IntentType: TypeInitialMembership, SubscriptionID: &sub.ID, PriceID: &terms.PriceID, Payload: payload, IdempotencyKey: InitialMembershipIdempotencyKey(key), NextAttemptAt: terms.AcceptedAt, Origin: origin, Actor: actor, OriginReason: reason})
		return err
	})
	var conflict *pgconn.PgError
	if errors.As(err, &conflict) && conflict.Code == "23505" && conflict.ConstraintName == tierChangeSubjectConstraint {
		return nil, s.tierChangeInFlight(ctx, sub.ID)
	}
	if err != nil {
		return nil, s.engineUpgradeRefusal(ctx, err, sub)
	}
	subject := tierChangeSubject{UserID: terms.CustomerID.String(), SubscriptionID: sub.ID, RequestedPrice: requested, PriceID: terms.PriceID, Quantity: subscriptions.CloneQuantity(terms.Quantity)}
	current, err := s.Intents.EnqueueOwnedAndExecute(ctx, initialMembershipReplayParams(operation), func(in gen.BillingProviderIntent) error { return tierChangeOwnedBy(in, subject) })
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

// scheduleEngineChange makes the change the subscription's pending change
// for the end of the current period; nothing is charged. Repeating it answers
// the same result.
func (s *CheckoutService) scheduleEngineChange(ctx context.Context, c *changeTarget) (*TierChangeResponse, error) {
	if err := s.engineScheduleAdmissible(c); err != nil {
		return nil, err
	}
	next, product, quantity, err := s.renewalTarget(ctx, c)
	if err != nil {
		return nil, err
	}
	sub := c.sub
	var expected *uuid.UUID
	if c.pending != nil {
		expected = &c.pending.ID
	}
	_, err = subscriptions.NewSubscriptionRepo(s.SubscriptionService.Database()).ReplaceScheduledChange(ctx, sub.ID, subscriptions.RenewalChange{
		ExpectedPriceID: sub.PriceID, ExpectedQuantity: subscriptions.CloneQuantity(sub.Quantity), ExpectedPending: expected,
		PriceID: next.ID, Quantity: quantity, Staff: c.staff,
	}, s.now())
	if err := scheduleRefusal(err); err != nil {
		return nil, err
	}
	amount, err := seatPrice(next, quantity)
	if err != nil {
		return nil, err
	}
	end := *sub.CurrentPeriodEndsAt
	subID := billing.SubscriptionID(sub.ID)
	return &TierChangeResponse{
		Status: "succeeded", Effective: "period_end", PriceID: billing.PriceID(next.ID), Quantity: quantity,
		Rail: string(sub.Rail), SubscriptionID: &subID, DelayedStart: &end,
		Currency: next.Currency, NextChargeAmount: amount, NextChargeDate: &end,
		Message: fmt.Sprintf("Scheduled: %s%s from %s. Nothing is charged now.", product.DisplayName, seatsText(quantity), end.UTC().Format("January 2, 2006")),
	}, nil
}

// cancelEngineChange answers a change back to what the subscription bills
// now: its pending change is canceled and nothing is charged.
func (s *CheckoutService) cancelEngineChange(ctx context.Context, c *changeTarget) (*TierChangeResponse, error) {
	sub := c.sub
	if sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.IsZero() {
		return nil, ErrTierChangePeriodUnknown
	}
	_, err := subscriptions.NewSubscriptionRepo(s.SubscriptionService.Database()).ReplaceScheduledChange(ctx, sub.ID, subscriptions.RenewalChange{
		ExpectedPriceID: sub.PriceID, ExpectedQuantity: subscriptions.CloneQuantity(sub.Quantity), ExpectedPending: &c.pending.ID,
		PriceID: sub.PriceID, Quantity: subscriptions.CloneQuantity(sub.Quantity), Staff: c.staff,
	}, s.now())
	if err := scheduleRefusal(err); err != nil {
		return nil, err
	}
	amount, err := seatPrice(c.currentPrice, sub.Quantity)
	if err != nil {
		return nil, err
	}
	end := *sub.CurrentPeriodEndsAt
	subID := billing.SubscriptionID(sub.ID)
	return &TierChangeResponse{
		Status: "succeeded", Effective: "now", PriceID: billing.PriceID(sub.PriceID), Quantity: subscriptions.CloneQuantity(sub.Quantity),
		Rail: string(sub.Rail), SubscriptionID: &subID,
		Currency: c.currentPrice.Currency, NextChargeAmount: amount, NextChargeDate: &end,
		Message: "The scheduled change is canceled; nothing is charged.",
	}, nil
}

func scheduleRefusal(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, subscriptions.ErrRebillTermsCommitted):
		return errTierChangeRenewalDue
	case errors.Is(err, subscriptions.ErrUpgradeReplacedChanged):
		return errTierChangeMoved
	case errors.Is(err, subscriptions.ErrChangeAlreadyScheduled):
		return errTierChangeScheduled
	}
	return err
}

// engineScheduleAdmissible refuses what the period-end renewal could not
// honour, and a change to another tier while one is pending: a pending price
// migration is canceled only by staff, and a pending tier change by changing
// back.
func (s *CheckoutService) engineScheduleAdmissible(c *changeTarget) error {
	sub := c.sub
	if sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.IsZero() {
		return ErrTierChangePeriodUnknown
	}
	if c.pending != nil && (c.pending.Source != billing.ScheduledChangeChange || c.tier && c.pending.PriceID != c.price.ID) {
		return errTierChangeScheduled
	}
	if !c.tier {
		return nil
	}
	if cycle := c.price.RecurringCycleHours(); cycle == nil || *cycle <= 0 {
		return ErrTierChangeCycleUnknown
	}
	return nil
}

// renewalTarget is the price and seats the next renewal bills after a
// deferred change: the target of a tier change, else the pending change's
// price unless the request names one; seats within that price's bounds.
func (s *CheckoutService) renewalTarget(ctx context.Context, c *changeTarget) (*models.Price, *models.Product, *int, error) {
	if c.tier || c.priceGiven || c.pending == nil || c.pending.PriceID == c.price.ID {
		return c.price, c.product, subscriptions.CloneQuantity(c.quantity), nil
	}
	price, err := s.PriceService.GetByID(ctx, c.pending.PriceID)
	if err != nil {
		return nil, nil, nil, err
	}
	product, err := s.ProductService.GetByID(ctx, price.ProductID)
	if err != nil {
		return nil, nil, nil, err
	}
	quantity, err := changeQuantity(&SubscriptionChangeRequest{Quantity: c.requested}, c.sub, price)
	return price, product, quantity, err
}

// previewEngineSchedule quotes what scheduleEngineChange records.
func (s *CheckoutService) previewEngineSchedule(ctx context.Context, resp *TierChangePreviewResponse, c *changeTarget) (*TierChangePreviewResponse, error) {
	if err := s.engineScheduleAdmissible(c); err != nil {
		return nil, err
	}
	next, product, quantity, err := s.renewalTarget(ctx, c)
	if err != nil {
		return nil, err
	}
	amount, err := seatPrice(next, quantity)
	if err != nil {
		return nil, err
	}
	end := *c.sub.CurrentPeriodEndsAt
	resp.PriceID, resp.Quantity, resp.Effective, resp.NextChargeAmount, resp.NextChargeDate = billing.PriceID(next.ID), quantity, "period_end", amount, &end
	resp.Message = fmt.Sprintf("No charge now. From %s: %s%s at %s.", end.UTC().Format("January 2, 2006"), product.DisplayName, seatsText(quantity), formatMinorAmount(amount, next.Currency))
	return resp, nil
}

// previewEngineCancel quotes a change back to what the subscription bills.
func previewEngineCancel(resp *TierChangePreviewResponse, c *changeTarget) (*TierChangePreviewResponse, error) {
	amount, err := seatPrice(c.currentPrice, c.sub.Quantity)
	if err != nil {
		return nil, err
	}
	resp.Effective, resp.NextChargeAmount, resp.NextChargeDate = "now", amount, c.sub.CurrentPeriodEndsAt
	resp.Message = "No charge. The scheduled change is canceled."
	return resp, nil
}

// previewEngineUpgrade quotes exactly what processEngineUpgrade charges.
func (s *CheckoutService) previewEngineUpgrade(resp *TierChangePreviewResponse, c *changeTarget) (*TierChangePreviewResponse, error) {
	terms, err := engineUpgradeQuote(c.sub, c.currentPrice, c.price, c.product, c.quantity, s.now().UTC().Truncate(time.Microsecond))
	if err != nil {
		return nil, err
	}
	end := terms.PeriodEnd
	resp.Effective, resp.AmountDueNow, resp.NextChargeAmount, resp.NextChargeDate = "now", terms.Amount, terms.RecurringAmount, &end
	resp.Message = fmt.Sprintf("You'll be charged %s now and %s on %s.", formatMinorAmount(terms.Amount, terms.Currency), formatMinorAmount(terms.RecurringAmount, terms.Currency), end.UTC().Format("January 2, 2006"))
	return resp, nil
}

// previewEngineSeats quotes exactly what addEngineSeats charges.
func (s *CheckoutService) previewEngineSeats(resp *TierChangePreviewResponse, c *changeTarget) (*TierChangePreviewResponse, error) {
	terms, err := seatIncreaseQuote(c.sub, c.currentPrice, c.product, c.quantity, s.now().UTC().Truncate(time.Microsecond))
	if err != nil {
		return nil, err
	}
	end := terms.PeriodEnd
	resp.Effective, resp.AmountDueNow, resp.NextChargeAmount, resp.NextChargeDate = "now", terms.Amount, terms.RecurringAmount, &end
	resp.Message = fmt.Sprintf("You'll be charged %s now for %d more seats and %s on %s.", formatMinorAmount(terms.Amount, terms.Currency), *c.quantity-*c.sub.Quantity, formatMinorAmount(terms.RecurringAmount, terms.Currency), end.UTC().Format("January 2, 2006"))
	return resp, nil
}

// seatsText names a quantity's seats in copy: "" for a price without seats.
func seatsText(q *int) string {
	if q == nil {
		return ""
	}
	return fmt.Sprintf(" for %d seats", *q)
}

// engineChangeResponse renders an engine change charged now: the changed
// membership while unresolved; once paid, the successor of an upgrade or the
// same membership with its new seats.
func engineChangeResponse(in gen.BillingProviderIntent) (*TierChangeResponse, error) {
	p, err := subscriptions.DecodeInitialMembershipPayload(in)
	if err != nil {
		return nil, err
	}
	changed, ok := p.Terms.Changed()
	if !ok {
		return nil, fmt.Errorf("intent %s is not a subscription change", in.ID)
	}
	subID := billing.SubscriptionID(changed)
	end := p.Terms.PeriodEnd
	resp := &TierChangeResponse{
		Effective: "now", PriceID: billing.PriceID(p.Terms.PriceID), Quantity: subscriptions.CloneQuantity(p.Terms.Quantity),
		Rail: in.Rail, SubscriptionID: &subID,
		Currency: p.Terms.Currency, AmountDueNow: p.Terms.Amount, NextChargeAmount: p.Terms.RecurringAmount, NextChargeDate: &end,
		OperationID: billing.PaymentOperationID(in.ID),
	}
	switch in.Status {
	case intents.StatusSucceeded:
		if err := intents.ValidateInitialMembershipTerminal(in); err != nil {
			return nil, err
		}
		successor := billing.SubscriptionID(p.Terms.SubscriptionID)
		resp.Status, resp.SubscriptionID = "succeeded", &successor
		if tx := intents.EvidenceString(in, "transaction_id"); tx != "" {
			resp.TransactionID = &tx
		}
		resp.Message = intents.EvidenceString(in, "message")
		return resp, nil
	case intents.StatusFailedTerminal, intents.StatusExpired, intents.StatusSuperseded:
		var evidence struct {
			Declined bool `json:"declined"`
		}
		_ = json.Unmarshal(in.ResultEvidence, &evidence)
		if evidence.Declined {
			return nil, &TierChangeDeclinedError{Reason: operationReason(in), Message: operationFailure(in).Message}
		}
		return nil, tierChangeRefused(in, "", 0, "")
	default:
		if authenticationRequired(in) {
			resp.Status = "requires_action"
			resp.NextAction = &billing.NextAction{Type: "payment_authentication"}
			resp.Message = "The card issuer requires authentication; authenticate operation " + in.ID.String() + " to complete the change"
			return resp, nil
		}
		return tierChangeProcessing(resp)
	}
}
