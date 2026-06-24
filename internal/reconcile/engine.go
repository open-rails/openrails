package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// Engine is the #107 phase-2 diff engine: it fetches each provider's declared
// state, diffs it against local billing state, persists PS-1..PS-9 findings
// with stable identity, and (in enforce mode) applies idempotent LOCAL
// convergence writes. It NEVER mutates a processor — remote actions are
// findings in the admin queue (requires_admin), and the fetchers are
// read-only by construction.
//
// The caller must run the engine on a merchant-scoped context (a pinned merchant
// connection / merchant in context) so every read and write is RLS-constrained
// to one merchant; a run executes under exactly one merchant.
type Engine struct {
	Fetchers map[Provider]ProcessorFetcher
	Store    Store
	Local    LocalStateLoader
	// Writer applies enforce-mode local writes. May be nil for advisory-only
	// engines.
	Writer LocalWriter
	// Intents is the PS-10 stuck-intent source (#358 provider intent ledger,
	// LOCAL read only). The pass runs on every run regardless of provider
	// filters; nil skips it (unit-test engines without a ledger).
	Intents StuckIntentSource
	// History is the THIRD dunning-forensics evidence source (OpenRails'
	// own ClickHouse analytics events, incl. imported legacy history). May be
	// nil / unconfigured: the forensics report then carries a note instead —
	// a missing or unreachable ClickHouse is NEVER a run error.
	History HistoryEventSource

	// Now is the clock (defaults to time.Now UTC).
	Now func() time.Time

	// Circuit breaker for absence-based PS-2 detection (design decision 6):
	// when the provider reports implausibly few live subscriptions vs local
	// state, ABORT the provider's run instead of generating mass PS-2.
	// Defaults: MinLocal 10, Ratio 0.10.
	CircuitBreakerMinLocal int
	CircuitBreakerRatio    float64
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
	// ProviderAccounts optionally binds each provider section to one
	// merchant-scoped provider account. When set, the engine scopes local mirror
	// reads and local materialization writes to that account id.
	ProviderAccounts map[Provider]ProviderAccountBinding
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
	case FindingLocalActiveRemoteDead, FindingStatusMismatch, FindingVaultMismatch, FindingEntitlementMismatch:
		return p.Overwrite
	default:
		return false
	}
}

// ProviderAccountBinding is the account row a provider-pull is authorized to
// treat as authoritative. ID is openrails.provider_accounts.id; AccountID is
// the raw provider-returned account identifier.
type ProviderAccountBinding struct {
	ID           uuid.UUID `json:"id"`
	ProviderType string    `json:"provider_type"`
	AccountID    string    `json:"account_id"`
}

// ProviderReport is one provider's section of the run summary.
type ProviderReport struct {
	Provider            Provider          `json:"provider"`
	ProviderAccountID   string            `json:"provider_account_id,omitempty"`
	Aborted             bool              `json:"aborted,omitempty"`
	Error               string            `json:"error,omitempty"`
	RemoteSubscriptions int               `json:"remote_subscriptions"`
	RemoteTransactions  int               `json:"remote_transactions"`
	RemoteVaultEntries  int               `json:"remote_vault_entries"`
	LocalSubscriptions  int               `json:"local_subscriptions"`
	FindingsByType      map[string]int    `json:"findings_by_type,omitempty"`
	FindingsBySeverity  map[string]int    `json:"findings_by_severity,omitempty"`
	NewFindings         int               `json:"new_findings"`
	UpdatedFindings     int               `json:"updated_findings"`
	RequiresReview      int               `json:"requires_review"`
	AdminRequired       int               `json:"-"`
	AutoResolved        int64             `json:"auto_resolved"`
	AutoFixed           int               `json:"auto_fixed"`
	ApplySkipped        int               `json:"apply_skipped,omitempty"`
	ApplyErrors         []string          `json:"apply_errors,omitempty"`
	Dunning             *DunningForensics `json:"dunning_forensics,omitempty"`
}

