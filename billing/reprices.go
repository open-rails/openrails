package billing

import (
	"time"

	"github.com/google/uuid"
)

// RepriceID names one subscription's scheduled price change (rep_).
type RepriceID uuid.UUID

// RepriceBatchID names one bulk reprice or plan migration (rpb_).
type RepriceBatchID uuid.UUID

const (
	RepriceIDPrefix      = "rep_"
	RepriceBatchIDPrefix = "rpb_"
)

func ParseRepriceID(s string) (RepriceID, error) {
	u, err := parsePrefixedID("reprice", RepriceIDPrefix, s)
	return RepriceID(u), err
}

func ParseRepriceBatchID(s string) (RepriceBatchID, error) {
	u, err := parsePrefixedID("reprice batch", RepriceBatchIDPrefix, s)
	return RepriceBatchID(u), err
}

func (id RepriceID) UUID() uuid.UUID      { return uuid.UUID(id) }
func (id RepriceID) IsZero() bool         { return uuid.UUID(id) == uuid.Nil }
func (id RepriceID) String() string       { return formatPrefixedID(RepriceIDPrefix, uuid.UUID(id)) }
func (id RepriceBatchID) UUID() uuid.UUID { return uuid.UUID(id) }
func (id RepriceBatchID) IsZero() bool    { return uuid.UUID(id) == uuid.Nil }
func (id RepriceBatchID) String() string {
	return formatPrefixedID(RepriceBatchIDPrefix, uuid.UUID(id))
}
func (id RepriceID) MarshalText() ([]byte, error)      { return []byte(id.String()), nil }
func (id RepriceBatchID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }
func (id *RepriceID) UnmarshalText(text []byte) error {
	parsed, err := ParseRepriceID(string(text))
	*id = parsed
	return err
}
func (id *RepriceBatchID) UnmarshalText(text []byte) error {
	parsed, err := ParseRepriceBatchID(string(text))
	*id = parsed
	return err
}

// RepriceStatus is where a scheduled price change stands.
type RepriceStatus string

const (
	RepriceScheduled RepriceStatus = "scheduled"
	RepriceApplied   RepriceStatus = "applied"
	RepriceCanceled  RepriceStatus = "canceled"
	// RepriceBlocked is a plan-migration member that could not be scheduled
	// (the rail needs the customer, or the rail push failed); BlockedReason
	// says why.
	RepriceBlocked RepriceStatus = "blocked"
)

// RepriceKind separates a same-product price move from a plan change, which
// also moves the subscription to the target price's product.
type RepriceKind string

const (
	RepriceKindReprice    RepriceKind = "reprice"
	RepriceKindPlanChange RepriceKind = "plan_change"
)

// Reprice is one subscription's price change, applied at its first renewal
// on or after EffectiveAt.
type Reprice struct {
	ID                      RepriceID       `json:"id"`
	SubscriptionID          SubscriptionID  `json:"subscription_id"`
	FromPriceID             PriceID         `json:"from_price_id"`
	ToPriceID               PriceID         `json:"to_price_id"`
	EffectiveAt             time.Time       `json:"effective_at"`
	Status                  RepriceStatus   `json:"status"`
	Kind                    RepriceKind     `json:"kind"`
	BlockedReason           *string         `json:"blocked_reason"`
	RepriceBatchID          *RepriceBatchID `json:"reprice_batch_id"`
	AcknowledgedShortNotice bool            `json:"acknowledged_short_notice"`
	CreatedAt               time.Time       `json:"created_at"`
	AppliedAt               *time.Time      `json:"applied_at"`
	CanceledAt              *time.Time      `json:"canceled_at"`
}

// RepriceListParams filters the merchant's reprices, newest first.
type RepriceListParams struct {
	PageRequest
	SubscriptionID SubscriptionID
	RepriceBatchID RepriceBatchID
	Status         RepriceStatus
}

// RepriceBatch is one bulk move: every subscriber on the prior versions of a
// price key (kind reprice), or every subscriber of a retired price (kind
// plan_change, a plan migration). Its reprices carry its id.
//
// Matched subscriptions were either skipped (no reprice) or got a reprice;
// Scheduled, Applied, Canceled and Blocked count those reprices now.
type RepriceBatch struct {
	ID             RepriceBatchID `json:"id"`
	Kind           RepriceKind    `json:"kind"`
	PriceKey       *string        `json:"price_key"`
	SourcePriceID  *PriceID       `json:"source_price_id"`
	ToPriceID      PriceID        `json:"to_price_id"`
	EffectiveAt    time.Time      `json:"effective_at"`
	FallbackPolicy *string        `json:"fallback_policy"`
	Matched        int            `json:"matched"`
	Skipped        int            `json:"skipped"`
	Scheduled      int            `json:"scheduled"`
	Applied        int            `json:"applied"`
	Canceled       int            `json:"canceled"`
	Blocked        int            `json:"blocked"`
	CreatedAt      time.Time      `json:"created_at"`
}

// RepriceBatchListParams filters the merchant's batches, newest first.
type RepriceBatchListParams struct {
	PageRequest
	PriceKey string
}

