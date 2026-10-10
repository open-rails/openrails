package abuse

import (
	"context"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/modules/ratelimit"
)

// CardAbuseConfig tunes the failure-driven captcha/block escalation. Each
// subject has a burst window (FailWindow: CaptchaAfter -> captcha, BlockAfter
// -> block) and a daily window (DailyWindow: DailyBlockAfter -> blocked for the
// day). The Global* fields set merchant-wide attack mode, which the
// FailureLedger decides.
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
	// GlobalWindow is the rolling window for merchant-wide failure counting.
	GlobalWindow time.Duration
	// GlobalAttackAfter: failures at one merchant within GlobalWindow that
	// put that merchant in attack mode, when they came from at least
	// GlobalAttackSubjects customers and as many client addresses. A few
	// accounts or addresses never make an attack: their own blocks contain them.
	GlobalAttackAfter    int64
	GlobalAttackSubjects int64
	// AttackTTL is how long the attack captcha stays up after the last
	// refusal seen while the merchant is under attack.
	AttackTTL time.Duration
}

// DefaultCardAbuseConfig returns the production policy.
func DefaultCardAbuseConfig() CardAbuseConfig {
	return CardAbuseConfig{
		FailWindow:           15 * time.Minute,
		CaptchaAfter:         3,
		BlockAfter:           6,
		ChallengeTTL:         15 * time.Minute,
		BlockTTL:             15 * time.Minute,
		DailyWindow:          24 * time.Hour,
		DailyBlockAfter:      10,
		DailyBlockTTL:        24 * time.Hour,
		GlobalWindow:         24 * time.Hour,
		GlobalAttackAfter:    100,
		GlobalAttackSubjects: 25,
		AttackTTL:            time.Hour,
	}
}

// CardAbuseGuard is the captcha accelerator over the durable FailureLedger: it
// captchas abusive subjects and, while the ledger reports an attack, everyone
// on that merchant's card routes. It is built only when Redis and a captcha are
// configured; otherwise the ledger's blocks are the whole policy.
type CardAbuseGuard struct {
	lim        *ratelimit.Limiter
	challenges *captcha.ChallengeStore
	cfg        CardAbuseConfig
}

// NewCardAbuseGuard builds the guard; a nil lim or challenges makes it a no-op.
// cfg is used verbatim, with no zero-value defaulting.
func NewCardAbuseGuard(lim *ratelimit.Limiter, challenges *captcha.ChallengeStore, cfg CardAbuseConfig) *CardAbuseGuard {
	return &CardAbuseGuard{lim: lim, challenges: challenges, cfg: cfg}
}

func (g *CardAbuseGuard) enabled() bool {
	return g != nil && g.lim != nil && g.challenges != nil
}

// countFailure counts one failure in (key, unit)'s window, capped at max, and
// returns the window's count; unit keeps co-existing windows' keys apart. Each
// window gets its own Check: the limiter is all-or-nothing across a Policy's
// windows, so a tripped burst window would stop the daily counter.
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

// RecordChargeFailure counts one failed card attempt for each captcha subject
// (middleware.SubjectKeysFromContext, the subjects RateLimitHTTP pinned) and
// escalates it. attack, the ledger's verdict, (re)opens the merchant's attack
// captcha for AttackTTL. Best-effort: errors are logged, never returned.
func (g *CardAbuseGuard) RecordChargeFailure(ctx context.Context, merchantID uuid.UUID, subjectKeys []string, attack bool) {
	if !g.enabled() {
		return
	}
	for _, key := range subjectKeys {
		if key == "" {
			continue
		}

		// Daily window first, so it keeps counting after the burst window blocks.
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

	if !attack || merchantID == uuid.Nil {
		return
	}
	fields := log.Fields{"merchant_id": merchantID}
	if err := g.challenges.MarkChallenged(ctx, captcha.CardAttackModeSubject(merchantID), g.cfg.AttackTTL); err != nil {
		log.WithError(err).WithFields(fields).Warn("card-abuse: failed to enable attack mode")
	} else {
		log.WithFields(fields).Warn("card-abuse: attack mode — captcha required on this merchant's card routes")
	}
}
