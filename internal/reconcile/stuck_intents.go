package reconcile

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
)

// PS-10 stuck thresholds (#107/#358 tie-in). HARDCODED by design (no-knobs
// policy):
const (
	// stuckActionableAge flags pending/failed_retryable intents: the executor
	// runs every minute, so a day-old unexecuted intent means provider
	// failures, bad credentials, a dead worker — or a deliberate park.
	stuckActionableAge = 24 * time.Hour
	// stuckVerifyAge flags in_flight/unknown_needs_verify intents: a healthy
	// verifier resolves unknowns in minutes, and an in_flight lease outliving
	// hours means a dead executor.
	stuckVerifyAge = 2 * time.Hour
)

// StuckIntent is the slice of a billing.provider_intents row the PS-10
// detector consumes.
type StuckIntent struct {
	ID                uuid.UUID
	Provider          string
	IntentType        string
	SubscriptionID    *uuid.UUID
	PaymentID         *uuid.UUID
	PriceID           *uuid.UUID
	Status            string
	Attempts          int32
	NextAttemptAt     time.Time
	Origin            string
	OriginReason      string
	LastFailureReason string
	ExpiresAt         *time.Time
	CreatedAt         time.Time
}

// StuckIntentSource lists the non-terminal provider intents older than the
// stuck cutoffs (LOCAL ledger read only — PS-10 never calls a provider). Nil
// on an Engine skips the PS-10 pass (advisory-only unit-test engines); every
// production wiring sets it.
type StuckIntentSource interface {
	// ListStuck returns intents stuck per the taxonomy: pending/
	// failed_retryable created at or before actionCutoff, and in_flight/
	// unknown_needs_verify created at or before verifyCutoff.
	ListStuck(ctx context.Context, actionCutoff, verifyCutoff time.Time) ([]StuckIntent, error)
}

// PGStuckIntentSource reads the intent ledger through the sqlc layer on a
// tenant-pinned connection.
type PGStuckIntentSource struct {
	DB *db.DB
}

var _ StuckIntentSource = (*PGStuckIntentSource)(nil)

func (s *PGStuckIntentSource) ListStuck(ctx context.Context, actionCutoff, verifyCutoff time.Time) ([]StuckIntent, error) {
	rows, err := s.DB.Gen(ctx).ListStuckProviderIntents(ctx, gen.ListStuckProviderIntentsParams{
		ActionCutoff: actionCutoff,
		VerifyCutoff: verifyCutoff,
	})
	if err != nil {
		return nil, err
	}
	out := make([]StuckIntent, 0, len(rows))
	for _, row := range rows {
		si := StuckIntent{
			ID:             row.ID,
			Provider:       row.Provider,
			IntentType:     row.IntentType,
			SubscriptionID: row.SubscriptionID,
			PaymentID:      row.PaymentID,
			PriceID:        row.PriceID,
			Status:         row.Status,
			Attempts:       row.Attempts,
			NextAttemptAt:  row.NextAttemptAt,
			Origin:         row.Origin,
			ExpiresAt:      row.ExpiresAt,
			CreatedAt:      row.CreatedAt,
		}
		if row.OriginReason != nil {
			si.OriginReason = *row.OriginReason
		}
		if row.LastFailureReason != nil {
			si.LastFailureReason = *row.LastFailureReason
		}
		out = append(out, si)
	}
	return out, nil
}

// isModeParkedReason reports whether an intent's last_failure_reason records a
// deliberate park by the operating mode or a kill switch. The executor records
// these via intents.GateExecution ("mode=readonly blocks all provider writes",
// "mode=limited blocks proactive (system-origin) provider writes") and the
// handlers' execution-time checks ("... (feature_flags.… kill switch)",
// "... (mode=readonly)"), so matching on the "mode=" and "kill switch" markers
// covers every mode/kill-switch park. Mode-parked is not broken: the executor
// drains the queue when the blocker lifts, so the finding is informational.
// Every OTHER park reason (unconfigured client, relevance-check failure) and
// every genuine failure stays admin-queued.
func isModeParkedReason(reason string) bool {
	return strings.Contains(reason, "mode=") || strings.Contains(reason, "kill switch")
}

