package checkout

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// A provider-billed (NMI-owned) subscription changes tier in place: NMI's
// existing schedule keeps its next billing date E and only its amount moves,
// so NMI never runs a second schedule. An upgrade (effective now) charges the
// new price's share of the time left to E less the old price's unused credit,
// then sets the schedule amount to the new price and switches access now. A
// downgrade (effective at E) sets the schedule amount now — NMI applies it to
// future charges only — and schedules the local price; the mirrored renewal at
// E opens the new tier. Both prices must share the period's cadence.

var errTierChangeLinkedPlan = &TierChangeError{HTTPStatus: http.StatusConflict, Code: openrails.CodeTierChangeRequiresLinkedPlan,
	Message: "this subscription is on a named NMI plan, which changes only by switching plans; link the new price to an NMI plan of the same amount and billing cycle"}

var errTierChangeScheduleUnavailable = &TierChangeError{HTTPStatus: http.StatusConflict, Code: openrails.CodeTierChangeRefused,
	Message: "the provider's billing schedule for this subscription could not be read; try again later"}

var errTierChangeCadence = &TierChangeError{HTTPStatus: http.StatusConflict, Code: openrails.CodeTierChangeCadenceUnsupported,
	Message: "this subscription is billed on the provider's schedule, which keeps its billing date; change to a price of the same billing cycle"}

// providerNMITierAdmissible refuses what an in-place change cannot honour.
func providerNMITierAdmissible(sub *models.Subscription, current, target *models.Price, now time.Time) error {
	if sub.Status != models.StatusActive || sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodStartsAt == nil || sub.RailSubscriptionID == "" || !sub.CurrentPeriodEndsAt.After(now) {
		return errTierChangeRenewalDue
	}
	oldCycle, newCycle := current.RecurringCycleHours(), target.RecurringCycleHours()
	if newCycle == nil || *newCycle <= 0 {
		return ErrTierChangeCycleUnknown
	}
	if oldCycle == nil || *oldCycle != *newCycle {
		return errTierChangeCadence
	}
	if sub.ScheduledPriceID != nil {
		return errTierChangeScheduled
	}
	if sub.PaymentMethodID == nil {
		return ErrPaymentMethodStale
	}
	return nil
}

// scheduleTargetPlan reads the subscription's NMI schedule and decides how it
// moves to target: "" sets the amount of a custom schedule; a plan id switches
// a named-plan schedule to the target price's linked plan, which must exist at
// NMI with the target's exact amount and cycle. Decided before any charge.
func (s *CheckoutService) scheduleTargetPlan(ctx context.Context, sub *models.Subscription, target *models.Price) (string, error) {
	rt, err := s.resolveRailTargetForPSP(ctx, string(sub.Rail), sub.PspID)
	if err != nil {
		return "", err
	}
	client, err := s.resolveNMIClient(db.WithPSPID(ctx, sub.PspID), nmiIntentClientName(rt.PSP, string(sub.Rail)))
	if err != nil || client == nil {
		return "", errTierChangeScheduleUnavailable
	}
	schedule, found, err := client.GetSubscription(ctx, sub.RailSubscriptionID)
	if err != nil || !found {
		return "", errTierChangeScheduleUnavailable
	}
	if !schedule.NamedPlan() {
		return "", nil
	}
	return linkedTargetPlan(ctx, client, target, checkoutPSPLinkForTarget(target, rt))
}

// linkedTargetPlan is the target price's NMI plan on the account, verified at
// NMI: same amount, whole-day cycle equal to the price's, unlimited payments.
func linkedTargetPlan(ctx context.Context, client *nmi.NMIClient, target *models.Price, link map[string]string) (string, error) {
	planID := strings.TrimSpace(link["plan_id"])
	cycle := target.RecurringCycleHours()
	if planID == "" || cycle == nil || *cycle <= 0 {
		return "", errTierChangeLinkedPlan
	}
	cents, err := moneyutil.NativeToRailMinorExact(target.Currency, target.Amount)
	if err != nil {
		return "", err
	}
	plan, err := client.GetRecurringPlanDetailByID(ctx, planID, target.Currency)
	if err != nil || !plan.Found || plan.AmountCents != int64(cents) || plan.DayFrequency*24 != *cycle || (plan.Payments != nil && *plan.Payments != 0) {
		return "", errTierChangeLinkedPlan
	}
	return planID, nil
}

func (s *CheckoutService) previewProviderNMITierChange(ctx context.Context, resp *TierChangePreviewResponse, sub *models.Subscription, current, target *models.Price, product *models.Product, downgrade bool) (*TierChangePreviewResponse, error) {
	now := s.now().UTC()
	if err := providerNMITierAdmissible(sub, current, target, now); err != nil {
		return nil, err
	}
	if _, err := s.scheduleTargetPlan(ctx, sub, target); err != nil {
		return nil, err
	}
	end := sub.CurrentPeriodEndsAt.UTC()
	resp.NextChargeDate = &end
	if downgrade {
		resp.Action, resp.Effective = "downgrade", "period_end"
		resp.Message = fmt.Sprintf("No charge now. Your plan changes to %s on %s, then renews at %s.", product.DisplayName, end.UTC().Format("January 2, 2006"), formatMinorAmount(target.Amount, target.Currency))
		return resp, nil
	}
	quote, err := QuoteKeepBoundaryUpgrade(modelBUpgradeOf(sub, current, target), now)
	if err != nil {
		return nil, err
	}
	resp.Action, resp.Effective, resp.AmountDueNow = "upgrade", "now", quote.ChargeNow
	resp.Message = fmt.Sprintf("You'll be charged %s now and %s on %s.", formatMinorAmount(quote.ChargeNow, target.Currency), formatMinorAmount(target.Amount, target.Currency), end.UTC().Format("January 2, 2006"))
	return resp, nil
}

