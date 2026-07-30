package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// DestructiveRunKindConvergeEnforce is the converge-enforce pass's key in
// openrails.destructive_runs — the SAME ledger `--prune` writes to (or#859
// §5.1), never a second one.
const DestructiveRunKindConvergeEnforce = "converge_enforce"

// supersededByReverseReason is stamped on every intent the reverse neutralises.
const supersededByReverseReason = "superseded by destructive-run reversal"

// DestructiveRunRecorder is what makes an enforce pass reversible. The pull
// engine holds one; without it a pass that would overwrite subscription state
// REFUSES rather than doing undoable damage (obligation 4 of or#859 §5.1: no
// bypass).
//
// It is an interface for one reason only — the engine is otherwise built from
// interfaces and its unit tests construct it by literal. Every production path
// gets the DB-backed implementation from NewEngine.
type DestructiveRunRecorder interface {
	// Open records the run BEFORE anything is written, carrying the coverage
	// proof that authorised it and the row count the pass predicted.
	Open(ctx context.Context, params OpenDestructiveRunParams) (uuid.UUID, error)
	// CaptureSubscription stores the subscription row and its live entitlement
	// windows exactly as they stand, stamped with the run. Returns the capture
	// instant, which bounds intent attribution for that subject.
	CaptureSubscription(ctx context.Context, runID, subscriptionID uuid.UUID) (time.Time, error)
	// StampIntents attributes provider writes queued for the subject since
	// `since` to the run, so the reverse can supersede the unfired ones.
	StampIntents(ctx context.Context, runID, subscriptionID uuid.UUID, since time.Time) (int, error)
	// Finish closes the run with its per-table actual counts.
	Finish(ctx context.Context, runID uuid.UUID, status string, affected map[string]int) error
}

// OpenDestructiveRunParams is one converge-enforce pass's run header.
type OpenDestructiveRunParams struct {
	// PspID is set when the pass is account-bound; NULL means merchant-wide.
	PspID *uuid.UUID
	Kind  string
	Actor string
	// Coverage is the SnapshotCoverage absence proof that authorised the pass,
	// stored verbatim. A destructive run is only as trustworthy as its proof.
	Coverage any
	// ExpectedRows is the count the pass predicted before mutating anything.
	// For an operator prune this is a typed confirmation; for converge-enforce
	// it is the plan, and a finished run whose `affected` disagrees with it is
	// evidence that something wrote outside the plan.
	ExpectedRows int
	Note         string
}

// PGDestructiveRunRecorder is the production recorder.
type PGDestructiveRunRecorder struct{ DB *db.DB }