// RunSummary is the persisted summary jsonb of a run.
type RunSummary struct {
	Mode      Mode                       `json:"mode"`
	Providers map[string]*ProviderReport `json:"providers"`
	// StuckIntents is the provider-independent PS-10 section (one per run,
	// not per provider). Nil when the engine has no intent source.
	StuckIntents *StuckIntentReport `json:"stuck_intents,omitempty"`
	Totals       SummaryTotals      `json:"totals"`
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

func (e *Engine) breakerMinLocal() int {
	if e.CircuitBreakerMinLocal > 0 {
		return e.CircuitBreakerMinLocal
	}
	return 10
}

func (e *Engine) breakerRatio() float64 {
	if e.CircuitBreakerRatio > 0 {
		return e.CircuitBreakerRatio
	}
	return 0.10
}

// providerTraits captures per-provider diff semantics that the capability
// flags alone cannot express.
type providerTraits struct {
	// absenceMeansCancelled: the provider's subscription listing only contains
	// LIVE subscriptions, so a local-live subscription that is absent remotely
	// is dead at the processor (NMI recurring report). Guarded by the circuit
	// breaker.
	absenceMeansCancelled bool
	// vaultExhaustive: the vault listing is the complete roster, so a local
	// payment method missing from it no longer exists at the processor.
	vaultExhaustive bool
}

func traitsFor(p Provider) providerTraits {
	switch p {
	case ProviderNMI:
		return providerTraits{absenceMeansCancelled: true, vaultExhaustive: true}
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

	// PS-10 stuck provider intents: provider-independent, every run, local
	// ledger only — it runs even when --provider narrowed the sections above,
	// and identically in check and fix (no applier exists for it).
	stuckRep, stuckErr := e.runStuckIntents(ctx, runID, result)
	summary.StuckIntents = stuckRep
	if stuckErr != nil {
		providerErrs = append(providerErrs, fmt.Sprintf("stuck-intents: %v", stuckErr))
		log.WithError(stuckErr).Error("reconcile: stuck-intent pass failed")
	}
	if stuckRep != nil {
		summary.Totals.Findings += stuckRep.Total
		summary.Totals.RequiresReview += stuckRep.RequiresReview
		summary.Totals.AdminRequired += stuckRep.AdminRequired
		summary.Totals.AutoResolved += stuckRep.AutoResolved
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
	binding := params.ProviderAccounts[provider]
	if binding.ID != uuid.Nil {
		rep.ProviderAccountID = binding.ID.String()
	}
	snap, err := fetcher.Fetch(ctx, FetchParams{
		Since:             params.Since,
		Until:             params.Until,
		ProviderAccountID: binding.ID.String(),
		ProviderType:      binding.ProviderType,
		AccountID:         binding.AccountID,
	})
	if err != nil {
		return rep, nil, nil, nil, fmt.Errorf("fetch: %w", err)
	}
	if binding.ID != uuid.Nil {
		snap.ProviderAccountID = binding.ID.String()
	}
	rep.RemoteSubscriptions = len(snap.Subscriptions)
	rep.RemoteTransactions = len(snap.Transactions)
	rep.RemoteVaultEntries = len(snap.VaultEntries)

	local, err := e.Local.Load(ctx, provider, nullableProviderAccountID(binding))
	if err != nil {
		return rep, nil, nil, nil, fmt.Errorf("load local state: %w", err)
	}
	rep.LocalSubscriptions = len(local.Subscriptions)

	// Circuit breaker (design decision 6): on absence-based providers, refuse
	// to treat absence as truth when the remote live set is implausibly small
	// relative to local live state — a truncated/failed report would otherwise
	// cancel the whole local roster as mass PS-2.
	traits := traitsFor(provider)
	if traits.absenceMeansCancelled && snap.Capabilities.Subscriptions {
		localLive := 0
		for i := range local.Subscriptions {
			s := &local.Subscriptions[i]
			if s.IsLive() && s.ProcessorSubscriptionID != "" {
				localLive++
			}
		}
		remoteLive := len(snap.Subscriptions)
		if localLive >= e.breakerMinLocal() && float64(remoteLive) < float64(localLive)*e.breakerRatio() {
			rep.Aborted = true
			return rep, nil, nil, nil, fmt.Errorf(
				"circuit breaker: %s reports only %d live subscriptions against %d locally-live linked subscriptions (< %.0f%%); refusing to treat absence as cancellation — the report is more likely truncated/broken than %d users all cancelled. No findings were generated; investigate the provider report before re-running",
				provider, remoteLive, localLive, e.breakerRatio()*100, localLive)
		}
	}

	// Local payments are looked up by the snapshot's transaction identity
	// set (plus refund->charge links) rather than a date window.
	txnIDs := collectTxnLookupIDs(snap)
	localPayments, err := e.Local.PaymentsByTransactionIDs(ctx, provider, nullableProviderAccountID(binding), txnIDs)
	if err != nil {
		return rep, nil, nil, nil, fmt.Errorf("load local payments: %w", err)
	}

	now := e.now()
	findings := diffProvider(provider, snap, local, localPayments, now, diffOptions{Materialize: params.Mode == ModeEnforce && params.Mutations.allowsInsert()})
	bindApplyActions(findings, nullableProviderAccountID(binding))

	if snap.Capabilities.Transactions {
		history, historyNote := e.fetchHistory(ctx, provider, params)
		rep.Dunning = computeDunningForensics(provider, snap, local, history, historyNote, now)
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
		if f.Apply != nil {
			planned = append(planned, mutationRecordsForFinding(provider, rec.ID, f, nil, "planned")...)
		}
		rep.FindingsByType[string(f.Type)]++
		rep.FindingsBySeverity[string(f.Severity)]++
		if rec.FirstSeenRun == runID {
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

	// Enforce: apply the idempotent local writes (one-shot fetch+diff+apply,
	// design decision 2). Apply failures don't abort the provider — each is
	// reported and the finding stays reconcile_required for the next run.
	if params.Mode == ModeEnforce {
		for i := range records {
			rec := &records[i]
			f, ok := applyByID[rec.ID]
			if !ok {
				continue
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
		if rec.LastSeenRun == runID {
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

// fetchHistory pulls the third dunning evidence source (ClickHouse analytics
// events). It NEVER fails the run: unconfigured or unreachable degrades to a
// note carried into the forensics report.
func (e *Engine) fetchHistory(ctx context.Context, provider Provider, params RunParams) ([]HistoryEvent, string) {
	if e.History == nil || !e.History.Configured() {
		return nil, "not configured (no ClickHouse analytics source; provider + local evidence only)"
	}
	events, err := e.History.ListEvents(ctx, localProcessorNames(provider), params.Since, params.Until)
	if err != nil {
		log.WithError(err).WithField("provider", provider).
			Warn("reconcile: analytics history source unavailable; forensics degrade to provider + local evidence")
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

func nullableProviderAccountID(binding ProviderAccountBinding) *uuid.UUID {
	if binding.ID == uuid.Nil {
		return nil
	}
	return &binding.ID
}

func bindApplyActions(findings []Finding, providerAccountID *uuid.UUID) {
	if providerAccountID == nil {
		return
	}
	for i := range findings {
		a := findings[i].Apply
		if a == nil {
			continue
		}
		if a.BackfillPayment != nil {
			a.BackfillPayment.ProviderAccountID = providerAccountID
		}
		if a.RecordRefund != nil {
			a.RecordRefund.ProviderAccountID = providerAccountID
		}
		if a.Materialize != nil {
			a.Materialize.ProviderAccountID = providerAccountID
			if a.Materialize.Backfill != nil {
				a.Materialize.Backfill.ProviderAccountID = providerAccountID
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
	case a.CancelLocal != nil:
		changed, err := e.Writer.CancelSubscriptionLocal(ctx, *a.CancelLocal)
		if err != nil {
			return nil, false, err
		}
		evidence["cancelled_locally"] = changed
		revoked, err := e.Writer.RevokeSubscriptionEntitlements(ctx, RevokeEntitlementsAction{
			SubscriptionID: a.CancelLocal.SubscriptionID,
			Reason:         a.CancelLocal.Reason,
		})
		if err != nil {
			return nil, changed, err
		}
		evidence["entitlements_revoked"] = revoked
		return evidence, changed || revoked > 0, nil

	case a.AdoptStatus != nil:
		changed, err := e.Writer.AdoptSubscriptionStatus(ctx, *a.AdoptStatus)
		if err != nil {
			return nil, false, err
		}
		evidence["adopted_status"] = a.AdoptStatus.Status
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

	case a.AdoptVault != nil:
		changed, err := e.Writer.AdoptPaymentMethod(ctx, *a.AdoptVault)
		if err != nil {
			return nil, false, err
		}
		evidence["vault_adopted"] = changed
		return evidence, changed, nil

	case a.GrantEntitlements != nil:
		granted, err := e.Writer.GrantEntitlements(ctx, *a.GrantEntitlements)
		if err != nil {
			return nil, false, err
		}
		evidence["entitlements_granted"] = granted
		return evidence, granted > 0, nil

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

	case a.RevokeEntitlements != nil:
		revoked, err := e.Writer.RevokeSubscriptionEntitlements(ctx, *a.RevokeEntitlements)
		if err != nil {
			return nil, false, err
		}
		evidence["entitlements_revoked"] = revoked
		return evidence, revoked > 0, nil
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
