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
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/failpoint"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// The Convergence Engine is the single idempotent driver of the internal
// DERIVE / CON / NOTIFY planes and the merchant's health findings. A
// customer's job runs it after each mutation that changed the customer
// (convergejob, queued in that mutation's transaction); a provider pull runs
// it merchant-wide, and the hourly health pass runs Health for merchants with
// activity. A converged scope emits no findings and every repair is a no-op,
// so re-running changes nothing.

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

// Scope narrows a Converge pass to a merchant, or to one of its customers.
type Scope struct {
	Merchant billing.MerchantID
	Customer *uuid.UUID
}

// ConvergeFinding is one divergence emitted by a plane pass. Repair, when set,
// is the idempotent local write that fixes an AUTO finding; nil means
// surface-only. RecommendedAction is the operator prose for ADMIN findings;
// the machine-executable shape rides in Evidence under recommend.EvidenceKey.
type ConvergeFinding struct {
	Type       string // finding_type, e.g. "derive.grant.excess"
	Shape      Shape
	Class      Class
	Severity   Severity
	SubjectKey string // stable per-finding identity within (merchant, provider, type)
	Provider   string // "self" for internal planes; a provider name for PULL
	// Retracts marks a repair of another shape that closes access, so the
	// destructive gate holds it as it holds every EXCESS repair.
	Retracts          bool
	Evidence          map[string]any
	RecommendedAction string                          // prose; "" = none
	Repair            func(ctx context.Context) error // AUTO repair; nil = surface only
}

// Pass is one diagnostic plane. Converge runs DERIVE then CON, and the
// NOTIFY stage after their repairs, on the converged state.
type Pass interface {
	Plane() string
	Run(ctx context.Context, scope Scope) ([]ConvergeFinding, error)
	// Standing lists the finding types a Run reports in full for its scope:
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
// shared reconciliation_findings ledger, applying AUTO repairs; the
// destructive gate holds every retraction.
type ConvergeEngine struct {
	DB  *db.DB
	Now func() time.Time
	// Notifier bridges persisted findings into the operator notification
	// store. Optional; nil is a no-op. Set directly on the constructed engine.
	Notifier reconcile.FindingNotifier

	passes []Pass
	// notify is not in passes: it detects on the state the repairs just left
	// (a window DERIVE re-projected this run never emails), so it runs after
	// the remediation loop.
	notify *notifyPass
	health *healthPass
}

// NewConvergeEngine wires the engine with the DERIVE → CON passes, the
// post-repair NOTIFY stage and the health pass. It reads the caller's clock
// when one is given.
func NewConvergeEngine(database *db.DB, clocks ...clockwork.Clock) *ConvergeEngine {
	clock := timeutil.FirstClock(clocks...)
	e := &ConvergeEngine{DB: database, Now: func() time.Time { return clock.Now().UTC() }}
	e.passes = []Pass{&derivePass{e: e}, &conPass{e: e}}
	e.notify = &notifyPass{e: e, lookback: accessEndedLookback}
	e.health = &healthPass{e: e}
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
	// Held counts the retractions the destructive gate refused; HeldReason is why.
	Held       int
	HeldReason string
	RunID      *uuid.UUID
}

// Converge runs DERIVE then CON for the scope, persists and remediates each
// finding, then runs NOTIFY on the converged state. Must run merchant-scoped.
// A clean scope does no writes besides closing what cleared.
func (e *ConvergeEngine) Converge(ctx context.Context, scope Scope) (ConvergeResult, error) {
	standing := e.notify.Standing()
	for _, p := range e.passes {
		standing = append(standing, p.Standing()...)
	}
	return e.run(ctx, scope, func(ctx context.Context) ([]ConvergeFinding, error) {
		var collected []ConvergeFinding
		for _, p := range e.passes {
			fs, err := p.Run(ctx, scope)
			if err != nil {
				return nil, fmt.Errorf("converge %s pass: %w", p.Plane(), err)
			}
			collected = append(collected, fs...)
		}
		return collected, nil
	}, e.notify, standing)
}

// Health evaluates the merchant's health findings (recovery funnel, held
// renewals, payment health, the unverified backlog, stuck and abandoned
// provider operations) and tells customers whose timed access lapsed.
func (e *ConvergeEngine) Health(ctx context.Context, merchantID billing.MerchantID) (ConvergeResult, error) {
	scope := Scope{Merchant: merchantID}
	return e.run(ctx, scope, func(ctx context.Context) ([]ConvergeFinding, error) {
		return e.health.Run(ctx, scope)
	}, &notifyPass{e: e, lookback: accessLapseLookback}, e.health.Standing())
}

// run persists and remediates what collect reports, then what notify finds
// on the converged state, and closes the open standing findings of the scope
// it no longer reports.
func (e *ConvergeEngine) run(ctx context.Context, scope Scope, collect func(context.Context) ([]ConvergeFinding, error), notify *notifyPass, standing []string) (res ConvergeResult, runErr error) {
	res.Scope = scope
	if scope.Merchant.UUID() == uuid.Nil {
		return res, fmt.Errorf("converge: scope.Merchant required")
	}
	scans := &scanState{truncated: map[string]bool{}}
	ctx = context.WithValue(ctx, scanStateKey{}, scans)
	// A pass closes what it no longer sees, but only what was last seen
	// before it began to look (resolveCleared).
	observedFrom, err := e.DB.Gen(ctx).ConvergeObservedFrom(ctx)
	if err != nil {
		return res, fmt.Errorf("converge: read the database clock: %w", err)
	}
	collected, err := collect(ctx)
	if err != nil {
		return res, err
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
			"held": res.Held,
		})
		if err := (&reconcile.PGStore{DB: e.DB}).FinishRun(ctx, *runID, status, summary, reason); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("converge: finish run: %w", err))
		}
	}()
	// Every retraction (cancel, revoke, close) asks the destructive gate; one
	// it refuses stays reconcile_required, the reason in evidence.
	hold := &retractionHold{merchant: scope.Merchant.UUID(), gate: destructive.New(e.DB)}
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
			status, held, err := e.remediate(ctx, hold, findings[i])
			if err != nil {
				return err
			}
			if held != "" {
				res.Held++
				res.HeldReason = held
			}
			row, perr := e.persist(ctx, q, scope, *runID, findings[i], status, held)
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
	notifyFindings, err := notify.Run(ctx, scope)
	if err != nil {
		return res, fmt.Errorf("converge %s pass: %w", notify.Plane(), err)
	}
	if err := apply(notifyFindings); err != nil {
		return res, err
	}
	if err := e.resolveCleared(ctx, q, scope, scans, observedFrom, standing, append(collected, notifyFindings...)); err != nil {
		return res, err
	}

	completed = true
	return res, nil
}

