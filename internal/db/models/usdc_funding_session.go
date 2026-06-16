package models

import (
	"time"

	"github.com/google/uuid"
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
	ID                uuid.UUID                `json:"id"`
	MerchantID        uuid.UUID                `json:"merchant_id"`
	CustomerID        uuid.UUID                `json:"customer_id"`
	CheckoutSessionID *uuid.UUID               `json:"checkout_session_id,omitempty"`
	Provider          string                   `json:"provider"`
	WalletAddress     string                   `json:"wallet_address"`
	Asset             string                   `json:"asset"`
	Network           string                   `json:"network"`
	RequestedAmount   string                   `json:"requested_amount"`
	ProviderSessionID *string                  `json:"provider_session_id,omitempty"`
	ProviderURL       string                   `json:"provider_url"`
	Status            USDCFundingSessionStatus `json:"status"`
	ReturnURL         *string                  `json:"return_url,omitempty"`
	IdempotencyKey    *string                  `json:"idempotency_key,omitempty"`
	Metadata          map[string]any           `json:"metadata,omitempty"`
	LastCheckedAt     *time.Time               `json:"last_checked_at,omitempty"`
	ExpiresAt         *time.Time               `json:"expires_at,omitempty"`
	CreatedAt         time.Time                `json:"created_at"`
	UpdatedAt         time.Time                `json:"updated_at"`
}
