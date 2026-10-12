package destructive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/billingauth"
)

// Kind identifies the admitted command, not its provider's internal effects.
type Kind string

const (
	Refund              Kind = "refund"
	CancelSubscription  Kind = "cancel_subscription"
	DeletePaymentMethod Kind = "delete_payment_method"
	RevokeCredit        Kind = "revoke_credit"
	RevokeAccess        Kind = "revoke_access"
	WaiveOrder          Kind = "waive_order"
	MarkUncollectible   Kind = "mark_uncollectible"
	CatalogChange       Kind = "catalog_change"
	ConfigurationChange Kind = "configuration_change"
	AccountDeletion     Kind = "account_deletion"
)

// Actor is verified command attribution. Credential is audit only; rotation
// never changes the issuer-qualified principal whose allowance is counted.
type Actor struct {
	Issuer     string `json:"issuer"`
	ID         string `json:"id"`
	Credential string `json:"credential,omitempty"`
}

// ActorFromContext reads only an identity admitted by OpenRails' route gate.
// Autonomous work has a separate policy and never borrows a staff identity.
func ActorFromContext(ctx context.Context) (Actor, error) {
	var who billingauth.Identity
	if staff, ok := billingauth.StaffFromContext(ctx); ok {
		who = staff.Identity
	} else if identity, ok := billingauth.IdentityFromContext(ctx); ok {
		who = identity
	} else {
		return Actor{}, errors.New("destructive command has no admitted actor")
	}
	actor := Actor{Issuer: who.Invoker.Issuer, ID: who.Invoker.ID, Credential: billingauth.CredentialName(who)}
	if err := actor.validate(); err != nil {
		return Actor{}, err
	}
	return actor, nil
}
func (a Actor) validate() error {
	if strings.TrimSpace(a.Issuer) == "" || strings.TrimSpace(a.ID) == "" || len(a.Issuer) > 2048 || len(a.ID) > 2048 || len(a.Credential) > 4096 {
		return errors.New("destructive command has invalid actor")
	}
	return nil
}
func (a Actor) key() string { return Digest([]string{a.Issuer, a.ID}) }

// Digest hashes safe canonical request terms. Never pass secrets or retain the
// raw request: only this digest is written to the action audit.
func Digest(terms any) string {
	body, err := json.Marshal(terms)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Command is a validated logical action. Key binds its existing idempotency
// key or target lifecycle revision; a re-cancel after resume uses a new key.
type Command struct {
	MerchantID uuid.UUID
	Kind       Kind
	Target     string
	Key        string
	Digest     string
	Units      int64
	Actor      Actor
}

func (c Command) validate() error {
	if c.MerchantID == uuid.Nil || c.Units < 1 || c.Units > 1000000000 || strings.TrimSpace(c.Key) == "" || len(c.Key) > 2048 || strings.TrimSpace(c.Target) == "" || len(c.Target) > 2048 {
		return errors.New("invalid destructive command")
	}
	b, err := hex.DecodeString(c.Digest)
	if err != nil || len(b) != 32 || c.Digest != strings.ToLower(c.Digest) {
		return errors.New("destructive command requires a SHA256 digest")
	}
	switch c.Kind {
	case Refund, CancelSubscription, DeletePaymentMethod, RevokeCredit, RevokeAccess, WaiveOrder, MarkUncollectible, CatalogChange, ConfigurationChange, AccountDeletion:
	default:
		return fmt.Errorf("unknown destructive command kind %q", c.Kind)
	}
	return c.Actor.validate()
}

type State string

const (
	StateReserved  State = "reserved"
	StateStarted   State = "started"
	StateSucceeded State = "succeeded"
	StateNoEffect  State = "no_effect"
	StateUncertain State = "uncertain"
)

type Outcome string

const (
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeNoEffect  Outcome = "no_effect"
	OutcomeUncertain Outcome = "uncertain"
)

// Action is the durable admission, reused by its provider and local effects.
type Action struct {
	ID         uuid.UUID  `json:"id"`
	MerchantID uuid.UUID  `json:"merchant_id"`
	Kind       Kind       `json:"kind"`
	Target     string     `json:"target"`
	Key        string     `json:"key"`
	Digest     string     `json:"digest"`
	Units      int64      `json:"units"`
	Actor      Actor      `json:"actor"`
	State      State      `json:"state"`
	AdmittedAt time.Time  `json:"admitted_at"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Result     string     `json:"result,omitempty"`
	Replayed   bool       `json:"replayed"`
}

type Scope string

const (
	ScopeActor      Scope = "actor"
	ScopeMerchant   Scope = "merchant"
	ScopeDeployment Scope = "deployment"
)

// Hold is an incident episode. MerchantID is the triggering merchant, not
// the authority boundary of an actor or deployment hold.
type Hold struct {
	ID           uuid.UUID  `json:"id"`
	MerchantID   uuid.UUID  `json:"merchant_id"`
	Scope        Scope      `json:"scope"`
	ScopeKey     string     `json:"scope_key"`
	TriggerActor Actor      `json:"trigger_actor"`
	ActionKind   Kind       `json:"action_kind"`
	Target       string     `json:"target"`
	Window       string     `json:"window"`
	Observed     int64      `json:"observed"`
	Requested    int64      `json:"requested"`
	Limit        int64      `json:"limit"`
	OpenedAt     time.Time  `json:"opened_at"`
	ClearedAt    *time.Time `json:"cleared_at"`
	ClearedBy    *Actor     `json:"cleared_by"`
	ClearReason  string     `json:"clear_reason,omitempty"`
}

// HoldProjector MUST commit the finding, typed event and durable delivery job
// through tx. Returning an error aborts the incident; no external I/O here.
// ClearedAt is nil when opening, set when clearing an episode.
type HoldProjector func(context.Context, pgx.Tx, Hold) error

var (
	ErrHeld        = errors.New("destructive actions held for operator review")
	ErrConflict    = errors.New("destructive action key already names another command")
	ErrActionFinal = errors.New("destructive action is final")
	ErrSelfClear   = errors.New("the triggering actor cannot clear a destructive hold")
)

// HeldError is returned only after an existing or newly created incident is
// durably committed. HTTP maps it to 429; it has no automatic retry deadline.
type HeldError struct{ Hold Hold }

func (e *HeldError) Error() string        { return ErrHeld.Error() }
func (e *HeldError) Is(target error) bool { return target == ErrHeld }
