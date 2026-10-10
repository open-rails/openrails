package converge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/failpoint"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// The Convergence Engine is the single idempotent driver of the internal
// DERIVE / LIFE / CON planes; the provider pull feeds it. Converge runs inline
// after a source mutation (customer/subscription scope), once after a pull
// (merchant scope) and on a background sweep. A converged scope emits no
// findings and every repair is a no-op, so re-running changes nothing.

// Severity is a finding's urgency label, persisted verbatim to the ledger.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
)

// Shape is the 1:1-to-repair-verb classification of a divergence.
type Shape string

const (
	ShapeMissing  Shape = "MISSING"  // under-representation → MATERIALIZE
	ShapeExcess   Shape = "EXCESS"   // over-representation → RETRACT (gated)
	ShapeMismatch Shape = "MISMATCH" // wrong attribute/value → ADJUST
)

// Class is the remediation class (orthogonal to plane + shape).
type Class string

const (
	ClassAuto     Class = "AUTO"     // idempotent reversible local write, applied immediately
	ClassAdmin    Class = "ADMIN"    // consequential; queued for approval
	ClassOperator Class = "OPERATOR" // no API / too sensitive (refund, dispute); surfaced
)

// SourceDomain names the source domain a destructive EXCESS repair depends on,
// so the confirmed-absence gate can hold it until that domain is proven fully
// reconciled. Empty = not gated (e.g. MISSING/MISMATCH, which are always safe).
type SourceDomain string

const (
	DomainNone          SourceDomain = ""
	DomainSubscriptions SourceDomain = "subscriptions"
	DomainPayments      SourceDomain = "payments"
	DomainGrants        SourceDomain = "grants"
)

// Scope narrows a Converge pass. Merchant is required; Customer and
// Subscription narrow it for the cheap inline path. A merchant-only scope (both
// nil) is the exhaustive sweep / post-pull pass.
type Scope struct {
	Merchant     billing.MerchantID
	Customer     *uuid.UUID
	Subscription *uuid.UUID
}

// IsGlobal reports whether the scope is merchant-wide (the sweep / post-pull pass).
func (s Scope) IsGlobal() bool { return s.Customer == nil && s.Subscription == nil }

// ConvergeFinding is one divergence emitted by a plane pass. Repair, when set,
// is the idempotent local write that fixes an AUTO finding; nil means
// surface-only. RecommendedAction is the operator prose for ADMIN findings;
// the machine-executable shape rides in Evidence under recommend.EvidenceKey.
type ConvergeFinding struct {
	Type         string // finding_type, e.g. "derive.grant.excess"
	Shape        Shape
	Class        Class
	Severity     Severity
	SubjectKey   string // stable per-finding identity within (merchant, provider, type)
	Provider     string // "self" for internal planes; a provider name for PULL
	SourceDomain SourceDomain
	// Ungated opts a ShapeExcess repair out of the confirmed-absence gate. Only
	// legitimate when a local terminal fact, not an absence in provider data,
	// justifies the retraction (an already-terminated grant's effect, an
	// abandoned checkout attempt). Every use must say why in a comment.
	Ungated           bool
	Evidence          map[string]any
	RecommendedAction string                          // prose; "" = none
	Repair            func(ctx context.Context) error // AUTO repair; nil = surface only
}

// Pass is one diagnostic plane. Passes run in DERIVE → LIFE → CON order; the
// NOTIFY stage runs after their repairs, on the converged state.
type Pass interface {
	Plane() string
	Run(ctx context.Context, scope Scope) ([]ConvergeFinding, error)
	// Standing lists the finding types a merchant-wide Run reports in full:
	// an open finding of one of them that the run no longer reports has
	// cleared and resolves itself.
	Standing() []string
}

// scanState carries, through one Converge, the standing types whose scan hit
// convergeScanCap: their unreported findings may lie past the cap, so they
// stay open.
type scanState struct{ truncated map[string]bool }

type scanStateKey struct{}

// markTruncated records that a capped scan of findingType returned n rows.
func markTruncated(ctx context.Context, n int, findingTypes ...string) {
	if n < convergeScanCap {
		return
	}
	if s, ok := ctx.Value(scanStateKey{}).(*scanState); ok {
		for _, t := range findingTypes {
			s.truncated[t] = true
		}
	}
}

