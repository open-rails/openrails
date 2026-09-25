package subscriptions

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
)

// PolicyOf converts and validates a declared dunning policy. Nil is the
// built-in schedule; a policy that declares no tiers keeps the built-in ones.
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
	if len(declared.Tiers) == 0 {
		p.Tiers = collection.DefaultPolicy.Tiers
	}
	for _, m := range declared.TransientRetryMinutes {
		p.Transient = append(p.Transient, time.Duration(m)*time.Minute)
	}
	switch declared.AccessDuringDunning {
	case "", openrails.DunningAccessKeep:
	case openrails.DunningAccessSuspend:
		p.SuspendAccess = true
	default:
		return collection.Policy{}, fmt.Errorf("dunning_policy: access_during_dunning must be %q or %q", openrails.DunningAccessKeep, openrails.DunningAccessSuspend)
	}
	switch declared.AccessWhileRenewalHeld {
	case "", openrails.DunningAccessKeep:
	case openrails.DunningAccessSuspend:
		p.SuspendWhenHeld = true
	default:
		return collection.Policy{}, fmt.Errorf("dunning_policy: access_while_renewal_held must be %q or %q", openrails.DunningAccessKeep, openrails.DunningAccessSuspend)
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

// DeclaredOf is the declared form of a resolved policy: what a dunning case
// records, so it reads back exactly as it was (#1102).
func DeclaredOf(p collection.Policy) openrails.DunningPolicy {
	out := openrails.DunningPolicy{AccessDuringDunning: openrails.DunningAccessKeep, AccessWhileRenewalHeld: openrails.DunningAccessKeep}
	for _, t := range p.Tiers {
		tier := openrails.DunningTier{MaxCycleHours: int(t.MaxCycle / time.Hour)}
		for _, o := range t.Offsets {
			tier.RetryAfterHours = append(tier.RetryAfterHours, int(o/time.Hour))
		}
		out.Tiers = append(out.Tiers, tier)
	}
	for _, d := range p.Transient {
		out.TransientRetryMinutes = append(out.TransientRetryMinutes, int(d/time.Minute))
	}
	if p.SuspendAccess {
		out.AccessDuringDunning = openrails.DunningAccessSuspend
	}
	if p.SuspendWhenHeld {
		out.AccessWhileRenewalHeld = openrails.DunningAccessSuspend
	}
	return out
}

// CasePolicy is the policy a membership's dunning case runs under: the one
// recorded when it opened, else the merchant's current policy.
func CasePolicy(ctx context.Context, d *db.DB, sub *models.Subscription) (collection.Policy, error) {
	if sub != nil && len(sub.DunningPolicy) > 0 {
		var declared openrails.DunningPolicy
		if err := json.Unmarshal(sub.DunningPolicy, &declared); err != nil {
			return collection.Policy{}, fmt.Errorf("subscription %s dunning policy: %w", sub.ID, err)
		}
		return PolicyOf(&declared)
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
	raw, err := json.Marshal(DeclaredOf(policy))
	if err != nil {
		return err
	}
	sub.DunningPolicy = raw
	return nil
}
