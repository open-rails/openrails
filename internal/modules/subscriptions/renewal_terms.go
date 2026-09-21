package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// RenewalTerms is the accepted local effect of one recurring charge. It is
// internal persisted intent data, not a caller-supplied catalog override.
// Preparation reads catalog and scheduled changes before admission; settlement
// consumes these facts without reinterpreting a subsequently edited catalog.
type RenewalTerms struct {
	PSPID                uuid.UUID       `json:"psp_id"`
	SubscriptionID       uuid.UUID       `json:"subscription_id"`
	CustomerID           uuid.UUID       `json:"customer_id"`
	FromPriceID          uuid.UUID       `json:"from_price_id"`
	FromProductID        uuid.UUID       `json:"from_product_id"`
	PriceID              uuid.UUID       `json:"price_id"`
	ProductID            uuid.UUID       `json:"product_id"`
	ProductName          string          `json:"product_name"`
	Amount               int64           `json:"amount,string"`
	Currency             string          `json:"currency"`
	PeriodStart          time.Time       `json:"period_start"`
	PeriodEnd            time.Time       `json:"period_end"`
	Entitlements         map[string]*int `json:"entitlements"`
	PreviousEntitlements map[string]*int `json:"previous_entitlements"`
	RepriceID            *uuid.UUID      `json:"reprice_id,omitempty"`
	ScheduledPriceID     *uuid.UUID      `json:"scheduled_price_id,omitempty"`
}

func (t RenewalTerms) Validate() error {
	if t.PSPID == uuid.Nil || t.SubscriptionID == uuid.Nil || t.CustomerID == uuid.Nil || t.FromPriceID == uuid.Nil || t.FromProductID == uuid.Nil || t.PriceID == uuid.Nil || t.ProductID == uuid.Nil || t.Amount <= 0 || t.PeriodStart.IsZero() || !t.PeriodEnd.After(t.PeriodStart) {
		return errors.New("renewal terms are incomplete")
	}
	if t.Currency != strings.ToUpper(strings.TrimSpace(t.Currency)) {
		return errors.New("renewal currency must be canonical")
	}
	if _, err := moneyutil.NativeToRailMinorExact(t.Currency, t.Amount); err != nil {
		return fmt.Errorf("renewal amount: %w", err)
	}
	if (t.RepriceID != nil && *t.RepriceID == uuid.Nil) || (t.ScheduledPriceID != nil && *t.ScheduledPriceID == uuid.Nil) || (t.RepriceID != nil && t.ScheduledPriceID != nil) {
		return errors.New("renewal has inconsistent scheduled changes")
	}
	return nil
}

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
		PriceID: sub.PriceID, ProductID: sub.ProductID, PeriodStart: sub.CurrentPeriodEndsAt.UTC(),
		Entitlements:         models.CloneEntitlementsSpec(sub.EntitlementsSpecSnapshot),
		PreviousEntitlements: models.CloneEntitlementsSpec(sub.EntitlementsSpecSnapshot),
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
			return terms, errors.New("engine paid agreement no longer matches the current obligation")
		}
		terms.Amount, terms.Currency, terms.ProductName = agreement.Amount, agreement.Currency, agreement.ProductName
		terms.PeriodEnd = terms.PeriodStart.Add(duration)
		terms.Entitlements = models.CloneEntitlementsSpec(agreement.Entitlements)
		terms.PreviousEntitlements = models.CloneEntitlementsSpec(agreement.Entitlements)
	} else if agreement != nil {
		return terms, errors.New("native renewal cannot substitute an engine agreement")
	}

	reprice, err := NewRepriceRepo(d).GetScheduledForSubscription(ctx, sub.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return terms, fmt.Errorf("prepare renewal scheduled change: %w", err)
	}
	if err == nil && reprice.IsDue(now) {
		if reprice.FromPriceID != sub.PriceID || sub.ScheduledPriceID != nil {
			return terms, errors.New("renewal scheduled changes contradict the current subscription")
		}
		terms.RepriceID = &reprice.ID
		terms.PriceID = reprice.ToPriceID
	} else if sub.ScheduledPriceID != nil {
		id := *sub.ScheduledPriceID
		terms.ScheduledPriceID = &id
		terms.PriceID = id
	}
	if sub.CollectionPolicy == models.CollectionPolicyEngine && terms.RepriceID == nil && terms.ScheduledPriceID == nil {
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
	terms.ProductID, terms.Amount, terms.Currency = price.ProductID, price.Amount, price.Currency
	terms.PeriodEnd = terms.PeriodStart.Add(time.Duration(*cycle) * time.Hour)
	product, err := catalog.NewProductService(d).GetByID(ctx, terms.ProductID)
	if err != nil {
		return terms, fmt.Errorf("prepare renewal product: %w", err)
	}
	terms.ProductName = product.DisplayName
	if terms.ProductID != sub.ProductID || terms.ScheduledPriceID != nil {
		terms.Entitlements = models.CloneEntitlementsSpec(product.EntitlementsSpec)
	}
	// Empty is a valid accepted benefit snapshot. Do not turn it into the
	// product's current benefits during settlement.
	return terms, terms.Validate()
}

