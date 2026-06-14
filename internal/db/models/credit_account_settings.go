package models

import (
	"time"

	"github.com/google/uuid"
)

// CreditAccountSettings is the per-(tenant, tenant subject, credit_type) spend policy
// and money-in configuration (issue #237). Tensorhub sets these; OpenRails
// stores and enforces them. NULL cap columns mean "no cap".
type CreditAccountSettings struct {
	ID           uuid.UUID `json:"id"`
	MerchantID   uuid.UUID `json:"tenant_id"`
	CustomerID   uuid.UUID `json:"customer_id"`
	CreditTypeID uuid.UUID `json:"credit_type_id"`

	// Policy.
	BillingMode              string     `json:"billing_mode"`
	MaxSpendPerDayMicros     *int64     `json:"max_spend_per_day_micros,omitempty"`
	MaxSpendPerMonthMicros   *int64     `json:"max_spend_per_month_micros,omitempty"`
	MaxOutstandingOwedMicros *int64     `json:"max_outstanding_owed_micros,omitempty"`
	LowBalanceThreshold      *int64     `json:"low_balance_threshold_micros,omitempty"`
	AutoTopupEnabled         bool       `json:"auto_topup_enabled"`
	AutoTopupAmountCents     *int64     `json:"auto_topup_amount_cents,omitempty"`
	AutoTopupPaymentMethod   *uuid.UUID `json:"auto_topup_payment_method_id,omitempty"`
	DefaultCreditExpiryDays  *int       `json:"default_credit_expiry_days,omitempty"`
	HardStopOnBreach         bool       `json:"hard_stop_on_breach"`
	AlertThresholdPct        int        `json:"alert_threshold_pct"`

	// Dynamic state (money-in / alert workers).
	OutstandingOwedMicros int64      `json:"outstanding_owed_micros"`
	LastAlertAt           *time.Time `json:"last_alert_at,omitempty"`
	LastTopupAt           *time.Time `json:"last_topup_at,omitempty"`

	// Suspension + payment-method-verification state (issue #299).
	VerifiedPaymentMethod bool       `json:"verified_payment_method"`
	VerifiedAt            *time.Time `json:"verified_at,omitempty"`
	SuspendedAt           *time.Time `json:"suspended_at,omitempty"`
	SuspendReason         *string    `json:"suspend_reason,omitempty"`
	// Tier is the graduated trust tier earned from cumulative paid spend (#298).
	Tier *string `json:"tier,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CreditSpendLimit is an optional per-actor spend cap under a tenant subject
// (issue #237 per_actor_caps / #246). The actor string is matched against
// credit_transactions.actor (the principal that caused usage), in one of the
// canonical forms: 'serviceToken:<key_id>', 'user:<user_id>', or '<issuer>:<sub>'.
type CreditSpendLimit struct {
	ID                     uuid.UUID `json:"id"`
	MerchantID             uuid.UUID `json:"tenant_id"`
	CustomerID             uuid.UUID `json:"customer_id"`
	CreditTypeID           uuid.UUID `json:"credit_type_id"`
	Actor                  string    `json:"actor"`
	MaxSpendPerDayMicros   *int64    `json:"max_spend_per_day_micros,omitempty"`
	MaxSpendPerMonthMicros *int64    `json:"max_spend_per_month_micros,omitempty"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
}
