package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

type CreditType struct {
	bun.BaseModel `bun:"table:billing.credit_types,alias:ct"`

	ID            uuid.UUID `bun:"id,pk,type:uuid" json:"id"`
	Name          string    `bun:"name,notnull,unique" json:"name"`
	DisplayName   string    `bun:"display_name,notnull" json:"display_name"`
	Unit          string    `bun:"unit,notnull" json:"unit"`
	DecimalPlaces int       `bun:"decimal_places,notnull" json:"decimal_places"`
	IsActive      bool      `bun:"is_active,notnull" json:"is_active"`
	CreatedAt     time.Time `bun:"created_at,notnull" json:"created_at"`
}

type UserCreditBalance struct {
	bun.BaseModel `bun:"table:billing.user_credit_balances,alias:ucb"`

	ID uuid.UUID `bun:"id,pk,type:uuid" json:"id"`
	// TenantID scopes this row to a tenant / billing namespace (issue #223).
	TenantID     uuid.UUID `bun:"tenant_id,type:uuid,nullzero" json:"tenant_id"`
	UserID       string    `bun:"user_id,notnull" json:"user_id"`
	CreditTypeID uuid.UUID `bun:"credit_type_id,notnull" json:"credit_type_id"`
	Balance      int64     `bun:"balance,notnull" json:"balance"`
	HeldBalance  int64     `bun:"held_balance,notnull" json:"held_balance"`
	CreatedAt    time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt    time.Time `bun:"updated_at,notnull" json:"updated_at"`
}

type CreditTransaction struct {
	bun.BaseModel `bun:"table:billing.credit_transactions,alias:ctran"`

	ID uuid.UUID `bun:"id,pk,type:uuid" json:"id"`
	// TenantID scopes this row to a tenant / billing namespace (issue #223).
	TenantID        uuid.UUID  `bun:"tenant_id,type:uuid,nullzero" json:"tenant_id"`
	UserID          string     `bun:"user_id,notnull" json:"user_id"`
	CreditTypeID    uuid.UUID  `bun:"credit_type_id,notnull" json:"credit_type_id"`
	Amount          int64      `bun:"amount,notnull" json:"amount"`
	BalanceAfter    *int64     `bun:"balance_after,nullzero" json:"balance_after,omitempty"`
	TransactionType string     `bun:"transaction_type,notnull" json:"transaction_type"`
	Status          string     `bun:"status,notnull" json:"status"`
	Authorized      *int64     `bun:"authorized_amount,nullzero" json:"authorized_amount,omitempty"`
	Captured        *int64     `bun:"captured_amount,nullzero" json:"captured_amount,omitempty"`
	Source          string     `bun:"source,notnull" json:"source"`
	SourceID        *string    `bun:"source_id,nullzero" json:"source_id,omitempty"`
	ExpiresAt       *time.Time `bun:"expires_at,nullzero" json:"expires_at,omitempty"`
	Description     *string    `bun:"description,nullzero" json:"description,omitempty"`
	CreatedAt       time.Time  `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt       time.Time  `bun:"updated_at,notnull" json:"updated_at"`
}

type CreditBlock struct {
	bun.BaseModel `bun:"table:billing.credit_blocks,alias:cb"`

	ID uuid.UUID `bun:"id,pk,type:uuid" json:"id"`
	// TenantID scopes this row to a tenant / billing namespace (issue #223).
	TenantID            uuid.UUID  `bun:"tenant_id,type:uuid,nullzero" json:"tenant_id"`
	UserID              string     `bun:"user_id,notnull" json:"user_id"`
	CreditTypeID        uuid.UUID  `bun:"credit_type_id,notnull" json:"credit_type_id"`
	OriginalAmount      int64      `bun:"original_amount,notnull" json:"original_amount"`
	RemainingAmount     int64      `bun:"remaining_amount,notnull" json:"remaining_amount"`
	ExpiresAt           *time.Time `bun:"expires_at,nullzero" json:"expires_at,omitempty"`
	SourceTransactionID *uuid.UUID `bun:"source_transaction_id,type:uuid,nullzero" json:"source_transaction_id,omitempty"`
	CreatedAt           time.Time  `bun:"created_at,notnull" json:"created_at"`
}

// Note: subscription_credit_grants table was removed in favor of deterministic deposit SourceIDs.
