package intents

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ledger is the Store surface the Runner drives (interface for unit tests).
type ledger interface {
	Enqueue(ctx context.Context, p EnqueueParams) (gen.OpenrailsProviderIntent, error)
	Get(ctx context.Context, id uuid.UUID) (gen.OpenrailsProviderIntent, error)
	ClaimByID(ctx context.Context, id uuid.UUID, now, leaseUntil time.Time) (gen.OpenrailsProviderIntent, bool, error)
	ClaimDue(ctx context.Context, now, leaseUntil time.Time, batch int64) ([]gen.OpenrailsProviderIntent, error)
	ClaimDueVerify(ctx context.Context, now, leaseUntil time.Time, batch int64) ([]gen.OpenrailsProviderIntent, error)
	ExpireOverdue(ctx context.Context, now time.Time) (int64, error)
	MarkSucceeded(ctx context.Context, id uuid.UUID, now time.Time, evidence map[string]any) error
	PruneSucceeded(ctx context.Context, id uuid.UUID, evidence map[string]any, keepPayload, keepEvidence bool) error
	MarkFailedRetryable(ctx context.Context, id uuid.UUID, nextAttemptAt time.Time, reason string) error
	MarkUnknown(ctx context.Context, id uuid.UUID, nextAttemptAt time.Time, reason string) error
	MarkFailedTerminal(ctx context.Context, id uuid.UUID, reason string, evidence map[string]any) error
	Park(ctx context.Context, id uuid.UUID, nextAttemptAt time.Time, reason string) error
	MarkSuperseded(ctx context.Context, id uuid.UUID, reason string) error
}

const (
	// DefaultLease bounds one claim: an executor that dies mid-batch frees its
	// rows for reclaim after this long.
	DefaultLease = 2 * time.Minute
	// DefaultBatchSize bounds one run's claim.
	DefaultBatchSize = 50
	// ParkRetryInterval is how soon a parked intent (mode gate, kill switch,
	// unconfigured client) is re-checked. Parks are re-evaluated until the
	// blocker lifts or relevance ends; they never escalate backoff.
	ParkRetryInterval = 5 * time.Minute
	// VerifyDelay schedules the verifier's first look at a fresh ambiguous
	// outcome.
	VerifyDelay = time.Minute
)

// Runner drains the intent ledger: RunExecuteOnce is the executor pass,
// RunVerifyOnce the verifier pass. Both are single steps — scheduling is the
// River periodic jobs' concern.
type Runner struct {
	Store    ledger
	Logger   MutationLogger
	Registry *Registry
	// Config gates execution by origin x operating mode. nil (tests) = full.
	Config ModeView
	Clock  clockwork.Clock
	Lease  time.Duration
	Batch  int64
}

