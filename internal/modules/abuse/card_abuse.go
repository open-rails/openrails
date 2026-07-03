package abuse

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/modules/ratelimit"
)

// CardAbuseConfig tunes the failure-driven captcha/block escalation (#371, #331).
//
// Per (user + ip) subject there are TWO rolling windows:
//   - a short BURST window (FailWindow, 15 min): CaptchaAfter -> captcha,
//     BlockAfter -> aggressive block for the remainder of the window.
//   - a DAILY window (DailyWindow, 24h): DailyBlockAfter -> blocked for the day.
//
// Plus the site-wide GLOBAL cap (GlobalWindow/GlobalAttackAfter): too many
// failures across everyone flips the whole site into attack mode.
type CardAbuseConfig struct {
	// FailWindow is the short rolling window for per-subject failed-charge counting.
	FailWindow time.Duration
	// CaptchaAfter: failures within FailWindow that trigger a captcha challenge.
	CaptchaAfter int64
	// BlockAfter: failures within FailWindow that escalate to an aggressive,
	// longer-lived challenge (effectively a temporary block until solved).
	BlockAfter int64
	// ChallengeTTL is how long the stage-1 captcha challenge lasts.
	ChallengeTTL time.Duration
	// BlockTTL is how long the stage-2 aggressive (burst-window) block lasts —
	// roughly the remainder of FailWindow.
	BlockTTL time.Duration
	// DailyWindow is the longer rolling window for the per-subject daily cap.
	DailyWindow time.Duration
	// DailyBlockAfter: failures within DailyWindow that block the subject for the day.
	DailyBlockAfter int64
	// DailyBlockTTL is how long the daily block lasts — roughly the remainder of
	// DailyWindow.
	DailyBlockTTL time.Duration
	// GlobalWindow is the rolling window for site-wide failure counting.
	GlobalWindow time.Duration
	// GlobalAttackAfter: site-wide failures within GlobalWindow that flip the
	// whole site into attack mode (captcha for everyone).
	GlobalAttackAfter int64
	// AttackTTL is how long attack mode stays on once triggered.
	AttackTTL time.Duration
}

// DefaultCardAbuseConfig returns the agreed policy (#331): per (account+IP), in a
// 15-min window 3 failures -> captcha and 6 -> block; in a 24h window 10 failures
// -> block for the day; site-wide 100/24h -> captcha for everyone (attack mode).
func DefaultCardAbuseConfig() CardAbuseConfig {
	return CardAbuseConfig{
		FailWindow:        15 * time.Minute,
		CaptchaAfter:      3,
		BlockAfter:        6,
		ChallengeTTL:      15 * time.Minute,
		BlockTTL:          15 * time.Minute,
		DailyWindow:       24 * time.Hour,
		DailyBlockAfter:   10,
		DailyBlockTTL:     24 * time.Hour,
		GlobalWindow:      24 * time.Hour,
		GlobalAttackAfter: 100,
		AttackTTL:         time.Hour,
	}
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
// configured). cfg is used VERBATIM (#711 — no zero-value re-defaulting):
// production wires DefaultCardAbuseConfig(), the ONE defaults path; tests
// pass a complete config.
func NewCardAbuseGuard(lim *ratelimit.Limiter, challenges *captcha.ChallengeStore, cfg CardAbuseConfig) *CardAbuseGuard {
	return &CardAbuseGuard{lim: lim, challenges: challenges, cfg: cfg}
}

func (g *CardAbuseGuard) enabled() bool {
	return g != nil && g.lim != nil && g.challenges != nil
}

// countFailure increments the failure counter for (key, unit) within window
// (capped at max) and returns the current count in the window. The unit
// distinguishes co-existing windows for the same subject (e.g. burst vs daily)
// so their Redis keys never collide.
//
// Each window is checked independently (its own Check call) rather than as a
// multi-window Policy: the limiter is all-or-nothing across a Policy's windows,
// so a tripped short window would stop the daily counter from advancing. Separate
// checks let the daily window keep accruing past the burst block.
func (g *CardAbuseGuard) countFailure(ctx context.Context, key, unit string, window time.Duration, max int64) (int64, error) {
	dec, err := g.lim.Check(ctx, "card_fail:"+key,
		ratelimit.Policy{Windows: []ratelimit.Limit{{Unit: unit, Window: window, Max: max}}},
		map[string]int64{unit: 1})
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
// captcha subjects (use middleware.RateLimitSubjectKeysHTTP(r): ["ip:x","user:y"]) and
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

		// Daily window (24h): independent counter; trips a day-long block at
		// DailyBlockAfter. Evaluated first so it still advances even when the
		// burst window has already blocked the subject.
		dailyCount, err := g.countFailure(ctx, key, "fail_day", g.cfg.DailyWindow, g.cfg.DailyBlockAfter)
		if err != nil {
			log.WithError(err).WithField("subject", key).Warn("card-abuse: failed to record daily charge failure")
		} else if dailyCount >= g.cfg.DailyBlockAfter {
			if err := g.challenges.MarkChallenged(ctx, key, g.cfg.DailyBlockTTL); err != nil {
				log.WithError(err).WithField("subject", key).Warn("card-abuse: failed to block subject for the day")
			} else {
				log.WithFields(log.Fields{"subject": key, "failures": dailyCount}).Warn("card-abuse: subject blocked for the day after repeated card failures")
			}
		}

		// Burst window (15 min): CaptchaAfter -> captcha, BlockAfter -> block for
		// the remainder of the window.
		count, err := g.countFailure(ctx, key, "fail", g.cfg.FailWindow, g.cfg.BlockAfter)
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

	globalCount, err := g.countFailure(ctx, "__global__", "fail", g.cfg.GlobalWindow, g.cfg.GlobalAttackAfter)
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
