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
// The volume breaker gates only these.
var destructiveIntentTypes = map[string]struct{}{
	TypeNMIDeleteSubscription:    {},
	TypeCCBillCancelSubscription: {}, // stops rebilling irreversibly (no resume API)
	// Deleting a vaulted card is irreversible (only the cardholder can re-enter
	// it). Held deletes stay pending until operator ack. Decline-cleanup deletes
	// bypass the intent log (paymentmethods.CleanupPaymentMethodBestEffort), so
	// card-testing floods cannot burn this budget. Only an explicit delete
	// request produces this type; bulk production would itself be the incident.
	TypeNMIPaymentMethodDelete:  {},
	TypeHyperSwitchMethodDelete: {},
	// TypeNMIPaymentSourceUpdate is not listed: repointing a subscription's
	// vaulted card destroys nothing (both vaults survive).
	//
	// TypeStripeCancelSubscription is not listed: it only follows a revoke of
	// access, and holding it would leave Stripe billing a member who has none.
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

// Breaker budget: per merchant, at most max(floor, pct% of active
// subscriptions) destructive executions per rolling window. Constants, not
// config: routine churn stays automatic, mass deletion is an incident.
const (
	DestructiveWindow          = 24 * time.Hour
	DestructiveBudgetFloor     = 25
	DestructiveBudgetActivePct = 1 // percent of the merchant's active subs
)

// HeldBulkFindingType is the operator finding a tripped breaker raises
// (string literal here — intents must not import internal/reconcile).
const HeldBulkFindingType = "life.provider_intent.held_bulk"

// heldBulkSubjectKey keys ONE standing finding per merchant across all
// destructive types (reconciliation_findings_finding_type_psp_id_subject_key_key).
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
// rolling-window execution count exceeds the budget. Tripping upserts ONE
// requires_review finding per merchant; execution stays halted while that
// finding is open. Operator ack (fixed) resumes, with the count window
// restarting at the resolution instant. Operator dismiss (ignored) silences
// the breaker for the merchant.
type VolumeBreaker struct {
	db *db.DB
}

func NewVolumeBreaker(d *db.DB) *VolumeBreaker { return &VolumeBreaker{db: d} }

// Check reports whether the destructive intent may execute now. held=true
// means the executor must park it (stays pending). Fails closed: a check
// error parks the intent rather than executing unexamined. Checks are
// serialized per merchant, and an admitted intent's attempt is recorded by
// admit in the same transaction, so concurrent executors cannot spend one
// remaining budget twice.
func (b *VolumeBreaker) Check(ctx context.Context, intent gen.BillingProviderIntent, now time.Time, admit func(context.Context, *db.DB) error) (held bool, reason string, err error) {
	if b == nil || b.db == nil {
		return false, "", fmt.Errorf("volume breaker: db not configured")
	}
	// Fail closed on an unpinned or wrong-merchant connection: the caller parks
	// the intent.
	if err := b.db.AssertMerchantScope(ctx, "destructive-volume breaker"); err != nil {
		return false, "", err
	}
	err = b.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := b.db.NewWithPgxTx(tx)
		if err := d.Gen(ctx).LockDestructiveBreaker(ctx, intent.MerchantID); err != nil {
			return fmt.Errorf("volume breaker: lock: %w", err)
		}
		if held, reason, err = b.check(ctx, d, intent, now); err != nil || held {
			return err
		}
		return admit(ctx, d)
	})
	if err != nil {
		return false, "", err
	}
	return held, reason, nil
}

func (b *VolumeBreaker) check(ctx context.Context, d *db.DB, intent gen.BillingProviderIntent, now time.Time) (held bool, reason string, err error) {
	q := d.Gen(ctx)

	windowStart := now.Add(-DestructiveWindow)
	finding, found, err := b.finding(ctx, d, intent.MerchantID)
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

	executed, err := q.CountDestructiveProviderIntentsExecutedSince(ctx, gen.CountDestructiveProviderIntentsExecutedSinceParams{
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

	// Over budget: raise or refresh the operator finding, then hold. The
	// ack_resume recommendation lets the findings queue approve mechanically;
	// resolution alone re-arms the breaker.
	evidence, merr := json.Marshal(map[string]any{
		"provider":             intent.Rail,
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
		RunID:             nil, // raised by the intent executor, not a reconcile run
	}); uerr != nil {
		return false, "", fmt.Errorf("volume breaker: upsert held_bulk finding: %w", uerr)
	}
	return true, fmt.Sprintf(
		"destructive execution halted: %d destructive intents executed in the last %s exceeds budget %d (max(%d, %d%% of %d active subs)); %s finding raised",
		executed, DestructiveWindow, budget, DestructiveBudgetFloor, DestructiveBudgetActivePct, active, HeldBulkFindingType,
	), nil
}

func (b *VolumeBreaker) finding(ctx context.Context, d *db.DB, merchantID uuid.UUID) (gen.BillingReconciliationFinding, bool, error) {
	row, err := d.Gen(ctx).GetReconciliationFindingByIdentity(ctx, gen.GetReconciliationFindingByIdentityParams{
		MerchantID:  merchantID,
		FindingType: HeldBulkFindingType,
		SubjectKey:  heldBulkSubjectKey,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return gen.BillingReconciliationFinding{}, false, nil
		}
		return gen.BillingReconciliationFinding{}, false, err
	}
	return row, true, nil
}
