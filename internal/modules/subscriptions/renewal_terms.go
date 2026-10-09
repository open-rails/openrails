package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// RenewalTerms is the accepted local effect of one recurring charge. It is
// internal persisted intent data, not a caller-supplied catalog override.
// Preparation reads catalog and scheduled changes before admission; settlement
// consumes these facts without reinterpreting a subsequently edited catalog.
type RenewalTerms struct {
	LegacyEntitlements  map[string]*int `json:"legacy_entitlements,omitzero"`
	AccessDurationHours *int            `json:"access_duration_hours"`
	PSPID               uuid.UUID       `json:"psp_id"`
	SubscriptionID      uuid.UUID       `json:"subscription_id"`
	CustomerID          uuid.UUID       `json:"customer_id"`
	FromPriceID         uuid.UUID       `json:"from_price_id"`
	FromProductID       uuid.UUID       `json:"from_product_id"`
	PriceID             uuid.UUID       `json:"price_id"`
	ProductID           uuid.UUID       `json:"product_id"`
	ProductName         string          `json:"product_name"`
	// Quantity is the seats of a per-seat price, nil otherwise; Amount is the
	// unit price times Quantity.
	Quantity    *int      `json:"quantity,omitempty"`
	Amount      int64     `json:"amount,string"`
	Currency    string    `json:"currency"`
	PeriodStart time.Time `json:"period_start"`
	PeriodEnd   time.Time `json:"period_end"`
	// Entitlements is the keys a renewal admitted before product access, kept
	// only to reproduce its provider binding. Access follows ProductID.
	Entitlements json.RawMessage `json:"entitlements,omitempty"`
	// ScheduledChangeID is the scheduled change this renewal applies.
	ScheduledChangeID *uuid.UUID `json:"scheduled_change_id,omitempty"`
}

// UnitAmount is the price of one seat.
func (t RenewalTerms) UnitAmount() int64 { return t.Amount / int64(SeatCount(t.Quantity)) }

func (t RenewalTerms) Validate() error {
	if err := validateAccessDuration(t.AccessDurationHours); err != nil {
		return err
	}
	if t.PSPID == uuid.Nil || t.SubscriptionID == uuid.Nil || t.CustomerID == uuid.Nil || t.FromPriceID == uuid.Nil || t.FromProductID == uuid.Nil || t.PriceID == uuid.Nil || t.ProductID == uuid.Nil || t.Amount <= 0 || t.PeriodStart.IsZero() || !t.PeriodEnd.After(t.PeriodStart) {
		return errors.New("renewal terms are incomplete")
	}
	if t.Currency != strings.ToUpper(strings.TrimSpace(t.Currency)) {
		return errors.New("renewal currency must be canonical")
	}
	if t.Quantity != nil && *t.Quantity < 1 || t.Amount%int64(SeatCount(t.Quantity)) != 0 {
		return errors.New("renewal seats contradict its amount")
	}
	if _, err := moneyutil.NativeToRailMinorExact(t.Currency, t.Amount); err != nil {
		return fmt.Errorf("renewal amount: %w", err)
	}
	if t.ScheduledChangeID != nil && *t.ScheduledChangeID == uuid.Nil {
		return errors.New("renewal names an empty scheduled change")
	}
	return nil
}

// ErrEngineAgreementMismatch means retained paid terms do not own the current
// obligation. It is distinct from storage failures while reading those terms.
var ErrEngineAgreementMismatch = errors.New("engine paid agreement no longer matches the current obligation")

