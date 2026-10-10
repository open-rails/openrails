package payments

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// TypeNMISale is the one-time card sale on a saved PSP-held card, on every
// card rail that charges a saved method directly (NMI vault sale, Stripe
// engine PaymentIntent).
const TypeNMISale = "nmi_sale"

// saleRails are the rails whose one-time sale charges a saved PSP card.
var saleRails = map[string]bool{"nmi": true, "stripe": true}

// NMISalePayload is the accepted one-time purchase, including the instrument
// and benefits. Recovery never reloads a current catalog or extends these
// windows from the time a delayed provider receipt becomes visible.
type NMISalePayload struct {
	LegacyEntitlements map[string]*int             `json:"legacy_entitlements,omitzero"`
	CheckoutAttemptID  uuid.UUID                   `json:"checkout_attempt_id,omitempty"`
	RequestFingerprint string                      `json:"request_fingerprint"`
	Provider           string                      `json:"provider"`
	PSP                string                      `json:"psp"`
	Amount             int64                       `json:"amount,string"`
	Currency           string                      `json:"currency"`
	Description        string                      `json:"description"`
	UserID             string                      `json:"user_id"`
	PriceID            uuid.UUID                   `json:"price_id"`
	E2ERunID           string                      `json:"e2e_run_id,omitempty"`
	PaymentMethodID    uuid.UUID                   `json:"payment_method_id"`
	Instrument         charge.FrozenInstrument     `json:"instrument"`
	PaymentID          uuid.UUID                   `json:"payment_id"`
	ProductID          uuid.UUID                   `json:"product_id"`
	ListAmount         int64                       `json:"list_amount,string"`
	AcceptedAt         time.Time                   `json:"accepted_at"`
	CreditGrant        *models.CreditGrantSnapshot `json:"credit_grant"`
	// Entitlements is the keys a sale admitted before product access, kept
	// only as accepted evidence. Access follows ProductID.
	Entitlements        json.RawMessage `json:"entitlements,omitempty"`
	AccessDurationHours *int            `json:"access_duration_hours"`
	EntitlementStart    time.Time       `json:"entitlement_start"`
	OwnershipStart      time.Time       `json:"ownership_start"`
	OwnershipEnd        *time.Time      `json:"ownership_end"`
	Eligibility         string          `json:"eligibility"`
	// OrderID is the order the sale pays, through its checkout attempt
	// CheckoutAttemptID. An order sale carries no price, product or benefit:
	// paying settles the order's own frozen lines.
	OrderID uuid.UUID `json:"order_id,omitzero"`
	// Recurring: the order has a recurring line, so the charge stores the
	// card for a recurring agreement.
	Recurring bool `json:"recurring,omitempty"`
	// NewCard: the sale saved its card from the buyer's token, so a decline
	// removes it. On an order it is a card just entered: the charge saves it
	// and is its storing transaction (Token on NMI, Instrument's pm_ on
	// Stripe), and PaymentMethodID names the method its success creates.
	NewCard bool `json:"new_card,omitempty"`
	// Token is an order's NMI Collect.js token; Billing the vault's details.
	Token   string          `json:"token,omitempty"`
	Billing *NewCardBilling `json:"billing,omitempty"`
	// Card and Fingerprint are a new Stripe card as Stripe reported it.
	Card        *models.Card `json:"card,omitempty"`
	Fingerprint string       `json:"fingerprint,omitempty"`
	// Reuse: the customer keeps the order's new card for one-click buys.
	// PurchaseScoped: a new card kept for this purchase only (one-time lines,
	// no Reuse): no stored-credential agreement, and nothing is saved.
	Reuse          bool `json:"reuse,omitempty"`
	PurchaseScoped bool `json:"purchase_scoped,omitempty"`
}

// NewCardBilling is who a new NMI card bills, for its vault.
type NewCardBilling struct {
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
	Address1  string `json:"address1,omitempty"`
	Address2  string `json:"address2,omitempty"`
	City      string `json:"city,omitempty"`
	State     string `json:"state,omitempty"`
	Zip       string `json:"zip,omitempty"`
	Country   string `json:"country,omitempty"`
	Phone     string `json:"phone,omitempty"`
	Email     string `json:"email,omitempty"`
}

// Agreement is the stored-credential agreement the sale charges under: a
// recurring one for an order with a recurring line, none for a
// purchase-scoped card, else the card's card-on-file reuse.
func (p NMISalePayload) Agreement() charge.Agreement {
	switch {
	case p.Recurring:
		return charge.AgreementRecurring
	case p.PurchaseScoped:
		return charge.AgreementNone
	}
	return charge.AgreementCardOnFile
}

// SavesCard reports a new card the order's charge stores.
func (p NMISalePayload) SavesCard() bool {
	return p.OrderID != uuid.Nil && p.NewCard && !p.PurchaseScoped
}

