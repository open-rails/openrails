package abuse

import (
	"context"
	"time"

	"github.com/open-rails/openrails/internal/modules/ratelimit"
)

// VelocityGuard caps how often a shared identifier appears in a window (#300):
// new accounts per card fingerprint / IP (sign-up velocity), and failed charge
// attempts per card (decline-storm / card-testing). It reuses the proven Redis
// fixed-window limiter rather than new infra.
type VelocityGuard struct {
	lim *ratelimit.Limiter
}

func NewVelocityGuard(lim *ratelimit.Limiter) *VelocityGuard { return &VelocityGuard{lim: lim} }

// Allow records one event for (namespace,key) and returns false if it would
// exceed max within window.
func (g *VelocityGuard) Allow(ctx context.Context, namespace, key string, max int64, window time.Duration) (bool, error) {
	if g == nil || g.lim == nil {
		return true, nil
	}
	d, err := g.lim.Check(ctx, namespace+":"+key,
		ratelimit.Policy{Windows: []ratelimit.Limit{{Unit: "event", Window: window, Max: max}}},
		map[string]int64{"event": 1})
	if err != nil {
		return false, err
	}
	return d.Allowed, nil
}

// AllowSignup caps new accounts per shared instrument (card fingerprint or IP) —
// the "sign up 100 times with one card" defense.
func (g *VelocityGuard) AllowSignup(ctx context.Context, instrument string, maxPerWindow int64, window time.Duration) (bool, error) {
	return g.Allow(ctx, "vel:signup", instrument, maxPerWindow, window)
}

// AllowChargeAttempt caps failed-charge attempts per card (card-testing /
// decline-storm) so retry storms don't probe stolen cards through us.
func (g *VelocityGuard) AllowChargeAttempt(ctx context.Context, card string, maxPerWindow int64, window time.Duration) (bool, error) {
	return g.Allow(ctx, "vel:decline", card, maxPerWindow, window)
}
