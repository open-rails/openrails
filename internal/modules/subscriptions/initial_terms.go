package subscriptions

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// InitialMembershipTerms is the accepted local membership effect, persisted
// before provider submission. A schedule-only pending phase confers no new
// access; a free phase confers its declared access without recording money.
type InitialMembershipTerms struct {
	LegacyEntitlements  map[string]*int         `json:"legacy_entitlements,omitzero"`
	CancelAfterInitial  bool                    `json:"cancel_after_initial,omitempty"`
	AccessDurationHours *int                    `json:"access_duration_hours"`
	CollectionPolicy    models.CollectionPolicy `json:"collection_policy"`
	SubscriptionID      uuid.UUID               `json:"subscription_id"`
	PaymentID           uuid.UUID               `json:"payment_id"`
	CustomerID          uuid.UUID               `json:"customer_id"`
	PSPID               uuid.UUID               `json:"psp_id"`
	ProductID           uuid.UUID               `json:"product_id"`
	PriceID             uuid.UUID               `json:"price_id"`
	PaymentMethodID     uuid.UUID               `json:"payment_method_id"`
	ProductName         string                  `json:"product_name"`
	// Quantity is the seats of a per-seat price, nil otherwise;
	// RecurringAmount is the unit price times Quantity.
	Quantity        *int      `json:"quantity,omitempty"`
	Amount          int64     `json:"amount,string"`
	RecurringAmount int64     `json:"recurring_amount,string"`
	Currency        string    `json:"currency"`
	AcceptedAt      time.Time `json:"accepted_at"`
	PeriodStart     time.Time `json:"period_start"`
	PeriodEnd       time.Time `json:"period_end"`
	Pending         bool      `json:"pending"`
	// Entitlements is the keys an enrollment admitted before product access,
	// kept only to reproduce its quote fingerprint. Access follows ProductID.
	Entitlements json.RawMessage `json:"entitlements,omitempty"`
	// Replaces is set on an engine tier upgrade: accepting this membership
	// supersedes that one. Amount is then the prorated charge and
	// RecurringAmount the new price every renewal bills.
	Replaces *ReplacedMembership `json:"replaces,omitempty"`
	// Adds is set on a seat increase: SubscriptionID is the membership itself,
	// which keeps its period. Amount is the added seats' prorated charge for
	// [PeriodStart, PeriodEnd) and RecurringAmount every renewal's.
	Adds *AddedSeats `json:"adds,omitempty"`
}

// AddedSeats freezes the membership a seat increase changes as the customer
// saw it: completion refuses if it moved since.
type AddedSeats struct {
	FromQuantity int `json:"from_quantity"`
}

// Changed is the existing membership this operation changes, if any.
func (t InitialMembershipTerms) Changed() (uuid.UUID, bool) {
	switch {
	case t.Replaces != nil:
		return t.Replaces.SubscriptionID, true
	case t.Adds != nil:
		return t.SubscriptionID, true
	}
	return uuid.Nil, false
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
	if err := moneyutil.RequireFiatCurrency(t.Currency); err != nil {
		return err
	}
	if t.CancelAfterInitial && t.CollectionPolicy != models.CollectionPolicyEngine {
		return errors.New("non-renewing order requires engine collection without a provider schedule")
	}
	if err := validateAccessDuration(t.AccessDurationHours); err != nil {
		return err
	}
	if !t.CollectionPolicy.Valid() || t.SubscriptionID == uuid.Nil || t.CustomerID == uuid.Nil || t.PSPID == uuid.Nil || t.ProductID == uuid.Nil || t.PriceID == uuid.Nil || t.PaymentMethodID == uuid.Nil || t.AcceptedAt.IsZero() || t.PeriodStart.IsZero() || !t.PeriodEnd.After(t.PeriodStart) || t.Amount < 0 || t.RecurringAmount < 0 {
		return errors.New("initial membership terms are incomplete")
	}
	for _, instant := range []time.Time{t.AcceptedAt, t.PeriodStart, t.PeriodEnd} {
		if !instant.Equal(instant.Truncate(time.Microsecond)) {
			return errors.New("initial membership instants must have PostgreSQL microsecond precision")
		}
	}
	if t.Quantity != nil && (*t.Quantity < 1 || t.CollectionPolicy != models.CollectionPolicyEngine) || t.RecurringAmount%int64(SeatCount(t.Quantity)) != 0 {
		return errors.New("initial membership quantity contradicts its recurring amount")
	}
	if t.Currency != strings.ToUpper(strings.TrimSpace(t.Currency)) || (t.Amount > 0) != (t.PaymentID != uuid.Nil) || (t.Pending && (t.Amount != 0 || !t.PeriodStart.After(t.AcceptedAt))) {
		return errors.New("initial membership phase contradicts its accepted payment")
	}
	if _, err := moneyutil.NativeToRailMinorExact(t.Currency, t.Amount); err != nil {
		return err
	}
	if a := t.Adds; a != nil {
		if t.Quantity == nil {
			return errors.New("seat increase terms name no seats")
		}
		added, err := Seats(t.UnitAmount(), *t.Quantity-a.FromQuantity)
		if t.Replaces != nil || t.CollectionPolicy != models.CollectionPolicyEngine || t.Pending || t.CancelAfterInitial || a.FromQuantity < 1 || a.FromQuantity >= *t.Quantity || err != nil || t.Amount <= 0 || t.Amount > added {
			return errors.New("seat increase terms contradict the membership they change")
		}
	}
	if r := t.Replaces; r != nil {
		if t.CollectionPolicy != models.CollectionPolicyEngine || t.Pending || r.SubscriptionID == uuid.Nil || r.SubscriptionID == t.SubscriptionID || r.PriceID == uuid.Nil || r.PriceID == t.PriceID || !r.PeriodEnd.After(t.AcceptedAt) || !r.PeriodEnd.Equal(r.PeriodEnd.Truncate(time.Microsecond)) || t.Amount <= 0 || r.Credit < 0 || t.Amount+r.Credit != t.RecurringAmount {
			return errors.New("engine upgrade terms contradict the membership they replace")
		}
	}
	_, err := moneyutil.NativeToRailMinorExact(t.Currency, t.RecurringAmount)
	return err
}

// UnitAmount is the price of one seat.
func (t InitialMembershipTerms) UnitAmount() int64 {
	return t.RecurringAmount / int64(SeatCount(t.Quantity))
}

// UnmarshalJSON preserves already accepted operations from before access and billing were separated.
// Only an absent access field uses that operation's original billing period; explicit null is indefinite.
func (t *InitialMembershipTerms) UnmarshalJSON(data []byte) error {
	type plain InitialMembershipTerms
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
	*t = InitialMembershipTerms(decoded)
	return nil
}