func (r *Runner) now() time.Time {
	if r.Clock != nil {
		return r.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *Runner) lease() time.Duration {
	if r.Lease > 0 {
		return r.Lease
	}
	return DefaultLease
}

func (r *Runner) batch() int64 {
	if r.Batch > 0 {
		return r.Batch
	}
	return DefaultBatchSize
}

// Stats summarizes one pass for logging.
type Stats struct {
	Claimed    int
	Succeeded  int
	Retryable  int
	Unknown    int
	Terminal   int
	Parked     int
	Superseded int
	Expired    int64
}

// RunExecuteOnce expires overdue intents, claims due ones and executes them
// through their registered handlers. Intent-level problems are recorded on
// the intent (never returned); the returned error is reserved for
// infrastructure failure (claim query failed).
func (r *Runner) RunExecuteOnce(ctx context.Context) (Stats, error) {
	var stats Stats
	now := r.now()

	expired, err := r.Store.ExpireOverdue(ctx, now)
	if err != nil {
		return stats, err
	}
	stats.Expired = expired

	claimed, err := r.Store.ClaimDue(ctx, now, now.Add(r.lease()), r.batch())
	if err != nil {
		return stats, err
	}
	stats.Claimed = len(claimed)

	for _, intent := range claimed {
		r.executeOne(ctx, intent, &stats)
	}
	return stats, nil
}

func (r *Runner) executeOne(ctx context.Context, intent gen.OpenrailsProviderIntent, stats *Stats) {
	logEntry := log.WithContext(ctx).WithFields(log.Fields{
		"intent_id":   intent.ID,
		"intent_type": intent.IntentType,
		"provider":    intent.Provider,
		"origin":      intent.Origin,
		"attempts":    intent.Attempts,
	})
	now := r.now()

	// Pin the intent's merchant so handler execution (and any merchant-scoped DB
	// write it triggers, e.g. membership renewal) resolves it (#336).
	ctx = merchant.WithID(ctx, merchant.ID(intent.MerchantID))

	handler := r.Registry.Lookup(intent.IntentType)
	if handler == nil {
		// Likely deploy skew (an intent type from a newer/older build). Keep
		// it pending — a build that knows the type will drain it.
		r.park(ctx, logEntry, stats, intent.ID, now, "no handler registered for intent type "+intent.IntentType)
		return
	}

	rel, err := handler.CheckRelevance(ctx, intent)
	if err != nil {
		// Local read failed; not an attempt — re-check soon without burning
		// backoff.
		r.park(ctx, logEntry, stats, intent.ID, now, "relevance check failed: "+err.Error())
		return
	}
	if !rel.Applicable {
		if err := r.Store.MarkSuperseded(ctx, intent.ID, rel.Reason); err != nil {
			logEntry.WithError(err).Error("intent executor: mark superseded failed")
			return
		}
		stats.Superseded++
		logEntry.WithField("reason", rel.Reason).Info("intent superseded (no longer applicable)")
		return
	}

	if blocked, reason := GateExecution(r.Config, Origin(intent.Origin)); blocked {
		r.park(ctx, logEntry, stats, intent.ID, now, reason)
		return
	}

	if err := r.logExternalMutation(ctx, intent, MutationLogPhaseAttempting, "", nil); err != nil {
		r.park(ctx, logEntry, stats, intent.ID, now, "mutation log unavailable: "+err.Error())
		return
	}
	outcome := handler.Execute(ctx, intent)
	if err := r.logExternalMutation(ctx, intent, mutationLogPhase(outcome), outcome.Reason, outcome.Evidence); err != nil {
		logEntry.WithError(err).Error("intent executor: external mutation result log failed")
	}
	r.apply(ctx, logEntry, stats, handler, intent, outcome, false)
}

// EnqueueAndExecute records the intent and immediately claims + executes THAT
// intent through the identical gate/execute/classify pipeline the scheduled
// executor runs (same lease mechanics, same outcome classification). The
// returned row is the canonical post-execution state; callers branch on its
// Status: succeeded (result_evidence says how), pending (gate/kill-switch
// parked it — the reason is last_failure_reason, NOT an error),
// unknown_needs_verify, failed_*. Anything not finished inline is drained
// later by the scheduled executor/verifier — the caller's process dying
// mid-call loses nothing.
//
// When the idempotency key conflicts with a row that is not claimable
// (succeeded, terminal, mid-lease, expired) the row is returned UNTOUCHED so
// the caller can act on the durable prior outcome (e.g. the dunning worker's
// repair-from-successful-rebill path).
func (r *Runner) EnqueueAndExecute(ctx context.Context, p EnqueueParams) (gen.OpenrailsProviderIntent, error) {
	row, err := r.Store.Enqueue(ctx, p)
	if err != nil {
		return gen.OpenrailsProviderIntent{}, err
	}
	switch row.Status {
	case StatusPending, StatusFailedRetryable:
		// claimable below
	default:
		return row, nil
	}

	now := r.now()
	claimed, ok, err := r.Store.ClaimByID(ctx, row.ID, now, now.Add(r.lease()))
	if err != nil {
		return gen.OpenrailsProviderIntent{}, err
	}
	if !ok {
		// Raced into an unclaimable state (another executor's lease, expiry
		// sweep); the durable row stands and the scheduled pipeline owns it.
		return r.Store.Get(ctx, row.ID)
	}

	var stats Stats
	r.executeOne(ctx, claimed, &stats)
	return r.Store.Get(ctx, row.ID)
}

// RunVerifyOnce claims due unknown_needs_verify intents and resolves them via
// the handlers' read-only Verify.
func (r *Runner) RunVerifyOnce(ctx context.Context) (Stats, error) {
	var stats Stats
	now := r.now()

	claimed, err := r.Store.ClaimDueVerify(ctx, now, now.Add(r.lease()), r.batch())
	if err != nil {
		return stats, err
	}
	stats.Claimed = len(claimed)

	for _, intent := range claimed {
		// Pin the intent's merchant for merchant-scoped verify/repair writes (#336).
		ctx := merchant.WithID(ctx, merchant.ID(intent.MerchantID))
		logEntry := log.WithContext(ctx).WithFields(log.Fields{
			"intent_id":   intent.ID,
			"intent_type": intent.IntentType,
			"provider":    intent.Provider,
		})
		handler := r.Registry.Lookup(intent.IntentType)
		if handler == nil {
			// Leave it unknown; push the next look out.
			if err := r.Store.MarkUnknown(ctx, intent.ID, r.now().Add(ParkRetryInterval), "no handler registered for intent type "+intent.IntentType); err != nil {
				logEntry.WithError(err).Error("intent verifier: mark unknown failed")
			}
			stats.Unknown++
			continue
		}
		rel, err := handler.CheckRelevance(ctx, intent)
		if err == nil && !rel.Applicable {
			if err := r.Store.MarkSuperseded(ctx, intent.ID, rel.Reason); err != nil {
				logEntry.WithError(err).Error("intent verifier: mark superseded failed")
				continue
			}
			stats.Superseded++
			continue
		}
		// Verification is read-only: no mode gate.
		r.apply(ctx, logEntry, &stats, handler, intent, handler.Verify(ctx, intent), true)
	}
	return stats, nil
}

// apply writes one classified outcome back to the ledger. verifying selects
// the verifier's interpretation of OutcomeAmbiguous (still inconclusive ->
// backoff the next verify) vs the executor's (fresh ambiguity -> first verify
// soon).
func (r *Runner) apply(ctx context.Context, logEntry *log.Entry, stats *Stats, handler Handler, intent gen.OpenrailsProviderIntent, outcome Outcome, verifying bool) {
	now := r.now()
	var err error
	switch outcome.Class {
	case OutcomeSucceeded:
		err = r.Store.MarkSucceeded(ctx, intent.ID, now, outcome.Evidence)
		stats.Succeeded++
		logEntry.WithField("evidence", outcome.Evidence).Info("intent succeeded")
		if err == nil {
			// Prune the now-succeeded row to a slim dedupe tombstone (#607):
			// drop the heavy payload and slim the forensic evidence, retaining
			// only what post-success readers need (per the handler's
			// PrunePolicy). The row survives — it is the effectively-once guard.
			// A prune failure is non-fatal (the full row stands; dedupe intact).
			keepPayload, keepEvidence := prunePolicyFor(handler)
			if perr := r.Store.PruneSucceeded(ctx, intent.ID, outcome.Evidence, keepPayload, keepEvidence); perr != nil {
				logEntry.WithError(perr).Warn("intent ledger: prune of succeeded intent failed; tombstone retains full payload (dedupe intact)")
			}
		}
	case OutcomeRetryable:
		err = r.Store.MarkFailedRetryable(ctx, intent.ID, now.Add(handler.Backoff(intent.Attempts)), outcome.Reason)
		stats.Retryable++
		logEntry.WithField("reason", outcome.Reason).Warn("intent attempt failed; will retry")
	case OutcomeAmbiguous:
		delay := VerifyDelay
		if verifying {
			delay = handler.Backoff(intent.Attempts)
		}
		err = r.Store.MarkUnknown(ctx, intent.ID, now.Add(delay), outcome.Reason)
		stats.Unknown++
		logEntry.WithField("reason", outcome.Reason).Warn("intent outcome ambiguous; verifier will resolve via provider reads")
	case OutcomeTerminal:
		err = r.Store.MarkFailedTerminal(ctx, intent.ID, outcome.Reason, outcome.Evidence)
		stats.Terminal++
		logEntry.WithField("reason", outcome.Reason).Error("intent failed terminally")
	case OutcomeParked:
		if verifying {
			// A verifier cannot park (reads are never blocked); treat as
			// still-unknown so the intent is not lost.
			err = r.Store.MarkUnknown(ctx, intent.ID, now.Add(ParkRetryInterval), outcome.Reason)
			stats.Unknown++
		} else {
			r.park(ctx, logEntry, stats, intent.ID, now, outcome.Reason)
			return
		}
	default:
		err = r.Store.MarkUnknown(ctx, intent.ID, now.Add(ParkRetryInterval), "unrecognized outcome class")
		stats.Unknown++
	}
	if err != nil {
		logEntry.WithError(err).Error("intent ledger: outcome transition failed; lease expiry will re-surface the intent")
	}
}

// prunePolicy lets a handler keep the heavy columns the prune (#607) would
// otherwise drop from its succeeded tombstone. A handler implements it ONLY
// when something reads the column AFTER success:
//   - keepPayload: the refund producer reads reservation_id off the durable
//     succeeded row to detect a double-refund conflict (admin_payments.go).
//   - keepEvidence: the catalog status view renders verification booleans off
//     succeeded archive/sunset rows (pkg/service/catalog_extras.go).
//
// Handlers whose only post-success reader is the pointer-key path (dunning's
// transaction_id/response_code, the admin operations view) need not implement
// it: the default slims evidence to those keys and drops the payload.
type prunePolicy interface {
	PrunePolicy() (keepPayload, keepEvidence bool)
}

// prunePolicyFor resolves the handler's prune policy, defaulting to the
// aggressive slim (drop payload, slim evidence to the pointer keys).
func prunePolicyFor(handler Handler) (keepPayload, keepEvidence bool) {
	if pp, ok := handler.(prunePolicy); ok {
		return pp.PrunePolicy()
	}
	return false, false
}

func (r *Runner) park(ctx context.Context, logEntry *log.Entry, stats *Stats, id uuid.UUID, now time.Time, reason string) {
	if err := r.Store.Park(ctx, id, now.Add(ParkRetryInterval), reason); err != nil {
		logEntry.WithError(err).Error("intent ledger: park failed; lease expiry will re-surface the intent")
		return
	}
	stats.Parked++
	logEntry.WithField("reason", reason).Warn("intent parked (stays pending)")
}

func (r *Runner) logExternalMutation(ctx context.Context, intent gen.OpenrailsProviderIntent, phase MutationLogPhase, reason string, evidence map[string]any) error {
	intentID := intent.ID
	logger := r.Logger
	if logger == nil {
		if fallback, ok := r.Store.(MutationLogger); ok {
			logger = fallback
		}
	}
	if logger == nil {
		return nil
	}
	return logger.LogExternalMutation(ctx, MutationLogParams{
		MerchantID:        intent.MerchantID,
		Provider:          intent.Provider,
		ProviderAccountID: intent.ProviderAccountID,
		ProviderIntentID:  &intentID,
		IntentType:        intent.IntentType,
		IdempotencyKey:    intent.IdempotencyKey,
		Attempt:           intent.Attempts,
		Phase:             phase,
		Reason:            reason,
		Evidence:          mutationLogEvidence(intent, evidence),
	})
}

func mutationLogPhase(outcome Outcome) MutationLogPhase {
	switch outcome.Class {
	case OutcomeSucceeded:
		return MutationLogPhaseSucceeded
	case OutcomeAmbiguous:
		return MutationLogPhaseUnknown
	case OutcomeParked:
		return MutationLogPhaseParked
	default:
		return MutationLogPhaseFailed
	}
}

func mutationLogEvidence(intent gen.OpenrailsProviderIntent, evidence map[string]any) map[string]any {
	out := map[string]any{}
	if intent.SubscriptionID != nil {
		out["subscription_id"] = intent.SubscriptionID.String()
	}
	if intent.PaymentID != nil {
		out["payment_id"] = intent.PaymentID.String()
	}
	if intent.PriceID != nil {
		out["price_id"] = intent.PriceID.String()
	}
	for k, v := range evidence {
		out[k] = v
	}
	return out
}
