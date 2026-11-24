package models

import (
	"errors"
	"strings"
	"time"

	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/currency"
)

// CreditGrantSnapshot is the prepaid value promised by one accepted payment.
// Admission freezes the amount, currency and expiration policy. First successful
// fulfillment fills the absolute dates, persisted on the immutable payment.
// Nil means the agreement promised no credit; it never falls back to a product.
type CreditGrantSnapshot struct {
	Amount           int64      `json:"amount,string"`
	Currency         string     `json:"currency"`
	ExpiresAfterDays int        `json:"expires_after_days"`
	StartsAt         time.Time  `json:"starts_at,omitzero"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
}

// Validate rejects contradictory or lossy persisted accepted credit terms.
func (s *CreditGrantSnapshot) Validate() error {
	if s == nil {
		return nil
	}
	if s.Amount <= 0 || s.Currency == "" || s.Currency != strings.ToUpper(strings.TrimSpace(s.Currency)) || s.ExpiresAfterDays <= 0 || s.ExpiresAfterDays > 36500 {
		return errors.New("accepted credit benefit is incomplete or invalid")
	}
	if s.StartsAt.IsZero() {
		if s.ExpiresAt != nil {
			return errors.New("unfulfilled credit has an expiration instant")
		}
		return nil
	}
	if !s.StartsAt.Equal(s.StartsAt.Truncate(time.Microsecond)) || s.ExpiresAt == nil || !s.ExpiresAt.Equal(s.StartsAt.AddDate(0, 0, s.ExpiresAfterDays)) {
		return errors.New("fulfilled credit dates contradict accepted expiration policy")
	}
	return nil
}

// AcceptCreditGrant is what a product's credit policy promises for one unit
// of a price in priceCurrency at priceAmount: credit in the price's currency,
// its amount or the payment itself, or a pack of a merchant's unit, which the
// policy counts and any currency buys.
func AcceptCreditGrant(policy *catalog.CreditGrantSpec, priceCurrency string, priceAmount int64) (*CreditGrantSnapshot, error) {
	if policy == nil {
		return nil, nil
	}
	code := strings.ToUpper(strings.TrimSpace(string(policy.Currency)))
	priceCurrency = strings.ToUpper(strings.TrimSpace(priceCurrency))
	switch {
	case currency.IsUnitCode(priceCurrency):
		return nil, errors.New("credit is bought with money, never with a merchant's unit")
	case code != priceCurrency && !currency.IsUnitCode(code):
		return nil, errors.New("purchased credit currency must match payment currency")
	case code != priceCurrency && policy.Amount == nil:
		return nil, errors.New("a pack of a merchant's unit names its amount")
	}
	amount := priceAmount
	if policy.Amount != nil {
		amount = *policy.Amount
	} else if !policy.FromPayment {
		return nil, errors.New("credit benefit requires an amount or from_payment")
	}
	days := 365
	if policy.ExpiresAfterDays != nil {
		days = *policy.ExpiresAfterDays
	}
	out := &CreditGrantSnapshot{Amount: amount, Currency: code, ExpiresAfterDays: days}
	return out, out.Validate()
}

// CloneCreditGrantSnapshot copies an optional agreement without sharing dates.
func CloneCreditGrantSnapshot(s *CreditGrantSnapshot) *CreditGrantSnapshot {
	if s == nil {
		return nil
	}
	out := *s
	if s.ExpiresAt != nil {
		end := *s.ExpiresAt
		out.ExpiresAt = &end
	}
	return &out
}

// SameCreditGrantPromise compares the accepted benefit independently of when it
// was fulfilled. The persisted payment alone owns the fulfillment dates.
func SameCreditGrantPromise(a, b *CreditGrantSnapshot) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Amount == b.Amount && a.Currency == b.Currency && a.ExpiresAfterDays == b.ExpiresAfterDays
}
