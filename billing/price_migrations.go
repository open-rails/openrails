package billing

import (
	"time"

	"github.com/google/uuid"
)

// PriceMigrationID names one price migration (pmig_).
type PriceMigrationID uuid.UUID

const PriceMigrationIDPrefix = "pmig_"

func ParsePriceMigrationID(s string) (PriceMigrationID, error) {
	u, err := parsePrefixedID("price migration", PriceMigrationIDPrefix, s)
	return PriceMigrationID(u), err
}

func (id PriceMigrationID) UUID() uuid.UUID { return uuid.UUID(id) }
func (id PriceMigrationID) IsZero() bool    { return uuid.UUID(id) == uuid.Nil }
func (id PriceMigrationID) String() string {
	return formatPrefixedID(PriceMigrationIDPrefix, uuid.UUID(id))
}
func (id PriceMigrationID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }
func (id *PriceMigrationID) UnmarshalText(text []byte) error {
	parsed, err := ParsePriceMigrationID(string(text))
	*id = parsed
	return err
}

// ScheduledChangeSource is what scheduled a subscription's change.
type ScheduledChangeSource string

const (
	// ScheduledChangeChange is a tier or seat change waiting for the renewal.
	ScheduledChangeChange ScheduledChangeSource = "change"
	// ScheduledChangeMigration is a price migration's move.
	ScheduledChangeMigration ScheduledChangeSource = "migration"
)

// ScheduledChange is the one change waiting for a subscription's renewal: a
// price, a seat count, or both. It applies at the first renewal on or after
// EffectiveAt and charges nothing before then. DELETE
// /v1/admin/subscriptions/{id}/scheduled-change removes it.
type ScheduledChange struct {
	// PriceID is the price billed from then on; a seat change keeps the
	// subscription's.
	PriceID PriceID `json:"price_id"`
	// Quantity is the seats from then on; null keeps the subscription's.
	Quantity    *int                  `json:"quantity"`
	EffectiveAt time.Time             `json:"effective_at"`
	Source      ScheduledChangeSource `json:"source"`
	// PriceMigrationID is set when Source is migration.
	PriceMigrationID *PriceMigrationID `json:"price_migration_id"`
	CreatedAt        time.Time         `json:"created_at"`
	// Price and Product describe PriceID; the customer's own routes fill them.
	Price   *Price          `json:"price,omitempty"`
	Product *ProductSummary `json:"product,omitempty"`
}

// MigrationFallback is what a price migration does with a subscription
// whose provider cannot be moved from OpenRails (CCBill, Solana, an NMI
// schedule no declared account reaches).
type MigrationFallback string

const (
	// MigrationKeepGrandfathered leaves it billing its old price (default).
	MigrationKeepGrandfathered MigrationFallback = "keep_grandfathered"
	// MigrationCancelAtPeriodEnd records that it should end with its period;
	// OpenRails does not cancel it.
	MigrationCancelAtPeriodEnd MigrationFallback = "cancel_at_period_end"
)

// PriceMigration moves the subscribers of one price, or of every version of
// a price key, to another price. Each subscription moves at its first renewal
// on or after EffectiveAt, carrying the move as its scheduled change until
// then; nothing is prorated or charged early.
type PriceMigration struct {
	ID PriceMigrationID `json:"id"`
	// FromPriceID is the price whose subscribers move; or ProductKey and
	// PriceKey: every version of that price but ToPriceID.
	FromPriceID    *PriceID          `json:"from_price_id"`
	ProductKey     *string           `json:"product_key"`
	PriceKey       *string           `json:"price_key"`
	ToPriceID      PriceID           `json:"to_price_id"`
	EffectiveAt    time.Time         `json:"effective_at"`
	FallbackPolicy MigrationFallback `json:"fallback_policy"`
	// Matched and Skipped are counted at creation (a skipped subscription is
	// not moved); Scheduled, Applied, Canceled and Blocked count the moves now.
	Matched    int        `json:"matched"`
	Skipped    int        `json:"skipped"`
	Scheduled  int        `json:"scheduled"`
	Applied    int        `json:"applied"`
	Canceled   int        `json:"canceled"`
	Blocked    int        `json:"blocked"`
	CreatedAt  time.Time  `json:"created_at"`
	CanceledAt *time.Time `json:"canceled_at"`
}

