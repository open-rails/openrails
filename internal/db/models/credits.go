package models

import (
	"time"

	"github.com/google/uuid"
)

type CreditType struct {
	ID            uuid.UUID `json:"id"`
	Name          string    `json:"name"`
	DisplayName   string    `json:"display_name"`
	Unit          string    `json:"unit"`
	DecimalPlaces int       `json:"decimal_places"`
	IsActive      bool      `json:"is_active"`
	CreatedAt     time.Time `json:"created_at"`
}

type CreditBalance struct {
	ID uuid.UUID `json:"id"`
	// MerchantID scopes this row to a tenant / billing namespace (issue #223).
	MerchantID uuid.UUID `json:"tenant_id"`
	// CustomerID is the tenant subject that OWNS this balance / is billed (issue #221,
	// payer/billing payer). Nullable during the additive rollout; defaults to the
	// actor's deterministic personal tenant-subject id. See pkg/identity.CustomerID.
	CustomerID   uuid.UUID `json:"customer_id"`
	CreditTypeID uuid.UUID `json:"credit_type_id"`
	Balance      int64     `json:"balance"`
	HeldBalance  int64     `json:"held_balance"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type CreditTransaction struct {
	ID uuid.UUID `json:"id"`
	// MerchantID scopes this row to a tenant / billing namespace (issue #223).
	MerchantID uuid.UUID `json:"tenant_id"`
	// CustomerID is the tenant subject that OWNS / is billed for this transaction (issue
	// #221, payer/billing payer). See pkg/identity.CustomerID.
	CustomerID uuid.UUID `json:"customer_id"`
	// Actor is the caller-supplied principal string that caused this charge
	// (opaque to OpenRails; also the per-actor spend-cap grouping key).
	Actor string `json:"actor"`
	// Resource is the caller-supplied free-form string for what the charge was
	// for (opaque to OpenRails; e.g. tensorhub endpoint slug, doujins plan slug).
	Resource *string `json:"resource,omitempty"`
	// Metadata is optional caller-supplied long-tail attribution (opaque).
	Metadata        map[string]any `json:"metadata,omitempty"`
	CreditTypeID    uuid.UUID      `json:"credit_type_id"`
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

type CreditBlock struct {
	ID uuid.UUID `json:"id"`
	// MerchantID scopes this row to a tenant / billing namespace (issue #223).
	MerchantID uuid.UUID `json:"tenant_id"`
	// CustomerID is the tenant subject that OWNS this block of credits (issue #221).
	CustomerID          uuid.UUID  `json:"customer_id"`
	CreditTypeID        uuid.UUID  `json:"credit_type_id"`
	OriginalAmount      int64      `json:"original_amount"`
	RemainingAmount     int64      `json:"remaining_amount"`
	ExpiresAt           *time.Time `json:"expires_at,omitempty"`
	SourceTransactionID *uuid.UUID `json:"source_transaction_id,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
}

// CreditWindow is a prepaid credit window (issue #335): one bulk reservation a
// host admits requests against locally. Opening it moves funds into the payer's
// held_balance (hold mechanics); settles decrement held+balance per request;
// close/expiry releases the unsettled remainder. First-class — NOT a reused hold.
type CreditWindow struct {
	ID uuid.UUID `json:"id"`
	// MerchantID scopes this row to a tenant / billing namespace (issue #223).
	MerchantID uuid.UUID `json:"tenant_id"`
	// CustomerID is the payer whose funds this window reserves (issue #221).
	CustomerID   uuid.UUID `json:"customer_id"`
	CreditTypeID uuid.UUID `json:"credit_type_id"`
	// HeldAmount is the total reserved (open + refills); mirrored in held_balance
	// while the window is open.
	HeldAmount int64 `json:"held_amount"`
	// SettledAmount is the sum of settled actuals; settled <= held is enforced.
	SettledAmount int64     `json:"settled_amount"`
	Status        string    `json:"status"` // open | closed | expired
	ExpiresAt     time.Time `json:"expires_at"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Note: subscription_credit_grants table was removed in favor of deterministic deposit SourceIDs.
