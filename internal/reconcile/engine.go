package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/shared/opsmetric"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Engine is the PULL-plane engine (#107 phase 2, #665 mirror-writer): it
// fetches each provider's declared state, diffs it against the local mirror,
// persists pull.* findings with stable identity, and (in enforce mode) applies
// idempotent LOCAL MIRROR writes plus decider invocations — subscription state
// transitions go through the ONE decider (Decide/DecisionApplier), never a
// bespoke applier. It NEVER mutates a rail — remote actions are findings in
// the admin queue (requires_admin), and the fetchers are read-only by
// construction. Internal-plane checks (derive.*/life.*/consistency.*) belong
// to the Convergence Engine, never here (#665).
//
// The caller must run the engine on a merchant-scoped context (a pinned merchant
// connection / merchant in context) so every read and write is RLS-constrained
// to one merchant; a run executes under exactly one merchant.
type Engine struct {
	Fetchers map[Provider]RailFetcher
	Store    Store
	Local    LocalStateLoader
	// Writer applies enforce-mode local mirror writes. May be nil for
	// advisory-only engines.
	Writer LocalWriter
	// Decisions applies decider transitions (PS-2/PS-3 demotion, #665). May be
	// nil for advisory-only engines.
	Decisions DecisionApplier
	// History is the THIRD dunning-forensics evidence source (Postgres:
	// imported legacy history + failed payments, #735). May be nil /
	// unconfigured: the forensics report then carries a note instead — an
	// unavailable history source is NEVER a run error.
	History HistoryEventSource

	// Notifier bridges persisted findings into the #736 operator notification
	// store (#787). Optional; nil is a no-op (e.g. embedded runtimes with no
	// alerting service wired). Best-effort: a notify failure is logged, never
	// fails the run — the finding is already durably persisted.
	Notifier FindingNotifier

	// Policy reads the merchant's destructive policy — today only the #835
	// evidence-staleness floor. NewEngine wires it; a nil Policy means the
	// decider falls back to trusting ONLY what this pass observed, which is
	// stricter, never more permissive.
	Policy EvidenceFloorReader

	// Runs makes an enforce pass REVERSIBLE (or#859): it opens a
	// destructive_runs record carrying the coverage proof that authorised the
	// pass, captures a before-image of every subscription the pass is about to
	// overwrite, and attributes the provider intents it queues. Enforce passes
	// that would overwrite subscription state REFUSE when it is nil — an
	// unrecorded destructive pass has no undo, and the empty-roster incident is
	// exactly why that is not an acceptable default.
	Runs DestructiveRunRecorder

	// Now is the clock (defaults to time.Now UTC).
	Now func() time.Time

	// Circuit breaker for absence-based PS-2 detection (design decision 6):
	// when the provider reports implausibly few live subscriptions vs local
	// state, ABORT the provider's run instead of generating mass PS-2.
	// Defaults: MinLocal 1 (#837 — no small-merchant blind spot), Ratio 0.10.
	CircuitBreakerMinLocal int
	CircuitBreakerRatio    float64

	// Actor attributes this engine's destructive runs (the operator who ran the
	// CLI, or the worker that scheduled the pass). Empty defaults to
	// "converge-enforce" — an audit trail, not authentication.
	Actor string

	// CancelBudget (#837) caps how many subscriptions ONE pass may cancel for
	// this merchant. Over the cap, the pass applies NOTHING, raises a
	// requires_review finding and halts. Zero value = the defaults.
	CancelBudget CancelBudget
}

// RunParams bounds one reconcile run.
type RunParams struct {
	Mode Mode
	// Mutations, when non-nil, limits enforce-mode local writes to specific
	// mutation classes. Nil preserves the historical "apply every safe local
	// fix" behavior for in-process callers; the operator CLI passes an explicit
	// policy from --insert/--overwrite.
	Mutations *LocalMutationPolicy
	// Providers to reconcile; empty means every wired fetcher.
	Providers []Provider
	// PSPCoverage (#841) declares, per provider, how many PSPs the merchant has
	// active on that rail and how many this pass actually read. A pull arms
	// from ONE PSP, so anything short of complete coverage strips
	// SubscriptionsExhaustive: a roster that saw one of two accounts cannot
	// prove a subscription of the OTHER account is gone.
	PSPCoverage map[Provider]PSPCoverage
	// PSPs optionally binds each provider section to one
	// merchant-scoped provider account. When set, the engine scopes local mirror
	// reads and local materialization writes to that account id.
	PSPs map[Provider]RailMerchantAccountBinding
	// Since/Until bound the transaction window passed to the fetchers.
	Since time.Time
	Until time.Time
}