// ConvergeEngine runs the internal plane passes and persists their findings to the
// shared reconciliation_findings ledger, applying AUTO repairs through the
// confirmed-absence gate.
type ConvergeEngine struct {
	DB  *db.DB
	Now func() time.Time
	// lifecycle is the shared subscription core: the LIFE pass's terminal
	// repairs (grace_exhausted / pending_stale) go through it, so a converged
	// cancellation equals a user-driven one (status flip, Solana cranker
	// cascade, as-of entitlement revoke) minus the durable side effects, which
	// are not re-fired for a transition that already happened. Side-effect
	// deps are intentionally nil.
	lifecycle *subscriptions.SubscriptionLifecycleService
	passes    []Pass

	// Notifier bridges persisted findings into the operator notification
	// store. Optional; nil is a no-op. Set directly on the constructed engine.
	Notifier reconcile.FindingNotifier

	// notify is not in passes: it detects on the state the repairs just left
	// (a window DERIVE re-projected this run never emails), so Converge runs it
	// after the remediation loop.
	notify *notifyPass
}

// NewConvergeEngine wires the engine with the DERIVE → LIFE → CON passes plus
// the post-repair NOTIFY stage. It reads the caller's clock when one is given.
func NewConvergeEngine(database *db.DB, clocks ...clockwork.Clock) *ConvergeEngine {
	clock := timeutil.FirstClock(clocks...)
	e := &ConvergeEngine{DB: database, Now: func() time.Time { return clock.Now().UTC() }}
	// Real clock: the LIFE pass passes its own detection instants (now / grace-end)
	// explicitly into the cores, so the lifecycle clock only stamps canceled_at.
	// Side-effect deps (notifications / event log / payments / deferred delete) are
	// nil: convergence applies LOCAL state only — converge-not-replay.
	e.lifecycle = subscriptions.NewSubscriptionLifecycleService(database, nil, nil, nil, nil, nil, nil, clock)
	e.passes = []Pass{&derivePass{e: e}, &lifePass{e: e}, &conPass{e: e}}
	e.notify = &notifyPass{e: e}
	return e
}

// ConvergeResult is a per-scope tally of what one Converge pass did.
type ConvergeResult struct {
	Scope             Scope
	Findings          int
	AutoFixed         int
	ReconcileRequired int
	RequiresReview    int
	AdminRequired     int
	Operator          int
	RunID             *uuid.UUID
}

// AfterMutation runs Converge for a customer inline, right after a state mutation
// commits, so the customer is left consistent by construction (entitlement/credit
// effects, lifecycle state, references). It is the per-mutation companion to the
// background sweep: every mutation re-converges its own scope, so drift never
// accumulates between sweeps. Cheap by design — a customer-scoped Converge scans
// only that customer's rows (all three planes take a customer filter) and does
// ZERO writes when nothing diverged.
//
// Best-effort: convergence failures are returned for the caller to LOG, never to
// fail the mutation that already succeeded — the sweep is the backstop. Must run
// on the request's merchant-scoped connection, after the mutation committed.
func AfterMutation(ctx context.Context, database *db.DB, merchantID billing.MerchantID, customer uuid.UUID, clocks ...clockwork.Clock) (ConvergeResult, error) {
	return NewConvergeEngine(database, clocks...).Converge(ctx, Scope{Merchant: merchantID, Customer: &customer})
}

