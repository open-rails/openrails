package subscriptions

import (
	"context"
	"fmt"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/entitlements"
)

// EngineRenewalGrace keeps an engine member's access across the paid-period
// boundary until the engine's own renewal decides. A qualified renewal
// supersedes it; a decline, cancellation or terminal outcome revokes it. If
// no outcome ever arrives (the fleet is down), access still ends when it
// expires, so engine access stays bounded by paid time plus this allowance.
const EngineRenewalGrace = 24 * time.Hour

type graceWriter interface {
	PushNewEntitlement(context.Context, entitlements.PushNewEntitlementParams) (*models.Entitlement, error)
}

// pushEngineRenewalGrace appends the renewal allowance after periodEnd for a
// live engine-owned subscription. Other collection policies project their own
// access (provider cohorts hold standing windows).
func pushEngineRenewalGrace(ctx context.Context, ent graceWriter, sub *models.Subscription, names []string, periodEnd time.Time) error {
	if ent == nil || sub == nil || sub.CollectionPolicy != models.CollectionPolicyEngine || sub.CancelledAt != nil || sub.Status == models.StatusCancelled {
		return nil
	}
	start := periodEnd.UTC()
	end := start.Add(EngineRenewalGrace)
	for _, name := range names {
		if _, err := ent.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{UserID: sub.CustomerID.String(), Entitlement: name, NotBefore: &start, EndAt: &end, SourceType: models.EntitlementSourceGrace, SourceID: sub.ID}); err != nil {
			return fmt.Errorf("grant engine renewal grace %s: %w", name, err)
		}
	}
	return nil
}

func entitlementNames(spec map[string]*int) []string {
	names := make([]string, 0, len(spec))
	for name := range spec {
		names = append(names, name)
	}
	return names
}
