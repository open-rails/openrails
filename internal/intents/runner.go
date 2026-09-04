package intents

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/progress"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ledger is the Store surface the Runner drives (interface for unit tests).
type ledger interface {
	Enqueue(ctx context.Context, p EnqueueParams) (gen.OpenrailsRailIntent, error)
	Get(ctx context.Context, id uuid.UUID) (gen.OpenrailsRailIntent, error)
	ClaimByID(ctx context.Context, id uuid.UUID, now, leaseUntil time.Time) (gen.OpenrailsRailIntent, bool, error)
	ClaimDue(ctx context.Context, now, leaseUntil time.Time, batch int64) ([]gen.OpenrailsRailIntent, error)
	ClaimDueVerify(ctx context.Context, now, leaseUntil time.Time, batch int64) ([]gen.OpenrailsRailIntent, error)
	RenewClaim(ctx context.Context, id uuid.UUID, now, leaseUntil time.Time) (bool, error)
	ExpireOverdue(ctx context.Context, now time.Time) (int64, error)
	MarkSucceeded(ctx context.Context, id uuid.UUID, now time.Time, evidence map[string]any) error
	PruneSucceeded(ctx context.Context, id uuid.UUID, evidence map[string]any, keepPayload, keepEvidence bool) error
	PruneTerminalPayload(ctx context.Context, id uuid.UUID) error
	MarkFailedRetryable(ctx context.Context, id uuid.UUID, nextAttemptAt time.Time, reason string) error
	MarkUnknown(ctx context.Context, id uuid.UUID, nextAttemptAt time.Time, reason string) error
	MarkFailedTerminal(ctx context.Context, id uuid.UUID, reason string, evidence map[string]any) error
	Park(ctx context.Context, id uuid.UUID, nextAttemptAt time.Time, reason string) error
	MarkSuperseded(ctx context.Context, id uuid.UUID, reason string) error
}

const (
	// DefaultLease is the silence an executor is allowed before its claim is
	// treated as abandoned. It is NOT how long a handler may run: the claim is
	// renewed every lease/4 for as long as the handler is executing
	// (renewClaimWhile, xs-007 row 32), so a lapsed lease means an executor
	// that stopped beating — dead, partitioned, or wedged — never a refund or
	// charge that is simply taking its time at the provider. Before the beat,
	// a refund still in flight at t=2m was handed to a second executor with
	// only the per-type verify-before-write between that and a double move.
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
	// Config gates execution by origin x operating mode. nil FAILS CLOSED
	// (or#865): every intent parks rather than executing, because a runner that
	// cannot tell which mode it is in must not attempt a provider write. A
	// missing Config is a wiring bug, not a test convenience.
	Config ModeView
	// Breaker halts destructive intent execution on merchant-level volume
	// anomalies (#679). nil = ungated (unit tests, non-destructive-only runners).
	Breaker *VolumeBreaker
	// Destructive is the #836 DB-backed operator kill switch, checked before
	// every destructive intent so an operator can halt in-flight provider
	// deletes with one UPDATE instead of a deploy. Same convention as Breaker:
	// nil = ungated (unit tests); production wiring always sets it.
	Destructive DestructiveGate
	Clock       clockwork.Clock
	Lease       time.Duration
	Batch       int64
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

// Add folds one merchant's pass into the deployment-wide totals the executor
// and verifier workers log (or#862: a pass is now per-merchant).
func (s *Stats) Add(o Stats) {
	s.Claimed += o.Claimed
	s.Succeeded += o.Succeeded
	s.Retryable += o.Retryable
	s.Unknown += o.Unknown
	s.Terminal += o.Terminal
	s.Parked += o.Parked
	s.Superseded += o.Superseded
	s.Expired += o.Expired
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
		progress.Mark(ctx, "intent execute "+intent.ID.String())
		r.executeOne(ctx, intent, &stats)
	}
	return stats, nil
}