// Converge runs every plane pass for the scope (DERIVE → LIFE → CON), persists
// and remediates each finding, then runs the post-repair NOTIFY stage on the
// converged state. Must run merchant-scoped. A clean scope does no writes at
// all, which keeps the inline hot path cheap.
func (e *ConvergeEngine) Converge(ctx context.Context, scope Scope) (res ConvergeResult, runErr error) {
	res.Scope = scope
	if scope.Merchant.UUID() == uuid.Nil {
		return res, fmt.Errorf("converge: scope.Merchant required")
	}
	scans := &scanState{truncated: map[string]bool{}}
	ctx = context.WithValue(ctx, scanStateKey{}, scans)
	var collected []ConvergeFinding
	for _, p := range e.passes {
		fs, err := p.Run(ctx, scope)
		if err != nil {
			return res, fmt.Errorf("converge %s pass: %w", p.Plane(), err)
		}
		collected = append(collected, fs...)
	}

	q := e.DB.Gen(ctx)
	// A run stamps first_seen_run/last_seen_run on the findings (and drives
	// auto-vanish). Created lazily — only when there is something to persist.
	var runID *uuid.UUID
	completed := false
	defer func() {
		if runID == nil {
			return
		}
		// Error, cancellation, and panic paths retain a failed run. Only the
		// fully completed pass may certify completion; cleanup grants no work.
		status, reason := "failed", "convergence interrupted before completion"
		if runErr == nil && ctx.Err() != nil {
			runErr = ctx.Err()
		}
		if runErr != nil {
			reason = runErr.Error()
		} else if completed {
			status, reason = "completed", ""
		}
		summary, _ := json.Marshal(map[string]any{
			"findings": res.Findings, "auto_fixed": res.AutoFixed,
			"reconcile_required": res.ReconcileRequired, "requires_review": res.RequiresReview, "operator": res.Operator,
		})
		if err := (&reconcile.PGStore{DB: e.DB}).FinishRun(ctx, *runID, status, summary, reason); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("converge: finish run: %w", err))
		}
	}()
	apply := func(findings []ConvergeFinding) error {
		if len(findings) == 0 {
			return nil
		}
		if runID == nil {
			run, err := (&reconcile.PGStore{DB: e.DB}).CreateRun(ctx, reconcile.ModeEnforce, []reconcile.Provider{"self"}, nil, nil)
			if err != nil {
				return fmt.Errorf("converge: create run: %w", err)
			}
			runID = &run
			res.RunID = runID
		}
		for i := range findings {
			status, err := e.remediate(ctx, scope, findings[i])
			if err != nil {
				return err
			}
			row, perr := e.persist(ctx, q, scope, *runID, findings[i], status)
			if perr != nil {
				return perr
			}
			if e.Notifier != nil {
				if nerr := e.Notifier.NotifyFinding(ctx, reconcile.FindingRecordFromRow(row)); nerr != nil {
					log.WithContext(ctx).WithError(nerr).WithField("finding_id", row.ID).
						Warn("converge: finding notification failed; continuing")
				}
			}
			res.Findings++
			switch status {
			case "auto_fixed":
				res.AutoFixed++
			case "reconcile_required":
				res.ReconcileRequired++
			case "requires_review":
				res.RequiresReview++
				res.AdminRequired++
			}
			if findings[i].Class == ClassOperator {
				res.Operator++
			}
		}
		return nil
	}
	if err := apply(collected); err != nil {
		return res, err
	}

	// NOTIFY detects on the state the repairs above just converged, so a
	// window DERIVE re-projected in this very run never emails "access ended".
	notifyFindings, err := e.notify.Run(ctx, scope)
	if err != nil {
		return res, fmt.Errorf("converge %s pass: %w", e.notify.Plane(), err)
	}
	if err := apply(notifyFindings); err != nil {
		return res, err
	}
	if err := e.resolveCleared(ctx, q, scope, scans, append(collected, notifyFindings...)); err != nil {
		return res, err
	}

	completed = true
	return res, nil
}

// resolveCleared closes the open standing findings a merchant-wide pass no
// longer reports: their subject has cleared. Subject keys do not carry the
// customer, so a narrower scope cannot tell cleared from out of scope.
func (e *ConvergeEngine) resolveCleared(ctx context.Context, q *gen.Queries, scope Scope, scans *scanState, reported []ConvergeFinding) error {
	if !scope.IsGlobal() {
		return nil
	}
	var types []string
	for _, p := range append(e.passes, e.notify) {
		for _, t := range p.Standing() {
			if !scans.truncated[t] {
				types = append(types, t)
			}
		}
	}
	keep := make([]string, 0, len(reported))
	for _, f := range reported {
		keep = append(keep, f.Type+findingKeySep+f.SubjectKey)
	}
	if _, err := q.ResolveClearedFindings(ctx, gen.ResolveClearedFindingsParams{
		MerchantID: scope.Merchant.UUID(), FindingTypes: types, Keep: keep,
	}); err != nil {
		return fmt.Errorf("converge: resolve cleared findings: %w", err)
	}
	return nil
}

