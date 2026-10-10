package collection

import (
	"fmt"
	"time"

	"github.com/open-rails/openrails/billing"
)

// PolicyOf converts and validates a declared dunning policy. Nil is the
// built-in schedule; a policy that declares no tiers keeps the built-in ones.
func PolicyOf(declared *billing.DunningPolicy) (Policy, error) {
	if declared == nil {
		return DefaultPolicy, nil
	}
	p := Policy{}
	for _, t := range declared.Tiers {
		tier := Tier{MaxCycle: time.Duration(t.MaxCycleHours) * time.Hour}
		for _, h := range t.RetryAfterHours {
			tier.Offsets = append(tier.Offsets, time.Duration(h)*time.Hour)
		}
		p.Tiers = append(p.Tiers, tier)
	}
	if len(declared.Tiers) == 0 {
		p.Tiers = DefaultPolicy.Tiers
	}
	for _, m := range declared.TransientRetryMinutes {
		p.Transient = append(p.Transient, time.Duration(m)*time.Minute)
	}
	switch declared.AccessDuringDunning {
	case "", billing.DunningAccessKeep:
	case billing.DunningAccessSuspend:
		p.SuspendAccess = true
	default:
		return Policy{}, fmt.Errorf("dunning_policy: access_during_dunning must be %q or %q", billing.DunningAccessKeep, billing.DunningAccessSuspend)
	}
	switch declared.AccessWhileRenewalHeld {
	case "", billing.DunningAccessKeep:
	case billing.DunningAccessSuspend:
		p.SuspendWhenHeld = true
	default:
		return Policy{}, fmt.Errorf("dunning_policy: access_while_renewal_held must be %q or %q", billing.DunningAccessKeep, billing.DunningAccessSuspend)
	}
	if err := p.Validate(); err != nil {
		return Policy{}, fmt.Errorf("dunning_policy: %w", err)
	}
	return p, nil
}

// DeclaredOf is the declared form of a resolved policy: what a dunning case
// records, so it reads back exactly as it was (#1102).
func DeclaredOf(p Policy) billing.DunningPolicy {
	out := billing.DunningPolicy{AccessDuringDunning: billing.DunningAccessKeep, AccessWhileRenewalHeld: billing.DunningAccessKeep}
	for _, t := range p.Tiers {
		tier := billing.DunningTier{MaxCycleHours: int(t.MaxCycle / time.Hour)}
		for _, o := range t.Offsets {
			tier.RetryAfterHours = append(tier.RetryAfterHours, int(o/time.Hour))
		}
		out.Tiers = append(out.Tiers, tier)
	}
	for _, d := range p.Transient {
		out.TransientRetryMinutes = append(out.TransientRetryMinutes, int(d/time.Minute))
	}
	if p.SuspendAccess {
		out.AccessDuringDunning = billing.DunningAccessSuspend
	}
	if p.SuspendWhenHeld {
		out.AccessWhileRenewalHeld = billing.DunningAccessSuspend
	}
	return out
}