// CreateRepriceBatchParams moves every active subscription on a prior
// version of PriceKey to the key's current price at EffectiveAt.
type CreateRepriceBatchParams struct {
	PriceKey    string    `json:"price_key"`
	EffectiveAt time.Time `json:"effective_at"`
	// AcknowledgeShortNotice permits a price increase inside the merchant's
	// notice window; without it those subscriptions are skipped.
	AcknowledgeShortNotice bool `json:"acknowledge_short_notice"`
}

// PreviewRepriceBatchParams counts the subscribers a batch for PriceKey would
// move, before the new price version exists.
type PreviewRepriceBatchParams struct {
	PriceKey string `json:"price_key"`
}

// RepriceBatchPreview is what a batch for PriceKey would move: every active
// subscriber on any version of the key.
type RepriceBatchPreview struct {
	PriceKey  string  `json:"price_key"`
	ToPriceID PriceID `json:"to_price_id"`
	Matched   int     `json:"matched"`
}

// RepriceOutcome is one subscription's result within a reprice batch.
type RepriceOutcome struct {
	SubscriptionID SubscriptionID `json:"subscription_id"`
	// RepriceID is null when the subscription was skipped.
	RepriceID *RepriceID `json:"reprice_id"`
	// Reason says why a subscription was skipped.
	Reason *string `json:"reason"`
	// AcknowledgedShortNotice marks an increase scheduled inside the notice
	// window under the batch's AcknowledgeShortNotice.
	AcknowledgedShortNotice bool `json:"acknowledged_short_notice"`
}

// RepriceBatchResult is a created reprice batch: each matched
// subscription's outcome, scheduled or skipped.
type RepriceBatchResult struct {
	BatchID   RepriceBatchID   `json:"batch_id"`
	ToPriceID PriceID          `json:"to_price_id"`
	Matched   int              `json:"matched"`
	Scheduled []RepriceOutcome `json:"scheduled"`
	Skipped   []RepriceOutcome `json:"skipped"`
}

// RepriceBatchCancel reports the batch's scheduled reprices canceled.
// Subscriptions in RailReleaseRequired still carry a Stripe subscription
// schedule that must be released in Stripe.
type RepriceBatchCancel struct {
	Canceled            int              `json:"canceled"`
	RailReleaseRequired []SubscriptionID `json:"rail_release_required"`
	Warning             *string          `json:"warning"`
}

// CreatePlanMigrationParams moves a price's subscribers to a price of another
// product at each subscription's first renewal on or after EffectiveAt.
// Prices are addressed by ID or key.
type CreatePlanMigrationParams struct {
	SourcePrice string `json:"source_price"`
	TargetPrice string `json:"target_price"`
	// EffectiveAt and NoticeDays are mutually exclusive; both empty means now.
	EffectiveAt time.Time `json:"effective_at,omitzero"`
	NoticeDays  int       `json:"notice_days,omitempty"`
	// Immediate also applies access cutover now for auto-migratable
	// subscriptions; nothing is charged until the next invoice.
	Immediate bool `json:"immediate,omitempty"`
	// AcknowledgeShortNotice permits a price increase inside the merchant's
	// notice window.
	AcknowledgeShortNotice bool `json:"acknowledge_short_notice,omitempty"`
	// FallbackPolicy applies to rails that cannot be migrated server-side:
	// keep_grandfathered (default) or cancel_at_period_end.
	FallbackPolicy string `json:"fallback_policy,omitempty"`
	// ArchiveSource defaults to true on create; preview ignores it.
	ArchiveSource *bool `json:"archive_source,omitempty"`
}

// PlanMigrationOutcome classifies one subscription:
// scheduled, applied_immediately, skipped or blocked.
type PlanMigrationOutcome struct {
	SubscriptionID SubscriptionID `json:"subscription_id"`
	RepriceID      *RepriceID     `json:"reprice_id"`
	Rail           string         `json:"rail"`
	Disposition    string         `json:"disposition"`
	Reason         *string        `json:"reason"`
}

// PlanMigrationRailCounts summarizes what each rail can migrate server-side.
type PlanMigrationRailCounts struct {
	Auto           int `json:"auto"`
	RequiresAction int `json:"requires_action"`
	Skipped        int `json:"skipped"`
}

// PlanMigrationResult is returned by preview (BatchID null, nothing written)
// and create.
type PlanMigrationResult struct {
	BatchID        *RepriceBatchID                     `json:"batch_id"`
	SourcePriceID  PriceID                             `json:"source_price_id"`
	TargetPriceID  PriceID                             `json:"target_price_id"`
	EffectiveAt    time.Time                           `json:"effective_at"`
	FallbackPolicy string                              `json:"fallback_policy"`
	Matched        int                                 `json:"matched"`
	Scheduled      int                                 `json:"scheduled"`
	Skipped        int                                 `json:"skipped"`
	Blocked        int                                 `json:"blocked"`
	ByRail         map[string]*PlanMigrationRailCounts `json:"by_rail"`
	Outcomes       []PlanMigrationOutcome              `json:"outcomes"`
	SourceArchived bool                                `json:"source_archived"`
}
