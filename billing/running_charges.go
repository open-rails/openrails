package billing

import (
	"time"

	"github.com/google/uuid"
)

// A running charge bills a customer at a rate per hour from StartedAt until it
// is stopped: round_half_up(RatePerHour x whole seconds / 3600), per second,
// no minimum. Nothing is written while it runs. What it has accrued is taken
// off the customer's available balance on every read (Balance.AccruedAmount),
// and reaches the ledger when the charge stops and before a credit lot it
// draws is expired, revoked or refunded.

// RunningChargeID names one running charge; on the wire "rch_<uuid>".
type RunningChargeID uuid.UUID

const RunningChargeIDPrefix = "rch_"

func ParseRunningChargeID(s string) (RunningChargeID, error) {
	u, err := parsePrefixedID("running charge", RunningChargeIDPrefix, s)
	return RunningChargeID(u), err
}

func (id RunningChargeID) UUID() uuid.UUID { return uuid.UUID(id) }
func (id RunningChargeID) IsZero() bool    { return uuid.UUID(id) == uuid.Nil }
func (id RunningChargeID) String() string {
	return formatPrefixedID(RunningChargeIDPrefix, uuid.UUID(id))
}
func (id RunningChargeID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }
func (id *RunningChargeID) UnmarshalText(text []byte) error {
	parsed, err := ParseRunningChargeID(string(text))
	*id = parsed
	return err
}

type RunningChargeState string

const (
	RunningChargeRunning RunningChargeState = "running"
	RunningChargeStopped RunningChargeState = "stopped"
)

// MaxRunningChargeRatePerHour bounds RatePerHour, in native units.
const MaxRunningChargeRatePerHour = 1_000_000_000_000

// StartRunningChargeParams starts charging a customer RatePerHour, in native
// units of Currency, from StartedAt.
//
// Reference is the host's name for what it charges for: 1 to 255 bytes, unique
// per merchant, and the charge's identity. Starting it again with the same
// terms answers the charge; with a changed term it is ErrIdempotencyKeyReused.
//
// StartedAt is the host's instant, now when nil: at most 60 seconds ahead of
// OpenRails' clock and 35 days behind it.
//
// MinimumAmount above 0 refuses the start (ErrInsufficientCredits, nothing
// written) unless the customer's capacity covers it; 0 never refuses. It is a
// gate for this call, not a term of the charge.
//
// OverdraftAmount is how far below zero the charge lets a prepaid customer's
// capacity reach before the customer counts as depleted; LowBalanceAmount is
// the balance under which it counts as low (0 is no line). Among a customer's
// running charges the smallest overdraft and the largest low line apply.
// Resource is attribution only.
type StartRunningChargeParams struct {
	Reference        string     `json:"reference"`
	CustomerID       CustomerID `json:"customer_id"`
	Currency         string     `json:"currency"`
	RatePerHour      int64      `json:"rate_per_hour,string"`
	StartedAt        *time.Time `json:"started_at,omitempty"`
	MinimumAmount    int64      `json:"minimum_amount,omitempty,string"`
	OverdraftAmount  int64      `json:"overdraft_amount,omitempty,string"`
	LowBalanceAmount int64      `json:"low_balance_amount,omitempty,string"`
	Resource         string     `json:"resource,omitempty"`
}

// StopRunningChargeParams stops a running charge at StoppedAt, the host's
// instant (now when nil): at most 60 seconds ahead of OpenRails' clock, and
// not before the charge started or was last posted. Stopping a stopped charge
// at the same instant, or with none, answers it; at another instant it is
// ErrIdempotencyKeyReused.
type StopRunningChargeParams struct {
	StoppedAt *time.Time `json:"stopped_at,omitempty"`
}

// RunningCharge is one charge that runs, or ran, at a rate. ChargedAmount is
// what it has charged as of AsOf: now while it runs, StoppedAt once stopped,
// when it is final. PostedAmount is the part the ledger holds; the rest is
// held against the customer's balance.
type RunningCharge struct {
	ID               RunningChargeID    `json:"id"`
	Reference        string             `json:"reference"`
	CustomerID       CustomerID         `json:"customer_id"`
	Currency         string             `json:"currency"`
	RatePerHour      int64              `json:"rate_per_hour,string"`
	State            RunningChargeState `json:"state"`
	StartedAt        time.Time          `json:"started_at"`
	StoppedAt        *time.Time         `json:"stopped_at"`
	ChargedAmount    int64              `json:"charged_amount,string"`
	PostedAmount     int64              `json:"posted_amount,string"`
	AsOf             time.Time          `json:"as_of"`
	OverdraftAmount  int64              `json:"overdraft_amount,string"`
	LowBalanceAmount int64              `json:"low_balance_amount,string"`
	Resource         *string            `json:"resource"`
	CreatedAt        time.Time          `json:"created_at"`
	UpdatedAt        time.Time          `json:"updated_at"`
}

// RunningChargeListParams pages the merchant's running charges, newest first:
// of CustomerID and in State when set.
type RunningChargeListParams struct {
	PageRequest
	CustomerID CustomerID         `form:"customer_id"`
	State      RunningChargeState `form:"state"`
}
