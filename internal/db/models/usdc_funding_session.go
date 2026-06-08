package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

type USDCFundingSessionStatus string

const (
	USDCFundingSessionCreated           USDCFundingSessionStatus = "created"
	USDCFundingSessionOpened            USDCFundingSessionStatus = "opened"
	USDCFundingSessionPendingProvider   USDCFundingSessionStatus = "pending_provider"
	USDCFundingSessionPendingSettlement USDCFundingSessionStatus = "pending_settlement"
	USDCFundingSessionFunded            USDCFundingSessionStatus = "funded"
	USDCFundingSessionFailed            USDCFundingSessionStatus = "failed"
	USDCFundingSessionExpired           USDCFundingSessionStatus = "expired"
	USDCFundingSessionCancelled         USDCFundingSessionStatus = "cancelled"
)

type USDCFundingSession struct {
	bun.BaseModel `bun:"table:billing.usdc_funding_sessions,alias:ufs"`

	ID                uuid.UUID                `bun:"id,pk,type:uuid" json:"id"`
	TenantID          uuid.UUID                `bun:"tenant_id,type:uuid,nullzero" json:"tenant_id"`
	TenantSubjectID   uuid.UUID                `bun:"tenant_subject_id,type:uuid,notnull" json:"tenant_subject_id"`
	CheckoutSessionID *uuid.UUID               `bun:"checkout_session_id,type:uuid,nullzero" json:"checkout_session_id,omitempty"`
	Provider          string                   `bun:"provider,notnull" json:"provider"`
	WalletAddress     string                   `bun:"wallet_address,notnull" json:"wallet_address"`
	Asset             string                   `bun:"asset,notnull" json:"asset"`
	Network           string                   `bun:"network,notnull" json:"network"`
	RequestedAmount   string                   `bun:"requested_amount,notnull" json:"requested_amount"`
	ProviderSessionID *string                  `bun:"provider_session_id,nullzero" json:"provider_session_id,omitempty"`
	ProviderURL       string                   `bun:"provider_url,notnull" json:"provider_url"`
	Status            USDCFundingSessionStatus `bun:"status,notnull" json:"status"`
	ReturnURL         *string                  `bun:"return_url,nullzero" json:"return_url,omitempty"`
	IdempotencyKey    *string                  `bun:"idempotency_key,nullzero" json:"idempotency_key,omitempty"`
	Metadata          map[string]any           `bun:"metadata,type:jsonb,notnull" json:"metadata,omitempty"`
	LastCheckedAt     *time.Time               `bun:"last_checked_at,nullzero" json:"last_checked_at,omitempty"`
	ExpiresAt         *time.Time               `bun:"expires_at,nullzero" json:"expires_at,omitempty"`
	CreatedAt         time.Time                `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt         time.Time                `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
}