func applyRenewalTerms(ctx context.Context, d *db.DB, sub *models.Subscription, terms RenewalTerms, previousPeriodEnd *time.Time) (bool, error) {
	if err := terms.Validate(); err != nil {
		return false, err
	}
	if sub.ID != terms.SubscriptionID || sub.CustomerID != terms.CustomerID || sub.PspID != terms.PSPID || sub.CurrentPeriodEndsAt == nil {
		return false, errors.New("subscription no longer matches the accepted renewal")
	}
	previous := terms.PeriodStart
	if previousPeriodEnd != nil {
		if sub.CollectionPolicy != models.CollectionPolicyEngine || previousPeriodEnd.IsZero() || previousPeriodEnd.After(terms.PeriodStart) {
			return false, errors.New("accepted previous boundary is not an engine renewal")
		}
		previous = previousPeriodEnd.UTC()
	} else if sub.CollectionPolicy == models.CollectionPolicyEngine {
		return false, errors.New("engine renewal requires its accepted previous boundary")
	}
	alreadyAdvanced := sub.CurrentPeriodEndsAt.Equal(terms.PeriodEnd) && sub.PriceID == terms.PriceID && sub.ProductID == terms.ProductID
	if !alreadyAdvanced && (!sub.CurrentPeriodEndsAt.Equal(previous) || sub.PriceID != terms.FromPriceID || sub.ProductID != terms.FromProductID) {
		return false, errors.New("subscription period no longer matches the accepted renewal")
	}

	if terms.RepriceID != nil {
		repo := NewRepriceRepo(d)
		change, err := repo.GetByID(ctx, *terms.RepriceID)
		if err != nil {
			return false, fmt.Errorf("load accepted renewal change: %w", err)
		}
		if change.SubscriptionID != sub.ID || change.FromPriceID != terms.FromPriceID || change.ToPriceID != terms.PriceID || (change.Status != models.RepriceStatusScheduled && !(alreadyAdvanced && change.Status == models.RepriceStatusApplied)) {
			return false, errors.New("scheduled change no longer matches the accepted renewal")
		}
		if change.Status == models.RepriceStatusScheduled {
			if err := repo.Apply(ctx, change.ID); err != nil {
				return false, fmt.Errorf("apply accepted renewal change: %w", err)
			}
		}
	}
	if terms.ScheduledPriceID != nil && !alreadyAdvanced {
		if sub.ScheduledPriceID == nil || *sub.ScheduledPriceID != *terms.ScheduledPriceID {
			return false, errors.New("scheduled price no longer matches the accepted renewal")
		}
		sub.ScheduledPriceID = nil
	}
	sub.PriceID, sub.ProductID = terms.PriceID, terms.ProductID
	sub.EntitlementsSpecSnapshot = models.CloneEntitlementsSpec(terms.Entitlements)
	return alreadyAdvanced, nil
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
		if params.PreviousPeriodEnd == nil || params.PaymentCustodian != models.CustodianHyperSwitch || sub.RailSubscriptionID != "" {
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
