package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// CreditAccountSettings is the per-(tenant, tenant subject, credit_type) spend policy
// and money-in configuration (issue #237). Tensorhub sets these; OpenRails
// stores and enforces them. NULL cap columns mean "no cap".
type CreditAccountSettings struct {
	bun.BaseModel `bun:"table:billing.credit_account_settings,alias:cas"`

	ID              uuid.UUID `bun:"id,pk,type:uuid" json:"id"`
	TenantID        uuid.UUID `bun:"tenant_id,type:uuid,nullzero" json:"tenant_id"`
	TenantSubjectID uuid.UUID `bun:"tenant_subject_id,type:uuid,nullzero" json:"tenant_subject_id"`
	CreditTypeID    uuid.UUID `bun:"credit_type_id,notnull,type:uuid" json:"credit_type_id"`

	// Policy.
	BillingMode             string     `bun:"billing_mode,notnull" json:"billing_mode"`
	MaxSpendPerDayCents     *int64     `bun:"max_spend_per_day_cents,nullzero" json:"max_spend_per_day_cents,omitempty"`
	MaxSpendPerMonthCents   *int64     `bun:"max_spend_per_month_cents,nullzero" json:"max_spend_per_month_cents,omitempty"`
	MaxOutstandingOwedCents *int64     `bun:"max_outstanding_owed_cents,nullzero" json:"max_outstanding_owed_cents,omitempty"`
	LowBalanceThreshold     *int64     `bun:"low_balance_threshold_cents,nullzero" json:"low_balance_threshold_cents,omitempty"`
	AutoTopupEnabled        bool       `bun:"auto_topup_enabled,notnull" json:"auto_topup_enabled"`
	AutoTopupAmountCents    *int64     `bun:"auto_topup_amount_cents,nullzero" json:"auto_topup_amount_cents,omitempty"`
	AutoTopupPaymentMethod  *uuid.UUID `bun:"auto_topup_payment_method_id,type:uuid,nullzero" json:"auto_topup_payment_method_id,omitempty"`
	DefaultCreditExpiryDays *int       `bun:"default_credit_expiry_days,nullzero" json:"default_credit_expiry_days,omitempty"`
	HardStopOnBreach        bool       `bun:"hard_stop_on_breach,notnull" json:"hard_stop_on_breach"`
	AlertThresholdPct       int        `bun:"alert_threshold_pct,notnull" json:"alert_threshold_pct"`

	// Dynamic state (money-in / alert workers).
	OutstandingOwedCents int64      `bun:"outstanding_owed_cents,notnull" json:"outstanding_owed_cents"`
	LastAlertAt          *time.Time `bun:"last_alert_at,nullzero" json:"last_alert_at,omitempty"`
	LastTopupAt          *time.Time `bun:"last_topup_at,nullzero" json:"last_topup_at,omitempty"`

	// Suspension + payment-method-verification state (issue #299).
	VerifiedPaymentMethod bool       `bun:"verified_payment_method,notnull" json:"verified_payment_method"`
	VerifiedAt            *time.Time `bun:"verified_at,nullzero" json:"verified_at,omitempty"`
	SuspendedAt           *time.Time `bun:"suspended_at,nullzero" json:"suspended_at,omitempty"`
	SuspendReason         *string    `bun:"suspend_reason,nullzero" json:"suspend_reason,omitempty"`
	// Tier is the graduated trust tier earned from cumulative paid spend (#298).
	Tier *string `bun:"tier,nullzero" json:"tier,omitempty"`

	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`
}

// CreditSpendLimit is an optional per-invoker spend cap under a tenant subject
// (issue #237 per_invoker_caps / #246). The invoker_id string is matched against
// credit_transactions.invoker_id (the principal that caused usage), in one of the
// canonical forms: 'serviceToken:<key_id>', 'user:<user_id>', or '<issuer>:<sub>'.
type CreditSpendLimit struct {
	bun.BaseModel `bun:"table:billing.credit_spend_limits,alias:csl"`

	ID                    uuid.UUID `bun:"id,pk,type:uuid" json:"id"`
	TenantID              uuid.UUID `bun:"tenant_id,type:uuid,nullzero" json:"tenant_id"`
	TenantSubjectID       uuid.UUID `bun:"tenant_subject_id,type:uuid,nullzero" json:"tenant_subject_id"`
	CreditTypeID          uuid.UUID `bun:"credit_type_id,notnull,type:uuid" json:"credit_type_id"`
	InvokerID             string    `bun:"invoker_id,notnull" json:"invoker_id"`
	MaxSpendPerDayCents   *int64    `bun:"max_spend_per_day_cents,nullzero" json:"max_spend_per_day_cents,omitempty"`
	MaxSpendPerMonthCents *int64    `bun:"max_spend_per_month_cents,nullzero" json:"max_spend_per_month_cents,omitempty"`
	CreatedAt             time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt             time.Time `bun:"updated_at,notnull" json:"updated_at"`
}
