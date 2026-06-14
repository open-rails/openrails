package models

import (
	"time"

	"github.com/google/uuid"
)

// TierPolicy is a per-payer, per-tier THROUGHPUT policy for the admission check
// (issue #298). The money axis stays in CreditAccountSettings; this holds the
// rate-limit windows. Rolling money-budget windows (#304) extend this.
type TierPolicy struct {
	ID         uuid.UUID `json:"id"`
	MerchantID uuid.UUID `json:"tenant_id"`
	// CustomerID the policy belongs to. The all-zero uuid is the reserved sentinel
	// for tenant-wide defaults; a real payer id scopes policy to that payer.
	CustomerID uuid.UUID `json:"customer_id"`
	// Tier name (e.g. "free", "tier_1"); the policy applies to actors at this tier.
	Tier string `json:"tier"`
	// Policy is the throughput windows: {"windows":[{"unit","window_seconds","max"}]}.
	Policy        ThroughputPolicy `json:"policy"`
	PolicyVersion int64            `json:"policy_version"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

// ThroughputPolicy is the JSONB-stored tier policy: fixed-window throughput
// limits, the set of endpoints/models this tier may call (empty = all allowed),
// and rolling money-budget windows (#304, e.g. $2/4h, $5/week). Endpoint gating
// denies a request whose model is not entitled (#298).
type ThroughputPolicy struct {
	// Windows is the DEFAULT throughput window list, applied to any
	// endpoint-release without a per-release override (#472 G1).
	Windows           []ThroughputWindow   `json:"windows"`
	EntitledResources []string             `json:"entitled_resources,omitempty"`
	BudgetWindows     []BudgetWindowPolicy `json:"budget_windows,omitempty"`

	// ReleaseWindows holds per-(endpoint-release) throughput window VALUES (#472
	// G1): the map key is the endpoint-release uuid (the host's releaseID, never a
	// name). A release present here uses ITS windows; a release absent falls back
	// to the default Windows above. This is how two releases under one payer carry
	// different RPM/TPM/IPM/VPM.
	ReleaseWindows map[string][]ThroughputWindow `json:"release_windows,omitempty"`

	// QueueLimits are the batch/queue reservation-pool caps (#472 G2): each is a
	// {unit, max} pool (NO time window) keyed per (payer, endpoint-release). The
	// admit ACQUIRES the request's units on enqueue and the settle RELEASES them
	// on completion; deny with BlockedBy="queue" when the in-flight sum would
	// exceed max. tensorhub's instance is the count form (unit="request").
	QueueLimits []QueueLimitPolicy `json:"queue_limits,omitempty"`
}

// QueueLimitPolicy is one batch/queue reservation-pool cap (#472 G2): at most
// Max units of Unit may be IN-FLIGHT (reserved-but-not-yet-completed) at once
// per (payer, endpoint-release). Unlike ThroughputWindow this is NOT a time
// window — units are held while a job is pending and released on completion.
type QueueLimitPolicy struct {
	Unit string `json:"unit"` // "request" (count form, weight 1) | "token" | ...
	Max  int64  `json:"max"`
}

// BudgetWindowPolicy is one rolling money-budget window for a tier (#304): at
// most LimitMicros (the ledger's smallest unit) of spend per WindowSeconds.
type BudgetWindowPolicy struct {
	Key           string `json:"key"`
	WindowSeconds int64  `json:"window_seconds"`
	LimitMicros   int64  `json:"limit_micros"`
	// Cadence is "session" (default) or "fixed" (#337 fixed windows).
	Cadence string `json:"cadence,omitempty"`
}

// ThroughputWindow is one limit: at most Max units of Unit per WindowSeconds.
type ThroughputWindow struct {
	Unit          string `json:"unit"`
	WindowSeconds int64  `json:"window_seconds"`
	Max           int64  `json:"max"`
}
