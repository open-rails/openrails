package models

import (
	"time"

	"github.com/google/uuid"
)

// Money domain types: the universal fractional-USD wallet (µ$ = 1e-6 USD).
// No credit_type dimension — money is identical for every tenant (#472).

type MoneyBalance struct {
	ID              uuid.UUID `json:"id"`
	TenantID        uuid.UUID `json:"tenant_id"`
	TenantSubjectID uuid.UUID `json:"tenant_subject_id"`
	Balance         int64     `json:"balance"`
	HeldBalance     int64     `json:"held_balance"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type MoneyTransaction struct {
	ID              uuid.UUID      `json:"id"`
	TenantID        uuid.UUID      `json:"tenant_id"`
	TenantSubjectID uuid.UUID      `json:"tenant_subject_id"`
	Actor           string         `json:"actor"`
	Resource        *string        `json:"resource,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
	Amount          int64          `json:"amount"`
	BalanceAfter    *int64         `json:"balance_after,omitempty"`
	TransactionType string         `json:"transaction_type"`
	Status          string         `json:"status"`
	Authorized      *int64         `json:"authorized_amount,omitempty"`
	Captured        *int64         `json:"captured_amount,omitempty"`
	Source          string         `json:"source"`
	SourceID        *string        `json:"source_id,omitempty"`
	ExpiresAt       *time.Time     `json:"expires_at,omitempty"`
	Description     *string        `json:"description,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

type MoneyBlock struct {
	ID                  uuid.UUID  `json:"id"`
	TenantID            uuid.UUID  `json:"tenant_id"`
	TenantSubjectID     uuid.UUID  `json:"tenant_subject_id"`
	OriginalAmount      int64      `json:"original_amount"`
	RemainingAmount     int64      `json:"remaining_amount"`
	ExpiresAt           *time.Time `json:"expires_at,omitempty"`
	SourceTransactionID *uuid.UUID `json:"source_transaction_id,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
}

// MoneyWindow is a prepaid money window (issue #335): one bulk reservation a
// host admits requests against locally. See CreditWindow for the mechanics.
type MoneyWindow struct {
	ID              uuid.UUID `json:"id"`
	TenantID        uuid.UUID `json:"tenant_id"`
	TenantSubjectID uuid.UUID `json:"tenant_subject_id"`
	HeldAmount      int64     `json:"held_amount"`
	SettledAmount   int64     `json:"settled_amount"`
	Status          string    `json:"status"` // open | closed | expired
	ExpiresAt       time.Time `json:"expires_at"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// MoneyAccount is the per-(tenant, tenant subject) spend policy and money-in
// configuration (issue #237). NULL cap columns mean "no cap".
type MoneyAccount struct {
	ID              uuid.UUID `json:"id"`
	TenantID        uuid.UUID `json:"tenant_id"`
	TenantSubjectID uuid.UUID `json:"tenant_subject_id"`

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

	OutstandingOwedMicros int64      `json:"outstanding_owed_micros"`
	LastAlertAt           *time.Time `json:"last_alert_at,omitempty"`
	LastTopupAt           *time.Time `json:"last_topup_at,omitempty"`

	VerifiedPaymentMethod bool       `json:"verified_payment_method"`
	VerifiedAt            *time.Time `json:"verified_at,omitempty"`
	SuspendedAt           *time.Time `json:"suspended_at,omitempty"`
	SuspendReason         *string    `json:"suspend_reason,omitempty"`
	Tier                  *string    `json:"tier,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// MoneySpendLimit is an optional per-actor µ$ spend cap under a tenant subject
// (issue #237). The actor string is matched against money_transactions.actor.
type MoneySpendLimit struct {
	ID                     uuid.UUID `json:"id"`
	TenantID               uuid.UUID `json:"tenant_id"`
	TenantSubjectID        uuid.UUID `json:"tenant_subject_id"`
	Actor                  string    `json:"actor"`
	MaxSpendPerDayMicros   *int64    `json:"max_spend_per_day_micros,omitempty"`
	MaxSpendPerMonthMicros *int64    `json:"max_spend_per_month_micros,omitempty"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
}
