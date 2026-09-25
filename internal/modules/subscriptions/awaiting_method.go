package subscriptions

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/open-rails/openrails/internal/billing/lifecycle"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
)

// ExpireAwaitingMethod ends a membership whose wait for a new card outlived
// its dunning window, as a spent schedule would. With the operator's
// destructive switch off the row keeps waiting, access intact, under a
// standing finding.
func (s *SubscriptionLifecycleService) ExpireAwaitingMethod(ctx context.Context, d *db.DB, sub *models.Subscription, blocked string) (bool, error) {
	now := s.now()
	if blocked != "" {
		evidence, _ := json.Marshal(map[string]any{"subscription_id": sub.ID, "collection_policy": sub.CollectionPolicy, "status": sub.Status, "refusal": blocked})
		action := fmt.Sprintf("subscription %s waited for a new payment method past its dunning window but ending it was refused (%s); it keeps access. Arm the destructive-action switch or resolve it by hand", sub.ID, blocked)
		_, err := d.Gen(ctx).UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{MerchantID: sub.MerchantID, FindingType: FindingTerminalHeld, SubjectKey: sub.ID.String(), Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: evidence})
		return false, err
	}
	return s.Decide(ctx, d, sub.ID, lifecycle.DunningExhausted{At: now}, func(_ context.Context, _ *db.DB, locked *models.Subscription) (bool, error) {
		return locked.Status == models.StatusAwaitingMethod && locked.GraceEndsAt != nil && !locked.GraceEndsAt.After(now), nil
	})
}
