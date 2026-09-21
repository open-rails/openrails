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
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

const TypeNMISale = "nmi_sale"

// NMISalePayload is the accepted one-time purchase, including the instrument
// and benefits. Recovery never reloads a current catalog or extends these
// windows from the time a delayed provider receipt becomes visible.
type NMISalePayload struct {
	RequestFingerprint  string                  `json:"request_fingerprint"`
	Provider            string                  `json:"provider"`
	PSP                 string                  `json:"psp"`
	Amount              int64                   `json:"amount,string"`
	Currency            string                  `json:"currency"`
	Description         string                  `json:"description"`
	UserID              string                  `json:"user_id"`
	PriceID             uuid.UUID               `json:"price_id"`
	E2ERunID            string                  `json:"e2e_run_id,omitempty"`
	PaymentMethodID     uuid.UUID               `json:"payment_method_id"`
	Instrument          charge.FrozenInstrument `json:"instrument"`
	PaymentID           uuid.UUID               `json:"payment_id"`
	ProductID           uuid.UUID               `json:"product_id"`
	ListAmount          int64                   `json:"list_amount,string"`
	AcceptedAt          time.Time               `json:"accepted_at"`
	Entitlements        map[string]*int         `json:"entitlements"`
	AccessDurationHours *int                    `json:"access_duration_hours"`
	EntitlementStart    time.Time               `json:"entitlement_start"`
	OwnershipStart      time.Time               `json:"ownership_start"`
	OwnershipEnd        *time.Time              `json:"ownership_end"`
	Eligibility         string                  `json:"eligibility"`
}

func DecodeNMISalePayload(in gen.OpenrailsRailIntent) (NMISalePayload, error) {
	var p NMISalePayload
	if err := json.Unmarshal(in.Payload, &p); err != nil {
		return p, err
	}
	customer, err := uuid.Parse(p.UserID)
	if err != nil || customer == uuid.Nil || in.ID == uuid.Nil || in.MerchantID == uuid.Nil || in.IntentType != TypeNMISale || in.Rail != "nmi" || in.PspID == nil || *in.PspID != p.Instrument.PSPID || in.CustodianID != nil || in.PriceID == nil || *in.PriceID != p.PriceID || p.PaymentID == uuid.Nil || p.ProductID == uuid.Nil || p.PaymentMethodID == uuid.Nil || p.Amount <= 0 || p.ListAmount < 0 || p.Currency != strings.ToUpper(strings.TrimSpace(p.Currency)) || p.AcceptedAt.IsZero() || p.EntitlementStart.IsZero() || p.OwnershipStart.IsZero() || p.Entitlements == nil {
		return p, errors.New("sale operation contradicts its accepted purchase")
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
	if p.Instrument.CustodianHeld() || p.Provider != "nmi" || p.Instrument.RailCustomerRef == "" {
		return p, errors.New("sale instrument contradicts its accepted purchase")
	}
	if p.AccessDurationHours != nil && *p.AccessDurationHours <= 0 || p.OwnershipEnd != nil && !p.OwnershipEnd.After(p.OwnershipStart) || p.EntitlementStart.Before(p.AcceptedAt) || !p.OwnershipStart.Equal(p.AcceptedAt) {
		return p, errors.New("sale access window is invalid")
	}
	if _, err := moneyutil.NativeToRailMinorExact(p.Currency, p.Amount); err != nil {
		return p, err
	}
	return p, nil
}

func NMISaleOrderReference(id uuid.UUID, runID string) string {
	if runID = strings.TrimSpace(runID); runID != "" {
		sum := sha256.Sum256([]byte(runID))
		return fmt.Sprintf("%s_e2e_%x", id, sum[:4])
	}
	return id.String()
}