// makeStuckIntentFinding diagnoses one stuck intent as a PS-10 finding.
// SubjectKey is the intent's id (stable identity per the
// (tenant, provider, finding_type, subject_key) convention); the finding
// carries the intent's own provider. No Apply ever: the executor/verifier own
// the intent, fix mode must not touch it.
func makeStuckIntentFinding(si *StuckIntent, now time.Time) Finding {
	age := now.Sub(si.CreatedAt)
	ev := map[string]any{
		"intent_id":       si.ID.String(),
		"intent_type":     si.IntentType,
		"provider":        si.Provider,
		"origin":          si.Origin,
		"status":          si.Status,
		"attempts":        si.Attempts,
		"created_at":      si.CreatedAt.Format(time.RFC3339),
		"next_attempt_at": si.NextAttemptAt.Format(time.RFC3339),
		"age":             age.Truncate(time.Minute).String(),
	}
	if si.SubscriptionID != nil {
		ev["subscription_id"] = si.SubscriptionID.String()
	}
	if si.PaymentID != nil {
		ev["payment_id"] = si.PaymentID.String()
	}
	if si.PriceID != nil {
		ev["price_id"] = si.PriceID.String()
	}
	if si.OriginReason != "" {
		ev["origin_reason"] = si.OriginReason
	}
	if si.LastFailureReason != "" {
		ev["last_failure_reason"] = si.LastFailureReason
	}
	if si.ExpiresAt != nil {
		ev["expires_at"] = si.ExpiresAt.Format(time.RFC3339)
	}

	f := Finding{
		Provider:       Provider(si.Provider),
		Type:           FindingStuckIntent,
		SubjectKey:     si.ID.String(),
		IntentEvidence: ev,
	}
	if isModeParkedReason(si.LastFailureReason) {
		f.Severity = SeverityLow
		f.Status = FindingStatusOpen
		f.RecommendedAction = "no admin action needed: the intent is parked by the operating mode / kill switch (see last_failure_reason); the executor drains it when the blocker lifts"
		return f
	}
	f.Severity = SeverityHigh
	f.Status = FindingStatusAdminPending
	f.RequiresAdmin = true
	f.RecommendedAction = fmt.Sprintf(
		"provider intent %s has sat %s for %s; investigate provider failures, credentials, or dead executor/verifier workers — the intent ledger owns the replay, reconcile never touches intents",
		si.IntentType, si.Status, age.Truncate(time.Minute))
	return f
}

// StuckIntentReport is the PS-10 section of the run summary: provider intents
// (#358 ledger) sitting non-terminal beyond the hardcoded stuck thresholds.
type StuckIntentReport struct {
	Total        int               `json:"total"`
	AdminQueued  int               `json:"admin_queued"`
	ModeParked   int               `json:"mode_parked"`
	ByIntentType map[string]int    `json:"by_intent_type,omitempty"`
	Intents      []StuckIntentLine `json:"intents,omitempty"`
	AutoResolved int64             `json:"auto_resolved"`
}

// StuckIntentLine is one stuck intent's report line, persisted in the run
// summary so `billing reconcile report` can render it later.
type StuckIntentLine struct {
	IntentID          string `json:"intent_id"`
	IntentType        string `json:"intent_type"`
	Provider          string `json:"provider"`
	Status            string `json:"status"`
	Origin            string `json:"origin"`
	Attempts          int32  `json:"attempts"`
	Age               string `json:"age"`
	LastFailureReason string `json:"last_failure_reason,omitempty"`
	ModeParked        bool   `json:"mode_parked,omitempty"`
}

// runStuckIntents is the PS-10 pass: it runs once per reconcile run AFTER the
// provider sections, regardless of provider filters (the subject is the local
// intent ledger; no provider calls). check and fix behave identically — no
// enforce applier exists for PS-10 by design. Findings absent from this run
// auto-resolve across ALL providers (the intent recovered or reached a
// terminal status).
func (e *Engine) runStuckIntents(ctx context.Context, runID uuid.UUID, result *RunResult) (*StuckIntentReport, error) {
	if e.Intents == nil {
		return nil, nil
	}
	now := e.now()
	stuck, err := e.Intents.ListStuck(ctx, now.Add(-stuckActionableAge), now.Add(-stuckVerifyAge))
	if err != nil {
		return nil, fmt.Errorf("list stuck intents: %w", err)
	}

	rep := &StuckIntentReport{}
	if len(stuck) > 0 {
		rep.ByIntentType = map[string]int{}
	}
	for i := range stuck {
		si := &stuck[i]
		f := makeStuckIntentFinding(si, now)
		rec, err := e.Store.UpsertFinding(ctx, runID, f)
		if err != nil {
			return rep, fmt.Errorf("persist finding %s/%s: %w", f.Type, f.SubjectKey, err)
		}
		result.Findings = append(result.Findings, rec)
		rep.Total++
		rep.ByIntentType[si.IntentType]++
		line := StuckIntentLine{
			IntentID:          si.ID.String(),
			IntentType:        si.IntentType,
			Provider:          si.Provider,
			Status:            si.Status,
			Origin:            si.Origin,
			Attempts:          si.Attempts,
			Age:               now.Sub(si.CreatedAt).Truncate(time.Minute).String(),
			LastFailureReason: si.LastFailureReason,
		}
		if f.RequiresAdmin {
			rep.AdminQueued++
		} else {
			rep.ModeParked++
			line.ModeParked = true
		}
		rep.Intents = append(rep.Intents, line)
	}

	resolved, err := e.Store.AutoResolveVanishedAllProviders(ctx, runID, []FindingType{FindingStuckIntent})
	if err != nil {
		return rep, fmt.Errorf("auto-resolve recovered stuck-intent findings: %w", err)
	}
	rep.AutoResolved = resolved
	return rep, nil
}