// processProviderNMITierChange freezes the change and runs it as one durable
// operation (nmi_upgrade_intent.go). The same Idempotency-Key replays it.
func (s *CheckoutService) processProviderNMITierChange(ctx context.Context, req *TierChangeRequest, user *UserIdentity, newPrice *models.Price, newProduct *models.Product, sub *models.Subscription, action string) (*TierChangeResponse, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if s.Intents == nil || s.Lifecycle == nil || s.SubscriptionService == nil {
		return nil, errors.New("durable tier change service unavailable")
	}
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return nil, tierChangeKeyRequired()
	}
	target, err := s.resolveRailTargetForPSP(ctx, string(sub.Rail), sub.PspID)
	if err != nil {
		return nil, err
	}
	ctx = db.WithPSPID(ctx, sub.PspID)
	key := tierChangeIdempotencyKey(tierChangeCustomer(user), req.IdempotencyKey)
	database := s.SubscriptionService.Database()
	if prior, err := intents.NewStore(database).GetByIdempotencyKey(ctx, key); err == nil {
		return s.replayTierChangeOperation(ctx, prior, &TierChangeRequest{SubscriptionID: sub.ID, PriceID: openrails.PriceID(newPrice.ID).String()}, user)
	} else if !db.IsNotFound(err) {
		return nil, err
	}
	current := sub.Price
	if current == nil {
		if current, err = s.PriceService.GetByID(ctx, sub.PriceID); err != nil {
			return nil, err
		}
	}
	customerID, err := customerIDFromUser(user.ID)
	if err != nil {
		return nil, err
	}
	if sub.CustomerID != customerID {
		return nil, errors.New("tier change subscription belongs to another customer")
	}
	now := s.now().UTC()
	downgrade := action == "downgrade"
	if err := providerNMITierAdmissible(sub, current, newPrice, now); err != nil {
		return nil, err
	}
	targetPlan, err := s.scheduleTargetPlan(ctx, sub, newPrice)
	if err != nil {
		return nil, err
	}
	if !downgrade {
		if _, err := subscriptions.NewRepriceRepo(database).GetScheduledForSubscription(ctx, sub.ID); err == nil {
			return nil, errTierChangeScheduled
		}
	}
	amount := int64(0)
	if !downgrade {
		quote, err := QuoteKeepBoundaryUpgrade(modelBUpgradeOf(sub, current, newPrice), now)
		if err != nil {
			return nil, err
		}
		amount = quote.ChargeNow
	}
	if _, err = moneyutil.NativeToRailMinorExact(newPrice.Currency, amount); err != nil {
		return nil, err
	}
	if _, err = moneyutil.NativeToRailMinorExact(newPrice.Currency, newPrice.Amount); err != nil {
		return nil, err
	}
	methodRow, err := database.Gen(ctx).GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: mid.UUID(), ID: *sub.PaymentMethodID})
	if err != nil {
		return nil, err
	}
	if methodRow.CustomerID != customerID || methodRow.PspID != sub.PspID || methodRow.Custodian != models.CustodianPSP || methodRow.RailCustomerRef == "" || methodRow.ParkReason != "" {
		return nil, ErrPaymentMethodStale
	}
	email := ""
	if user.Email != nil {
		email = strings.TrimSpace(*user.Email)
	}
	payload := subscriptions.NMIUpgradePayload{Action: action, RequestedPrice: strings.TrimSpace(req.PriceID), PSP: target.PSP, UserID: user.ID, Email: email,
		OldSubscriptionID: sub.ID, OldPriceID: sub.PriceID, OldProviderSubscriptionID: sub.RailSubscriptionID, NewPaymentID: uuidutil.NewV7(),
		PriceID: newPrice.ID, ProductID: newProduct.ID, ProductName: newProduct.DisplayName, Instrument: charge.FreezeInstrument(methodRow), PaymentMethodID: methodRow.ID,
		RecurringAmount: newPrice.Amount, ProrationAmount: amount, Currency: newPrice.Currency, PeriodStart: now, PeriodEnd: sub.CurrentPeriodEndsAt.UTC(),
		Entitlements: models.CloneEntitlementsSpec(newProduct.EntitlementsSpec), TargetPlanID: targetPlan}
	reason := "customer tier upgrade"
	if downgrade {
		reason = "customer tier downgrade"
	}
	intent, err := s.Intents.EnqueueOwnedAndExecute(ctx, intents.EnqueueParams{MerchantID: sub.MerchantID, Provider: string(sub.Rail), PspID: sub.PspID, IntentType: TypeNMIUpgrade, SubscriptionID: &sub.ID, PriceID: &newPrice.ID, Payload: payload, IdempotencyKey: key, NextAttemptAt: now, Origin: intents.OriginUser, OriginReason: reason},
		func(row gen.OpenrailsRailIntent) error { return tierChangeOwnedBy(row, nmiUpgradeSubject(payload)) })
	var conflict *pgconn.PgError
	if errors.As(err, &conflict) && conflict.Code == "23505" && conflict.ConstraintName == tierChangeSubjectConstraint {
		return nil, s.tierChangeInFlight(ctx, sub.ID)
	}
	if err != nil {
		return nil, err
	}
	return tierChangeResponse(intent)
}
