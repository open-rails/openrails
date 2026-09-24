package subscriptions

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// InitialMembershipTerms is the accepted local membership effect, persisted
// before provider submission. A schedule-only pending phase confers no new
// access; a free phase confers its declared access without recording money.
type InitialMembershipTerms struct {
	CollectionPolicy models.CollectionPolicy `json:"collection_policy"`
	SubscriptionID   uuid.UUID               `json:"subscription_id"`
	PaymentID        uuid.UUID               `json:"payment_id"`
	CustomerID       uuid.UUID               `json:"customer_id"`
	PSPID            uuid.UUID               `json:"psp_id"`
	ProductID        uuid.UUID               `json:"product_id"`
	PriceID          uuid.UUID               `json:"price_id"`
	PaymentMethodID  uuid.UUID               `json:"payment_method_id"`
	ProductName      string                  `json:"product_name"`
	Amount           int64                   `json:"amount,string"`
	RecurringAmount  int64                   `json:"recurring_amount,string"`
	Currency         string                  `json:"currency"`
	AcceptedAt       time.Time               `json:"accepted_at"`
	PeriodStart      time.Time               `json:"period_start"`
	PeriodEnd        time.Time               `json:"period_end"`
	Pending          bool                    `json:"pending"`
	Entitlements     map[string]*int         `json:"entitlements"`
	// Replaces is set on an engine tier upgrade: accepting this membership
	// supersedes that one. Amount is then the prorated charge and
	// RecurringAmount the new price every renewal bills.
	Replaces *ReplacedMembership `json:"replaces,omitempty"`
}

// ReplacedMembership freezes the engine membership an upgrade supersedes as
// the customer saw it: completion refuses if it moved since.
type ReplacedMembership struct {
	SubscriptionID uuid.UUID `json:"subscription_id"`
	PriceID        uuid.UUID `json:"price_id"`
	PeriodEnd      time.Time `json:"period_end"`
	Credit         int64     `json:"credit,string"`
}

func (t InitialMembershipTerms) Validate() error {
	if !t.CollectionPolicy.Valid() || t.SubscriptionID == uuid.Nil || t.CustomerID == uuid.Nil || t.PSPID == uuid.Nil || t.ProductID == uuid.Nil || t.PriceID == uuid.Nil || t.PaymentMethodID == uuid.Nil || t.AcceptedAt.IsZero() || t.PeriodStart.IsZero() || !t.PeriodEnd.After(t.PeriodStart) || t.Amount < 0 || t.RecurringAmount < 0 || t.Entitlements == nil {
		return errors.New("initial membership terms are incomplete")
	}
	for _, instant := range []time.Time{t.AcceptedAt, t.PeriodStart, t.PeriodEnd} {
		if !instant.Equal(instant.Truncate(time.Microsecond)) {
			return errors.New("initial membership instants must have PostgreSQL microsecond precision")
		}
	}
	if t.Currency != strings.ToUpper(strings.TrimSpace(t.Currency)) || (t.Amount > 0) != (t.PaymentID != uuid.Nil) || (t.Pending && (t.Amount != 0 || !t.PeriodStart.After(t.AcceptedAt))) {
		return errors.New("initial membership phase contradicts its accepted payment")
	}
	if _, err := moneyutil.NativeToRailMinorExact(t.Currency, t.Amount); err != nil {
		return err
	}
	if r := t.Replaces; r != nil {
		if t.CollectionPolicy != models.CollectionPolicyEngine || t.Pending || r.SubscriptionID == uuid.Nil || r.SubscriptionID == t.SubscriptionID || r.PriceID == uuid.Nil || r.PriceID == t.PriceID || !r.PeriodEnd.After(t.AcceptedAt) || !r.PeriodEnd.Equal(r.PeriodEnd.Truncate(time.Microsecond)) || t.Amount <= 0 || r.Credit < 0 || t.Amount+r.Credit != t.RecurringAmount {
			return errors.New("engine upgrade terms contradict the membership they replace")
		}
	}
	_, err := moneyutil.NativeToRailMinorExact(t.Currency, t.RecurringAmount)
	return err
}
