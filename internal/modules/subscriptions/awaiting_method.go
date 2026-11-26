package subscriptions

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"time"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/lifecycle"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/modules/alerting/alertjob"
)

// ExpireAwaitingMethod ends a membership whose wait for a new card outlived
// its dunning window, as a spent schedule would: its ending's reason is
// expired. With the operator's destructive switch off the row keeps waiting,
// access intact, under a standing finding.
func (s *SubscriptionLifecycleService) ExpireAwaitingMethod(ctx context.Context, d *db.DB, sub *models.Subscription, blocked string) (bool, error) {
	now := s.now()
	if blocked != "" {
		held := TerminalHold{Reason: blocked, Remedy: SwitchRemedy}
		action := fmt.Sprintf("subscription %s waited for a new payment method past its dunning window but ending it was refused (%s); it keeps access. %s", sub.ID, blocked, SwitchRemedy)
		return false, s.raiseTerminalHeld(ctx, d, sub, held, action, nil, now)
	}
	return s.Decide(ctx, d, sub.ID, lifecycle.DunningExhausted{At: now}, EffectOptions{EndedReason: PremiumEndReasonExpired}, func(_ context.Context, _ *db.DB, locked *models.Subscription) (bool, error) {
		return locked.Status == models.StatusAwaitingMethod && locked.GraceEndsAt != nil && !locked.GraceEndsAt.After(now), nil
	})
}

// raiseTerminalHeld records a terminal outcome a hold refused as a standing
// finding on d, the caller's transaction, and alerts the merchant's operator
// once per case.
func (s *SubscriptionLifecycleService) raiseTerminalHeld(ctx context.Context, d *db.DB, sub *models.Subscription, held TerminalHold, action string, extra map[string]any, now time.Time) error {
	facts := map[string]any{"subscription_id": sub.ID, "collection_policy": sub.CollectionPolicy, "status": sub.Status, "refusal": held.Reason, "remedy": held.Remedy}
	maps.Copy(facts, extra)
	evidence, err := json.Marshal(facts)
	if err != nil {
		return err
	}
	row, err := d.Gen(ctx).UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{MerchantID: sub.MerchantID, FindingType: FindingTerminalHeld, SubjectKey: sub.ID.String(),
		Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: evidence})
	if err != nil {
		return fmt.Errorf("record held terminal outcome for %s: %w", sub.ID, err)
	}
	if err := alertjob.Notify(ctx, d, alertjob.FindingOf(row), now); err != nil && ctx.Err() == nil {
		s.Log.WarnContext(ctx, "alerting the operator of a held terminal outcome failed; the finding stays in the console", logx.Err(err), "subscription_id", sub.ID)
	}
	return nil
}

// EndAdviceDeferred ends, at the next renewal, a case the network's advice
// deferred there: exhausted, its ending issuer_deferred. With the operator's
// destructive switch off the row keeps waiting, access intact, under a
// standing finding.
func (s *SubscriptionLifecycleService) EndAdviceDeferred(ctx context.Context, d *db.DB, sub *models.Subscription, blocked string) (bool, error) {
	now := s.now()
	if blocked != "" {
		held := TerminalHold{Reason: blocked, Remedy: SwitchRemedy}
		action := fmt.Sprintf("the issuer deferred every retry of subscription %s past its next renewal, but ending it there was refused (%s); it keeps access. %s", sub.ID, blocked, SwitchRemedy)
		return false, s.raiseTerminalHeld(ctx, d, sub, held, action, nil, now)
	}
	return s.Decide(ctx, d, sub.ID, lifecycle.DunningExhausted{At: now}, EffectOptions{EndedReason: PremiumEndReasonExpired}, func(_ context.Context, _ *db.DB, locked *models.Subscription) (bool, error) {
		if !locked.AdviceDeferred() || locked.GraceEndsAt.After(now) {
			return false, nil
		}
		reason := CancelIssuerDeferred
		locked.CancelFeedback = &reason
		return true, nil
	})
}

// EndHeldOutcome ends an engine case whose terminal outcome waited for the
// merchant's destructive arming, as a spent schedule would.
func (s *SubscriptionLifecycleService) EndHeldOutcome(ctx context.Context, d *db.DB, sub *models.Subscription, blocked string) (bool, error) {
	now := s.now()
	if blocked != "" {
		held := TerminalHold{Reason: blocked, Remedy: SwitchRemedy}
		action := fmt.Sprintf("subscription %s reached a held terminal outcome but ending it was refused (%s); it keeps access. %s", sub.ID, blocked, SwitchRemedy)
		return false, s.raiseTerminalHeld(ctx, d, sub, held, action, nil, now)
	}
	return s.Decide(ctx, d, sub.ID, lifecycle.DunningExhausted{At: now}, EffectOptions{EndedReason: PremiumEndReasonExpired}, func(_ context.Context, _ *db.DB, locked *models.Subscription) (bool, error) {
		return locked.CollectionPolicy == models.CollectionPolicyEngine && locked.Status == models.StatusPastDue && locked.NextRetryAt == nil && locked.RetryAdvisedAt == nil && locked.GraceEndsAt != nil && !locked.GraceEndsAt.After(now), nil
	})
}