// PrepareRenewalTerms must run under the admission transaction's subscription
// lock. It does not mutate the subscription or mark a scheduled change applied:
// those effects belong to settlement of the accepted charge.
func PrepareRenewalTerms(ctx context.Context, d *db.DB, sub *models.Subscription, now time.Time, agreement *RenewalTerms) (RenewalTerms, error) {
	var terms RenewalTerms
	if d == nil || d.Pool() != nil || sub == nil || sub.CurrentPeriodEndsAt == nil {
		return terms, errors.New("renewal preparation requires a locked subscription and transaction")
	}
	terms = RenewalTerms{
		PSPID: sub.PspID, SubscriptionID: sub.ID, CustomerID: sub.CustomerID, FromPriceID: sub.PriceID, FromProductID: sub.ProductID,
		PriceID: sub.PriceID, ProductID: sub.ProductID, PeriodStart: sub.CurrentPeriodEndsAt.UTC(), AccessDurationHours: sub.AccessDurationHoursSnapshot,
		Quantity: CloneQuantity(sub.Quantity),
	}
	if sub.CollectionPolicy == models.CollectionPolicyEngine {
		if agreement == nil {
			return terms, errors.New("engine renewal requires its qualified paid agreement")
		}
		if err := agreement.Validate(); err != nil {
			return terms, err
		}
		duration := agreement.PeriodEnd.Sub(agreement.PeriodStart)
		if agreement.SubscriptionID != sub.ID || agreement.CustomerID != sub.CustomerID || agreement.PSPID != sub.PspID || agreement.PriceID != sub.PriceID || agreement.ProductID != sub.ProductID || !agreement.PeriodEnd.Equal(*sub.CurrentPeriodEndsAt) || sub.CurrentPeriodStartsAt == nil || !agreement.PeriodStart.Equal(*sub.CurrentPeriodStartsAt) || !agreement.PeriodStart.Add(duration).Equal(agreement.PeriodEnd) {
			return terms, ErrEngineAgreementMismatch
		}
		// The agreement fixes the unit price; seats are the subscription's.
		amount, err := SeatAmount(agreement.UnitAmount(), terms.Quantity)
		if err != nil {
			return terms, err
		}
		terms.Amount, terms.Currency, terms.ProductName = amount, agreement.Currency, agreement.ProductName
		terms.AccessDurationHours = agreement.AccessDurationHours
		terms.PeriodEnd = terms.PeriodStart.Add(duration)
	} else if agreement != nil {
		return terms, errors.New("native renewal cannot substitute an engine agreement")
	}

	change, err := PendingChange(ctx, d, sub.ID)
	if err != nil {
		return terms, fmt.Errorf("prepare renewal scheduled change: %w", err)
	}
	if change.IsDue(now) {
		if change.FromPriceID != sub.PriceID {
			return terms, errors.New("renewal scheduled change contradicts the current subscription")
		}
		terms.ScheduledChangeID = &change.ID
		terms.PriceID = change.PriceID
		if change.Quantity != nil {
			terms.Quantity = CloneQuantity(change.Quantity)
		}
	}
	if sub.CollectionPolicy == models.CollectionPolicyEngine && terms.ScheduledChangeID == nil {
		return terms, terms.Validate()
	}
	price, err := catalog.NewPriceService(d).GetByID(ctx, terms.PriceID)
	if err != nil {
		return terms, fmt.Errorf("prepare renewal price: %w", err)
	}
	cycle := price.RecurringCycleHours()
	if cycle == nil || *cycle <= 0 || int64(*cycle) > math.MaxInt64/int64(time.Hour) {
		return terms, errors.New("renewal price has no valid recurring cadence")
	}
	// The renewal's price decides whether it has seats: a price newly per seat
	// starts at its minimum, one without seats drops them.
	switch {
	case price.Quantity == nil || !SeatsChangeable(sub):
		terms.Quantity = nil
	case terms.Quantity == nil:
		terms.Quantity = CloneQuantity(&price.Quantity.Min)
	}
	amount, err := SeatAmount(price.Amount, terms.Quantity)
	if err != nil {
		return terms, err
	}
	terms.ProductID, terms.Amount, terms.Currency = price.ProductID, amount, price.Currency
	terms.AccessDurationHours = price.AccessDurationHours
	terms.PeriodEnd = terms.PeriodStart.Add(time.Duration(*cycle) * time.Hour)
	product, err := catalog.NewProductService(d).GetByID(ctx, terms.ProductID)
	if err != nil {
		return terms, fmt.Errorf("prepare renewal product: %w", err)
	}
	terms.ProductName = product.DisplayName
	return terms, terms.Validate()
}