func DecodeNMISalePayload(in gen.BillingProviderIntent) (NMISalePayload, error) {
	var p NMISalePayload
	if err := json.Unmarshal(in.Payload, &p); err != nil {
		return p, err
	}
	if err := moneyutil.RequireFiatCurrency(p.Currency); err != nil {
		return p, err
	}
	customer, err := uuid.Parse(p.UserID)
	if p.OrderID != uuid.Nil {
		return p, validateOrderSale(in, p, customer, err)
	}
	if err != nil || customer == uuid.Nil || in.ID == uuid.Nil || in.MerchantID == uuid.Nil || in.IntentType != TypeNMISale || !saleRails[in.Rail] || in.PspID == nil || *in.PspID != p.Instrument.PSPID || in.CustodianID != nil || in.PriceID == nil || *in.PriceID != p.PriceID || p.PaymentID == uuid.Nil || p.ProductID == uuid.Nil || p.PaymentMethodID == uuid.Nil || p.Amount <= 0 || p.ListAmount < 0 || p.Currency != strings.ToUpper(strings.TrimSpace(p.Currency)) || p.AcceptedAt.IsZero() || p.EntitlementStart.IsZero() || p.OwnershipStart.IsZero() {
		return p, errors.New("sale operation contradicts its accepted purchase")
	}
	for _, instant := range []time.Time{p.AcceptedAt, p.EntitlementStart, p.OwnershipStart} {
		if !instant.Equal(instant.Truncate(time.Microsecond)) {
			return p, errors.New("accepted purchase instants require database precision")
		}
	}
	digest, err := hex.DecodeString(p.RequestFingerprint)
	if err != nil || len(digest) != 32 || p.RequestFingerprint != strings.ToLower(p.RequestFingerprint) {
		return p, errors.New("sale has no canonical request binding")
	}
	if p.Eligibility != "allowed" {
		return p, errors.New("sale was not eligible at acceptance")
	}
	if p.AccessDurationHours == nil && p.OwnershipEnd != nil {
		return p, errors.New("indefinite sale has a finite ownership window")
	}
	if p.AccessDurationHours != nil {
		if *p.AccessDurationHours <= 0 || p.OwnershipEnd == nil || !p.OwnershipEnd.Equal(p.AcceptedAt.Add(time.Duration(*p.AccessDurationHours)*time.Hour)) {
			return p, errors.New("sale ownership does not match accepted duration")
		}
	}
	if err := p.Instrument.Validate(); err != nil {
		return p, err
	}
	if p.Instrument.CustodianHeld() || p.Provider != in.Rail || p.Instrument.RailCustomerRef == "" || in.Rail == "stripe" && p.Instrument.RailMethodRef == "" {
		return p, errors.New("sale instrument contradicts its accepted purchase")
	}
	if p.AccessDurationHours != nil && *p.AccessDurationHours <= 0 || p.OwnershipEnd != nil && !p.OwnershipEnd.After(p.OwnershipStart) || p.EntitlementStart.Before(p.AcceptedAt) || !p.OwnershipStart.Equal(p.AcceptedAt) {
		return p, errors.New("sale access window is invalid")
	}
	if _, err := moneyutil.NativeToRailMinorExact(p.Currency, p.Amount); err != nil {
		return p, err
	}
	if err := p.CreditGrant.Validate(); err != nil {
		return p, err
	}
	return p, nil
}

// validateOrderSale checks an order sale: money, instrument and binding, and
// none of a price sale's benefits.
func validateOrderSale(in gen.BillingProviderIntent, p NMISalePayload, customer uuid.UUID, parseErr error) error {
	if parseErr != nil || customer == uuid.Nil || in.ID == uuid.Nil || in.MerchantID == uuid.Nil || in.IntentType != TypeNMISale || !saleRails[in.Rail] || in.PspID == nil || *in.PspID != p.Instrument.PSPID || in.CustodianID != nil || in.PriceID != nil ||
		p.CheckoutAttemptID == uuid.Nil || p.PriceID != uuid.Nil || p.ProductID != uuid.Nil || p.CreditGrant != nil || p.AccessDurationHours != nil || p.OwnershipEnd != nil || len(p.Entitlements) > 0 ||
		p.PaymentID == uuid.Nil || p.PaymentMethodID == uuid.Nil || p.Amount <= 0 || p.ListAmount != p.Amount || p.Currency != strings.ToUpper(strings.TrimSpace(p.Currency)) || p.AcceptedAt.IsZero() || !p.AcceptedAt.Equal(p.AcceptedAt.Truncate(time.Microsecond)) {
		return errors.New("order sale contradicts its order")
	}
	digest, err := hex.DecodeString(p.RequestFingerprint)
	if err != nil || len(digest) != 32 || p.RequestFingerprint != strings.ToLower(p.RequestFingerprint) {
		return errors.New("order sale has no canonical request binding")
	}
	if err := p.Instrument.Validate(); err != nil {
		return err
	}
	nmiToken := in.Rail != "stripe" && p.NewCard
	if p.Instrument.CustodianHeld() || p.Provider != in.Rail || (p.Instrument.RailCustomerRef == "") != nmiToken || in.Rail == "stripe" && p.Instrument.RailMethodRef == "" ||
		(p.Token != "") != nmiToken || p.PurchaseScoped != (p.NewCard && !p.Recurring && !p.Reuse) || p.Reuse && !p.NewCard || p.NewCard && p.Instrument.Mandate != nil {
		return errors.New("order sale instrument contradicts its order")
	}
	_, err = moneyutil.NativeToRailMinorExact(p.Currency, p.Amount)
	return err
}

func NMISaleOrderReference(id uuid.UUID, runID string) string {
	if runID = strings.TrimSpace(runID); runID != "" {
		sum := sha256.Sum256([]byte(runID))
		return fmt.Sprintf("%s_e2e_%x", id, sum[:4])
	}
	return id.String()
}

func (p *NMISalePayload) UnmarshalJSON(data []byte) error {
	type plain NMISalePayload
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if _, _, err := grants.DecodeAcceptedEntitlements(decoded.Entitlements, decoded.LegacyEntitlements); err != nil {
		return err
	}
	*p = NMISalePayload(decoded)
	return nil
}
