package converge

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// The Convergence Engine (#511) — the single idempotent driver of the internal
// DERIVE / LIFE / CON planes (see docs/consistency-invariants.md §7). The PULL
// plane is the provider pull that feeds it; once Phase B/F land, `reconcile pull`
// overwrites the mirror and then runs one Converge pass, and this becomes the
// sole engine (the legacy per-provider PS-* diff in engine.go is retired).
//
// Converge is invoked three ways: inline after a source mutation (scoped to a
// customer/subscription), once after a pull (scoped to a merchant), and on a
// background sweep. It is idempotent: a converged scope emits no findings and
// every repair is a no-op, so re-running changes nothing.

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
	ShapeExcess   Shape = "EXCESS"   // over-representation → RETRACT (gated §3.2)
	ShapeMismatch Shape = "MISMATCH" // wrong attribute/value → ADJUST
)

// Class is the remediation class (orthogonal to plane + shape).
type Class string

const (
	ClassAuto     Class = "AUTO"     // idempotent reversible local write, applied immediately
	ClassAdmin    Class = "ADMIN"    // consequential; queued for approval
	ClassOperator Class = "OPERATOR" // no API / too sensitive (refund, dispute); surfaced
)

// SourceDomain names the source domain a destructive EXCESS repair depends on, so
// the confirmed-absence gate (§3.2) can hold it until that domain is proven fully
// reconciled. Empty = not gated (e.g. MISSING/MISMATCH, which are always safe).
type SourceDomain string

const (
	DomainNone          SourceDomain = ""
	DomainSubscriptions SourceDomain = "subscriptions"
	DomainPayments      SourceDomain = "payments"
	DomainGrants        SourceDomain = "grants"
)

// Scope narrows a Converge pass. Merchant is required (RLS scope); Customer and
// Subscription narrow it for the cheap inline path. A merchant-only scope (both
// nil) is the exhaustive sweep / post-pull pass.
type Scope struct {
	Merchant     merchant.ID
	Customer     *uuid.UUID
	Subscription *uuid.UUID
}

// IsGlobal reports whether the scope is merchant-wide (the sweep / post-pull pass).
func (s Scope) IsGlobal() bool { return s.Customer == nil && s.Subscription == nil }

// ConvergeFinding is one divergence emitted by a plane pass. Repair, when set, is
// the idempotent local write that fixes an AUTO finding; nil means surface-only.
type ConvergeFinding struct {
	Type         string // finding_type, e.g. "derive.grant.excess"
	Shape        Shape
	Class        Class
	Severity     Severity
	SubjectKey   string // stable per-finding identity within (merchant, provider, type)
	Provider     string // "self" for internal planes; a provider name for PULL
	SourceDomain SourceDomain
	Evidence     map[string]any
	Repair       func(ctx context.Context) error // AUTO repair; nil = surface only
}

// Pass is one diagnostic plane. Passes run in DERIVE → LIFE → CON order.
type Pass interface {
	Plane() string
	Run(ctx context.Context, scope Scope) ([]ConvergeFinding, error)
}

// ConvergeEngine runs the internal plane passes and persists their findings to the
// shared reconciliation_findings ledger, applying AUTO repairs through the
// confirmed-absence gate.
type ConvergeEngine struct {
	DB  *db.DB
	Now func() time.Time
	// lifecycle is the shared subscription local-state core. The LIFE pass's
	// terminal repairs (grace_exhausted / pending_stale / period_overdue) route
	// through it instead of bespoke SQL appliers, so a converged cancellation is
	// identical to a user-driven one (status flip, the #264 Solana cranker
	// cascade, as-of entitlement revoke) — minus the durable side-effects, which
	// are deliberately not re-fired for a transition that logically already
	// happened. Built from db (no import cycle: subscriptions never imports
	// converge); side-effect deps are intentionally nil.
	lifecycle *subscriptions.SubscriptionLifecycleService
	passes    []Pass
}

// NewConvergeEngine wires the engine with the DERIVE → LIFE → CON passes (each
// fleshed out across #511 Phase D).
func NewConvergeEngine(database *db.DB) *ConvergeEngine {
	e := &ConvergeEngine{DB: database, Now: func() time.Time { return time.Now().UTC() }}
	// Real clock: the LIFE pass passes its own detection instants (now / grace-end)
	// explicitly into the cores, so the lifecycle clock only stamps cancelled_at.
	// Side-effect deps (notifications / event log / payments / deferred delete) are
	// nil: convergence applies LOCAL state only — converge-not-replay.
	e.lifecycle = subscriptions.NewSubscriptionLifecycleService(database, nil, nil, nil, nil, nil, nil, clockwork.NewRealClock())
	e.passes = []Pass{&derivePass{e: e}, &lifePass{e: e}, &conPass{e: e}}
	return e
}

