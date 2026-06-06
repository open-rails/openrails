package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// TierPolicy is a per-payer, per-tier THROUGHPUT policy for the admission check
// (issue #298). The money axis stays in CreditAccountSettings; this holds the
// rate-limit windows. Rolling money-budget windows (#304) extend this.
type TierPolicy struct {
	bun.BaseModel `bun:"table:billing.tier_policies,alias:tp"`

	ID       uuid.UUID `bun:"id,pk,type:uuid" json:"id"`
	TenantID uuid.UUID `bun:"tenant_id,type:uuid,nullzero" json:"tenant_id"`
	// TenantSubjectID the policy belongs to. The all-zero uuid is the reserved sentinel
	// for tenant-wide defaults; a real payer id scopes policy to that payer.
	TenantSubjectID uuid.UUID `bun:"tenant_subject_id,type:uuid,nullzero" json:"tenant_subject_id"`
	// Tier name (e.g. "free", "tier_1"); the policy applies to invokers at this tier.
	Tier string `bun:"tier,notnull" json:"tier"`
	// Policy is the throughput windows: {"windows":[{"unit","window_seconds","max"}]}.
	Policy        ThroughputPolicy `bun:"policy,type:jsonb,nullzero" json:"policy"`
	PolicyVersion int64            `bun:"policy_version,notnull" json:"policy_version"`
	CreatedAt     time.Time        `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt     time.Time        `bun:"updated_at,notnull" json:"updated_at"`
}

// ThroughputPolicy is the JSONB-stored tier policy: fixed-window throughput
// limits, the set of endpoints/models this tier may call (empty = all allowed),
// and rolling money-budget windows (#304, e.g. $2/4h, $5/week). Endpoint gating
// denies a request whose model is not entitled (#298).
type ThroughputPolicy struct {
	Windows           []ThroughputWindow   `json:"windows"`
	EntitledEndpoints []string             `json:"entitled_endpoints,omitempty"`
	BudgetWindows     []BudgetWindowPolicy `json:"budget_windows,omitempty"`
}

// BudgetWindowPolicy is one rolling money-budget window for a tier (#304): at
// most LimitMillicents (the ledger's smallest unit) of spend per WindowSeconds.
type BudgetWindowPolicy struct {
	Key             string `json:"key"`
	WindowSeconds   int64  `json:"window_seconds"`
	LimitMillicents int64  `json:"limit_millicents"`
}

// ThroughputWindow is one limit: at most Max units of Unit per WindowSeconds.
type ThroughputWindow struct {
	Unit          string `json:"unit"`
	WindowSeconds int64  `json:"window_seconds"`
	Max           int64  `json:"max"`
}
