package subscriptions

import (
	"context"
	"fmt"
	"time"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
)

// PolicyOf converts and validates a declared dunning policy. Nil is the
// built-in schedule.
func PolicyOf(declared *openrails.DunningPolicy) (collection.Policy, error) {
	if declared == nil {
		return collection.DefaultPolicy, nil
	}
	p := collection.Policy{}
	for _, t := range declared.Tiers {
		tier := collection.Tier{MaxCycle: time.Duration(t.MaxCycleHours) * time.Hour}
		for _, h := range t.RetryAfterHours {
			tier.Offsets = append(tier.Offsets, time.Duration(h)*time.Hour)
		}
		p.Tiers = append(p.Tiers, tier)
	}
	for _, m := range declared.TransientRetryMinutes {
		p.Transient = append(p.Transient, time.Duration(m)*time.Minute)
	}
	if err := p.Validate(); err != nil {
		return collection.Policy{}, fmt.Errorf("dunning_policy: %w", err)
	}
	return p, nil
}

// DunningPolicy is the merchant's dunning schedule, read on the caller's
// merchant-scoped handle.
func DunningPolicy(ctx context.Context, d *db.DB) (collection.Policy, error) {
	cfg, _, err := merchantconfig.NewStore(d).Get(ctx)
	if err != nil {
		return collection.Policy{}, fmt.Errorf("load dunning policy: %w", err)
	}
	return PolicyOf(cfg.DunningPolicy)
}
