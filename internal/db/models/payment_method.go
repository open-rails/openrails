package models

import (
	"strings"
	"time"
	"unicode"

	"github.com/open-rails/openrails/billing"
	sharedformat "github.com/open-rails/openrails/internal/shared/format"

	"github.com/google/uuid"
)

// PaymentMethod is a stored payment method on any rail.
type PaymentMethod struct {
	ID uuid.UUID `json:"id"`
	// CustomerID is the host's subject UUID within MerchantID.
	CustomerID uuid.UUID `json:"customer_id,omitempty"`
	Rail       Rail      `json:"rail"` // Rail: nmi, ccbill, solana

	// PspID is the PSP holding a PSP-held card (Custodian == CustodianPSP),
	// nil for a card a third-party custodian holds: routing picks the PSP for
	// each of its charges.
	PspID *uuid.UUID `json:"psp_id"`

	// Rail handles: a customer-scope ref and an instrument-scope ref. For NMI
	// the customer-scope ref is per card: OpenRails mints one vault customer
	// per card, so the person exists only as the local customer_id.
	RailCustomerRef string `json:"-"` // NMI customer_vault_id (per card); "" for Stripe, see psp_customers
	RailMethodRef   string `json:"-"` // instrument-scope handle (NMI billing_id — legacy imports only; Stripe pm_, Spreedly/HyperSwitch token)

	// Custodian is who holds this instrument, orthogonal to who charges it
	// (Rail + PspID); see the Custodian* constants. CCBill and Solana store no
	// instrument: no payment_methods row, not a custodian value.
	Custodian   string     `json:"-"`
	CustodianID *uuid.UUID `json:"-"`

	// Custodian-held instrument fields: Fingerprint is the custodian's stable
	// PAN fingerprint; ChargeVia routes pan_proxy|network_token; a non-empty
	// ParkReason parks the instrument (a custody problem, never a cancel).
	Fingerprint        string     `json:"-"`
	NetworkTokenID     string     `json:"-"`
	NetworkTokenStatus string     `json:"-"`
	NetworkTokenPAR    string     `json:"-"`
	ChargeVia          string     `json:"-"`
	ParkReason         string     `json:"-"`
	ParkedAt           *time.Time `json:"-"`

	// Status is active, closed, replaced or removed; only active is charged
	// and the others are final.
	Status       string     `json:"status"`
	ReplacedByID *uuid.UUID `json:"-"`
	// ContactCardholderAt is when the issuer last asked for the cardholder.
	ContactCardholderAt *time.Time `json:"-"`

	Card     Card           `json:"card"`
	Metadata map[string]any `json:"metadata,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Relationships
	Subscriptions []*Subscription `json:"subscriptions,omitempty"`
}

// Chargeable reports whether the method can be charged now: active, with no
// holder-side hold.
func (pm *PaymentMethod) Chargeable() bool {
	return pm.Status == "active" && pm.ParkReason == ""
}

// HoldingPSP is the PSP holding a PSP-held card; uuid.Nil for a card a
// third-party custodian holds.
func (pm *PaymentMethod) HoldingPSP() uuid.UUID {
	if pm.PspID == nil {
		return uuid.Nil
	}
	return *pm.PspID
}

// HeldBy reports whether psp holds the card. A card a third-party custodian
// holds is held by no PSP: any PSP of its rail that reaches its custodian can
// charge it.
func (pm *PaymentMethod) HeldBy(psp uuid.UUID) bool {
	return pm.PspID != nil && *pm.PspID == psp
}

// ChargeableOn reports whether a charge through psp can use the card: the PSP
// that holds a PSP-held card, or any PSP for a custodian-held one.
func (pm *PaymentMethod) ChargeableOn(psp uuid.UUID) bool {
	return pm.CustodianHeld() || pm.HeldBy(psp)
}

// CustodianHeld reports whether a third-party custodian holds the card; an
// unstated custodian is the PSP.
func (pm *PaymentMethod) CustodianHeld() bool {
	return pm.Custodian != "" && pm.Custodian != CustodianPSP
}

// Card is a stored card's display facts. A zero field is one the provider did
// not report.
type Card struct {
	Brand string `json:"brand"`
	Last4 string `json:"last4"`
	// ExpMonth (1-12) and ExpYear (four digits) are both set or both zero.
	ExpMonth int `json:"exp_month"`
	ExpYear  int `json:"exp_year"`
}

// Expired reports whether the card's expiry month ended before now.
func (c Card) Expired(now time.Time) bool {
	return c.ExpYear != 0 && !now.Before(c.ExpiresAt())
}

// ExpiresAt is the first instant after the expiry month (UTC); zero when the
// expiry is unknown.
func (c Card) ExpiresAt() time.Time {
	if c.ExpYear == 0 || c.ExpMonth < 1 || c.ExpMonth > 12 {
		return time.Time{}
	}
	return time.Date(c.ExpYear, time.Month(c.ExpMonth), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
}

// ParseCard reads the display facts a provider or custodian reports: a brand,
// a last four or masked number, and an expiry spelled MMYY, MM/YY, MM-YY or
// MM/YYYY. A fact that does not parse is left unknown, never guessed.
func ParseCard(brand, number, expiry string) Card {
	c := Card{Brand: cardBrand(brand), Last4: cardLast4(number)}
	c.ExpMonth, c.ExpYear = cardExpiry(expiry)
	return c
}

func cardBrand(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" || len(v) > 30 {
		return ""
	}
	for _, r := range v {
		if !unicode.IsLetter(r) && r != ' ' && r != '-' && r != '_' {
			return ""
		}
	}
	return v
}

func cardLast4(v string) string {
	var digits []rune
	for _, r := range v {
		if r >= '0' && r <= '9' {
			digits = append(digits, r)
		}
	}
	if len(digits) < 4 {
		return ""
	}
	return string(digits[len(digits)-4:])
}

func cardExpiry(v string) (month, year int) {
	v = strings.TrimSpace(v)
	if len(v) == 4 && !strings.ContainsAny(v, "/-") {
		v = v[:2] + "/" + v[2:]
	}
	m, y, err := sharedformat.ParseExpiry(v)
	if err != nil {
		return 0, 0
	}
	return m, y
}

// CardFromDetails reads a card's wire shape, keeping only well-formed facts.
func CardFromDetails(d *billing.CardDetails) Card {
	if d == nil {
		return Card{}
	}
	deref := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	c := ParseCard(deref(d.Brand), deref(d.Last4), "")
	if len(deref(d.Last4)) != 4 {
		c.Last4 = ""
	}
	if d.ExpMonth != nil && d.ExpYear != nil && *d.ExpMonth >= 1 && *d.ExpMonth <= 12 && *d.ExpYear >= 2000 && *d.ExpYear <= 2199 {
		c.ExpMonth, c.ExpYear = *d.ExpMonth, *d.ExpYear
	}
	return c
}

// Details is the card's wire shape; nil when nothing is known about it.
func (c Card) Details() *billing.CardDetails {
	if c == (Card{}) {
		return nil
	}
	out := &billing.CardDetails{}
	if c.Brand != "" {
		out.Brand = &c.Brand
	}
	if c.Last4 != "" {
		out.Last4 = &c.Last4
	}
	if c.ExpYear != 0 {
		month, year := c.ExpMonth, c.ExpYear
		out.ExpMonth, out.ExpYear = &month, &year
	}
	return out
}

// CardFromColumns reads a card's nullable columns.
func CardFromColumns(brand, last4 *string, month, year *int16) Card {
	c := Card{}
	if brand != nil {
		c.Brand = *brand
	}
	if last4 != nil {
		c.Last4 = *last4
	}
	if month != nil && year != nil {
		c.ExpMonth, c.ExpYear = int(*month), int(*year)
	}
	return c
}

// Columns is the card as its nullable columns: NULL for an unreported field.
func (c Card) Columns() (brand, last4 *string, month, year *int16) {
	if c.Brand != "" {
		brand = &c.Brand
	}
	if c.Last4 != "" {
		last4 = &c.Last4
	}
	if c.ExpMonth >= 1 && c.ExpMonth <= 12 && c.ExpYear >= 2000 && c.ExpYear <= 2199 {
		m, y := int16(c.ExpMonth), int16(c.ExpYear) // #nosec G115 -- bounded above
		month, year = &m, &y
	}
	return brand, last4, month, year
}

// Custodian values (payment_methods.custodian). Custody (who holds the card)
// is orthogonal to the processor (Rail + PspID); the DB CHECK pins the same
// set, so adding a custodian is a migration.
const (
	// CustodianPSP: the instrument lives at the processor itself — a Stripe
	// pm_ on a Stripe Customer, an NMI customer_vault_id in the gateway.
	CustodianPSP = "psp"
	// CustodianBasisTheory: the PAN lives in the Basis Theory vault and is
	// proxied to the processor at charge time.
	CustodianBasisTheory = "basis_theory"
	CustodianHyperSwitch = "hyperswitch"
)

// Custodians lists the declared custody values in stable order.
func Custodians() []string { return []string{CustodianPSP, CustodianBasisTheory, CustodianHyperSwitch} }

// PaymentMethodCharge is a payment method's last-charge health, derived from
// payments at query time, never stored.
type PaymentMethodCharge struct {
	LastChargedAt time.Time
	Status        string // completed | failed: the latest charge attempt's outcome
}
