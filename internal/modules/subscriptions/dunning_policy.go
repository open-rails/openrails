package subscriptions

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
)

// DunningPolicy is the merchant's dunning schedule, read on the caller's
// merchant-scoped handle.
func DunningPolicy(ctx context.Context, d *db.DB) (collection.Policy, error) {
	cfg, _, err := merchantconfig.NewStore(d).Get(ctx)
	if err != nil {
		return collection.Policy{}, fmt.Errorf("load dunning policy: %w", err)
	}
	return collection.PolicyOf(cfg.DunningPolicy)
}

// CasePolicy is the policy a membership's dunning case runs under: the one
// recorded when it opened, else the merchant's current policy.
func CasePolicy(ctx context.Context, d *db.DB, sub *models.Subscription) (collection.Policy, error) {
	if sub != nil && len(sub.DunningPolicy) > 0 {
		var declared billing.DunningPolicy
		if err := json.Unmarshal(sub.DunningPolicy, &declared); err != nil {
			return collection.Policy{}, fmt.Errorf("subscription %s dunning policy: %w", sub.ID, err)
		}
		return collection.PolicyOf(&declared)
	}
	return DunningPolicy(ctx, d)
}

// openCase records the policy a dunning case opens under, once.
func openCase(ctx context.Context, d *db.DB, sub *models.Subscription) error {
	if len(sub.DunningPolicy) > 0 {
		return nil
	}
	policy, err := DunningPolicy(ctx, d)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(collection.DeclaredOf(policy))
	if err != nil {
		return err
	}
	sub.DunningPolicy = raw
	return nil
}
