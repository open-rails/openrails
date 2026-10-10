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
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/opsmetric"
)

// Engine is the pull-plane engine: it fetches each provider's declared state,
// diffs it against the local mirror, persists pull.* findings with stable
// identity and, in enforce mode, applies idempotent local mirror writes, with
// subscription transitions going through the one decider. It never mutates a
// rail: remote actions are requires_review findings, and the fetchers are
// read-only by construction. Internal-plane checks belong to the Convergence
// Engine. A run executes under exactly one merchant-scoped context.
type Engine struct {
	Fetchers map[Provider]RailFetcher
	Store    Store
	Local    LocalStateLoader
	// Writer applies enforce-mode local mirror writes. May be nil for
	// advisory-only engines.
	Writer LocalWriter
	// Decisions applies decider transitions (PS-2/PS-3). May be nil for
	// advisory-only engines.
	Decisions DecisionApplier
	// History is the third dunning-forensics evidence source (failed payment
	// attempts). May be nil or unconfigured: the forensics report then carries
	// a note; an unavailable history source is never a run error.
	History HistoryEventSource

	// RecoverInvoicePayment qualifies an observed NMI invoice receipt through
	// its canonical financial writer before the generic subscription diff.
	// Nil leaves missing invoice receipts as visible findings.
	RecoverInvoicePayment func(context.Context, uuid.UUID, string) error

	// Notifier bridges persisted findings into the operator notification
	// store. Optional; nil is a no-op. Best-effort: a notify failure is
	// logged, never fails the run (the finding is already persisted).
	Notifier FindingNotifier

	// Policy reads the merchant's evidence-staleness floor. A nil Policy makes
	// the decider trust only what this pass observed: stricter, never more
	// permissive.
	Policy EvidenceFloorReader

	// Runs makes an enforce pass reversible: it opens a maintenance_runs
	// record with the coverage proof that authorised the pass, captures a
	// before-image of every subscription the pass overwrites, and attributes
	// the provider intents it queues. Passes that would overwrite subscription
	// state refuse when it is nil: an unrecorded destructive pass has no undo.
	Runs DestructiveRunRecorder

	// Now is the clock (defaults to time.Now UTC).
	Now func() time.Time

	// Circuit breaker for absence-based PS-2 detection: when the provider
	// reports implausibly few live subscriptions vs local state, abort the
	// provider's run instead of generating mass PS-2. Defaults: MinLocal 1
	// (no small-merchant blind spot), Ratio 0.10.
	CircuitBreakerMinLocal int
	CircuitBreakerRatio    float64

	// Actor attributes this engine's destructive runs (the operator who ran the
	// CLI, or the worker that scheduled the pass). Empty defaults to
	// "converge-enforce" — an audit trail, not authentication.
	Actor string

	// CancelBudget caps how many subscriptions one pass may cancel for this
	// merchant. Over the cap the pass applies nothing, raises a
	// requires_review finding and halts. Zero value = the defaults.
	CancelBudget CancelBudget
}

