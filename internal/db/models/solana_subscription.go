package models

import (
	"time"

	"github.com/google/uuid"
)

// SolanaSubscriptionStatus enumerates the on-chain record lifecycle.
const (
	SolanaSubscriptionActive    = "active"
	SolanaSubscriptionCancelled = "cancelled"
	SolanaSubscriptionExpired   = "expired"
)

// SolanaSubscription is the per-subscriber on-chain state for a recurring Solana
// subscription (issue #255). It links 1:1 to a openrails.subscriptions row (the
// canonical lifecycle record) and holds ONLY public on-chain data — never a
// private key. The hourly pull worker (#256) queries due rows by
// (tenant_id, next_pull_at).
type SolanaSubscription struct {
	ID             uuid.UUID `json:"id"`
	MerchantID       uuid.UUID `json:"tenant_id"`
	SubscriptionID uuid.UUID `json:"subscription_id"`

	SubscriberWallet string `json:"subscriber_wallet"`
	AuthorityPDA     string `json:"authority_pda"`
	SubscriptionPDA  string `json:"subscription_pda"`
	PlanPDA          string `json:"plan_pda"`
	MerchantAddress  string `json:"merchant_address"`
	Mint             string `json:"mint"`

	// PlanCreatedAtFingerprint is the on-chain plan created_at snapshotted at
	// subscribe time; a mismatch on pull means ghost-plan recreation.
	PlanCreatedAtFingerprint int64 `json:"plan_created_at_fingerprint"`

	LastPulledPeriodStart *time.Time `json:"last_pulled_period_start,omitempty"`
	LastSignature         *string    `json:"last_signature,omitempty"`
	NextPullAt            time.Time  `json:"next_pull_at"`

	Status string `json:"status"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
