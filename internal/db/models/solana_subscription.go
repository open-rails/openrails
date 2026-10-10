package models

import (
	"time"

	"github.com/google/uuid"
)

// SolanaSubscription statuses: the on-chain record lifecycle.
const (
	SolanaSubscriptionActive   = "active"
	SolanaSubscriptionCanceled = "canceled"
	SolanaSubscriptionExpired  = "expired"
)

// SolanaSubscription is the on-chain state of a recurring Solana
// subscription, 1:1 with its subscriptions row. It holds only public on-chain
// data, never a private key. The pull worker reads due rows by
// (merchant_id, next_pull_at).
type SolanaSubscription struct {
	ID             uuid.UUID `json:"id"`
	MerchantID     uuid.UUID `json:"merchant_id"`
	SubscriptionID uuid.UUID `json:"subscription_id"`

	// PspID is the PSP owning the mirrored subscription, carried on the
	// due-window read so the pull intent names its account. Zero elsewhere.
	PspID uuid.UUID `json:"psp_id,omitempty"`

	SubscriberWallet string `json:"subscriber_wallet"`
	AuthorityPDA     string `json:"authority_pda"`
	SubscriptionPDA  string `json:"subscription_pda"`
	PlanPDA          string `json:"plan_pda"`
	MerchantAddress  string `json:"merchant_address"`
	Mint             string `json:"mint"`

	// PlanCreatedAtFingerprint is the on-chain plan created_at snapshotted at
	// subscribe time; a mismatch on pull means ghost-plan recreation.
	PlanCreatedAtFingerprint int64 `json:"plan_created_at_fingerprint"`

	LastPulledPeriodStartsAt *time.Time `json:"last_pulled_period_starts_at,omitempty"`
	LastSignature            *string    `json:"last_signature,omitempty"`
	NextPullAt               time.Time  `json:"next_pull_at"`

	Status string `json:"status"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
