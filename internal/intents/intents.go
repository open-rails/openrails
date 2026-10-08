// Package intents implements the financial authorization and evidence ledger.
// Every accepted provider mutation records immutable terms in provider_intents and
// atomically inserts its own River job. Inline attempts and River dispatch use
// the same claim/submission fences. River wakes one operation at a time; the
// ledger classifies its outcome:
//
//   - succeeded:            done; result_evidence records how
//   - retryable failure:    re-scheduled with the type's backoff
//   - ambiguous:            parked as unknown_needs_verify; the verifier
//     resolves it via provider READS before any retry
//   - terminal failure:     failed_terminal (surfaces in reconcile, #107)
//   - parked:               deliberately not attempted (mode, kill switch,
//     unconfigured client) — stays pending with the reason recorded;
//     the queue drains when the blocker lifts
//
// Failure reasons are recorded on the intent, never raised as errors.
package intents

import (
	"context"
	"encoding/json"
	"time"

	"github.com/open-rails/openrails/internal/db/gen"
)

// Intent statuses (billing.provider_intents.status).
const (
	StatusPending            = "pending"
	StatusInFlight           = "in_flight"
	StatusSucceeded          = "succeeded"
	StatusUnknownNeedsVerify = "unknown_needs_verify"
	StatusFailedRetryable    = "failed_retryable"
	StatusFailedTerminal     = "failed_terminal"
	StatusSuperseded         = "superseded"
	StatusExpired            = "expired"
)

// Origin identifies who wanted the mutation; it gates execution under the
// operating modes (#346): user/admin-origin intents are reactive completions
// and execute under mode=limited, system-origin intents require mode=full,
// and NOTHING executes under mode=readonly.
type Origin string

const (
	OriginUser   Origin = "user"
	OriginAdmin  Origin = "admin"
	OriginSystem Origin = "system"
)

// OutcomeClass classifies one execution (or verification) attempt.
type OutcomeClass int

const (
	// OutcomeSucceeded: the mutation is effectively done.
	OutcomeSucceeded OutcomeClass = iota
	// OutcomeRetryable: the attempt failed CLEANLY (the mutation definitely
	// did not happen), or the provider enforces this operation's idempotency
	// key; retry after the type's backoff. Empty search alone is neither.
	OutcomeRetryable
	// OutcomeAmbiguous: the attempt MAY have happened (transport error after
	// the write was sent, local finalize failure...). Never blind-retried —
	// the verifier resolves via provider reads.
	OutcomeAmbiguous
	// OutcomeTerminal: the handler classified the failure as unretryable.
	OutcomeTerminal
	// OutcomeParked: the attempt was deliberately NOT made (kill switch,
	// provider client unconfigured, read-only client). The intent stays
	// pending with the reason recorded and is re-checked periodically.
	OutcomeParked
)

func (c OutcomeClass) String() string {
	switch c {
	case OutcomeSucceeded:
		return "succeeded"
	case OutcomeRetryable:
		return "retryable"
	case OutcomeAmbiguous:
		return "ambiguous"
	case OutcomeTerminal:
		return "terminal"
	case OutcomeParked:
		return "parked"
	default:
		return "unknown"
	}
}

// Outcome is the classified result of one execution/verification attempt.
type Outcome struct {
	Class    OutcomeClass
	Reason   string
	Evidence map[string]any
}

func Succeeded(evidence map[string]any) Outcome {
	return Outcome{Class: OutcomeSucceeded, Evidence: evidence}
}
func Retryable(reason string) Outcome { return Outcome{Class: OutcomeRetryable, Reason: reason} }

// RecoveryHeld preserves why this operation can be reconsidered promptly when
// provider catch-up completes. It grants no permission to dispatch a mutation.
func RecoveryHeld(reason string) Outcome {
	return Outcome{Class: OutcomeParked, Reason: reason, Evidence: map[string]any{"recovery_held": true}}
}

// IsRecoveryHeld identifies dispatcher waiting, never evidence of provider work.
func IsRecoveryHeld(in gen.BillingProviderIntent) bool {
	var evidence struct {
		Held bool `json:"recovery_held"`
	}
	return json.Unmarshal(in.ResultEvidence, &evidence) == nil && evidence.Held
}
func Ambiguous(reason string) Outcome { return Outcome{Class: OutcomeAmbiguous, Reason: reason} }

// AmbiguousWithEvidence retains an exact provider receipt while local effects
// remain incomplete. The verifier can resume it without depending on search lag.
func AmbiguousWithEvidence(reason string, evidence map[string]any) Outcome {
	return Outcome{Class: OutcomeAmbiguous, Reason: reason, Evidence: evidence}
}

func Terminal(reason string) Outcome { return Outcome{Class: OutcomeTerminal, Reason: reason} }

