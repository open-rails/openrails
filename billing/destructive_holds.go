package billing

import (
	"time"

	"github.com/google/uuid"
	auth "github.com/open-rails/authkit/auth"
)

// DestructiveHoldID identifies a durable break-glass incident.
type DestructiveHoldID uuid.UUID

const DestructiveHoldIDPrefix = "dsh_"

func ParseDestructiveHoldID(s string) (DestructiveHoldID, error) {
	u, err := parsePrefixedID("destructive hold", DestructiveHoldIDPrefix, s)
	return DestructiveHoldID(u), err
}
func (id DestructiveHoldID) UUID() uuid.UUID { return uuid.UUID(id) }
func (id DestructiveHoldID) IsZero() bool    { return uuid.UUID(id) == uuid.Nil }
func (id DestructiveHoldID) String() string {
	return formatPrefixedID(DestructiveHoldIDPrefix, uuid.UUID(id))
}
func (id DestructiveHoldID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }
func (id *DestructiveHoldID) UnmarshalText(b []byte) error {
	v, err := ParseDestructiveHoldID(string(b))
	if err == nil {
		*id = v
	}
	return err
}

// DestructiveHold is an incident raised by destructive-action admission.
// The merchant identifies where the incident originated; actor/deployment
// scope can hold new admissions at other merchants too.
type DestructiveHold struct {
	ID         DestructiveHoldID `json:"id"`
	MerchantID MerchantID        `json:"merchant_id"`
	Scope      string            `json:"scope"`
	Action     string            `json:"action"`
	Window     string            `json:"window"`
	Observed   int64             `json:"observed"`
	Requested  int64             `json:"requested"`
	Ceiling    int64             `json:"ceiling"`
	OpenedAt   time.Time         `json:"opened_at"`
	ClearedAt  *time.Time        `json:"cleared_at"`
}

// DestructiveHoldListParams pages a merchant's incidents. IDs instead reads 1
// to MaxBatchItems named incidents in one page; unknown ones are absent.
type DestructiveHoldListParams struct {
	PageRequest
	IDs []DestructiveHoldID `form:"ids,comma"`
}

// ClearDestructiveHoldParams records why an authorized operator cleared this episode.
type ClearDestructiveHoldParams struct {
	Reason string `json:"reason"`
}

// DestructiveHoldEvent announces one incident transition, without exposing
// another merchant's actor or target details.
type DestructiveHoldEvent struct {
	HoldID DestructiveHoldID `json:"hold_id"`
	Scope  string            `json:"scope"`
}

// DestructiveHoldReview is an incident with its private evidence: the actor
// whose action opened it, the target, and who cleared it. Only the
// standalone server's operator methods read it, never a merchant route.
type DestructiveHoldReview struct {
	Hold         DestructiveHold `json:"hold"`
	TriggerActor auth.Invoker    `json:"trigger_actor"`
	Target       string          `json:"target"`
	ClearedBy    *auth.Invoker   `json:"cleared_by"`
	ClearReason  string          `json:"clear_reason,omitempty"`
}