// LocalMutationPolicy limits pull-provider's local mirror writes by operator
// mutation class. It never permits external provider writes.
type LocalMutationPolicy struct {
	Insert    bool
	Overwrite bool
}

func (p *LocalMutationPolicy) allowsInsert() bool {
	return p == nil || p.Insert
}

func (p *LocalMutationPolicy) allows(f *Finding) bool {
	if p == nil {
		return true
	}
	switch f.Type {
	case FindingRemoteSubMissingLocal, FindingChargeMissingLocal, FindingRefundUnrecorded:
		return p.Insert
	case FindingLocalActiveRemoteDead, FindingStatusMismatch, FindingPaymentMethodMismatch:
		return p.Overwrite
	default:
		return false
	}
}

// RailMerchantAccountBinding is the account row a provider-pull is authorized to
// treat as authoritative. ID is openrails.psps.id; AccountID is
// the raw provider-returned account identifier.
type RailMerchantAccountBinding struct {
	ID        uuid.UUID `json:"id"`
	Rail      string    `json:"rail"`
	AccountID string    `json:"account_id"`
}

// ProviderReport is one provider's section of the run summary.
type ProviderReport struct {
	Provider Provider `json:"provider"`
	PspID    string   `json:"psp_id,omitempty"`
	// DestructiveRunID is set when this provider's enforce pass overwrote
	// subscription state: the handle `openrails undo-run --run <id>`
	// reverses (or#859).
	DestructiveRunID     string            `json:"destructive_run_id,omitempty"`
	Aborted              bool              `json:"aborted,omitempty"`
	Error                string            `json:"error,omitempty"`
	Coverage             SnapshotCoverage  `json:"coverage"`
	RemoteSubscriptions  int               `json:"remote_subscriptions"`
	RemoteTransactions   int               `json:"remote_transactions"`
	RemotePaymentMethods int               `json:"remote_vault_entries"`
	LocalSubscriptions   int               `json:"local_subscriptions"`
	FindingsByType       map[string]int    `json:"findings_by_type,omitempty"`
	FindingsBySeverity   map[string]int    `json:"findings_by_severity,omitempty"`
	NewFindings          int               `json:"new_findings"`
	UpdatedFindings      int               `json:"updated_findings"`
	RequiresReview       int               `json:"requires_review"`
	AdminRequired        int               `json:"-"`
	AutoResolved         int64             `json:"auto_resolved"`
	AutoFixed            int               `json:"auto_fixed"`
	ApplySkipped         int               `json:"apply_skipped,omitempty"`
	ApplyErrors          []string          `json:"apply_errors,omitempty"`
	Dunning              *DunningForensics `json:"dunning_forensics,omitempty"`
}

// RunSummary is the persisted summary jsonb of a run.
type RunSummary struct {
	Mode      Mode                       `json:"mode"`
	Providers map[string]*ProviderReport `json:"providers"`
	Totals    SummaryTotals              `json:"totals"`
}

// SummaryTotals aggregates across providers.
type SummaryTotals struct {
	Findings       int   `json:"findings"`
	RequiresReview int   `json:"requires_review"`
	AdminRequired  int   `json:"-"`
	AutoResolved   int64 `json:"auto_resolved"`
	AutoFixed      int   `json:"auto_fixed"`
}

// RunResult is what a run returns to its caller (CLI / admin API).
type RunResult struct {
	RunID          uuid.UUID        `json:"run_id"`
	Mode           Mode             `json:"mode"`
	Status         string           `json:"status"`
	Summary        *RunSummary      `json:"summary"`
	Findings       []FindingRecord  `json:"findings"`
	PlannedChanges []MutationRecord `json:"planned_changes,omitempty"`
	AppliedChanges []MutationRecord `json:"applied_changes,omitempty"`
}