// TerminalWithEvidence is Terminal plus structured forensics persisted as
// result_evidence (e.g. a gateway decline's response code, which the dunning
// worker classifies hard/soft off the ledger).
func TerminalWithEvidence(reason string, evidence map[string]any) Outcome {
	return Outcome{Class: OutcomeTerminal, Reason: reason, Evidence: evidence}
}
func Parked(reason string) Outcome { return Outcome{Class: OutcomeParked, Reason: reason} }

// Relevance is the result of a type's relevance check: is this intent still
// applicable, or has the world moved on (e.g. the subscription whose delete
// was deferred has been resumed)?
type Relevance struct {
	Applicable bool
	// Reason explains why the intent is superseded when Applicable is false.
	Reason string
}

func StillRelevant() Relevance             { return Relevance{Applicable: true} }
func SupersededBy(reason string) Relevance { return Relevance{Applicable: false, Reason: reason} }

// Handler implements one intent type's semantics. Implementations must be
// safe for concurrent use.
type Handler interface {
	// Type is the registry key (billing.provider_intents.intent_type).
	Type() string
	// CheckRelevance reports whether the intent is still applicable. A
	// returned error keeps the intent pending (re-checked on the next run);
	// Applicable=false marks it superseded.
	CheckRelevance(ctx context.Context, intent gen.BillingProviderIntent) (Relevance, error)
	// Execute performs the provider mutation, honoring per-type
	// effectively-once semantics (deletes verify-then-execute, money movers
	// never blind-retry, ...). Kill switches are checked here, at execution
	// time, and reported as OutcomeParked.
	Execute(ctx context.Context, intent gen.BillingProviderIntent) Outcome
	// Verify resolves an unknown_needs_verify intent using provider READS
	// only. OutcomeRetryable means "verified NOT executed" (the executor may
	// retry); OutcomeAmbiguous means still inconclusive.
	Verify(ctx context.Context, intent gen.BillingProviderIntent) Outcome
	// Backoff returns the delay before the next attempt after the given
	// number of attempts (>= 1).
	Backoff(attempts int32) time.Duration
}

// Registry maps intent_type -> Handler.
type Registry struct {
	handlers map[string]Handler
}

func NewRegistry(handlers ...Handler) *Registry {
	r := &Registry{handlers: make(map[string]Handler, len(handlers))}
	for _, h := range handlers {
		r.Register(h)
	}
	return r
}

// Register adds a handler. Registering two handlers for one type is a wiring
// bug and panics (mirrors river.AddWorker semantics).
func (r *Registry) Register(h Handler) {
	if h == nil {
		panic("intents: Register(nil handler)")
	}
	if _, dup := r.handlers[h.Type()]; dup {
		panic("intents: duplicate handler for intent type " + h.Type())
	}
	r.handlers[h.Type()] = h
}

// Lookup returns the handler for the type, or nil.
func (r *Registry) Lookup(intentType string) Handler {
	if r == nil {
		return nil
	}
	return r.handlers[intentType]
}

// Types returns the registered intent types (unordered).
func (r *Registry) Types() []string {
	out := make([]string, 0, len(r.handlers))
	for t := range r.handlers {
		out = append(out, t)
	}
	return out
}

// EvidenceString reads a captured operation receipt from durable intent state.
func EvidenceString(intent gen.BillingProviderIntent, key string) string {
	var evidence map[string]json.RawMessage
	if json.Unmarshal(intent.ResultEvidence, &evidence) != nil {
		return ""
	}
	var value string
	_ = json.Unmarshal(evidence[key], &value)
	return value
}

// hasSubmissionEvidence is shared by parking and recovery redispatch. A
// durable marker is evidence of possible provider acceptance, never permission
// to infer non-execution from an empty read or to issue another POST.
func hasSubmissionEvidence(in gen.BillingProviderIntent) (bool, error) {
	var evidence map[string]json.RawMessage
	if len(in.ResultEvidence) > 0 {
		if err := json.Unmarshal(in.ResultEvidence, &evidence); err != nil {
			return false, err
		}
	}
	key := "submitted_at"
	switch in.IntentType {
	case "initial_membership":
		key = "initial_submitted"
	case "nmi_sale":
		key = "sale_submitted"
	case "nmi_upgrade":
		// Tier changes fence their one-time proration inside the step. The
		// remaining schedule update must not strand its charge behind a gate.
		if raw := evidence["proration"]; len(raw) > 0 && string(raw) != "null" {
			var step map[string]json.RawMessage
			if err := json.Unmarshal(raw, &step); err != nil {
				return false, err
			}
			evidence = step
		}
	}
	_, found := evidence[key]
	return found, nil
}