// applyRenewalTerms moves sub to the accepted terms and answers the scheduled
// change they apply, if any.
func applyRenewalTerms(ctx context.Context, d *db.DB, sub *models.Subscription, terms RenewalTerms, previousPeriodEnd *time.Time) (*models.ScheduledChange, bool, error) {
	if err := terms.Validate(); err != nil {
		return nil, false, err
	}
	if sub.ID != terms.SubscriptionID || sub.CustomerID != terms.CustomerID || sub.PspID != terms.PSPID || sub.CurrentPeriodEndsAt == nil {
		return nil, false, errors.New("subscription no longer matches the accepted renewal")
	}
	previous := terms.PeriodStart
	if previousPeriodEnd != nil {
		if sub.CollectionPolicy != models.CollectionPolicyEngine || previousPeriodEnd.IsZero() || previousPeriodEnd.After(terms.PeriodStart) {
			return nil, false, errors.New("accepted previous boundary is not an engine renewal")
		}
		previous = previousPeriodEnd.UTC()
	} else if sub.CollectionPolicy == models.CollectionPolicyEngine {
		return nil, false, errors.New("engine renewal requires its accepted previous boundary")
	}
	alreadyAdvanced := sub.CurrentPeriodEndsAt.Equal(terms.PeriodEnd) && sub.PriceID == terms.PriceID && sub.ProductID == terms.ProductID && SameQuantity(sub.Quantity, terms.Quantity)
	if !alreadyAdvanced && (!sub.CurrentPeriodEndsAt.Equal(previous) || sub.PriceID != terms.FromPriceID || sub.ProductID != terms.FromProductID) {
		return nil, false, errors.New("subscription period no longer matches the accepted renewal")
	}

	var applied *models.ScheduledChange
	if terms.ScheduledChangeID != nil {
		row, err := d.Gen(ctx).GetScheduledChange(ctx, gen.GetScheduledChangeParams{MerchantID: sub.MerchantID, ID: *terms.ScheduledChangeID})
		if err != nil {
			return nil, false, fmt.Errorf("load accepted renewal change: %w", err)
		}
		applied = models.ScheduledChangeFromGen(row)
		change := applied
		if change.SubscriptionID != sub.ID || change.FromPriceID != terms.FromPriceID || change.PriceID != terms.PriceID || (change.Status != models.ScheduledChangeScheduled && !(alreadyAdvanced && change.Status == models.ScheduledChangeApplied)) {
			return nil, false, errors.New("scheduled change no longer matches the accepted renewal")
		}
		if change.Status == models.ScheduledChangeScheduled {
			if err := ApplyChange(ctx, d, change.ID, terms.PeriodStart); err != nil {
				return nil, false, fmt.Errorf("apply accepted renewal change: %w", err)
			}
		}
	}
	sub.PriceID, sub.ProductID, sub.Quantity = terms.PriceID, terms.ProductID, CloneQuantity(terms.Quantity)
	sub.AccessDurationHoursSnapshot = terms.AccessDurationHours
	return applied, alreadyAdvanced, nil
}

// Prepared renewals name the local obligation. Native calls must still match
// their immutable remote binding; an engine call must carry its prior boundary.
func lockedRenewalSubscription(ctx context.Context, d *db.DB, params *RenewMembershipParams) (*models.Subscription, error) {
	repo := NewSubscriptionRepo(d)
	if params.Prepared == nil {
		if params.PreviousPeriodEnd != nil {
			return nil, errors.New("previous boundary requires accepted renewal terms")
		}
		return repo.GetByPSPSubscriptionIDForUpdate(ctx, string(params.Rail), params.RailSubscriptionID)
	}
	sub, err := repo.GetByIDForUpdate(ctx, params.Prepared.SubscriptionID)
	if err != nil {
		return nil, err
	}
	if sub.Rail != params.Rail || sub.PspID != params.Prepared.PSPID || sub.RailSubscriptionID != params.RailSubscriptionID {
		return nil, errors.New("accepted renewal provider binding changed")
	}
	if sub.CollectionPolicy == models.CollectionPolicyEngine {
		if params.PreviousPeriodEnd == nil || (params.PaymentCustodian != models.CustodianHyperSwitch && params.PaymentCustodian != models.CustodianPSP) || sub.RailSubscriptionID != "" {
			return nil, errors.New("engine renewal lacks accepted boundary or custody")
		}
	} else if params.PreviousPeriodEnd != nil || params.PaymentCustodian != "" {
		return nil, errors.New("native renewal cannot change its accepted boundary or custody")
	}
	return sub, nil
}

func renewalPaymentCustodian(params *RenewMembershipParams) string {
	if params.PaymentCustodian != "" {
		return params.PaymentCustodian
	}
	return models.CustodianPSP
}

// UnmarshalJSON preserves already accepted operations from before access and billing were separated.
// Only an absent access field uses that operation's original billing period; explicit null is indefinite.
func (t *RenewalTerms) UnmarshalJSON(data []byte) error {
	type plain RenewalTerms
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if _, _, err := grants.DecodeAcceptedEntitlements(decoded.Entitlements, decoded.LegacyEntitlements); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if _, present := fields["access_duration_hours"]; !present {
		hours := int(decoded.PeriodEnd.Sub(decoded.PeriodStart) / time.Hour)
		decoded.AccessDurationHours = &hours
	}
	*t = RenewalTerms(decoded)
	return nil
}