func (r *PGDestructiveRunRecorder) Open(ctx context.Context, params OpenDestructiveRunParams) (uuid.UUID, error) {
	mid, err := requireMerchantUUID(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	var coverage []byte
	if params.Coverage != nil {
		if coverage, err = json.Marshal(params.Coverage); err != nil {
			return uuid.Nil, fmt.Errorf("marshal coverage proof: %w", err)
		}
	}
	actor := params.Actor
	if actor == "" {
		actor = "unknown"
	}
	runID := uuid.New()
	expected := int64(params.ExpectedRows)
	var note *string
	if params.Note != "" {
		note = &params.Note
	}
	if _, err := r.DB.Gen(ctx).CreateDestructiveRun(ctx, gen.CreateDestructiveRunParams{
		ID: runID, MerchantID: mid, PspID: params.PspID, Kind: params.Kind,
		Actor: actor, DryRun: false, Coverage: coverage, ExpectedRows: &expected, Note: note,
	}); err != nil {
		return uuid.Nil, fmt.Errorf("open destructive run: %w", err)
	}
	return runID, nil
}

func (r *PGDestructiveRunRecorder) CaptureSubscription(ctx context.Context, runID, subscriptionID uuid.UUID) (time.Time, error) {
	mid, err := requireMerchantUUID(ctx)
	if err != nil {
		return time.Time{}, err
	}
	now := time.Now().UTC()
	q := r.DB.Gen(ctx)
	if _, err := q.CaptureSubscriptionBeforeImage(ctx, gen.CaptureSubscriptionBeforeImageParams{
		RunID: runID, MerchantID: mid, SubscriptionID: subscriptionID, Now: now,
	}); err != nil {
		return time.Time{}, fmt.Errorf("capture subscription before-image %s: %w", subscriptionID, err)
	}
	if _, err := q.CaptureSubscriptionEntitlementBeforeImages(ctx, gen.CaptureSubscriptionEntitlementBeforeImagesParams{
		RunID: runID, MerchantID: mid, SubscriptionID: subscriptionID, Now: now,
	}); err != nil {
		return time.Time{}, fmt.Errorf("capture entitlement before-images for %s: %w", subscriptionID, err)
	}
	return now, nil
}

func (r *PGDestructiveRunRecorder) StampIntents(ctx context.Context, runID, subscriptionID uuid.UUID, since time.Time) (int, error) {
	mid, err := requireMerchantUUID(ctx)
	if err != nil {
		return 0, err
	}
	n, err := r.DB.Gen(ctx).StampRailIntentsForRun(ctx, gen.StampRailIntentsForRunParams{
		RunID: runID, MerchantID: mid, SubscriptionID: subscriptionID, Since: since,
	})
	if err != nil {
		return 0, fmt.Errorf("stamp intents for %s: %w", subscriptionID, err)
	}
	return int(n), nil
}

func (r *PGDestructiveRunRecorder) Finish(ctx context.Context, runID uuid.UUID, status string, affected map[string]int) error {
	mid, err := requireMerchantUUID(ctx)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(affected)
	if _, err := r.DB.Gen(ctx).FinishDestructiveRun(ctx, gen.FinishDestructiveRunParams{
		MerchantID: mid, ID: runID, Status: status, Now: time.Now().UTC(), Affected: payload,
	}); err != nil {
		return fmt.Errorf("close destructive run %s: %w", runID, err)
	}
	return nil
}

// --- the reverse --------------------------------------------------------------

// IntentDivergence is one provider write a reversed run had queued, and what
// became of it.
type IntentDivergence struct {
	IntentID       uuid.UUID  `json:"intent_id"`
	IntentType     string     `json:"intent_type"`
	Rail           string     `json:"rail"`
	SubscriptionID *uuid.UUID `json:"subscription_id,omitempty"`
	Status         string     `json:"status"`
	ExecutedAt     *time.Time `json:"executed_at,omitempty"`
	// Consequence spells out, per intent type, what the provider side now looks
	// like. Only set for irreversible/ambiguous rows.
	Consequence string `json:"consequence,omitempty"`
}

// ConvergeRollbackResult is the reverse's report. Deliberately explicit about
// what it could NOT undo: an intent that already fired is divergence, and
// counting it as undone would be the single most dangerous lie this command
// could tell.
type ConvergeRollbackResult struct {
	RunID uuid.UUID `json:"run_id"`
	// SubscriptionsRestored is how many rows were re-asserted from before-images.
	SubscriptionsRestored int64 `json:"subscriptions_restored"`
	// EntitlementsCaptured is how many entitlement windows the run closed. They
	// are NOT restored — Converge recomputes them from the untouched grant log.
	EntitlementsCaptured int64 `json:"entitlements_captured"`
	// IntentsSuperseded is the unfired provider writes this reverse neutralised.
	IntentsSuperseded int `json:"intents_superseded"`
	// IntentsIrreversible already reached the provider. Operator work.
	IntentsIrreversible []IntentDivergence `json:"intents_irreversible,omitempty"`
	// IntentsAmbiguous were in flight or awaiting verification: they MAY have
	// reached the provider. Treated as irreversible until the verifier resolves.
	IntentsAmbiguous []IntentDivergence `json:"intents_ambiguous,omitempty"`
	// ProvenDomainsReset is how many reconciliation_state rows lost their
	// fully_reconciled flag (the confirmed-absence gate is now closed again).
	ProvenDomainsReset int64 `json:"proven_domains_reset"`
	// EnforcementDisarmed records that the merchant's first-enforce arming was
	// cleared, so the next pull runs advisory.
	EnforcementDisarmed bool `json:"enforcement_disarmed"`
}

// Complete reports whether the reversal restored everything it touched — i.e.
// nothing reached the provider before the reverse got there.
func (r ConvergeRollbackResult) Complete() bool {
	return len(r.IntentsIrreversible) == 0 && len(r.IntentsAmbiguous) == 0
}

// RollbackConvergeEnforceRun reverses one converge-enforce run.
//
// Five steps, in this order, and the order is the design (or#859 §5.2):
//
//  1. QUIESCE. Clear the merchant's first-enforce arming and trip its
//     destructive stop, so nothing re-cancels what is about to be restored and
//     no NEW intent claim starts mid-reversal.
//  2. SUPERSEDE THE UNFIRED INTENTS — FIRST, before a single row is restored,
//     because this is the only step racing a live actor. The intent runner may
//     claim a queued NMI vault delete at any moment; every millisecond spent
//     restoring rows first is a millisecond that race can be lost. Winning it
//     is what makes recovery from the mass-cancel COMPLETE rather than partial.
//  3. RESTORE the stamped rows from their before-images. Class A untouched;
//     Class D not restored.
//  4. RESET the confirmed-absence proof, so the post-rollback book — which is
//     definitionally incomplete — cannot license a mass retraction.
//  5. REPORT, including what could not be undone.
//
// Steps 2-4 share ONE transaction: a reversal that superseded the intents but
// failed to restore the rows would leave the operator worse off than before.
//
// Per or#859 §2.1 a rollback is not a complete operation — `rollback → pull →
// converge` is. Entitlements come back through that Converge, recomputed from
// the append-only grant log; they are never restored here. Must run
// merchant-scoped.
func RollbackConvergeEnforceRun(ctx context.Context, database *db.DB, runID uuid.UUID, actor string) (ConvergeRollbackResult, error) {
	res := ConvergeRollbackResult{RunID: runID}
	mid, err := requireMerchantUUID(ctx)
	if err != nil {
		return res, err
	}
	if actor == "" {
		actor = "unknown"
	}

	run, err := database.Gen(ctx).GetDestructiveRun(ctx, gen.GetDestructiveRunParams{MerchantID: mid, ID: runID})
	if err != nil {
		if isNoRows(err) {
			return res, fmt.Errorf("destructive run %s not found for this merchant", runID)
		}
		return res, fmt.Errorf("load destructive run %s: %w", runID, err)
	}
	if run.Kind != DestructiveRunKindConvergeEnforce {
		return res, fmt.Errorf("destructive run %s is kind %q, not %q", runID, run.Kind, DestructiveRunKindConvergeEnforce)
	}
	if run.Status == "reversed" {
		return res, fmt.Errorf("destructive run %s was already reversed", runID)
	}

	// --- step 1: quiesce -------------------------------------------------------
	// Outside the transaction on purpose: the stop must be visible to the intent
	// runner's own connection IMMEDIATELY, not at our commit.
	reason := fmt.Sprintf("reversing destructive run %s", runID)
	if err := database.Gen(ctx).DisarmMerchantEnforcement(ctx, gen.DisarmMerchantEnforcementParams{
		MerchantID: mid, UpdatedBy: &actor, Reason: &reason,
	}); err != nil {
		return res, fmt.Errorf("quiesce merchant before reversal: %w", err)
	}
	res.EnforcementDisarmed = true

	var manifest []gen.ListRailIntentsForRunRow
	if err := database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		tq := gen.New(tx)
		now := time.Now().UTC()

		// --- step 2: supersede the unfired intents, FIRST ---------------------
		superseded, e := tq.SupersedeUnfiredRailIntentsForRun(ctx, gen.SupersedeUnfiredRailIntentsForRunParams{
			MerchantID: mid, RunID: runID, Reason: supersededByReverseReason,
		})
		if e != nil {
			return fmt.Errorf("supersede unfired intents: %w", e)
		}
		res.IntentsSuperseded = len(superseded)

		// --- step 3: restore ---------------------------------------------------
		if res.SubscriptionsRestored, e = tq.RestoreSubscriptionsFromBeforeImages(ctx, gen.RestoreSubscriptionsFromBeforeImagesParams{
			MerchantID: mid, RunID: runID, Now: now,
		}); e != nil {
			return fmt.Errorf("restore subscriptions from before-images: %w", e)
		}
		if _, e = tq.MarkBeforeImagesRestored(ctx, gen.MarkBeforeImagesRestoredParams{
			MerchantID: mid, RunID: runID, TableName: "subscriptions", Now: now,
		}); e != nil {
			return fmt.Errorf("mark before-images restored: %w", e)
		}
		counts, e := tq.CountBeforeImagesForRun(ctx, gen.CountBeforeImagesForRunParams{MerchantID: mid, RunID: runID})
		if e != nil {
			return fmt.Errorf("count before-images: %w", e)
		}
		res.EntitlementsCaptured = counts.Entitlements

		// --- step 4: close the confirmed-absence gate --------------------------
		if res.ProvenDomainsReset, e = tq.ResetReconciliationStateUnproven(ctx, mid); e != nil {
			return fmt.Errorf("reset proven source domains: %w", e)
		}

		// The manifest is read INSIDE the transaction, after the supersede, so
		// every status in it is final with respect to this reversal.
		if manifest, e = tq.ListRailIntentsForRun(ctx, gen.ListRailIntentsForRunParams{MerchantID: mid, RunID: runID}); e != nil {
			return fmt.Errorf("read intent divergence manifest: %w", e)
		}
		if _, e = tq.MarkDestructiveRunReversed(ctx, gen.MarkDestructiveRunReversedParams{
			MerchantID: mid, ID: runID, Now: now, ReversedBy: actor,
		}); e != nil {
			return fmt.Errorf("mark run reversed: %w", e)
		}
		return nil
	}); err != nil {
		return ConvergeRollbackResult{RunID: runID, EnforcementDisarmed: res.EnforcementDisarmed}, err
	}

	// --- step 5: report --------------------------------------------------------
	for i := range manifest {
		m := &manifest[i]
		d := IntentDivergence{
			IntentID: m.ID, IntentType: m.IntentType, Rail: m.Rail,
			SubscriptionID: m.SubscriptionID, Status: m.Status, ExecutedAt: m.ExecutedAt,
		}
		switch m.Status {
		case "succeeded":
			d.Consequence = irreversibleConsequence(m.IntentType)
			res.IntentsIrreversible = append(res.IntentsIrreversible, d)
		case "in_flight", "unknown_needs_verify":
			d.Consequence = "may already have reached the provider (" + irreversibleConsequence(m.IntentType) + "); the verifier resolves it — treat as irreversible until it does"
			res.IntentsAmbiguous = append(res.IntentsAmbiguous, d)
		}
	}
	if !res.Complete() {
		log.WithContext(ctx).WithFields(log.Fields{
			"run_id":       runID,
			"irreversible": len(res.IntentsIrreversible),
			"ambiguous":    len(res.IntentsAmbiguous),
		}).Error("destructive-run reversal is INCOMPLETE: provider writes escaped before the reverse reached them")
	}
	return res, nil
}

func requireMerchantUUID(ctx context.Context) (uuid.UUID, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	return mid.UUID(), nil
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// irreversibleConsequence names the provider-side fact a fired intent created,
// per type. An operator reading "1 intent already fired" learns nothing; an
// operator reading "the NMI vault entry is gone — the customer must re-enter a
// card" knows what work they now own.
func irreversibleConsequence(intentType string) string {
	switch intentType {
	case "nmi_delete_subscription":
		return "the NMI recurring subscription and its vault entry are deleted at the gateway; the stored card is gone and the customer must re-enter one to resubscribe"
	default:
		return "the provider write was executed and cannot be recalled; verify the provider's current state before acting"
	}
}
