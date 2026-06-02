package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// SolanaSubscriptionStatus enumerates the on-chain record lifecycle.
const (
	SolanaSubscriptionActive    = "active"
	SolanaSubscriptionCancelled = "cancelled"
	SolanaSubscriptionExpired   = "expired"
)

// SolanaSubscription is the per-subscriber on-chain state for a recurring Solana
// subscription (issue #255). It links 1:1 to a billing.subscriptions row (the
// canonical lifecycle record) and holds ONLY public on-chain data — never a
// private key. The hourly pull worker (#256) queries due rows by
// (tenant_id, next_pull_at).
type SolanaSubscription struct {
	bun.BaseModel `bun:"table:billing.solana_subscriptions,alias:ss"`

	ID             uuid.UUID `bun:"id,pk,type:uuid" json:"id"`
	TenantID       uuid.UUID `bun:"tenant_id,type:uuid,nullzero" json:"tenant_id"`
	SubscriptionID uuid.UUID `bun:"subscription_id,notnull,type:uuid" json:"subscription_id"`

	SubscriberWallet string `bun:"subscriber_wallet,notnull" json:"subscriber_wallet"`
	AuthorityPDA     string `bun:"authority_pda,notnull" json:"authority_pda"`
	SubscriptionPDA  string `bun:"subscription_pda,notnull" json:"subscription_pda"`
	PlanPDA          string `bun:"plan_pda,notnull" json:"plan_pda"`
	MerchantAddress  string `bun:"merchant_address,notnull" json:"merchant_address"`
	Mint             string `bun:"mint,notnull" json:"mint"`

	// PlanCreatedAtFingerprint is the on-chain plan created_at snapshotted at
	// subscribe time; a mismatch on pull means ghost-plan recreation.
	PlanCreatedAtFingerprint int64 `bun:"plan_created_at_fingerprint,notnull" json:"plan_created_at_fingerprint"`

	LastPulledPeriodStart *time.Time `bun:"last_pulled_period_start,nullzero" json:"last_pulled_period_start,omitempty"`
	LastSignature         *string    `bun:"last_signature,nullzero" json:"last_signature,omitempty"`
	NextPullAt            time.Time  `bun:"next_pull_at,notnull" json:"next_pull_at"`

	Status string `bun:"status,notnull" json:"status"`

	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`
}
