package abuse

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/modules/ratelimit"
)

// CardAbuseConfig tunes the failure-driven captcha/block escalation (#371).
type CardAbuseConfig struct {
	// FailWindow is the rolling window for per-subject failed-charge counting.
	FailWindow time.Duration
	// CaptchaAfter: failures within FailWindow that trigger a captcha challenge.
	CaptchaAfter int64
	// BlockAfter: failures within FailWindow that escalate to an aggressive,
	// longer-lived challenge (effectively a temporary block until solved).
	BlockAfter int64
	// ChallengeTTL is how long the stage-1 captcha challenge lasts.
	ChallengeTTL time.Duration
	// BlockTTL is how long the stage-2 aggressive challenge lasts.
	BlockTTL time.Duration
	// GlobalWindow is the rolling window for site-wide failure counting.
	GlobalWindow time.Duration
	// GlobalAttackAfter: site-wide failures within GlobalWindow that flip the
	// whole site into attack mode (captcha for everyone).
	GlobalAttackAfter int64
	// AttackTTL is how long attack mode stays on once triggered.
	AttackTTL time.Duration
}

// DefaultCardAbuseConfig returns the policy the user specified: per account+IP,
// 3 failures/min -> captcha, ~6/min -> aggressive block; site-wide ~100/24h ->
// captcha for everyone.
func DefaultCardAbuseConfig() CardAbuseConfig {
	return CardAbuseConfig{
		FailWindow:        time.Minute,
		CaptchaAfter:      3,
		BlockAfter:        6,
		ChallengeTTL:      15 * time.Minute,
		BlockTTL:          time.Hour,
		GlobalWindow:      24 * time.Hour,
		GlobalAttackAfter: 100,
		AttackTTL:         time.Hour,
	}
}

func (c CardAbuseConfig) withDefaults() CardAbuseConfig {
	d := DefaultCardAbuseConfig()
	if c.FailWindow <= 0 {
		c.FailWindow = d.FailWindow
	}
	if c.CaptchaAfter <= 0 {
		c.CaptchaAfter = d.CaptchaAfter
	}
	if c.BlockAfter <= 0 {
		c.BlockAfter = d.BlockAfter
	}
	if c.BlockAfter < c.CaptchaAfter {
		c.BlockAfter = c.CaptchaAfter
	}
	if c.ChallengeTTL <= 0 {
		c.ChallengeTTL = d.ChallengeTTL
	}
	if c.BlockTTL <= 0 {
		c.BlockTTL = d.BlockTTL
	}
	if c.GlobalWindow <= 0 {
		c.GlobalWindow = d.GlobalWindow
	}
	if c.GlobalAttackAfter <= 0 {
		c.GlobalAttackAfter = d.GlobalAttackAfter
	}
	if c.AttackTTL <= 0 {
		c.AttackTTL = d.AttackTTL
	}
	return c
}

// CardAbuseGuard records failed card attempts and escalates abusive subjects to
// captcha (then an aggressive block), plus a site-wide attack mode — all by
// reusing the existing Redis windowed limiter and the captcha ChallengeStore.
// Captcha stays dormant for normal users; only repeated FAILURES trigger it.
type CardAbuseGuard struct {
	lim        *ratelimit.Limiter
	challenges *captcha.ChallengeStore
	cfg        CardAbuseConfig
}

// NewCardAbuseGuard builds the guard. If lim or challenges is nil the guard is
// a safe no-op (so it never breaks the charge path when Redis/captcha aren't
// configured).
func NewCardAbuseGuard(lim *ratelimit.Limiter, challenges *captcha.ChallengeStore, cfg CardAbuseConfig) *CardAbuseGuard {
	return &CardAbuseGuard{lim: lim, challenges: challenges, cfg: cfg.withDefaults()}
}

func (g *CardAbuseGuard) enabled() bool {
	return g != nil && g.lim != nil && g.challenges != nil
}

// countFailure increments the failure counter for key within window (capped at
// max) and returns the current count in the window.
func (g *CardAbuseGuard) countFailure(ctx context.Context, key string, window time.Duration, max int64) (int64, error) {
	dec, err := g.lim.Check(ctx, "card_fail:"+key,
		ratelimit.Policy{Windows: []ratelimit.Limit{{Unit: "fail", Window: window, Max: max}}},
		map[string]int64{"fail": 1})
	if err != nil {
		return 0, err
	}
	if len(dec.Windows) == 0 {
		return 0, nil
	}
	count := max - dec.Windows[0].Remaining
	if count < 0 {
		count = 0
	}
	return count, nil
}

// RecordChargeFailure records one failed/declined card attempt for the given
// captcha subjects (use ginmw.RateLimitSubjectKeys(c): ["ip:x","user:y"]) and
// escalates each subject's captcha/block state, then advances the site-wide
// attack-mode counter. Best-effort: errors are logged, never propagated, so
// abuse tracking can't break the (already failed) charge response.
func (g *CardAbuseGuard) RecordChargeFailure(ctx context.Context, subjectKeys []string) {
	if !g.enabled() {
		return
	}
	for _, key := range subjectKeys {
		if key == "" {
			continue
		}
		count, err := g.countFailure(ctx, key, g.cfg.FailWindow, g.cfg.BlockAfter)
		if err != nil {
			log.WithError(err).WithField("subject", key).Warn("card-abuse: failed to record charge failure")
			continue
		}
		switch {
		case count >= g.cfg.BlockAfter:
			if err := g.challenges.MarkChallenged(ctx, key, g.cfg.BlockTTL); err != nil {
				log.WithError(err).WithField("subject", key).Warn("card-abuse: failed to block subject")
			} else {
				log.WithFields(log.Fields{"subject": key, "failures": count}).Warn("card-abuse: subject blocked (aggressive captcha) after repeated card failures")
			}
		case count >= g.cfg.CaptchaAfter:
			if err := g.challenges.MarkChallenged(ctx, key, g.cfg.ChallengeTTL); err != nil {
				log.WithError(err).WithField("subject", key).Warn("card-abuse: failed to challenge subject")
			} else {
				log.WithFields(log.Fields{"subject": key, "failures": count}).Info("card-abuse: subject captcha-challenged after repeated card failures")
			}
		}
	}

	globalCount, err := g.countFailure(ctx, "__global__", g.cfg.GlobalWindow, g.cfg.GlobalAttackAfter)
	if err != nil {
		log.WithError(err).Warn("card-abuse: failed to record global charge failure")
		return
	}
	if globalCount >= g.cfg.GlobalAttackAfter {
		if err := g.challenges.MarkChallenged(ctx, captcha.CardAttackModeSubject, g.cfg.AttackTTL); err != nil {
			log.WithError(err).Warn("card-abuse: failed to enable site-wide attack mode")
		} else {
			log.WithField("failures", globalCount).Warn("card-abuse: site-wide attack mode ENABLED — captcha required for all card requests")
		}
	}
}

// AttackModeActive reports whether the site is currently in card-testing attack
// mode (captcha for everyone on card buckets).
func (g *CardAbuseGuard) AttackModeActive(ctx context.Context) (bool, error) {
	if !g.enabled() {
		return false, nil
	}
	return g.challenges.IsChallenged(ctx, captcha.CardAttackModeSubject)
}
