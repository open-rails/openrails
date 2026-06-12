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
// The caller must run the engine on a tenant-scoped context (a pinned tenant
// connection / tenant in context) so every read and write is RLS-constrained
// to one tenant; a run executes under exactly one tenant.
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

	// DisableEntitlementRevocation mirrors config.IsEntitlementExpirationDisabled:
	// when true, every entitlement-revoking applier is skipped with a logged
	// note (grants and other appliers still run).
	DisableEntitlementRevocation bool

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
	// Providers to reconcile; empty means every wired fetcher.
	Providers []Provider
	// Since/Until bound the transaction window passed to the fetchers.
	Since time.Time
	Until time.Time
}

// ProviderReport is one provider's section of the run summary.
type ProviderReport struct {
	Provider            Provider          `json:"provider"`
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
	AdminPending        int               `json:"admin_pending"`
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
	Findings     int   `json:"findings"`
	AdminPending int   `json:"admin_pending"`
	AutoResolved int64 `json:"auto_resolved"`
	AutoFixed    int   `json:"auto_fixed"`
}

// RunResult is what a run returns to its caller (CLI / admin API).
type RunResult struct {
	RunID    uuid.UUID       `json:"run_id"`
	Mode     Mode            `json:"mode"`
	Status   string          `json:"status"`
	Summary  *RunSummary     `json:"summary"`
	Findings []FindingRecord `json:"findings"`
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
		rep, records, perr := e.runProvider(ctx, runID, p, params)
		summary.Providers[string(p)] = rep
		result.Findings = append(result.Findings, records...)
		if perr != nil {
			rep.Error = perr.Error()
			providerErrs = append(providerErrs, fmt.Sprintf("%s: %v", p, perr))
			log.WithError(perr).WithField("provider", p).Error("reconcile: provider run failed")
		}
		summary.Totals.Findings += rep.NewFindings + rep.UpdatedFindings
		summary.Totals.AdminPending += rep.AdminPending
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
		summary.Totals.AdminPending += stuckRep.AdminQueued
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
// persisted or resolved for it.
func (e *Engine) runProvider(ctx context.Context, runID uuid.UUID, provider Provider, params RunParams) (*ProviderReport, []FindingRecord, error) {
	rep := &ProviderReport{
		Provider:           provider,
		FindingsByType:     map[string]int{},
		FindingsBySeverity: map[string]int{},
	}

	fetcher := e.Fetchers[provider]
	snap, err := fetcher.Fetch(ctx, FetchParams{Since: params.Since, Until: params.Until})
	if err != nil {
		return rep, nil, fmt.Errorf("fetch: %w", err)
	}
	rep.RemoteSubscriptions = len(snap.Subscriptions)
	rep.RemoteTransactions = len(snap.Transactions)
	rep.RemoteVaultEntries = len(snap.VaultEntries)

	local, err := e.Local.Load(ctx, provider)
	if err != nil {
		return rep, nil, fmt.Errorf("load local state: %w", err)
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
			return rep, nil, fmt.Errorf(
				"circuit breaker: %s reports only %d live subscriptions against %d locally-live linked subscriptions (< %.0f%%); refusing to treat absence as cancellation — the report is more likely truncated/broken than %d users all cancelled. No findings were generated; investigate the provider report before re-running",
				provider, remoteLive, localLive, e.breakerRatio()*100, localLive)
		}
	}

	// Local payments are looked up by the snapshot's transaction identity
	// set (plus refund->charge links) rather than a date window.
	txnIDs := collectTxnLookupIDs(snap)
	localPayments, err := e.Local.PaymentsByTransactionIDs(ctx, provider, txnIDs)
	if err != nil {
		return rep, nil, fmt.Errorf("load local payments: %w", err)
	}

	now := e.now()
	findings := diffProvider(provider, snap, local, localPayments, now, diffOptions{Materialize: params.Mode == ModeEnforce})

	if snap.Capabilities.Transactions {
		history, historyNote := e.fetchHistory(ctx, provider, params)
		rep.Dunning = computeDunningForensics(provider, snap, local, history, historyNote, now)
	}

	// Persist findings (stable identity: upsert by (tenant, provider, type,
	// subject_key)).
	records := make([]FindingRecord, 0, len(findings))
	applyByID := map[uuid.UUID]*Finding{}
	for i := range findings {
		f := &findings[i]
		rec, err := e.Store.UpsertFinding(ctx, runID, *f)
		if err != nil {
			return rep, records, fmt.Errorf("persist finding %s/%s: %w", f.Type, f.SubjectKey, err)
		}
		records = append(records, rec)
		rep.FindingsByType[string(f.Type)]++
		rep.FindingsBySeverity[string(f.Severity)]++
		if rec.OccurrenceCount <= 1 {
			rep.NewFindings++
		} else {
			rep.UpdatedFindings++
		}
		if rec.Status == FindingStatusAdminPending {
			rep.AdminPending++
		}
		if f.Apply != nil && rec.Status == FindingStatusOpen {
			applyByID[rec.ID] = f
		}
	}

	// Enforce: apply the idempotent local writes (one-shot fetch+diff+apply,
	// design decision 2). Apply failures don't abort the provider — each is
	// reported and the finding stays open for the next run.
	if params.Mode == ModeEnforce {
		for i := range records {
			rec := &records[i]
			f, ok := applyByID[rec.ID]
			if !ok {
				continue
			}
			evidence, applied, err := e.applyFinding(ctx, f)
			if err != nil {
				rep.ApplyErrors = append(rep.ApplyErrors, fmt.Sprintf("%s/%s: %v", f.Type, f.SubjectKey, err))
				continue
			}
			if !applied {
				rep.ApplySkipped++
				continue
			}
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
		return rep, records, fmt.Errorf("auto-resolve vanished findings: %w", err)
	}
	rep.AutoResolved = resolved

	// ...while transaction-window findings (PS-4/5/6) only auto-resolve when
	// this run's window re-covered the transaction and it no longer diffed.
	coveredSince, coveredUntil := e.coveredWindow(provider, params, now)
	open, err := e.Store.ListOpenFindingsByProvider(ctx, provider)
	if err != nil {
		return rep, records, fmt.Errorf("list open findings: %w", err)
	}
	for _, rec := range open {
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
			return rep, records, fmt.Errorf("auto-resolve windowed finding %s: %w", rec.ID, err)
		}
		rep.AutoResolved++
	}

	return rep, records, nil
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
		if e.DisableEntitlementRevocation {
			log.WithField("subscription_id", a.CancelLocal.SubscriptionID).
				Warn("reconcile: entitlement expiration disabled; skipping subscription-entitlement revocation for PS-2 cancel")
			evidence["entitlement_revocation_skipped"] = "entitlement expiration disabled"
			return evidence, changed, nil
		}
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
		if e.DisableEntitlementRevocation {
			log.WithField("subscription_id", a.RevokeEntitlements.SubscriptionID).
				Warn("reconcile: entitlement expiration disabled; skipping PS-9 entitlement revocation")
			return nil, false, nil
		}
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