// RunParams bounds one reconcile run.
type RunParams struct {
	Mode Mode
	// Mutations, when non-nil, limits enforce-mode local writes to specific
	// mutation classes. Nil applies every safe local fix (in-process callers);
	// the operator CLI passes an explicit policy from --insert/--overwrite.
	Mutations *LocalMutationPolicy
	// Providers to reconcile; empty means every wired fetcher.
	Providers []Provider
	// PSPCoverage declares, per provider, how many PSPs the merchant has
	// active on that rail and how many this pass read. A pull arms from one
	// PSP, so anything short of complete coverage strips
	// SubscriptionsExhaustive: a roster that saw one of two accounts cannot
	// prove a subscription of the other is gone.
	PSPCoverage map[Provider]PSPCoverage
	// PSPs binds each provider section to the one merchant-scoped PSP whose
	// credentials armed its fetcher. Required: the engine scopes mirror reads
	// and stamps materialization writes with it, and a provider with no
	// binding is refused rather than run account-agnostically.
	PSPs map[Provider]PSPBinding
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

// PSPBinding is the account row a provider-pull is authorized to
// treat as authoritative. ID is billing.psps.id; AccountID is
// the raw provider-returned account identifier.
type PSPBinding struct {
	ID        uuid.UUID `json:"id"`
	Rail      string    `json:"rail"`
	AccountID string    `json:"account_id"`
}

// ProviderReport is one provider's section of the run summary.
type ProviderReport struct {
	Provider Provider `json:"provider"`
	PspID    string   `json:"psp_id,omitempty"`
	// DestructiveRunID is set when this provider's enforce pass overwrote
	// subscription state: the handle `openrails undo-run --run <id>` reverses.
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
	WithheldChanges      int               `json:"withheld_changes,omitempty"`
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
	// absenceMeansCanceled: the provider's subscription listing only contains
	// LIVE subscriptions, so a local-live subscription that is absent remotely
	// is dead at the rail (NMI recurring report). Guarded by the circuit
	// breaker.
	absenceMeansCanceled bool
	// paymentMethodsExhaustive: the vault listing is the complete roster, so a local
	// payment method missing from it no longer exists at the rail.
	paymentMethodsExhaustive bool
}

func traitsFor(p Provider) providerTraits {
	switch p {
	case ProviderNMI:
		return providerTraits{absenceMeansCanceled: true, paymentMethodsExhaustive: true}
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
	e.syncDBClocks()

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

func (e *Engine) syncDBClocks() {
	if store, ok := e.Store.(*PGStore); ok {
		store.Now = e.now
	}
	if writer, ok := e.Writer.(*PGLocalWriter); ok {
		writer.Now = e.now
	}
}

// EvidenceFloorReader yields a merchant's evidence-staleness floor.
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
	// A pull is always bound to the one PSP whose credentials armed its
	// fetcher; an unbound section would diff one PSP's roster against
	// another's rows. Refuse.
	binding, ok := params.PSPs[provider]
	if !ok || binding.ID == uuid.Nil {
		return rep, nil, nil, nil, fmt.Errorf("no PSP binding for provider %s: a pull must name the PSP its credentials armed from", provider)
	}
	rep.PspID = binding.ID.String()
	// Local rows load BEFORE the fetch: a row created while the roster was
	// being read is absent from it without being gone at the provider.
	local, err := e.Local.Load(ctx, provider, binding.ID)
	if err != nil {
		return rep, nil, nil, nil, fmt.Errorf("load local state: %w", err)
	}
	rep.LocalSubscriptions = len(local.Subscriptions)
	snap, err := fetcher.Fetch(ctx, FetchParams{
		Since:      params.Since,
		Until:      params.Until,
		ObservedAt: e.now(),
		PspID:      binding.ID.String(),
		Rail:       binding.Rail,
		AccountID:  binding.AccountID,
	})
	if err != nil {
		return rep, nil, nil, nil, fmt.Errorf("fetch: %w", err)
	}
	snap.PspID = binding.ID.String()
	// Strip the absence proof when the pass did not read every active PSP on
	// the rail, or a merchant with two NMI PSPs would have the unread one's
	// whole book canceled as "absent from an exhaustive roster".
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

	localLive := 0
	for i := range local.Subscriptions {
		s := &local.Subscriptions[i]
		if s.IsLive() && s.RailSubscriptionID != "" {
			localLive++
		}
	}

	// Circuit breaker: on absence-based providers, refuse to treat absence as
	// truth when the remote live set is implausibly small against local live
	// state; a truncated report would otherwise cancel the whole roster as
	// mass PS-2. It guards small books too, and only where the roster claims
	// to be an absence proof (a non-exhaustive one yields no absence findings).
	traits := traitsFor(provider)
	if (params.Mutations == nil || params.Mutations.Overwrite) && traits.absenceMeansCanceled && snap.Capabilities.Subscriptions && snap.Coverage.SubscriptionsExhaustive {
		tripped, reason := e.rosterBreaker().Implausible(provider, len(snap.Subscriptions), localLive)
		// The ratio is emitted on every absence-capable pass, not only when it
		// trips, so a roster degrading toward the threshold can be trended.
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
	localPayments, err := e.Local.PaymentsByTransactionIDs(ctx, provider, binding.ID, txnIDs)
	if err != nil {
		return rep, nil, nil, nil, fmt.Errorf("load local payments: %w", err)
	}
	recoveryErrors := map[string]error{}
	if provider == ProviderNMI && params.Mode == ModeEnforce && params.Mutations.allowsInsert() && e.RecoverInvoicePayment != nil {
		known := make(map[string]bool, len(localPayments))
		for _, payment := range localPayments {
			known[payment.TransactionID] = true
		}
		recovered := false
		for _, transaction := range snap.Transactions {
			var raw struct {
				OrderDescription string `json:"order_description"`
			}
			if json.Unmarshal(transaction.Raw, &raw) != nil || known[transaction.TransactionID] || transaction.Type != TransactionTypeSale || !transaction.Success || !strings.HasPrefix(raw.OrderDescription, "invoice") {
				continue
			}
			if err := e.RecoverInvoicePayment(ctx, binding.ID, transaction.TransactionID); err != nil {
				recoveryErrors[transaction.TransactionID] = err
			} else {
				recovered = true
			}
		}
		if recovered {
			localPayments, err = e.Local.PaymentsByTransactionIDs(ctx, provider, binding.ID, txnIDs)
			if err != nil {
				return rep, nil, nil, nil, fmt.Errorf("reload recovered invoice payments: %w", err)
			}
		}
	}

	now := e.now()
	findings := diffProvider(provider, snap, local, localPayments, now, diffOptions{
		Materialize:   params.Mode == ModeEnforce && params.Mutations.allowsInsert(),
		EvidenceFloor: e.evidenceFloor(ctx),
	})
	for i := range findings {
		finding := &findings[i]
		if err := recoveryErrors[finding.SubjectKey]; err != nil && finding.Type == FindingChargeMissingLocal {
			finding.Apply = nil
			finding.Status, finding.RequiresAdmin = FindingStatusRequiresReview, true
			finding.RecommendedAction = "invoice receipt remains unresolved: " + err.Error()
		}
	}
	findings = confirmPaymentMethodFindings(ctx, provider, fetcher, local, findings)
	bindApplyActions(findings, binding.ID)

	if snap.Capabilities.Transactions {
		history, historyNote := e.fetchHistory(ctx, provider, params)
		rep.Dunning = computeDunningForensics(provider, snap, local, history, historyNote, now)
	}

	// Cancellation cap, counted before anything is applied over the decider
	// transitions this pass would perform (the local cancel + revoke, which no
	// other guard sees). Over the cap the pass applies nothing (findings are
	// still persisted as the operator's evidence) and halts the merchant.
	plannedCancels := countPlannedCancellations(findings)
	capExceeded, capReason := e.CancelBudget.Exceeded(plannedCancels, localLive)
	if params.Mutations != nil && !params.Mutations.Overwrite {
		capExceeded = false
	}
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
		if strings.HasPrefix(string(f.Type), "pull.") {
			f.PSPID = binding.ID
		}
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
		} else if f.Apply != nil && rec.Status == FindingStatusReconcileRequired {
			rep.WithheldChanges++
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

	// Enforce: apply the idempotent local writes. Apply failures don't abort
	// the provider: each is reported and the finding stays reconcile_required
	// for the next run.
	if params.Mode == ModeEnforce {
		// A pass that overwrites subscription state opens a destructive run
		// before it writes anything, carrying the coverage proof that
		// authorised it and its predicted row count: the run id plus a
		// before-image per row is the undo.
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
				n, serr := e.Runs.Seal(ctx, destRunID, f.Apply.Decide.SubscriptionID, capturedAt)
				if serr != nil {
					rep.ApplyErrors = append(rep.ApplyErrors, fmt.Sprintf("%s/%s: %v", f.Type, f.SubjectKey, serr))
				}
				runAffected["provider_intents"] += n
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
	// vanished on their own.
	resolvable := []FindingType{FindingPaymentMethodMismatch}
	if snap.Coverage.SubscriptionsExhaustive {
		resolvable = stateRosterFindingTypes
	}
	resolved, err := e.Store.AutoResolveVanished(ctx, binding.ID, runID, resolvable)
	if err != nil {
		return rep, records, planned, appliedChanges, fmt.Errorf("auto-resolve vanished findings: %w", err)
	}
	rep.AutoResolved = resolved

	// Transaction windows may be keyed by provider modification time (NMI),
	// not occurrence time. Only an actually returned transaction can qualify
	// its earlier finding as resolved; an absent date-window match proves none.
	observedTransactions := make(map[string]bool, len(snap.Transactions))
	for _, transaction := range snap.Transactions {
		observedTransactions[transaction.TransactionID] = true
	}
	actionable, err := e.Store.ListActionablePullFindings(ctx, binding.ID)
	if err != nil {
		return rep, records, planned, appliedChanges, fmt.Errorf("list actionable findings: %w", err)
	}
	for _, rec := range actionable {
		if rec.LastSeenRun != nil && *rec.LastSeenRun == runID {
			continue
		}
		switch rec.Type {
		case FindingChargeMissingLocal, FindingRefundUnrecorded, FindingChargebackActiveSub, FindingReversalUnlinked:
		default:
			continue
		}
		if !observedTransactions[rec.SubjectKey] {
			continue
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

// openDestructiveRun opens the run for a converge-enforce pass about to
// overwrite subscription state, or returns uuid.Nil when it overwrites
// nothing. It is the no-bypass gate: an enforce pass with state transitions
// and no recorder is refused rather than run without an undo.
func (e *Engine) openDestructiveRun(ctx context.Context, provider Provider, binding PSPBinding, snap *RemoteSnapshot, plannedOverwrites int) (uuid.UUID, error) {
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

// fetchHistory pulls the third dunning evidence source (failed payment
// attempts). It never fails the run: unconfigured or unreachable degrades to a
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

// bindApplyActions stamps the pull's PSP onto every local write the pass will
// perform. runProvider refuses a section without a PSP, so no mirror row the
// pull path creates is unattributed.
func bindApplyActions(findings []Finding, psp uuid.UUID) {
	pspID := &psp
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
			a.Materialize.PspID = psp
			if a.Materialize.Backfill != nil {
				a.Materialize.Backfill.PspID = pspID
			}
		}
	}
}

// applyFinding executes one finding's local-write instruction. Returns the
// resolution evidence, whether anything changed (false = the local state was
// already converged or the write was skipped), and the first hard error.
func (e *Engine) applyFinding(ctx context.Context, f *Finding) (map[string]any, bool, error) {
	a := f.Apply
	now := e.now()
	evidence := map[string]any{"applied_at": now.Format(time.RFC3339)}
	switch {
	case a.Decide != nil:
		// Subscription state transitions route through the one decider
		// applier (park + resolve via the shared lifecycle).
		if e.Decisions == nil {
			return nil, false, fmt.Errorf("no decision applier wired (enforce with subscription transitions requires Engine.Decisions)")
		}
		if setter, ok := e.Decisions.(interface{ SetClock(clockwork.Clock) }); ok {
			setter.SetClock(clockwork.NewFakeClockAt(now))
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
			evidence["access_granted_for"] = a.BackfillPayment.Grant.ProductID.String()
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
		evidence["access_granted"] = res.AccessGranted
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
		add(bc.PaymentIntent)
	}
	return out
}