// MutationRecord is a row-level local change planned or applied during a
// provider pull. It is intentionally derived from the reconcile apply action,
// not persisted; operators get it in the pull-provider log artifact.
type MutationRecord struct {
	Phase        string         `json:"phase"`
	Provider     Provider       `json:"provider"`
	FindingID    uuid.UUID      `json:"finding_id,omitempty"`
	FindingType  FindingType    `json:"finding_type,omitempty"`
	SubjectKey   string         `json:"subject_key,omitempty"`
	Table        string         `json:"table"`
	Operation    string         `json:"operation"`
	RowID        string         `json:"row_id,omitempty"`
	ExternalID   string         `json:"external_id,omitempty"`
	RowsAffected int            `json:"rows_affected,omitempty"`
	Evidence     map[string]any `json:"evidence,omitempty"`
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

func (e *Engine) actor() string {
	if e.Actor != "" {
		return e.Actor
	}
	return "converge-enforce"
}

func (e *Engine) rosterBreaker() RosterBreaker {
	return RosterBreaker{MinLocal: e.CircuitBreakerMinLocal, Ratio: e.CircuitBreakerRatio}
}

// providerTraits captures per-provider diff semantics that the capability
// flags alone cannot express.
type providerTraits struct {
	// absenceMeansCancelled: the provider's subscription listing only contains
	// LIVE subscriptions, so a local-live subscription that is absent remotely
	// is dead at the rail (NMI recurring report). Guarded by the circuit
	// breaker.
	absenceMeansCancelled bool
	// paymentMethodsExhaustive: the vault listing is the complete roster, so a local
	// payment method missing from it no longer exists at the rail.
	paymentMethodsExhaustive bool
}

func traitsFor(p Provider) providerTraits {
	switch p {
	case ProviderNMI:
		return providerTraits{absenceMeansCancelled: true, paymentMethodsExhaustive: true}
	default:
		// CCBill: absence from ACTIVEMEMBERS is inactive-or-out-of-window, NOT
		// proof of termination — only CANCELLATION/EXPIRE rows assert death.
		// Stripe/Solana list terminal statuses explicitly.
		return providerTraits{}
	}
}

// Run executes one reconcile run: fetch + diff (+ apply in enforce mode) for
// every requested provider, persisting findings and the run summary.
func (e *Engine) Run(ctx context.Context, params RunParams) (*RunResult, error) {
	if params.Mode != ModeAdvisory && params.Mode != ModeEnforce {
		return nil, fmt.Errorf("reconcile: invalid mode %q", params.Mode)
	}
	if params.Mode == ModeEnforce && e.Writer == nil {
		return nil, fmt.Errorf("reconcile: enforce mode requires a LocalWriter")
	}

	providers := params.Providers
	if len(providers) == 0 {
		for p := range e.Fetchers {
			providers = append(providers, p)
		}
		sort.Slice(providers, func(i, j int) bool { return providers[i] < providers[j] })
	}
	for _, p := range providers {
		if _, ok := e.Fetchers[p]; !ok {
			return nil, fmt.Errorf("reconcile: no fetcher wired for provider %q", p)
		}
	}

	var sincePtr, untilPtr *time.Time
	if !params.Since.IsZero() {
		t := params.Since.UTC()
		sincePtr = &t
	}
	if !params.Until.IsZero() {
		t := params.Until.UTC()
		untilPtr = &t
	}

	runID, err := e.Store.CreateRun(ctx, params.Mode, providers, sincePtr, untilPtr)
	if err != nil {
		return nil, fmt.Errorf("reconcile: create run: %w", err)
	}

	summary := &RunSummary{Mode: params.Mode, Providers: map[string]*ProviderReport{}}
	result := &RunResult{RunID: runID, Mode: params.Mode, Summary: summary}
	var providerErrs []string

	for _, p := range providers {
		rep, records, planned, applied, perr := e.runProvider(ctx, runID, p, params)
		summary.Providers[string(p)] = rep
		result.Findings = append(result.Findings, records...)
		result.PlannedChanges = append(result.PlannedChanges, planned...)
		result.AppliedChanges = append(result.AppliedChanges, applied...)
		if perr != nil {
			rep.Error = perr.Error()
			providerErrs = append(providerErrs, fmt.Sprintf("%s: %v", p, perr))
			log.WithError(perr).WithField("provider", p).Error("reconcile: provider run failed")
		}
		summary.Totals.Findings += rep.NewFindings + rep.UpdatedFindings
		summary.Totals.RequiresReview += rep.RequiresReview
		summary.Totals.AdminRequired += rep.AdminRequired
		summary.Totals.AutoResolved += rep.AutoResolved
		summary.Totals.AutoFixed += rep.AutoFixed
	}

	status := "completed"
	runErr := ""
	if len(providerErrs) > 0 {
		status = "failed"
		runErr = strings.Join(providerErrs, "; ")
	}
	result.Status = status

	summaryJSON, err := json.Marshal(summary)
	if err != nil {
		summaryJSON = nil
	}
	if err := e.Store.FinishRun(ctx, runID, status, summaryJSON, runErr); err != nil {
		return result, fmt.Errorf("reconcile: finish run: %w", err)
	}
	if runErr != "" {
		return result, fmt.Errorf("reconcile run %s finished with provider errors: %s", runID, runErr)
	}
	return result, nil
}

// EvidenceFloorReader yields a merchant's #835 evidence-staleness floor.
// internal/destructive.Gate implements it.
type EvidenceFloorReader interface {
	EvidenceFloor(ctx context.Context, merchantID uuid.UUID) time.Time
}

// evidenceFloor reads the running merchant's staleness floor off the run's own
// merchant-scoped context, so the pull path cannot be run without it.
func (e *Engine) evidenceFloor(ctx context.Context) time.Time {
	if e.Policy == nil {
		return time.Time{}
	}
	mid, ok := merchant.FromContext(ctx)
	if !ok {
		return time.Time{}
	}
	return e.Policy.EvidenceFloor(ctx, mid.UUID())
}

// runProvider fetches, diffs, persists, applies (enforce), and auto-resolves
// for one provider. A returned error means the provider's reconciliation did
// not complete (e.g. fetch failure or circuit-breaker abort) and NOTHING was
// persisted or fixed for it.
func (e *Engine) runProvider(ctx context.Context, runID uuid.UUID, provider Provider, params RunParams) (*ProviderReport, []FindingRecord, []MutationRecord, []MutationRecord, error) {
	rep := &ProviderReport{
		Provider:           provider,
		FindingsByType:     map[string]int{},
		FindingsBySeverity: map[string]int{},
	}

	fetcher := e.Fetchers[provider]
	binding := params.PSPs[provider]
	if binding.ID != uuid.Nil {
		rep.PspID = binding.ID.String()
	}
	snap, err := fetcher.Fetch(ctx, FetchParams{
		Since:     params.Since,
		Until:     params.Until,
		PspID:     binding.ID.String(),
		Rail:      binding.Rail,
		AccountID: binding.AccountID,
	})
	if err != nil {
		return rep, nil, nil, nil, fmt.Errorf("fetch: %w", err)
	}
	if binding.ID != uuid.Nil {
		snap.PspID = binding.ID.String()
	}
	// #841: strip the absence proof when the pass did not read every active PSP
	// on the rail. A merchant running mobius + paykings on NMI would otherwise
	// have the non-armed PSP's entire book cancelled as "absent from an
	// exhaustive roster".
	if cov, ok := params.PSPCoverage[provider]; ok && !cov.Complete() && snap.Coverage.SubscriptionsExhaustive {
		snap.Coverage.SubscriptionsExhaustive = false
		log.WithContext(ctx).WithFields(log.Fields{
			"provider": provider, "active_psps": cov.Declared, "pulled_psps": cov.Pulled,
		}).Warn("reconcile: rail is covered by only some of its active PSPs; the roster is NOT an absence proof (#841)")
	}
	rep.Coverage = snap.Coverage
	rep.RemoteSubscriptions = len(snap.Subscriptions)
	rep.RemoteTransactions = len(snap.Transactions)
	rep.RemotePaymentMethods = len(snap.PaymentMethods)

	local, err := e.Local.Load(ctx, provider, nullableRailMerchantAccountID(binding))
	if err != nil {
		return rep, nil, nil, nil, fmt.Errorf("load local state: %w", err)
	}
	rep.LocalSubscriptions = len(local.Subscriptions)

	localLive := 0
	for i := range local.Subscriptions {
		s := &local.Subscriptions[i]
		if s.IsLive() && s.RailSubscriptionID != "" {
			localLive++
		}
	}

	// Circuit breaker (design decision 6): on absence-based providers, refuse
	// to treat absence as truth when the remote live set is implausibly small
	// relative to local live state — a truncated/failed report would otherwise
	// cancel the whole local roster as mass PS-2. #837: the old `localLive >= 10`
	// floor DISABLED the breaker for exactly the merchants least able to absorb
	// the mistake; it is gone. The breaker only has a job where the roster claims
	// to be an absence proof (#842) — a non-exhaustive roster proves nothing and
	// produces no absence findings to guard.
	traits := traitsFor(provider)
	if traits.absenceMeansCancelled && snap.Capabilities.Subscriptions && snap.Coverage.SubscriptionsExhaustive {
		tripped, reason := e.rosterBreaker().Implausible(provider, len(snap.Subscriptions), localLive)
		// or#837: the ratio is emitted on EVERY absence-capable pass, not only
		// when it trips. A breaker whose only trace is the moment it fires
		// cannot be trended, so nobody sees the roster degrading toward the
		// threshold until it has already halted a merchant.
		opsmetric.Emit(ctx, opsmetric.MetricRosterRatio, log.Fields{
			"provider": string(provider), "remote_live": len(snap.Subscriptions),
			"local_live": localLive, "ratio": ratioOf(len(snap.Subscriptions), localLive),
			"threshold": e.rosterBreaker().ratio(), "tripped": tripped,
		})
		if tripped {
			rep.Aborted = true
			return rep, nil, nil, nil, errors.New(reason)
		}
	}

	// Local payments are looked up by the snapshot's transaction identity
	// set (plus refund->charge links) rather than a date window.
	txnIDs := collectTxnLookupIDs(snap)
	localPayments, err := e.Local.PaymentsByTransactionIDs(ctx, provider, nullableRailMerchantAccountID(binding), txnIDs)
	if err != nil {
		return rep, nil, nil, nil, fmt.Errorf("load local payments: %w", err)
	}

	now := e.now()
	findings := diffProvider(provider, snap, local, localPayments, now, diffOptions{
		Materialize:   params.Mode == ModeEnforce && params.Mutations.allowsInsert(),
		EvidenceFloor: e.evidenceFloor(ctx),
	})
	bindApplyActions(findings, nullableRailMerchantAccountID(binding))

	if snap.Capabilities.Transactions {
		history, historyNote := e.fetchHistory(ctx, provider, params)
		rep.Dunning = computeDunningForensics(provider, snap, local, history, historyNote, now)
	}

	// #837 cancellation cap. Counted BEFORE anything is applied, over the
	// decider transitions this pass would perform — the LOCAL cancel + revoke,
	// which no other guard in the system sees. Over the cap the pass applies
	// NOTHING (findings are still persisted: they are the evidence an operator
	// needs) and halts the merchant.
	plannedCancels := countPlannedCancellations(findings)
	capExceeded, capReason := e.CancelBudget.Exceeded(plannedCancels, localLive)
	opsmetric.Emit(ctx, opsmetric.MetricCancellationsPerPass, log.Fields{
		"provider": string(provider), "planned_cancellations": plannedCancels,
		"local_live": localLive, "allowed": e.CancelBudget.Limit(localLive),
		"capped": capExceeded, "path": "pull",
	})
	if capExceeded {
		findings = append(findings, cancellationCapFinding(provider, plannedCancels, localLive, len(snap.Subscriptions), capReason))
	}

	// Persist findings (stable identity: upsert by (tenant, provider, type,
	// subject_key)).
	records := make([]FindingRecord, 0, len(findings))
	var planned []MutationRecord
	var appliedChanges []MutationRecord
	applyByID := map[uuid.UUID]*Finding{}
	for i := range findings {
		f := &findings[i]
		rec, err := e.Store.UpsertFinding(ctx, runID, *f)
		if err != nil {
			return rep, records, planned, appliedChanges, fmt.Errorf("persist finding %s/%s: %w", f.Type, f.SubjectKey, err)
		}
		records = append(records, rec)
		if e.Notifier != nil {
			if nerr := e.Notifier.NotifyFinding(ctx, rec); nerr != nil {
				log.WithContext(ctx).WithError(nerr).WithField("finding_id", rec.ID).
					Warn("reconcile: finding notification failed; continuing")
			}
		}
		if f.Apply != nil {
			planned = append(planned, mutationRecordsForFinding(provider, rec.ID, f, nil, "planned")...)
		}
		rep.FindingsByType[string(f.Type)]++
		rep.FindingsBySeverity[string(f.Severity)]++
		if rec.FirstSeenRun != nil && *rec.FirstSeenRun == runID {
			rep.NewFindings++
		} else {
			rep.UpdatedFindings++
		}
		if rec.Status == FindingStatusAdminRequired {
			rep.RequiresReview++
			rep.AdminRequired++
		}
		if f.Apply != nil && rec.Status == FindingStatusReconcileRequired && params.Mutations.allows(f) {
			applyByID[rec.ID] = f
		}
	}

	if capExceeded {
		rep.Aborted = true
		rep.ApplySkipped += len(applyByID)
		log.WithContext(ctx).WithFields(log.Fields{
			"provider": provider, "planned_cancellations": plannedCancels,
			"local_live": localLive, "remote_live": len(snap.Subscriptions),
		}).Error("reconcile: cancellation cap exceeded; applied nothing and halted this merchant's pass")
		return rep, records, planned, appliedChanges, errors.New(capReason)
	}

	// Enforce: apply the idempotent local writes (one-shot fetch+diff+apply,
	// design decision 2). Apply failures don't abort the provider — each is
	// reported and the finding stays reconcile_required for the next run.
	if params.Mode == ModeEnforce {
		// or#859 tier 1: a pass that OVERWRITES subscription state opens a
		// destructive run BEFORE it writes anything, carrying the coverage proof
		// that authorised it and the row count it predicted. The empty-roster
		// incident cancelled 40/40 subscriptions with no record of what the rows
		// looked like beforehand and no handle to undo it by; a run id plus a
		// before-image per row is precisely that missing pair.
		destRunID, runErr := e.openDestructiveRun(ctx, provider, binding, snap, countStateOverwrites(applyByID))
		if runErr != nil {
			return rep, records, planned, appliedChanges, runErr
		}
		runAffected := map[string]int{}
		if destRunID != uuid.Nil {
			rep.DestructiveRunID = destRunID.String()
		}

		for i := range records {
			rec := &records[i]
			f, ok := applyByID[rec.ID]
			if !ok {
				continue
			}
			// Capture the row as it stands BEFORE the transition overwrites it.
			// A capture failure means this write would be irreversible, so it is
			// not made: better a finding that stays reconcile_required for the
			// next pass than damage with no undo.
			var capturedAt time.Time
			if destRunID != uuid.Nil && f.Apply.Decide != nil {
				var cerr error
				if capturedAt, cerr = e.Runs.CaptureSubscription(ctx, destRunID, f.Apply.Decide.SubscriptionID); cerr != nil {
					rep.ApplyErrors = append(rep.ApplyErrors, fmt.Sprintf("%s/%s: before-image capture failed, transition NOT applied: %v", f.Type, f.SubjectKey, cerr))
					rep.ApplySkipped++
					continue
				}
			}
			evidence, didApply, err := e.applyFinding(ctx, f)
			if err != nil {
				rep.ApplyErrors = append(rep.ApplyErrors, fmt.Sprintf("%s/%s: %v", f.Type, f.SubjectKey, err))
				continue
			}
			if !didApply {
				rep.ApplySkipped++
				continue
			}
			// Attribute the provider writes the transition queued (a deferred NMI
			// vault delete) to this run, so the reverse can supersede the ones
			// that have not fired and manifest the ones that have.
			if destRunID != uuid.Nil && f.Apply.Decide != nil {
				runAffected["subscriptions"]++
				n, serr := e.Runs.StampIntents(ctx, destRunID, f.Apply.Decide.SubscriptionID, capturedAt)
				if serr != nil {
					rep.ApplyErrors = append(rep.ApplyErrors, fmt.Sprintf("%s/%s: %v", f.Type, f.SubjectKey, serr))
				}
				runAffected["rail_intents"] += n
			}
			appliedChanges = append(appliedChanges, mutationRecordsForFinding(provider, rec.ID, f, evidence, "applied")...)
			log.WithFields(log.Fields{
				"finding_id":  rec.ID,
				"type":        f.Type,
				"subject_key": f.SubjectKey,
				"provider":    provider,
				"evidence":    evidence,
			}).Info("reconcile: enforce applied local fix")
			if err := e.Store.MarkFindingAutoFixed(ctx, rec.ID, evidence); err != nil {
				rep.ApplyErrors = append(rep.ApplyErrors, fmt.Sprintf("%s/%s: mark auto_fixed: %v", f.Type, f.SubjectKey, err))
				continue
			}
			rec.Status = FindingStatusAutoFixed
			rep.AutoFixed++
		}

		// Close the run with what it actually did. A run left `running` is still
		// reversible (rows are captured before they are written, not after), so
		// a crash here loses the tally, never the undo.
		if destRunID != uuid.Nil {
			status := "completed"
			if len(rep.ApplyErrors) > 0 {
				status = "failed"
			}
			if ferr := e.Runs.Finish(ctx, destRunID, status, runAffected); ferr != nil {
				log.WithContext(ctx).WithError(ferr).WithField("destructive_run_id", destRunID).
					Error("reconcile: could not close the destructive run; it stays reversible by id")
			}
		}
	}

	// Auto-resolve: state-roster findings absent from this completed run
	// vanished on their own (design decision 1)...
	resolved, err := e.Store.AutoResolveVanished(ctx, provider, runID, stateRosterFindingTypes)
	if err != nil {
		return rep, records, planned, appliedChanges, fmt.Errorf("auto-resolve vanished findings: %w", err)
	}
	rep.AutoResolved = resolved

	// ...while transaction-window findings (PS-4/5/6) only auto-resolve when
	// this run's window re-covered the transaction and it no longer diffed.
	coveredSince, coveredUntil := e.coveredWindow(provider, params, now)
	actionable, err := e.Store.ListActionableFindingsByProvider(ctx, provider)
	if err != nil {
		return rep, records, planned, appliedChanges, fmt.Errorf("list actionable findings: %w", err)
	}
	for _, rec := range actionable {
		if rec.LastSeenRun != nil && *rec.LastSeenRun == runID {
			continue
		}
		switch rec.Type {
		case FindingChargeMissingLocal, FindingRefundUnrecorded, FindingChargebackActiveSub:
		default:
			continue
		}
		occurredAt, ok := evidenceTime(rec.RemoteEvidence, "occurred_at")
		if !ok {
			continue
		}
		if occurredAt.Before(coveredSince) || occurredAt.After(coveredUntil) {
			continue // this run did not look at that part of the timeline
		}
		if err := e.Store.MarkFindingVanished(ctx, rec.ID); err != nil {
			return rep, records, planned, appliedChanges, fmt.Errorf("auto-resolve windowed finding %s: %w", rec.ID, err)
		}
		rep.AutoResolved++
	}

	return rep, records, planned, appliedChanges, nil
}

// countStateOverwrites is how many of this pass's applies would OVERWRITE
// existing subscription state — the decider transitions. The other apply kinds
// (backfill a payment, record a refund, adopt a vault entry, materialize a
// missing subscription) are additive inserts: they destroy nothing, so they
// need no before-image and do not by themselves make a pass destructive.
func countStateOverwrites(applyByID map[uuid.UUID]*Finding) int {
	n := 0
	for _, f := range applyByID {
		if f.Apply != nil && f.Apply.Decide != nil {
			n++
		}
	}
	return n
}

// openDestructiveRun opens the run for a converge-enforce pass that is about to
// overwrite subscription state, or returns uuid.Nil when the pass overwrites
// nothing. It is the no-bypass gate (or#859 §5.1 obligation 4): an enforce pass
// with state transitions and no recorder is refused outright rather than run
// without an undo.
func (e *Engine) openDestructiveRun(ctx context.Context, provider Provider, binding RailMerchantAccountBinding, snap *RemoteSnapshot, plannedOverwrites int) (uuid.UUID, error) {
	if plannedOverwrites == 0 {
		return uuid.Nil, nil
	}
	if e.Runs == nil {
		return uuid.Nil, fmt.Errorf("reconcile: enforce mode with %d subscription state transitions requires a DestructiveRunRecorder (or#859: a destructive pass with no run record has no undo)", plannedOverwrites)
	}
	var pspID *uuid.UUID
	note := fmt.Sprintf("converge-enforce pull %s", provider)
	if binding.ID != uuid.Nil {
		id := binding.ID
		pspID = &id
		note = fmt.Sprintf("converge-enforce pull %s account %s", provider, binding.AccountID)
	}
	var coverage any
	if snap != nil {
		coverage = snap.Coverage
	}
	return e.Runs.Open(ctx, OpenDestructiveRunParams{
		PspID: pspID, Kind: DestructiveRunKindConvergeEnforce, Actor: e.actor(),
		Coverage: coverage, ExpectedRows: plannedOverwrites, Note: note,
	})
}

// fetchHistory pulls the third dunning evidence source (Postgres history,
// #735). It NEVER fails the run: unconfigured or unreachable degrades to a
// note carried into the forensics report.
func (e *Engine) fetchHistory(ctx context.Context, provider Provider, params RunParams) ([]HistoryEvent, string) {
	if e.History == nil || !e.History.Configured() {
		return nil, "not configured (no history source; provider + local evidence only)"
	}
	events, err := e.History.ListEvents(ctx, localRailNames(provider), params.Since, params.Until)
	if err != nil {
		log.WithError(err).WithField("provider", provider).
			Warn("reconcile: history source unavailable; forensics degrade to provider + local evidence")
		return nil, "unavailable: " + err.Error()
	}
	return events, fmt.Sprintf("ok: %d events", len(events))
}

// coveredWindow is the transaction timeline this run actually examined for
// the provider (fetcher defaults applied).
func (e *Engine) coveredWindow(provider Provider, params RunParams, now time.Time) (time.Time, time.Time) {
	until := params.Until
	if until.IsZero() {
		until = now
	}
	since := params.Since
	if since.IsZero() {
		if provider == ProviderCCBill {
			since = until.Add(-30 * 24 * time.Hour) // CCBillFetcher's default export window
		}
		// NMI/Stripe unbounded queries cover the full timeline.
	}
	return since, until
}

func nullableRailMerchantAccountID(binding RailMerchantAccountBinding) *uuid.UUID {
	if binding.ID == uuid.Nil {
		return nil
	}
	return &binding.ID
}

func bindApplyActions(findings []Finding, pspID *uuid.UUID) {
	if pspID == nil {
		return
	}
	for i := range findings {
		a := findings[i].Apply
		if a == nil {
			continue
		}
		if a.BackfillPayment != nil {
			a.BackfillPayment.PspID = pspID
		}
		if a.RecordRefund != nil {
			a.RecordRefund.PspID = pspID
		}
		if a.Materialize != nil {
			a.Materialize.PspID = pspID
			if a.Materialize.Backfill != nil {
				a.Materialize.Backfill.PspID = pspID
			}
		}
	}
}

func evidenceTime(m map[string]any, key string) (time.Time, bool) {
	if m == nil {
		return time.Time{}, false
	}
	s, ok := m[key].(string)
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// applyFinding executes one finding's local-write instruction. Returns the
// resolution evidence, whether anything changed (false = the local state was
// already converged or the write was skipped), and the first hard error.
func (e *Engine) applyFinding(ctx context.Context, f *Finding) (map[string]any, bool, error) {
	a := f.Apply
	evidence := map[string]any{"applied_at": e.now().Format(time.RFC3339)}
	switch {
	case a.Decide != nil:
		// #665: subscription state transitions route through the ONE decider
		// applier (park + resolve via the shared lifecycle), never a bespoke
		// SQL applier.
		if e.Decisions == nil {
			return nil, false, fmt.Errorf("no decision applier wired (enforce with subscription transitions requires Engine.Decisions)")
		}
		changed, err := e.Decisions.ApplyDecision(ctx, a.Decide.SubscriptionID, a.Decide.Decision)
		if err != nil {
			return nil, false, err
		}
		evidence["transition"] = a.Decide.Decision.Kind.String()
		if a.Decide.Decision.Reason != "" {
			evidence["transition_reason"] = a.Decide.Decision.Reason
		}
		return evidence, changed, nil

	case a.BackfillPayment != nil:
		changed, err := e.Writer.BackfillPayment(ctx, *a.BackfillPayment)
		if err != nil {
			return nil, false, err
		}
		evidence["payment_backfilled"] = changed
		if a.BackfillPayment.Grant != nil {
			evidence["entitlements_granted_for"] = a.BackfillPayment.Grant.Entitlements
		}
		return evidence, changed, nil

	case a.RecordRefund != nil:
		changed, err := e.Writer.RecordRefund(ctx, *a.RecordRefund)
		if err != nil {
			return nil, false, err
		}
		evidence["refund_recorded"] = changed
		return evidence, changed, nil

	case a.AdoptPaymentMethod != nil:
		changed, err := e.Writer.AdoptPaymentMethod(ctx, *a.AdoptPaymentMethod)
		if err != nil {
			return nil, false, err
		}
		evidence["vault_adopted"] = changed
		return evidence, changed, nil

	case a.Materialize != nil:
		res, err := e.Writer.MaterializeSubscription(ctx, *a.Materialize)
		if err != nil {
			return nil, false, err
		}
		if !res.Created {
			return nil, false, nil // already materialized: skipped, re-diffed next run
		}
		evidence["materialized_subscription_id"] = res.SubscriptionID.String()
		evidence["identity_via"] = a.Materialize.IdentityVia
		evidence["price_id"] = a.Materialize.PriceID.String()
		evidence["status"] = a.Materialize.Status
		evidence["entitlements_granted"] = res.EntitlementsGranted
		evidence["payment_backfilled"] = res.PaymentBackfilled
		if a.Materialize.Backfill != nil {
			evidence["backfill_transaction_id"] = a.Materialize.Backfill.TransactionID
		}
		return evidence, true, nil
	}
	return nil, false, fmt.Errorf("finding %s/%s has an empty apply action", f.Type, f.SubjectKey)
}

// collectTxnLookupIDs gathers every transaction identity the diff needs to
// look up locally: the snapshot's own transaction ids plus the original
// charge ids that refunds/disputes reference (Stripe re_/dp_ objects carry
// the ch_ id in "charge").
func collectTxnLookupIDs(snap *RemoteSnapshot) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(id string) {
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for i := range snap.Transactions {
		t := &snap.Transactions[i]
		add(t.TransactionID)
		bc := decodeBreadcrumbs(t.Raw)
		add(bc.Charge)
	}
	return out
}
