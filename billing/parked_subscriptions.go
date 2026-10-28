package billing

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// ParkedSubscriptionID names one park of a provider subscription (pks_).
type ParkedSubscriptionID uuid.UUID

const ParkedSubscriptionIDPrefix = "pks_"

func ParseParkedSubscriptionID(s string) (ParkedSubscriptionID, error) {
	u, err := parsePrefixedID("parked subscription", ParkedSubscriptionIDPrefix, s)
	return ParkedSubscriptionID(u), err
}

func (id ParkedSubscriptionID) UUID() uuid.UUID { return uuid.UUID(id) }
func (id ParkedSubscriptionID) IsZero() bool    { return uuid.UUID(id) == uuid.Nil }
func (id ParkedSubscriptionID) String() string {
	return formatPrefixedID(ParkedSubscriptionIDPrefix, uuid.UUID(id))
}
func (id ParkedSubscriptionID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }
func (id *ParkedSubscriptionID) UnmarshalText(text []byte) error {
	parsed, err := ParseParkedSubscriptionID(string(text))
	*id = parsed
	return err
}

// ParkReason is why OpenRails cannot land a live provider subscription.
type ParkReason string

const (
	// ParkPriceUnmapped: the provider price or plan it bills maps to no price.
	ParkPriceUnmapped ParkReason = "price_unmapped"
	// ParkCustomerUnlinked: its provider customer is linked to no customer.
	ParkCustomerUnlinked ParkReason = "customer_unlinked"
	// ParkProductHeld: its customer already holds the product.
	ParkProductHeld ParkReason = "product_held"
)

// ParkRelease is how a park closed.
type ParkRelease string

const (
	// ParkLanded: the subscription landed, by any path.
	ParkLanded ParkRelease = "landed"
	// ParkConverged: converge no longer needs it (a held move landed).
	ParkConverged ParkRelease = "converged"
	// ParkGone: the provider ended the subscription before it landed.
	ParkGone ParkRelease = "gone"
)

// ParkedSubscription is a live provider subscription OpenRails could not land.
// The customer keeps paying the provider; nothing is canceled or refunded.
// Fixing the cause (mapping the price, linking the customer) converges it
// again; Subscription is set once it landed.
type ParkedSubscription struct {
	ID              ParkedSubscriptionID `json:"id"`
	PSP             PSPID                `json:"psp"`
	Rail            string               `json:"rail"`
	SubscriptionRef string               `json:"subscription_ref"`
	Reason          ParkReason           `json:"reason"`
	PriceRef        *string              `json:"price_ref"`
	CustomerRef     *string              `json:"customer_ref"`
	Evidence        json.RawMessage      `json:"evidence"`
	ParkedAt        time.Time            `json:"parked_at"`
	LastSeenAt      time.Time            `json:"last_seen_at"`
	ReleasedAt      *time.Time           `json:"released_at"`
	ReleasedBy      *ParkRelease         `json:"released_by"`
	Subscription    *SubscriptionID      `json:"subscription_id"`
}

// ParkedSubscriptionListParams filters parks, newest first. PSP is a PSP's key
// or id; Released lists closed parks instead of open ones.
type ParkedSubscriptionListParams struct {
	PageRequest
	PSP      string
	Reason   ParkReason
	Released bool
}