// ConvergeResult is a per-scope tally of what one Converge pass did.
type ConvergeResult struct {
	Scope       Scope
	Findings    int
	AutoFixed   int
	Held        int
	AdminQueued int
	Operator    int
	RunID       *uuid.UUID
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
// on the request's merchant-scoped connection (RLS), after the mutation committed.
func AfterMutation(ctx context.Context, database *db.DB, merchantID merchant.ID, customer uuid.UUID) (ConvergeResult, error) {
	return NewConvergeEngine(database).Converge(ctx, Scope{Merchant: merchantID, Customer: &customer})
}

// Converge runs every plane pass for the scope (DERIVE → LIFE → CON), then
// persists + remediates each finding. It must be called inside a merchant-scoped
// connection (RunInMerchantConn) so RLS + the gen queries resolve to the merchant.
// When the scope is clean (no findings) it does no writes at all — the idempotent
// no-op that keeps the inline hot path cheap.
func (e *ConvergeEngine) Converge(ctx context.Context, scope Scope) (ConvergeResult, error) {
	res := ConvergeResult{Scope: scope}
	if scope.Merchant.UUID() == uuid.Nil {
		return res, fmt.Errorf("converge: scope.Merchant required")
	}

	var collected []ConvergeFinding
	for _, p := range e.passes {
		fs, err := p.Run(ctx, scope)
		if err != nil {
			return res, fmt.Errorf("converge %s pass: %w", p.Plane(), err)
		}
		collected = append(collected, fs...)
	}
	if len(collected) == 0 {
		return res, nil // converged: no run, no writes
	}

	q := e.DB.Gen(ctx)
	// A run stamps first_seen_run/last_seen_run on the findings (and drives
	// auto-vanish). Created lazily — only when there is something to persist.
	run, err := q.CreateReconciliationRun(ctx, gen.CreateReconciliationRunParams{
		MerchantID: scope.Merchant.UUID(), Mode: "enforce", Providers: []string{"self"},
	})
	if err != nil {
		return res, fmt.Errorf("converge: create run: %w", err)
	}
	res.RunID = &run.ID

	for i := range collected {
		status, err := e.remediate(ctx, scope, collected[i])
		if err != nil {
			return res, err
		}
		if perr := e.persist(ctx, q, scope, run.ID, collected[i], status); perr != nil {
			return res, perr
		}
		res.Findings++
		switch status {
		case "auto_fixed":
			res.AutoFixed++
		case "held":
			res.Held++
		case "admin_pending":
			res.AdminQueued++
		}
		if collected[i].Class == ClassOperator {
			res.Operator++
		}
	}

	summary, _ := json.Marshal(map[string]any{
		"findings": res.Findings, "auto_fixed": res.AutoFixed,
		"held": res.Held, "admin_queued": res.AdminQueued, "operator": res.Operator,
	})
	if _, err := q.FinishReconciliationRun(ctx, gen.FinishReconciliationRunParams{
		ID: run.ID, Status: "completed", Summary: summary,
	}); err != nil {
		return res, fmt.Errorf("converge: finish run: %w", err)
	}
	return res, nil
}

// remediate decides a finding's ledger status and applies its repair. The
// confirmed-absence gate (§3.2) holds a destructive EXCESS repair as `held` until
// its source domain is proven fully reconciled for the merchant.
func (e *ConvergeEngine) remediate(ctx context.Context, scope Scope, f ConvergeFinding) (string, error) {
	if f.Shape == ShapeExcess && f.SourceDomain != DomainNone {
		reconciled, err := e.DB.Gen(ctx).IsSourceDomainReconciled(ctx, gen.IsSourceDomainReconciledParams{
			MerchantID: scope.Merchant.UUID(), SourceDomain: string(f.SourceDomain),
		})
		if err != nil {
			return "", fmt.Errorf("converge: gate check %s: %w", f.SourceDomain, err)
		}
		if reconciled == nil || !*reconciled {
			return "held", nil // confirmed-absence gate: do not retract yet
		}
	}

	switch f.Class {
	case ClassAuto:
		if f.Repair != nil {
			if err := f.Repair(ctx); err != nil {
				return "", fmt.Errorf("converge: repair %s (%s): %w", f.Type, f.SubjectKey, err)
			}
			return "auto_fixed", nil
		}
		return "open", nil
	case ClassAdmin, ClassOperator:
		return "admin_pending", nil // queued for human action (requires_admin)
	default:
		return "open", nil
	}
}

// persist upserts a finding into the shared ledger. finding_type is the
// self-describing qualified slug; shape/class + evidence ride in local_evidence.
func (e *ConvergeEngine) persist(ctx context.Context, q *gen.Queries, scope Scope, runID uuid.UUID, f ConvergeFinding, status string) error {
	provider := f.Provider
	if provider == "" {
		provider = "self"
	}
	meta := map[string]any{
		"shape": string(f.Shape), "class": string(f.Class),
	}
	if f.SourceDomain != DomainNone {
		meta["source_domain"] = string(f.SourceDomain)
	}
	for k, v := range f.Evidence {
		meta[k] = v
	}
	evidence, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("converge: marshal evidence %s: %w", f.Type, err)
	}
	_, err = q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
		MerchantID:    scope.Merchant.UUID(),
		Provider:      provider,
		FindingType:   f.Type,
		SubjectKey:    f.SubjectKey,
		Severity:      string(f.Severity),
		Status:        status,
		RequiresAdmin: status == "admin_pending",
		LocalEvidence: evidence,
		RunID:         runID,
	})
	if err != nil {
		return fmt.Errorf("converge: upsert finding %s (%s): %w", f.Type, f.SubjectKey, err)
	}
	return nil
}
