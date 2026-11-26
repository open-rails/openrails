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
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// TypeNMISale is the one-time card sale on a saved PSP-held card, on every
// card rail that charges a saved method directly (NMI vault sale, Stripe
// engine PaymentIntent).
const TypeNMISale = "nmi_sale"

// saleRails are the rails whose one-time sale charges a saved PSP card.
var saleRails = map[string]bool{"nmi": true, "stripe": true}

// NMISalePayload is an invoice's accepted card sale: its money, instrument
// and binding. Paying settles the invoice's own frozen lines.
type NMISalePayload struct {
	// InvoiceCaptchaSubjects preserves the initiating HTTP address for worker completion.
	InvoiceCaptchaSubjects []string `json:"order_captcha_subjects,omitempty"`

	InvoiceAttemptID   uuid.UUID               `json:"order_attempt_id,omitempty"`
	RequestFingerprint string                  `json:"request_fingerprint"`
	Provider           string                  `json:"provider"`
	PSP                string                  `json:"psp"`
	Amount             int64                   `json:"amount,string"`
	Currency           string                  `json:"currency"`
	Description        string                  `json:"description"`
	UserID             string                  `json:"user_id"`
	E2ERunID           string                  `json:"e2e_run_id,omitempty"`
	PaymentMethodID    uuid.UUID               `json:"payment_method_id"`
	Instrument         charge.FrozenInstrument `json:"instrument"`
	PaymentID          uuid.UUID               `json:"payment_id"`
	ListAmount         int64                   `json:"list_amount,string"`
	AcceptedAt         time.Time               `json:"accepted_at"`
	// InvoiceID is the invoice the sale pays, through its invoice attempt
	// InvoiceAttemptID.
	InvoiceID uuid.UUID `json:"order_id,omitzero"`
	// Recurring: the invoice has a recurring line, so the charge stores the
	// card for a recurring agreement.
	Recurring bool `json:"recurring,omitempty"`
	// ReuseConsent permits presenting a saved platform card to open a new
	// recurring agreement. Its one-time reference does not authorize renewals.
	ReuseConsent *charge.Mandate `json:"reuse_consent,omitempty"`
	// NewCard: the sale saved its card from the buyer's token, so a decline
	// removes it. On an invoice it is a card just entered: the charge saves it
	// and is its storing transaction (Token on NMI, Instrument's pm_ on
	// Stripe), and PaymentMethodID names the method its success creates.
	NewCard bool `json:"new_card,omitempty"`
	// Token is an invoice's NMI Collect.js token, or the Basis Theory token
	// intent that collected a custodian-held card; Billing the vault's
	// details.
	Token   string          `json:"token,omitempty"`
	Billing *NewCardBilling `json:"billing,omitempty"`
	// Card is a new card as Stripe or the Basis Theory collection reported
	// it; Fingerprint is a new Stripe card's.
	Card        *models.Card `json:"card,omitempty"`
	Fingerprint string       `json:"fingerprint,omitempty"`
	// Reuse: the customer keeps the invoice's new card for one-click buys.
	// PurchaseScoped: a new card kept for this purchase only (one-time lines,
	// no Reuse): no stored-credential agreement, and nothing is saved.
	Reuse          bool `json:"reuse,omitempty"`
	PurchaseScoped bool `json:"purchase_scoped,omitempty"`
	// HyperSwitch binds an invoice's HyperSwitch-held card to the custodian
	// account that holds it: the sale goes through its NMI proxy.
	HyperSwitch *charge.HyperSwitchBinding `json:"hyperswitch,omitempty"`
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
// recurring one for an invoice with a recurring line, none for a
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

// SavesCard reports a new card the invoice's charge stores.
func (p NMISalePayload) SavesCard() bool {
	return p.NewCard && !p.PurchaseScoped
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
	return p, validateInvoiceSale(in, p, customer, err)
}

// validateInvoiceSale checks an invoice sale: money, instrument and binding.
func validateInvoiceSale(in gen.BillingProviderIntent, p NMISalePayload, customer uuid.UUID, parseErr error) error {
	if parseErr != nil || customer == uuid.Nil || in.ID == uuid.Nil || in.MerchantID == uuid.Nil || in.IntentType != TypeNMISale || !saleRails[in.Rail] || in.PspID == nil || *in.PspID != p.Instrument.PSPID || invalidInvoiceCustodianAddress(in, p.Instrument) || in.PriceID != nil ||
		p.InvoiceID == uuid.Nil || p.InvoiceAttemptID == uuid.Nil ||
		p.PaymentID == uuid.Nil || p.PaymentMethodID == uuid.Nil || p.Amount <= 0 || p.ListAmount != p.Amount || p.Currency != strings.ToUpper(strings.TrimSpace(p.Currency)) || p.AcceptedAt.IsZero() || !p.AcceptedAt.Equal(p.AcceptedAt.Truncate(time.Microsecond)) {
		return errors.New("invoice sale contradicts its invoice")
	}
	digest, err := hex.DecodeString(p.RequestFingerprint)
	if err != nil || len(digest) != 32 || p.RequestFingerprint != strings.ToLower(p.RequestFingerprint) {
		return errors.New("invoice sale has no canonical request binding")
	}
	if err := p.Instrument.Validate(); err != nil {
		return err
	}
	nmiToken := in.Rail != "stripe" && p.NewCard
	// A HyperSwitch-held card pays an invoice that opens a subscription, as its
	// recurring storing charge. A merchant Basis Theory card pays one-time lines as
	// its token intent presents it: a new card, or one the customer holds.
	custody := true
	switch p.Instrument.Custodian {
	case models.CustodianPSP:
		custody = p.HyperSwitch == nil
	case models.CustodianHyperSwitch:
		hs := p.HyperSwitch
		custody = hs != nil && hs.Validate() == nil && in.Rail == "nmi" && p.Recurring && !p.NewCard
	case models.CustodianBasisTheory, models.CustodianPlatform:
		source := p.Token != ""
		if !p.NewCard && p.Token == "" {
			consent := p.Instrument.Mandate
			if p.Recurring {
				consent = p.ReuseConsent
			}
			source = consent != nil && consent.Kind == charge.AgreementCardOnFile && consent.InitialTransactionID != ""
		}
		custody = p.HyperSwitch == nil && in.Rail == "nmi" && source && p.Instrument.RailCustomerRef == "" && p.NewCard == (p.Instrument.RailMethodRef == "")
	}
	if p.ReuseConsent != nil && (!p.Recurring || p.NewCard || p.Token != "" || !models.BasisTheoryCustody(p.Instrument.Custodian) || p.Instrument.Mandate != nil) {
		return errors.New("reuse consent is only for a saved platform card opening a recurring agreement")
	}
	if !custody {
		return errors.New("invoice sale custody contradicts its invoice")
	}
	bt := p.Instrument.Custodian == models.CustodianBasisTheory || p.Instrument.Custodian == models.CustodianPlatform
	if p.Provider != in.Rail || !bt && ((p.Instrument.RailCustomerRef == "") != nmiToken || (p.Token != "") != nmiToken) || in.Rail == "stripe" && p.Instrument.RailMethodRef == "" ||
		p.PurchaseScoped != (p.NewCard && !p.Recurring && !p.Reuse) || p.Reuse && !p.NewCard || p.NewCard && p.Instrument.Mandate != nil {
		return errors.New("invoice sale instrument contradicts its invoice")
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

// invalidInvoiceCustodianAddress: a sale of a Basis Theory or platform-held
// card is addressed to exactly its custodian, any other sale to none.
func invalidInvoiceCustodianAddress(in gen.BillingProviderIntent, instrument charge.FrozenInstrument) bool {
	if !models.BasisTheoryCustody(instrument.Custodian) {
		return in.CustodianID != nil
	}
	return in.CustodianID == nil || instrument.CustodianID == nil || *in.CustodianID != *instrument.CustodianID
}
