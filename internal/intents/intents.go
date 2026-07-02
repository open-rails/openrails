// Package intents implements the provider intent ledger (#358 phase A): the
// durable, effectively-once outbox for ALL outbound provider mutations.
//
// Every action OpenRails wants to perform against an external payment provider
// is enqueued as a openrails.rail_intents row (idempotent per merchant on
// idempotency_key). A scheduled executor (the Runner, driven by a River
// worker) claims due intents under a SKIP LOCKED lease, checks per-type
// relevance, gates execution on operating mode x origin, executes via the
// type's registered handler and classifies the outcome:
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
	"time"

	"github.com/open-rails/openrails/internal/db/gen"
)

// Intent statuses (openrails.rail_intents.status).
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
	// did not happen); retry after the type's backoff.
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
func Ambiguous(reason string) Outcome { return Outcome{Class: OutcomeAmbiguous, Reason: reason} }
func Terminal(reason string) Outcome  { return Outcome{Class: OutcomeTerminal, Reason: reason} }

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
	// Type is the registry key (openrails.rail_intents.intent_type).
	Type() string
	// CheckRelevance reports whether the intent is still applicable. A
	// returned error keeps the intent pending (re-checked on the next run);
	// Applicable=false marks it superseded.
	CheckRelevance(ctx context.Context, intent gen.OpenrailsRailIntent) (Relevance, error)
	// Execute performs the provider mutation, honoring per-type
	// effectively-once semantics (deletes verify-then-execute, money movers
	// never blind-retry, ...). Kill switches are checked here, at execution
	// time, and reported as OutcomeParked.
	Execute(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome
	// Verify resolves an unknown_needs_verify intent using provider READS
	// only. OutcomeRetryable means "verified NOT executed" (the executor may
	// retry); OutcomeAmbiguous means still inconclusive.
	Verify(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome
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