func (r *Runner) executeOne(ctx context.Context, intent gen.OpenrailsRailIntent, stats *Stats) {
	logEntry := log.WithContext(ctx).WithFields(log.Fields{
		"intent_id":   intent.ID,
		"intent_type": intent.IntentType,
		"provider":    intent.Rail,
		"origin":      intent.Origin,
		"attempts":    intent.Attempts,
	})
	now := r.now()

	// Pin the intent's merchant so handler execution (and any merchant-scoped DB
	// write it triggers, e.g. membership renewal) resolves it (#336).
	ctx = merchant.WithID(ctx, merchant.ID(intent.MerchantID))
	// or#893: and its PSP. The intent row records the account this write is
	// addressed to, so every mirror row the handler creates — the charge, the
	// subscription, the vaulted method — inherits that provenance instead of
	// having to re-resolve (or fail to).
	ctx = pinIntentAddress(ctx, intent)

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

	// #836 kill switch: an operator can halt every destructive provider write
	// on every node with one UPDATE. Parks (never fails) so the intent resumes
	// the moment the switch is flipped back.
	if r.Destructive != nil && IsDestructiveIntentType(intent.IntentType) {
		if allowed, reason := r.Destructive.AllowDestructive(ctx, intent.MerchantID); !allowed {
			r.park(ctx, logEntry, stats, intent.ID, now, reason)
			return
		}
	}

	// #679 volume breaker: destructive types park (stay pending) while the
	// merchant is over its rolling execution budget or an operator finding is
	// open. Fails closed — a breaker error parks rather than executing unexamined.
	if r.Breaker != nil && IsDestructiveIntentType(intent.IntentType) {
		held, reason, err := r.Breaker.Check(ctx, intent, now)
		if err != nil {
			r.park(ctx, logEntry, stats, intent.ID, now, "destructive-volume breaker check failed: "+err.Error())
			return
		}
		if held {
			r.park(ctx, logEntry, stats, intent.ID, now, reason)
			return
		}
	}

	if err := r.logExternalMutation(ctx, intent, MutationLogPhaseAttempting, "", nil); err != nil {
		r.park(ctx, logEntry, stats, intent.ID, now, "mutation log unavailable: "+err.Error())
		return
	}
	stopBeat := r.renewClaimWhile(ctx, logEntry, intent.ID)
	outcome := handler.Execute(ctx, intent)
	stopBeat()
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
func (r *Runner) EnqueueAndExecute(ctx context.Context, p EnqueueParams) (gen.OpenrailsRailIntent, error) {
	row, err := r.Store.Enqueue(ctx, p)
	if err != nil {
		return gen.OpenrailsRailIntent{}, err
	}
	return r.ExecuteByID(ctx, row.ID)
}

// ExecuteByID runs committed work without enqueuing or changing its payload.
func (r *Runner) ExecuteByID(ctx context.Context, id uuid.UUID) (gen.OpenrailsRailIntent, error) {
	row, err := r.Store.Get(ctx, id)
	if err != nil {
		return gen.OpenrailsRailIntent{}, err
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
		return gen.OpenrailsRailIntent{}, err
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
		progress.Mark(ctx, "intent verify "+intent.ID.String())
		// Pin the intent's merchant for merchant-scoped verify/repair writes
		// (#336) and its PSP for their provenance (or#893) — a verifier's repair
		// writes the same mirror rows the executor would have.
		ctx := merchant.WithID(ctx, merchant.ID(intent.MerchantID))
		ctx = pinIntentAddress(ctx, intent)
		logEntry := log.WithContext(ctx).WithFields(log.Fields{
			"intent_id":   intent.ID,
			"intent_type": intent.IntentType,
			"provider":    intent.Rail,
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
		stopBeat := r.renewClaimWhile(ctx, logEntry, intent.ID)
		outcome := handler.Verify(ctx, intent)
		stopBeat()
		r.apply(ctx, logEntry, &stats, handler, intent, outcome, true)
	}
	return stats, nil
}

// renewClaimWhile beats the intent's lease every lease/4 until the returned
// stop is called — the same shape as the webhook pending-lease heartbeat
// (#678). A beat that finds the lease already lapsed is logged loudly: the
// handler keeps running (a provider call cannot be un-made), and the outcome
// it produces is still recorded — the per-type verify-before-write of whoever
// claimed the row next is the remaining guard, exactly as before the beat.
// Renewal failures (DB unreachable) are logged and retried on the next beat.
func (r *Runner) renewClaimWhile(ctx context.Context, logEntry *log.Entry, id uuid.UUID) func() {
	lease := r.lease()
	hbCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := r.newTicker(lease / 4)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.Chan():
			}
			now := r.now()
			renewed, err := r.Store.RenewClaim(hbCtx, id, now, now.Add(lease))
			switch {
			case err != nil && hbCtx.Err() == nil:
				logEntry.WithError(err).Warn("intent lease renewal failed; retrying next beat")
			case err == nil && !renewed:
				logEntry.Error("intent lease lapsed while the handler was still running; another executor may now hold it (its verify-before-write is the guard)")
				return
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func (r *Runner) newTicker(d time.Duration) clockwork.Ticker {
	if r.Clock != nil {
		return r.Clock.NewTicker(d)
	}
	return clockwork.NewRealClock().NewTicker(d)
}

// apply writes one classified outcome back to the ledger. verifying selects
// the verifier's interpretation of OutcomeAmbiguous (still inconclusive ->
// backoff the next verify) vs the executor's (fresh ambiguity -> first verify
// soon).
func (r *Runner) apply(ctx context.Context, logEntry *log.Entry, stats *Stats, handler Handler, intent gen.OpenrailsRailIntent, outcome Outcome, verifying bool) {
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
		if err == nil && pruneTerminalPayloadFor(handler) {
			if perr := r.Store.PruneTerminalPayload(ctx, intent.ID); perr != nil {
				logEntry.WithError(perr).Warn("intent ledger: terminal payload prune failed")
			}
		}
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

// terminalPayloadPruner is implemented by handlers whose live payload contains
// a short-lived credential that must not remain after terminal classification.
type terminalPayloadPruner interface {
	PruneTerminalPayload() bool
}

// prunePolicyFor resolves the handler's prune policy, defaulting to the
// aggressive slim (drop payload, slim evidence to the pointer keys).
func prunePolicyFor(handler Handler) (keepPayload, keepEvidence bool) {
	if pp, ok := handler.(prunePolicy); ok {
		return pp.PrunePolicy()
	}
	return false, false
}

func pruneTerminalPayloadFor(handler Handler) bool {
	policy, ok := handler.(terminalPayloadPruner)
	return ok && policy.PruneTerminalPayload()
}

func (r *Runner) park(ctx context.Context, logEntry *log.Entry, stats *Stats, id uuid.UUID, now time.Time, reason string) {
	if err := r.Store.Park(ctx, id, now.Add(ParkRetryInterval), reason); err != nil {
		logEntry.WithError(err).Error("intent ledger: park failed; lease expiry will re-surface the intent")
		return
	}
	stats.Parked++
	logEntry.WithField("reason", reason).Warn("intent parked (stays pending)")
}

func (r *Runner) logExternalMutation(ctx context.Context, intent gen.OpenrailsRailIntent, phase MutationLogPhase, reason string, evidence map[string]any) error {
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
		MerchantID:       intent.MerchantID,
		Provider:         intent.Rail,
		PspID:            derefUUID(intent.PspID),
		CustodianID:      derefUUID(intent.CustodianID),
		ProviderIntentID: &intentID,
		IntentType:       intent.IntentType,
		IdempotencyKey:   intent.IdempotencyKey,
		Attempt:          intent.Attempts,
		Phase:            phase,
		Reason:           reason,
		Evidence:         mutationLogEvidence(intent, evidence),
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

func mutationLogEvidence(intent gen.OpenrailsRailIntent, evidence map[string]any) map[string]any {
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

// DestructiveGate is the #836 operator kill switch as the runner needs it.
// internal/destructive.Gate implements it; the indirection keeps intents free
// of a database dependency it does not otherwise have.
type DestructiveGate interface {
	// AllowDestructive reports whether destructive provider writes may execute
	// for a merchant. It must FAIL CLOSED: an unreadable policy denies.
	AllowDestructive(ctx context.Context, merchantID uuid.UUID) (bool, string)
}

// pinIntentAddress puts the account the intent is addressed to on the context,
// so every mirror row a handler or verifier writes inherits the provenance the
// intent row already recorded instead of re-resolving it (or#893). An intent
// names a PSP, a custodian, or — for a custodian-proxy write — both;
// rail_intents_addressed guarantees at least one.
func pinIntentAddress(ctx context.Context, intent gen.OpenrailsRailIntent) context.Context {
	ctx = db.WithPSPID(ctx, derefUUID(intent.PspID))
	return db.WithCustodianID(ctx, derefUUID(intent.CustodianID))
}

func derefUUID(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}
