package models

import (
	"time"

	"github.com/google/uuid"
)

type CheckoutSessionMode string

const (
	CheckoutSessionModeOneOff       CheckoutSessionMode = "one_off"
	CheckoutSessionModeSubscription CheckoutSessionMode = "subscription"
	// CheckoutSessionModeSolanaCancel and CheckoutSessionModeSolanaTierChange
	// extend the Solana Pay transaction-request machinery to the recurring
	// subscription lifecycle (#272+). A cancel session carries the target
	// subscription_id in ProcessorState; a tier-change session additionally
	// carries new_price_id. The public Solana Pay endpoint builds the unsigned
	// (or cranker-co-signed) on-chain tx with the Solana Pay reference attached,
	// and the reference poller mirrors the confirmed cancel / tier-change into
	// the DB — the same protocol as a checkout/subscribe session, just a
	// different on-chain action.
	CheckoutSessionModeSolanaCancel     CheckoutSessionMode = "solana_cancel"
	CheckoutSessionModeSolanaTierChange CheckoutSessionMode = "solana_tier_change"
)

type CheckoutSessionStatus string

const (
	CheckoutSessionStatusCreated        CheckoutSessionStatus = "created"
	CheckoutSessionStatusRequiresAction CheckoutSessionStatus = "requires_action"
	CheckoutSessionStatusSucceeded      CheckoutSessionStatus = "succeeded"
	CheckoutSessionStatusFailed         CheckoutSessionStatus = "failed"
	CheckoutSessionStatusExpired        CheckoutSessionStatus = "expired"
	CheckoutSessionStatusCanceled       CheckoutSessionStatus = "canceled"
)

type CheckoutSession struct {
	ID uuid.UUID `json:"id"`
	// CustomerID is the OpenRails payable merchant subject for this row (#317).
	// Additive during the hard-cut rollout; writers populate it and readers move to
	// it before user_id is dropped. Join openrails.customers for issuer/subject.
	CustomerID uuid.UUID `json:"customer_id,omitempty"`

	PriceID uuid.UUID           `json:"price_id"`
	Mode    CheckoutSessionMode `json:"mode"`

	Processor Processor             `json:"processor"`
	Status    CheckoutSessionStatus `json:"status"`

	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`

	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Reference *string    `json:"reference,omitempty"`

	TransactionID  *string    `json:"transaction_id,omitempty"`
	PaymentID      *uuid.UUID `json:"payment_id,omitempty"`
	SubscriptionID *uuid.UUID `json:"subscription_id,omitempty"`

	Metadata map[string]string `json:"metadata,omitempty"`

	ProcessorFields map[string]any `json:"processor_fields,omitempty"`
	ProcessorState  map[string]any `json:"processor_state,omitempty"`

	IdempotencyKey *string `json:"idempotency_key,omitempty"`
	// ProviderAccountID is the provider_accounts row selected for this provider
	// checkout. It prevents provider sessions from being confused across rotated
	// Stripe/NMI/CCBill accounts.
	ProviderAccountID *uuid.UUID `json:"provider_account_id,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`

	Price      *Price  `json:"price,omitempty"`
	LastFour   *string `json:"last_four,omitempty"`
	CardType   *string `json:"card_type,omitempty"`
	ExpiryDate *string `json:"expiry_date,omitempty"`
}
