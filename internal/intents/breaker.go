package intents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/reconcile/recommend"
)

// Destructive intent types irreversibly destroy provider-side billing state.
// The volume breaker (#679) gates ONLY these; add future destructive types here.
var destructiveIntentTypes = map[string]struct{}{
	TypeNMIDeleteSubscription:    {},
	TypeCCBillCancelSubscription: {}, // #696: stops rebilling irreversibly (no resume API)
}

// IsDestructiveIntentType reports whether the type is breaker-gated.
func IsDestructiveIntentType(intentType string) bool {
	_, ok := destructiveIntentTypes[intentType]
	return ok
}

// DestructiveIntentTypes returns the gated types (sorted, for SQL ANY params).
func DestructiveIntentTypes() []string {
	out := make([]string, 0, len(destructiveIntentTypes))
	for t := range destructiveIntentTypes {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// #679 breaker budget: per merchant, at most max(floor, pct% of active
// subscriptions) destructive executions per rolling window. Constants, not
// config knobs — routine churn stays automatic, mass deletion is an incident.
const (
	DestructiveWindow          = 24 * time.Hour
	DestructiveBudgetFloor     = 25
	DestructiveBudgetActivePct = 1 // percent of the merchant's active subs
)

// HeldBulkFindingType is the operator finding a tripped breaker raises
// (string literal here — intents must not import internal/reconcile).
const HeldBulkFindingType = "life.provider_intent.held_bulk"

// heldBulkSubjectKey keys ONE standing finding per merchant across all
// destructive types (uq_reconciliation_findings_identity).
const heldBulkSubjectKey = "destructive_volume"

// DestructiveBudget is the breaker's window budget for a merchant with the
// given number of active subscriptions.
func DestructiveBudget(activeSubscriptions int64) int64 {
	pct := activeSubscriptions * DestructiveBudgetActivePct / 100
	if pct < DestructiveBudgetFloor {
		return DestructiveBudgetFloor
	}
	return pct
}

// VolumeBreaker halts destructive intent execution for a merchant when the
// rolling-window execution count exceeds the budget (#679). Tripping upserts
// ONE requires_review finding per merchant; execution stays halted while that
// finding is OPEN. Operator ack (fixed) resumes — the count window restarts at
// the resolution instant, so the rolling window decides if it trips again.
// Operator dismiss (ignored) silences the breaker for the merchant (findings
// semantics: an ignored identity stays ignored).
type VolumeBreaker struct {
	db *db.DB
}

func NewVolumeBreaker(d *db.DB) *VolumeBreaker { return &VolumeBreaker{db: d} }

// Check reports whether the destructive intent may execute now. held=true
// means the executor must park it (stays pending). Fails closed: a check
// error parks the intent rather than executing unexamined.
func (b *VolumeBreaker) Check(ctx context.Context, intent gen.OpenrailsRailIntent, now time.Time) (held bool, reason string, err error) {
	if b == nil || b.db == nil {
		return false, "", fmt.Errorf("volume breaker: db not configured")
	}
	q := b.db.Gen(ctx)

	windowStart := now.Add(-DestructiveWindow)
	finding, found, err := b.finding(ctx, intent.MerchantID)
	if err != nil {
		return false, "", fmt.Errorf("volume breaker: load held_bulk finding: %w", err)
	}
	if found {
		switch finding.Status {
		case "reconcile_required", "requires_review":
			return true, "destructive execution halted: open " + HeldBulkFindingType + " finding awaits operator resolution", nil
		case "ignored":
			// Operator explicitly silenced the breaker for this merchant.
			return false, "", nil
		default: // fixed / auto_fixed — resumed; count only executions since.
			if finding.ResolvedAt != nil && finding.ResolvedAt.After(windowStart) {
				windowStart = *finding.ResolvedAt
			}
		}
	}

	executed, err := q.CountDestructiveRailIntentsExecutedSince(ctx, gen.CountDestructiveRailIntentsExecutedSinceParams{
		MerchantID:  intent.MerchantID,
		IntentTypes: DestructiveIntentTypes(),
		Since:       windowStart.UTC(),
	})
	if err != nil {
		return false, "", fmt.Errorf("volume breaker: count executed: %w", err)
	}
	active, err := q.CountActiveSubscriptionsByMerchant(ctx, intent.MerchantID)
	if err != nil {
		return false, "", fmt.Errorf("volume breaker: count active subscriptions: %w", err)
	}
	budget := DestructiveBudget(active)
	if executed < budget {
		return false, "", nil
	}

	// Over budget: raise/refresh the operator finding, then hold. The #692
	// structured recommendation (ack_resume) lets the findings queue approve
	// mechanically — resolution alone re-arms the breaker.
	evidence, merr := json.Marshal(map[string]any{
		"provider":             intent.Provider,
		"intent_type":          intent.IntentType,
		"executed_count":       executed,
		"budget":               budget,
		"window_hours":         int(DestructiveWindow / time.Hour),
		"active_subscriptions": active,
		"held_at":              now.UTC().Format(time.RFC3339),
		recommend.EvidenceKey:  recommend.Recommendation{Action: recommend.ActionAckResume}.Map(),
	})
	if merr != nil {
		return false, "", fmt.Errorf("volume breaker: marshal evidence: %w", merr)
	}
	action := fmt.Sprintf(
		"%d destructive provider intents in %s exceeded budget %d — review the cohort; approve (ack) to resume destructive execution for the merchant",
		executed, DestructiveWindow, budget)
	if _, uerr := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
		MerchantID:        intent.MerchantID,
		FindingType:       HeldBulkFindingType,
		SubjectKey:        heldBulkSubjectKey,
		Severity:          "critical",
		Status:            "requires_review",
		RecommendedAction: &action,
		Evidence:          evidence,
		RunID:             uuid.Nil, // raised by the intent executor, not a reconcile run
	}); uerr != nil {
		return false, "", fmt.Errorf("volume breaker: upsert held_bulk finding: %w", uerr)
	}
	return true, fmt.Sprintf(
		"destructive execution halted: %d destructive intents executed in the last %s exceeds budget %d (max(%d, %d%% of %d active subs)); %s finding raised",
		executed, DestructiveWindow, budget, DestructiveBudgetFloor, DestructiveBudgetActivePct, active, HeldBulkFindingType,
	), nil
}

func (b *VolumeBreaker) finding(ctx context.Context, merchantID uuid.UUID) (gen.OpenrailsReconciliationFinding, bool, error) {
	row, err := b.db.Gen(ctx).GetReconciliationFindingByIdentity(ctx, gen.GetReconciliationFindingByIdentityParams{
		MerchantID:  merchantID,
		FindingType: HeldBulkFindingType,
		SubjectKey:  heldBulkSubjectKey,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return gen.OpenrailsReconciliationFinding{}, false, nil
		}
		return gen.OpenrailsReconciliationFinding{}, false, err
	}
	return row, true, nil
}
