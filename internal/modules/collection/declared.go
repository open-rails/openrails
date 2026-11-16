package collection

import (
	"fmt"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// PolicyOf converts and validates a declared dunning policy. Nil is the
// built-in policy; a policy keeps the built-in tiers, transient ladder and
// customer retries it does not declare. An empty ladder is none.
func PolicyOf(declared *billing.DunningPolicy) (Policy, error) {
	if declared == nil {
		return DefaultPolicy, nil
	}
	p := Policy{CustomerRetries: DefaultCustomerRetries}
	if declared.CustomerRetries != nil {
		p.CustomerRetries = *declared.CustomerRetries
	}
	for _, t := range declared.Tiers {
		if t.MaxCycleHours < 0 || t.MaxCycleHours > catalog.MaxDurationHours {
			return Policy{}, fmt.Errorf("dunning_policy: max_cycle_hours must be between 0 and %d", catalog.MaxDurationHours)
		}
		tier := Tier{MaxCycle: time.Duration(t.MaxCycleHours) * time.Hour}
		for _, h := range t.RetryAfterHours {
			if h <= 0 || h > catalog.MaxDurationHours {
				return Policy{}, fmt.Errorf("dunning_policy: retry_after_hours must be between 1 and %d", catalog.MaxDurationHours)
			}
			tier.Offsets = append(tier.Offsets, time.Duration(h)*time.Hour)
		}
		p.Tiers = append(p.Tiers, tier)
	}
	if len(declared.Tiers) == 0 {
		p.Tiers = DefaultPolicy.Tiers
	}
	if declared.TransientRetryMinutes == nil {
		p.Transient = TransientLadder
	} else {
		for _, m := range *declared.TransientRetryMinutes {
			if m < 1 || m > 120 {
				return Policy{}, fmt.Errorf("dunning_policy: transient retries run between 1 minute and 2 hours after the attempt")
			}
			p.Transient = append(p.Transient, time.Duration(m)*time.Minute)
		}
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

// RecordedPolicyOf reads the policy a dunning case recorded (DeclaredOf). A
// record lists its transient retries: one without the list, written before
// an empty list was recorded, has none, never the built-in ladder.
func RecordedPolicyOf(recorded billing.DunningPolicy) (Policy, error) {
	if recorded.TransientRetryMinutes == nil {
		recorded.TransientRetryMinutes = &[]int{}
	}
	return PolicyOf(&recorded)
}

// DeclaredOf is the declared form of a resolved policy: what a dunning case
// records, so it reads back exactly as it was (#1102). Every default is
// written out.
func DeclaredOf(p Policy) billing.DunningPolicy {
	retries, ladder := p.CustomerRetries, make([]int, 0, len(p.Transient))
	for _, d := range p.Transient {
		ladder = append(ladder, int(d/time.Minute))
	}
	out := billing.DunningPolicy{TransientRetryMinutes: &ladder, CustomerRetries: &retries, AccessDuringDunning: billing.DunningAccessKeep, AccessWhileRenewalHeld: billing.DunningAccessKeep}
	for _, t := range p.Tiers {
		tier := billing.DunningTier{MaxCycleHours: int(t.MaxCycle / time.Hour)}
		for _, o := range t.Offsets {
			tier.RetryAfterHours = append(tier.RetryAfterHours, int(o/time.Hour))
		}
		out.Tiers = append(out.Tiers, tier)
	}
	if p.SuspendAccess {
		out.AccessDuringDunning = billing.DunningAccessSuspend
	}
	if p.SuspendWhenHeld {
		out.AccessWhileRenewalHeld = billing.DunningAccessSuspend
	}
	return out
}
