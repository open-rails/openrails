package models

import (
	"errors"
	"strings"
	"time"
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