// findingKeySep joins a finding's type and subject (ResolveClearedFindings).
const findingKeySep = "\x1f"

// remediate decides a finding's ledger status and applies its repair. The
// confirmed-absence gate keeps a destructive EXCESS repair in
// reconcile_required until its source domain is proven fully reconciled.
func (e *ConvergeEngine) remediate(ctx context.Context, scope Scope, f ConvergeFinding) (string, error) {
	// Every retraction passes the gate, including
	// life.subscription.pending_stale, which cancels off a timeout.
	if f.Shape == ShapeExcess && !f.Ungated {
		if f.SourceDomain == DomainNone {
			// A retraction that names no source of truth cannot prove the thing
			// it is retracting is really gone. That is a human decision.
			return "requires_review", nil
		}
		reconciled, err := e.DB.Gen(ctx).IsSourceDomainReconciled(ctx, gen.IsSourceDomainReconciledParams{
			MerchantID: scope.Merchant.UUID(), SourceDomain: string(f.SourceDomain),
		})
		if err != nil {
			return "", fmt.Errorf("converge: gate check %s: %w", f.SourceDomain, err)
		}
		if reconciled == nil || !*reconciled {
			return "reconcile_required", nil // confirmed-absence gate: do not retract yet
		}
	}

	switch f.Class {
	case ClassAuto:
		if f.Repair != nil {
			if err := failpoint.Hit(ctx, failpoint.Site{Point: failpoint.BeforeRepair, Kind: f.Type, Subscription: subjectSubscription(f.SubjectKey)}); err != nil {
				return "", err
			}
			if err := f.Repair(ctx); err != nil {
				return "", fmt.Errorf("converge: repair %s (%s): %w", f.Type, f.SubjectKey, err)
			}
			return "auto_fixed", nil
		}
		return "reconcile_required", nil
	case ClassAdmin, ClassOperator:
		return "requires_review", nil // queued for human review
	default:
		return "requires_review", nil
	}
}

// persist upserts a finding into the shared ledger and returns the row for a
// FindingNotifier. finding_type is the qualified slug; shape/class and details
// ride in evidence.
func (e *ConvergeEngine) persist(ctx context.Context, q *gen.Queries, scope Scope, runID uuid.UUID, f ConvergeFinding, status string) (gen.BillingReconciliationFinding, error) {
	meta := map[string]any{
		"shape": string(f.Shape), "class": string(f.Class),
	}
	if f.SourceDomain != DomainNone {
		meta["source_domain"] = string(f.SourceDomain)
	}
	for k, v := range f.Evidence {
		meta[k] = v
	}
	payload := map[string]any{"local": meta}
	if f.Provider != "" && f.Provider != "self" {
		payload["provider"] = f.Provider
	}
	evidence, err := json.Marshal(payload)
	if err != nil {
		return gen.BillingReconciliationFinding{}, fmt.Errorf("converge: marshal evidence %s: %w", f.Type, err)
	}
	var recommended *string
	if f.RecommendedAction != "" {
		recommended = &f.RecommendedAction
	}
	row, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
		MerchantID:        scope.Merchant.UUID(),
		FindingType:       f.Type,
		SubjectKey:        f.SubjectKey,
		Severity:          string(f.Severity),
		Status:            status,
		RecommendedAction: recommended,
		Evidence:          evidence,
		RunID:             &runID,
	})
	if err != nil {
		return gen.BillingReconciliationFinding{}, fmt.Errorf("converge: upsert finding %s (%s): %w", f.Type, f.SubjectKey, err)
	}
	return row, nil
}

// subjectSubscription is the subscription a "subscription:<id>" subject names.
func subjectSubscription(subject string) uuid.UUID {
	id, _ := uuid.Parse(strings.TrimPrefix(subject, "subscription:"))
	return id
}
