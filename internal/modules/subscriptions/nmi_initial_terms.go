package subscriptions

import (
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"strings"
	"time"
)

const TypeNMIInitialEnrollment = "nmi_subscription_create"

type NMIInitialEnrollmentPayload struct {
	Terms              InitialMembershipTerms  `json:"terms"`
	Instrument         charge.FrozenInstrument `json:"instrument"`
	RequestFingerprint string                  `json:"request_fingerprint"`
	DayFrequency       int                     `json:"day_frequency"`
	PlanPayments       int                     `json:"plan_payments"`
	// Provider is the RAIL ("nmi") — local row vocabulary.
	Provider string `json:"provider"`
	// PSP is the payment provider (account key, e.g. "mobius")
	// this create charges through; "" resolves the rail's active account.
	PSP             string `json:"psp,omitempty"`
	PlanID          string `json:"plan_id"`
	CustomerVaultID string `json:"customer_vault_id"`
	// BillingID binds the subscription to ONE stored card in the vault (#682
	// shared-vault support); "" uses the vault's priority-1 entry.
	BillingID           string     `json:"billing_id,omitempty"`
	AmountMicros        int64      `json:"amount_micros"`
	Currency            string     `json:"currency"`
	Email               string     `json:"email,omitempty"`
	UserID              string     `json:"user_id"`
	PriceID             uuid.UUID  `json:"price_id"`
	LocalSubscriptionID uuid.UUID  `json:"local_subscription_id"`
	PaymentMethodID     *uuid.UUID `json:"payment_method_id,omitempty"`
	// StartDate (YYYYMMDD, "" = immediate) + DelayedStart mirror
	// nmiSubscriptionStartDate's coverage-derived delayed start.
	StartDate    string     `json:"start_date,omitempty"`
	DelayedStart *time.Time `json:"delayed_start,omitempty"`
	// StoredCredentialRef is the instrument's RECURRING-sequence
	// stored-credential anchor at enqueue (#297). "" = this enrollment is the
	// sequence's initial CIT (indicator=stored) and finalize captures the
	// first-charge transaction id as the anchor (delayed starts produce no
	// first charge — the anchor then back-fills from the first dunning MIT).
	StoredCredentialRef string `json:"stored_credential_ref,omitempty"`
	E2ERunID            string `json:"e2e_run_id,omitempty"`
	// CheckoutIdempotencyKey lets finalize complete the request-level
	// idempotency record so a client replay gets the cached response.
	CheckoutIdempotencyKey string `json:"checkout_idempotency_key"`

	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
	Address1  string `json:"address1,omitempty"`
	City      string `json:"city,omitempty"`
	State     string `json:"state,omitempty"`
	Zip       string `json:"zip,omitempty"`
	Country   string `json:"country,omitempty"`
}

// DecodeNMIInitialEnrollmentPayload binds provider addressing and local effects
// to the same canonical accepted operation; historical incomplete payloads are
// not admitted under the fresh-database contract.
func DecodeNMIInitialEnrollmentPayload(in gen.OpenrailsRailIntent) (NMIInitialEnrollmentPayload, error) {
	var p NMIInitialEnrollmentPayload
	if err := json.Unmarshal(in.Payload, &p); err != nil {
		return p, err
	}
	if err := p.Terms.Validate(); err != nil {
		return p, err
	}
	if err := p.Instrument.Validate(); err != nil {
		return p, err
	}
	if in.ID == uuid.Nil || in.MerchantID == uuid.Nil || in.IntentType != TypeNMIInitialEnrollment || in.Rail != "nmi" || in.PspID == nil || in.CustodianID != nil || *in.PspID != p.Instrument.PSPID || p.Instrument.PSPID != p.Terms.PSPID || p.Instrument.CustodianHeld() || p.Instrument.RailCustomerRef == "" || in.PriceID == nil || *in.PriceID != p.Terms.PriceID || p.Provider != "nmi" || strings.TrimSpace(p.PSP) == "" || strings.TrimSpace(p.PlanID) == "" || p.RequestFingerprint == "" || p.CheckoutIdempotencyKey == "" || p.DayFrequency <= 0 || p.PlanPayments < 0 {
		return p, errors.New("initial enrollment operation contradicts its accepted scope")
	}
	if p.UserID != p.Terms.CustomerID.String() || p.PriceID != p.Terms.PriceID || p.LocalSubscriptionID != p.Terms.SubscriptionID || p.PaymentMethodID == nil || *p.PaymentMethodID != p.Terms.PaymentMethodID || p.AmountMicros != p.Terms.Amount || p.Currency != p.Terms.Currency || p.CustomerVaultID != p.Instrument.RailCustomerRef || p.BillingID != p.Instrument.RailMethodRef || p.StoredCredentialRef != p.Instrument.StoredCredentialRecurringRef {
		return p, errors.New("initial enrollment addressing contradicts accepted membership")
	}
	start, err := time.Parse("20060102", p.StartDate)
	if err != nil || !start.After(p.Terms.AcceptedAt) {
		return p, errors.New("initial enrollment has no exact future recurring date")
	}
	if p.Terms.Pending {
		if p.DelayedStart == nil || !p.DelayedStart.Equal(p.Terms.PeriodStart) || !start.Equal(p.Terms.PeriodStart) {
			return p, errors.New("delayed enrollment contradicts accepted coverage")
		}
	} else if p.DelayedStart != nil || start.Format("20060102") != p.Terms.PeriodEnd.UTC().Format("20060102") {
		return p, errors.New("initial recurring date contradicts accepted initial period")
	}
	return p, nil
}