// CreatePriceMigrationParams names the subscribers to move and where.
type CreatePriceMigrationParams struct {
	// FromPriceID moves one price's subscribers.
	FromPriceID PriceID `json:"from_price_id,omitzero"`
	// ProductKey and PriceKey instead move every version of that price but
	// ToPriceID.
	ProductKey string `json:"product_key,omitempty"`
	PriceKey   string `json:"price_key,omitempty"`
	// ToPriceID is required to create. A preview by key may omit it: it then
	// counts the key's subscribers before the version they move to exists.
	ToPriceID PriceID `json:"to_price_id,omitzero"`
	// EffectiveAt omitted moves each subscription at its next renewal.
	EffectiveAt time.Time `json:"effective_at,omitzero"`
	// AcknowledgeShortNotice moves a subscription whose price rises inside
	// the merchant's notice window; without it that subscription is skipped.
	AcknowledgeShortNotice bool              `json:"acknowledge_short_notice,omitempty"`
	FallbackPolicy         MigrationFallback `json:"fallback_policy,omitempty"`
	// ArchiveSource archives FromPriceID (or the key's current version, when
	// ToPriceID is another price) so it stops selling; default true.
	ArchiveSource *bool `json:"archive_source,omitempty"`
}

// PriceMigrationDisposition is what a migration does with one subscription.
type PriceMigrationDisposition string

const (
	// MigrationScheduled moves it at its renewal.
	MigrationScheduled PriceMigrationDisposition = "scheduled"
	// MigrationApplied already moved it: an NMI schedule bills the new amount
	// from its next rebill once pushed.
	MigrationApplied PriceMigrationDisposition = "applied"
	// MigrationSkipped leaves it; Reason says why.
	MigrationSkipped PriceMigrationDisposition = "skipped"
	// MigrationBlocked leaves it because its provider cannot be moved from
	// OpenRails, or the push failed; Reason says which.
	MigrationBlocked PriceMigrationDisposition = "blocked"
)

// PriceMigrationOutcome is one subscription's disposition.
type PriceMigrationOutcome struct {
	SubscriptionID SubscriptionID            `json:"subscription_id"`
	Rail           string                    `json:"rail"`
	Disposition    PriceMigrationDisposition `json:"disposition"`
	Reason         *string                   `json:"reason"`
}

// PriceMigrationRailCounts is one rail's dispositions: Auto moves at renewal
// (or was pushed), RequiresAction is blocked, Skipped is left.
type PriceMigrationRailCounts struct {
	Auto           int `json:"auto"`
	RequiresAction int `json:"requires_action"`
	Skipped        int `json:"skipped"`
}

// PriceMigrationPreview is what creating the migration would do, with
// nothing written.
type PriceMigrationPreview struct {
	ToPriceID   *PriceID                             `json:"to_price_id"`
	EffectiveAt time.Time                            `json:"effective_at"`
	Matched     int                                  `json:"matched"`
	Scheduled   int                                  `json:"scheduled"`
	Skipped     int                                  `json:"skipped"`
	Blocked     int                                  `json:"blocked"`
	ByRail      map[string]*PriceMigrationRailCounts `json:"by_rail"`
	Outcomes    []PriceMigrationOutcome              `json:"outcomes"`
}

// PriceMigrationCancel reports a canceled migration: Canceled moves still
// scheduled are canceled. A Stripe subscription already carrying the move as
// a Stripe subscription schedule is in RailReleaseRequired: release that
// schedule in Stripe, or the price still changes at period end.
type PriceMigrationCancel struct {
	PriceMigration      PriceMigration   `json:"price_migration"`
	Canceled            int              `json:"canceled"`
	RailReleaseRequired []SubscriptionID `json:"rail_release_required"`
}

// PriceMigrationListParams filters the merchant's migrations, newest first.
type PriceMigrationListParams struct {
	PageRequest
	// IDs instead reads 1 to MaxBatchItems named migrations in one page.
	IDs []PriceMigrationID
	// ProductKey and PriceKey keep the migrations of that price key.
	ProductKey string
	PriceKey   string
}