// resolveCleared closes the open standing findings of the scope the pass no
// longer reports: their subject has cleared. A customer's findings carry
// customer_id in their evidence. A finding another pass saw after
// observedFrom stays open: this pass's scan is older than that sighting.
func (e *ConvergeEngine) resolveCleared(ctx context.Context, q *gen.Queries, scope Scope, scans *scanState, observedFrom time.Time, standing []string, reported []ConvergeFinding) error {
	var types []string
	for _, t := range standing {
		if !scans.truncated[t] {
			types = append(types, t)
		}
	}
	keep := make([]string, 0, len(reported))
	for _, f := range reported {
		keep = append(keep, f.Type+findingKeySep+f.SubjectKey)
	}
	var err error
	if scope.Customer != nil {
		_, err = q.ResolveClearedCustomerFindings(ctx, gen.ResolveClearedCustomerFindingsParams{
			MerchantID: scope.Merchant.UUID(), FindingTypes: types, CustomerID: scope.Customer.String(), ObservedFrom: observedFrom, Keep: keep,
		})
	} else {
		_, err = q.ResolveClearedFindings(ctx, gen.ResolveClearedFindingsParams{
			MerchantID: scope.Merchant.UUID(), FindingTypes: types, ObservedFrom: observedFrom, Keep: keep,
		})
	}
	if err != nil {
		return fmt.Errorf("converge: resolve cleared findings: %w", err)
	}
	return nil
}

// findingKeySep joins a finding's type and subject (ResolveClearedFindings).
const findingKeySep = "\x1f"

// retractionHold reads the destructive gate at most once per run, at the
// first retraction.
type retractionHold struct {
	gate     *destructive.Gate
	merchant uuid.UUID
	read     bool
	reason   string
}

// why is the reason the merchant's retractions are held, or "".
func (h *retractionHold) why(ctx context.Context) string {
	if h.gate == nil {
		return ""
	}
	if !h.read {
		if v := h.gate.Check(ctx, h.merchant); !v.Allowed {
			h.reason = v.Reason
		}
		h.read = true
	}
	return h.reason
}

// remediate decides a finding's ledger status and applies its repair. The
// destructive gate keeps a retraction in reconcile_required while it refuses;
// held is its reason.
func (e *ConvergeEngine) remediate(ctx context.Context, hold *retractionHold, f ConvergeFinding) (status, held string, err error) {
	switch f.Class {
	case ClassAuto:
		if f.Repair != nil {
			if f.Shape == ShapeExcess || f.Retracts {
				if why := hold.why(ctx); why != "" {
					return "reconcile_required", why, nil
				}
			}
			if err := failpoint.Hit(ctx, failpoint.Site{Point: failpoint.BeforeRepair, Kind: f.Type, Subscription: subjectSubscription(f.SubjectKey)}); err != nil {
				return "", "", err
			}
			if err := f.Repair(ctx); err != nil {
				return "", "", fmt.Errorf("converge: repair %s (%s): %w", f.Type, f.SubjectKey, err)
			}
			return "auto_fixed", "", nil
		}
		return "reconcile_required", "", nil
	default:
		return "requires_review", "", nil // queued for human review
	}
}

// persist upserts a finding into the shared ledger and returns the row for a
// FindingNotifier. finding_type is the qualified slug; shape/class, the hold
// and details ride in evidence.
func (e *ConvergeEngine) persist(ctx context.Context, q *gen.Queries, scope Scope, runID uuid.UUID, f ConvergeFinding, status, held string) (gen.BillingReconciliationFinding, error) {
	meta := map[string]any{
		"shape": string(f.Shape), "class": string(f.Class),
	}
	for k, v := range f.Evidence {
		meta[k] = v
	}
	if held != "" {
		meta["held_by"], meta["held_reason"] = "kill_switch", held
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
